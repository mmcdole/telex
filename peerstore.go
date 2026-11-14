package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"time"

	"github.com/gotd/td/telegram/peers"
	bolt "go.etcd.io/bbolt"
)

// boltPeerStorage persists access hashes for peers.Manager, so a chat ID an
// agent got yesterday still resolves after a restart without re-listing dialogs.
type boltPeerStorage struct{ db *bolt.DB }

var _ peers.Storage = boltPeerStorage{}

var (
	bucketPeers  = []byte("peers")
	bucketPhones = []byte("phones")
	bucketMeta   = []byte("meta")
	keyContacts  = []byte("contacts_hash")
)

func openPeerStorage(path string) (boltPeerStorage, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return boltPeerStorage{}, fmt.Errorf("open peer db: %w", err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, b := range [][]byte{bucketPeers, bucketPhones, bucketMeta} {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		_ = db.Close()
		return boltPeerStorage{}, err
	}
	// This is a cache of access hashes Telegram will hand out again; losing
	// the last few writes in a crash is harmless, an fsync per peer is not.
	db.NoSync = true
	return boltPeerStorage{db: db}, nil
}

func (s boltPeerStorage) Close() error { return s.db.Close() }

func peerKey(k peers.Key) []byte {
	b := make([]byte, 0, len(k.Prefix)+1+8)
	b = append(b, k.Prefix...)
	b = append(b, 0)
	return binary.BigEndian.AppendUint64(b, uint64(k.ID))
}

func decodePeerKey(b []byte) (peers.Key, bool) {
	if len(b) < 9 || b[len(b)-9] != 0 {
		return peers.Key{}, false
	}
	return peers.Key{Prefix: string(b[:len(b)-9]), ID: int64(binary.BigEndian.Uint64(b[len(b)-8:]))}, true
}

func i64(v int64) []byte { return binary.BigEndian.AppendUint64(nil, uint64(v)) }

func (s boltPeerStorage) Save(_ context.Context, key peers.Key, value peers.Value) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketPeers).Put(peerKey(key), i64(value.AccessHash))
	})
}

func (s boltPeerStorage) Find(_ context.Context, key peers.Key) (v peers.Value, found bool, err error) {
	err = s.db.View(func(tx *bolt.Tx) error {
		if b := tx.Bucket(bucketPeers).Get(peerKey(key)); len(b) == 8 {
			v.AccessHash, found = int64(binary.BigEndian.Uint64(b)), true
		}
		return nil
	})
	return v, found, err
}

func (s boltPeerStorage) SavePhone(_ context.Context, phone string, key peers.Key) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketPhones).Put([]byte(phone), peerKey(key))
	})
}

func (s boltPeerStorage) FindPhone(ctx context.Context, phone string) (key peers.Key, v peers.Value, found bool, err error) {
	err = s.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket(bucketPhones).Get([]byte(phone))
		if raw == nil {
			return nil
		}
		k, ok := decodePeerKey(raw)
		if !ok {
			return nil
		}
		if b := tx.Bucket(bucketPeers).Get(raw); len(b) == 8 {
			key, v.AccessHash, found = k, int64(binary.BigEndian.Uint64(b)), true
		}
		return nil
	})
	return key, v, found, err
}

func (s boltPeerStorage) GetContactsHash(context.Context) (h int64, err error) {
	err = s.db.View(func(tx *bolt.Tx) error {
		if b := tx.Bucket(bucketMeta).Get(keyContacts); len(b) == 8 {
			h = int64(binary.BigEndian.Uint64(b))
		}
		return nil
	})
	return h, err
}

func (s boltPeerStorage) SaveContactsHash(_ context.Context, hash int64) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketMeta).Put(keyContacts, i64(hash))
	})
}
