package auth

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	bolt "go.etcd.io/bbolt"
)

type loginFlow struct {
	Nonce, Verifier, ReturnTo string
	Expires                   time.Time
}
type session struct {
	Principal Principal
	Expires   time.Time
}
type agent struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Owner       string     `json:"-"`
	Verifier    string     `json:"-"`
	Products    []string   `json:"products"`
	Permissions []string   `json:"permissions"`
	CreatedAt   time.Time  `json:"created_at"`
	RevokedAt   *time.Time `json:"revoked_at,omitempty"`
}

// Disk records have their own encoding: never rely on the public agent JSON
// projection for private verifier/ownership persistence.
type agentRecord struct {
	Info            agent
	Owner, Verifier string
}
type device struct {
	Name, UserHash, Status, AgentID, Owner string
	Products, Permissions                  []string
	Expires, LastPoll                      time.Time
	Interval                               int
	Attempts                               int
}
type authState struct {
	Version  int
	Flows    map[string]loginFlow
	Sessions map[string]session
	Agents   map[string]agentRecord
	Devices  map[string]device
}

func emptyState() authState {
	return authState{Version: 1, Flows: map[string]loginFlow{}, Sessions: map[string]session{}, Agents: map[string]agentRecord{}, Devices: map[string]device{}}
}
func (s *authState) prune(now time.Time) {
	for k, v := range s.Flows {
		if !now.Before(v.Expires) {
			delete(s.Flows, k)
		}
	}
	for k, v := range s.Sessions {
		if !now.Before(v.Expires) {
			delete(s.Sessions, k)
		}
	}
	for k, v := range s.Devices {
		if now.After(v.Expires.Add(time.Hour)) {
			delete(s.Devices, k)
		}
	}
}

type privateStore struct {
	db   *bolt.DB
	pool *pgxpool.Pool
	aead cipher.AEAD
}

func openStore(ctx context.Context, c Config) (*privateStore, error) {
	block, err := aes.NewCipher(c.EncryptionKey)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	s := &privateStore{aead: aead}
	if c.DatabaseURL != "" {
		s.pool, err = pgxpool.New(ctx, c.DatabaseURL)
		if err != nil {
			return nil, err
		}
		_, err = s.pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS rcp_auth_private (id integer PRIMARY KEY CHECK (id=1), payload bytea NOT NULL)`)
		if err == nil {
			var b []byte
			b, err = s.encode(emptyState())
			if err == nil {
				_, err = s.pool.Exec(ctx, `INSERT INTO rcp_auth_private (id,payload) VALUES (1,$1) ON CONFLICT (id) DO NOTHING`, b)
			}
		}
	} else {
		if err = os.MkdirAll(filepath.Dir(c.FilePath), 0700); err == nil {
			s.db, err = bolt.Open(c.FilePath, 0600, &bolt.Options{Timeout: time.Second})
			if err == nil {
				err = os.Chmod(c.FilePath, 0600)
			}
		}
		if err == nil {
			err = s.db.Update(func(tx *bolt.Tx) error { _, e := tx.CreateBucketIfNotExists([]byte("private_auth")); return e })
		}
	}
	if err != nil {
		s.Close()
		return nil, err
	}
	if err = s.update(ctx, func(*authState) error { return nil }); err != nil {
		s.Close()
		return nil, fmt.Errorf("opening encrypted auth state: %w", err)
	}
	return s, nil
}
func (s *privateStore) Close() error {
	if s.pool != nil {
		s.pool.Close()
	}
	if s.db != nil {
		return s.db.Close()
	}
	return nil
}
func (s *privateStore) encode(st authState) ([]byte, error) {
	b, e := json.Marshal(st)
	if e != nil {
		return nil, e
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, e = rand.Read(nonce); e != nil {
		return nil, e
	}
	return s.aead.Seal(nonce, nonce, b, []byte("rcp-auth-private-v1")), nil
}
func (s *privateStore) decode(b []byte) (authState, error) {
	if len(b) == 0 {
		return emptyState(), nil
	}
	n := s.aead.NonceSize()
	if len(b) < n {
		return authState{}, errors.New("invalid encrypted auth state")
	}
	plain, e := s.aead.Open(nil, b[:n], b[n:], []byte("rcp-auth-private-v1"))
	if e != nil {
		return authState{}, errors.New("invalid auth encryption key or ciphertext")
	}
	var st authState
	e = json.Unmarshal(plain, &st)
	if e == nil && (st.Version != 1 || st.Flows == nil || st.Sessions == nil || st.Agents == nil || st.Devices == nil) {
		e = errors.New("invalid auth state schema")
	}
	return st, e
}
func (s *privateStore) update(ctx context.Context, fn func(*authState) error) error {
	apply := func(b []byte) ([]byte, error) {
		st, e := s.decode(b)
		if e != nil {
			return nil, e
		}
		st.prune(time.Now())
		if e = fn(&st); e != nil {
			return nil, e
		}
		return s.encode(st)
	}
	if s.db != nil {
		return s.db.Update(func(tx *bolt.Tx) error {
			bucket := tx.Bucket([]byte("private_auth"))
			b, e := apply(bucket.Get([]byte("state")))
			if e != nil {
				return e
			}
			return bucket.Put([]byte("state"), b)
		})
	}
	tx, e := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	var b []byte
	if e = tx.QueryRow(ctx, `SELECT payload FROM rcp_auth_private WHERE id=1 FOR UPDATE`).Scan(&b); e != nil {
		return e
	}
	b, e = apply(b)
	if e != nil {
		return e
	}
	if _, e = tx.Exec(ctx, `UPDATE rcp_auth_private SET payload=$1 WHERE id=1`, b); e != nil {
		return e
	}
	return tx.Commit(ctx)
}
