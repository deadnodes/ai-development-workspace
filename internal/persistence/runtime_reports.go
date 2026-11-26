package persistence

import (
	"context"
	"encoding/json"
	"github.com/jackc/pgx/v5"
	bolt "go.etcd.io/bbolt"
	"releasecontrol/internal/auth"
	"releasecontrol/internal/domain"
	rr "releasecontrol/internal/runtimereport"
	"strings"
	"time"
)

var runtimeReportsBucket = []byte("runtime-reports")

func (s *LocalStore) SaveRuntimeReport(ctx context.Context, r rr.Report, now time.Time) (changes rr.JournalResult, err error) {
	s.lifecycle.RLock()
	defer s.lifecycle.RUnlock()
	err = s.db.Update(func(tx *bolt.Tx) error {
		if e := ctx.Err(); e != nil {
			return e
		}
		if auth.RequiresExistingProduct(ctx) {
			visible, err := localProductAccess(ctx, tx, r.ProductID)
			if err != nil {
				return err
			}
			if !visible {
				return domain.ErrReadScopeNotFound
			}
		}
		b, e := tx.CreateBucketIfNotExists(runtimeReportsBucket)
		if e != nil {
			return e
		}
		key := []byte(rr.Scope(r.ProductID, r.EnvironmentID, r.Cluster))
		var old rr.Snapshot
		if raw := b.Get(key); raw != nil {
			if e = json.Unmarshal(raw, &old); e != nil {
				return e
			}
		}
		prefix := []byte(rr.Scope(r.ProductID, r.EnvironmentID, ""))
		cur := b.Cursor()
		for k, v := cur.Seek(prefix); k != nil && strings.HasPrefix(string(k), string(prefix)); k, v = cur.Next() {
			var snap rr.Snapshot
			if e = json.Unmarshal(v, &snap); e != nil {
				return e
			}
			if now.Sub(snap.AcceptedAt) < 20*time.Second {
				return rr.ErrRateLimit
			}
		}
		next, c, e := rr.Apply(old, r, now)
		if e != nil {
			return e
		}
		next.Journal, changes = rr.ReserveJournal(old.Journal, c, now)
		raw, e := json.Marshal(next)
		if e != nil {
			return e
		}
		return b.Put(key, raw)
	})
	return
}
func (s *LocalStore) ReadRuntimeReports(ctx context.Context, p, e, c string, windows ...rr.ReadWindow) (out []rr.Cluster, err error) {
	out = []rr.Cluster{}
	s.lifecycle.RLock()
	defer s.lifecycle.RUnlock()
	err = s.db.View(func(tx *bolt.Tx) error {
		if auth.RequiresExistingProduct(ctx) {
			data := tx.Bucket(localBucket).Get(localStateKey)
			hidden, err := hiddenLocalProducts(ctx, data)
			if err != nil {
				return err
			}
			if hidden[p] || historyRecord(ctx, data, "products", p, "") == nil {
				return domain.ErrReadScopeNotFound
			}
		}
		b := tx.Bucket(runtimeReportsBucket)
		if b == nil {
			return nil
		}
		prefix := rr.Scope(p, e, "")
		cur := b.Cursor()
		for k, v := cur.Seek([]byte(prefix)); k != nil && strings.HasPrefix(string(k), prefix); k, v = cur.Next() {
			if err := ctx.Err(); err != nil {
				return err
			}
			var snap rr.Snapshot
			if err := json.Unmarshal(v, &snap); err != nil {
				return err
			}
			if c == "" || snap.Cluster.Cluster == c {
				out = append(out, snap.Cluster)
				if len(windows) > 0 {
					out = rr.WindowClusters(out, windows...)
					n := 0
					for _, v := range out {
						if len(v.Records) == 0 {
							n++
						} else {
							n += len(v.Records)
						}
					}
					if n >= windows[0].Limit {
						break
					}
				}
			}
		}
		return nil
	})
	return
}
func (s *Store) SaveRuntimeReport(ctx context.Context, r rr.Report, now time.Time) (rr.JournalResult, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return rr.JournalResult{}, err
	}
	defer tx.Rollback(ctx)
	if auth.RequiresExistingProduct(ctx) {
		var revision int64
		if err := tx.QueryRow(ctx, "SELECT revision FROM control_revision WHERE id=1 FOR SHARE").Scan(&revision); err != nil {
			return rr.JournalResult{}, err
		}
		visible, err := readProductAccess(ctx, tx, r.ProductID)
		if err != nil {
			return rr.JournalResult{}, err
		}
		if !visible {
			return rr.JournalResult{}, domain.ErrReadScopeNotFound
		}
	}
	// Serialize the absent-set reconciliation and rate limit even for a new cluster.
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, strings.ReplaceAll(rr.Scope(r.ProductID, r.EnvironmentID, ""), "\x00", "/")); err != nil {
		return rr.JournalResult{}, err
	}
	var latest time.Time
	if err = tx.QueryRow(ctx, `SELECT COALESCE(MAX(accepted_at),'epoch'::timestamptz) FROM runtime_report_clusters WHERE product_id=$1 AND environment_id=$2`, r.ProductID, r.EnvironmentID).Scan(&latest); err != nil {
		return rr.JournalResult{}, err
	}
	if now.Sub(latest) < 20*time.Second {
		return rr.JournalResult{}, rr.ErrRateLimit
	}
	old := rr.Snapshot{}
	var budgetRaw []byte
	err = tx.QueryRow(ctx, `SELECT reported_at,workloads,accepted_at,journal_budget FROM runtime_report_clusters WHERE product_id=$1 AND environment_id=$2 AND cluster=$3`, r.ProductID, r.EnvironmentID, r.Cluster).Scan(&old.Cluster.ReportedAt, &old.Cluster.Workloads, &old.AcceptedAt, &budgetRaw)
	if err != nil && err != pgx.ErrNoRows {
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
	if _, err = tx.Exec(ctx, `DELETE FROM runtime_reports WHERE product_id=$1 AND environment_id=$2 AND cluster=$3`, r.ProductID, r.EnvironmentID, r.Cluster); err != nil {
		return rr.JournalResult{}, err
	}
	for _, v := range next.Cluster.Records {
		raw, err := json.Marshal(v.Document)
		if err != nil {
			return rr.JournalResult{}, err
		}
		w := v.Document
		_, err = tx.Exec(ctx, `INSERT INTO runtime_reports VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, r.ProductID, r.EnvironmentID, r.Cluster, w.Namespace, w.Kind, w.Name, raw, v.ObservedAt, v.ChangedAt, v.AbsentSince)
		if err != nil {
			return rr.JournalResult{}, err
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO runtime_report_clusters (product_id,environment_id,cluster,reported_at,workloads,accepted_at,journal_budget) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(product_id,environment_id,cluster) DO UPDATE SET reported_at=EXCLUDED.reported_at,workloads=EXCLUDED.workloads,accepted_at=EXCLUDED.accepted_at,journal_budget=EXCLUDED.journal_budget`, r.ProductID, r.EnvironmentID, r.Cluster, r.ReportedAt, len(r.Workloads), now, budgetRaw)
	if err != nil {
		return rr.JournalResult{}, err
	}
	return journal, tx.Commit(ctx)
}
func (s *Store) ReadRuntimeReports(ctx context.Context, p, e, c string, windows ...rr.ReadWindow) ([]rr.Cluster, error) {
	after := ""
	limit := int64(9223372036854775807)
	if len(windows) > 0 {
		after = windows[0].After
		limit = int64(windows[0].Limit)
	}
	queryer := interface {
		Query(context.Context, string, ...any) (pgx.Rows, error)
	}(s.pool)
	if auth.RequiresExistingProduct(ctx) {
		tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
		if err != nil {
			return nil, err
		}
		defer tx.Rollback(ctx)
		hidden, err := hiddenProducts(ctx, tx)
		if err != nil {
			return nil, err
		}
		visible, err := visibleProductIn(ctx, tx, p, hidden)
		if err != nil {
			return nil, err
		}
		if !visible {
			return nil, domain.ErrReadScopeNotFound
		}
		queryer = tx
	}
	rows, err := queryer.Query(ctx, `SELECT c.cluster,c.reported_at,c.workloads,r.document,r.observed_at,r.changed_at,r.absent_since
 FROM runtime_report_clusters c LEFT JOIN runtime_reports r USING(product_id,environment_id,cluster)
 WHERE c.product_id=$1 AND c.environment_id=$2 AND ($3='' OR c.cluster=$3)
 AND (c.cluster || '!' || COALESCE(r.namespace || '/' || r.kind || '/' || r.name,'')) COLLATE "C" > $4
 ORDER BY (c.cluster || '!' || COALESCE(r.namespace || '/' || r.kind || '/' || r.name,'')) COLLATE "C" LIMIT $5`, p, e, c, after, limit)
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
