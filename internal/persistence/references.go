package persistence

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
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

func referenceEvents(ctx context.Context, tx stateTransaction, ids []string, known []domain.Event) ([]domain.Event, error) {
	events := append([]domain.Event(nil), known...)
	seen := map[string]bool{}
	for _, e := range known {
		seen[e.ID] = true
	}
	rows, err := tx.Query(ctx, tx.sql(referenceClosure, sqliteReferenceClosure)+`SELECT document FROM events WHERE id IN(SELECT id FROM refs) ORDER BY sequence`, ids)
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

func featureRestoreEvents(ctx context.Context, tx stateTransaction, feature domain.Feature, known []domain.Event) ([]domain.Event, error) {
	rows, err := tx.Query(ctx, `WITH history AS (
		SELECT sequence, document, document->>'action' AS action, document->'data'->'data'->>'status' AS status
		FROM events WHERE product_id=$1 AND feature_id=$2
		UNION ALL
		SELECT sequence, document, 'approve_idea_promotion', NULL
		FROM events WHERE product_id=$1 AND feature_id='' AND document->>'entity_id'=$2 AND document->>'action'='approve_idea_promotion'
	), last_archive AS (
		SELECT MAX(sequence) AS sequence FROM history WHERE document->>'entity_id'=$2 AND action='update_feature' AND status='archived'
	), transitions AS (
		SELECT MAX(sequence) AS sequence FROM history WHERE sequence<(SELECT sequence FROM last_archive) AND document->>'entity_id'=$2
		AND (action IN ('create_feature','approve_idea_promotion') OR (action='update_feature' AND status IN ('idea','ready_for_execution','planned','active','blocked','completed')))
	), archive AS (
		SELECT MIN(sequence) AS sequence FROM history WHERE sequence>COALESCE((SELECT sequence FROM transitions),0)
		AND document->>'entity_id'=$2 AND action='update_feature' AND status='archived'
	), reopens AS (
		SELECT MAX(sequence) AS sequence FROM history WHERE sequence>(SELECT sequence FROM transitions) AND sequence<(SELECT sequence FROM archive) AND
		(action IN ('create_integration','update_integration','start_integration') OR (action='transition_integration' AND status IN ('planned','working','implemented','verifying')))
	)
	SELECT document FROM history WHERE sequence IN (SELECT sequence FROM archive UNION ALL SELECT sequence FROM transitions UNION ALL SELECT sequence FROM reopens)
	ORDER BY sequence`, feature.ProductID, feature.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := append([]domain.Event(nil), known...)
	for rows.Next() {
		var raw []byte
		var event domain.Event
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &event); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}
