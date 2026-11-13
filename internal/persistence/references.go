package persistence

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
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
	rows, err := tx.Query(ctx, referenceClosure+`SELECT kind,document FROM records WHERE id IN(SELECT id FROM refs) OR kind='environment_bindings' UNION ALL SELECT 'events',document FROM events WHERE id IN(SELECT id FROM refs)`, ids)
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
