package application

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
)

// Unlike the production borrowed-read contract, this wrapper detects every
// undeclared write and every modification of an append-only prefix.
type enforcingWorkerStore struct {
	Store
	t      *testing.T
	scopes []domain.StateMutationScope
}

func (s *enforcingWorkerStore) Update(context.Context, func(*domain.State) error) error {
	s.t.Fatal("unexpected full Update on hot path")
	return nil
}
func cloneWorkerState(t *testing.T, st domain.State) domain.State {
	t.Helper()
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	var out domain.State
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
func (s *enforcingWorkerStore) UpdateScoped(ctx context.Context, scope domain.StateMutationScope, fn func(*domain.State) error) error {
	s.scopes = append(s.scopes, scope)
	checked := func(st *domain.State) error {
		before := cloneWorkerState(s.t, *st)
		err := fn(st)
		old, cur := reflect.ValueOf(before), reflect.ValueOf(*st)
		for i := 0; i < old.NumField(); i++ {
			name := old.Type().Field(i).Tag.Get("json")
			mutable, appendOnly := false, name == "events" // events are implicit in the store contract
			for _, n := range scope.MutableCollections {
				mutable = mutable || n == name
			}
			for _, n := range scope.AppendCollections {
				appendOnly = appendOnly || n == name
			}
			if mutable {
				continue
			}
			a, b := old.Field(i), cur.Field(i)
			if appendOnly {
				if b.Len() < a.Len() || !reflect.DeepEqual(a.Interface(), b.Slice(0, a.Len()).Interface()) {
					s.t.Fatalf("modified append-only %s", name)
				}
			} else if !reflect.DeepEqual(a.Interface(), b.Interface()) {
				s.t.Fatalf("out-of-scope write to %s", name)
			}
		}
		return err
	}
	if scoped, ok := s.Store.(interface {
		UpdateScoped(context.Context, domain.StateMutationScope, func(*domain.State) error) error
	}); ok {
		return scoped.UpdateScoped(ctx, scope, checked)
	}
	return s.Store.Update(ctx, checked)
}

// Hide UpdateScoped to exercise the original full-Update fallback.
type fullWorkerStore struct{ Store }

// Compare every persisted field, canonicalizing generated identities and wall-clock
// timestamps. Backoff durations are asserted separately below.
func normalizedWorkerState(t *testing.T, st domain.State) string {
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	text := regexp.MustCompile(`\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d+)?Z`).ReplaceAllString(string(b), "<time>")
	ids := map[string]string{}
	return regexp.MustCompile(`[a-f0-9]{32}`).ReplaceAllStringFunc(text, func(id string) string {
		if ids[id] == "" {
			ids[id] = fmt.Sprintf("<id%d>", len(ids))
		}
		return ids[id]
	})
}
func TestHotWorkerScopedEquivalent(t *testing.T) {
	for _, backend := range []string{"memory", "postgres"} {
		for _, mode := range []string{"git-tick", "git", "git-released", "git-failure", "git-deleted", "repository", "repository-duplicate", "finish", "save-delivery", "retry", "deadline", "advance", "lease-lost", "runtime"} {
			t.Run(backend+"/"+mode, func(t *testing.T) {
				_, fixture, provider := externalFixture(t)
				seed := cloneWorkerState(t, fixture.state)
				seed.IntegrationRevisions = []domain.IntegrationRevision{}
				in := integration(&seed, "i")
				in.Branches = []domain.Branch{{RepositoryID: "source", Name: "feature/work"}}
				in.PullRequests = []domain.PullRequest{{RepositoryID: "source", ID: "7", URL: "https://github.com/owner/source/pull/7", Status: "open"}}
				if mode == "git-released" {
					in.Status = "released"
				}
				op := domain.ExternalOperation{Meta: domain.Meta{ID: "scope-op", ProductID: "p", FeatureID: "f", CreatedAt: time.Now().UTC()}, Kind: "REFRESH_GIT", IntegrationID: "i", RepositoryID: "source", Phase: "OBSERVE", Status: "RUNNING", LeaseOwner: "lease", LeaseUntil: time.Now().Add(time.Hour)}
				if strings.HasPrefix(mode, "repository") {
					op.Kind = "REFRESH_REPOSITORY_GIT"
				}
				if mode == "deadline" {
					op.CreatedAt = time.Now().Add(-time.Hour)
				}
				if mode == "advance" {
					// Match the worker's deployment workflow dispatch transition.
					op.Kind = "DEPLOY"
					op.Phase = "DISPATCH"
					op.EnvironmentID = "dev"
					op.ReservationEpoch = environmentByID(&seed, "dev").ReservationEpoch
				}
				if mode == "runtime" {
					op.Status = "SUCCEEDED"
					op.EnvironmentID = "dev"
					op.ApplicationID = "app"
					op.GitOpsResult = &delivery.GitOpsResult{CommitSHA: "commit"}
					op.Artifact = &delivery.ArtifactResult{Digest: "digest"}
				}
				if mode == "save-delivery" {
					op.Snapshot = &domain.DeliverySnapshot{}
					op.Run = &delivery.RunResult{}
					op.Artifact = &delivery.ArtifactResult{Digest: "digest"}
				}
				if mode == "git-tick" {
					op.Status = "PENDING"
					op.LeaseOwner = ""
					op.LeaseUntil = time.Time{}
				}
				seed.Operations = []domain.ExternalOperation{op}
				results := []string{}
				for _, scoped := range []bool{false, true} {
					var base Store = &memoryStore{state: cloneWorkerState(t, seed)}
					if backend == "postgres" {
						db, _ := openWorkerTestStore(t, "postgres")
						defer db.Close()
						if err := db.Update(context.Background(), func(st *domain.State) error { *st = cloneWorkerState(t, seed); return nil }); err != nil {
							t.Fatal(err)
						}
						base = db
					}
					var store Store = fullWorkerStore{base}
					enforce := &enforcingWorkerStore{Store: base, t: t}
					if scoped {
						store = enforce
					}
					s := NewWithProvider(store, &scopedGitFake{fakeDelivery: provider, deletedBranch: mode == "git-deleted"})
					var err error
					switch mode {
					case "git-tick":
						_, err = s.Tick(context.Background())
					case "git-failure":
						s.provider = &fakePRObserver{fakeDelivery: provider, fail: true}
						err = s.observeGit(context.Background(), op, "lease")
					case "repository", "repository-duplicate":
						s.provider = &inventoryGitFake{fakeDelivery: provider}
						err = s.observeRepositoryGit(context.Background(), op, "lease")
						if mode == "repository-duplicate" && err == nil {
							// Restore only the lease, retaining the inventory for duplicate detection.
							err = base.Update(context.Background(), func(st *domain.State) error { st.Operations[0] = op; return nil })
							if err == nil {
								err = s.observeRepositoryGit(context.Background(), op, "lease")
							}
						}
					case "finish", "save-delivery":
						err = s.finish(context.Background(), op, "lease", "FAILED", "provider error", nil)
					case "retry", "deadline":
						err = s.retry(context.Background(), op, "lease", "retry")
					case "advance":
						err = s.advance(context.Background(), op, "lease", "BUILD_LOOKUP", "BUILDING", "workflow dispatched", nil)
					case "lease-lost":
						err = s.finish(context.Background(), op, "wrong", "SUCCEEDED", "done", nil)
					case "runtime":
						err = s.ObserveRuntimeOnce(context.Background(), []delivery.RuntimeObserver{&fakeRuntimeObserver{target: delivery.RuntimeTarget{ProductID: "p", EnvironmentID: "dev", ApplicationID: "app"}, evidence: delivery.RuntimeEvidence{Healthy: true, Details: "healthy"}}})
					default:
						err = s.observeGit(context.Background(), op, "lease")
					}
					if mode == "lease-lost" {
						if err == nil {
							t.Fatal("expected lease error")
						}
					} else if err != nil {
						t.Fatal(err)
					}
					if scoped && len(enforce.scopes) == 0 {
						t.Fatal("no scoped writes")
					}
					st, err := base.Read(context.Background())
					if err != nil {
						t.Fatal(err)
					}
					if mode == "git-tick" && (st.Operations[0].Status != "SUCCEEDED" || len(st.GitObservations) == 0 || len(st.IntegrationRevisions) == 0) {
						t.Fatal("Tick did not persist the successful Git observation and revision")
					}
					if mode == "retry" || mode == "advance" {
						next := st.Operations[0].NextAttemptAt.Sub(st.Operations[0].UpdatedAt)
						want := 2 * time.Second
						if mode == "advance" {
							want = 10 * time.Second
						}
						if next != want {
							t.Fatalf("backoff = %v, want %v", next, want)
						}
						if st.Operations[0].LeaseOwner != "" {
							t.Fatal("lease not released")
						}
					}
					results = append(results, normalizedWorkerState(t, st))
				}
				if results[0] != results[1] {
					t.Fatalf("full/scoped state mismatch\nfull: %s\nscoped: %s", results[0], results[1])
				}
			})
		}
	}
}
