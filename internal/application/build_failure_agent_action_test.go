package application

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"releasecontrol/internal/domain"
	"releasecontrol/internal/workerconfig"
)

// Source-provider and accepted worker reports are controlled peers. All domain
// creation, job admission and failure propagation use the normal service path.
func buildActionFixture(t *testing.T, kind, result string, components int, baseOnly ...bool) (*nativeFixture, string) {
	t.Helper()
	f := automaticFixture(t, kind)
	t.Cleanup(func() { f.store.Close() })
	headB := strings.Repeat("e", 40)
	provider := &fakeFlow{main: shaBase, branches: map[string]string{"feature/work": shaHead, "feature/b": headB}, parents: map[string][]string{shaHead: {shaBase}, headB: {shaBase}}, deployments: map[string]string{}, applyCounts: map[string]int{}}
	f.s.provider = provider
	exec(t, f.s, domain.Command{Action: "create_feature", ID: "build-action-fb", ProductID: "p", Data: map[string]any{"title": "B", "goal": "Preserve B"}})
	exec(t, f.s, domain.Command{Action: "create_integration", ID: "build-action-ib", FeatureID: "build-action-fb", Data: map[string]any{"title": "B", "objective": "Keep B"}})
	exec(t, f.s, domain.Command{Action: "record_integration_revision", ID: "build-action-rb", IntegrationID: "build-action-ib", Data: map[string]any{"repository_id": "source", "branch": "feature/b", "base_commit": shaBase, "head_commit": headB, "commits": []string{headB}}})
	revisions := []string{"rev", "build-action-rb"}
	if len(baseOnly) > 0 && baseOnly[0] {
		revisions = nil
	}
	selected := []domain.CompositionComponent{component("component", revisions...)}
	if components == 2 {
		recipe, _ := f.s.externalWorkers.Recipe("build")
		pub := f.recipe
		pub.ID, pub.ComponentID, pub.Repository = "publish-second", "second", "fixture/second"
		pub.Hash = workerconfig.PublicationHash(pub)
		authority, err := workerconfig.New(workerconfig.Config{Revision: "cfg1", LeaseSeconds: 5, Recipes: []workerconfig.Recipe{recipe}, Publications: []workerconfig.PublicationRecipe{f.recipe, pub}, Credentials: []workerconfig.Credential{{ID: "build", Principal: "worker/build", TokenEnv: "NATIVE_BUILD_TOKEN", ProductIDs: []string{"p"}, RepositoryIDs: []string{"source"}, Capabilities: []string{"local_oci_build"}, ExpiresAt: time.Now().Add(time.Hour)}}}, "native-control-fixture", os.Getenv)
		if err != nil {
			t.Fatal(err)
		}
		f.s.ConfigureWorkers(authority)
		exec(t, f.s, domain.Command{Action: "create_application", ID: "second", ProductID: "p", Data: map[string]any{"name": "Second component", "repository_id": "source"}})
		exec(t, f.s, domain.Command{Action: "configure_component", ProductID: "p", Data: map[string]any{"application_id": "second", "connection_id": "conn", "mode": "NATIVE", "profile": pub.Profile, "image_repository": nativeImage(pub), "native": map[string]any{"build_recipe_id": "build", "publication_recipe_id": pub.ID, "parameters": map[string]any{}}}})
		st, _ := f.store.Read(context.Background())
		target := environmentBinding(&st, "dev", "component")
		exec(t, f.s, domain.Command{Action: "configure_environment", ProductID: "p", Data: map[string]any{"environment_id": "dev", "application_id": "second", "connection_id": target.ConnectionID, "repository_id": target.RepositoryID, "purpose": target.Purpose, "ref": target.Ref, "path": "second.yaml", "image_field": target.ImageField, "digest_field": target.DigestField, "allow_deploy": true, "gitops_mode": "PR"}})
		selected = append(selected, component("second", revisions...))
	}
	exec(t, f.s, domain.Command{Action: "plan_composition", ID: "build-action-comp", ProductID: "p", Data: map[string]any{"name": "A+B", "environment_id": "dev", "components": selected}})
	parent := exec(t, f.s, domain.Command{Action: "reconcile_composition", ID: "build-action-comp", ProductID: "p", Data: map[string]any{}}).(domain.ExternalOperation)
	ctx := context.Background()
	for range 40 {
		nativeTick(t, f)
		st, _ := f.store.Read(ctx)
		if len(st.WorkerJobs) == components {
			break
		}
	}
	st, _ := f.store.Read(ctx)
	if len(st.WorkerJobs) != components || len(st.CompositionConflicts) != 0 || operationByID(&st, parent.ID).Sources[0].Result.Conflict {
		t.Fatal("normal clean native composition was not queued")
	}
	p, err := f.s.externalWorkers.Authenticate(nativeBuildToken, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	capability := "local_oci_build"
	if result == "PUBLICATION_FAILED" {
		automaticBuild(t, f)
		for range 20 {
			nativeTick(t, f)
			if len(buildActionState(t, f).WorkerJobs) == 2 {
				break
			}
		}
		p, capability = f.p, "registry_publish"
	}
	for range components {
		lease, err := f.s.ClaimWorker(ctx, p, domain.WorkerClaim{ProductID: "p", RepositoryID: "source", Capability: capability})
		if err != nil || lease == nil {
			t.Fatal("normal claim", err)
		}
		if _, err := f.s.StartWorker(ctx, p, workerRequest(lease)); err != nil {
			t.Fatal(err)
		}
		switch result {
		case "NO_REPORT":
			continue
		case "CANCELLED":
			exec(t, f.s, domain.Command{Action: "cancel_native_delivery", ProductID: "p", ID: lease.Job.Inputs.SourceProof.DeliveryID, Data: map[string]any{}})
			continue
		case "LEASE_EXPIRED":
			time.Sleep(time.Until(lease.Attempt.LeaseUntil) + 20*time.Millisecond)
			if next, err := f.s.ClaimWorker(ctx, p, domain.WorkerClaim{ProductID: "p", RepositoryID: "source", Capability: capability}); err != nil || next != nil {
				t.Fatal("unknown execution was reclaimed", err)
			}
			continue
		}
		reported := result
		if result == "PUBLICATION_FAILED" {
			reported = "EXECUTOR_FAILED"
		}
		if _, err := f.s.ReportWorker(ctx, p, domain.WorkerReport{WorkerAttemptRequest: workerRequest(lease), Result: reported}); err != nil {
			t.Fatal("accepted report", err)
		}
	}
	for range 20 {
		nativeTick(t, f)
	}
	return f, parent.ID
}

func buildActionState(t *testing.T, f *nativeFixture) domain.State {
	t.Helper()
	st, err := f.store.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestBuildFailureAgentNormalPathAndPersistentReopen(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		for _, result := range []string{"CHECK_FAILED", "EXECUTOR_FAILED"} {
			t.Run(kind+"/"+result, func(t *testing.T) {
				f, id := buildActionFixture(t, kind, result, 1)
				st := buildActionState(t, f)
				parent := operationByID(&st, id)
				if parent.Status != "FAILED" || len(st.DeliveryArtifacts) != 0 {
					t.Fatal("accepted failed build did not stop delivery")
				}
				before, _ := json.Marshal(st)
				v, err := f.s.Query(context.Background(), "get_operation", id)
				if err != nil {
					t.Fatal(err)
				}
				action, ok := v.(map[string]any)["agent_action"].(*buildFailureAgentAction)
				if !ok || action.Status != "ACTION_REQUIRED" || action.Type != "INVESTIGATE_BUILD_FAILURE" || len(action.Failures) != 1 {
					t.Fatalf("missing investigative action: %+v", v.(map[string]any)["agent_action"])
				}
				got := action.Failures[0]
				if got.ReportedResult != result || got.SourceSHA != parent.Sources[0].Result.SHA || len(got.Inputs) != 2 || got.Inputs[0].RevisionID != "rev" || got.Inputs[1].RevisionID != "build-action-rb" || got.Inputs[1].HeadSHA != strings.Repeat("e", 40) || got.Inputs[1].FeatureID != "build-action-fb" || got.JobID != st.WorkerJobs[0].ID || got.AttemptID != st.WorkerAttempts[0].ID {
					t.Fatalf("lost exact failed attempt or ordered A+B inputs: %+v", got)
				}
				after, _ := json.Marshal(buildActionState(t, f))
				if string(before) != string(after) {
					t.Fatal("query mutated state")
				}
				f.store.Close()
				f.store = f.reopen()
				f.s = New(f.store) // No current worker authority is needed for history.
				reopened, err := f.s.Query(context.Background(), "get_operation", id)
				if err != nil {
					t.Fatal(err)
				}
				left, _ := json.Marshal(action)
				right, _ := json.Marshal(reopened.(map[string]any)["agent_action"])
				if string(left) != string(right) {
					t.Fatal("historical reply changed after reopen without live authority")
				}
			})
		}
	}
}

func TestBuildFailureAgentMultipleComponents(t *testing.T) {
	f, id := buildActionFixture(t, "local", "CHECK_FAILED", 2)
	st := buildActionState(t, f)
	v := operationBuildFailureAction(&st, *operationByID(&st, id), time.Now())
	if v == nil || v.Status != "ACTION_REQUIRED" || len(v.Failures) != 2 || v.Failures[0].ComponentID == v.Failures[1].ComponentID || v.Failures[0].JobID == v.Failures[1].JobID {
		t.Fatalf("partial failed-build set: %+v", v)
	}
}

func TestBuildFailureAgentAdmissionHistoryAndCompleteSet(t *testing.T) {
	f, id := buildActionFixture(t, "local", "CHECK_FAILED", 2)
	seed := buildActionState(t, f)
	for _, tc := range []struct {
		name string
		edit func(*domain.State)
	}{
		{"admitted job fence changed", func(st *domain.State) { st.WorkerJobs[0].Fence++ }},
		{"admitted job now running", func(st *domain.State) { st.WorkerJobs[0].Status = "RUNNING" }},
		{"admitted job now succeeded", func(st *domain.State) { st.WorkerJobs[0].Status = "SUCCEEDED" }},
		{"admitted job cancellation flag", func(st *domain.State) { st.WorkerJobs[0].CancellationRequested = true }},
		{"admitted child cancelled", func(st *domain.State) {
			operationByID(st, st.WorkerJobs[0].Inputs.SourceProof.DeliveryID).Status = "CANCELLED"
		}},
		{"lost attempt", func(st *domain.State) { st.WorkerAttempts = st.WorkerAttempts[1:] }},
		{"lost report", func(st *domain.State) { st.WorkerAttempts[0].Report = nil }},
		{"rehashed execution", func(st *domain.State) {
			st.WorkerJobs[0].Inputs.Execution.CheckArgv = []string{"other-check"}
			rehashBuildActionJob(st)
		}},
		{"rehashed repository URL", func(st *domain.State) {
			st.WorkerJobs[0].Inputs.RepositoryURL = "https://github.com/other/repo"
			rehashBuildActionJob(st)
		}},
		{"rehashed trust", func(st *domain.State) {
			st.WorkerJobs[0].Inputs.Trust = "unknown"
			rehashBuildActionJob(st)
		}},
		{"missing admission", func(st *domain.State) {
			st.Events = slices.DeleteFunc(st.Events, func(e domain.Event) bool { return e.Action == "worker_report" && e.EntityID == st.WorkerJobs[0].ID })
		}},
		{"duplicate admission", func(st *domain.State) {
			for _, e := range st.Events {
				if e.Action == "worker_report" && e.EntityID == st.WorkerJobs[0].ID {
					st.Events = append(st.Events, e)
					return
				}
			}
		}},
		{"foreign admission scope", func(st *domain.State) {
			for n := range st.Events {
				if st.Events[n].Action == "worker_report" && st.Events[n].EntityID == st.WorkerJobs[0].ID {
					st.Events[n].ProductID = "foreign"
				}
			}
		}},
		{"malformed admission fence", func(st *domain.State) {
			for n := range st.Events {
				if st.Events[n].Action == "worker_report" && st.Events[n].EntityID == st.WorkerJobs[0].ID {
					st.Events[n].Data.Data["fence"] = "invalid"
				}
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := cloneCallerState(t, seed)
			tc.edit(&st)
			v := operationBuildFailureAction(&st, *operationByID(&st, id), time.Now())
			if v == nil || v.Status != "UNAVAILABLE" || len(v.Failures) != 0 || len(v.Instructions) != 0 {
				t.Fatalf("incomplete or changed history became actionable: %+v", v)
			}
		})
	}
}

func TestBuildFailureAgentBaseOnlyHasNoFabricatedFeatureInputs(t *testing.T) {
	f, id := buildActionFixture(t, "local", "CHECK_FAILED", 1, true)
	st := buildActionState(t, f)
	v := operationBuildFailureAction(&st, *operationByID(&st, id), time.Now())
	if v == nil || v.Status != "ACTION_REQUIRED" || len(v.Failures) != 1 || len(v.Failures[0].Inputs) != 0 || v.Failures[0].BaseSHA != shaBase {
		t.Fatalf("base-only source lost or fabricated selected Features: %+v", v)
	}
}

func rehashBuildActionJob(st *domain.State) {
	j := &st.WorkerJobs[0]
	j.InputHash = workerconfig.Fingerprint(j.Inputs)
	j.RequestHash = j.InputHash
	for n := range st.WorkerAttempts {
		if st.WorkerAttempts[n].JobID == j.ID {
			st.WorkerAttempts[n].InputHash = j.InputHash
			if st.WorkerAttempts[n].Report != nil {
				st.WorkerAttempts[n].Report.InputHash = j.InputHash
			}
		}
	}
}

func TestBuildFailureAgentRejectsBrokenHistoricalChain(t *testing.T) {
	f, id := buildActionFixture(t, "local", "CHECK_FAILED", 1)
	seed := buildActionState(t, f)
	for _, tc := range []struct {
		name string
		edit func(*domain.State, *domain.ExternalOperation)
	}{
		{"duplicate parent", func(st *domain.State, p *domain.ExternalOperation) { st.Operations = append(st.Operations, *p) }},
		{"duplicate child", func(st *domain.State, p *domain.ExternalOperation) {
			st.Operations = append(st.Operations, *operationByID(st, p.ChildIDs[0]))
		}},
		{"unlisted child", func(_ *domain.State, p *domain.ExternalOperation) { p.ChildIDs = []string{"absent"} }},
		{"duplicate job", func(st *domain.State, _ *domain.ExternalOperation) {
			st.WorkerJobs = append(st.WorkerJobs, st.WorkerJobs[0])
		}},
		{"duplicate attempt", func(st *domain.State, _ *domain.ExternalOperation) {
			st.WorkerAttempts = append(st.WorkerAttempts, st.WorkerAttempts[0])
		}},
		{"missing proof", func(st *domain.State, _ *domain.ExternalOperation) {
			st.WorkerJobs[0].Inputs.SourceProof = nil
			rehashBuildActionJob(st)
		}},
		{"duplicate worker operation", func(st *domain.State, _ *domain.ExternalOperation) {
			st.Operations = append(st.Operations, *operationByID(st, st.WorkerJobs[0].OperationID))
		}},
		{"empty child set", func(_ *domain.State, p *domain.ExternalOperation) { p.ChildIDs = nil }},
		{"foreign child", func(st *domain.State, p *domain.ExternalOperation) {
			operationByID(st, p.ChildIDs[0]).ProductID = "foreign"
		}},
		{"missing worker operation", func(st *domain.State, _ *domain.ExternalOperation) { st.WorkerJobs[0].OperationID = "missing" }},
		{"orphan failed job", func(st *domain.State, _ *domain.ExternalOperation) {
			j := st.WorkerJobs[0]
			j.ID = "orphan"
			st.WorkerJobs = append(st.WorkerJobs, j)
		}},
		{"foreign job", func(st *domain.State, _ *domain.ExternalOperation) { st.WorkerJobs[0].ProductID = "foreign" }},
		{"wrong report attempt", func(st *domain.State, _ *domain.ExternalOperation) { st.WorkerAttempts[0].Report.AttemptID = "other" }},
		{"wrong report job", func(st *domain.State, _ *domain.ExternalOperation) { st.WorkerAttempts[0].Report.JobID = "other" }},
		{"wrong reported result", func(st *domain.State, _ *domain.ExternalOperation) { st.WorkerAttempts[0].Report.Result = "PASSED" }},
		{"foreign attempt", func(st *domain.State, _ *domain.ExternalOperation) { st.WorkerAttempts[0].ProductID = "foreign" }},
		{"attempt hash mismatch", func(st *domain.State, _ *domain.ExternalOperation) {
			st.WorkerAttempts[0].InputHash = workerconfig.Fingerprint("other")
		}},
		{"wrong report fence", func(st *domain.State, _ *domain.ExternalOperation) { st.WorkerAttempts[0].Report.Fence++ }},
		{"wrong report hash", func(st *domain.State, _ *domain.ExternalOperation) {
			st.WorkerAttempts[0].Report.InputHash = workerconfig.Fingerprint("wrong")
		}},
		{"wrong worker parent", func(st *domain.State, _ *domain.ExternalOperation) {
			operationByID(st, st.WorkerJobs[0].OperationID).ParentID = "other"
		}},
		{"wrong worker status", func(st *domain.State, _ *domain.ExternalOperation) {
			operationByID(st, st.WorkerJobs[0].OperationID).Status = "RUNNING"
		}},
		{"self-consistent recipe ID", func(st *domain.State, _ *domain.ExternalOperation) {
			st.WorkerJobs[0].Inputs.RecipeID = "other"
			rehashBuildActionJob(st)
		}},
		{"self-consistent recipe hash", func(st *domain.State, _ *domain.ExternalOperation) {
			st.WorkerJobs[0].Inputs.RecipeHash = workerconfig.Fingerprint("other")
			rehashBuildActionJob(st)
		}},
		{"self-consistent config", func(st *domain.State, _ *domain.ExternalOperation) {
			st.WorkerJobs[0].Inputs.ConfigRevision = "cfg2"
			rehashBuildActionJob(st)
		}},
		{"self-consistent recipe revision", func(st *domain.State, _ *domain.ExternalOperation) {
			st.WorkerJobs[0].Inputs.RecipeRevision = "v2"
			rehashBuildActionJob(st)
		}},
		{"self-consistent parameters", func(st *domain.State, _ *domain.ExternalOperation) {
			st.WorkerJobs[0].Inputs.Parameters["wrong"] = "input"
			rehashBuildActionJob(st)
		}},
		{"changed prepared recipe", func(_ *domain.State, p *domain.ExternalOperation) {
			p.PreparedDeployments[0].Native.BuildRecipeHash = workerconfig.Fingerprint("other")
		}},
		{"changed source SHA", func(_ *domain.State, p *domain.ExternalOperation) { p.Sources[0].Result.SHA = strings.Repeat("d", 40) }},
		{"changed revision order", func(_ *domain.State, p *domain.ExternalOperation) {
			slices.Reverse(p.CompositionSnapshot.Components[0].RevisionIDs)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := cloneCallerState(t, seed)
			p := operationByID(&st, id)
			tc.edit(&st, p)
			before, _ := json.Marshal(st)
			v := operationBuildFailureAction(&st, *operationByID(&st, id), time.Now())
			if v != nil && (v.Status != "UNAVAILABLE" || len(v.Failures) != 0 || len(v.Instructions) != 0) {
				t.Fatalf("malformed historical context became actionable: %+v", v)
			}
			after, _ := json.Marshal(st)
			if string(before) != string(after) {
				t.Fatal("rejected read mutated state")
			}
		})
	}
}

func TestBuildFailureAgentPrivacyAndHistoricalLease(t *testing.T) {
	f, id := buildActionFixture(t, "local", "EXECUTOR_FAILED", 1)
	st := buildActionState(t, f)
	secret := "PRIVATE_OPERATOR_AND_FEATURE_CONTEXT"
	st.Features[0].Goal = strings.Repeat(secret, 1000)
	st.Memories = append(st.Memories, domain.Memory{Body: strings.Repeat(secret, 1000)})
	st.WorkerAttempts[0].Principal = secret
	st.WorkerAttempts[0].LeaseUntil = time.Now().Add(-time.Hour)
	st.WorkerJobs[0].Deadline = time.Now().Add(-time.Hour)
	p := operationByID(&st, id)
	p.Detail = secret
	v := operationBuildFailureAction(&st, *p, time.Now())
	b, _ := json.Marshal(v)
	if v == nil || v.Status != "ACTION_REQUIRED" || strings.Contains(string(b), secret) || len(b) > conflictAgentActionBytes || !strings.Contains(strings.Join(v.Instructions, " "), "Cause is unclassified") {
		t.Fatalf("historical context leaked or became current authority: %s", b)
	}
	for _, tc := range []struct {
		name string
		set  func(*domain.WorkerInputs)
	}{
		{"recipe ID", func(in *domain.WorkerInputs) { in.RecipeID = strings.Repeat("x", 129) }},
		{"recipe revision", func(in *domain.WorkerInputs) { in.RecipeRevision = strings.Repeat("x", 129) }},
		{"config revision", func(in *domain.WorkerInputs) { in.ConfigRevision = strings.Repeat("x", 129) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			copy := cloneCallerState(t, st)
			tc.set(&copy.WorkerJobs[0].Inputs)
			rehashBuildActionJob(&copy)
			v := operationBuildFailureAction(&copy, *operationByID(&copy, id), time.Now())
			if v != nil && v.Status == "ACTION_REQUIRED" {
				t.Fatal("invalid identifier echoed")
			}
		})
	}
}

func TestBuildFailureAgentNonBuildFailuresAndTextualPrecedence(t *testing.T) {
	for _, result := range []string{"NO_REPORT", "CANCELLED", "LEASE_EXPIRED", "PUBLICATION_FAILED"} {
		t.Run(result, func(t *testing.T) {
			f, id := buildActionFixture(t, "local", result, 1)
			st := buildActionState(t, f)
			before, _ := json.Marshal(st)
			v, err := f.s.Query(context.Background(), "get_operation", id)
			if err != nil || v.(map[string]any)["agent_action"] != nil {
				t.Fatalf("unaccepted/nonbuild failure relabelled: %+v %v", v, err)
			}
			after, _ := json.Marshal(buildActionState(t, f))
			if string(before) != string(after) {
				t.Fatal("query changed a stopped or unknown execution")
			}
		})
	}
	f, id := buildActionFixture(t, "local", "CHECK_FAILED", 1)
	st := buildActionState(t, f)
	// A broken textual record still takes precedence over a valid build failure.
	st.CompositionConflicts = append(st.CompositionConflicts, domain.CompositionConflict{Meta: domain.Meta{ID: "textual-record", ProductID: "p"}, OperationID: id})
	svc := New(&memoryStore{state: st})
	v, err := svc.Query(context.Background(), "get_operation", id)
	if err != nil {
		t.Fatal(err)
	}
	action, ok := v.(map[string]any)["agent_action"].(*conflictAgentAction)
	if !ok || action.Type != "RESOLVE_CONFLICT" || action.Status != "UNAVAILABLE" {
		t.Fatal("invalid textual context fell through into another workflow")
	}
}

func TestBuildFailureAgentCompleteSetBudget(t *testing.T) {
	f, id := buildActionFixture(t, "local", "CHECK_FAILED", 1)
	st := buildActionState(t, f)
	parent := *operationByID(&st, id)
	child := *operationByID(&st, parent.ChildIDs[0])
	workerOp := *operationByID(&st, st.WorkerJobs[0].OperationID)
	job, attempt := st.WorkerJobs[0], st.WorkerAttempts[0]
	prepared := parent.PreparedDeployments[0]
	app := parent.CompositionSnapshot.ApplicationSnapshots[0]
	selected := parent.CompositionSnapshot.Components[0]
	parent.ChildIDs, parent.PreparedDeployments = nil, nil
	parent.CompositionSnapshot.Components, parent.CompositionSnapshot.ApplicationSnapshots = nil, nil
	st.Operations, st.WorkerJobs, st.WorkerAttempts, st.Events = nil, nil, nil, nil
	// Synthetic aggregation stress, derived from one normally admitted failure.
	for n := range 40 {
		componentID := fmt.Sprintf("budget-component-%d", n)
		appCopy, selection, preparedCopy := app, selected, prepared
		appCopy.ID, selection.ApplicationID = componentID, componentID
		preparedCopy.Application = appCopy
		preparedCopy.Build.ApplicationID = componentID
		parent.CompositionSnapshot.ApplicationSnapshots = append(parent.CompositionSnapshot.ApplicationSnapshots, appCopy)
		parent.CompositionSnapshot.Components = append(parent.CompositionSnapshot.Components, selection)
		parent.PreparedDeployments = append(parent.PreparedDeployments, preparedCopy)
		parent.ChildIDs = append(parent.ChildIDs, fmt.Sprintf("budget-child-%d", n))
	}
	st.Operations = append(st.Operations, parent)
	for n := range 40 {
		copy := cloneCallerState(t, domain.State{Operations: []domain.ExternalOperation{child, workerOp}, WorkerJobs: []domain.WorkerJob{job}, WorkerAttempts: []domain.WorkerAttempt{attempt}})
		c, w, j, a := &copy.Operations[0], &copy.Operations[1], &copy.WorkerJobs[0], &copy.WorkerAttempts[0]
		c.ID, c.ApplicationID = parent.ChildIDs[n], parent.PreparedDeployments[n].Application.ID
		c.Snapshot.Application = parent.PreparedDeployments[n].Application
		c.Snapshot.Build.ApplicationID = c.ApplicationID
		c.Snapshot.Revision.ID = fmt.Sprintf("budget-generated-revision-%d", n)
		c.Native.BuildJobID = fmt.Sprintf("budget-job-%d", n)
		c.Native.ParentHash = nativeParentHash(&parent)
		w.ID, w.ParentID = fmt.Sprintf("budget-worker-%d", n), c.ID
		j.ID, j.OperationID, j.Inputs.RevisionID = c.Native.BuildJobID, w.ID, c.Snapshot.Revision.ID
		j.Inputs.SourceProof = nativeSourceProof(c)
		j.InputHash = workerconfig.Fingerprint(j.Inputs)
		j.RequestHash = j.InputHash
		a.ID, a.JobID, a.InputHash = fmt.Sprintf("budget-attempt-%d", n), j.ID, j.InputHash
		a.Report.WorkerAttemptRequest = domain.WorkerAttemptRequest{JobID: j.ID, AttemptID: a.ID, Fence: j.Fence, InputHash: j.InputHash}
		st.Operations = append(st.Operations, *c, *w)
		st.WorkerJobs = append(st.WorkerJobs, *j)
		st.WorkerAttempts = append(st.WorkerAttempts, *a)
		// Seed the existing admission audit for each synthetic cloned job.
		workerAudit(&st, j, a.Principal, "worker_report", time.Now())
	}
	v := operationBuildFailureAction(&st, *operationByID(&st, id), time.Now())
	b, _ := json.Marshal(v)
	if v == nil || v.Status != "UNAVAILABLE" || v.Reason != "BUILD_FAILURE_CONTEXT_TOO_LARGE" || len(v.Failures) != 0 || len(v.Instructions) != 0 || len(b) > conflictAgentActionBytes {
		t.Fatalf("partial or oversized aggregate returned: %s", b)
	}
}
