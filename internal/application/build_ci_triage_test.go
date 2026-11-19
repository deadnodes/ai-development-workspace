package application

import (
	"context"
	"releasecontrol/internal/buildconfig"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/providers/forgejo"
	"strings"
	"testing"
	"time"
)

func TestBuildCITriageMatchesAndAttentionSuppression(t *testing.T) {
	now := time.Now().UTC()
	sha := strings.Repeat("a", 40)
	st := domain.State{Repositories: []domain.Repository{{Meta: domain.Meta{ID: "repo", ProductID: "p"}, Name: "repo", URL: "https://forge/o/repo.git"}}, Features: []domain.Feature{{Meta: domain.Meta{ID: "f", ProductID: "p"}, Title: "Feature"}}, Integrations: []domain.Integration{{Meta: domain.Meta{ID: "i", ProductID: "p", FeatureID: "f"}, Commits: []domain.Commit{{RepositoryID: "repo", SHA: sha}}}}, Memories: []domain.Memory{{Meta: domain.Meta{ID: "m", ProductID: "p", FeatureID: "f", CreatedAt: now}, Kind: "handoff", Title: "handoff", Commit: sha}}}
	features, handoffs := buildCITriageMatches(st, domain.Build{Meta: domain.Meta{ProductID: "p"}}, buildconfig.Profile{Repository: "o/repo"}, []domain.BuildCICommit{{SHA: sha}})
	if len(features) != 1 || len(features[0].IntegrationIDs) != 1 || len(handoffs) != 1 {
		t.Fatalf("features=%+v handoffs=%+v", features, handoffs)
	}
	old := domain.Build{Meta: domain.Meta{ID: "old", ProductID: "p"}, ProfileID: "x", Branch: "feat", CIState: "failure", QueuedAt: now, CIFailedAt: &now}
	newer := old
	newer.ID = "new"
	newer.QueuedAt = now.Add(time.Minute)
	newer.CIState = "pending"
	if got := buildCIAttention(domain.State{Builds: []domain.Build{old, newer}}, "p"); len(got) != 1 {
		t.Fatalf("pending suppressed failure: %v", got)
	}
	newer.CIState = "success"
	if got := buildCIAttention(domain.State{Builds: []domain.Build{old, newer}}, "p"); len(got) != 0 {
		t.Fatalf("green did not suppress failure: %v", got)
	}
}
func TestPipelineChecksAttention(t *testing.T) {
	b := domain.Build{CIState: "failure", CIFailingContexts: map[string]string{"lint": "failure"}}
	stage := buildChecksStage(b)
	if stage.Status != "failed" || stage.Label != "Checks failed" || stage.Hint != "lint" {
		t.Fatal(stage)
	}
	if got := overallPipeline(domain.Build{Status: "SUCCEEDED"}, []domain.PipelineStage{stage}); got != "attention" {
		t.Fatal(got)
	}
}

func TestWatchBuildCIPendingFailureRecoveryAndRateLimit(t *testing.T) {
	s, m, source, _ := deploymentFixture(t)
	source.status = forgejo.CommitStatus{State: "pending", Statuses: []forgejo.StatusContext{{Context: "verify", Status: "pending"}}}
	m.state.Builds[0].Status = "SUCCEEDED"
	m.state.Builds[0].Branch = "topic"
	m.state.Builds[0].QueuedAt = time.Now().UTC()
	m.state.Builds[0].FinishedAt = copyTime(&m.state.Builds[0].QueuedAt)
	// Required (push) contexts exist only on the default branch: other branches are not observed.
	if err := s.watchBuildCI(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m.state.Builds[0].CIState != "" || m.state.Builds[0].CICheckedAt != nil {
		t.Fatalf("non-default branch observed: %+v", m.state.Builds[0])
	}
	page, err := s.ListPipelines(context.Background(), domain.ListPipelinesRequest{ProductID: "p"})
	if err != nil || len(page.Items) == 0 {
		t.Fatal(page, err)
	}
	for _, st := range page.Items[0].Stages {
		if st.Kind == "checks" && st.Status != "skipped" {
			t.Fatalf("non-default branch checks stage %+v", st)
		}
	}
	m.state.Builds[0].Branch = "main"
	if err := s.watchBuildCI(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m.state.Builds[0].CIState != "pending" {
		t.Fatalf("initial pending state: %+v", m.state.Builds[0])
	}
	oldPoll := time.Now().Add(-time.Minute - time.Second)
	m.state.Builds[0].CICheckedAt = &oldPoll
	source.status = forgejoFailureStatus()
	if err := s.watchBuildCI(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m.state.Builds[0].CIState != "failure" || m.state.Builds[0].CITriage == nil {
		t.Fatalf("build=%+v", m.state.Builds[0])
	}
	n := 0
	for _, e := range m.state.Events {
		if e.Action == "build_ci_failed" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("failure events %d", n)
	}
	checked := copyTime(m.state.Builds[0].CICheckedAt)
	if err := s.watchBuildCI(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !m.state.Builds[0].CICheckedAt.Equal(*checked) {
		t.Fatal("poll reservation changed within a minute")
	}
	old := time.Now().Add(-time.Minute - time.Second)
	m.state.Builds[0].CICheckedAt = &old
	source.status = forgejo.CommitStatus{State: "success", Statuses: []forgejo.StatusContext{{Context: "verify", Status: "success"}}}
	if err := s.watchBuildCI(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m.state.Builds[0].CIState != "success" || len(buildCIAttention(m.state, "p")) != 0 {
		t.Fatalf("recovery %+v", m.state.Builds[0])
	}
	rec := 0
	for _, e := range m.state.Events {
		if e.Action == "build_ci_recovered" {
			rec++
		}
	}
	if rec != 1 {
		t.Fatalf("recovery events %d", rec)
	}
}
func forgejoFailureStatus() forgejo.CommitStatus {
	return forgejo.CommitStatus{State: "failure", Statuses: []forgejo.StatusContext{{Context: "verify", Status: "failure"}}}
}
