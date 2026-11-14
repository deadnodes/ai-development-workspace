package domain

import (
	"time"
)

// RuntimeObservedAt consumes observation-ID keyed timestamps from BindRuntimeFreshness.
// It exposes liveness without replacing historical provenance.
func RuntimeObservedAt(r RuntimeObservation, freshness map[string]time.Time) time.Time {
	if at := freshness[r.ID]; at.After(r.CreatedAt) {
		return at
	}
	return r.CreatedAt
}

type RuntimeFreshness struct {
	ObservationID string    `json:"observation_id"`
	ObservedAt    time.Time `json:"observed_at"`
	Healthy       bool      `json:"healthy"`
}

func BindRuntimeFreshness(rows []RuntimeObservation, freshness map[string]RuntimeFreshness) map[string]time.Time {
	latest := map[string]RuntimeObservation{}
	for _, r := range rows {
		old, ok := latest[r.OperationID]
		if !ok || r.CreatedAt.After(old.CreatedAt) || r.CreatedAt.Equal(old.CreatedAt) && r.ID > old.ID {
			latest[r.OperationID] = r
		}
	}
	out := map[string]time.Time{}
	for op, r := range latest {
		if f, ok := freshness[op]; ok && f.ObservationID == r.ID && f.Healthy == r.Healthy {
			out[r.ID] = f.ObservedAt
		}
	}
	return out
}

// runtimeObservationRoots retains the roots used by retention.go: every
// attested digest remains represented, reconciliation witnesses remain, and
// release-time healthy evidence is never replaced with a later heartbeat.
type runtimeObservationIndex struct {
	keep map[string]bool
	rows map[string]RuntimeObservation
}

func runtimeObservationRoots(st *State) runtimeObservationIndex {
	rows := map[string]RuntimeObservation{}
	keep := map[string]bool{}
	latest := map[string]RuntimeObservation{}
	earliest := map[string]RuntimeObservation{}
	ops := map[string]ExternalOperation{}
	for _, o := range st.Operations {
		ops[o.ID] = o
	}
	newer := func(a, b RuntimeObservation) bool {
		return a.CreatedAt.After(b.CreatedAt) || a.CreatedAt.Equal(b.CreatedAt) && a.ID > b.ID
	}
	for _, r := range st.RuntimeObservations {
		rows[r.ID] = r
		op := ops[r.OperationID]
		keys := []string{"operation\x00" + r.OperationID, "target\x00" + r.ProductID + "\x00" + r.EnvironmentID + "\x00" + op.ApplicationID}
		for _, key := range keys {
			old, ok := latest[key]
			if !ok || newer(r, old) {
				latest[key] = r
			}
		}
		// Runtime attestations are image roots even when unhealthy or superseded.
		key := r.ProductID + "\x00" + r.OperationID + "\x00" + r.ArtifactDigest
		old, ok := earliest[key]
		if !ok || newer(old, r) {
			earliest[key] = r
		}
		if runtimeReconciliationWitness(r, op) {
			key = "reconciled\x00" + r.OperationID
			old, ok := earliest[key]
			if !ok || newer(old, r) {
				earliest[key] = r
			}
		}
	}
	for _, r := range latest {
		keep[r.ID] = true
	}
	for _, r := range earliest {
		keep[r.ID] = true
	}
	// Index healthy witnesses once. A release only needs the earliest matching
	// witness: if it is newer than the release, no matching witness existed yet.
	type witnessKey struct{ product, environment, operation, digest, commit string }
	witnesses := map[witnessKey]RuntimeObservation{}
	for _, r := range st.RuntimeObservations {
		if !r.Healthy || r.CreatedAt.IsZero() {
			continue
		}
		k := witnessKey{r.ProductID, r.EnvironmentID, r.OperationID, r.ArtifactDigest, r.GitOpsCommit}
		if old, ok := witnesses[k]; !ok || newer(old, r) {
			witnesses[k] = r
		}
	}
	type digestKey struct{ product, digest string }
	pins := map[digestKey]bool{}
	for _, p := range st.ArtifactPins {
		if p.ReleasedAt == nil {
			pins[digestKey{p.ProductID, p.Digest}] = true
		}
	}
	type releaseKey struct{ product, environment, app, digest string }
	releases := map[releaseKey]time.Time{}
	for _, release := range st.Releases {
		for app, digest := range release.ArtifactDigests {
			k := releaseKey{release.ProductID, release.EnvironmentID, app, digest}
			if release.CreatedAt.After(releases[k]) {
				releases[k] = release.CreatedAt
			}
		}
	}
	for _, r := range witnesses {
		op := ops[r.OperationID]
		at, ok := releases[releaseKey{r.ProductID, r.EnvironmentID, op.ApplicationID, r.ArtifactDigest}]
		if ok && op.GitOpsResult != nil && r.GitOpsCommit == op.GitOpsResult.CommitSHA && r.EnvironmentID == op.EnvironmentID && !r.CreatedAt.After(at) {
			keep[r.ID] = true
		}
	}

	for _, r := range st.RuntimeObservations {
		if pins[digestKey{r.ProductID, r.ArtifactDigest}] {
			keep[r.ID] = true
		}
	}
	return runtimeObservationIndex{keep: keep, rows: rows}
}

// PrunableRuntimeObservations selects a bounded batch in stored order in O(n + refs).
func PrunableRuntimeObservations(st *State, cutoff time.Time, limit int) map[string]bool {
	keep := runtimeObservationRoots(st)
	out := map[string]bool{}
	for _, r := range st.RuntimeObservations {
		if len(out) >= limit {
			break
		}
		if r.ID != "" && !keep.keep[r.ID] && r.CreatedAt.Before(cutoff) {
			out[r.ID] = true
		}
	}
	return out
}

// RevalidateRuntimeObservationCandidates never expands the borrowed-view batch.
func RevalidateRuntimeObservationCandidates(st *State, candidates map[string]bool, cutoff time.Time) map[string]bool {
	keep := runtimeObservationRoots(st)
	out := map[string]bool{}
	for candidate := range candidates {
		r, present := keep.rows[candidate]
		if present && !keep.keep[r.ID] && r.CreatedAt.Before(cutoff) {
			out[r.ID] = true
		}
	}
	return out
}

// Shared with the protected-root inventory: pruning cannot erase these proofs.
func runtimeReconciliationWitness(r RuntimeObservation, o ExternalOperation) bool {
	return r.ProductID == o.ProductID && r.OperationID == o.ID && r.EnvironmentID == o.EnvironmentID && r.Healthy && o.GitOpsResult != nil && o.Artifact != nil && r.GitOpsCommit == o.GitOpsResult.CommitSHA && r.ArtifactDigest == o.Artifact.Digest && !r.CreatedAt.Before(o.UpdatedAt) && !r.CreatedAt.IsZero()
}
func runtimeReleaseWitness(r RuntimeObservation, o ExternalOperation, release Release) bool {
	return r.ProductID == release.ProductID && r.OperationID == o.ID && r.EnvironmentID == o.EnvironmentID && r.Healthy && o.GitOpsResult != nil && r.GitOpsCommit == o.GitOpsResult.CommitSHA && r.ArtifactDigest == release.ArtifactDigests[o.ApplicationID] && !r.CreatedAt.IsZero() && !r.CreatedAt.After(release.CreatedAt)
}
