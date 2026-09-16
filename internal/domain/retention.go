package domain

import (
	"sort"
	"time"
)

// ArtifactRetention is advisory. No bytes or provenance records are deleted.
type ArtifactRetention struct {
	KeepLast     int  `json:"keep_last"`
	KeepCurrent  bool `json:"keep_current"`
	KeepPrevious int  `json:"keep_previous"`
}
type RetentionEvaluation struct {
	ArtifactID        string   `json:"artifact_id"`
	ApplicationID     string   `json:"application_id"`
	Digest            string   `json:"digest"`
	Availability      string   `json:"availability"`
	ExpectedAvailable bool     `json:"expected_available"`
	Reasons           []string `json:"reasons"`
}

func EvaluateRetention(st State, product string) []RetentionEvaluation {
	out := []RetentionEvaluation{}
	for _, app := range st.Applications {
		if app.ProductID != product || app.Retention == nil {
			continue
		}
		p := app.Retention
		// Multiple observations of one immutable image share one retention identity.
		latest := map[string]DeliveryArtifact{}
		firstCreated := map[string]time.Time{}
		key := func(a DeliveryArtifact) string { return a.ImageRepository + "@" + a.Digest }
		for _, a := range st.DeliveryArtifacts {
			if a.ProductID == product && a.ApplicationID == app.ID && a.Digest != "" {
				if first, exists := firstCreated[key(a)]; !exists || a.CreatedAt.Before(first) {
					firstCreated[key(a)] = a.CreatedAt
				}
				old, ok := latest[key(a)]
				if !ok || a.ObservedAt.After(old.ObservedAt) || (a.ObservedAt.Equal(old.ObservedAt) && a.ID > old.ID) {
					latest[key(a)] = a
				}
			}
		}
		artifacts := []DeliveryArtifact{}
		for _, a := range latest {
			a.CreatedAt = firstCreated[key(a)]
			artifacts = append(artifacts, a)
		}
		sort.Slice(artifacts, func(i, j int) bool {
			if artifacts[i].CreatedAt.Equal(artifacts[j].CreatedAt) {
				return artifacts[i].ID > artifacts[j].ID
			}
			return artifacts[i].CreatedAt.After(artifacts[j].CreatedAt)
		})
		protected := map[string][]string{}
		for n, a := range artifacts {
			if n < p.KeepLast {
				protected[key(a)] = append(protected[key(a)], "keep_last")
			}
		}
		ops := append([]ExternalOperation(nil), st.Operations...)
		sort.SliceStable(ops, func(i, j int) bool { return ops[i].CreatedAt.After(ops[j].CreatedAt) })
		seen := map[string]map[string]bool{}
		for _, op := range ops {
			if op.ProductID != product || op.ApplicationID != app.ID || op.GitOpsResult == nil || op.Artifact == nil || op.Snapshot == nil || op.EnvironmentID == "" {
				continue
			}
			k := op.Snapshot.Build.ImageRepository + "@" + op.Artifact.Digest
			if seen[op.EnvironmentID] == nil {
				seen[op.EnvironmentID] = map[string]bool{}
			}
			if seen[op.EnvironmentID][k] {
				continue
			}
			n := len(seen[op.EnvironmentID])
			seen[op.EnvironmentID][k] = true
			if (n == 0 && p.KeepCurrent) || (n > 0 && n <= p.KeepPrevious) {
				protected[k] = append(protected[k], "environment:"+op.EnvironmentID)
			}
		}
		for _, a := range artifacts {
			reasons := protected[key(a)]
			out = append(out, RetentionEvaluation{a.ID, app.ID, a.Digest, a.Availability, len(reasons) > 0, reasons})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ArtifactID < out[j].ArtifactID })
	return out
}
