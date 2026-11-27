package persistence

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"releasecontrol/internal/domain"
	"slices"
)

const sqliteUIMemorySQL = `json_object('id',document->'id','product_id',document->'product_id','feature_id',document->'feature_id','integration_id',document->'integration_id','actor',document->'actor','created_at',document->'created_at','updated_at',document->'updated_at','kind',document->'kind','status',document->'status','commit',document->'commit','deployment',document->'deployment','session',document->'session','title',substr(coalesce(document->>'title',''),1,8192),'body',substr(coalesce(document->>'body',''),1,8192),'reason',substr(coalesce(document->>'reason',''),1,8192),'current',substr(coalesce(document->>'current',''),1,8192),'resolution',substr(coalesce(document->>'resolution',''),1,8192),'completed',json((SELECT json_group_array(substr(value,1,512)) FROM json_each(document,'$.completed') WHERE type='text' AND key<20)),'remaining',json((SELECT json_group_array(substr(value,1,512)) FROM json_each(document,'$.remaining') WHERE type='text' AND key<20)),'next',json((SELECT json_group_array(substr(value,1,512)) FROM json_each(document,'$.next') WHERE type='text' AND key<20)),'warnings',json((SELECT json_group_array(substr(value,1,512)) FROM json_each(document,'$.warnings') WHERE type='text' AND key<20)))`

