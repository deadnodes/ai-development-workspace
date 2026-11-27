package persistence

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
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
	hidden, e := hiddenProducts(ctx, tx)
	if e != nil {
		return out, e
	}
	e = tx.QueryRow(ctx, `SELECT `+visibleProductSQL, q.ProductID, hidden).Scan(&out.ProductFound)
	if e != nil || !out.ProductFound {
		return out, e
	}
	e = tx.QueryRow(ctx, `SELECT ($2='' OR EXISTS(SELECT 1 FROM records WHERE kind='features' AND id=$2 AND product_id=$1))`, q.ProductID, q.FeatureID).Scan(&out.FeatureFound)
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
