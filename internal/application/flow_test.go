package application

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
)

type fakeFlow struct {
	fakeDelivery
	main         string
	branches     map[string]string
	parents      map[string][]string
	plans        []delivery.SourcePlan
	builds       []delivery.BuildRequest
	deployments  map[string]string
	applyCounts  map[string]int
	conflict     bool
	advances     int
	onRead       func()
	reportDigest string
}

func (f *fakeFlow) Branches(context.Context, delivery.Connection, string) ([]delivery.BranchInfo, error) {
	out := []delivery.BranchInfo{{Name: "main", SHA: f.main}}
	for name, sha := range f.branches {
		out = append(out, delivery.BranchInfo{Name: name, SHA: sha})
	}
	return out, nil
}
func (f *fakeFlow) Compare(_ context.Context, _ delivery.Connection, _ string, base, head string) (delivery.CompareResult, error) {
	ancestors := func(root string) map[string]bool {
		seen := map[string]bool{}
		queue := []string{root}
		for len(queue) > 0 {
			v := queue[0]
			queue = queue[1:]
			if seen[v] {
				continue
			}
			seen[v] = true
			queue = append(queue, f.parents[v]...)
		}
		return seen
	}
	a, b := ancestors(base), ancestors(head)
	out := delivery.CompareResult{BaseSHA: base, HeadSHA: head, Status: "identical"}
	shas := []string{}
	for sha := range b {
		if !a[sha] {
			shas = append(shas, sha)
		}
	}
	sort.Strings(shas)
	for _, sha := range shas {
		out.Commits = append(out.Commits, delivery.CommitInfo{SHA: sha})
		out.Ahead++
	}
	for sha := range a {
		if !b[sha] {
			out.Behind++
		}
	}
	if out.Behind > 0 {
		out.Status = "diverged"
	} else if out.Ahead > 0 {
		out.Status = "ahead"
	}
	return out, nil
}
func (f *fakeFlow) ComposeSource(_ context.Context, _ delivery.Connection, p delivery.SourcePlan) (delivery.SourceResult, error) {
	f.plans = append(f.plans, p)
	if f.conflict {
		return delivery.SourceResult{Branch: p.TargetBranch, Conflict: true, Detail: "shared.go"}, nil
	}
	sum := sha256.Sum256([]byte(p.BaseSHA + strings.Join(p.HeadSHAs, "")))
	sha := fmt.Sprintf("%x", sum)[:40]
	f.parents[sha] = append([]string{p.BaseSHA}, p.HeadSHAs...)
	f.branches[p.TargetBranch] = sha
	return delivery.SourceResult{Branch: p.TargetBranch, SHA: sha}, nil
}
func (f *fakeFlow) AdvanceMain(_ context.Context, _ delivery.Connection, _ string, _ string, base, candidate string) (delivery.SourceResult, error) {
	if f.main != base && f.main != candidate {
		return delivery.SourceResult{}, fmt.Errorf("main moved")
	}
	f.advances++
	f.main = candidate
	return delivery.SourceResult{Branch: "main", SHA: candidate}, nil
}
func (f *fakeFlow) Dispatch(_ context.Context, _ delivery.Connection, req delivery.BuildRequest) (delivery.DispatchResult, error) {
	f.builds = append(f.builds, req)
	return delivery.DispatchResult{ProviderRunID: int64(len(f.builds))}, nil
}
func (f *fakeFlow) FindRun(_ context.Context, _ delivery.Connection, req delivery.BuildRequest) (delivery.RunResult, error) {
	digest := flowDigest(req.SourceSHA)
	if f.reportDigest != "" {
		digest = f.reportDigest
	}
	return delivery.RunResult{Found: true, ID: 1, Status: "completed", Conclusion: "success", HeadSHA: req.WorkflowSHA, Artifacts: map[string]string{"digest": digest, "source_sha": req.SourceSHA, "image_repository": "ghcr.io/owner/component"}}, nil
}
func flowDigest(sha string) string { return fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(sha))) }
func (f *fakeFlow) InspectArtifact(_ context.Context, _ delivery.Connection, req delivery.ArtifactRequest) (delivery.ArtifactResult, error) {
	available := req.EvidenceRunID > 0
	for _, b := range f.builds {
		if b.SourceSHA == req.SourceSHA {
			available = true
		}
	}
	return delivery.ArtifactResult{Available: available, Digest: flowDigest(req.SourceSHA), ObservedAt: time.Now().UTC()}, nil
}
func (f *fakeFlow) ReadGitOps(_ context.Context, _ delivery.Connection, req delivery.GitOpsRequest) (delivery.GitOpsSnapshot, error) {
	if f.onRead != nil {
		f.onRead()
	}
	return delivery.GitOpsSnapshot{ImageRepository: "ghcr.io/owner/component", HeadSHA: "gitops-current", BlobSHA: "blob-current", Digest: f.deployments[req.Path]}, nil
}
func (f *fakeFlow) ApplyGitOps(_ context.Context, _ delivery.Connection, req delivery.GitOpsApply) (delivery.GitOpsResult, error) {
	f.deployments[req.Request.Path] = req.Digest
	f.applyCounts[req.Request.Path]++
	return delivery.GitOpsResult{CommitSHA: "gitops-" + req.OperationID, BlobSHA: "blob-new"}, nil
}
func flowFixture(t *testing.T) (*Service, *memoryStore, *fakeFlow) {
	_, m, _ := externalFixture(t)
	f := &fakeFlow{main: shaBase, branches: map[string]string{"feature/work": shaHead, "feature/second": strings.Repeat("e", 40)}, parents: map[string][]string{shaHead: {shaBase}, strings.Repeat("e", 40): {shaBase}}, deployments: map[string]string{}, applyCounts: map[string]int{}}
	return NewWithProvider(m, f), m, f
}
func flowEnvironment(t *testing.T, s *Service, id, purpose string) {
	exec(t, s, domain.Command{Action: "create_environment", ID: id, ProductID: "p", Data: map[string]any{"name": id, "cluster": "local", "namespace": id}})
	exec(t, s, domain.Command{Action: "configure_environment", ProductID: "p", Data: map[string]any{"environment_id": id, "application_id": "component", "purpose": purpose, "connection_id": "conn", "repository_id": "gitops", "ref": "main", "path": id + ".yaml", "image_field": "spec.values.image.repository", "digest_field": "spec.values.image.tag", "allow_deploy": true}})
}
func flowPlan(t *testing.T, s *Service, id, env string, revisions ...string) {
	exec(t, s, domain.Command{Action: "plan_composition", ID: id, ProductID: "p", Data: map[string]any{"name": id, "environment_id": env, "components": []domain.CompositionComponent{component("component", revisions...)}}})
}
func flowTickUntil(t *testing.T, s *Service, m *memoryStore, id string, predicate func(domain.ExternalOperation) bool) {
	t.Helper()
	for range 100 {
		op := operationByID(&m.state, id)
		if op == nil {
			t.Fatal("missing operation")
		}
		if predicate(*op) {
			return
		}
		if op.Status == "FAILED" {
			t.Fatalf("operation failed: %+v", *op)
		}
		for i := range m.state.Operations {
			m.state.Operations[i].NextAttemptAt = m.state.Operations[i].NextAttemptAt.Add(-time.Hour)
			m.state.Operations[i].LeaseUntil = time.Time{}
		}
		worked, err := s.Tick(context.Background())
		if err != nil || !worked {
			t.Fatalf("flow tick worked=%v err=%v", worked, err)
		}
	}
	t.Fatalf("operation never reached expected state: %+v", *operationByID(&m.state, id))
}
func flowStart(t *testing.T, s *Service, m *memoryStore, plan string) string {
	exec(t, s, domain.Command{Action: "reconcile_composition", ID: plan, Data: map[string]any{}})
	return m.state.Operations[len(m.state.Operations)-1].ID
}
func flowApplied(op domain.ExternalOperation) bool {
	return op.DeploymentState == "GITOPS_APPLIED" || op.DeploymentState == "PENDING_RECONCILIATION" || op.DeploymentState == "DEPLOYED"
}
func TestFlowIndependentTargetsRemoveFeature(t *testing.T) {
	s, m, f := flowFixture(t)
	flowEnvironment(t, s, "test", "TEST")
	exec(t, s, domain.Command{Action: "create_feature", ID: "f2", ProductID: "p", Data: map[string]any{"title": "Second", "goal": "Independent"}})
	exec(t, s, domain.Command{Action: "create_integration", ID: "i2", FeatureID: "f2", Data: map[string]any{"title": "Other", "objective": "Ship separately"}})
	exec(t, s, domain.Command{Action: "record_integration_revision", ID: "rev2", IntegrationID: "i2", Data: map[string]any{"repository_id": "source", "branch": "feature/second", "base_commit": shaBase, "head_commit": strings.Repeat("e", 40), "commits": []string{strings.Repeat("e", 40)}}})
	flowPlan(t, s, "both", "dev", "rev", "rev2")
	a := flowStart(t, s, m, "both")
	flowTickUntil(t, s, m, a, flowApplied)
	flowPlan(t, s, "other-target", "test", "rev2")
	b := flowStart(t, s, m, "other-target")
	flowTickUntil(t, s, m, b, flowApplied)
	unchanged := f.deployments["test.yaml"]
	old := f.deployments["app.yaml"]
	flowPlan(t, s, "remove-first", "dev", "rev2")
	removed := flowStart(t, s, m, "remove-first")
	flowTickUntil(t, s, m, removed, flowApplied)
	if f.deployments["test.yaml"] != unchanged || f.applyCounts["test.yaml"] != 1 {
		t.Fatal("other target was changed")
	}
	if f.deployments["app.yaml"] == old {
		t.Fatal("removed feature still in artifact")
	}
	last := f.plans[len(f.plans)-1]
	if len(last.HeadSHAs) != 1 || last.HeadSHAs[0] != strings.Repeat("e", 40) || last.BaseSHA != shaBase {
		t.Fatal("removal did not rebuild from clean main")
	}
}
func TestFlowConflictAndSupersessionDoNotDeploy(t *testing.T) {
	for _, mode := range []string{"conflict", "superseded"} {
		t.Run(mode, func(t *testing.T) {
			s, m, f := flowFixture(t)
			f.deployments["app.yaml"] = "old-running-digest"
			flowPlan(t, s, "first", "dev", "rev")
			op := flowStart(t, s, m, "first")
			if mode == "conflict" {
				f.conflict = true
			} else {
				flowPlan(t, s, "replacement", "dev")
				exec(t, s, domain.Command{Action: "select_composition", ID: "replacement"})
			}
			flowTickUntil(t, s, m, op, func(o domain.ExternalOperation) bool { return terminalOperation(o.Status) })
			if f.applyCounts["app.yaml"] != 0 || f.deployments["app.yaml"] != "old-running-digest" {
				t.Fatal("invalid composition touched live desired image")
			}
		})
	}
}
func TestFlowHotfixKeepsFeatureAndReleaseHistory(t *testing.T) {
	s, m, _ := flowFixture(t)
	exec(t, s, domain.Command{Action: "start_integration", IntegrationID: "i"})
	exec(t, s, domain.Command{Action: "complete_integration", IntegrationID: "i"})
	// Seed an already released integration; release completion owns this transition.
	integration(&m.state, "i").Status = "released"
	exec(t, s, domain.Command{Action: "create_hotfix", ID: "fix", FeatureID: "f", Data: map[string]any{"title": "Fix callback", "objective": "Preserve released API", "repositories": []string{"source"}}})
	fix := integration(&m.state, "fix")
	if fix == nil || fix.Kind != "hotfix" || fix.FeatureID != "f" || integration(&m.state, "i").Status != "released" || len(m.state.Features) != 1 {
		t.Fatal("hotfix rewrote released work or created another feature")
	}
}

