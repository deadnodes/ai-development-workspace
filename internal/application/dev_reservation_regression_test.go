package application

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
)

// Each repository has its own main/ref graph; sharing one fake main would hide
// the sequential composition/advance behavior this regression protects.
type reservationMultiRepoProvider struct {
	*fakeFlow
	second                     *fakeFlow
	onCompose                  func()
	composeCalls, advanceCalls map[string]int
	failCompose, failAdvance   bool
}

func (p *reservationMultiRepoProvider) flow(repo string) *fakeFlow {
	if repo == "44" || repo == "owner/second" {
		return p.second
	}
	return p.fakeFlow
}
func (p *reservationMultiRepoProvider) DiscoverRepositories(context.Context, delivery.Connection) ([]delivery.RepositoryInfo, error) {
	return []delivery.RepositoryInfo{{ID: 42, FullName: "owner/source", DefaultBranch: "main"}, {ID: 43, FullName: "owner/gitops", DefaultBranch: "main"}, {ID: 44, FullName: "owner/second", DefaultBranch: "main"}}, nil
}
func (p *reservationMultiRepoProvider) Branches(ctx context.Context, c delivery.Connection, repo string) ([]delivery.BranchInfo, error) {
	return p.flow(repo).Branches(ctx, c, repo)
}
func (p *reservationMultiRepoProvider) Compare(ctx context.Context, c delivery.Connection, repo, base, head string) (delivery.CompareResult, error) {
	return p.flow(repo).Compare(ctx, c, repo, base, head)
}
func (p *reservationMultiRepoProvider) ComposeSource(ctx context.Context, c delivery.Connection, plan delivery.SourcePlan) (delivery.SourceResult, error) {
	p.composeCalls[plan.Repository]++
	result, err := p.flow(plan.Repository).ComposeSource(ctx, c, plan)
	if p.onCompose != nil {
		hook := p.onCompose
		p.onCompose = nil
		hook()
	}
	if p.failCompose {
		return result, fmt.Errorf("composition response lost")
	}
	return result, err
}
func (p *reservationMultiRepoProvider) AdvanceMain(ctx context.Context, c delivery.Connection, repo, ref, base, candidate string) (delivery.SourceResult, error) {
	p.advanceCalls[repo]++
	result, err := p.flow(repo).AdvanceMain(ctx, c, repo, ref, base, candidate)
	if p.failAdvance {
		return result, fmt.Errorf("advance response lost")
	}
	return result, err
}
func (p *reservationMultiRepoProvider) ObserveBranch(_ context.Context, _ delivery.Connection, repo, branch string) (delivery.BranchInfo, error) {
	flow := p.flow(repo)
	sha := flow.branches[branch]
	if branch == "main" {
		sha = flow.main
	}
	if sha == "" {
		return delivery.BranchInfo{}, fmt.Errorf("branch absent")
	}
	return delivery.BranchInfo{Name: branch, SHA: sha}, nil
}
func (p *reservationMultiRepoProvider) VerifyComposedSource(ctx context.Context, c delivery.Connection, plan delivery.SourcePlan, result delivery.SourceResult) error {
	return p.flow(plan.Repository).VerifyComposedSource(ctx, c, plan, result)
}

