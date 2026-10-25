package persistence

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	bolt "go.etcd.io/bbolt"
	"releasecontrol/internal/domain"
	"sort"
)

// This relation projects only frozen identity fields. It deliberately does not
// recompute execution fingerprints or treat recorded provenance as verification.
const historyCTE = `WITH jobs AS (
 SELECT j.* FROM records j WHERE j.kind='worker_jobs' AND j.product_id=$1
), managed AS (
 SELECT j.id job_id, d.id delivery_id, d.document delivery, p.document parent, j.document job
 FROM jobs j JOIN records d ON d.kind='operations' AND d.id=j.document#>>'{inputs,source_proof,delivery_id}' AND d.product_id=$1
 LEFT JOIN records p ON p.kind='operations' AND p.id=d.document->>'parent_id' AND p.product_id=$1
 WHERE d.document->>'kind'='DEPLOY'
 AND coalesce(d.document->>'integration_id','')=coalesce(j.document->>'integration_id','')
 AND d.document#>>'{snapshot,revision,id}'=j.document#>>'{inputs,revision_id}'
 AND d.document#>>'{snapshot,revision,repository_id}'=j.document#>>'{inputs,repository_id}'
 AND d.document#>>'{snapshot,revision,head_commit}'=j.document#>>'{inputs,source_sha}'
 AND d.document#>>'{snapshot,revision,product_id}'=$1
 AND coalesce(j.document#>>'{inputs,source_proof,snapshot_hash}','')<>''
 AND coalesce(d.document->>'parent_id','')=coalesce(j.document#>>'{inputs,source_proof,parent_id}','')
 AND coalesce(d.document#>>'{native,parent_hash}','')=coalesce(j.document#>>'{inputs,source_proof,parent_hash}','')
 AND ((j.document->>'capability'='local_oci_build' AND d.document#>>'{native,build_job_id}'=j.id)
 OR (j.document->>'capability'='registry_publish' AND d.document#>>'{native,publication_job_id}'=j.id))
), links AS (
 SELECT j.id job_id, r.document revision, 'recorded_revision'::text provenance FROM jobs j
 JOIN records r ON r.kind='integration_revisions' AND r.id=j.document#>>'{inputs,revision_id}' AND r.product_id=$1
 WHERE j.document#>'{inputs,source_proof}' IS NULL
 AND r.document->>'integration_id'=j.document->>'integration_id'
 AND r.document->>'repository_id'=j.document#>>'{inputs,repository_id}'
 AND r.document->>'head_commit'=j.document#>>'{inputs,source_sha}'
 AND coalesce(r.document->>'feature_id','')<>''
 UNION ALL
 SELECT m.job_id, m.delivery#>'{snapshot,revision}', 'recorded_snapshot_unverified' FROM managed m
 WHERE coalesce(m.delivery->>'parent_id','')='' AND coalesce(m.delivery#>>'{snapshot,revision,feature_id}','')<>''
 AND m.delivery#>>'{snapshot,revision,integration_id}'=m.job->>'integration_id'
 UNION ALL
 SELECT m.job_id, r, 'recorded_snapshot_unverified' FROM managed m
 CROSS JOIN LATERAL jsonb_array_elements(CASE WHEN jsonb_typeof(m.parent#>'{composition_snapshot,revision_snapshots}')='array' THEN m.parent#>'{composition_snapshot,revision_snapshots}' ELSE '[]'::jsonb END) r
 WHERE coalesce(m.delivery->>'parent_id','')<>''
 AND m.parent#>>'{composition_snapshot,product_id}'=$1
 AND (SELECT count(*) FROM jsonb_array_elements(CASE WHEN jsonb_typeof(m.parent#>'{composition_snapshot,components}')='array' THEN m.parent#>'{composition_snapshot,components}' ELSE '[]'::jsonb END) c WHERE c->>'application_id'=m.delivery#>>'{snapshot,application,id}')=1
 AND NOT EXISTS(SELECT 1 FROM jsonb_array_elements(CASE WHEN jsonb_typeof(m.parent#>'{composition_snapshot,components}')='array' THEN m.parent#>'{composition_snapshot,components}' ELSE '[]'::jsonb END) c CROSS JOIN LATERAL jsonb_array_elements(CASE WHEN jsonb_typeof(c->'revision_ids')='array' THEN c->'revision_ids' ELSE '[]'::jsonb END) wanted
 WHERE c->>'application_id'=m.delivery#>>'{snapshot,application,id}' AND
 (jsonb_typeof(wanted)<>'string' OR coalesce(wanted#>>'{}','')='' OR
 (SELECT count(*) FROM jsonb_array_elements(CASE WHEN jsonb_typeof(c->'revision_ids')='array' THEN c->'revision_ids' ELSE '[]'::jsonb END) v WHERE v=wanted)<>1 OR
 (SELECT count(*) FROM jsonb_array_elements(CASE WHEN jsonb_typeof(m.parent#>'{composition_snapshot,revision_snapshots}')='array' THEN m.parent#>'{composition_snapshot,revision_snapshots}' ELSE '[]'::jsonb END) frozen WHERE frozen->>'id'=wanted#>>'{}')<>1))
 AND ((m.parent->>'kind'='COMPOSE' AND (SELECT count(*) FROM jsonb_array_elements(CASE WHEN jsonb_typeof(m.parent->'sources')='array' THEN m.parent->'sources' ELSE '[]'::jsonb END) src WHERE src->>'repository_id'=m.job#>>'{inputs,repository_id}')=1)
 OR (m.parent->>'kind'='RELEASE_CANDIDATE' AND (SELECT count(*) FROM jsonb_array_elements(CASE WHEN jsonb_typeof(m.parent#>'{main_candidate,request,main_sources}')='array' THEN m.parent#>'{main_candidate,request,main_sources}' ELSE '[]'::jsonb END) src WHERE src->>'application_id'=m.delivery->>'application_id')=1))


 -- Reverse coverage: no frozen repository revision or source member may be
 -- silently discarded just because the component selected only a subset.
 AND NOT EXISTS(SELECT 1 FROM jsonb_array_elements(CASE WHEN jsonb_typeof(m.parent#>'{composition_snapshot,components}')='array' THEN m.parent#>'{composition_snapshot,components}' ELSE '[]'::jsonb END) c CROSS JOIN LATERAL jsonb_array_elements(CASE WHEN jsonb_typeof(m.parent#>'{composition_snapshot,revision_snapshots}')='array' THEN m.parent#>'{composition_snapshot,revision_snapshots}' ELSE '[]'::jsonb END) frozen
 WHERE c->>'application_id'=m.delivery->>'application_id' AND frozen->>'repository_id'=m.job#>>'{inputs,repository_id}'
 AND (coalesce(frozen->>'id','')='' OR NOT (CASE WHEN jsonb_typeof(c->'revision_ids')='array' THEN c->'revision_ids' ELSE '[]'::jsonb END ? (frozen->>'id'))))
 AND NOT EXISTS(SELECT 1 FROM jsonb_array_elements(CASE WHEN jsonb_typeof(m.parent#>'{composition_snapshot,components}')='array' THEN m.parent#>'{composition_snapshot,components}' ELSE '[]'::jsonb END) c CROSS JOIN LATERAL jsonb_array_elements(CASE WHEN jsonb_typeof(m.parent->'sources')='array' THEN m.parent->'sources' ELSE '[]'::jsonb END) src CROSS JOIN LATERAL jsonb_array_elements(CASE WHEN jsonb_typeof(src#>'{plan,head_shas}')='array' THEN src#>'{plan,head_shas}' ELSE '[]'::jsonb END) head
 WHERE m.parent->>'kind'='COMPOSE' AND c->>'application_id'=m.delivery->>'application_id' AND src->>'repository_id'=m.job#>>'{inputs,repository_id}'
 AND (jsonb_typeof(head)<>'string' OR coalesce(head#>>'{}','')='' OR
 (SELECT count(*) FROM jsonb_array_elements(CASE WHEN jsonb_typeof(src#>'{plan,head_shas}')='array' THEN src#>'{plan,head_shas}' ELSE '[]'::jsonb END) h WHERE h=head)<>1 OR
 (SELECT count(*) FROM jsonb_array_elements(CASE WHEN jsonb_typeof(m.parent#>'{composition_snapshot,revision_snapshots}')='array' THEN m.parent#>'{composition_snapshot,revision_snapshots}' ELSE '[]'::jsonb END) frozen WHERE frozen->>'repository_id'=m.job#>>'{inputs,repository_id}' AND frozen->>'head_commit'=head#>>'{}' AND CASE WHEN jsonb_typeof(c->'revision_ids')='array' THEN c->'revision_ids' ELSE '[]'::jsonb END ? (frozen->>'id'))<>1))
 AND NOT EXISTS(SELECT 1 FROM jsonb_array_elements(CASE WHEN jsonb_typeof(m.parent#>'{composition_snapshot,components}')='array' THEN m.parent#>'{composition_snapshot,components}' ELSE '[]'::jsonb END) c CROSS JOIN LATERAL jsonb_array_elements(CASE WHEN jsonb_typeof(m.parent#>'{main_candidate,request,main_sources}')='array' THEN m.parent#>'{main_candidate,request,main_sources}' ELSE '[]'::jsonb END) src CROSS JOIN LATERAL jsonb_array_elements(CASE WHEN jsonb_typeof(src->'revisions')='array' THEN src->'revisions' ELSE '[]'::jsonb END) member
 WHERE m.parent->>'kind'='RELEASE_CANDIDATE' AND c->>'application_id'=m.delivery->>'application_id' AND src->>'application_id'=m.delivery->>'application_id'
 AND (coalesce(member->>'revision_id','')='' OR NOT (CASE WHEN jsonb_typeof(c->'revision_ids')='array' THEN c->'revision_ids' ELSE '[]'::jsonb END ? (member->>'revision_id'))))
 AND m.delivery#>>'{snapshot,application,product_id}'=$1
 AND m.delivery->>'application_id'=m.delivery#>>'{snapshot,application,id}'
 AND (CASE WHEN jsonb_typeof(m.parent->'child_ids')='array' THEN m.parent->'child_ids' ELSE '[]'::jsonb END) ? m.delivery_id
 AND ((m.parent->>'kind'='COMPOSE' AND EXISTS (
 SELECT 1 FROM jsonb_array_elements(CASE WHEN jsonb_typeof(m.parent->'sources')='array' THEN m.parent->'sources' ELSE '[]'::jsonb END) s
 WHERE s->>'repository_id'=m.job#>>'{inputs,repository_id}' AND s#>>'{result,sha}'=m.job#>>'{inputs,source_sha}' AND s#>>'{result,conflict}'='false' AND (CASE WHEN jsonb_typeof(s#>'{plan,head_shas}')='array' THEN s#>'{plan,head_shas}' ELSE '[]'::jsonb END) ? (r->>'head_commit')))
 OR (m.parent->>'kind'='RELEASE_CANDIDATE' AND m.parent#>>'{main_candidate,evidence_kind}'='ATTRIBUTED_RCP_VERIFICATION' AND EXISTS (
 SELECT 1 FROM jsonb_array_elements(CASE WHEN jsonb_typeof(m.parent#>'{main_candidate,request,main_sources}')='array' THEN m.parent#>'{main_candidate,request,main_sources}' ELSE '[]'::jsonb END) s
 WHERE s->>'application_id'=m.delivery->>'application_id' AND s->>'main_sha'=m.job#>>'{inputs,source_sha}' AND (SELECT count(*) FROM jsonb_array_elements(CASE WHEN jsonb_typeof(s->'revisions')='array' THEN s->'revisions' ELSE '[]'::jsonb END) x WHERE x->>'revision_id'=r->>'id')=1)))
 AND r->>'product_id'=$1 AND r->>'repository_id'=m.job#>>'{inputs,repository_id}'
 AND coalesce(r->>'feature_id','')<>'' AND coalesce(r->>'integration_id','')<>''
 AND NOT EXISTS (SELECT 1 FROM jsonb_array_elements(CASE WHEN jsonb_typeof(m.parent#>'{composition_snapshot,components}')='array' THEN m.parent#>'{composition_snapshot,components}' ELSE '[]'::jsonb END) c CROSS JOIN LATERAL jsonb_array_elements_text(CASE WHEN jsonb_typeof(c->'revision_ids')='array' THEN c->'revision_ids' ELSE '[]'::jsonb END) wanted
 WHERE c->>'application_id'=m.delivery#>>'{snapshot,application,id}' AND (SELECT count(*) FROM jsonb_array_elements(CASE WHEN jsonb_typeof(m.parent#>'{composition_snapshot,revision_snapshots}')='array' THEN m.parent#>'{composition_snapshot,revision_snapshots}' ELSE '[]'::jsonb END) frozen WHERE frozen->>'id'=wanted AND frozen->>'product_id'=$1 AND frozen->>'repository_id'=m.job#>>'{inputs,repository_id}' AND coalesce(frozen->>'feature_id','')<>'' AND coalesce(frozen->>'integration_id','')<>''
 AND ((m.parent->>'kind'='COMPOSE' AND EXISTS (
 SELECT 1 FROM jsonb_array_elements(CASE WHEN jsonb_typeof(m.parent->'sources')='array' THEN m.parent->'sources' ELSE '[]'::jsonb END) src
 WHERE src->>'repository_id'=m.job#>>'{inputs,repository_id}' AND src#>>'{result,sha}'=m.job#>>'{inputs,source_sha}' AND src#>>'{result,conflict}'='false' AND (CASE WHEN jsonb_typeof(src#>'{plan,head_shas}')='array' THEN src#>'{plan,head_shas}' ELSE '[]'::jsonb END) ? (frozen->>'head_commit')))
 OR (m.parent->>'kind'='RELEASE_CANDIDATE' AND m.parent#>>'{main_candidate,evidence_kind}'='ATTRIBUTED_RCP_VERIFICATION' AND EXISTS (
 SELECT 1 FROM jsonb_array_elements(CASE WHEN jsonb_typeof(m.parent#>'{main_candidate,request,main_sources}')='array' THEN m.parent#>'{main_candidate,request,main_sources}' ELSE '[]'::jsonb END) src
 WHERE src->>'application_id'=m.delivery->>'application_id' AND src->>'main_sha'=m.job#>>'{inputs,source_sha}' AND (SELECT count(*) FROM jsonb_array_elements(CASE WHEN jsonb_typeof(src->'revisions')='array' THEN src->'revisions' ELSE '[]'::jsonb END) x WHERE x->>'revision_id'=frozen->>'id')=1))))<>1)
 AND EXISTS (SELECT 1 FROM jsonb_array_elements(CASE WHEN jsonb_typeof(m.parent#>'{composition_snapshot,components}')='array' THEN m.parent#>'{composition_snapshot,components}' ELSE '[]'::jsonb END) c
 WHERE c->>'application_id'=m.delivery#>>'{snapshot,application,id}' AND (CASE WHEN jsonb_typeof(c->'revision_ids')='array' THEN c->'revision_ids' ELSE '[]'::jsonb END) ? (r->>'id'))
), selected AS (
 SELECT j.* FROM jobs j WHERE ($2='' OR EXISTS(SELECT 1 FROM links l WHERE l.job_id=j.id AND l.revision->>'feature_id'=$2)) AND ($3='' OR j.id=$3)
)`

