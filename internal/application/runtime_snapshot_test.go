package application

import (
	"context"
	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
	"strings"
	"testing"
	"time"
)

type inventoryFake struct {
	target delivery.RuntimeTarget
	result delivery.RuntimeSnapshot
	calls  int
}

func (f *inventoryFake) TargetIdentity() delivery.RuntimeTarget { return f.target }
func (f *inventoryFake) Snapshot(context.Context) delivery.RuntimeSnapshot {
	f.calls++
	return f.result
}
func TestRuntimeSnapshotIndependentOfDeploymentAndMultipleWorkloads(t *testing.T) {
	s, m, p := flowFixture(t)
	d := "sha256:" + strings.Repeat("a", 64)
	p.deployments["app.yaml"] = d
	target := delivery.RuntimeTarget{ProductID: "p", EnvironmentID: "dev", ApplicationID: "component"}
	a := &inventoryFake{target: target, result: delivery.RuntimeSnapshot{Deployment: "api", Container: "api", Namespace: "dev", ObservedAt: time.Now(), Health: "HEALTHY", WorkloadImage: "ghcr.io/owner/component@" + d, Pods: []delivery.RuntimePod{{Image: "ghcr.io/owner/component@" + d, ImageID: "docker-pullable://ghcr.io/owner/component@" + d, Ready: true}}}}
	b := &inventoryFake{target: target, result: a.result}
	b.result.Deployment = "worker"
	unrelated := &inventoryFake{target: delivery.RuntimeTarget{ProductID: "other", EnvironmentID: "dev", ApplicationID: "component"}}
	s.SetRuntimeObservers([]delivery.RuntimeSnapshotObserver{a, b, unrelated})
	exec(t, s, domain.Command{Action: "refresh_environment_runtime", ID: "inventory", ProductID: "p", Data: map[string]any{"environment_id": "dev"}})
	if a.calls != 0 {
		t.Fatal("request blocked on network")
	}
	if _, err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if unrelated.calls != 0 || a.calls != 1 || b.calls != 1 || len(m.state.RuntimeSnapshots) != 2 {
		t.Fatalf("scope/multiple workload mismatch: %+v", m.state.RuntimeSnapshots)
	}
	if operationByID(&m.state, "inventory").Status != "SUCCEEDED" {
		t.Fatal("observation operation not complete")
	}
	if len(m.state.RuntimeObservations) != 0 || len(p.applyCounts) != 0 {
		t.Fatal("inventory must never apply GitOps or certify deployment")
	}
	for _, v := range m.state.RuntimeSnapshots {
		if v.Comparison != "MATCH" {
			t.Fatalf("digest match: %+v", v)
		}
	}
	v, err := s.Query(context.Background(), "get_environment_state", "dev")
	if err != nil {
		t.Fatal(err)
	}
	if len(v.(map[string]any)["runtime_snapshots"].([]domain.RuntimeSnapshot)) != 2 {
		t.Fatal("query missing evidence")
	}
	// Fresh read failure is retained rather than showing old health as current.
	a.result.Errors = []string{"Kubernetes read failed"}
	a.result.Health = "UNKNOWN"
	exec(t, s, domain.Command{Action: "refresh_environment_runtime", ID: "inventory-failure", ProductID: "p", Data: map[string]any{"environment_id": "dev"}})
	if _, err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(m.state.RuntimeSnapshots) != 4 || operationByID(&m.state, "inventory-failure").Status != "FAILED" {
		t.Fatal("failed evidence lost")
	}
}
func TestRuntimeComparisonNeverCertifiesTags(t *testing.T) {
	d := "sha256:" + strings.Repeat("a", 64)
	e := domain.RuntimeExpected{ImageRepository: "ghcr.io/a/b", Value: "v1"}
	a := delivery.RuntimeSnapshot{WorkloadImage: "ghcr.io/a/b:v1", Pods: []delivery.RuntimePod{{Image: "ghcr.io/a/b:v1", ImageID: d}}}
	if runtimeComparison(e, a) != "UNKNOWN" {
		t.Fatal("mutable tag treated as immutable match")
	}
	a.WorkloadImage = "ghcr.io/a/b:v2"
	if runtimeComparison(e, a) != "DRIFT" {
		t.Fatal("different desired workload not flagged")
	}
	e.Value = d
	a.WorkloadImage = "ghcr.io/a/b@" + d
	a.Pods[0].Image = "ghcr.io/a/b@" + d
	if runtimeComparison(e, a) != "MATCH" {
		t.Fatal("exact digest not matched")
	}
	a.Pods[0].ImageID = "sha256:" + strings.Repeat("b", 64)
	if runtimeComparison(e, a) != "UNKNOWN" {
		t.Fatal("platform digest cannot prove drift")
	}
}
func TestRuntimeRefreshScopeValidation(t *testing.T) {
	s, _, _ := flowFixture(t)
	if _, err := s.Execute(context.Background(), domain.Command{Action: "refresh_environment_runtime", Actor: "test", ProductID: "other", Data: map[string]any{"environment_id": "dev"}}); err == nil {
		t.Fatal("cross-product environment accepted")
	}
}

func TestRuntimeComparisonTaggedDigestAndStalePods(t *testing.T) {
	d := "sha256:" + strings.Repeat("a", 64)
	expected := domain.RuntimeExpected{ImageRepository: "ghcr.io/a/b", Value: d}
	actual := delivery.RuntimeSnapshot{WorkloadImage: "ghcr.io/a/b:v1@" + d, Pods: []delivery.RuntimePod{{Image: "ghcr.io/a/b:v1@" + d, ImageID: d}}}
	if runtimeComparison(expected, actual) != "MATCH" {
		t.Fatal("tagged immutable reference mismatched")
	}
	expected.Value = "v2"
	actual.WorkloadImage = "ghcr.io/a/b:v2"
	actual.Pods[0].Image = "ghcr.io/a/b:v1"
	if runtimeComparison(expected, actual) != "DRIFT" {
		t.Fatal("old tag pod not detected")
	}
}

func TestRuntimeReferenceComparisonSeparateFromDigest(t *testing.T) {
	d := "sha256:" + strings.Repeat("a", 64)
	expected := domain.RuntimeExpected{ImageRepository: "ghcr.io/a/b", Value: "v1"}
	actual := delivery.RuntimeSnapshot{WorkloadImage: "ghcr.io/a/b:v1", Pods: []delivery.RuntimePod{{Image: "ghcr.io/a/b:v1", ImageID: d}}}
	if runtimeReferenceComparison(expected, actual) != "MATCH" || runtimeComparison(expected, actual) != "UNKNOWN" {
		t.Fatal("tag reference matching conflated with immutable verification")
	}
	actual.Pods[0].Image = "ghcr.io/a/b:v0"
	if runtimeReferenceComparison(expected, actual) != "DRIFT" {
		t.Fatal("stale pod reference not detected")
	}
	expected.Value = d
	actual.WorkloadImage = "ghcr.io/a/b:v1@" + d
	actual.Pods[0].Image = "ghcr.io/a/b@" + d
	if runtimeReferenceComparison(expected, actual) != "MATCH" {
		t.Fatal("canonical digest references must match irrespective of accompanying tag")
	}
	actual.Errors = []string{"pod list incomplete"}
	if runtimeReferenceComparison(expected, actual) != "UNKNOWN" {
		t.Fatal("partial read cannot certify reference match")
	}
}
