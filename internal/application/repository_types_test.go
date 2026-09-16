package application

import (
	"context"
	"testing"

	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
)

func repositoryPurposeFixture(t *testing.T, role string) (*Service, *memoryStore) {
	t.Helper()
	_, m := fixture(t)
	s := NewWithProvider(m, &fakeDelivery{})
	exec(t, s, domain.Command{Action: "create_github_connection", ID: "conn", ProductID: "p", Data: map[string]any{"name": "GitHub", "app_id": 1, "installation_id": 2, "owner": "owner", "private_key_ref": "env:RCP_TEST_KEY"}})
	if _, err := s.Query(context.Background(), "discover_repositories", "conn"); err != nil {
		t.Fatal(err)
	}
	exec(t, s, domain.Command{Action: "import_repository", ID: "repo", ProductID: "p", Data: map[string]any{"connection_id": "conn", "full_name": "owner/source", "role": role, "default_branch": "main"}})
	return s, m
}
func TestRepositoryPurposeAndComponentKindCatalog(t *testing.T) {
	for _, role := range domain.RepositoryRoles() {
		t.Run(role, func(t *testing.T) {
			s, m := repositoryPurposeFixture(t, role)
			if m.state.Repositories[0].Role != role || m.state.RepositoryBindings[0].Role != role {
				t.Fatal("repository purpose not retained")
			}
			for _, kind := range domain.ComponentKinds() {
				c := domain.Command{Action: "create_application", ID: kind, ProductID: "p", Data: map[string]any{"name": kind, "repository_id": "repo", "kind": kind}}
				if domain.RepositoryAllows(role, kind) {
					exec(t, s, c)
				} else {
					reject(t, s, c)
				}
			}
			reject(t, s, domain.Command{Action: "create_application", ProductID: "p", Data: map[string]any{"name": "Bad", "repository_id": "repo", "kind": "SERVICE"}})
			exec(t, s, domain.Command{Action: "create_product", ID: "other", Data: map[string]any{"name": "Other"}})
			reject(t, s, domain.Command{Action: "create_application", ProductID: "other", Data: map[string]any{"name": "Foreign", "repository_id": "repo", "kind": "LIBRARY"}})
		})
	}
	s, _ := fixture(t)
	for _, role := range []string{"", "custom", "library", "DEPLOYMENT", "SOURCE"} {
		reject(t, s, domain.Command{Action: "create_repository", ProductID: "p", Data: map[string]any{"name": "Invalid", "url": "https://example.test/repo", "role": role}})
	}
	exec(t, s, domain.Command{Action: "create_repository", ID: "default-app", ProductID: "p", Data: map[string]any{"name": "Application", "url": "https://example.test/repo"}})
	exec(t, s, domain.Command{Action: "create_application", ProductID: "p", Data: map[string]any{"name": "Default app", "repository_id": "default-app"}})
	if domain.ComponentKind(domain.Application{}) != "" {
		t.Fatal("missing stored kind was silently inferred")
	}
}
func TestLibraryNeedsNoClusterAndCannotBecomeDeployment(t *testing.T) {
	s, m := repositoryPurposeFixture(t, "LIBRARY")
	exec(t, s, domain.Command{Action: "create_application", ID: "library", ProductID: "p", Data: map[string]any{"name": "SDK", "repository_id": "repo", "kind": "LIBRARY"}})
	exec(t, s, domain.Command{Action: "record_integration_revision", ID: "lib-revision", IntegrationID: "i", Data: revisionData(shaHead)})
	exec(t, s, domain.Command{Action: "configure_publication", ID: "publication", ProductID: "p", Data: map[string]any{"application_id": "library", "format": "npm", "registry_url": "https://registry.example.test", "package_name": "@team/sdk"}})
	if len(m.state.Environments) != 0 || len(m.state.ComponentBuilds) != 0 {
		t.Fatal("library setup invented deployment infrastructure")
	}
	exec(t, s, domain.Command{Action: "refresh_integration_git", ID: "sync-library", IntegrationID: "i"})
	runDelivery(t, s, m)
	if m.state.Operations[0].Status != "SUCCEEDED" || len(m.state.GitObservations) == 0 {
		t.Fatal("library Git observation unavailable")
	}
	reject(t, s, domain.Command{Action: "configure_component", ProductID: "p", Data: map[string]any{"application_id": "library", "connection_id": "conn", "workflow": "build.yml", "image_repository": "ghcr.io/owner/sdk"}})
	exec(t, s, domain.Command{Action: "create_environment", ID: "env", ProductID: "p", Data: map[string]any{"name": "Existing DEV"}})
	reject(t, s, domain.Command{Action: "configure_environment", ProductID: "p", Data: map[string]any{"environment_id": "env", "application_id": "library", "purpose": "DEV", "connection_id": "conn", "repository_id": "repo", "ref": "main", "path": "sdk.yaml", "image_field": "image.repository", "digest_field": "image.digest", "allow_deploy": true}})
	reject(t, s, domain.Command{Action: "plan_composition", ProductID: "p", Data: planData(component("library", "lib-revision"))})
	reject(t, s, domain.Command{Action: "deploy_integration", IntegrationID: "i", Data: map[string]any{"environment_id": "env", "application_id": "library", "revision_id": "lib-revision"}})
}
func TestMixedRepositoryApplicationCompositionExcludesLibrary(t *testing.T) {
	s, m := repositoryPurposeFixture(t, "MIXED")
	for _, v := range []struct{ id, kind string }{{"app", "APPLICATION"}, {"library", "LIBRARY"}} {
		exec(t, s, domain.Command{Action: "create_application", ID: v.id, ProductID: "p", Data: map[string]any{"name": v.id, "repository_id": "repo", "kind": v.kind}})
	}
	exec(t, s, domain.Command{Action: "record_integration_revision", ID: "revision", IntegrationID: "i", Data: revisionData(shaHead)})
	exec(t, s, domain.Command{Action: "create_environment", ID: "env", ProductID: "p", Data: map[string]any{"name": "DEV"}})
	exec(t, s, domain.Command{Action: "plan_composition", ID: "application-only", ProductID: "p", Data: planData(component("app", "revision"))})
	if len(m.state.Compositions[0].Components) != 1 || m.state.Compositions[0].Components[0].ApplicationID != "app" {
		t.Fatal("library entered application deployment composition")
	}
	reject(t, s, domain.Command{Action: "plan_composition", ProductID: "p", Data: planData(component("app", "revision"), component("library", "revision"))})
}

