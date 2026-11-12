package persistence

import (
	"context"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"net/url"
	"os"
	"path/filepath"
	"releasecontrol/internal/domain"
	"testing"
	"time"
)

func TestBuildPersistenceBothStores(t *testing.T) {
	for _, kind := range []string{"bbolt", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			var open func() (interface {
				Read(context.Context) (domain.State, error)
				Update(context.Context, func(*domain.State) error) error
				Close()
			}, error)
			if kind == "bbolt" {
				path := filepath.Join(t.TempDir(), "state.db")
				open = func() (interface {
					Read(context.Context) (domain.State, error)
					Update(context.Context, func(*domain.State) error) error
					Close()
				}, error) {
					return OpenLocal(ctx, path)
				}
			} else {
				dsn := os.Getenv("TEST_DATABASE_URL")
				if dsn == "" {
					t.Skip("TEST_DATABASE_URL not configured; PostgreSQL build persistence skipped")
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
				schema := fmt.Sprintf("build_test_%d", time.Now().UnixNano())
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
				open = func() (interface {
					Read(context.Context) (domain.State, error)
					Update(context.Context, func(*domain.State) error) error
					Close()
				}, error) {
					return Open(ctx, u.String())
				}
			}
			s, e := open()
			if e != nil {
				t.Fatal(e)
			}
			now := time.Now().UTC()
			e = s.Update(ctx, func(st *domain.State) error {
				st.Products = append(st.Products, domain.Product{Meta: domain.Meta{ID: "p"}})
				st.Builds = append(st.Builds, domain.Build{Meta: domain.Meta{ID: "build", ProductID: "p", Actor: "agent", CreatedAt: now, UpdatedAt: now}, ProfileID: "app", Status: "RECONCILIATION_REQUIRED", ProviderRunID: 42, SourceSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Workflow: "build.yml", ImageRef: "http://registry/o/r:br-aaaaaaaa@sha256:abc", Digest: "sha256:abc", Reused: "run", QueuedAt: now, DispatchedAt: &now})
				st.BuildReceipts = append(st.BuildReceipts, domain.BuildReceipt{Meta: domain.Meta{ID: "receipt", ProductID: "p"}, BuildID: "build", RequestKey: "key", ProfileID: "app", Workflow: "build.yml", SourceSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"})
				return nil
			})
			if e != nil {
				t.Fatal(e)
			}
			s.Close()
			s, e = open()
			if e != nil {
				t.Fatal(e)
			}
			defer s.Close()
			st, e := s.Read(ctx)
			if e != nil || len(st.Builds) != 1 || st.Builds[0].ProviderRunID != 42 || st.Builds[0].Status != "RECONCILIATION_REQUIRED" || len(st.BuildReceipts) != 1 || st.Builds[0].Workflow != "build.yml" || st.Builds[0].Reused != "run" || st.Builds[0].Digest != "sha256:abc" || st.BuildReceipts[0].Workflow != "build.yml" || st.BuildReceipts[0].SourceSHA != st.Builds[0].SourceSHA {
				t.Fatal(st.Builds, e)
			}
			rollback := errors.New("rollback")
			if e = s.Update(ctx, func(st *domain.State) error { st.Builds[0].Status = "SUCCEEDED"; return rollback }); !errors.Is(e, rollback) {
				t.Fatal(e)
			}
			st, e = s.Read(ctx)
			if e != nil || st.Builds[0].Status != "RECONCILIATION_REQUIRED" {
				t.Fatal("rollback lost", e)
			}
		})
	}
}