func TestFlowSelectedMainCandidateRequiresOwnVerification(t *testing.T) {
	s, m, f := flowFixture(t)
	flowEnvironment(t, s, "prod", "PROD")
	exec(t, s, domain.Command{Action: "start_integration", IntegrationID: "i"})
	exec(t, s, domain.Command{Action: "complete_integration", IntegrationID: "i"})
	exec(t, s, domain.Command{Action: "prepare_release_candidate", ID: "candidate", ProductID: "p", Data: map[string]any{"name": "Selected release", "environment_id": "prod", "components": []domain.CompositionComponent{component("component", "rev")}, "approve_main_update": true}})
	flowTickUntil(t, s, m, "candidate", func(o domain.ExternalOperation) bool { return o.DeploymentState == "READY_FOR_VERIFICATION" })
	candidate := operationByID(&m.state, "candidate")
	if f.advances != 1 || f.applyCounts["prod.yaml"] != 0 || len(candidate.ChildIDs) != 1 {
		t.Fatal("candidate must advance selected main/build without deployment")
	}
	child := operationByID(&m.state, candidate.ChildIDs[0])
	if child.Snapshot.BuildRequest.SourceSHA != f.main || child.Artifact == nil {
		t.Fatal("candidate not built from resulting main")
	}
	reject(t, s, domain.Command{Action: "promote_release_candidate", ID: "candidate", Data: map[string]any{"approve": true}})
	exec(t, s, domain.Command{Action: "create_test_scenario", ID: "release-scenario", ProductID: "p", Data: map[string]any{"title": "Isolated main behavior", "objective": "Works without other DEV work", "integration_ids": []string{"i"}, "steps": []string{"exercise API"}, "expected_outcomes": []string{"success"}, "mechanism": "integration", "blocking": true}})
	exec(t, s, domain.Command{Action: "record_scenario_run", ID: "verified", ProductID: "p", Data: map[string]any{"scenario_version_id": "release-scenario", "candidate_operation_id": "candidate", "components": []domain.ScenarioComponentEvidence{{ApplicationID: "component", OperationID: child.ID, SourceSHA: child.Snapshot.BuildRequest.SourceSHA, ArtifactDigest: child.Artifact.Digest}}, "result": "passed", "observations": []string{"executor verified exact candidate image"}}})
	exec(t, s, domain.Command{Action: "promote_release_candidate", ID: "candidate", Data: map[string]any{"approve": true}})
	promotion := m.state.Operations[len(m.state.Operations)-1].ID
	flowTickUntil(t, s, m, promotion, flowApplied)
	if len(m.state.Releases) > 0 {
		t.Fatal("release recorded before runtime observation")
	}
	parent := operationByID(&m.state, promotion)
	for _, childID := range parent.ChildIDs {
		deploy := operationByID(&m.state, childID)
		exec(t, s, domain.Command{Action: "record_runtime_observation", ID: childID, Data: map[string]any{"environment_id": "prod", "gitops_commit": deploy.GitOpsResult.CommitSHA, "artifact_digest": deploy.Artifact.Digest, "healthy": true, "details": "Flux revision and healthy pods observed by executor"}})
	}
	flowTickUntil(t, s, m, promotion, func(o domain.ExternalOperation) bool { return o.DeploymentState == "DEPLOYED" })
	if len(m.state.Releases) != 1 || integration(&m.state, "i").Status != "released" || f.deployments["prod.yaml"] != child.Artifact.Digest {
		t.Fatal("release/runtime provenance missing")
	}
	originalRelease := m.state.Releases[0]
	priorMain := f.main
	fixSHA := strings.Repeat("f", 40)
	f.parents[fixSHA] = []string{priorMain}
	f.branches["feature/hotfix"] = fixSHA
	exec(t, s, domain.Command{Action: "create_hotfix", ID: "hotfix", FeatureID: "f", Data: map[string]any{"title": "Fix A", "objective": "Keep release behavior", "repositories": []string{"source"}}})
	exec(t, s, domain.Command{Action: "record_integration_revision", ID: "fix-revision", IntegrationID: "hotfix", Data: map[string]any{"repository_id": "source", "branch": "feature/hotfix", "base_commit": priorMain, "head_commit": fixSHA, "commits": []string{fixSHA}}})
	fixComponent := component("component", "fix-revision")
	fixComponent.BaseCommit = priorMain
	exec(t, s, domain.Command{Action: "plan_composition", ID: "fix-test", ProductID: "p", Data: map[string]any{"name": "Test hotfix", "environment_id": "dev", "components": []domain.CompositionComponent{fixComponent}}})
	fixTest := flowStart(t, s, m, "fix-test")
	flowTickUntil(t, s, m, fixTest, flowApplied)
	flowConfirmRuntime(t, s, m, fixTest)
	exec(t, s, domain.Command{Action: "start_integration", IntegrationID: "hotfix"})
	exec(t, s, domain.Command{Action: "complete_integration", IntegrationID: "hotfix"})
	exec(t, s, domain.Command{Action: "prepare_release_candidate", ID: "fix-candidate", ProductID: "p", Data: map[string]any{"name": "Hotfix release", "environment_id": "prod", "components": []domain.CompositionComponent{fixComponent}, "approve_main_update": true}})
	flowTickUntil(t, s, m, "fix-candidate", func(o domain.ExternalOperation) bool { return o.DeploymentState == "READY_FOR_VERIFICATION" })
	flowVerifyAndPromote(t, s, m, "fix-candidate", "fix-scenario", "hotfix")
	if len(m.state.Features) != 1 || len(m.state.Releases) != 2 || m.state.Releases[0].ID != originalRelease.ID || m.state.Releases[0].SourceCommits["component"] != originalRelease.SourceCommits["component"] || integration(&m.state, "hotfix").Status != "released" {
		t.Fatal("hotfix must append release to same feature without rewriting original release")
	}

}