func TestRepositoryClassificationKeepsBindingHistoryAndScope(t *testing.T) {
	s, m := repositoryPurposeFixture(t, "APPLICATION")
	original := m.state.RepositoryBindings[0]
	exec(t, s, domain.Command{Action: "classify_repository", ID: "repo", ProductID: "p", Data: map[string]any{"role": "LIBRARY"}})
	if m.state.Repositories[0].Role != "LIBRARY" || len(m.state.RepositoryBindings) != 2 || m.state.RepositoryBindings[0] != original {
		t.Fatal("classification overwrote binding history")
	}
	exec(t, s, domain.Command{Action: "create_application", ID: "library", ProductID: "p", Data: map[string]any{"name": "SDK", "repository_id": "repo", "kind": "LIBRARY"}})
	exec(t, s, domain.Command{Action: "classify_repository", ID: "repo", ProductID: "p", Data: map[string]any{"role": "MIXED"}})
	exec(t, s, domain.Command{Action: "create_application", ID: "app", ProductID: "p", Data: map[string]any{"name": "Service", "repository_id": "repo", "kind": "APPLICATION"}})
	for _, role := range []string{"LIBRARY", "APPLICATION", "GITOPS", "custom"} {
		reject(t, s, domain.Command{Action: "classify_repository", ID: "repo", ProductID: "p", Data: map[string]any{"role": role}})
	}
	exec(t, s, domain.Command{Action: "create_product", ID: "other", Data: map[string]any{"name": "Other"}})
	reject(t, s, domain.Command{Action: "classify_repository", ID: "repo", ProductID: "other", Data: map[string]any{"role": "MIXED"}})
}

func TestLibraryPullRequestReviewRemainsSourceContext(t *testing.T) {
	s, m := repositoryPurposeFixture(t, "LIBRARY")
	exec(t, s, domain.Command{Action: "create_application", ID: "library", ProductID: "p", Data: map[string]any{"name": "SDK", "repository_id": "repo", "kind": "LIBRARY"}})
	exec(t, s, domain.Command{Action: "update_integration", IntegrationID: "i", Data: map[string]any{"branches": []domain.Branch{{RepositoryID: "repo", Name: "feature/work"}}}})
	s.provider = &reviewFixture{Provider: &fakeDelivery{}, review: delivery.PullRequestReview{Number: 42, Head: "feature/work", Base: "main", HeadSHA: shaHead, Comments: []delivery.ReviewComment{{ID: 1, Kind: "inline", Author: "chatgpt-codex-connector[bot]", Body: "[P2] Preserve public SDK API", Commit: shaHead}}}}
	if _, err := s.SyncReview(context.Background(), "agent", "i", "repo", 42, nil, false); err != nil {
		t.Fatal(err)
	}
	if len(m.state.Findings) != 1 || m.state.Findings[0].ReviewSource.RepositoryID != "repo" || len(m.state.Environments) != 0 {
		t.Fatal("library review context lost or deployment state invented")
	}
}
