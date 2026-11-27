package persistence

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"releasecontrol/internal/domain"
)

// ReadContext avoids loading unrelated audit/operation/runtime history. Product
// mode preserves the project-context contract; feature mode is an explicitly
// bounded agent summary with SQL-side excerpts before data crosses the wire.
func (s *SQLiteStore) ReadContext(ctx context.Context, product, feature string) (domain.State, error) {
	groups := map[string][]json.RawMessage{}
	tx, err := s.begin(ctx, true)
	if err != nil {
		return domain.State{}, err
	}
	defer tx.Rollback(ctx)
	hidden, err := sqliteHiddenProducts(ctx, tx)
	if err != nil {
		return domain.State{}, err
	}
	if feature != "" {
		err = tx.QueryRow(ctx, `SELECT product_id FROM records WHERE kind='features' AND id=$1 AND product_id  NOT IN (SELECT value FROM json_each($2)) AND ($3='' OR product_id=$3)`, feature, hidden, product).Scan(&product)
		if errors.Is(err, sql.ErrNoRows) {
			return domain.EmptyState(), nil
		}
		if err != nil {
			return domain.State{}, err
		}
	}
	visible, err := sqliteVisibleProductIn(ctx, tx, product, hidden)
	if err != nil {
		return domain.State{}, err
	}
	if !visible {
		return domain.EmptyState(), nil
	}
	var rows stateRows
	if feature == "" {
		rows, err = tx.Query(ctx, `SELECT kind,document FROM records WHERE (kind='products' AND id=$1) OR (product_id=$1 AND (kind IN ('repositories','applications','environments','workspace_documents','local_checkouts') OR (kind='project_knowledge' AND id=(SELECT id FROM records WHERE kind='project_knowledge' AND product_id=$1 ORDER BY position DESC,id DESC LIMIT 1)))) ORDER BY kind,position,id`, product)
	} else {
		// Each branch is independently bounded. Large prose and old snapshots are
		// excluded at SQL level rather than decoded and discarded by the service.
		rows, err = tx.Query(ctx, `SELECT kind,document FROM (
	   SELECT * FROM (SELECT kind,document,position FROM records WHERE kind='products' AND id=$1)
	   UNION ALL
	   SELECT * FROM (SELECT kind,json_object('id',id,'product_id',product_id,'title',substr(document->>'title',1,256),'status',document->>'status','goal',substr(document->>'goal',1,1200),'owner',substr(document->>'owner',1,128)) AS document,position FROM records WHERE kind='features' AND id=$2 AND product_id=$1 LIMIT 1)
   UNION ALL
   SELECT * FROM (SELECT kind,json_object('id',id,'product_id',product_id,'feature_id',feature_id,'title',substr(document->>'title',1,256),'status',document->>'status','objective',substr(document->>'objective',1,600),'owner',substr(document->>'owner',1,128),'assignee',substr(document->>'assignee',1,128),'position',document->'position') AS document,position FROM records WHERE kind='integrations' AND product_id=$1 AND feature_id=$2 ORDER BY position,id LIMIT 101)
   UNION ALL
   SELECT * FROM (SELECT kind,json_object('id',id,'product_id',product_id,'feature_id',feature_id,'integration_id',document->>'integration_id','kind',document->>'kind','title',substr(document->>'title',1,256),'body',substr(document->>'body',1,800),'current',substr(document->>'current',1,400),'created_at',document->'created_at','updated_at',document->'updated_at','status',document->>'status','reason',substr(document->>'reason',1,400),'completed', json((SELECT json_group_array(substr(value,1,256)) FROM (SELECT value FROM json_each(document,'$.completed') WHERE type='text' ORDER BY key LIMIT 5))),'remaining', json((SELECT json_group_array(substr(value,1,256)) FROM (SELECT value FROM json_each(document,'$.remaining') WHERE type='text' ORDER BY key LIMIT 5))),'next', json((SELECT json_group_array(substr(value,1,256)) FROM (SELECT value FROM json_each(document,'$.next') WHERE type='text' ORDER BY key LIMIT 5))),'question', json(CASE WHEN document->>'kind'='question' THEN json_object('kind',document->'question'->>'kind','addressed_to',substr(document->'question'->>'addressed_to',1,128),'blocking',document->'question'->'blocking') END),'warnings', json((SELECT json_group_array(substr(value,1,256)) FROM (SELECT value FROM json_each(document,'$.warnings') WHERE type='text' ORDER BY key LIMIT 5)))) AS document,position FROM records WHERE kind='memories' AND product_id=$1 AND feature_id=$2 AND id IN (
     SELECT id FROM (SELECT id FROM records WHERE kind='memories' AND product_id=$1 AND feature_id=$2 ORDER BY position DESC,id DESC LIMIT 21) recent
     UNION ALL
     SELECT id FROM (SELECT id FROM records WHERE kind='memories' AND product_id=$1 AND feature_id=$2 AND document->>'kind'='question' AND document->>'status'='open' ORDER BY position DESC,id DESC LIMIT 21) open_questions
   ))
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