func historyStringSQL(path string) string { return "left(coalesce(" + path + ",''),129)" }
func (s *Store) ReadWorkerHistory(ctx context.Context, q domain.WorkerHistoryMetadataRequest) (domain.WorkerHistoryMetadata, error) {
	var out domain.WorkerHistoryMetadata
	if !validHistoryRequest(q) {
		return out, errors.New("invalid worker history request")
	}
	tx, e := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if e != nil {
		return out, e
	}
	defer tx.Rollback(ctx)
	e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM records WHERE kind='products' AND id=$1),($2='' OR EXISTS(SELECT 1 FROM records WHERE kind='features' AND id=$2 AND product_id=$1))`, q.ProductID, q.FeatureID).Scan(&out.ProductFound, &out.FeatureFound)
	if e != nil || !out.ProductFound || !out.FeatureFound {
		return out, e
	}
	args := []any{q.ProductID, q.FeatureID, q.JobID, q.AfterID, q.Limit}
	e = tx.QueryRow(ctx, historyCTE+`SELECT ($3='' OR EXISTS(SELECT 1 FROM selected))`, args[:3]...).Scan(&out.JobFound)
	if e != nil || !out.JobFound {
		return out, e
	}
	var query string
	base := func(alias string, fields map[string]string) string {
		text := `jsonb_build_object('id',` + historyStringSQL(alias+`.id`) + `,'product_id',$1`
		// Fixed keys, never user SQL. JSON serialization/order is immaterial here.
		keys := make([]string, 0, len(fields))
		for k := range fields {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			text += `,'` + k + `',` + historyStringSQL(fields[k])
		}
		return text
	}
	switch q.Collection {
	case "jobs":
		query = historyCTE + `SELECT ` + base("j", map[string]string{"delivery_id": "j.document#>>'{inputs,source_proof,delivery_id}'", "parent_id": "j.document#>>'{inputs,source_proof,parent_id}'", "snapshot_hash": "j.document#>>'{inputs,source_proof,snapshot_hash}'", "parent_hash": "j.document#>>'{inputs,source_proof,parent_hash}'", "operation_id": "j.document->>'operation_id'", "integration_id": "j.document->>'integration_id'", "recorded_status": "j.document->>'status'", "capability": "j.document->>'capability'", "source_sha": "j.document#>>'{inputs,source_sha}'", "revision_id": "j.document#>>'{inputs,revision_id}'", "repository_id": "j.document#>>'{inputs,repository_id}'", "recipe_hash": "j.document#>>'{inputs,recipe_hash}'", "input_hash": "j.document->>'input_hash'", "created_at": "j.document->>'created_at'", "updated_at": "j.document->>'updated_at'"}) + `, 'provenance',CASE WHEN EXISTS(SELECT 1 FROM links l WHERE l.job_id=j.id) THEN CASE WHEN j.document#>'{inputs,source_proof}' IS NULL THEN 'recorded_revision' ELSE 'recorded_snapshot_unverified' END ELSE 'unknown' END,'attempts',(SELECT count(*) FROM records a WHERE a.kind='worker_attempts' AND a.product_id=$1 AND a.document->>'job_id'=j.id),'links',(SELECT count(DISTINCT l.revision->>'id') FROM links l WHERE l.job_id=j.id)) FROM selected j WHERE j.id COLLATE "C">$4 COLLATE "C" ORDER BY j.id COLLATE "C" LIMIT $5`
	case "attempts":
		query = historyCTE + `SELECT ` + base("a", map[string]string{"job_id": "a.document->>'job_id'", "recorded_status": "a.document->>'status'", "input_hash": "a.document->>'input_hash'", "created_at": "a.document->>'created_at'", "updated_at": "a.document->>'updated_at'"}) + `) FROM records a WHERE a.kind='worker_attempts' AND a.product_id=$1 AND a.document->>'job_id'=$3 AND EXISTS(SELECT 1 FROM selected) AND a.id COLLATE "C">$4 COLLATE "C" ORDER BY a.id COLLATE "C" LIMIT $5`
	case "links":
		query = historyCTE + `SELECT jsonb_build_object('id',left(l.revision->>'id',129),'product_id',$1,'job_id',$3,'revision_id',left(l.revision->>'id',129),'feature_id',left(l.revision->>'feature_id',129),'integration_id',left(l.revision->>'integration_id',129),'repository_id',left(l.revision->>'repository_id',129),'source_sha',left(l.revision->>'head_commit',129),'provenance',l.provenance) FROM (SELECT DISTINCT ON (revision->>'id') * FROM links WHERE job_id=$3 ORDER BY revision->>'id') l WHERE EXISTS(SELECT 1 FROM selected) AND (l.revision->>'id') COLLATE "C">$4 COLLATE "C" ORDER BY (l.revision->>'id') COLLATE "C" LIMIT $5`
	}
	rows, e := tx.Query(ctx, query, args...)
	if e != nil {
		return out, e
	}
	defer rows.Close()
	for rows.Next() {
		var raw []byte
		var item domain.WorkerHistoryItem
		if e = rows.Scan(&raw); e != nil {
			return out, e
		}
		if e = json.Unmarshal(raw, &item); e != nil {
			return out, e
		}
		out.Items = append(out.Items, item)
	}
	if e = rows.Err(); e != nil {
		return out, e
	}
	rows.Close()
	return out, tx.Commit(ctx)
}
func validHistoryRequest(q domain.WorkerHistoryMetadataRequest) bool {
	return q.ProductID != "" && q.Limit >= 1 && q.Limit <= 101 && (q.Collection == "jobs" || q.JobID != "" && (q.Collection == "attempts" || q.Collection == "links"))
}

