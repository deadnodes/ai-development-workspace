package domain

import (
	"releasecontrol/internal/delivery"
	"testing"
	"time"
)

func TestRetentionProtectsCurrentPreviousAndNewest(t *testing.T) {
	now := time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC)
	st := State{Applications: []Application{{Meta: Meta{ID: "app", ProductID: "p"}, Retention: &ArtifactRetention{KeepLast: 1, KeepCurrent: true, KeepPrevious: 1}}}}
	for n, d := range []string{"old", "previous", "current", "new"} {
		st.DeliveryArtifacts = append(st.DeliveryArtifacts, DeliveryArtifact{Meta: Meta{ID: d, ProductID: "p", CreatedAt: now.Add(time.Duration(n) * time.Hour)}, ApplicationID: "app", ImageRepository: "image", Digest: d, Availability: "PRESENT"})
	}
	for n, d := range []string{"old", "previous", "current"} {
		st.Operations = append(st.Operations, ExternalOperation{Meta: Meta{ProductID: "p", CreatedAt: now.Add(time.Duration(n) * time.Hour)}, ApplicationID: "app", EnvironmentID: "dev", Artifact: &delivery.ArtifactResult{Digest: d}, GitOpsResult: &delivery.GitOpsResult{}, Snapshot: &DeliverySnapshot{Build: ComponentBuild{ImageRepository: "image"}}})
	}
	got := EvaluateRetention(st, "p")
	if len(got) != 4 {
		t.Fatal(got)
	}
	for _, e := range got {
		if e.ExpectedAvailable != (e.Digest != "old") {
			t.Fatal(e)
		}
	}
	if len(EvaluateRetention(st, "other")) != 0 {
		t.Fatal("cross product leak")
	}
	// Latest registry evidence of the same immutable image wins, no duplicate quota slot.
	a := st.DeliveryArtifacts[3]
	a.ID = "new-missing"
	a.Availability = "MISSING"
	a.ObservedAt = now.Add(10 * time.Hour)
	st.DeliveryArtifacts = append(st.DeliveryArtifacts, a)
	got = EvaluateRetention(st, "p")
	if len(got) != 4 {
		t.Fatal(got)
	}
	for _, e := range got {
		if e.Digest == "new" && (e.Availability != "MISSING" || !e.ExpectedAvailable) {
			t.Fatal(e)
		}
	}
}
