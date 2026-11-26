package persistence

import (
	"context"
	"encoding/json"
	"errors"
	"releasecontrol/internal/domain"
	"sort"
)

// This relation projects only frozen identity fields. It deliberately does not
// recompute execution fingerprints or treat recorded provenance as verification.
const sqliteHistoryCTE = `WITH jobs AS (
 SELECT j.* FROM records j WHERE j.kind='worker_jobs' AND j.product_id=$1
), managed AS (
 SELECT j.id job_id, d.id delivery_id, d.document delivery, p.document parent, j.document job
 FROM jobs j JOIN records d ON d.kind='operations' AND d.id=j.document->>'$.inputs.source_proof.delivery_id' AND d.product_id=$1
 LEFT JOIN records p ON p.kind='operations' AND p.id=d.document->>'parent_id' AND p.product_id=$1
 WHERE d.document->>'kind'='DEPLOY'
 AND coalesce(d.document->>'integration_id','')=coalesce(j.document->>'integration_id','')
 AND d.document->>'$.snapshot.revision.id'=j.document->>'$.inputs.revision_id'
 AND d.document->>'$.snapshot.revision.repository_id'=j.document->>'$.inputs.repository_id'
 AND d.document->>'$.snapshot.revision.head_commit'=j.document->>'$.inputs.source_sha'
 AND d.document->>'$.snapshot.revision.product_id'=$1
 AND coalesce(j.document->>'$.inputs.source_proof.snapshot_hash','')<>''
 AND coalesce(d.document->>'parent_id','')=coalesce(j.document->>'$.inputs.source_proof.parent_id','')
 AND coalesce(d.document->>'$.native.parent_hash','')=coalesce(j.document->>'$.inputs.source_proof.parent_hash','')
 AND ((j.document->>'capability'='local_oci_build' AND d.document->>'$.native.build_job_id'=j.id)
 OR (j.document->>'capability'='registry_publish' AND d.document->>'$.native.publication_job_id'=j.id))
), links AS (
 SELECT j.id job_id, r.document revision, 'recorded_revision' provenance FROM jobs j
 JOIN records r ON r.kind='integration_revisions' AND r.id=j.document->>'$.inputs.revision_id' AND r.product_id=$1
 WHERE json_type(j.document,'$.inputs.source_proof') IS NULL
 AND r.document->>'integration_id'=j.document->>'integration_id'
 AND r.document->>'repository_id'=j.document->>'$.inputs.repository_id'
 AND r.document->>'head_commit'=j.document->>'$.inputs.source_sha'
 AND coalesce(r.document->>'feature_id','')<>''
 UNION ALL
 SELECT m.job_id, json_extract(m.delivery,'$.snapshot.revision'), 'recorded_snapshot_unverified' FROM managed m
 WHERE coalesce(m.delivery->>'parent_id','')='' AND coalesce(m.delivery->>'$.snapshot.revision.feature_id','')<>''
 AND m.delivery->>'$.snapshot.revision.integration_id'=m.job->>'integration_id'
 UNION ALL
 SELECT m.job_id, r.value, 'recorded_snapshot_unverified' FROM managed m
 CROSS JOIN json_each(CASE WHEN json_type(m.parent,'$.composition_snapshot.revision_snapshots')='array' THEN json_extract(m.parent,'$.composition_snapshot.revision_snapshots') ELSE '[]' END) r
 WHERE coalesce(m.delivery->>'parent_id','')<>''
 AND m.parent->>'$.composition_snapshot.product_id'=$1
 AND (SELECT count(*) FROM json_each(CASE WHEN json_type(m.parent,'$.composition_snapshot.components')='array' THEN json_extract(m.parent,'$.composition_snapshot.components') ELSE '[]' END) c WHERE (CASE WHEN c.type='object' THEN c.value ELSE '{}' END)->>'application_id'=m.delivery->>'$.snapshot.application.id')=1
 AND NOT EXISTS(SELECT 1 FROM json_each(CASE WHEN json_type(m.parent,'$.composition_snapshot.components')='array' THEN json_extract(m.parent,'$.composition_snapshot.components') ELSE '[]' END) c CROSS JOIN json_each(CASE WHEN json_type((CASE WHEN c.type='object' THEN c.value ELSE '{}' END)->'revision_ids')='array' THEN (CASE WHEN c.type='object' THEN c.value ELSE '{}' END)->'revision_ids' ELSE '[]' END) wanted
 WHERE (CASE WHEN c.type='object' THEN c.value ELSE '{}' END)->>'application_id'=m.delivery->>'$.snapshot.application.id' AND
 (wanted.type<>'text' OR coalesce(wanted.value,'')='' OR
 (SELECT count(*) FROM json_each(CASE WHEN json_type((CASE WHEN c.type='object' THEN c.value ELSE '{}' END)->'revision_ids')='array' THEN (CASE WHEN c.type='object' THEN c.value ELSE '{}' END)->'revision_ids' ELSE '[]' END) v WHERE v.value=wanted.value)<>1 OR
 (SELECT count(*) FROM json_each(CASE WHEN json_type(m.parent,'$.composition_snapshot.revision_snapshots')='array' THEN json_extract(m.parent,'$.composition_snapshot.revision_snapshots') ELSE '[]' END) frozen WHERE (CASE WHEN frozen.type='object' THEN frozen.value ELSE '{}' END)->>'id'=wanted.value)<>1))
 AND ((m.parent->>'kind'='COMPOSE' AND (SELECT count(*) FROM json_each(CASE WHEN json_type(m.parent->'sources')='array' THEN m.parent->'sources' ELSE '[]' END) src WHERE (CASE WHEN src.type='object' THEN src.value ELSE '{}' END)->>'repository_id'=m.job->>'$.inputs.repository_id')=1)
 OR (m.parent->>'kind'='RELEASE_CANDIDATE' AND (SELECT count(*) FROM json_each(CASE WHEN json_type(m.parent,'$.main_candidate.request.main_sources')='array' THEN json_extract(m.parent,'$.main_candidate.request.main_sources') ELSE '[]' END) src WHERE (CASE WHEN src.type='object' THEN src.value ELSE '{}' END)->>'application_id'=m.delivery->>'application_id')=1))


 -- Reverse coverage: no frozen repository revision or source member may be
 -- silently discarded just because the component selected only a subset.
 AND NOT EXISTS(SELECT 1 FROM json_each(CASE WHEN json_type(m.parent,'$.composition_snapshot.components')='array' THEN json_extract(m.parent,'$.composition_snapshot.components') ELSE '[]' END) c CROSS JOIN json_each(CASE WHEN json_type(m.parent,'$.composition_snapshot.revision_snapshots')='array' THEN json_extract(m.parent,'$.composition_snapshot.revision_snapshots') ELSE '[]' END) frozen
 WHERE (CASE WHEN c.type='object' THEN c.value ELSE '{}' END)->>'application_id'=m.delivery->>'application_id' AND (CASE WHEN frozen.type='object' THEN frozen.value ELSE '{}' END)->>'repository_id'=m.job->>'$.inputs.repository_id'
 AND (coalesce((CASE WHEN frozen.type='object' THEN frozen.value ELSE '{}' END)->>'id','')='' OR NOT EXISTS (SELECT 1 FROM json_each((CASE WHEN c.type='object' THEN c.value ELSE '{}' END)->'revision_ids') member_value WHERE member_value.type='text' AND member_value.value=((CASE WHEN frozen.type='object' THEN frozen.value ELSE '{}' END)->>'id'))))
 AND NOT EXISTS(SELECT 1 FROM json_each(CASE WHEN json_type(m.parent,'$.composition_snapshot.components')='array' THEN json_extract(m.parent,'$.composition_snapshot.components') ELSE '[]' END) c CROSS JOIN json_each(CASE WHEN json_type(m.parent->'sources')='array' THEN m.parent->'sources' ELSE '[]' END) src CROSS JOIN json_each(CASE WHEN json_type(json_extract(src.value,'$.plan.head_shas'))='array' THEN json_extract(src.value,'$.plan.head_shas') ELSE '[]' END) head
 WHERE m.parent->>'kind'='COMPOSE' AND (CASE WHEN c.type='object' THEN c.value ELSE '{}' END)->>'application_id'=m.delivery->>'application_id' AND (CASE WHEN src.type='object' THEN src.value ELSE '{}' END)->>'repository_id'=m.job->>'$.inputs.repository_id'
 AND (head.type<>'text' OR coalesce(head.value,'')='' OR
 (SELECT count(*) FROM json_each(CASE WHEN json_type(json_extract(src.value,'$.plan.head_shas'))='array' THEN json_extract(src.value,'$.plan.head_shas') ELSE '[]' END) h WHERE h.type=head.type AND h.value=head.value)<>1 OR
 (SELECT count(*) FROM json_each(CASE WHEN json_type(m.parent,'$.composition_snapshot.revision_snapshots')='array' THEN m.parent->'$.composition_snapshot.revision_snapshots' ELSE '[]' END) frozen WHERE (CASE WHEN frozen.type='object' THEN frozen.value ELSE '{}' END)->>'repository_id'=m.job->>'$.inputs.repository_id' AND (CASE WHEN frozen.type='object' THEN frozen.value ELSE '{}' END)->>'head_commit'=head.value AND EXISTS (SELECT 1 FROM json_each(CASE WHEN json_type(c.value,'$.revision_ids')='array' THEN (CASE WHEN c.type='object' THEN c.value ELSE '{}' END)->'revision_ids' ELSE '[]' END) member_value WHERE member_value.type='text' AND member_value.value=(CASE WHEN frozen.type='object' THEN frozen.value ELSE '{}' END)->>'id'))<>1))
 AND NOT EXISTS(SELECT 1 FROM json_each(CASE WHEN json_type(m.parent,'$.composition_snapshot.components')='array' THEN json_extract(m.parent,'$.composition_snapshot.components') ELSE '[]' END) c CROSS JOIN json_each(CASE WHEN json_type(m.parent,'$.main_candidate.request.main_sources')='array' THEN json_extract(m.parent,'$.main_candidate.request.main_sources') ELSE '[]' END) src CROSS JOIN json_each(CASE WHEN json_type((CASE WHEN src.type='object' THEN src.value ELSE '{}' END)->'revisions')='array' THEN (CASE WHEN src.type='object' THEN src.value ELSE '{}' END)->'revisions' ELSE '[]' END) member
 WHERE m.parent->>'kind'='RELEASE_CANDIDATE' AND (CASE WHEN c.type='object' THEN c.value ELSE '{}' END)->>'application_id'=m.delivery->>'application_id' AND (CASE WHEN src.type='object' THEN src.value ELSE '{}' END)->>'application_id'=m.delivery->>'application_id'
 AND (coalesce((CASE WHEN member.type='object' THEN member.value ELSE '{}' END)->>'revision_id','')='' OR NOT EXISTS (SELECT 1 FROM json_each((CASE WHEN c.type='object' THEN c.value ELSE '{}' END)->'revision_ids') member_value WHERE member_value.type='text' AND member_value.value=((CASE WHEN member.type='object' THEN member.value ELSE '{}' END)->>'revision_id'))))
 AND m.delivery->>'$.snapshot.application.product_id'=$1
 AND m.delivery->>'application_id'=m.delivery->>'$.snapshot.application.id'
 AND EXISTS (SELECT 1 FROM json_each(m.parent->'child_ids') member_value WHERE member_value.type='text' AND member_value.value=m.delivery_id)
 AND ((m.parent->>'kind'='COMPOSE' AND EXISTS (
 SELECT 1 FROM json_each(CASE WHEN json_type(m.parent->'sources')='array' THEN m.parent->'sources' ELSE '[]' END) s
 WHERE (CASE WHEN s.type='object' THEN s.value ELSE '{}' END)->>'repository_id'=m.job->>'$.inputs.repository_id' AND (CASE WHEN s.type='object' THEN s.value ELSE '{}' END)->>'$.result.sha'=m.job->>'$.inputs.source_sha' AND json_type(s.value,'$.result.conflict')='false' AND EXISTS (SELECT 1 FROM json_each(json_extract(s.value,'$.plan.head_shas')) member_value WHERE member_value.type='text' AND member_value.value=((CASE WHEN r.type='object' THEN r.value ELSE '{}' END)->>'head_commit'))))
 OR (m.parent->>'kind'='RELEASE_CANDIDATE' AND m.parent->>'$.main_candidate.evidence_kind'='ATTRIBUTED_RCP_VERIFICATION' AND EXISTS (
 SELECT 1 FROM json_each(CASE WHEN json_type(m.parent,'$.main_candidate.request.main_sources')='array' THEN json_extract(m.parent,'$.main_candidate.request.main_sources') ELSE '[]' END) s
 WHERE (CASE WHEN s.type='object' THEN s.value ELSE '{}' END)->>'application_id'=m.delivery->>'application_id' AND (CASE WHEN s.type='object' THEN s.value ELSE '{}' END)->>'main_sha'=m.job->>'$.inputs.source_sha' AND (SELECT count(*) FROM json_each(CASE WHEN json_type((CASE WHEN s.type='object' THEN s.value ELSE '{}' END)->'revisions')='array' THEN (CASE WHEN s.type='object' THEN s.value ELSE '{}' END)->'revisions' ELSE '[]' END) x WHERE (CASE WHEN x.type='object' THEN x.value ELSE '{}' END)->>'revision_id'=(CASE WHEN r.type='object' THEN r.value ELSE '{}' END)->>'id')=1)))
 AND (CASE WHEN r.type='object' THEN r.value ELSE '{}' END)->>'product_id'=$1 AND (CASE WHEN r.type='object' THEN r.value ELSE '{}' END)->>'repository_id'=m.job->>'$.inputs.repository_id'
 AND coalesce((CASE WHEN r.type='object' THEN r.value ELSE '{}' END)->>'feature_id','')<>'' AND coalesce((CASE WHEN r.type='object' THEN r.value ELSE '{}' END)->>'integration_id','')<>''
 AND NOT EXISTS (SELECT 1 FROM json_each(CASE WHEN json_type(m.parent,'$.composition_snapshot.components')='array' THEN json_extract(m.parent,'$.composition_snapshot.components') ELSE '[]' END) c CROSS JOIN json_each(CASE WHEN json_type((CASE WHEN c.type='object' THEN c.value ELSE '{}' END)->'revision_ids')='array' THEN (CASE WHEN c.type='object' THEN c.value ELSE '{}' END)->'revision_ids' ELSE '[]' END) wanted
 WHERE (CASE WHEN c.type='object' THEN c.value ELSE '{}' END)->>'application_id'=m.delivery->>'$.snapshot.application.id' AND (SELECT count(*) FROM json_each(CASE WHEN json_type(m.parent,'$.composition_snapshot.revision_snapshots')='array' THEN json_extract(m.parent,'$.composition_snapshot.revision_snapshots') ELSE '[]' END) frozen WHERE (CASE WHEN frozen.type='object' THEN frozen.value ELSE '{}' END)->>'id'=wanted.value AND (CASE WHEN frozen.type='object' THEN frozen.value ELSE '{}' END)->>'product_id'=$1 AND (CASE WHEN frozen.type='object' THEN frozen.value ELSE '{}' END)->>'repository_id'=m.job->>'$.inputs.repository_id' AND coalesce((CASE WHEN frozen.type='object' THEN frozen.value ELSE '{}' END)->>'feature_id','')<>'' AND coalesce((CASE WHEN frozen.type='object' THEN frozen.value ELSE '{}' END)->>'integration_id','')<>''
 AND ((m.parent->>'kind'='COMPOSE' AND EXISTS (
 SELECT 1 FROM json_each(CASE WHEN json_type(m.parent->'sources')='array' THEN m.parent->'sources' ELSE '[]' END) src
 WHERE (CASE WHEN src.type='object' THEN src.value ELSE '{}' END)->>'repository_id'=m.job->>'$.inputs.repository_id' AND (CASE WHEN src.type='object' THEN src.value ELSE '{}' END)->>'$.result.sha'=m.job->>'$.inputs.source_sha' AND json_type(src.value,'$.result.conflict')='false' AND EXISTS (SELECT 1 FROM json_each(json_extract(src.value,'$.plan.head_shas')) member_value WHERE member_value.type='text' AND member_value.value=((CASE WHEN frozen.type='object' THEN frozen.value ELSE '{}' END)->>'head_commit'))))
 OR (m.parent->>'kind'='RELEASE_CANDIDATE' AND m.parent->>'$.main_candidate.evidence_kind'='ATTRIBUTED_RCP_VERIFICATION' AND EXISTS (
 SELECT 1 FROM json_each(CASE WHEN json_type(m.parent,'$.main_candidate.request.main_sources')='array' THEN json_extract(m.parent,'$.main_candidate.request.main_sources') ELSE '[]' END) src
 WHERE (CASE WHEN src.type='object' THEN src.value ELSE '{}' END)->>'application_id'=m.delivery->>'application_id' AND (CASE WHEN src.type='object' THEN src.value ELSE '{}' END)->>'main_sha'=m.job->>'$.inputs.source_sha' AND (SELECT count(*) FROM json_each(CASE WHEN json_type((CASE WHEN src.type='object' THEN src.value ELSE '{}' END)->'revisions')='array' THEN (CASE WHEN src.type='object' THEN src.value ELSE '{}' END)->'revisions' ELSE '[]' END) x WHERE (CASE WHEN x.type='object' THEN x.value ELSE '{}' END)->>'revision_id'=(CASE WHEN frozen.type='object' THEN frozen.value ELSE '{}' END)->>'id')=1))))<>1)
 AND EXISTS (SELECT 1 FROM json_each(CASE WHEN json_type(m.parent,'$.composition_snapshot.components')='array' THEN json_extract(m.parent,'$.composition_snapshot.components') ELSE '[]' END) c
 WHERE (CASE WHEN c.type='object' THEN c.value ELSE '{}' END)->>'application_id'=m.delivery->>'$.snapshot.application.id' AND EXISTS (SELECT 1 FROM json_each((CASE WHEN c.type='object' THEN c.value ELSE '{}' END)->'revision_ids') member_value WHERE member_value.type='text' AND member_value.value=((CASE WHEN r.type='object' THEN r.value ELSE '{}' END)->>'id')))
), selected AS (
 SELECT j.* FROM jobs j WHERE ($2='' OR EXISTS(SELECT 1 FROM links l WHERE l.job_id=j.id AND l.revision->>'feature_id'=$2)) AND ($3='' OR j.id=$3)
)`

