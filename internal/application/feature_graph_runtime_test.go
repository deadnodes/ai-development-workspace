package application

import (
	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
	"strings"
	"testing"
	"time"
)

func TestGraphRuntimeMatchesAndIsolation(t *testing.T) {
	sha := "abcdef1234567890123456789012345678901234"
	st := domain.State{Applications: []domain.Application{{Meta: domain.Meta{ID: "app", ProductID: "p"}, RepositoryID: "repo", Name: "app"}}}
	s := domain.RuntimeSnapshot{Meta: domain.Meta{ID: "s", ProductID: "p"}, ApplicationID: "app", EnvironmentID: "dev", Runtime: delivery.RuntimeSnapshot{ObservedAt: time.Now(), Deployment: "app", Container: "app", Health: "HEALTHY", Pods: []delivery.RuntimePod{{Name: "pod", Phase: "Running", Image: "ghcr.io/org/app:dev-abcdef12", ImageID: "ghcr.io/org/app@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}}, ActionsRuns: []delivery.ActionsRun{{HeadSHA: sha, HeadBranch: "dev"}}}
	st.RuntimeSnapshots = []domain.RuntimeSnapshot{s}
	g := FeatureGraph{Feature: domain.Feature{Meta: domain.Meta{ProductID: "p"}}, Repositories: []GraphRepository{{Repository: domain.Repository{Meta: domain.Meta{ID: "repo", ProductID: "p"}}, Branches: []GraphBranch{{Name: "dev", HeadCommit: sha}}}}}
	attachGraphRuntime(&st, &g)
	if len(g.Repositories[0].Runtime) != 1 || len(g.Repositories[0].Runtime[0].Matches) != 2 || g.Repositories[0].Runtime[0].Matches[0].Evidence != "tag_sha_hint" {
		t.Fatalf("%+v", g.Repositories[0].Runtime)
	}
	// Short SHA collisions must remain unknown.
	g.Repositories[0].Commits = []string{"abcdef12" + strings.Repeat("9", 32)}
	attachGraphRuntime(&st, &g)
	if len(g.Repositories[0].Runtime[0].Matches) != 0 {
		t.Fatal("ambiguous prefix matched")
	}
	// Exact digest provenance is stronger, independent of the mutable tag.
	st.RuntimeSnapshots[0].ArtifactMatches = []domain.DeliveryArtifact{{Meta: domain.Meta{ProductID: "p"}, ApplicationID: "app", RepositoryID: "repo", SourceCommit: sha, ImageRepository: "ghcr.io/org/app", Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}
	attachGraphRuntime(&st, &g)
	if g.Repositories[0].Runtime[0].Matches[0].Evidence != "digest_provenance" {
		t.Fatal("digest not matched")
	}
	// A newer failed observation must replace old matches, not revive stale evidence.
	newer := s
	newer.ID = "new"
	newer.Runtime.ObservedAt = s.Runtime.ObservedAt.Add(time.Minute)
	newer.Runtime.Errors = []string{"unavailable"}
	st.RuntimeSnapshots = append(st.RuntimeSnapshots, newer)
	attachGraphRuntime(&st, &g)
	if len(g.Repositories[0].Runtime[0].Matches) != 0 {
		t.Fatal("old match survives failed read")
	}
	st.RuntimeSnapshots[0].ProductID = "other"
	st.RuntimeSnapshots[1].ProductID = "other"
	attachGraphRuntime(&st, &g)
	if len(g.Repositories[0].Runtime) != 0 {
		t.Fatal("cross-product runtime leak")
	}
}