func TestFlowSelectionChangesDuringGitOpsRead(t *testing.T) {
	s, m, f := flowFixture(t)
	flowPlan(t, s, "first", "dev", "rev")
	flowPlan(t, s, "replacement", "dev")
	op := flowStart(t, s, m, "first")
	changed := false
	f.onRead = func() {
		if changed {
			return
		}
		for _, child := range m.state.Operations {
			if child.ParentID == op && child.Phase == "APPLY" {
				changed = true
				exec(t, s, domain.Command{Action: "select_composition", ID: "replacement"})
				return
			}
		}
	}
	flowTickUntil(t, s, m, op, func(o domain.ExternalOperation) bool { return terminalOperation(o.Status) })
	if !changed {
		t.Fatal("did not exercise interleaving at GitOps pre-write read")
	}
	if f.applyCounts["app.yaml"] != 0 {
		t.Fatal("superseded desired state committed during ReadGitOps race")
	}
}

func flowConfirmRuntime(t *testing.T, s *Service, m *memoryStore, parentID string) {
	t.Helper()
	parent := operationByID(&m.state, parentID)
	ids := append([]string{}, parent.ChildIDs...)
	for _, childID := range ids {
		deploy := operationByID(&m.state, childID)
		exec(t, s, domain.Command{Action: "record_runtime_observation", ID: childID, Data: map[string]any{"environment_id": parent.EnvironmentID, "gitops_commit": deploy.GitOpsResult.CommitSHA, "artifact_digest": deploy.Artifact.Digest, "healthy": true, "details": "executor observed matching healthy runtime"}})
	}
	flowTickUntil(t, s, m, parentID, func(o domain.ExternalOperation) bool { return o.DeploymentState == "DEPLOYED" })
}
func flowVerifyAndPromote(t *testing.T, s *Service, m *memoryStore, candidateID, scenarioID, integrationID string) {
	t.Helper()
	candidate := operationByID(&m.state, candidateID)
	child := operationByID(&m.state, candidate.ChildIDs[0])
	exec(t, s, domain.Command{Action: "create_test_scenario", ID: scenarioID, ProductID: "p", Data: map[string]any{"title": "Regression", "objective": "Preserve compatibility", "integration_ids": []string{integrationID}, "steps": []string{"exercise API"}, "expected_outcomes": []string{"compatible result"}, "mechanism": "integration", "blocking": true}})
	exec(t, s, domain.Command{Action: "record_scenario_run", ProductID: "p", Data: map[string]any{"scenario_version_id": scenarioID, "candidate_operation_id": candidateID, "components": []domain.ScenarioComponentEvidence{{ApplicationID: "component", OperationID: child.ID, SourceSHA: child.Snapshot.BuildRequest.SourceSHA, ArtifactDigest: child.Artifact.Digest}}, "result": "passed", "observations": []string{"candidate regression passed"}}})
	exec(t, s, domain.Command{Action: "promote_release_candidate", ID: candidateID, Data: map[string]any{"approve": true}})
	promotion := m.state.Operations[len(m.state.Operations)-1].ID
	flowTickUntil(t, s, m, promotion, flowApplied)
	flowConfirmRuntime(t, s, m, promotion)
}