// Local JSON is scanned in-place. Unknown fields (including reports, parameters,
// credentials and execution recipes) are skipped without allocation.
func historyPath(raw []byte, path ...string) []byte {
	if len(path) == 0 {
		return raw
	}
	var out []byte
	_ = objectFields(raw, func(k string, v []byte) error {
		if k == path[0] {
			out = historyPath(v, path[1:]...)
		}
		return nil
	})
	return out
}
func historyText(raw []byte, path ...string) string {
	v := historyPath(raw, path...)
	if len(v) == 0 {
		return ""
	}
	s, cut, e := metadataString(v)
	if e != nil || cut || len(s) > 128 {
		return ""
	}
	return s
}
func historyArray(ctx context.Context, raw []byte, visit func([]byte) error) error {
	i := white(raw, 0)
	if i >= len(raw) || raw[i] != '[' {
		return nil
	}
	i++
	for {
		if e := ctx.Err(); e != nil {
			return e
		}
		i = white(raw, i)
		if i >= len(raw) {
			return errors.New("invalid history collection")
		}
		if raw[i] == ']' {
			return nil
		}
		end, e := skipJSON(raw, i)
		if e != nil {
			return e
		}
		if e = visit(raw[i:end]); e != nil {
			return e
		}
		i = white(raw, end)
		if i < len(raw) && raw[i] == ',' {
			i++
			continue
		}
		if i < len(raw) && raw[i] == ']' {
			return nil
		}
		return errors.New("invalid history collection")
	}
}
func historyScan(ctx context.Context, data []byte, kind string, visit func([]byte) error) error {
	return historyArray(ctx, historyPath(data, kind), visit)
}
func historyRecord(ctx context.Context, data []byte, kind, id, product string) []byte {
	var found []byte
	_ = historyScan(ctx, data, kind, func(raw []byte) error {
		if historyText(raw, "id") == id && (product == "" || historyText(raw, "product_id") == product) {
			found = raw
		}
		return nil
	})
	return found
}
func historyLocalLinks(ctx context.Context, data, job []byte, visit func(domain.WorkerHistoryItem)) {
	product := historyText(job, "product_id")
	jobID := historyText(job, "id")
	repo := historyText(job, "inputs", "repository_id")
	emit := func(r []byte, provenance string) {
		if historyText(r, "product_id") != product || historyText(r, "repository_id") != repo || historyText(r, "feature_id") == "" || historyText(r, "integration_id") == "" || historyText(r, "id") == "" {
			return
		}
		visit(domain.WorkerHistoryItem{ID: historyText(r, "id"), ProductID: product, JobID: jobID, RevisionID: historyText(r, "id"), FeatureID: historyText(r, "feature_id"), IntegrationID: historyText(r, "integration_id"), RepositoryID: repo, SourceSHA: historyText(r, "head_commit"), Provenance: provenance})
	}
	proof := historyPath(job, "inputs", "source_proof")
	if len(proof) == 0 {
		r := historyRecord(ctx, data, "integration_revisions", historyText(job, "inputs", "revision_id"), product)
		if historyText(r, "integration_id") == historyText(job, "integration_id") && historyText(r, "head_commit") == historyText(job, "inputs", "source_sha") {
			emit(r, "recorded_revision")
		}
		return
	}
	d := historyRecord(ctx, data, "operations", historyText(proof, "delivery_id"), product)
	r := historyPath(d, "snapshot", "revision")
	if len(d) == 0 || historyText(d, "kind") != "DEPLOY" || historyText(proof, "snapshot_hash") == "" || historyText(d, "parent_id") != historyText(proof, "parent_id") || historyText(d, "native", "parent_hash") != historyText(proof, "parent_hash") || historyText(d, "integration_id") != historyText(job, "integration_id") || historyText(r, "product_id") != product || historyText(r, "id") != historyText(job, "inputs", "revision_id") || historyText(r, "repository_id") != repo || historyText(r, "head_commit") != historyText(job, "inputs", "source_sha") {
		return
	}
	capability := historyText(job, "capability")
	if !(capability == "local_oci_build" && historyText(d, "native", "build_job_id") == jobID || capability == "registry_publish" && historyText(d, "native", "publication_job_id") == jobID) {
		return
	}
	if historyText(d, "parent_id") == "" {
		if historyText(r, "integration_id") == historyText(job, "integration_id") {
			emit(r, "recorded_snapshot_unverified")
		}
		return
	}
	parent := historyRecord(ctx, data, "operations", historyText(d, "parent_id"), product)
	comp := historyPath(parent, "composition_snapshot")
	if historyText(comp, "product_id") != product || historyText(d, "snapshot", "application", "product_id") != product || historyText(d, "application_id") != historyText(d, "snapshot", "application", "id") || !historyContains(ctx, historyPath(parent, "child_ids"), historyText(d, "id")) {
		return
	}
	if !historyCompleteComponent(ctx, parent, comp, d, job) {
		return
	}
	_ = historyArray(ctx, historyPath(comp, "revision_snapshots"), func(r []byte) error {
		member := false
		_ = historyArray(ctx, historyPath(comp, "components"), func(c []byte) error {
			if historyText(c, "application_id") == historyText(d, "snapshot", "application", "id") {
				return historyArray(ctx, historyPath(c, "revision_ids"), func(id []byte) error {
					var s string
					if len(id) <= 800 && json.Unmarshal(id, &s) == nil && s == historyText(r, "id") {
						member = true
					}
					return nil
				})
			}
			return nil
		})
		if member && historyParentSource(ctx, parent, d, job, r) {
			emit(r, "recorded_snapshot_unverified")
		}
		return nil
	})
}
func (s *LocalStore) ReadWorkerHistory(ctx context.Context, q domain.WorkerHistoryMetadataRequest) (domain.WorkerHistoryMetadata, error) {
	var out domain.WorkerHistoryMetadata
	if !validHistoryRequest(q) {
		return out, errors.New("invalid worker history request")
	}
	s.lifecycle.RLock()
	defer s.lifecycle.RUnlock()
	if s.closed {
		return out, errors.New("local database is closed")
	}
	e := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(localBucket)
		if b == nil {
			return errors.New("local state bucket missing")
		}
		data := b.Get(localStateKey)
		if len(data) == 0 || len(data) > maxLocalStateBytes {
			return errors.New("local state missing or oversized")
		}
		out.ProductFound = historyRecord(ctx, data, "products", q.ProductID, "") != nil
		out.FeatureFound = q.FeatureID == "" || historyRecord(ctx, data, "features", q.FeatureID, q.ProductID) != nil
		out.JobFound = q.JobID == ""
		if !out.ProductFound || !out.FeatureFound {
			return ctx.Err()
		}
		add := func(c domain.WorkerHistoryItem) {
			if c.ID <= q.AfterID {
				return
			}
			i := sort.Search(len(out.Items), func(i int) bool { return out.Items[i].ID >= c.ID })
			if i < len(out.Items) && out.Items[i].ID == c.ID || i >= q.Limit {
				return
			}
			out.Items = append(out.Items, domain.WorkerHistoryItem{})
			copy(out.Items[i+1:], out.Items[i:])
			out.Items[i] = c
			if len(out.Items) > q.Limit {
				out.Items = out.Items[:q.Limit]
			}
		}
		return historyScan(ctx, data, "worker_jobs", func(j []byte) error {
			id := historyText(j, "id")
			if historyText(j, "product_id") != q.ProductID || q.JobID != "" && id != q.JobID {
				return nil
			}
			selected := q.FeatureID == ""
			links := 0
			historyLocalLinks(ctx, data, j, func(c domain.WorkerHistoryItem) {
				links++
				if c.FeatureID == q.FeatureID {
					selected = true
				}
			})
			if !selected {
				return nil
			}
			if q.JobID != "" {
				out.JobFound = true
			}
			switch q.Collection {
			case "links":
				historyLocalLinks(ctx, data, j, add)
			case "attempts":
				return historyScan(ctx, data, "worker_attempts", func(a []byte) error {
					if historyText(a, "product_id") == q.ProductID && historyText(a, "job_id") == id {
						add(domain.WorkerHistoryItem{ID: historyText(a, "id"), ProductID: q.ProductID, JobID: id, RecordedStatus: historyText(a, "status"), InputHash: historyText(a, "input_hash"), CreatedAt: historyText(a, "created_at"), UpdatedAt: historyText(a, "updated_at")})
					}
					return nil
				})
			case "jobs":
				attempts := 0
				_ = historyScan(ctx, data, "worker_attempts", func(a []byte) error {
					if historyText(a, "product_id") == q.ProductID && historyText(a, "job_id") == id {
						attempts++
					}
					return nil
				})
				provenance := "unknown"
				if links > 0 {
					provenance = "recorded_revision"
					if len(historyPath(j, "inputs", "source_proof")) != 0 {
						provenance = "recorded_snapshot_unverified"
					}
				}
				add(domain.WorkerHistoryItem{ID: id, ProductID: q.ProductID, DeliveryID: historyText(j, "inputs", "source_proof", "delivery_id"), ParentID: historyText(j, "inputs", "source_proof", "parent_id"), SnapshotHash: historyText(j, "inputs", "source_proof", "snapshot_hash"), ParentHash: historyText(j, "inputs", "source_proof", "parent_hash"), OperationID: historyText(j, "operation_id"), IntegrationID: historyText(j, "integration_id"), RecordedStatus: historyText(j, "status"), Capability: historyText(j, "capability"), InputHash: historyText(j, "input_hash"), RecipeHash: historyText(j, "inputs", "recipe_hash"), SourceSHA: historyText(j, "inputs", "source_sha"), RevisionID: historyText(j, "inputs", "revision_id"), RepositoryID: historyText(j, "inputs", "repository_id"), CreatedAt: historyText(j, "created_at"), UpdatedAt: historyText(j, "updated_at"), Attempts: attempts, Links: links, Provenance: provenance})
			}
			return ctx.Err()
		})
	})
	return out, e
}

