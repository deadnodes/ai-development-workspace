package application

import (
	"context"
	"releasecontrol/internal/domain"
	"strings"
	"testing"
	"time"
)

func TestFlowRuntimeRequiresEveryTarget(t *testing.T) {
	for _, target := range []string{"", "A"} {
		t.Run("target="+target, func(t *testing.T) {
			s, m, _ := flowFixture(t)
			flowPlan(t, s, "plan", "dev", "rev")
			id := flowStart(t, s, m, "plan")
			flowTickUntil(t, s, m, id, flowApplied)
			parent := operationByID(&m.state, id)
			for _, childID := range parent.ChildIDs {
				child := operationByID(&m.state, childID)
				for i, target := range []string{target, "B"} {
					exec(t, s, domain.Command{Action: "record_runtime_observation", ID: childID, Data: map[string]any{"target": target, "environment_id": "dev", "gitops_commit": child.GitOpsResult.CommitSHA, "artifact_digest": child.Artifact.Digest, "healthy": i == 1, "details": "target evidence"}})
				}
			}
			value, err := s.Query(context.Background(), "list_attention", "p")
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, item := range value.([]map[string]any) {
				if item["id"] == parent.ChildIDs[0] {
					found = true
				}
			}
			if !found {
				t.Fatal("unhealthy target hidden from attention")
			}
			// Exercise the transactional finalization recheck independently of WAIT_RUNTIME.
			parent = operationByID(&m.state, id)
			parent.LeaseOwner = "test"
			if err := s.finishFlowRelease(context.Background(), *parent, "test"); err == nil || !strings.Contains(err.Error(), "runtime evidence changed") {
				t.Fatalf("expected unhealthy target rejection, got %v", err)
			}
			if len(m.state.Releases) != 0 {
				t.Fatal("release recorded")
			}
			flowTickUntil(t, s, m, id, func(o domain.ExternalOperation) bool { return terminalOperation(o.Status) })
			if operationByID(&m.state, id).DeploymentState == "DEPLOYED" {
				t.Fatal("unhealthy target deployed")
			}
		})
	}
}

func TestNotesBaselinesEveryTargetHealthAndFreshness(t *testing.T) {
	for _, mode := range []string{"unhealthy", "stale", "fresh", "recovered"} {
		t.Run(mode, func(t *testing.T) {
			_, m, _, _ := releaseNotesFixture(t)
			a := m.state.RuntimeObservations[0]
			a.Target = "A"
			a.ID = "a"
			a.CreatedAt = time.Now().Add(-time.Hour)
			a.Healthy = mode != "unhealthy"
			b := a
			b.Target = "B"
			b.ID = "b"
			b.CreatedAt = time.Now()
			b.Healthy = true
			m.state.RuntimeObservations = []domain.RuntimeObservation{b, a} // Ordering must not choose health.
			fresh := map[string]time.Time{"b": time.Now()}
			if mode != "stale" {
				fresh["a"] = time.Now()
			}
			if mode == "recovered" {
				c := a
				c.ID = "c"
				c.CreatedAt = time.Now()
				c.Healthy = true
				m.state.RuntimeObservations[1].Healthy = false
				m.state.RuntimeObservations = append(m.state.RuntimeObservations, c)
			}
			_, _, err := notesBaselines(&m.state, "p", "env", intelligenceRequest{PreviousReleaseID: "previous"}, fresh)
			if (err == nil) != (mode == "fresh" || mode == "recovered") {
				t.Fatalf("unexpected baseline result: %v", err)
			}
		})
	}
}
