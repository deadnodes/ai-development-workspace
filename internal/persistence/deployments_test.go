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

func TestDeploymentPersistenceBothStores(t *testing.T) {
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
					t.Skip("TEST_DATABASE_URL not configured; PostgreSQL deployment persistence skipped")
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
				schema := fmt.Sprintf("deployment_test_%d", time.Now().UnixNano())
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
				st.Deployments = append(st.Deployments, domain.Deployment{Meta: domain.Meta{ID: "deployment", ProductID: "p", Actor: "agent", CreatedAt: now, UpdatedAt: now}, GitOpsRepository: "o/flux", TargetFiles: []string{"dev.yaml"}, BuildID: "build", ProfileID: "app", EnvironmentID: "dev", Status: "RECONCILIATION_REQUIRED", UncertainAction: "merge", PRNumber: 7, PRURL: "https://github.example/pr/7", HeadSHA: "head", MergeCommitSHA: "merge", CIState: "success", CIContexts: map[string]string{"verify": "success"}, RegistryCheckedAt: &now, Images: []domain.BuildImage{{ID: "app", Digest: "sha256:abc"}}})
				st.DeploymentReceipts = append(st.DeploymentReceipts, domain.DeploymentReceipt{Meta: domain.Meta{ID: "receipt", ProductID: "p"}, RequestKey: "key", BuildID: "build", DeploymentID: "deployment", EnvironmentID: "dev"})
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
			if e != nil || len(st.Deployments) != 1 || st.Deployments[0].PRNumber != 7 || st.Deployments[0].GitOpsRepository != "o/flux" || len(st.Deployments[0].TargetFiles) != 1 || st.Deployments[0].TargetFiles[0] != "dev.yaml" || st.Deployments[0].Status != "RECONCILIATION_REQUIRED" || st.Deployments[0].UncertainAction != "merge" || st.Deployments[0].CIContexts["verify"] != "success" || st.Deployments[0].RegistryCheckedAt == nil || len(st.DeploymentReceipts) != 1 || st.DeploymentReceipts[0].DeploymentID != "deployment" || st.Deployments[0].Images[0].Digest != "sha256:abc" {
				t.Fatal(st.Deployments, e)
			}
			rollback := errors.New("rollback")
			if e = s.Update(ctx, func(st *domain.State) error { st.Deployments[0].Status = "SUCCEEDED"; return rollback }); !errors.Is(e, rollback) {
				t.Fatal(e)
			}
			st, e = s.Read(ctx)
			if e != nil || st.Deployments[0].Status != "RECONCILIATION_REQUIRED" {
				t.Fatal("rollback lost", e)
			}
		})
	}
}
