package persistence

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	bolt "go.etcd.io/bbolt"
	"releasecontrol/internal/domain"
)

// Indexed reference closure used by the shared application authorization policy.
// UNION (not UNION ALL) terminates cycles without dropping any reachable ID.
const referenceClosure = `WITH RECURSIVE refs(id) AS (
 SELECT unnest($1::text[])
 UNION
 SELECT target.id FROM refs JOIN records r ON r.id=refs.id
 CROSS JOIN LATERAL unnest(ARRAY[r.document->>'environment_id',r.document->>'composition_id',r.document->>'candidate_operation_id',r.document->>'operation_id',r.document->>'parent_id']) target(id)
 WHERE target.id IS NOT NULL AND target.id<>''
) `

func (s *Store) ReadReferences(ctx context.Context, ids []string) (domain.State, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return domain.State{}, err
	}
	defer tx.Rollback(context.Background())
	hidden, err := hiddenProducts(ctx, tx)
	if err != nil {
		return domain.State{}, err
	}
	const owner = `(CASE WHEN kind='products' THEN id ELSE coalesce(product_id,'') END)`
	rows, err := tx.Query(ctx, referenceClosure+`
 SELECT kind,document FROM records WHERE (id IN(SELECT id FROM refs) OR kind='environment_bindings') AND `+owner+` <> ALL($2::text[])
 UNION ALL SELECT 'events',document FROM events WHERE id IN(SELECT id FROM refs) AND coalesce(product_id,'') <> ALL($2::text[])
 UNION ALL SELECT '', 'null'::jsonb WHERE EXISTS (SELECT 1 FROM records WHERE id IN(SELECT id FROM refs) AND `+owner+` = ANY($2::text[]))
 OR EXISTS (SELECT 1 FROM events WHERE id IN(SELECT id FROM refs) AND product_id=ANY($2::text[]))`, ids, hidden)
	if err != nil {
		return domain.State{}, err
	}
	groups := map[string][]json.RawMessage{}
	for rows.Next() {
		var kind string
		var raw []byte
		if err = rows.Scan(&kind, &raw); err != nil {
			rows.Close()
			return domain.State{}, err
		}
		if kind == "" {
			rows.Close()
			return domain.State{}, domain.ErrReadScopeNotFound
		}
		groups[kind] = append(groups[kind], json.RawMessage(raw))
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return domain.State{}, err
	}
	st, err := decodeStoredDocuments(groups)
	if err != nil {
		return st, err
	}
	return st, tx.Commit(ctx)
}

func (s *LocalStore) ReadReferences(ctx context.Context, ids []string) (domain.State, error) {
	s.lifecycle.RLock()
	defer s.lifecycle.RUnlock()
	if s.closed {
		return domain.State{}, errors.New("local database is closed")
	}
	groups := map[string][]json.RawMessage{}
	err := s.db.View(func(tx *bolt.Tx) error {
		data := tx.Bucket(localBucket).Get(localStateKey)
		hidden, err := hiddenLocalProducts(ctx, data)
		if err != nil {
			return err
		}
		kinds, err := codecStateSchema()
		if err != nil {
			return err
		}
		needed := map[string]bool{}
		seen := map[string]map[string]bool{}
		for _, id := range ids {
			needed[id] = true
		}
		for more := true; more; {
			more = false
			for kind := range kinds {
				if seen[kind] == nil {
					seen[kind] = map[string]bool{}
				}
				if err := historyScan(ctx, data, kind, func(raw []byte) error {
					id := scopeText(raw, "id")
					if !needed[id] && kind != "environment_bindings" {
						return nil
					}
					product := scopeText(raw, "product_id")
					if kind == "products" {
						product = id
					}
					if hidden[product] {
						if needed[id] {
							return domain.ErrReadScopeNotFound
						}
						return nil
					}
					if !seen[kind][id] {
						groups[kind] = append(groups[kind], append(json.RawMessage(nil), raw...))
						seen[kind][id] = true
					}
					if needed[id] && kind != "events" {
						for _, edge := range []string{"environment_id", "composition_id", "candidate_operation_id", "operation_id", "parent_id"} {
							if target := scopeText(raw, edge); target != "" && !needed[target] {
								needed[target] = true
								more = true
							}
						}
					}
					return nil
				}); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return domain.State{}, err
	}
	return decodeStoredDocuments(groups)
}
func referenceEvents(ctx context.Context, tx pgx.Tx, ids []string, known []domain.Event) ([]domain.Event, error) {
	events := append([]domain.Event(nil), known...)
	seen := map[string]bool{}
	for _, e := range known {
		seen[e.ID] = true
	}
	rows, err := tx.Query(ctx, referenceClosure+`SELECT document FROM events WHERE id IN(SELECT id FROM refs) ORDER BY sequence`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var raw []byte
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		var event domain.Event
		if err = json.Unmarshal(raw, &event); err != nil {
			return nil, err
		}
		if !seen[event.ID] {
			events = append(events, event)
			seen[event.ID] = true
		}
	}
	return events, rows.Err()
}
