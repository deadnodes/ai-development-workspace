package persistence

import (
	"context"
	"fmt"
	"releasecontrol/internal/auth"
	"releasecontrol/internal/domain"
	"strings"
	"time"
)

func (s *SQLiteStore) TouchRuntimeFreshness(ctx context.Context, operationID string, target string, observationID string, observedAt time.Time, healthy bool) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO runtime_observation_freshness(operation_id, target, observation_id, observed_at, healthy) VALUES($1,$2,$3,$4,$5) ON CONFLICT (operation_id, target) DO UPDATE SET observation_id=EXCLUDED.observation_id, observed_at=EXCLUDED.observed_at, healthy=EXCLUDED.healthy WHERE runtime_observation_freshness.observed_at <= EXCLUDED.observed_at`, sqliteArgs([]any{operationID, target, observationID, observedAt, healthy})...)
	return err
}
func (s *SQLiteStore) RuntimeFreshness(ctx context.Context, ids []string) (map[string]domain.RuntimeFreshness, error) {
	out := map[string]domain.RuntimeFreshness{}
	if len(ids) == 0 {
		return out, nil
	}
	operations, targets := make([]string, len(ids)), make([]string, len(ids))
	for i, key := range ids {
		operations[i], targets[i], _ = strings.Cut(key, "\x00")
	}
	user, restricted := auth.PrivateProductViewer(ctx)
	tx, err := s.begin(ctx, true)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT f.operation_id, f.target, f.observation_id, f.observed_at, f.healthy
		FROM runtime_observation_freshness f JOIN json_each($1) operations ON f.operation_id=operations.value JOIN json_each($2) targets ON targets.key=operations.key AND f.target=targets.value
		WHERE NOT $3 OR EXISTS (SELECT 1 FROM records observation JOIN records product ON product.kind='products' AND product.id=observation.product_id
		WHERE observation.kind='runtime_observations' AND observation.id=f.observation_id AND (NOT $5 OR (`+fmt.Sprintf(sqlitePrivateProductDeniedSQL, "product.", "$4")+`) IS NOT TRUE))`, operations, targets, auth.RequiresExistingProduct(ctx), user, restricted)
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
