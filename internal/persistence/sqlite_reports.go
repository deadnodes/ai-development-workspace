package persistence

import (
	"context"
	"database/sql"
	"encoding/json"
	"releasecontrol/internal/auth"
	"releasecontrol/internal/domain"
	rr "releasecontrol/internal/runtimereport"
	"time"
)

func (s *SQLiteStore) SaveRuntimeReport(ctx context.Context, r rr.Report, now time.Time) (rr.JournalResult, error) {
	tx, err := s.begin(ctx, false)
	if err != nil {
		return rr.JournalResult{}, err
	}
	defer tx.Rollback(ctx)
	if auth.RequiresExistingProduct(ctx) {
		visible, err := sqliteProductAccess(ctx, tx, r.ProductID)
		if err != nil {
			return rr.JournalResult{}, err
		}
		if !visible {
			return rr.JournalResult{}, domain.ErrReadScopeNotFound
		}
	}
	var latest time.Time
	if err = tx.QueryRow(ctx, `SELECT accepted_at FROM runtime_report_clusters WHERE product_id=$1 AND environment_id=$2 ORDER BY accepted_at DESC LIMIT 1`, r.ProductID, r.EnvironmentID).Scan(&latest); err != nil && err != sql.ErrNoRows {
		return rr.JournalResult{}, err
	}
	if !latest.IsZero() && now.Sub(latest) < 20*time.Second {
		return rr.JournalResult{}, rr.ErrRateLimit
	}
	old := rr.Snapshot{}
	var budgetRaw []byte
	err = tx.QueryRow(ctx, `SELECT reported_at,workloads,accepted_at,journal_budget FROM runtime_report_clusters WHERE product_id=$1 AND environment_id=$2 AND cluster=$3`, r.ProductID, r.EnvironmentID, r.Cluster).Scan(&old.Cluster.ReportedAt, &old.Cluster.Workloads, &old.AcceptedAt, &budgetRaw)
	if err != nil && err != sql.ErrNoRows {
		return rr.JournalResult{}, err
	}
	if len(budgetRaw) > 0 {
		if err = json.Unmarshal(budgetRaw, &old.Journal); err != nil {
			return rr.JournalResult{}, err
		}
	}
	rows, err := tx.Query(ctx, `SELECT document,observed_at,changed_at,absent_since FROM runtime_reports WHERE product_id=$1 AND environment_id=$2 AND cluster=$3`, r.ProductID, r.EnvironmentID, r.Cluster)
	if err != nil {
		return rr.JournalResult{}, err
	}
	for rows.Next() {
		var rec rr.Record
		var raw []byte
		if err = rows.Scan(&raw, &rec.ObservedAt, &rec.ChangedAt, &rec.AbsentSince); err != nil {
			rows.Close()
			return rr.JournalResult{}, err
		}
		if err = json.Unmarshal(raw, &rec.Document); err != nil {
			rows.Close()
			return rr.JournalResult{}, err
		}
		old.Cluster.Records = append(old.Cluster.Records, rec)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return rr.JournalResult{}, err
	}
	next, changes, err := rr.Apply(old, r, now)
	if err != nil {
		return rr.JournalResult{}, err
	}
	var journal rr.JournalResult
	next.Journal, journal = rr.ReserveJournal(old.Journal, changes, now)
	budgetRaw, err = json.Marshal(next.Journal)
	if err != nil {
		return rr.JournalResult{}, err
	}
	// Replace only this cluster's bounded observation set atomically, removing expired/evicted tombstones.
	if err = tx.Exec(ctx, `DELETE FROM runtime_reports WHERE product_id=$1 AND environment_id=$2 AND cluster=$3`, r.ProductID, r.EnvironmentID, r.Cluster); err != nil {
		return rr.JournalResult{}, err
	}
	for _, v := range next.Cluster.Records {
		raw, err := json.Marshal(v.Document)
		if err != nil {
			return rr.JournalResult{}, err
		}
		w := v.Document
		err = tx.Exec(ctx, `INSERT INTO runtime_reports VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, r.ProductID, r.EnvironmentID, r.Cluster, w.Namespace, w.Kind, w.Name, string(raw), v.ObservedAt, v.ChangedAt, v.AbsentSince)
		if err != nil {
			return rr.JournalResult{}, err
		}
	}
	err = tx.Exec(ctx, `INSERT INTO runtime_report_clusters (product_id,environment_id,cluster,reported_at,workloads,accepted_at,journal_budget) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(product_id,environment_id,cluster) DO UPDATE SET reported_at=EXCLUDED.reported_at,workloads=EXCLUDED.workloads,accepted_at=EXCLUDED.accepted_at,journal_budget=EXCLUDED.journal_budget`, r.ProductID, r.EnvironmentID, r.Cluster, r.ReportedAt, len(r.Workloads), now, string(budgetRaw))
	if err != nil {
		return rr.JournalResult{}, err
	}
	return journal, tx.Commit(ctx)
}
func (s *SQLiteStore) ReadRuntimeReports(ctx context.Context, p, e, c string, windows ...rr.ReadWindow) ([]rr.Cluster, error) {
	after := ""
	limit := int64(9223372036854775807)
	if len(windows) > 0 {
		after = windows[0].After
		limit = int64(windows[0].Limit)
	}
	tx, err := s.begin(ctx, true)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if auth.RequiresExistingProduct(ctx) {
		hidden, err := sqliteHiddenProducts(ctx, tx)
		if err != nil {
			return nil, err
		}
		visible, err := sqliteVisibleProductIn(ctx, tx, p, hidden)
		if err != nil {
			return nil, err
		}
		if !visible {
			return nil, domain.ErrReadScopeNotFound
		}
	}
	rows, err := tx.Query(ctx, `SELECT c.cluster,c.reported_at,c.workloads,r.document,r.observed_at,r.changed_at,r.absent_since
 FROM runtime_report_clusters c LEFT JOIN runtime_reports r USING(product_id,environment_id,cluster)
 WHERE c.product_id=$1 AND c.environment_id=$2 AND ($3='' OR c.cluster=$3)
 AND (c.cluster || '!' || COALESCE(r.namespace || '/' || r.kind || '/' || r.name,'')) COLLATE BINARY > $4
 ORDER BY (c.cluster || '!' || COALESCE(r.namespace || '/' || r.kind || '/' || r.name,'')) COLLATE BINARY LIMIT $5`, p, e, c, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []rr.Cluster{}
	for rows.Next() {
		v := rr.Cluster{ProductID: p, EnvironmentID: e, Records: []rr.Record{}}
		var raw []byte
		var observed, changed, absent *time.Time
		if err = rows.Scan(&v.Cluster, &v.ReportedAt, &v.Workloads, &raw, &observed, &changed, &absent); err != nil {
			return nil, err
		}
		if len(out) == 0 || out[len(out)-1].Cluster != v.Cluster {
			out = append(out, v)
		}
		if raw != nil {
			var record rr.Record
			if err = json.Unmarshal(raw, &record.Document); err != nil {
				return nil, err
			}
			record.ObservedAt = *observed
			record.ChangedAt = *changed
			record.AbsentSince = *absent
			i := len(out) - 1
			out[i].Records = append(out[i].Records, record)
		}
	}
	return out, rows.Err()
}
