package persistence

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	bolt "go.etcd.io/bbolt"
	"releasecontrol/internal/auth"
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
	restricted := auth.RequiresExistingProduct(ctx)
	const allowedRecord = `(` + owner + ` = '' OR (` + owner + ` <> ALL($2::text[]) AND (NOT $3::boolean OR ` + owner + ` IN (SELECT id FROM records WHERE kind='products'))))`
	const allowedEvent = `(CASE WHEN coalesce(product_id,'') = '' THEN NOT $3::boolean OR NOT (document->>'action' IN ('create_product','restore_backup_evidence') OR (document->>'action'='restore_backup' AND coalesce(document->'data'->'data' ?| ARRAY['interrupted_operations','quarantined_builds','quarantined_deployments','quarantined_signatures'],false))) ELSE product_id <> ALL($2::text[]) AND (NOT $3::boolean OR product_id IN (SELECT id FROM records WHERE kind='products')) END)`
	rows, err := tx.Query(ctx, referenceClosure+`
 SELECT kind,document FROM records WHERE (id IN(SELECT id FROM refs) OR kind='environment_bindings') AND `+allowedRecord+`
 UNION ALL SELECT 'events',document FROM events WHERE id IN(SELECT id FROM refs) AND `+allowedEvent+`
 UNION ALL SELECT '', 'null'::jsonb WHERE EXISTS (SELECT 1 FROM records WHERE id IN(SELECT id FROM refs) AND NOT `+allowedRecord+`)
 OR EXISTS (SELECT 1 FROM events WHERE id IN(SELECT id FROM refs) AND `+allowedEvent+` IS NOT TRUE)`, ids, hidden, restricted)
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
		return readLocalReferences(ctx, data, ids, groups, objectFields)
	})
	if err != nil {
		return domain.State{}, err
	}
	return decodeStoredDocuments(groups)
}

func readLocalReferences(ctx context.Context, data []byte, ids []string, groups map[string][]json.RawMessage, scan func([]byte, func(string, []byte) error) error) error {
	if len(data) == 0 {
		return nil
	}
	collections := map[string][]byte{}
	kinds, err := codecStateSchema()
	if err != nil {
		return err
	}
	if err := scan(data, func(kind string, raw []byte) error {
		if _, ok := kinds[kind]; ok {
			collections[kind] = raw
		}
		return nil
	}); err != nil {
		return err
	}
	restricted := auth.RequiresExistingProduct(ctx)
	visible := map[string]bool{}
	if restricted {
		err := historyArray(ctx, collections["products"], func(raw []byte) error {
			var product domain.Product
			if err := json.Unmarshal(raw, &product); err != nil {
				return err
			}
			visible[product.ID] = productVisible(ctx, product)
			return nil
		})
		if err != nil {
			return err
		}
	}
	needed := map[string]bool{}
	seen := map[string]map[string]bool{}
	for _, id := range ids {
		needed[id] = true
	}
	for more := true; more; {
		more = false
		for kind, collection := range collections {
			if seen[kind] == nil {
				seen[kind] = map[string]bool{}
			}
			if err := historyArray(ctx, collection, func(raw []byte) error {
				id := scopeText(raw, "id")
				if !needed[id] && kind != "environment_bindings" {
					return nil
				}
				product := scopeText(raw, "product_id")
				if kind == "products" {
					product = id
				}
				unscopedEvidence := false
				if restricted && product == "" && kind == "events" {
					switch scopeText(raw, "action") {
					case "create_product", "restore_backup_evidence":
						unscopedEvidence = true
					case "restore_backup":
						for _, key := range domain.RestoreEvidenceCollections() {
							unscopedEvidence = unscopedEvidence || len(historyPath(raw, "data", "data", key)) > 0
						}
					}
				}
				if restricted && (product != "" && !visible[product] || unscopedEvidence) {
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
