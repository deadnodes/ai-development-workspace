package application

import (
	"context"
	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
	"strings"
	"testing"
	"time"
)

type fakeRuntimeObserver struct {
	calls          int
	target         delivery.RuntimeTarget
	evidence       delivery.RuntimeEvidence
	commit, digest string
}

func (f *fakeRuntimeObserver) TargetIdentity() delivery.RuntimeTarget { return f.target }
func (f *fakeRuntimeObserver) Observe(_ context.Context, commit, digest string) delivery.RuntimeEvidence {
	f.calls++
	f.commit = commit
	f.digest = digest
	return f.evidence
}
func TestRuntimeObserverScopesAndPersistsEvidence(t *testing.T) {
	m := &memoryStore{state: domain.EmptyState()}
	// The observer must never certify another Product's newer deployment.
	now := time.Now().UTC()
	m.state.Operations = []domain.ExternalOperation{
		{Meta: domain.Meta{ID: "own", ProductID: "p", CreatedAt: now}, EnvironmentID: "env", ApplicationID: "app", Status: "SUCCEEDED", GitOpsResult: &delivery.GitOpsResult{CommitSHA: "own-commit"}, Artifact: &delivery.ArtifactResult{Digest: "own-digest"}},
		{Meta: domain.Meta{ID: "other", ProductID: "other", CreatedAt: now.Add(time.Hour)}, EnvironmentID: "env", ApplicationID: "app", Status: "SUCCEEDED", GitOpsResult: &delivery.GitOpsResult{CommitSHA: "other-commit"}, Artifact: &delivery.ArtifactResult{Digest: "other-digest"}},
	}
	s := New(m)
	o := &fakeRuntimeObserver{target: delivery.RuntimeTarget{ProductID: "p", EnvironmentID: "env", ApplicationID: "app"}, evidence: delivery.RuntimeEvidence{Details: `{"runtime":"UNKNOWN"}`}}
	if e := s.ObserveRuntimeOnce(context.Background(), []delivery.RuntimeObserver{o}); e != nil {
		t.Fatal(e)
	}
	if o.commit != "own-commit" || o.digest != "own-digest" || len(m.state.RuntimeObservations) != 1 || m.state.RuntimeObservations[0].Healthy {
		t.Fatalf("wrong observation %+v", m.state.RuntimeObservations)
	}
	if e := s.ObserveRuntimeOnce(context.Background(), []delivery.RuntimeObserver{o}); e != nil {
		t.Fatal(e)
	}
	if len(m.state.RuntimeObservations) != 1 {
		t.Fatal("duplicate unchanged poll")
	}
	o.evidence = delivery.RuntimeEvidence{Healthy: true, Details: "exact healthy observation"}
	if e := s.ObserveRuntimeOnce(context.Background(), []delivery.RuntimeObserver{o}); e != nil {
		t.Fatal(e)
	}
	if len(m.state.RuntimeObservations) != 2 || !m.state.RuntimeObservations[1].Healthy {
		t.Fatal("changed evidence missing")
	}
}

func TestObservedDeploymentEnvironmentAndAttention(t *testing.T) {
	s, m, _ := flowFixture(t)
	op := domain.ExternalOperation{Meta: domain.Meta{ID: "direct", ProductID: "p"}, Kind: "DEPLOY", EnvironmentID: "dev", Status: "SUCCEEDED", DeploymentState: "GITOPS_APPLIED", GitOpsResult: &delivery.GitOpsResult{CommitSHA: "commit"}, Artifact: &delivery.ArtifactResult{Digest: "digest"}}
	m.state.Operations = append(m.state.Operations, op)
	m.state.RuntimeObservations = append(m.state.RuntimeObservations, domain.RuntimeObservation{Meta: domain.Meta{ProductID: "p", Actor: "system/runtime-observer"}, OperationID: op.ID, EnvironmentID: "dev", GitOpsCommit: "commit", ArtifactDigest: "digest", Healthy: true, Details: "server-collected evidence"})
	value, e := s.Query(context.Background(), "get_environment_state", "dev")
	if e != nil {
		t.Fatal(e)
	}
	result := value.(map[string]any)
	if strings.Contains(result["reconciliation"].(string), "no direct Flux observer") || len(result["runtime_observations"].([]domain.RuntimeObservation)) != 1 {
		t.Fatal("observer evidence misrepresented")
	}
	value, e = s.Query(context.Background(), "list_attention", "p")
	if e != nil {
		t.Fatal(e)
	}
	for _, item := range value.([]map[string]any) {
		if item["id"] == op.ID {
			t.Fatal("healthy exact observation remains pending", item)
		}
	}
	m.state.RuntimeObservations[0].Healthy = false
	value, e = s.Query(context.Background(), "list_attention", "p")
	if e != nil {
		t.Fatal(e)
	}
	found := false
	for _, item := range value.([]map[string]any) {
		if item["id"] == op.ID && item["reason"] == "PENDING_RECONCILIATION" {
			found = true
		}
	}
	if !found {
		t.Fatal("unhealthy observation must retain pending attention")
	}
}
