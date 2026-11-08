package persistence

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"releasecontrol/internal/domain"
)

// ReadContext avoids loading unrelated audit/operation/runtime history. Product
// mode preserves the project-context contract; feature mode is an explicitly
// bounded agent summary with SQL-side excerpts before data crosses the wire.
func (s *Store) ReadContext(ctx context.Context, product, feature string) (domain.State, error) {
	groups := map[string][]json.RawMessage{}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return domain.State{}, err
	}
	defer tx.Rollback(ctx)
	if feature != "" {
		err = tx.QueryRow(ctx, `SELECT product_id FROM records WHERE kind='features' AND id=$1`, feature).Scan(&product)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.EmptyState(), nil
		}
		if err != nil {
			return domain.State{}, err
		}
	}
	var rows pgx.Rows
	if feature == "" {
		rows, err = tx.Query(ctx, `SELECT kind,document FROM records WHERE (kind='products' AND id=$1) OR (product_id=$1 AND kind IN ('repositories','applications','environments','workspace_documents','local_checkouts')) OR id=(SELECT id FROM records WHERE kind='project_knowledge' AND product_id=$1 ORDER BY position DESC,id DESC LIMIT 1) ORDER BY kind,position,id`, product)
	} else {
		// Each branch is independently bounded. Large prose and old snapshots are
		// excluded at SQL level rather than decoded and discarded by the service.
		rows, err = tx.Query(ctx, `SELECT kind,document FROM (
   (SELECT kind,jsonb_build_object('id',id,'product_id',product_id,'title',left(document->>'title',256),'status',document->>'status','goal',left(document->>'goal',1200),'owner',left(document->>'owner',128)) AS document,position FROM records WHERE kind='features' AND id=$2 LIMIT 1)
   UNION ALL
   (SELECT kind,jsonb_build_object('id',id,'product_id',product_id,'feature_id',feature_id,'title',left(document->>'title',256),'status',document->>'status','objective',left(document->>'objective',600),'owner',left(document->>'owner',128),'position',document->'position') AS document,position FROM records WHERE kind='integrations' AND product_id=$1 AND feature_id=$2 ORDER BY position,id LIMIT 101)
   UNION ALL
   (SELECT kind,jsonb_build_object('id',id,'product_id',product_id,'feature_id',feature_id,'integration_id',document->>'integration_id','kind',document->>'kind','title',left(document->>'title',256),'body',left(document->>'body',800),'current',left(document->>'current',400),'created_at',document->'created_at','updated_at',document->'updated_at','status',document->>'status','reason',left(document->>'reason',400),'completed', (SELECT coalesce(jsonb_agg(left(item,256)), '[]'::jsonb) FROM (SELECT value AS item FROM jsonb_array_elements_text(CASE WHEN jsonb_typeof(document->'completed')='array' THEN document->'completed' ELSE '[]'::jsonb END) WITH ORDINALITY AS a(value,ord) ORDER BY ord LIMIT 5) excerpt),'remaining', (SELECT coalesce(jsonb_agg(left(item,256)), '[]'::jsonb) FROM (SELECT value AS item FROM jsonb_array_elements_text(CASE WHEN jsonb_typeof(document->'remaining')='array' THEN document->'remaining' ELSE '[]'::jsonb END) WITH ORDINALITY AS a(value,ord) ORDER BY ord LIMIT 5) excerpt),'next', (SELECT coalesce(jsonb_agg(left(item,256)), '[]'::jsonb) FROM (SELECT value AS item FROM jsonb_array_elements_text(CASE WHEN jsonb_typeof(document->'next')='array' THEN document->'next' ELSE '[]'::jsonb END) WITH ORDINALITY AS a(value,ord) ORDER BY ord LIMIT 5) excerpt),'warnings', (SELECT coalesce(jsonb_agg(left(item,256)), '[]'::jsonb) FROM (SELECT value AS item FROM jsonb_array_elements_text(CASE WHEN jsonb_typeof(document->'warnings')='array' THEN document->'warnings' ELSE '[]'::jsonb END) WITH ORDINALITY AS a(value,ord) ORDER BY ord LIMIT 5) excerpt)) AS document,position FROM records WHERE kind='memories' AND product_id=$1 AND feature_id=$2 ORDER BY position DESC,id DESC LIMIT 21)
  ) bounded ORDER BY kind,position`, product, feature)
	}
	if err != nil {
		return domain.State{}, err
	}
	for rows.Next() {
		var kind string
		var doc []byte
		if err = rows.Scan(&kind, &doc); err != nil {
			rows.Close()
			return domain.State{}, err
		}
		groups[kind] = append(groups[kind], json.RawMessage(doc))
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
