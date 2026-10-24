package application

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
)

type mainCandidateProvider struct {
	fakeFlow
	observation delivery.PullRequestObservation
	onObserve   func()
}

func (p *mainCandidateProvider) ObservePullRequest(context.Context, delivery.Connection, string, int) (delivery.PullRequestObservation, error) {
	if p.onObserve != nil {
		p.onObserve()
	}
	return p.observation, nil
}
func mainCandidateFixture(t *testing.T, kind string) (*nativeFixture, *mainCandidateProvider, domain.Command) {
	t.Helper()
	f := automaticFixture(t, kind)
	p := &mainCandidateProvider{fakeFlow: fakeFlow{main: shaHead, parents: map[string][]string{shaHead: {shaBase}}}, observation: delivery.PullRequestObservation{Number: 17, State: "merged", BaseRepositoryID: 101, HeadRepositoryID: 101, Base: "main", Head: "feature/work", HeadSHA: shaHead, MergeSHA: shaHead, MergedAt: time.Now().UTC().Truncate(time.Second)}}
	f.s.provider = p
	if err := f.store.Update(context.Background(), func(st *domain.State) error {
		st.Environments = append(st.Environments, domain.Environment{Meta: domain.Meta{ID: "prod", ProductID: "p"}, Cluster: "fixture", Namespace: "prod"})
		target := *environmentBinding(st, "dev", "component")
		target.ID = "prod-target"
		target.EnvironmentID = "prod"
		target.Purpose = "PROD"
		target.GitOpsMode = "PR"
		st.EnvironmentBindings = append(st.EnvironmentBindings, target)
		in := integration(st, "i")
		in.Status = "ready"
		rev := revisionByID(st, "rev")
		rev.FeatureID = in.FeatureID
		rev.Branch = "feature/work"
		for i := range st.Repositories {
			if st.Repositories[i].ID == "source" {
				for j := range st.RegisteredRepositories {
					if st.RegisteredRepositories[j].ID == st.Repositories[i].RegisteredRepositoryID {
						st.RegisteredRepositories[j].ProviderRepositoryID = 101
					}
				}
			}
		}
		g := domain.Gate{Meta: domain.Meta{ID: "source-gate", ProductID: "p", FeatureID: in.FeatureID}, Title: "source", Result: "pending", Blocking: true, IntegrationIDs: []string{"i"}}
		st.Gates = append(st.Gates, g)
		st.Checks = append(st.Checks, domain.Check{Meta: domain.Meta{ID: "source-check", ProductID: "p", FeatureID: in.FeatureID}, GateID: g.ID, Title: "test", Mechanism: "test", Instructions: "run"})
		st.Results = append(st.Results, domain.CheckResult{Meta: domain.Meta{ID: "source-result", ProductID: "p", FeatureID: in.FeatureID, CreatedAt: time.Now().UTC()}, CheckID: "source-check", Commit: shaHead, Result: "passed"})
		in.Commits = []domain.Commit{{RepositoryID: "source", SHA: shaHead}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	in := domain.MainCandidateRequest{Name: "selected main A", EnvironmentID: "prod", IdempotencyKey: "main-candidate-17", MainSources: []domain.MainCandidateSource{{ApplicationID: "component", MainSHA: shaHead, Native: domain.NativeBinding{BuildRecipeID: "build", PublicationRecipeID: "publish", Parameters: map[string]any{}}, Revisions: []domain.MainCandidateRevision{{RevisionID: "rev", PullRequestNumber: 17, VerificationResultIDs: []string{"source-result"}}}}}}
	b, _ := json.Marshal(in)
	data := map[string]any{}
	_ = json.Unmarshal(b, &data)
	return f, p, domain.Command{Action: "prepare_release_candidate", Actor: "operator", ProductID: "p", Data: data}
}
func TestMainCandidateCausalAuthorityAndDrift(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			f, p, c := mainCandidateFixture(t, kind)
			ctx := context.Background()
			before, _ := f.store.Read(ctx)
			v, err := f.s.Execute(ctx, c)
			if err != nil {
				t.Fatal(err)
			}
			op := v.(domain.ExternalOperation)
			if op.Kind != "RELEASE_CANDIDATE" || op.Phase != "QUEUE_BUILDS" || op.MainCandidate == nil || op.MainCandidate.EvidenceKind != "ATTRIBUTED_RCP_VERIFICATION" || op.Sources != nil {
				t.Fatal("candidate did not freeze build-only source")
			}
			st, _ := f.store.Read(ctx)
			if !reflect.DeepEqual(*componentBuild(&before, "component"), *componentBuild(&st, "component")) || !reflect.DeepEqual(before.Environments, st.Environments) {
				t.Fatal("candidate mutated global/desired configuration")
			}
			// No branch still exists, current main advances beyond A. Historical pin stays A.
			newer := strings.Repeat("e", 40)
			p.main = newer
			p.parents[newer] = []string{shaHead}
			if err = f.s.observeMainCandidate(ctx, &op, false); err != nil {
				t.Fatal(err)
			}
			f.store.Close()
			f.store = f.reopen()
			f.s.store = f.store
			replay, err := f.s.Execute(ctx, c)
			if err != nil || replay.(domain.ExternalOperation).ID != op.ID {
				t.Fatal("durable replay", err)
			}
			for _, result := range []string{"failed", "blocked", "passed"} {
				t.Run("later-old-clock-"+result, func(t *testing.T) {
					copy := st
					raw, _ := json.Marshal(st)
					_ = json.Unmarshal(raw, &copy)
					later := copy.Results[len(copy.Results)-1]
					later.ID = "late-" + result
					later.Result = result
					later.CreatedAt = time.Unix(1, 0)
					copy.Results = append(copy.Results, later)
					if err := f.s.mainCandidateAllowed(&copy, &op); err == nil {
						t.Fatal("accepted later causal result with older clock")
					}
				})
			}
			for _, change := range []string{"new-check", "definition", "scope", "integration", "baseline", "parent", "target", "connection", "result-payload", "legacy-phase"} {
				t.Run(change, func(t *testing.T) {
					var copy domain.State
					raw, _ := json.Marshal(st)
					_ = json.Unmarshal(raw, &copy)
					candidate := operationByID(&copy, op.ID)
					switch change {
					case "legacy-phase":
						candidate.Phase = "ADVANCE_MAIN"
					case "new-check":
						copy.Checks = append(copy.Checks, domain.Check{Meta: domain.Meta{ID: "new", ProductID: "p", FeatureID: copy.Checks[0].FeatureID}, GateID: "source-gate"})
					case "definition":
						check(&copy, "source-check").Instructions = "different"
					case "scope":
						gate(&copy, "source-gate").IntegrationIDs = nil
					case "integration":
						integration(&copy, "i").Status = "working"
					case "baseline":
						componentBuild(&copy, "component").Profile = "changed"
					case "parent":
						candidate.PreparedDeployments[0].MainCandidate.AuthorityHash = "changed"
					case "target":
						environmentBinding(&copy, "prod", "component").Purpose = "DEV"
					case "connection":
						connection(&copy, "conn").Config.AppID++
					case "result-payload":
						copy.Results[len(copy.Results)-1].Commit = shaBase
					}
					if err := f.s.mainCandidateAllowed(&copy, candidate); err == nil {
						t.Fatal("drift accepted")
					}
				})
			}
			p.observation.BaseRepositoryID = 999
			if err = f.s.observeMainCandidate(ctx, &op, false); err == nil {
				t.Fatal("foreign physical PR accepted")
			}
		})
	}
}
func TestMainCandidateMalformedAndMergedProof(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			for _, change := range []string{"mixed", "approval", "empty", "duplicate", "case-alias", "partial", "foreign-revision", "not-merged", "zero-time", "wrong-head", "wrong-base", "bad-merge", "wrong-selected", "force-main"} {
				t.Run(change, func(t *testing.T) {
					f, p, c := mainCandidateFixture(t, kind)
					switch change {
					case "case-alias":
						v := c.Data["main_sources"].([]any)[0].(map[string]any)
						v["Main_SHA"] = v["main_sha"]
						delete(v, "main_sha")
					case "mixed":
						c.Data["components"] = []any{}
					case "approval":
						c.Data["approve_main_update"] = true
					case "empty":
						c.Data["main_sources"] = []any{}
					case "duplicate":
						v := c.Data["main_sources"].([]any)
						c.Data["main_sources"] = append(v, v[0])
					case "partial":
						v := c.Data["main_sources"].([]any)[0].(map[string]any)
						v["revisions"].([]any)[0].(map[string]any)["verification_result_ids"] = []any{}
					case "foreign-revision":
						v := c.Data["main_sources"].([]any)[0].(map[string]any)
						v["revisions"].([]any)[0].(map[string]any)["revision_id"] = "missing"
					case "not-merged":
						p.observation.State = "closed"
					case "zero-time":
						p.observation.MergedAt = time.Time{}
					case "wrong-head":
						p.observation.HeadSHA = shaBase
					case "wrong-base":
						p.observation.Base = "dev"
					case "bad-merge":
						p.observation.MergeSHA = "short"
					case "wrong-selected":
						p.observation.MergeSHA = shaBase
					case "force-main":
						p.main = strings.Repeat("f", 40)
					}
					if _, err := f.s.Execute(context.Background(), c); err == nil {
						t.Fatal("invalid candidate accepted")
					}
					st, _ := f.store.Read(context.Background())
					if len(st.Operations) != 0 || len(st.WorkerJobs) != 0 || len(p.plans) != 0 || p.advances != 0 {
						t.Fatal("invalid candidate caused execution")
					}
				})
			}
		})
	}
}

