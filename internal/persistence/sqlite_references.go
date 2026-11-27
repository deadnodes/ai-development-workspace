package persistence

import (
	"context"
	"encoding/json"
	"releasecontrol/internal/auth"
	"releasecontrol/internal/domain"
)

const sqliteReferenceClosure = `WITH RECURSIVE refs(id) AS (
 SELECT value FROM json_each($1)
 UNION
 SELECT target.value FROM refs JOIN records r ON r.id=refs.id
 CROSS JOIN json_each(json_array(r.document->>'environment_id',r.document->>'composition_id',r.document->>'candidate_operation_id',r.document->>'operation_id',r.document->>'parent_id')) target
 WHERE target.value IS NOT NULL AND target.value<>''
) `

func (s *SQLiteStore) ReadReferences(ctx context.Context, ids []string) (domain.State, error) {
	tx, err := s.begin(ctx, true)
	if err != nil {
		return domain.State{}, err
	}
	defer tx.Rollback(context.Background())
	hidden, err := sqliteHiddenProducts(ctx, tx)
	if err != nil {
		return domain.State{}, err
	}
	const owner = `(CASE WHEN kind='products' THEN id ELSE coalesce(product_id,'') END)`
	restricted := auth.RequiresExistingProduct(ctx)
	const allowedRecord = `(` + owner + ` = '' OR (` + owner + `  NOT IN (SELECT value FROM json_each($2)) AND (NOT $3 OR ` + owner + ` IN (SELECT id FROM records WHERE kind='products'))))`
	const allowedEvent = `(CASE WHEN coalesce(product_id,'') = '' THEN NOT $3 OR NOT (document->>'action' IN ('create_product','restore_backup_evidence') OR (document->>'action'='restore_backup' AND EXISTS (SELECT 1 FROM json_each(document,'$.data.data') WHERE key IN ('interrupted_operations','quarantined_builds','quarantined_deployments','quarantined_signatures')))) ELSE product_id  NOT IN (SELECT value FROM json_each($2)) AND (NOT $3 OR product_id IN (SELECT id FROM records WHERE kind='products')) END)`
	rows, err := tx.Query(ctx, sqliteReferenceClosure+`
 SELECT kind,document FROM records WHERE (id IN(SELECT id FROM refs) OR kind='environment_bindings') AND `+allowedRecord+`
 UNION ALL SELECT 'events',document FROM events WHERE id IN(SELECT id FROM refs) AND `+allowedEvent+`
 UNION ALL SELECT '', 'null' WHERE EXISTS (SELECT 1 FROM records WHERE id IN(SELECT id FROM refs) AND NOT `+allowedRecord+`)
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
