package application

import (
	"context"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"net/url"
	"os"
	"path/filepath"
	"releasecontrol/internal/buildregistry"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/persistence"
	"strings"
	"testing"
	"time"
)

type buildTestStore interface {
	Store
	Close()
}

func TestBuildDispatcherDurableRestart(t *testing.T) {
	for _, kind := range []string{"bbolt", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			var open func() (buildTestStore, error)
			if kind == "bbolt" {
				path := filepath.Join(t.TempDir(), "build.db")
				open = func() (buildTestStore, error) { return persistence.OpenLocal(ctx, path) }
			} else {
				dsn := os.Getenv("TEST_DATABASE_URL")
				if dsn == "" {
					t.Skip("TEST_DATABASE_URL not configured; PostgreSQL dispatcher restart skipped")
				}
				u, e := url.Parse(dsn)
				if e != nil {
					t.Fatal(e)
				}
				pool, e := pgxpool.New(ctx, dsn)
				if e != nil {
					t.Fatal(e)
				}
				defer pool.Close()
				schema := fmt.Sprintf("build_resume_%d", time.Now().UnixNano())
				if _, e = pool.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); e != nil {
					t.Fatal(e)
				}
				defer func() {
					if _, e := pool.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE"); e != nil {
						t.Error(e)
					}
				}()
				q := u.Query()
				q.Set("search_path", schema)
				u.RawQuery = q.Encode()
				open = func() (buildTestStore, error) { return persistence.Open(ctx, u.String()) }
			}
			store, e := open()
			if e != nil {
				t.Fatal(e)
			}
			if e = store.Update(ctx, func(st *domain.State) error {
				st.Products = append(st.Products, domain.Product{Meta: domain.Meta{ID: "p"}})
				return nil
			}); e != nil {
				t.Fatal(e)
			}
			s := New(store)
			f := &fakeForgejo{head: strings.Repeat("a", 40), uncertain: true}
			r := &fakeBuildRegistry{err: buildregistry.ErrMissing}
			if e = s.ConfigureBuilds(buildConfig(), f, r); e != nil {
				t.Fatal(e)
			}
			b := queue(t, s, "durable")
			tick(t, s)
			if getBuild(t, s, b).Status != "RECONCILIATION_REQUIRED" {
				t.Fatal("intent lost")
			}
			store.Close()
			store, e = open()
			if e != nil {
				t.Fatal(e)
			}
			defer store.Close()
			s = New(store)
			if e = s.ConfigureBuilds(buildConfig(), f, r); e != nil {
				t.Fatal(e)
			}
			tick(t, s)
			f.runs[0].Status = "failure"
			tick(t, s)
			if v := getBuild(t, s, b); v.Status != "FAILED" || f.dispatches != 1 {
				t.Fatal(v)
			}
			if same := queue(t, s, "durable"); same.ID != b.ID {
				t.Fatal("durable key lost")
			}
		})
	}
}
