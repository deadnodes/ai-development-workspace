package application

import (
	"context"
	"encoding/json"
	"errors"
	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
	"strings"
	"testing"
	"time"
)

type fakePRObserver struct {
	*fakeDelivery
	fail  bool
	calls int
}

func (p *fakePRObserver) ObservePullRequest(_ context.Context, _ delivery.Connection, _ string, n int) (delivery.PullRequestObservation, error) {
	p.calls++
	if p.fail {
		return delivery.PullRequestObservation{}, errors.New("permission denied")
	}
	return delivery.PullRequestObservation{Number: n, State: "MERGED", Title: "Completed change", Head: "deleted-feature", Base: "main", HeadSHA: shaHead, MergeSHA: shaBase}, nil
}
func TestRefreshGitLinkedPRWithoutBranch(t *testing.T) {
	for _, mode := range []string{"pr-only", "both", "failure", "released"} {
		t.Run(mode, func(t *testing.T) {
			s, m, f := externalFixture(t)
			if mode != "both" {
				m.state.IntegrationRevisions = nil
			}
			in := integration(&m.state, "i")
			in.PullRequests = []domain.PullRequest{{ID: "7", RepositoryID: "source", URL: "https://github.com/owner/source/pull/7", Status: "open"}}
			if mode == "released" {
				in.Status = "released"
			}
			p := &fakePRObserver{fakeDelivery: f, fail: mode == "failure"}
			s.provider = p
			exec(t, s, domain.Command{Action: "refresh_integration_git", IntegrationID: "i"})
			tickNow(t, s, m)
			op := m.state.Operations[0]
			if mode == "failure" {
				if op.Status != "FAILED" || len(m.state.Memories) != 0 {
					t.Fatal("failed observation was published", op)
				}
				return
			}
			if op.Status != "SUCCEEDED" || p.calls != 1 {
				t.Fatal(op)
			}
			if mode != "both" && (len(m.state.GitObservations) != 0 || len(m.state.IntegrationRevisions) != 0 || len(in.Branches) != 0) {
				t.Fatal("PR observation created deployable source")
			}
			if mode == "both" && len(m.state.GitObservations) != 1 {
				t.Fatal("branch observation lost")
			}
			graph, err := featureGraph(m.state, "f")
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, r := range graph.Repositories {
				for _, pr := range r.PullRequests {
					if pr.ID == "7" {
						found = true
						if pr.Status != "MERGED" || pr.SourceBranch != "deleted-feature" || pr.MergeSHA != shaBase || pr.ObservedAt == "" {
							t.Fatal(pr)
						}
					}
				}
			}
			gitState, err := s.Query(context.Background(), "get_integration_git", "i")
			if err != nil {
				t.Fatal(err)
			}
			if len(gitState.(map[string]any)["pull_requests"].([]GraphPullRequest)) != 1 {
				t.Fatal("MCP git context missing PR")
			}
			if !found {
				t.Fatal("PR absent from graph")
			}
			want := "merged"
			if mode == "released" {
				want = "open"
			}
			if integration(&m.state, "i").PullRequests[0].Status != want {
				t.Fatal("wrong persisted snapshot status")
			}
		})
	}
}

func TestSuccessfulGitRefreshClearsOnlyOlderFailedAttention(t *testing.T) {
	s, m, _ := externalFixture(t)
	now := time.Now().UTC()
	older := now.Add(-time.Minute)
	later := now.Add(time.Minute)
	m.state.Operations = []domain.ExternalOperation{
		{Meta: domain.Meta{ID: "old", ProductID: "p", CreatedAt: older}, IntegrationID: "i", Kind: "REFRESH_GIT", Status: "FAILED", FinishedAt: &older},
		{Meta: domain.Meta{ID: "ok", ProductID: "p", CreatedAt: now}, IntegrationID: "i", Kind: "REFRESH_GIT", Status: "SUCCEEDED", FinishedAt: &now},
		{Meta: domain.Meta{ID: "new", ProductID: "p", CreatedAt: later}, IntegrationID: "i", Kind: "REFRESH_GIT", Status: "FAILED", FinishedAt: &later},
	}
	value, err := s.Query(context.Background(), "list_attention", "p")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(value)
	if strings.Contains(string(body), `"id":"old"`) || !strings.Contains(string(body), `"id":"new"`) {
		t.Fatal(string(body))
	}
	if len(m.state.Operations) != 3 {
		t.Fatal("history removed")
	}
}