func sqliteHistoryStringSQL(path string) string { return "substr(coalesce(" + path + ",''),1,129)" }
func (s *SQLiteStore) ReadWorkerHistory(ctx context.Context, q domain.WorkerHistoryMetadataRequest) (domain.WorkerHistoryMetadata, error) {
	var out domain.WorkerHistoryMetadata
	if !validHistoryRequest(q) {
		return out, errors.New("invalid worker history request")
	}
	tx, e := s.begin(ctx, true)
	if e != nil {
		return out, e
	}
	defer tx.Rollback(ctx)
	hidden, e := sqliteHiddenProducts(ctx, tx)
	if e != nil {
		return out, e
	}
	e = tx.QueryRow(ctx, `SELECT `+sqliteVisibleProductSQL, q.ProductID, hidden).Scan(&out.ProductFound)
	if e != nil || !out.ProductFound {
		return out, e
	}
	e = tx.QueryRow(ctx, `SELECT ($2='' OR EXISTS(SELECT 1 FROM records WHERE kind='features' AND id=$2 AND product_id=$1))`, q.ProductID, q.FeatureID).Scan(&out.FeatureFound)
	if e != nil || !out.ProductFound || !out.FeatureFound {
		return out, e
	}
	args := []any{q.ProductID, q.FeatureID, q.JobID, q.AfterID, q.Limit}
	e = tx.QueryRow(ctx, sqliteHistoryCTE+`SELECT ($3='' OR EXISTS(SELECT 1 FROM selected))`, args[:3]...).Scan(&out.JobFound)
	if e != nil || !out.JobFound {
		return out, e
	}
	var query string
	base := func(alias string, fields map[string]string) string {
		text := `json_object('id',` + sqliteHistoryStringSQL(alias+`.id`) + `,'product_id',$1`
		// Fixed keys, never user SQL. JSON serialization/order is immaterial here.
		keys := make([]string, 0, len(fields))
		for k := range fields {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			text += `,'` + k + `',` + sqliteHistoryStringSQL(fields[k])
		}
		return text
	}
	switch q.Collection {
	case "jobs":
		query = sqliteHistoryCTE + `SELECT ` + base("j", map[string]string{"delivery_id": "j.document->>'$.inputs.source_proof.delivery_id'", "parent_id": "j.document->>'$.inputs.source_proof.parent_id'", "snapshot_hash": "j.document->>'$.inputs.source_proof.snapshot_hash'", "parent_hash": "j.document->>'$.inputs.source_proof.parent_hash'", "operation_id": "j.document->>'operation_id'", "integration_id": "j.document->>'integration_id'", "recorded_status": "j.document->>'status'", "capability": "j.document->>'capability'", "source_sha": "j.document->>'$.inputs.source_sha'", "revision_id": "j.document->>'$.inputs.revision_id'", "repository_id": "j.document->>'$.inputs.repository_id'", "recipe_hash": "j.document->>'$.inputs.recipe_hash'", "input_hash": "j.document->>'input_hash'", "created_at": "j.document->>'created_at'", "updated_at": "j.document->>'updated_at'"}) + `, 'provenance',CASE WHEN EXISTS(SELECT 1 FROM links l WHERE l.job_id=j.id) THEN CASE WHEN json_type(j.document,'$.inputs.source_proof') IS NULL THEN 'recorded_revision' ELSE 'recorded_snapshot_unverified' END ELSE 'unknown' END,'attempts',(SELECT count(*) FROM records a WHERE a.kind='worker_attempts' AND a.product_id=$1 AND a.document->>'job_id'=j.id),'links',(SELECT count(DISTINCT l.revision->>'id') FROM links l WHERE l.job_id=j.id)) FROM selected j WHERE j.id COLLATE BINARY>$4 COLLATE BINARY ORDER BY j.id COLLATE BINARY LIMIT $5`
	case "attempts":
		query = sqliteHistoryCTE + `SELECT ` + base("a", map[string]string{"job_id": "a.document->>'job_id'", "recorded_status": "a.document->>'status'", "input_hash": "a.document->>'input_hash'", "created_at": "a.document->>'created_at'", "updated_at": "a.document->>'updated_at'"}) + `) FROM records a WHERE a.kind='worker_attempts' AND a.product_id=$1 AND a.document->>'job_id'=$3 AND EXISTS(SELECT 1 FROM selected) AND a.id COLLATE BINARY>$4 COLLATE BINARY ORDER BY a.id COLLATE BINARY LIMIT $5`
	case "links":
		query = sqliteHistoryCTE + `SELECT json_object('id',substr(l.revision->>'id',1,129),'product_id',$1,'job_id',$3,'revision_id',substr(l.revision->>'id',1,129),'feature_id',substr(l.revision->>'feature_id',1,129),'integration_id',substr(l.revision->>'integration_id',1,129),'repository_id',substr(l.revision->>'repository_id',1,129),'source_sha',substr(l.revision->>'head_commit',1,129),'provenance',l.provenance) FROM (SELECT * FROM (SELECT *,row_number() OVER(PARTITION BY revision->>'id' ORDER BY provenance) AS rn FROM links WHERE job_id=$3) WHERE rn=1) l WHERE EXISTS(SELECT 1 FROM selected) AND (l.revision->>'id') COLLATE BINARY>$4 COLLATE BINARY ORDER BY (l.revision->>'id') COLLATE BINARY LIMIT $5`
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
