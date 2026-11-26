package persistence

import (
	"context"
	"encoding/json"
	"fmt"
	bolt "go.etcd.io/bbolt"
	"releasecontrol/internal/auth"
	"releasecontrol/internal/domain"
	"strings"
	"time"
)

// Freshness deliberately bypasses the state transaction, revision and caches.
func (s *Store) TouchRuntimeFreshness(ctx context.Context, operationID string, target string, observationID string, observedAt time.Time, healthy bool) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO runtime_observation_freshness(operation_id, target, observation_id, observed_at, healthy) VALUES($1,$2,$3,$4,$5) ON CONFLICT (operation_id, target) DO UPDATE SET observation_id=EXCLUDED.observation_id, observed_at=EXCLUDED.observed_at, healthy=EXCLUDED.healthy WHERE runtime_observation_freshness.observed_at <= EXCLUDED.observed_at`, operationID, target, observationID, observedAt, healthy)
	return err
}
func (s *Store) RuntimeFreshness(ctx context.Context, ids []string) (map[string]domain.RuntimeFreshness, error) {
	out := map[string]domain.RuntimeFreshness{}
	if len(ids) == 0 {
		return out, nil
	}
	operations, targets := make([]string, len(ids)), make([]string, len(ids))
	for i, key := range ids {
		operations[i], targets[i], _ = strings.Cut(key, "\x00")
	}
	user, restricted := auth.PrivateProductViewer(ctx)
	rows, err := s.pool.Query(ctx, `SELECT f.operation_id, f.target, f.observation_id, f.observed_at, f.healthy
		FROM runtime_observation_freshness f JOIN unnest($1::text[], $2::text[]) AS wanted(operation_id, target) USING (operation_id, target)
		WHERE NOT $3::boolean OR EXISTS (SELECT 1 FROM records observation JOIN records product ON product.kind='products' AND product.id=observation.product_id
		WHERE observation.kind='runtime_observations' AND observation.id=f.observation_id AND (`+fmt.Sprintf(privateProductDeniedSQL, "product.", "$4")+`) IS NOT TRUE)`, operations, targets, restricted, user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, target string
		var f domain.RuntimeFreshness
		if err = rows.Scan(&id, &target, &f.ObservationID, &f.ObservedAt, &f.Healthy); err != nil {
			return nil, err
		}
		out[domain.RuntimeObservationKey(id, target)] = f
	}
	return out, rows.Err()
}

var runtimeFreshnessBucket = []byte("runtime-observation-freshness")

type localRuntimeFreshness struct {
	ObservationID string    `json:"observation_id"`
	ObservedAt    time.Time `json:"observed_at"`
	Healthy       bool      `json:"healthy"`
}

func (s *LocalStore) TouchRuntimeFreshness(ctx context.Context, operationID string, target string, observationID string, observedAt time.Time, healthy bool) error {
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
		if raw := b.Get([]byte(domain.RuntimeObservationKey(operationID, target))); raw != nil {
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
		return b.Put([]byte(domain.RuntimeObservationKey(operationID, target)), raw)
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
				var value localRuntimeFreshness
				if err := json.Unmarshal(raw, &value); err != nil {
					return err
				}
				out[id] = domain.RuntimeFreshness{ObservationID: value.ObservationID, ObservedAt: value.ObservedAt, Healthy: value.Healthy}
			}
		}
		if _, restricted := auth.PrivateProductViewer(ctx); restricted && len(out) > 0 {
			data := tx.Bucket(localBucket).Get(localStateKey)
			hidden, err := hiddenLocalProducts(ctx, data)
			if err != nil {
				return err
			}
			products := map[string]bool{}
			if err := historyScan(ctx, data, "products", func(raw []byte) error {
				id := scopeText(raw, "id")
				products[id] = !hidden[id]
				return nil
			}); err != nil {
				return err
			}
			visibleObservations := make(map[string]bool, len(out))
			for _, freshness := range out {
				visibleObservations[freshness.ObservationID] = false
			}
			if err := historyScan(ctx, data, "runtime_observations", func(raw []byte) error {
				id := scopeText(raw, "id")
				if _, wanted := visibleObservations[id]; wanted && products[scopeText(raw, "product_id")] {
					visibleObservations[id] = true
				}
				return nil
			}); err != nil {
				return err
			}
			for key, freshness := range out {
				if !visibleObservations[freshness.ObservationID] {
					delete(out, key)
				}
			}
		}
		return nil
	})
	return out, err
}
