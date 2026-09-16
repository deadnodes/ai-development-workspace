package persistence

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
	"releasecontrol/internal/domain"
)

const maxLocalStateBytes = 256 << 20

var localBucket = []byte("release-control")
var localStateKey = []byte("state")

// LocalStore persists the same domain.State and application transactions as
// PostgreSQL. bbolt supplies process locking, atomic commits and fsync durability.
// One local instance owns the file; use PostgreSQL for a shared database.
type LocalStore struct {
	db        *bolt.DB
	writer    chan struct{}
	lifecycle sync.RWMutex
	closed    bool
}

func OpenLocal(ctx context.Context, path string) (*LocalStore, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if path == "" {
		return nil, errors.New("local data path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	timeout := 2 * time.Second
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < timeout {
		timeout = time.Until(deadline)
		if timeout <= 0 {
			return nil, context.DeadlineExceeded
		}
	}
	db, err := bolt.Open(path, 0600, &bolt.Options{Timeout: timeout})
	if err != nil {
		return nil, fmt.Errorf("open local database: %w", err)
	}
	s := &LocalStore{db: db, writer: make(chan struct{}, 1)}
	err = db.Update(func(tx *bolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		bucket, err := tx.CreateBucketIfNotExists(localBucket)
		if err != nil {
			return err
		}
		if bucket.Get(localStateKey) == nil {
			encoded, err := json.Marshal(domain.EmptyState())
			if err != nil {
				return err
			}
			return bucket.Put(localStateKey, encoded)
		}
		_, err = readLocal(tx)
		return err
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}
func (s *LocalStore) Close() {
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	if !s.closed {
		s.closed = true
		_ = s.db.Close()
	}
}
func readLocal(tx *bolt.Tx) (domain.State, error) {
	st := domain.EmptyState()
	bucket := tx.Bucket(localBucket)
	if bucket == nil {
		return st, errors.New("local state bucket missing")
	}
	data := bucket.Get(localStateKey)
	if len(data) == 0 || len(data) > maxLocalStateBytes {
		return st, errors.New("local state missing or exceeds 256 MiB limit")
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return st, fmt.Errorf("decode local state: %w", err)
	}
	if err := domain.ValidateStateStatuses(st); err != nil {
		return st, err
	}
	return st, nil
}
func (s *LocalStore) Read(ctx context.Context) (domain.State, error) {
	if err := ctx.Err(); err != nil {
		return domain.State{}, err
	}
	s.lifecycle.RLock()
	defer s.lifecycle.RUnlock()
	if s.closed {
		return domain.State{}, errors.New("local store closed")
	}
	var st domain.State
	err := s.db.View(func(tx *bolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		var err error
		st, err = readLocal(tx)
		return err
	})
	return st, err
}
func (s *LocalStore) Update(ctx context.Context, fn func(*domain.State) error) error {
	select {
	case s.writer <- struct{}{}:
		defer func() { <-s.writer }()
	case <-ctx.Done():
		return ctx.Err()
	}
	s.lifecycle.RLock()
	defer s.lifecycle.RUnlock()
	if s.closed {
		return errors.New("local store closed")
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		st, err := readLocal(tx)
		if err != nil {
			return err
		}
		beforeEvents, err := json.Marshal(st.Events)
		if err != nil {
			return err
		}
		oldEventCount := len(st.Events)
		before, err := documents(st)
		if err != nil {
			return err
		}
		if err = fn(&st); err != nil {
			return err
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		if err = domain.ValidateStateStatuses(st); err != nil {
			return err
		}
		if len(st.Events) < oldEventCount {
			return errors.New("audit history cannot be removed")
		}
		prefix, err := json.Marshal(st.Events[:oldEventCount])
		if err != nil {
			return err
		}
		if !bytes.Equal(beforeEvents, prefix) {
			return errors.New("audit history cannot be rewritten")
		}
		after, err := documents(st)
		if err != nil {
			return err
		}
		ids := map[string]string{}
		for kind, records := range after {
			for _, raw := range records {
				var meta struct {
					ID string `json:"id"`
				}
				if err = json.Unmarshal(raw, &meta); err != nil {
					return err
				}
				if meta.ID == "" {
					return errors.New("record ID is required")
				}
				if _, exists := ids[meta.ID]; exists {
					return errors.New("duplicate record ID")
				}
				ids[meta.ID] = kind
			}
		}
		// Match the existing PostgreSQL store's non-deleting contract explicitly.
		for kind, records := range before {
			for _, raw := range records {
				var meta struct {
					ID string `json:"id"`
				}
				if err = json.Unmarshal(raw, &meta); err != nil {
					return err
				}
				if ids[meta.ID] != kind {
					return errors.New("existing records cannot be removed or change kind")
				}
			}
		}
		eventIDs := map[string]bool{}
		for _, event := range st.Events {
			if event.ID == "" || eventIDs[event.ID] {
				return errors.New("audit event ID must be unique and nonempty")
			}
			eventIDs[event.ID] = true
		}
		encoded, err := json.Marshal(st)
		if err != nil {
			return err
		}
		if len(encoded) > maxLocalStateBytes {
			return errors.New("local state exceeds 256 MiB limit; use PostgreSQL")
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		return tx.Bucket(localBucket).Put(localStateKey, encoded)
	})
}
