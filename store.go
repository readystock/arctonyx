package raft_badger

import (
	"fmt"
	"github.com/Ready-Stock/badger"
	"github.com/golang/protobuf/proto"
	"github.com/hashicorp/raft"
	"github.com/kataras/golog"
	uuid2 "github.com/satori/go.uuid"
	"net"
	"os"
	"sync"
	"time"
)

const (
	retainSnapshotCount = 2
	raftTimeout         = 5 * time.Second
)


type Store struct {
	raft        *raft.Raft
	badger      *badger.DB
	sequenceIds *badger.Sequence

	chunkMapMutex  *sync.Mutex
	sequenceChunks map[string]*SequenceChunk

	sequenceClient *sequenceClient
	clusterClient  *clusterClient
	nodeId         uint64
}

// Creates and possibly joins a cluster.
func CreateStore(directory string, joinAddr *string) (*Store, error) {
	// Setup Raft configuration.
	config := raft.DefaultConfig()
	config.LocalID = raft.ServerID("")
	store := Store{}

	addr, err := net.ResolveTCPAddr("tcp", ":6543")
	if err != nil {
		return nil, err
	}

	transport, err := raft.NewTCPTransport(":6543", addr, 5, 10*time.Second, os.Stderr)
	if err != nil {
		return nil, err
	}

	opts := badger.DefaultOptions
	opts.Dir = directory
	opts.ValueDir = directory
	db, err := badger.Open(opts)
	if err != nil {
		return nil, err
	}
	store.badger = db

	stable := stableStore(store)
	log := logStore(store)
	if id, err := stable.Get([]byte("/_server_id_/")); err != nil {
		if err.Error() == "Key not found" {
			if uuid, err := uuid2.NewV4(); err != nil {
				return nil, err
			} else {
				stable.Set([]byte("/_server_id_/"), []byte(uuid.String()))
				config.LocalID = raft.ServerID(string(uuid.String()))
			}
		} else {
			return nil, err
		}
	} else {
		config.LocalID = raft.ServerID(string(id))
	}

	snapshots, err := raft.NewFileSnapshotStore(directory, retainSnapshotCount, os.Stderr)
	if err != nil {
		return nil, fmt.Errorf("file snapshot store: %s", err)
	}

	ra, err := raft.NewRaft(config, (*fsm)(&store), &log, &stable, snapshots, transport)
	if err != nil {
		return nil, fmt.Errorf("new raft: %s", err)
	}
	store.raft = ra
	return &store, nil
}

func (store *Store) Join(nodeId, addr string) error {
	golog.Debugf("received join request from remote node [%s] at [%s]", nodeId, addr)

	configFuture := store.raft.GetConfiguration()
	if err := configFuture.Error(); err != nil {
		golog.Errorf("failed to get raft configuration: %s", err.Error())
	}
	return nil
}

func (store *Store) Get(key []byte) (value []byte, err error) {
	err = store.badger.View(func(txn *badger.Txn) error {
		item, err := txn.Get(key)
		if err != nil {
			return err
		}
		value, err = item.Value()
		return err
	})
	return value, err
}

func (store *Store) Set(key, value []byte) (err error) {
	c := &Command{Operation:Operation_SET, Key:key, Value:value}
	if store.raft.State() != raft.Leader {
		if _, err := store.clusterClient.sendCommand(store.raft.Leader(), c); err != nil {
			return err
		}
		return nil
	}
	b, err := proto.Marshal(c)
	if err != nil {
		return err
	}
	return store.raft.Apply(b, raftTimeout).Error()
}

func (store *Store) Delete(key []byte) (err error) {
	c := &Command{Operation:Operation_DELETE, Key:key, Value:nil}
	if store.raft.State() != raft.Leader {
		if _, err := store.clusterClient.sendCommand(store.raft.Leader(), c); err != nil {
			return err
		}
		return nil
	}
	b, err := proto.Marshal(c)
	if err != nil {
		return err
	}
	return store.raft.Apply(b, raftTimeout).Error()
}