func historyContains(ctx context.Context, raw []byte, value string) bool {
	found := false
	_ = historyArray(ctx, raw, func(v []byte) error {
		var text string
		if len(v) <= 800 && json.Unmarshal(v, &text) == nil && text == value {
			found = true
		}
		return nil
	})
	return found
}
func historyParentSource(ctx context.Context, parent, d, j, r []byte) bool {
	found := false
	sourceCount := 0
	switch historyText(parent, "kind") {
	case "COMPOSE":
		_ = historyArray(ctx, historyPath(parent, "sources"), func(s []byte) error {
			if historyText(s, "repository_id") == historyText(j, "inputs", "repository_id") {
				sourceCount++
			}
			if historyText(s, "repository_id") == historyText(j, "inputs", "repository_id") && historyText(s, "result", "sha") == historyText(j, "inputs", "source_sha") && string(historyPath(s, "result", "conflict")) == "false" && historyContains(ctx, historyPath(s, "plan", "head_shas"), historyText(r, "head_commit")) {
				found = true
			}
			return nil
		})
	case "RELEASE_CANDIDATE":
		if historyText(parent, "main_candidate", "evidence_kind") != "ATTRIBUTED_RCP_VERIFICATION" {
			return false
		}
		_ = historyArray(ctx, historyPath(parent, "main_candidate", "request", "main_sources"), func(s []byte) error {
			if historyText(s, "application_id") == historyText(d, "application_id") {
				sourceCount++
			}
			if historyText(s, "application_id") == historyText(d, "application_id") && historyText(s, "main_sha") == historyText(j, "inputs", "source_sha") {
				matches := 0
				_ = historyArray(ctx, historyPath(s, "revisions"), func(x []byte) error {
					if historyText(x, "revision_id") == historyText(r, "id") {
						matches++
					}
					return nil
				})
				found = matches == 1
			}
			return nil
		})
	}
	return found && sourceCount == 1
}

