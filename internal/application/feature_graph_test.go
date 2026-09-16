package application

import (
	"context"
	"encoding/json"
	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
	"testing"
)

func TestFeatureGraphSeparatesMergeReportedAndRuntimeEvidence(t *testing.T) {
	s, m := fixture(t)
	m.state.Repositories = []domain.Repository{{Meta: domain.Meta{ID: "r", ProductID: "p"}, Name: "org/repo", URL: "https://github.com/org/repo"}}
	m.state.Integrations[0].Repositories = []string{"r"}
	m.state.Integrations[0].PullRequests = []domain.PullRequest{{RepositoryID: "r", ID: "42", URL: "https://github.com/org/repo/pull/42", Status: "merged"}}
	m.state.Memories = []domain.Memory{{Meta: domain.Meta{ID: "history", ProductID: "p", FeatureID: "f"}, IntegrationID: "i", Body: `[{"url":"https://github.com/org/repo/pull/42","source_branch":"feature/a","target_branch":"main","head_sha":"head","merge_sha":"merge","status":"MERGED","observed_at":"2026-09-16T10:00:00Z"}]`}, {Meta: domain.Meta{ID: "reported", ProductID: "p", FeatureID: "f"}, Body: `{"reported_at":"2026-09-16","cluster":"prod","source_commit_identities_verified_on_github":{"repo":{"verified":{"sha":"old","url":"https://github.com/org/repo/commit/old"}}}}`}}
	m.state.Operations = []domain.ExternalOperation{{Meta: domain.Meta{ID: "op", ProductID: "p"}, IntegrationID: "i", EnvironmentID: "dev", Status: "SUCCEEDED", DeploymentState: "GITOPS_APPLIED", GitOpsResult: &delivery.GitOpsResult{CommitSHA: "gitops"}}}
	value, err := s.Query(context.Background(), "get_feature_graph", "f")
	if err != nil {
		t.Fatal(err)
	}
	g := value.(*FeatureGraph)
	if len(g.Repositories) != 1 || len(g.Repositories[0].PullRequests) != 1 {
		t.Fatalf("missing repository PR %+v", g)
	}
	p := g.Repositories[0].PullRequests[0]
	if p.SourceBranch != "feature/a" || p.TargetBranch != "main" || p.MergeSHA != "merge" || p.SourceMemoryID != "history" {
		t.Fatalf("bad provenance %+v", p)
	}
	if len(g.ReportedSnapshots) != 1 || g.ReportedSnapshots[0].Evidence != "reported" || g.ReportedSnapshots[0].Components[0].Commit != "old" {
		t.Fatalf("bad historical snapshot %+v", g.ReportedSnapshots)
	}
	if len(g.Deployments) != 1 || g.Deployments[0].DeploymentState != "GITOPS_APPLIED" || len(g.Deployments[0].RuntimeObservations) != 0 {
		t.Fatalf("GitOps falsely treated as runtime %+v", g.Deployments)
	}
	for _, b := range g.Repositories[0].Branches {
		if b.HeadCommit != "" {
			t.Fatal("historical PR invented current branch HEAD")
		}
	}
}
func TestFeatureGraphScopesEvidenceAndDeduplicatesPR(t *testing.T) {
	s, m := fixture(t)
	m.state.Repositories = []domain.Repository{{Meta: domain.Meta{ID: "r", ProductID: "p"}, Name: "repo"}, {Meta: domain.Meta{ID: "other", ProductID: "other"}, Name: "secret"}}
	m.state.Features[0].Repositories = []string{"r", "other"}
	m.state.Integrations[0].PullRequests = []domain.PullRequest{{RepositoryID: "r", ID: "1", URL: "https://github.com/org/repo/pull/1"}}
	second := m.state.Integrations[0]
	second.ID = "second"
	m.state.Integrations = append(m.state.Integrations, second)
	m.state.Memories = []domain.Memory{{Meta: domain.Meta{ID: "bad", ProductID: "other", FeatureID: "f"}, IntegrationID: "i", Body: `[{"url":"https://github.com/org/repo/pull/1","source_branch":"secret"}]`}, {Meta: domain.Meta{ID: "unlinked", ProductID: "p", FeatureID: "f"}, IntegrationID: "i", Body: `[{"url":"https://github.com/org/repo/pull/99","source_branch":"unlinked"}]`}}
	m.state.Operations = []domain.ExternalOperation{{Meta: domain.Meta{ID: "other", ProductID: "other"}, IntegrationID: "i", EnvironmentID: "secret"}}
	g, err := featureGraph(m.state, "f")
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Repositories) != 1 || len(g.Repositories[0].PullRequests) != 1 || len(g.Repositories[0].PullRequests[0].IntegrationIDs) != 2 || g.Repositories[0].PullRequests[0].SourceBranch != "" || len(g.Deployments) != 0 {
		t.Fatalf("scope leak %+v", g)
	}
	a, _ := json.Marshal(g)
	again, _ := s.Query(context.Background(), "get_feature_graph", "f")
	b, _ := json.Marshal(again)
	if string(a) != string(b) {
		t.Fatal("non deterministic graph")
	}
	if _, err := featureGraph(m.state, "absent"); err == nil {
		t.Fatal("missing feature accepted")
	}
}

func TestFeatureGraphDeploymentRepositoryIdentity(t *testing.T) {
	_, m := fixture(t)
	m.state.Features[0].Repositories = []string{"r", "r2"}
	m.state.Repositories = []domain.Repository{{Meta: domain.Meta{ID: "r", ProductID: "p"}}, {Meta: domain.Meta{ID: "r2", ProductID: "p"}}, {Meta: domain.Meta{ID: "secret", ProductID: "other"}}}
	m.state.Operations = []domain.ExternalOperation{
		{Meta: domain.Meta{ID: "single", ProductID: "p"}, IntegrationID: "i", EnvironmentID: "dev", Snapshot: &domain.DeliverySnapshot{Revision: domain.IntegrationRevision{RepositoryID: "r", HeadCommit: "head", Branch: "feature"}}},
		{Meta: domain.Meta{ID: "multi", ProductID: "p"}, IntegrationIDs: []string{"i", "unrelated"}, EnvironmentID: "dev", Sources: []domain.FlowSource{{RepositoryID: "r", Result: &delivery.SourceResult{SHA: "one"}}, {RepositoryID: "r2", Result: &delivery.SourceResult{SHA: "two"}}, {RepositoryID: "secret", Result: &delivery.SourceResult{SHA: "private"}}}},
	}
	g, err := featureGraph(m.state, "f")
	if err != nil {
		t.Fatal(err)
	}
	single := g.Deployments[0]
	if single.RepositoryID != "r" || single.SourceCommit != "head" || len(single.IntegrationIDs) != 1 || single.IntegrationIDs[0] != "i" {
		t.Fatalf("single repository filter cannot match %+v", single)
	}
	multi := g.Deployments[1]
	if len(multi.Sources) != 2 || multi.Sources[1].RepositoryID != "r2" || len(multi.IntegrationIDs) != 1 {
		t.Fatalf("multi repository identity or scope failed %+v", multi)
	}
}
