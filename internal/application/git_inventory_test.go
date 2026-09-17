package application

import (
	"context"
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

func TestUnlinkedBranchDriftIsNotActionableAttention(t *testing.T) {
	s, m, _ := externalFixture(t)
	now := time.Now().UTC()
	m.state.GitObservations = []domain.GitObservation{{
		Meta:         domain.Meta{ID: "stale", ProductID: "p", CreatedAt: now},
		RepositoryID: "source", Branch: "codex/old-work", MainCommit: "base", HeadCommit: "head",
		Ahead: 3, Behind: 12, Status: "diverged", ObservedAt: now,
	}}
	value, err := s.Query(context.Background(), "list_attention", "p")
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range value.([]map[string]any) {
		if item["reason"] == "BRANCH_BEHIND" || item["reason"] == "BRANCH_DIVERGED" {
			t.Fatalf("unlinked stale branch became actionable: %v", item)
		}
	}
}

func TestActiveIntegrationBranchDriftRemainsActionableAttention(t *testing.T) {
	s, m, _ := externalFixture(t)
	now := time.Now().UTC()
	in := integration(&m.state, "i")
	in.Status = "working"
	in.Owner = "agent/test"
	m.state.GitObservations = []domain.GitObservation{{
		Meta:          domain.Meta{ID: "active-drift", ProductID: "p", FeatureID: "f", CreatedAt: now},
		IntegrationID: "i", RepositoryID: "source", Branch: "codex/active-work", MainCommit: "base", HeadCommit: "head",
		Ahead: 2, Behind: 4, Status: "diverged", ObservedAt: now,
	}}
	value, err := s.Query(context.Background(), "list_attention", "p")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range value.([]map[string]any) {
		if item["id"] == "active-drift" && item["reason"] == "BRANCH_DIVERGED" {
			found = true
		}
	}
	if !found {
		t.Fatalf("active integration drift disappeared from attention: %v", value)
	}
}