func historyCompleteComponent(ctx context.Context, parent, comp, d, j []byte) bool {
	valid := true
	components := 0
	_ = historyArray(ctx, historyPath(comp, "components"), func(c []byte) error {
		if historyText(c, "application_id") != historyText(d, "snapshot", "application", "id") {
			return nil
		}
		components++
		if !historyReverseComponent(ctx, parent, comp, c, d, j) {
			valid = false
		}
		return historyArray(ctx, historyPath(c, "revision_ids"), func(w []byte) error {
			var wanted string
			if len(w) > 800 || json.Unmarshal(w, &wanted) != nil || wanted == "" {
				valid = false
				return nil
			}
			entries := 0
			_ = historyArray(ctx, historyPath(c, "revision_ids"), func(v []byte) error {
				var identity string
				if len(v) <= 800 && json.Unmarshal(v, &identity) == nil && identity == wanted {
					entries++
				}
				return nil
			})
			if entries != 1 {
				valid = false
			}
			count := 0
			_ = historyArray(ctx, historyPath(comp, "revision_snapshots"), func(r []byte) error {
				if historyText(r, "id") == wanted {
					count++
					if historyText(r, "product_id") != historyText(j, "product_id") || historyText(r, "repository_id") != historyText(j, "inputs", "repository_id") || historyText(r, "feature_id") == "" || historyText(r, "integration_id") == "" || !historyParentSource(ctx, parent, d, j, r) {
						valid = false
					}
				}
				return nil
			})
			if count != 1 {
				valid = false
			}
			return nil
		})
	})
	return valid && components == 1
}

