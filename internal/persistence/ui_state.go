package persistence

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/jackc/pgx/v5"
	"releasecontrol/internal/domain"
	"slices"
	"sort"
)

const uiCollectionLimit = 1000
const uiHistoryLimit = 100

var uiOmitted = []string{"runtime_observations", "project_knowledge", "local_checkouts", "workspace_documents", "worker_jobs", "worker_attempts", "operation_steps", "memories", "events", "git_observations", "runtime_snapshots"}

func uiProjection(q domain.UIStateRequest) domain.UIProjection {
	return domain.UIProjection{Kind: "bounded_browser", ProductID: q.ProductID, FeatureID: q.FeatureID, CollectionLimit: uiCollectionLimit, HistoryLimit: uiHistoryLimit, Truncated: []string{}, Guidance: "Browser view only. Collections show at most 1000 recent records; selected feature history shows at most 100. Background refresh history and raw runtime/audit payloads are omitted. Full history remains in operator snapshots/backups and scoped context queries."}
}
func (s *Store) ReadUIState(ctx context.Context, q domain.UIStateRequest) (domain.UIState, error) {
	out := domain.UIState{State: domain.EmptyState()}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	// Resolve deep links before selecting product-owned rows. Unauthorized features cannot select a scope.
	if q.FeatureID != "" {
		var p string
		err = tx.QueryRow(ctx, `SELECT product_id FROM records WHERE kind='features' AND id=$1 AND (NOT $2::boolean OR product_id=ANY($3::text[]))`, q.FeatureID, q.RestrictProducts, q.AllowedProductIDs).Scan(&p)
		if err == nil {
			q.ProductID = p
		} else if err != pgx.ErrNoRows {
			return out, err
		} else {
			q.FeatureID = ""
		}
	}
	if q.ProductID == "" {
		err = tx.QueryRow(ctx, `SELECT id FROM records WHERE kind='products' AND (NOT $1::boolean OR id=ANY($2::text[])) ORDER BY position,id LIMIT 1`, q.RestrictProducts, q.AllowedProductIDs).Scan(&q.ProductID)
		if err != nil && err != pgx.ErrNoRows {
			return out, err
		}
	}
	if q.RestrictProducts && !slices.Contains(q.AllowedProductIDs, q.ProductID) {
		return out, fmt.Errorf("UI product unavailable")
	}
	out.Projection = uiProjection(q)
	// Filtering and limits are applied in SQL before documents cross the connection.
	rows, err := tx.Query(ctx, `WITH selected AS (
 SELECT kind,CASE WHEN kind='features' THEN jsonb_build_object('id',id,'product_id',product_id,'title',left(coalesce(document->>'title',''),512),'status',document->>'status','owner',left(coalesce(document->>'owner',''),512),'created_at',document->'created_at','updated_at',document->'updated_at') WHEN kind='products' AND id<>$1 THEN jsonb_build_object('id',id,'name',left(coalesce(document->>'name',''),512),'created_at',document->'created_at','updated_at',document->'updated_at') ELSE document END AS document,position,id,row_number() OVER(PARTITION BY kind ORDER BY position DESC,id) AS n FROM records
 WHERE ((kind IN ('products','features') AND (NOT $2::boolean OR (CASE WHEN kind='products' THEN id ELSE product_id END)=ANY($3::text[])))
 OR (kind<>ALL($4::text[]) AND kind NOT IN ('products','features','operations') AND (product_id=$1 OR coalesce(product_id,'')='')))
 ) SELECT kind,document,n FROM selected WHERE n<=CASE WHEN kind IN ('git_observations','runtime_snapshots') THEN 51 ELSE $5 END ORDER BY kind,position,id`, q.ProductID, q.RestrictProducts, q.AllowedProductIDs, uiOmitted, uiCollectionLimit+1)
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
		rows, err = tx.Query(ctx, `SELECT document FROM (SELECT DISTINCT ON (`+spec.keys+`) document,position FROM records WHERE kind=$1 AND product_id=$2 ORDER BY `+spec.keys+`,position DESC) latest ORDER BY position LIMIT 1000`, spec.kind, q.ProductID)
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
	rows, err = tx.Query(ctx, `SELECT document FROM (SELECT DISTINCT ON (document->>'kind',coalesce(document->>'integration_id',''),coalesce(document->>'environment_id','')) document,position FROM records WHERE kind='operations' AND product_id=$1 AND document->>'kind' IN ('REFRESH_GIT','REFRESH_RUNTIME') ORDER BY document->>'kind',coalesce(document->>'integration_id',''),coalesce(document->>'environment_id',''),position DESC) latest ORDER BY position DESC LIMIT 100`, q.ProductID)
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
	rows, err = tx.Query(ctx, `SELECT document FROM records WHERE kind='operations' AND id=ANY($1::text[]) ORDER BY position,id`, ids)
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
	rows, err = tx.Query(ctx, `SELECT jsonb_set(document,'{detail}',to_jsonb(left(coalesce(document->>'detail',''),4096))) FROM records WHERE kind='operation_steps' AND product_id=$1 AND document->>'operation_id'=ANY($2::text[]) ORDER BY position DESC LIMIT 1000`, q.ProductID, ids)
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
			column := uiMemorySQL
			if kind == "events" {
				column = "document - 'data'"
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
		} else if err != pgx.ErrNoRows {
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
func (s *LocalStore) ReadUIState(ctx context.Context, q domain.UIStateRequest) (domain.UIState, error) {
	st, err := s.Read(ctx)
	out := domain.UIState{State: domain.EmptyState()}
	if err != nil {
		return out, err
	}
	allowed := func(p string) bool { return !q.RestrictProducts || slices.Contains(q.AllowedProductIDs, p) }
	for _, f := range st.Features {
		if f.ID == q.FeatureID && allowed(f.ProductID) {
			q.ProductID = f.ProductID
		}
	}
	if q.ProductID == "" {
		for _, p := range st.Products {
			if allowed(p.ID) {
				q.ProductID = p.ID
				break
			}
		}
	}
	groups, err := codecDocuments(st)
	if err != nil {
		return out, err
	}
	out.Projection = uiProjection(q)
	selected := map[string][]json.RawMessage{}
	for kind, docs := range groups {
		if slices.Contains(uiOmitted, kind) && kind != "git_observations" && kind != "runtime_snapshots" {
			continue
		}
		for _, d := range docs {
			var m struct{ ID, ProductID, FeatureID, Kind string }
			var row map[string]json.RawMessage
			_ = json.Unmarshal(d, &row)
			_ = json.Unmarshal(row["id"], &m.ID)
			_ = json.Unmarshal(row["product_id"], &m.ProductID)
			_ = json.Unmarshal(row["feature_id"], &m.FeatureID)
			_ = json.Unmarshal(row["kind"], &m.Kind)
			p := m.ProductID
			if kind == "products" {
				p = m.ID
			}
			if !allowed(p) && p != "" {
				continue
			}
			if kind != "products" && kind != "features" && p != "" && p != q.ProductID {
				continue
			}
			if kind == "operations" && len(m.Kind) >= 7 && m.Kind[:7] == "REFRESH" {
				continue
			}
			if (kind == "features" && m.ID != q.FeatureID) || (kind == "products" && m.ID != q.ProductID) {
				header := map[string]json.RawMessage{}
				for _, key := range []string{"id", "product_id", "title", "status", "owner", "created_at", "updated_at", "name"} {
					if v, ok := row[key]; ok {
						if key == "title" || key == "name" || key == "owner" {
							var label string
							_ = json.Unmarshal(v, &label)
							r := []rune(label)
							if len(r) > 512 {
								label = string(r[:512])
							}
							v, _ = json.Marshal(label)
						}
						header[key] = v
					}
				}
				d, _ = json.Marshal(header)
			}
			selected[kind] = append(selected[kind], d)
		}
		if len(selected[kind]) > uiCollectionLimit {
			out.Projection.Truncated = append(out.Projection.Truncated, kind)
			selected[kind] = selected[kind][len(selected[kind])-uiCollectionLimit:]
		}
	}
	for _, kind := range []string{"memories", "events"} {
		if q.FeatureID == "" {
			continue
		}
		var docs []json.RawMessage
		if kind == "events" {
			for _, e := range st.Events {
				if e.ProductID == q.ProductID && e.FeatureID == q.FeatureID {
					b, _ := json.Marshal(e)
					docs = append(docs, b)
				}
			}
		} else {
			for _, m := range st.Memories {
				if m.ProductID == q.ProductID && m.FeatureID == q.FeatureID {
					b, _ := json.Marshal(m)
					docs = append(docs, b)
				}
			}
		}
		if len(docs) > uiHistoryLimit {
			docs = docs[len(docs)-uiHistoryLimit:]
			out.Projection.Truncated = append(out.Projection.Truncated, kind)
		}
		selected[kind] = docs
	}
	sort.Strings(out.Projection.Truncated)
	if err = budgetUIGroups(selected, &out.Projection); err != nil {
		return out, err
	}
	out.State, err = decodeStoredDocuments(selected)
	return out, err
}

const uiMemorySQL = `jsonb_build_object('id',document->'id','product_id',document->'product_id','feature_id',document->'feature_id','integration_id',document->'integration_id','actor',document->'actor','created_at',document->'created_at','updated_at',document->'updated_at','kind',document->'kind','status',document->'status','commit',document->'commit','deployment',document->'deployment','session',document->'session','title',left(coalesce(document->>'title',''),8192),'body',left(coalesce(document->>'body',''),8192),'reason',left(coalesce(document->>'reason',''),8192),'current',left(coalesce(document->>'current',''),8192),'resolution',left(coalesce(document->>'resolution',''),8192),'completed',coalesce((SELECT jsonb_agg(left(value,512)) FROM jsonb_array_elements_text(CASE WHEN jsonb_typeof(document->'completed')='array' THEN document->'completed' ELSE '[]'::jsonb END) WITH ORDINALITY AS a(value,n) WHERE n<=20),'[]'::jsonb),'remaining',coalesce((SELECT jsonb_agg(left(value,512)) FROM jsonb_array_elements_text(CASE WHEN jsonb_typeof(document->'remaining')='array' THEN document->'remaining' ELSE '[]'::jsonb END) WITH ORDINALITY AS a(value,n) WHERE n<=20),'[]'::jsonb),'next',coalesce((SELECT jsonb_agg(left(value,512)) FROM jsonb_array_elements_text(CASE WHEN jsonb_typeof(document->'next')='array' THEN document->'next' ELSE '[]'::jsonb END) WITH ORDINALITY AS a(value,n) WHERE n<=20),'[]'::jsonb),'warnings',coalesce((SELECT jsonb_agg(left(value,512)) FROM jsonb_array_elements_text(CASE WHEN jsonb_typeof(document->'warnings')='array' THEN document->'warnings' ELSE '[]'::jsonb END) WITH ORDINALITY AS a(value,n) WHERE n<=20),'[]'::jsonb))`

// Bound the assembled response as well as each SQL window. History is a read-only
// excerpt: dropping older rows here never changes persisted records or editable plans.
func budgetUIGroups(groups map[string][]json.RawMessage, p *domain.UIProjection) error {
	total := 0
	for _, docs := range groups {
		for _, d := range docs {
			total += len(d)
		}
	}
	for _, kind := range []string{"events", "memories", "operation_steps", "git_observations", "runtime_snapshots"} {
		for total > 7<<20 && len(groups[kind]) > 0 {
			d := groups[kind][0]
			groups[kind] = groups[kind][1:]
			total -= len(d)
			if !slices.Contains(p.Truncated, kind) {
				p.Truncated = append(p.Truncated, kind)
			}
		}
	}
	if total > 7<<20 {
		return fmt.Errorf("editable browser records exceed response budget; select a narrower feature scope")
	}
	return nil
}