func TestMainCandidateManagedWorkerFences(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			f, p, c := mainCandidateFixture(t, kind)
			ctx := context.Background()
			v, err := f.s.Execute(ctx, c)
			if err != nil {
				t.Fatal(err)
			}
			parent := v.(domain.ExternalOperation)
			for range 3 {
				nativeTick(t, f)
			}
			st, _ := f.store.Read(ctx)
			var job *domain.WorkerJob
			for i := range st.WorkerJobs {
				if st.WorkerJobs[i].Inputs.SourceProof != nil {
					job = &st.WorkerJobs[i]
				}
			}
			if job == nil || job.Capability != "local_oci_build" || job.Inputs.SourceSHA != shaHead {
				t.Fatal("candidate did not queue exact native build")
			}
			child := operationByID(&st, job.Inputs.SourceProof.DeliveryID)
			if !child.BuildOnly || child.Native.ParentHash != nativeParentHash(operationByID(&st, parent.ID)) || child.Snapshot.BuildBaseline == nil || child.Snapshot.MainCandidate == nil {
				t.Fatal("missing frozen build-only parent proof")
			}
			for _, phase := range []string{"PREFLIGHT", "PROMOTION_PREFLIGHT", "APPLY_UNCERTAIN", "DISPATCH"} {
				mutated := *child
				mutated.Phase = phase
				if err := f.s.nativeMainChildAllowed(&st, &mutated); err == nil {
					t.Fatal("candidate accepted external-writing phase", phase)
				}
			}
			bp, _ := f.s.externalWorkers.Authenticate(nativeBuildToken, time.Now())
			lease, err := f.s.ClaimWorker(ctx, bp, domain.WorkerClaim{ProductID: "p", RepositoryID: "source", Capability: "local_oci_build"})
			if err != nil || lease == nil {
				t.Fatal("claim", err)
			}
			req := workerRequest(lease)
			if _, err = f.s.StartWorker(ctx, bp, req); err != nil {
				t.Fatal(err)
			}
			if _, err = f.s.HeartbeatWorker(ctx, bp, req); err != nil {
				t.Fatal(err)
			}
			// Later causal failure invalidates already-running execution despite older clock.
			if err = f.store.Update(ctx, func(st *domain.State) error {
				r := st.Results[len(st.Results)-1]
				r.ID = "late-failed"
				r.CreatedAt = time.Unix(1, 0)
				r.Result = "failed"
				st.Results = append(st.Results, r)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if _, err = f.s.HeartbeatWorker(ctx, bp, req); err == nil {
				t.Fatal("stale running worker kept authority")
			}
			if _, err = f.s.ReportWorker(ctx, bp, domain.WorkerReport{WorkerAttemptRequest: req, Result: "FAILED"}); err == nil {
				t.Fatal("stale worker report accepted")
			}
			if len(p.plans) != 0 || p.advances != 0 || p.dispatches != 0 || p.applies != 0 {
				t.Fatal("native candidate wrote refs, dispatched Actions or applied GitOps")
			}
		})
	}
}