// Validate reverse membership without retaining the source/revision arrays.
// Forward checks separately validate identities, ownership and source evidence.
func historyReverseComponent(ctx context.Context, parent, comp, c, d, j []byte) bool {
	valid := true
	selected := historyPath(c, "revision_ids")
	repo := historyText(j, "inputs", "repository_id")
	_ = historyArray(ctx, historyPath(comp, "revision_snapshots"), func(r []byte) error {
		if historyText(r, "repository_id") == repo && !historyContains(ctx, selected, historyText(r, "id")) {
			valid = false
		}
		return nil
	})
	switch historyText(parent, "kind") {
	case "COMPOSE":
		_ = historyArray(ctx, historyPath(parent, "sources"), func(src []byte) error {
			if historyText(src, "repository_id") != repo {
				return nil
			}
			heads := historyPath(src, "plan", "head_shas")
			return historyArray(ctx, heads, func(raw []byte) error {
				var head string
				if len(raw) > 800 || json.Unmarshal(raw, &head) != nil || head == "" {
					valid = false
					return nil
				}
				duplicates := 0
				_ = historyArray(ctx, heads, func(v []byte) error {
					var value string
					if len(v) <= 800 && json.Unmarshal(v, &value) == nil && value == head {
						duplicates++
					}
					return nil
				})
				if duplicates != 1 {
					valid = false
				}
				matches := 0
				_ = historyArray(ctx, historyPath(comp, "revision_snapshots"), func(r []byte) error {
					if historyText(r, "repository_id") == repo && historyText(r, "head_commit") == head && historyContains(ctx, selected, historyText(r, "id")) {
						matches++
					}
					return nil
				})
				if matches != 1 {
					valid = false
				}
				return nil
			})
		})
	case "RELEASE_CANDIDATE":
		_ = historyArray(ctx, historyPath(parent, "main_candidate", "request", "main_sources"), func(src []byte) error {
			if historyText(src, "application_id") != historyText(d, "application_id") {
				return nil
			}
			return historyArray(ctx, historyPath(src, "revisions"), func(r []byte) error {
				if historyText(r, "revision_id") == "" || !historyContains(ctx, selected, historyText(r, "revision_id")) {
					valid = false
				}
				return nil
			})
		})
	}
	return valid
}
