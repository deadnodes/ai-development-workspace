package persistence

import (
	"context"
	"encoding/json"
	bolt "go.etcd.io/bbolt"
	"releasecontrol/internal/domain"
	"time"
)

// Freshness deliberately bypasses the state transaction, revision and caches.
func (s *Store) TouchRuntimeFreshness(ctx context.Context, operationID string, observationID string, observedAt time.Time, healthy bool) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO runtime_observation_freshness(operation_id, observation_id, observed_at, healthy) VALUES($1,$2,$3,$4) ON CONFLICT (operation_id) DO UPDATE SET observation_id=EXCLUDED.observation_id, observed_at=EXCLUDED.observed_at, healthy=EXCLUDED.healthy WHERE runtime_observation_freshness.observed_at <= EXCLUDED.observed_at`, operationID, observationID, observedAt, healthy)
	return err
}
func (s *Store) RuntimeFreshness(ctx context.Context, ids []string) (map[string]domain.RuntimeFreshness, error) {
	out := map[string]domain.RuntimeFreshness{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT operation_id, observation_id, observed_at, healthy FROM runtime_observation_freshness WHERE operation_id=ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var f domain.RuntimeFreshness
		if err = rows.Scan(&id, &f.ObservationID, &f.ObservedAt, &f.Healthy); err != nil {
			return nil, err
		}
		out[id] = f
	}
	return out, rows.Err()
}

var runtimeFreshnessBucket = []byte("runtime-observation-freshness")

type localRuntimeFreshness struct {
	ObservationID string    `json:"observation_id"`
	ObservedAt    time.Time `json:"observed_at"`
	Healthy       bool      `json:"healthy"`
}

func (s *LocalStore) TouchRuntimeFreshness(ctx context.Context, operationID string, observationID string, observedAt time.Time, healthy bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.lifecycle.RLock()
	defer s.lifecycle.RUnlock()
	return s.db.Update(func(tx *bolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		b, err := tx.CreateBucketIfNotExists(runtimeFreshnessBucket)
		if err != nil {
			return err
		}
		var old localRuntimeFreshness
		if raw := b.Get([]byte(operationID)); raw != nil {
			if err = json.Unmarshal(raw, &old); err != nil {
				return err
			}
			if old.ObservedAt.After(observedAt) {
				return nil
			}
		}
		raw, err := json.Marshal(localRuntimeFreshness{ObservationID: observationID, ObservedAt: observedAt, Healthy: healthy})
		if err != nil {
			return err
		}
		return b.Put([]byte(operationID), raw)
	})
}
func (s *LocalStore) RuntimeFreshness(ctx context.Context, ids []string) (map[string]domain.RuntimeFreshness, error) {
	out := map[string]domain.RuntimeFreshness{}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.lifecycle.RLock()
	defer s.lifecycle.RUnlock()
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(runtimeFreshnessBucket)
		if b == nil {
			return nil
		}
		for _, id := range ids {
			if err := ctx.Err(); err != nil {
				return err
			}
			if raw := b.Get([]byte(id)); raw != nil {
				var v localRuntimeFreshness
				if err := json.Unmarshal(raw, &v); err != nil {
					return err
				}
				out[id] = domain.RuntimeFreshness{ObservationID: v.ObservationID, ObservedAt: v.ObservedAt, Healthy: v.Healthy}
			}
		}
		return nil
	})
	return out, err
}