func TestFlowRestartDoesNotDuplicateSourceOrBuild(t *testing.T) {
	s, m, f := flowFixture(t)
	flowPlan(t, s, "restart", "dev", "rev")
	opID := flowStart(t, s, m, "restart")
	flowTickUntil(t, s, m, opID, func(o domain.ExternalOperation) bool { return len(o.Sources) > 0 && o.Sources[0].Result != nil })
	before := len(f.plans)
	s = NewWithProvider(m, f)
	flowTickUntil(t, s, m, opID, func(o domain.ExternalOperation) bool {
		for _, id := range o.ChildIDs {
			child := operationByID(&m.state, id)
			if child != nil && child.Phase == "BUILD_LOOKUP" {
				return true
			}
		}
		return false
	})
	s = NewWithProvider(m, f)
	flowTickUntil(t, s, m, opID, flowApplied)
	if len(f.plans) != before || len(f.builds) != 1 || f.applyCounts["app.yaml"] != 1 {
		t.Fatal("restart duplicated source/build/deploy side effects")
	}
}

func TestFlowSourceRejectsUndeclaredHistory(t *testing.T) {
	for _, mode := range []string{"extra-commit", "generated", "main", "wrong-origin", "missing-head", "valid-old-origin"} {
		t.Run(mode, func(t *testing.T) {
			s, m, f := flowFixture(t)
			flowPlan(t, s, "plan", "dev", "rev")
			opID := flowStart(t, s, m, "plan")
			op := *operationByID(&m.state, opID)
			source := op.Sources[0]
			switch mode {
			case "extra-commit":
				foreign := strings.Repeat("d", 40)
				f.parents[foreign] = []string{shaBase}
				f.parents[shaHead] = []string{foreign}
			case "generated":
				op.CompositionSnapshot.RevisionSnapshots[0].Branch = "generated/dev"
			case "main":
				op.CompositionSnapshot.RevisionSnapshots[0].Branch = "main"
			case "wrong-origin":
				op.CompositionSnapshot.RevisionSnapshots[0].BaseCommit = strings.Repeat("9", 40)
			case "missing-head":
				source.Plan.HeadSHAs = nil
			case "valid-old-origin":
				newMain := strings.Repeat("8", 40)
				f.parents[newMain] = []string{shaBase}
				f.main = newMain
				source.Plan.BaseSHA = newMain
			}
			err := validateFlowSource(context.Background(), f, op, source)
			if mode == "valid-old-origin" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatalf("accepted %s", mode)
			}
		})
	}
}