func TestMainCandidateSelectedSourceAndOtherBlockingGates(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			f, p, c := mainCandidateFixture(t, kind)
			ctx := context.Background()
			mainSHA := strings.Repeat("e", 40)
			p.main = mainSHA
			p.parents[mainSHA] = []string{shaHead}
			p.observation.MergeSHA = mainSHA
			c.Data["main_sources"].([]any)[0].(map[string]any)["main_sha"] = mainSHA
			if err := f.store.Update(ctx, func(st *domain.State) error {
				in := integration(st, "i")
				in.Commits = append(in.Commits, domain.Commit{RepositoryID: "source", SHA: mainSHA})
				g := domain.Gate{Meta: domain.Meta{ID: "dev-gate", ProductID: "p", FeatureID: in.FeatureID}, Title: "Dev main acceptance", Result: "pending", Blocking: true, IntegrationIDs: []string{"i"}, Commit: mainSHA, EnvironmentID: "dev", Deployment: "dev-image"}
				st.Gates = append(st.Gates, g)
				st.Checks = append(st.Checks, domain.Check{Meta: domain.Meta{ID: "dev-check", ProductID: "p", FeatureID: in.FeatureID}, GateID: g.ID, Title: "runtime", Mechanism: "fixture", Instructions: "verify"})
				older := domain.CheckResult{Meta: domain.Meta{ID: "dev-old-fail", ProductID: "p", FeatureID: in.FeatureID, CreatedAt: time.Now().UTC().Add(time.Hour)}, CheckID: "dev-check", Commit: mainSHA, EnvironmentID: "dev", Deployment: "dev-image", Result: "failed"}
				st.Results = append(st.Results, older)
				current := older
				current.ID = "dev-current-pass"
				current.Result = "passed"
				current.CreatedAt = time.Unix(1, 0)
				st.Results = append(st.Results, current)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			v, err := f.s.Execute(ctx, c)
			if err != nil {
				t.Fatal("distinct source/Dev gates or causal latest rejected", err)
			}
			op := v.(domain.ExternalOperation)
			if err = f.store.Update(ctx, func(st *domain.State) error {
				r := st.Results[len(st.Results)-1]
				r.ID = "dev-later-pass"
				r.CreatedAt = time.Unix(0, 0)
				st.Results = append(st.Results, r)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			st, _ := f.store.Read(ctx)
			if err = f.s.mainCandidateAllowed(&st, &op); err == nil {
				t.Fatal("unselected blocking gate causal proof changed without invalidation")
			}
		})
	}
}

func TestMainCandidateCancellationRestoreAndReplay(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			f, p, c := mainCandidateFixture(t, kind)
			ctx := context.Background()
			v, err := f.s.Execute(ctx, c)
			if err != nil {
				t.Fatal(err)
			}
			op := v.(domain.ExternalOperation)
			for range 3 {
				nativeTick(t, f)
			}
			bp, _ := f.s.externalWorkers.Authenticate(nativeBuildToken, time.Now())
			lease, err := f.s.ClaimWorker(ctx, bp, domain.WorkerClaim{ProductID: "p", RepositoryID: "source", Capability: "local_oci_build"})
			if err != nil || lease == nil {
				t.Fatal(err)
			}
			req := workerRequest(lease)
			if _, err = f.s.StartWorker(ctx, bp, req); err != nil {
				t.Fatal(err)
			}
			backup, err := f.s.CreateBackup(ctx, "operator")
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.s.Execute(ctx, domain.Command{Action: "cancel_native_delivery", ProductID: "p", ID: op.ID, Actor: "operator", Data: map[string]any{}}); err != nil {
				t.Fatal(err)
			}
			st, _ := f.store.Read(ctx)
			if operationByID(&st, op.ID).Status != "CANCELLED" || workerJob(&st, lease.Job.ID).Status != "RECONCILIATION_REQUIRED" {
				t.Fatal("candidate cancellation lost uncertain physical execution")
			}
			if _, err = f.s.HeartbeatWorker(ctx, bp, req); err == nil {
				t.Fatal("cancelled candidate heartbeat accepted")
			}
			if replay, err := f.s.Execute(ctx, c); err != nil || replay.(domain.ExternalOperation).ID != op.ID || replay.(domain.ExternalOperation).Status != "CANCELLED" {
				t.Fatal("cancelled replay", err)
			}
			bad := c
			bad.Actor = "different"
			if _, err = f.s.Execute(ctx, bad); err == nil {
				t.Fatal("different actor reused candidate key")
			}
			restored, _ := openWorkerTestStore(t, kind)
			defer restored.Close()
			copy := NewWithProvider(restored, p)
			copy.ConfigureWorkers(f.s.externalWorkers)
			if _, err = copy.RestoreBackup(ctx, "operator", backup.ArchiveBase64, backup.SHA256); err != nil {
				t.Fatal(err)
			}
			if replay, err := copy.Execute(ctx, c); err != nil || replay.(domain.ExternalOperation).ID != op.ID || replay.(domain.ExternalOperation).Status != "CANCELLED" {
				t.Fatal("restored candidate historical replay", err)
			}
			if lease, err := copy.ClaimWorker(ctx, bp, domain.WorkerClaim{ProductID: "p", RepositoryID: "source", Capability: "local_oci_build"}); err != nil || lease != nil {
				t.Fatal("restore started native execution", err)
			}
			raw, _ := json.Marshal(c.Data)
			var fresh map[string]any
			_ = json.Unmarshal(raw, &fresh)
			fresh["idempotency_key"] = "fresh-after-restore"
			bad = c
			bad.Data = fresh
			if _, err = copy.Execute(ctx, bad); err == nil {
				t.Fatal("restore granted new candidate execution")
			}
		})
	}
}
