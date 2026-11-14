package domain

import (
	"releasecontrol/internal/delivery"
	"slices"
	"testing"
	"time"
)

func TestRuntimeObservationRetentionReferences(t *testing.T) {
	now := time.Now().UTC()
	old := now.Add(-10 * 24 * time.Hour)
	st := EmptyState()
	st.Operations = []ExternalOperation{{Meta: Meta{ID: "op", ProductID: "p", UpdatedAt: old.Add(time.Hour)}, ApplicationID: "app", EnvironmentID: "env", GitOpsResult: &delivery.GitOpsResult{CommitSHA: "commit"}, Artifact: &delivery.ArtifactResult{Digest: "digest"}}, {Meta: Meta{ID: "other", ProductID: "p"}, ApplicationID: "app", EnvironmentID: "env"}}
	row := func(id, operation, digest string, at time.Time, healthy bool) RuntimeObservation {
		return RuntimeObservation{Meta: Meta{ID: id, ProductID: "p", CreatedAt: at}, OperationID: operation, EnvironmentID: "env", ArtifactDigest: digest, GitOpsCommit: "commit", Healthy: healthy}
	}
	st.RuntimeObservations = []RuntimeObservation{row("root", "op", "digest", old, false), row("prunable", "op", "digest", old.Add(time.Minute), false), row("reconcile", "op", "digest", old.Add(2*time.Hour), true), row("latest-op", "op", "digest", old.Add(3*time.Hour), true), row("latest-target", "other", "other", old.Add(4*time.Hour), false), row("pinned-root", "op", "pinned", old, false), row("pinned-duplicate", "op", "pinned", old.Add(time.Minute), false)}
	st.ArtifactPins = []ArtifactPin{{Meta: Meta{ID: "pin", ProductID: "p"}, Digest: "pinned"}}
	st.Releases = []Release{{Meta: Meta{ID: "release", ProductID: "p", CreatedAt: old.Add(150 * time.Minute)}, EnvironmentID: "env", ArtifactDigests: map[string]string{"app": "digest"}, GitOpsCommits: map[string]string{"app": "commit"}}}
	before := EvaluateRetention(st, "p", now, nil)
	after := EvaluateRetention(st, "p", now, nil, map[string]time.Time{"latest-op": now, "latest-target": now})
	if !slices.Contains(before.Blockers, "runtime_attestation_stale_or_unknown") || !slices.Contains(after.Blockers, "runtime_attestation_stale_or_unknown") {
		t.Fatal("historical attestations must remain stale")
	}
	remove := PrunableRuntimeObservations(&st, now.Add(-7*24*time.Hour), 5000)
	if len(remove) != 1 || !remove["prunable"] {
		t.Fatalf("lost references or latest: %v", remove)
	}
	r := st.RuntimeObservations[0]
	if got := RuntimeObservedAt(r, map[string]time.Time{r.ID: now}); !got.Equal(now) {
		t.Fatal(got)
	}
	if got := RuntimeObservedAt(r, map[string]time.Time{r.ID: old.Add(-time.Hour)}); !got.Equal(old) {
		t.Fatal("freshness moved backwards")
	}
}

func TestRuntimeFreshnessOnlyLatestMatchingHealth(t *testing.T) {
	now := time.Now().UTC()
	rows := []RuntimeObservation{
		{Meta: Meta{ID: "healthy", CreatedAt: now.Add(-2 * time.Hour)}, OperationID: "op", Healthy: true},
		{Meta: Meta{ID: "unhealthy", CreatedAt: now.Add(-time.Hour)}, OperationID: "op", Healthy: false},
	}
	for _, healthy := range []bool{true, false} {
		bound := BindRuntimeFreshness(rows, map[string]RuntimeFreshness{"op": {ObservationID: "unhealthy", ObservedAt: now, Healthy: healthy}})
		if got := RuntimeObservedAt(rows[0], bound); !got.Equal(rows[0].CreatedAt) {
			t.Fatal("historical healthy observation marked fresh")
		}
		want := rows[1].CreatedAt
		if !healthy {
			want = now
		}
		if got := RuntimeObservedAt(rows[1], bound); !got.Equal(want) {
			t.Fatalf("health binding: %v want %v", got, want)
		}
	}
}

func TestRuntimeCandidateRevalidation(t *testing.T) {
	now := time.Now().UTC()
	old := now.Add(-10 * 24 * time.Hour)
	for _, change := range []string{"missing", "newer", "latest", "pin", "release"} {
		t.Run(change, func(t *testing.T) {
			st := EmptyState()
			st.Operations = []ExternalOperation{{Meta: Meta{ID: "op", ProductID: "p"}, ApplicationID: "app", EnvironmentID: "env", GitOpsResult: &delivery.GitOpsResult{CommitSHA: "commit"}}}
			for i, id := range []string{"root", "candidate", "latest"} {
				st.RuntimeObservations = append(st.RuntimeObservations, RuntimeObservation{Meta: Meta{ID: id, ProductID: "p", CreatedAt: old.Add(time.Duration(i) * time.Minute)}, OperationID: "op", EnvironmentID: "env", ArtifactDigest: "digest", GitOpsCommit: "commit", Healthy: i > 0})
			}
			cutoff := now.Add(-7 * 24 * time.Hour)
			candidates := PrunableRuntimeObservations(&st, cutoff, 5000)
			if !candidates["candidate"] {
				t.Fatal("fixture candidate not selected")
			}
			switch change {
			case "missing":
				st.RuntimeObservations = append(st.RuntimeObservations[:1], st.RuntimeObservations[2:]...)
			case "newer":
				st.RuntimeObservations[1].CreatedAt = now
			case "latest":
				st.RuntimeObservations = st.RuntimeObservations[:2]
			case "pin":
				st.ArtifactPins = []ArtifactPin{{Meta: Meta{ProductID: "p"}, Digest: "digest"}}
			case "release":
				st.Releases = []Release{{Meta: Meta{ID: "release", ProductID: "p", CreatedAt: old.Add(90 * time.Second)}, EnvironmentID: "env", ArtifactDigests: map[string]string{"app": "digest"}}}
			}
			if got := RevalidateRuntimeObservationCandidates(&st, candidates, cutoff); len(got) != 0 {
				t.Fatalf("deleted protected candidate: %v", got)
			}
		})
	}
}

func TestRuntimeFreshnessRejectsSupersededIdentity(t *testing.T) {
	now := time.Now().UTC()
	rows := []RuntimeObservation{{Meta: Meta{ID: "old", CreatedAt: now.Add(-time.Hour)}, OperationID: "op", Healthy: true}, {Meta: Meta{ID: "latest", CreatedAt: now.Add(-time.Minute)}, OperationID: "op", Healthy: true}}
	bound := BindRuntimeFreshness(rows, map[string]RuntimeFreshness{"op": {ObservationID: "old", ObservedAt: now, Healthy: true}})
	for _, r := range rows {
		if !RuntimeObservedAt(r, bound).Equal(r.CreatedAt) {
			t.Fatal("superseded heartbeat reused")
		}
	}
}
