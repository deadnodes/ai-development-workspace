package application

import (
	"context"
	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
	"testing"
	"time"
)

type reviewFixture struct {
	delivery.Provider
	review delivery.PullRequestReview
	err    error
}

func (r *reviewFixture) PullRequestReview(context.Context, delivery.Connection, string, int) (delivery.PullRequestReview, error) {
	return r.review, r.err
}
func TestReviewImportDedupResolutionAndContext(t *testing.T) {
	s, m := fixture(t)
	ctx := context.Background()
	m.state.Repositories = append(m.state.Repositories, domain.Repository{Meta: domain.Meta{ID: "repo", ProductID: "p"}})
	m.state.RepositoryBindings = append(m.state.RepositoryBindings, domain.RepositoryBinding{Meta: domain.Meta{ID: "binding", ProductID: "p"}, RepositoryID: "repo", ConnectionID: "conn", Role: "SOURCE", FullName: "example/source"})
	m.state.GitHubConnections = append(m.state.GitHubConnections, domain.GitHubConnection{Meta: domain.Meta{ID: "conn"}})
	m.state.ConnectionGrants = append(m.state.ConnectionGrants, domain.ConnectionGrant{Meta: domain.Meta{ID: "grant", ProductID: "p"}, ConnectionID: "conn"})
	provider := &reviewFixture{review: delivery.PullRequestReview{Number: 5, Head: "feature/work", Base: "dev", Comments: []delivery.ReviewComment{{ID: 11, Kind: "inline", Author: "chatgpt-codex-connector[bot]", Body: "**![P2 Badge](https://example.test/badge) Fix duplicate poll**", Commit: "abc", UpdatedAt: time.Now()}, {ID: 12, Kind: "inline", Body: "[P3] Optional style"}}}}
	s.provider = provider
	m.state.Integrations[0].Branches = []domain.Branch{{RepositoryID: "repo", Name: "feature/work"}}
	sync := func() {
		t.Helper()
		if _, err := s.SyncReview(ctx, "agent", "i", "repo", 5, nil, false); err != nil {
			t.Fatal(err)
		}
	}
	sync()
	sync()
	if len(m.state.Findings) != 1 || len(m.state.ReviewSyncs) != 2 {
		t.Fatal("duplicate finding or missing history")
	}
	finding := m.state.Findings[0]
	if finding.ResultID != "" || finding.ReviewSource.Priority != "P2" {
		t.Fatal("review faked verification evidence")
	}
	reject(t, s, domain.Command{Action: "complete_integration", IntegrationID: "i"})
	exec(t, s, domain.Command{Action: "resolve_finding", ID: finding.ID, Data: map[string]any{"body": "Fixed and verified", "commit": "fix-sha"}})
	sync()
	if m.state.Findings[0].Status != "resolved" {
		t.Fatal("unchanged comment reopened fix")
	}
	provider.review.Comments[0].Body = "[P1] Updated issue"
	provider.review.Comments[0].UpdatedAt = time.Now().Add(time.Minute)
	sync()
	if m.state.Findings[0].Status != "open" || m.state.Findings[0].Severity != "P1" {
		t.Fatal("changed evidence not reopened")
	}
	attention, err := s.Query(ctx, "list_attention", "p")
	if err != nil || len(attention.([]map[string]any)) != 1 {
		t.Fatal("MCP attention missing")
	}
	resumed, err := s.Resume(ctx, "f")
	if err != nil || len(resumed.(map[string]any)["review_syncs"].([]domain.ReviewSync)) != 4 {
		t.Fatal("context missing review history")
	}
	if _, err = s.SyncReview(ctx, "agent", "i", "other", 5, nil, false); err == nil {
		t.Fatal("foreign repo allowed")
	}
}
func TestReviewPriorities(t *testing.T) {
	for body, want := range map[string]string{"![P1 Badge](url) Fix": "P1", "[P2] Fix": "P2", "[P3] Style": "", "ordinary priority P2 discussion": "", "[P2] second\n[P1] first": "P1"} {
		if priority(body) != want {
			t.Fatalf("priority %q", body)
		}
	}
}

func TestSharedPRRequiresExplicitAttribution(t *testing.T) {
	s, m := fixture(t)
	m.state.Repositories = append(m.state.Repositories, domain.Repository{Meta: domain.Meta{ID: "repo", ProductID: "p"}})
	m.state.RepositoryBindings = append(m.state.RepositoryBindings, domain.RepositoryBinding{Meta: domain.Meta{ID: "binding", ProductID: "p"}, RepositoryID: "repo", ConnectionID: "conn", Role: "SOURCE", FullName: "example/source"})
	m.state.GitHubConnections = append(m.state.GitHubConnections, domain.GitHubConnection{Meta: domain.Meta{ID: "conn"}})
	m.state.ConnectionGrants = append(m.state.ConnectionGrants, domain.ConnectionGrant{Meta: domain.Meta{ID: "grant", ProductID: "p"}, ConnectionID: "conn"})
	s.provider = &reviewFixture{review: delivery.PullRequestReview{Number: 6, Head: "dev", Base: "main", Comments: []delivery.ReviewComment{{ID: 1, Kind: "inline", Body: "[P1] Ours"}, {ID: 2, Kind: "inline", Body: "[P2] Another feature"}}}}
	ctx := context.Background()
	if _, err := s.SyncReview(ctx, "agent", "i", "repo", 6, nil, true); err != nil {
		t.Fatal(err)
	}
	if len(m.state.Findings) != 0 {
		t.Fatal("preview mutated findings")
	}
	if _, err := s.SyncReview(ctx, "agent", "i", "repo", 6, nil, false); err == nil {
		t.Fatal("shared PR attributed wholesale")
	}
	if _, err := s.SyncReview(ctx, "agent", "i", "repo", 6, []string{"inline:1"}, false); err != nil {
		t.Fatal(err)
	}
	if len(m.state.Findings) != 1 || m.state.Findings[0].Body != "[P1] Ours" {
		t.Fatal("selection ignored")
	}
}
