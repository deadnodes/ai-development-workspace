package persistence

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"releasecontrol/internal/domain"
	rr "releasecontrol/internal/runtimereport"
	"testing"
	"time"
)

func TestRuntimeReportsStores(t *testing.T) {
	for _, backend := range []string{"local", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			var reports rr.Store
			var read func(context.Context) (domain.State, error)
			if backend == "postgres" {
				s := retrievalPostgres(t)
				reports = s
				read = s.Read
			} else {
				s, e := OpenLocal(ctx, filepath.Join(t.TempDir(), "state.db"))
				if e != nil {
					t.Fatal(e)
				}
				defer s.Close()
				reports = s
				read = s.Read
			}
			before, e := read(ctx)
			if e != nil {
				t.Fatal(e)
			}
			at := time.Now().UTC().Truncate(time.Microsecond)
			r := rr.Report{ProductID: "p", EnvironmentID: "prod", Cluster: "c", ReportedAt: at, Workloads: []rr.Workload{{Namespace: "app", Kind: "Deployment", Name: "api", ReadyReplicas: 1, Containers: []rr.Container{{Name: "api", Image: "v1"}}}}}
			changes, e := reports.SaveRuntimeReport(ctx, r, at)
			if e != nil || len(changes.Changes) != 1 {
				t.Fatal(changes, e)
			}
			if _, e = reports.SaveRuntimeReport(ctx, r, at.Add(time.Second)); !errors.Is(e, rr.ErrRateLimit) {
				t.Fatal(e)
			}
			rotated := r
			rotated.Cluster = "other"
			if _, err := reports.SaveRuntimeReport(ctx, rotated, at.Add(19*time.Second)); !errors.Is(err, rr.ErrRateLimit) {
				t.Fatal("cluster rotation", err)
			}
			r.ReportedAt = at.Add(time.Minute)
			changes, e = reports.SaveRuntimeReport(ctx, r, r.ReportedAt)
			if e != nil || len(changes.Changes) != 0 {
				t.Fatal(changes, e)
			}
			got, e := reports.ReadRuntimeReports(ctx, "p", "prod", "c")
			if e != nil || len(got) != 1 || !got[0].Records[0].ChangedAt.Equal(at) || !got[0].Records[0].ObservedAt.Equal(r.ReportedAt) {
				t.Fatal(got, e)
			}
			r.ReportedAt = at.Add(2 * time.Minute)
			r.Workloads = nil
			changes, e = reports.SaveRuntimeReport(ctx, r, r.ReportedAt)
			if e != nil || len(changes.Changes) != 1 || !changes.Changes[0].New.Absent {
				t.Fatal(changes, e)
			}
			r.ReportedAt = at.Add(8 * 24 * time.Hour)
			if _, err := reports.SaveRuntimeReport(ctx, r, r.ReportedAt); err != nil {
				t.Fatal(err)
			}
			got, e = reports.ReadRuntimeReports(ctx, "p", "prod", "c")
			if e != nil || len(got) != 1 || len(got[0].Records) != 0 {
				t.Fatal("expired tombstone", got, e)
			}
			after, e := read(ctx)
			if e != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("report modified State", e)
			}
			if s, ok := reports.(*Store); ok {
				var revision int64
				if e = s.pool.QueryRow(ctx, "SELECT revision FROM control_revision WHERE id=1").Scan(&revision); e != nil || revision != 0 {
					t.Fatalf("revision=%d err=%v", revision, e)
				}
			}
		})
	}
}

func TestRuntimeReportsStorePagination(t *testing.T) {
	for _, backend := range []string{"local", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			var store rr.Store
			if backend == "postgres" {
				store = retrievalPostgres(t)
			} else {
				local, err := OpenLocal(ctx, filepath.Join(t.TempDir(), "state.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer local.Close()
				store = local
			}
			at := time.Now().UTC()
			for i, c := range []string{"c", "c-other", "z"} {
				r := rr.Report{ProductID: "p", EnvironmentID: "prod", Cluster: c, ReportedAt: at.Add(time.Duration(i) * time.Minute)}
				if c != "z" {
					r.Workloads = []rr.Workload{{Namespace: "app", Kind: "Deployment", Name: "api"}, {Namespace: "app", Kind: "Deployment", Name: "web"}}
				}
				if _, err := store.SaveRuntimeReport(ctx, r, r.ReportedAt); err != nil {
					t.Fatal(err)
				}
			}
			after := ""
			for _, want := range []string{"c!app/Deployment/api", "c!app/Deployment/web", "c-other!app/Deployment/api", "c-other!app/Deployment/web", "z!"} {
				got, err := store.ReadRuntimeReports(ctx, "p", "prod", "", rr.ReadWindow{After: after, Limit: 1})
				if err != nil || len(got) != 1 {
					t.Fatal(got, err)
				}
				key := got[0].Cluster + "!"
				if len(got[0].Records) > 1 {
					t.Fatal("unbounded page", got)
				}
				if len(got[0].Records) == 1 {
					key += rr.Key(got[0].Records[0].Document)
				}
				if key != want {
					t.Fatalf("got %s want %s", key, want)
				}
				after = key
			}
			got, err := store.ReadRuntimeReports(ctx, "p", "prod", "", rr.ReadWindow{After: after, Limit: 1})
			if err != nil || len(got) != 0 {
				t.Fatal(got, err)
			}
		})
	}
}
