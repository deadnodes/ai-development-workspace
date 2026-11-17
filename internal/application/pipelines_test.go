package application

import (
	"context"
	"strings"
	"testing"
	"time"

	"releasecontrol/internal/domain"
)

func TestPipelineStageMappings(t *testing.T) {
	cases := []struct {
		status, code, want string
		hint               bool
	}{{"RECONCILIATION_REQUIRED", "", "attention", true}, {"CANCELLED", "", "cancelled", false}, {"CANCELLED", "SUPERSEDED", "superseded", false}, {"FAILED", "", "failed", false}, {"SUCCEEDED", "", "success", false}}
	for _, tc := range cases {
		b := domain.Build{Meta: domain.Meta{ID: "b"}, Status: tc.status, FailureCode: tc.code}
		if tc.code == "SUPERSEDED" {
			b.SupersededBy = "new"
		}
		got := buildPipelineStage(b, true, true)
		if got.Status != tc.want {
			t.Errorf("build %s: got %s want %s", tc.status, got.Status, tc.want)
		}
		if tc.hint && got.Hint == "" {
			t.Errorf("build %s missing hint", tc.status)
		}
	}
	d := domain.Deployment{Meta: domain.Meta{ID: "d"}, Status: "RECONCILIATION_REQUIRED", FailureCode: "PR_UNCERTAIN"}
	stage := deploymentPipelineStage(d, false)
	if stage.Status != "attention" || !strings.Contains(stage.Hint, "Flux PR") {
		t.Fatalf("PR_UNCERTAIN mapping: %#v", stage)
	}
	d.Status = "FAILED"
	d.FailureCode = "OPERATOR_CLOSED"
	d.CloseReason = "operator requested closure"
	stage = deploymentPipelineStage(d, false)
	if stage.Status != "cancelled" || stage.Hint != d.CloseReason {
		t.Fatalf("operator close mapping: %#v", stage)
	}
	d.FailureCode = "SUPERSEDED"
	stage = deploymentPipelineStage(d, false)
	if stage.Status != "superseded" {
		t.Fatalf("superseded mapping: %#v", stage)
	}
}

func TestPipelineOrderingAndSearchHelpers(t *testing.T) {
	a := domain.Build{Meta: domain.Meta{ID: "z"}, QueuedAt: mustPipelineTime("2026-01-02T00:00:00Z")}
	b := domain.Build{Meta: domain.Meta{ID: "a"}, QueuedAt: a.QueuedAt}
	if pipelineTime(a).Before(pipelineTime(b)) || a.ID <= b.ID {
		t.Fatal("expected time-descending ordering with ID-descending tie break")
	}
	b.SourceSHA = "abcdef0123456789"
	b.Images = []domain.BuildImage{{Digest: "sha256:deadbeef"}}
	dep := domain.Deployment{PRNumber: 12, Images: []domain.BuildImage{{Digest: "sha256:cafebabe"}}}
	for _, q := range []string{"abcdef0", "#12", "12", "deadbeef", "cafebabe"} {
		if !pipelineMatches(b, []domain.Deployment{dep}, q) {
			t.Errorf("query %q did not match", q)
		}
	}
}
func mustPipelineTime(v string) time.Time {
	t, e := time.Parse(time.RFC3339, v)
	if e != nil {
		panic(e)
	}
	return t
}

func TestListPipelinesTimeCursorAndFilters(t *testing.T) {
	s, m, f, _ := buildFixture(t)
	for _, sha := range []string{"a", "b", "c"} {
		f.head = strings.Repeat(sha, 40)
		queue(t, s, "")
	}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range m.state.Builds {
		m.state.Builds[i].QueuedAt = base.Add(time.Duration(i) * time.Minute)
		m.state.Builds[i].CreatedAt = m.state.Builds[i].QueuedAt
		m.state.Builds[i].Status = "SUCCEEDED"
	}
	// Seed one deployment to exercise environment, deployment-ID and PR searches.
	b := m.state.Builds[1]
	m.state.Deployments = append(m.state.Deployments, domain.Deployment{Meta: domain.Meta{ID: "deploy-z", ProductID: "p", CreatedAt: base.Add(time.Hour)}, BuildID: b.ID, ProfileID: b.ProfileID, EnvironmentID: "dev", Status: "FAILED", FailureCode: "CI_NOT_GREEN", PRNumber: 12})
	first, err := s.ListPipelines(context.Background(), domain.ListPipelinesRequest{ProductID: "p", Limit: 2})
	if err != nil || len(first.Items) != 2 || first.NextCursor == "" {
		t.Fatal(first, err)
	}
	second, err := s.ListPipelines(context.Background(), domain.ListPipelinesRequest{ProductID: "p", Limit: 2, Cursor: first.NextCursor})
	if err != nil || len(second.Items) != 1 || second.NextCursor != "" {
		t.Fatal(second, err)
	}
	seen := map[string]bool{}
	for _, p := range append(first.Items, second.Items...) {
		if seen[p.Build.ID] {
			t.Fatalf("duplicate cursor item %s", p.Build.ID)
		}
		seen[p.Build.ID] = true
	}
	if len(seen) != 3 {
		t.Fatal("cursor gap", seen)
	}
	if first.Items[0].Build.QueuedAt.Before(first.Items[1].Build.QueuedAt) {
		t.Fatal("pipelines not time ordered")
	}
	if _, err = s.ListPipelines(context.Background(), domain.ListPipelinesRequest{ProductID: "else", Limit: 2, Cursor: first.NextCursor}); err == nil {
		t.Fatal("cursor filter binding ignored")
	}
	if _, err = s.ListPipelines(context.Background(), domain.ListPipelinesRequest{ProductID: "p", ProfileID: "other", Limit: 2, Cursor: first.NextCursor}); err == nil {
		t.Fatal("cursor profile binding ignored")
	}
	for _, tc := range []struct {
		in    domain.ListPipelinesRequest
		count int
	}{{domain.ListPipelinesRequest{ProductID: "p", ProfileID: "app"}, 3}, {domain.ListPipelinesRequest{ProductID: "p", EnvironmentID: "dev"}, 1}, {domain.ListPipelinesRequest{ProductID: "p", Query: "#12"}, 1}, {domain.ListPipelinesRequest{ProductID: "p", Query: "deploy-z"}, 1}, {domain.ListPipelinesRequest{ProductID: "p", Query: "bbbbbbb"}, 1}} {
		page, e := s.ListPipelines(context.Background(), tc.in)
		if e != nil || len(page.Items) != tc.count {
			t.Fatalf("filter %#v: got %d err %v", tc.in, len(page.Items), e)
		}
	}
}