// Bound the assembled response as well as each SQL window. History is a read-only
// excerpt: dropping older rows here never changes persisted records or editable plans.
func (s *SQLiteStore) ReadUIState(ctx context.Context, q domain.UIStateRequest) (domain.UIState, error) {
	out := domain.UIState{State: domain.EmptyState()}
	tx, err := s.begin(ctx, true)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	hidden, err := sqliteHiddenProducts(ctx, tx)
	if err != nil {
		return out, err
	}
	if q.ProductID != "" {
		visible, err := sqliteVisibleProductIn(ctx, tx, q.ProductID, hidden)
		if err != nil {
			return out, err
		}
		if !visible || q.RestrictProducts && !slices.Contains(q.AllowedProductIDs, q.ProductID) {
			return out, domain.ErrReadScopeNotFound
		}
	}
	// Resolve deep links before selecting product-owned rows. Unauthorized features cannot select a scope.
	if q.FeatureID != "" {
		var p string
		err = tx.QueryRow(ctx, `SELECT product_id FROM records WHERE kind='features' AND id=$1 AND (NOT $2 OR product_id IN (SELECT value FROM json_each($3))) AND product_id  NOT IN (SELECT value FROM json_each($4))`, q.FeatureID, q.RestrictProducts, q.AllowedProductIDs, hidden).Scan(&p)
		if err == nil {
			q.ProductID = p
		} else if err != sql.ErrNoRows {
			return out, err
		} else {
			return out, domain.ErrReadScopeNotFound
		}
	}
	if q.ProductID == "" {
		err = tx.QueryRow(ctx, `SELECT id FROM records WHERE kind='products' AND (NOT $1 OR id IN (SELECT value FROM json_each($2))) AND id  NOT IN (SELECT value FROM json_each($3)) ORDER BY position,id LIMIT 1`, q.RestrictProducts, q.AllowedProductIDs, hidden).Scan(&q.ProductID)
		if err != nil && err != sql.ErrNoRows {
			return out, err
		}
	}
	out.Projection = uiProjection(q)
	// Filtering and limits are applied in SQL before documents cross the connection.
	rows, err := tx.Query(ctx, `WITH selected AS (
 SELECT kind,CASE WHEN kind='features' THEN json_object('id',id,'product_id',product_id,'title',substr(coalesce(document->>'title',''),1,512),'status',document->>'status','owner',substr(coalesce(document->>'owner',''),1,512),'created_at',document->'created_at','updated_at',document->'updated_at') WHEN kind='products' AND id<>$1 THEN json_object('id',id,'name',substr(coalesce(document->>'name',''),1,512),'created_at',document->'created_at','updated_at',document->'updated_at') ELSE document END AS document,position,id,row_number() OVER(PARTITION BY kind ORDER BY (kind='products' AND id=$1) DESC,position DESC,id) AS n FROM records
 WHERE (CASE WHEN kind='products' THEN id ELSE coalesce(product_id,'') END)  NOT IN (SELECT value FROM json_each($6)) AND ((kind IN ('products','features') AND (NOT $2 OR (CASE WHEN kind='products' THEN id ELSE product_id END) IN (SELECT value FROM json_each($3))))
 OR (kind NOT IN (SELECT value FROM json_each($4)) AND kind NOT IN ('products','features','operations') AND (product_id=$1 OR coalesce(product_id,'')='')))
	) SELECT kind,document,n FROM selected WHERE n<=CASE WHEN kind IN ('git_observations','runtime_snapshots') THEN 51 WHEN kind='prod_promotions' THEN 101 ELSE $5 END ORDER BY kind,position,id`, q.ProductID, q.RestrictProducts, q.AllowedProductIDs, uiOmitted, uiCollectionLimit+1, hidden)
	if err != nil {
		return out, err
	}
	groups := map[string][]json.RawMessage{}
	bytes := 0
	for rows.Next() {
		var kind string
		var doc []byte
		var n int
		if err = rows.Scan(&kind, &doc, &n); err != nil {
			rows.Close()
			return out, err
		}
		limit := uiCollectionLimit
		if kind == "prod_promotions" {
			limit = 100
		}
		if kind == "git_observations" || kind == "runtime_snapshots" {
			limit = 50
		}
		if n > limit {
			out.Projection.Truncated = append(out.Projection.Truncated, kind)
			continue
		}
		bytes += len(doc)
		if bytes > 8<<20 {
			rows.Close()
			return out, fmt.Errorf("browser projection exceeds 8 MiB budget; narrow the scope")
		}
		groups[kind] = append(groups[kind], json.RawMessage(doc))
	}
	if err = rows.Err(); err != nil {
		return out, err
	}
	rows.Close()
	// Materialized latest observations use identity groups, not an arbitrary history window.
	for _, spec := range []struct{ kind, keys string }{{"git_observations", "coalesce(document->>'integration_id',''),coalesce(document->>'repository_id',''),coalesce(document->>'branch','')"}, {"runtime_snapshots", "coalesce(document->>'environment_id',''),coalesce(document->>'application_id',''),coalesce(document->'runtime'->>'context',''),coalesce(document->'runtime'->>'namespace',''),coalesce(document->'runtime'->>'deployment',''),coalesce(document->'runtime'->>'container','')"}} {
		rows, err = tx.Query(ctx, `SELECT document FROM (SELECT document,position,row_number() OVER(PARTITION BY `+spec.keys+` ORDER BY position DESC,id DESC) AS rn FROM records WHERE kind=$1 AND product_id=$2) latest WHERE rn=1 ORDER BY position LIMIT 1000`, spec.kind, q.ProductID)
		if err != nil {
			return out, err
		}
		for rows.Next() {
			var d []byte
			if err = rows.Scan(&d); err != nil {
				return out, err
			}
			groups[spec.kind] = append(groups[spec.kind], json.RawMessage(d))
		}
		if err = rows.Err(); err != nil {
			return out, err
		}
		rows.Close()
	}
	rows, err = tx.Query(ctx, `SELECT document FROM records WHERE kind='operations' AND product_id=$1 AND coalesce(document->>'kind','') NOT LIKE 'REFRESH%' ORDER BY position DESC LIMIT 201`, q.ProductID)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var d []byte
		if err = rows.Scan(&d); err != nil {
			return out, err
		}
		if len(groups["operations"]) >= 200 {
			out.Projection.Truncated = append(out.Projection.Truncated, "operations")
			break
		}
		groups["operations"] = append(groups["operations"], json.RawMessage(d))
	}
	if err = rows.Err(); err != nil {
		return out, err
	}
	rows.Close()
	// Only recent refresh statuses per integration/environment, rather than every historical scan.
	rows, err = tx.Query(ctx, `SELECT document FROM (SELECT document,position,row_number() OVER(PARTITION BY document->>'kind',coalesce(document->>'integration_id',''),coalesce(document->>'environment_id','') ORDER BY position DESC,id DESC) AS rn FROM records WHERE kind='operations' AND product_id=$1 AND document->>'kind' IN ('REFRESH_GIT','REFRESH_RUNTIME')) latest WHERE rn=1 ORDER BY position DESC LIMIT 100`, q.ProductID)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var d []byte
		if err = rows.Scan(&d); err != nil {
			return out, err
		}
		groups["operations"] = append(groups["operations"], json.RawMessage(d))
	}
	if err = rows.Err(); err != nil {
		return out, err
	}
	rows.Close()
	ids := []string{}
	for _, d := range groups["operations"] {
		var v struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(d, &v)
		ids = append(ids, v.ID)
	}
	// Consumers expect append order after the newest windows have been chosen.
	rows, err = tx.Query(ctx, `SELECT document FROM records WHERE kind='operations' AND product_id=$2 AND id IN (SELECT value FROM json_each($1)) ORDER BY position,id`, ids, q.ProductID)
	if err != nil {
		return out, err
	}
	groups["operations"] = nil
	for rows.Next() {
		var d []byte
		if err = rows.Scan(&d); err != nil {
			return out, err
		}
		groups["operations"] = append(groups["operations"], json.RawMessage(d))
	}
	if err = rows.Err(); err != nil {
		return out, err
	}
	rows.Close()
	rows, err = tx.Query(ctx, `SELECT json_set(document,'$.detail',substr(coalesce(document->>'detail',''),1,4096)) FROM records WHERE kind='operation_steps' AND product_id=$1 AND document->>'operation_id' IN (SELECT value FROM json_each($2)) ORDER BY position DESC LIMIT 1000`, q.ProductID, ids)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var d []byte
		if err = rows.Scan(&d); err != nil {
			return out, err
		}
		groups["operation_steps"] = append(groups["operation_steps"], json.RawMessage(d))
	}
	if err = rows.Err(); err != nil {
		return out, err
	}
	rows.Close()
	slices.Reverse(groups["operation_steps"])
	if q.FeatureID != "" {
		for _, kind := range []string{"memories", "events"} {
			table := "records"
			filter := "kind='memories' AND "
			order := "position"
			if kind == "events" {
				table = "events"
				filter = ""
				order = "sequence"
			}
			column := sqliteUIMemorySQL
			if kind == "events" {
				column = "json_remove(document,'$.data')"
			}
			rows, err = tx.Query(ctx, `SELECT `+column+` FROM `+table+` WHERE `+filter+`product_id=$1 AND feature_id=$2 ORDER BY `+order+` DESC LIMIT 101`, q.ProductID, q.FeatureID)
			if err != nil {
				return out, err
			}
			docs := []json.RawMessage{}
			for rows.Next() {
				var d []byte
				if err = rows.Scan(&d); err != nil {
					return out, err
				}
				docs = append(docs, json.RawMessage(d))
			}
			if err = rows.Err(); err != nil {
				return out, err
			}
			rows.Close()
			if len(docs) > uiHistoryLimit {
				out.Projection.Truncated = append(out.Projection.Truncated, kind)
				docs = docs[:uiHistoryLimit]
			}
			slices.Reverse(docs)
			groups[kind] = docs
		}
	}
	// A selected feature must remain available even outside the navigation window.
	if q.FeatureID != "" {
		var doc []byte
		err = tx.QueryRow(ctx, `SELECT document FROM records WHERE kind='features' AND id=$1 AND product_id=$2`, q.FeatureID, q.ProductID).Scan(&doc)
		if err == nil {
			found := false
			for i, d := range groups["features"] {
				var v struct {
					ID string `json:"id"`
				}
				_ = json.Unmarshal(d, &v)
				if v.ID == q.FeatureID {
					groups["features"][i] = json.RawMessage(doc)
					found = true
					break
				}
			}
			if !found {
				groups["features"] = append(groups["features"], json.RawMessage(doc))
			}
		} else if err != sql.ErrNoRows {
			return out, err
		}
	}
	if err = budgetUIGroups(groups, &out.Projection); err != nil {
		return out, err
	}
	out.State, err = decodeStoredDocuments(groups)
	if err != nil {
		return out, err
	}
	return out, tx.Commit(ctx)
}

// Embedded installations retain a monolithic value; filter before returning it.
