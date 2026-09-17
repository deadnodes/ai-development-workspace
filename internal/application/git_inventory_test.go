package application

import (
	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
	"testing"
	"time"
)

func TestUnlinkedGitCommitsRemainAttentionUntilLinked(t *testing.T) {
	now := time.Now().UTC()
	state := domain.EmptyState()
	state.Products = []domain.Product{{Meta: domain.Meta{ID: "p"}, Name: "Product"}}
	state.Repositories = []domain.Repository{{Meta: domain.Meta{ID: "repo", ProductID: "p"}, Name: "service"}}
	state.GitObservations = []domain.GitObservation{{Meta: domain.Meta{ID: "obs", ProductID: "p"}, RepositoryID: "repo", Branch: "codex/work", HeadCommit: "head", MainCommit: "base", ObservedAt: now, Commits: []delivery.CommitInfo{{SHA: "head", Message: "Unlinked work", URL: "https://example.test/commit/head"}}}}
	items := unlinkedGitCommits(state, "p")
	if len(items) != 1 || items[0]["reason"] != "UNLINKED_GIT_COMMIT" {
		t.Fatalf("items=%v", items)
	}
	state.Integrations = []domain.Integration{{Meta: domain.Meta{ID: "i", ProductID: "p"}, Commits: []domain.Commit{{RepositoryID: "repo", SHA: "head"}}}}
	if got := unlinkedGitCommits(state, "p"); len(got) != 0 {
		t.Fatalf("linked commit still requires attention: %v", got)
	}
}