func TestFlowPromotionRefreshesRegistryProofAndPinsApprovedDigest(t *testing.T) {
	for _, changedDigest := range []bool{false, true} {
		t.Run(fmt.Sprintf("rebuilt_digest_changes_%t", changedDigest), func(t *testing.T) {
			s, m, f := flowFixture(t)
			flowEnvironment(t, s, "prod", "PROD")
			exec(t, s, domain.Command{Action: "start_integration", IntegrationID: "i"})
			exec(t, s, domain.Command{Action: "complete_integration", IntegrationID: "i"})
			exec(t, s, domain.Command{Action: "prepare_release_candidate", ID: "candidate", ProductID: "p", Data: map[string]any{"name": "Selected main", "environment_id": "prod", "components": []domain.CompositionComponent{component("component", "rev")}, "approve_main_update": true}})
			flowTickUntil(t, s, m, "candidate", func(op domain.ExternalOperation) bool { return op.DeploymentState == "READY_FOR_VERIFICATION" })
			candidate := operationByID(&m.state, "candidate")
			built := operationByID(&m.state, candidate.ChildIDs[0])
			approvedDigest := built.Artifact.Digest
			originalRequest := f.builds[0]
			exec(t, s, domain.Command{Action: "create_test_scenario", ID: "scenario", ProductID: "p", Data: map[string]any{"title": "Main verification", "objective": "Safe release", "integration_ids": []string{"i"}, "steps": []string{"test API"}, "expected_outcomes": []string{"compatible"}, "mechanism": "integration", "blocking": true}})
			exec(t, s, domain.Command{Action: "record_scenario_run", ProductID: "p", Data: map[string]any{"scenario_version_id": "scenario", "candidate_operation_id": "candidate", "components": []domain.ScenarioComponentEvidence{{ApplicationID: "component", OperationID: built.ID, SourceSHA: built.Snapshot.Revision.HeadCommit, ArtifactDigest: approvedDigest}}, "result": "passed", "observations": []string{"exact approved image verified"}}})
			if changedDigest {
				f.reportDigest = "sha256:" + strings.Repeat("9", 64)
			}
			exec(t, s, domain.Command{Action: "promote_release_candidate", ID: "candidate", Data: map[string]any{"approve": true}})
			promotionID := m.state.Operations[len(m.state.Operations)-1].ID
			flowTickUntil(t, s, m, promotionID, func(op domain.ExternalOperation) bool { return terminalOperation(op.Status) || flowApplied(op) })
			if len(f.builds) != 2 {
				t.Fatalf("expected fresh promotion probe after candidate build, got %d", len(f.builds))
			}
			fresh := f.builds[1]
			parent := operationByID(&m.state, promotionID)
			deployment := operationByID(&m.state, parent.ChildIDs[0])
			if fresh.OperationID == originalRequest.OperationID || fresh.OperationID != deployment.ID || fresh.Inputs["operation_id"] != deployment.ID || fresh.SourceSHA != originalRequest.SourceSHA || fresh.ImageTag != originalRequest.ImageTag || !fresh.RequestedAt.After(originalRequest.RequestedAt) {
				t.Fatal("promotion reused old registry correlation or changed approved source")
			}
			if changedDigest {
				if f.applyCounts["prod.yaml"] != 0 || parent.Status != "FAILED" || deployment.ExpectedDigest != approvedDigest || !strings.Contains(deployment.Detail, "different digest") {
					t.Fatal("reconstructed image bypassed approved digest requirement")
				}
			} else if f.deployments["prod.yaml"] != approvedDigest || f.applyCounts["prod.yaml"] != 1 {
				t.Fatal("fresh registry proof did not deploy approved immutable digest")
			}
		})
	}
}