func reservationMultiRepoFixture(t *testing.T, kind string) (*Service, workerTestStore, *reservationMultiRepoProvider, string) {
	t.Helper()
	_, seed, first := flowFixture(t)
	secondHead := strings.Repeat("e", 40)
	second := &fakeFlow{main: shaBase, branches: map[string]string{"feature/second": secondHead}, parents: map[string][]string{secondHead: {shaBase}}, deployments: map[string]string{}, applyCounts: map[string]int{}}
	p := &reservationMultiRepoProvider{fakeFlow: first, second: second, composeCalls: map[string]int{}, advanceCalls: map[string]int{}}
	store, _ := openWorkerTestStore(t, kind)
	t.Cleanup(func() { store.Close() })
	if err := store.Update(context.Background(), func(st *domain.State) error { *st = seed.state; return nil }); err != nil {
		t.Fatal(err)
	}
	s := NewWithProvider(store, p)
	if _, err := s.Query(context.Background(), "discover_repositories", "conn"); err != nil {
		t.Fatal(err)
	}
	exec(t, s, domain.Command{Action: "import_repository", ID: "second", ProductID: "p", Data: map[string]any{"connection_id": "conn", "full_name": "owner/second", "role": "APPLICATION", "default_branch": "main"}})
	exec(t, s, domain.Command{Action: "create_application", ID: "second-component", ProductID: "p", Data: map[string]any{"name": "Second", "repository_id": "second"}})
	exec(t, s, domain.Command{Action: "configure_component", ProductID: "p", Data: map[string]any{"application_id": "second-component", "connection_id": "conn", "workflow": "build.yml", "rebuild_missing": true, "image_repository": "ghcr.io/owner/component"}})
	flowEnvironment(t, s, "prod", "PROD")
	exec(t, s, domain.Command{Action: "configure_environment", ProductID: "p", Data: map[string]any{"environment_id": "prod", "application_id": "second-component", "purpose": "PROD", "connection_id": "conn", "repository_id": "gitops", "ref": "main", "path": "second-prod.yaml", "image_field": "spec.values.image.repository", "digest_field": "spec.values.image.tag", "allow_deploy": true}})
	exec(t, s, domain.Command{Action: "create_integration", ID: "second-integration", FeatureID: "f", Data: map[string]any{"title": "Second", "objective": "Second", "repositories": []string{"second"}}})
	exec(t, s, domain.Command{Action: "record_integration_revision", ID: "second-revision", IntegrationID: "second-integration", Data: map[string]any{"repository_id": "second", "branch": "feature/second", "base_commit": shaBase, "head_commit": secondHead, "commits": []string{secondHead}}})
	for _, iid := range []string{"i", "second-integration"} {
		exec(t, s, domain.Command{Action: "start_integration", IntegrationID: iid})
		exec(t, s, domain.Command{Action: "complete_integration", IntegrationID: iid})
	}
	value := exec(t, s, domain.Command{Action: "prepare_release_candidate", ID: "multi-candidate", ProductID: "p", Data: map[string]any{"name": "Two source repositories", "environment_id": "prod", "components": []domain.CompositionComponent{component("component", "rev"), component("second-component", "second-revision")}, "approve_main_update": true}})
	candidate := value.(domain.ExternalOperation)
	if len(candidate.Sources) != 2 {
		t.Fatal("candidate must contain two independent source repositories")
	}
	return s, store, p, candidate.ID
}
func reservationMultiTick(t *testing.T, s *Service, store workerTestStore) (bool, error) {
	t.Helper()
	if err := store.Update(context.Background(), func(st *domain.State) error {
		for i := range st.Operations {
			st.Operations[i].NextAttemptAt = time.Time{}
			st.Operations[i].LeaseUntil = time.Time{}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return s.Tick(context.Background())
}
func TestDevReservationLegacyTwoSourceMutationAcknowledgements(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		for _, interrupted := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/interrupted-%v", kind, interrupted), func(t *testing.T) {
				ctx := context.Background()
				s, store, p, parentID := reservationMultiRepoFixture(t, kind)
				if interrupted {
					p.onCompose = func() {
						if err := store.Update(ctx, func(st *domain.State) error { operationByID(st, parentID).LeaseOwner = "interrupted-owner"; return nil }); err != nil {
							t.Fatal(err)
						}
					}
				}
				firstTick := true
				for range 10 {
					worked, err := reservationMultiTick(t, s, store)
					if firstTick && interrupted {
						firstTick = false
						if err == nil {
							t.Fatal("lost admission lease should preserve unknown source intent")
						}
						st, _ := store.Read(ctx)
						parent := operationByID(&st, parentID)
						if parent.ReservationMutationIntent != "SOURCE" || parent.Sources[0].Result != nil {
							t.Fatal("unacknowledged mutation lost its intent")
						}
						continue
					}
					firstTick = false
					if err != nil || !worked {
						t.Fatalf("sequential multi-source tick worked=%v err=%v", worked, err)
					}
					st, _ := store.Read(ctx)
					parent := operationByID(&st, parentID)
					if parent.Status == "FAILED" {
						t.Fatalf("candidate failed: %s", parent.Detail)
					}
					if parent.ReservationMutationIntent != "" {
						t.Fatalf("confirmed individual source mutation retained intent in phase %s", parent.Phase)
					}
					if parent.Phase == "QUEUE_BUILDS" {
						for _, source := range parent.Sources {
							if source.Result == nil || !source.MainAdvanced {
								t.Fatal("each source must be composed and main-advanced")
							}
							if p.composeCalls[source.Plan.Repository] != 1 || p.advanceCalls[source.Plan.Repository] != 1 {
								t.Fatalf("source mutation repeated/missing: compose=%v advance=%v", p.composeCalls, p.advanceCalls)
							}
						}
						t.Logf("%s two ComposeSource + two AdvanceMain acknowledgements; interrupted=%v, no repeated write", kind, interrupted)
						return
					}
				}
				t.Fatal("two-source candidate did not reach QUEUE_BUILDS")
			})
		}
	}
}

func TestDevReservationLegacyUnknownSourceOutcomeRetainsIntent(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		for _, stage := range []string{"compose", "advance"} {
			t.Run(kind+"/"+stage, func(t *testing.T) {
				ctx := context.Background()
				s, store, p, parentID := reservationMultiRepoFixture(t, kind)
				if stage == "compose" {
					p.failCompose = true
				} else {
					p.failAdvance = true
				}
				for range 8 {
					worked, err := reservationMultiTick(t, s, store)
					if err != nil || !worked {
						t.Fatalf("unknown-outcome tick worked=%v err=%v", worked, err)
					}
					st, _ := store.Read(ctx)
					parent := operationByID(&st, parentID)
					if parent.Status == "FAILED" {
						if parent.ReservationMutationIntent != "SOURCE" || unresolvedReservationEffects(&st, "prod") == nil {
							t.Fatal("unknown source outcome lost reservation fence")
						}
						beforeCompose, beforeAdvance := fmt.Sprint(p.composeCalls), fmt.Sprint(p.advanceCalls)
						worked, err = reservationMultiTick(t, s, store)
						if err != nil || worked {
							t.Fatal("terminal unknown source outcome restarted")
						}
						if beforeCompose != fmt.Sprint(p.composeCalls) || beforeAdvance != fmt.Sprint(p.advanceCalls) {
							t.Fatal("unknown write repeated")
						}
						return
					}
				}
				t.Fatal("unknown outcome fixture did not stop")
			})
		}
	}
}
