package application

import (
	"context"
	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
	"strings"
	"testing"
	"time"
)

func TestAutomaticGitScopeAndBackoff(t *testing.T) {
	s, m, _ := externalFixture(t)
	now := time.Now().UTC()
	interval := 2 * time.Minute
	in := integration(&m.state, "i")
	in.Owner = "agent/test"
	in.Status = "working"
	in.Branches = []domain.Branch{{RepositoryID: "source", Name: "feature/work"}}
	run := func() {
		t.Helper()
		if err := s.ScheduleGitRefresh(context.Background(), now, interval); err != nil {
			t.Fatal(err)
		}
	}
	run()
	n := len(m.state.Operations)
	if n != 1 || m.state.Operations[0].RequestedBy != autoGitActor {
		t.Fatal(m.state.Operations)
	}
	run()
	if len(m.state.Operations) != n {
		t.Fatal("duplicate pending")
	}
	m.state.Operations[0].Status = "FAILED"
	finished := now
	m.state.Operations[0].FinishedAt = &finished
	now = now.Add(3 * time.Minute)
	run()
	if len(m.state.Operations) != n {
		t.Fatal("backoff ignored")
	}
	now = now.Add(8 * time.Minute)
	run()
	if len(m.state.Operations) != n+1 {
		t.Fatal("backoff never expires")
	}
	// Captured revisions / mere repository membership are not active branch claims.
	in = integration(&m.state, "i")
	in.Branches = nil
	in.PullRequests = nil
	if autoGitEligible(&m.state, *in) {
		t.Fatal("historical revision activates polling")
	}
	in.Status = "ready"
	in.PullRequests = []domain.PullRequest{{RepositoryID: "source", ID: "1", Status: "open"}}
	if !autoGitEligible(&m.state, *in) {
		t.Fatal("pending review ignored")
	}
	in.PullRequests[0].Status = "merged"
	if autoGitEligible(&m.state, *in) {
		t.Fatal("closed PR activates polling")
	}
	in.PullRequests[0].Status = "open"
	in.PullRequests[0].RepositoryID = "unknown"
	if autoGitEligible(&m.state, *in) {
		t.Fatal("unconfigured repository activates polling")
	}
	in.PullRequests[0].RepositoryID = "source"
	feature(&m.state, in.FeatureID).Status = "completed"
	if autoGitEligible(&m.state, *in) {
		t.Fatal("completed feature polled")
	}
}

type scopedGitFake struct{ *fakeDelivery }

func (f *scopedGitFake) ObserveBranch(_ context.Context, _ delivery.Connection, _ string, branch string) (delivery.BranchInfo, error) {
	sha := shaHead
	if branch == "main" {
		sha = shaBase
	}
	return delivery.BranchInfo{Name: branch, SHA: sha}, nil
}
func (f *scopedGitFake) DiscoverBranchPullRequests(context.Context, delivery.Connection, string, string) ([]delivery.PullRequestObservation, error) {
	return []delivery.PullRequestObservation{{Number: 7, URL: "https://github.com/owner/source/pull/7", State: "OPEN"}}, nil
}
func (f *scopedGitFake) ObservePullRequest(context.Context, delivery.Connection, string, int) (delivery.PullRequestObservation, error) {
	return delivery.PullRequestObservation{Number: 7, URL: "https://github.com/owner/source/pull/7", State: "OPEN", Head: "feature/work", Base: "main", HeadSHA: shaHead}, nil
}
func TestAutomaticGitDiscoversPRAndObservesBoundBranch(t *testing.T) {
	s, m, f := externalFixture(t)
	s.provider = &scopedGitFake{f}
	in := integration(&m.state, "i")
	in.Owner = "agent/test"
	in.Status = "working"
	in.Branches = []domain.Branch{{RepositoryID: "source", Name: "feature/work"}}
	if err := s.ScheduleGitRefresh(context.Background(), time.Now().UTC(), time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	in = integration(&m.state, "i")
	if len(in.PullRequests) != 1 || in.PullRequests[0].ID != "7" || len(m.state.GitObservations) != 1 || m.state.Operations[0].Status != "SUCCEEDED" {
		t.Fatalf("PR=%+v op=%+v", in.PullRequests, m.state.Operations)
	}
}

type inventoryGitFake struct{ *fakeDelivery }

func (f *inventoryGitFake) Branches(context.Context, delivery.Connection, string) ([]delivery.BranchInfo, error) {
	return []delivery.BranchInfo{{Name: "feature/work", SHA: strings.Repeat("c", 40)}, {Name: "main", SHA: shaBase}}, nil
}

func (f *inventoryGitFake) Compare(_ context.Context, _ delivery.Connection, _ string, base, head string) (delivery.CompareResult, error) {
	return delivery.CompareResult{BaseSHA: base, HeadSHA: head, Ahead: 1, Status: "ahead", Commits: []delivery.CommitInfo{{SHA: head, Message: "Unlinked repository work", URL: "https://example.test/commit/" + head}}}, nil
}

func TestRepositoryGitRefreshFindsUnlinkedCommit(t *testing.T) {
	s, m, f := externalFixture(t)
	s.provider = &inventoryGitFake{f}
	if err := s.ScheduleRepositoryGitRefresh(context.Background(), time.Now().UTC(), time.Minute); err != nil {
		t.Fatal(err)
	}
	if len(m.state.Operations) != 1 || m.state.Operations[0].Kind != "REFRESH_REPOSITORY_GIT" {
		t.Fatalf("operations=%+v", m.state.Operations)
	}
	if _, err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(m.state.GitObservations) != 1 || m.state.GitObservations[0].IntegrationID != "" {
		t.Fatalf("observations=%+v", m.state.GitObservations)
	}
	items := unlinkedGitCommits(m.state, "p")
	if len(items) != 1 || items[0]["reason"] != "UNLINKED_GIT_COMMIT" {
		t.Fatalf("unlinked=%v", items)
	}
}
