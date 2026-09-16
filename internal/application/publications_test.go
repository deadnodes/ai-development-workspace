package application

import (
	"strings"
	"testing"
	"time"

	"releasecontrol/internal/domain"
)

func publicationFixture() domain.State {
	st := domain.EmptyState()
	st.Products = []domain.Product{{Meta: domain.Meta{ID: "p"}}}
	st.Repositories = []domain.Repository{{Meta: domain.Meta{ID: "repo", ProductID: "p"}}, {Meta: domain.Meta{ID: "other-repo", ProductID: "p"}}}
	st.Applications = []domain.Application{{Meta: domain.Meta{ID: "library", ProductID: "p"}, RepositoryID: "repo", Kind: "LIBRARY"}, {Meta: domain.Meta{ID: "service", ProductID: "p"}, RepositoryID: "repo", Kind: "APPLICATION"}}
	st.Features = []domain.Feature{{Meta: domain.Meta{ID: "feature", ProductID: "p"}, Repositories: []string{"repo"}}}
	st.Integrations = []domain.Integration{{Meta: domain.Meta{ID: "integration", ProductID: "p", FeatureID: "feature"}, Repositories: []string{"repo"}}}
	return st
}
func publicationConfigureData() map[string]any {
	return map[string]any{"application_id": "library", "format": "npm", "registry_url": "https://registry.example.test", "package_name": "@team/library"}
}
func publicationArtifactData() map[string]any {
	return map[string]any{"application_id": "library", "publication_target_id": "target", "source_commit": strings.Repeat("a", 40), "version": "1.2.3", "checksum": "sha256:" + strings.Repeat("b", 64), "uri": "https://registry.example.test/team/library-1.2.3.tgz", "build_url": "https://github.com/team/library/actions/runs/77"}
}
func publicationApply(st *domain.State, action, id, integrationID string, data map[string]any) error {
	_, _, err := applyPublication(st, domain.Command{Action: action, ProductID: "p", IntegrationID: integrationID, Actor: "agent", Data: data}, domain.Meta{ID: id, ProductID: "p", Actor: "agent", CreatedAt: time.Now().UTC()})
	return err
}
func TestLibraryPublicationNeedsNoEnvironmentAndRetainsProvenance(t *testing.T) {
	st := publicationFixture()
	if err := publicationApply(&st, "configure_publication", "target", "", publicationConfigureData()); err != nil {
		t.Fatal(err)
	}
	if err := publicationApply(&st, "record_package_artifact", "artifact", "integration", publicationArtifactData()); err != nil {
		t.Fatal(err)
	}
	if len(st.Environments) != 0 || len(st.Operations) != 0 || len(st.PackageArtifacts) != 1 {
		t.Fatal("metadata created deployment infrastructure")
	}
	recorded := st.PackageArtifacts[0]
	if recorded.RepositoryID != "repo" || recorded.FeatureID != "feature" || recorded.Actor != "agent" || recorded.IntegrationID != "integration" {
		t.Fatal("missing scoped actor provenance")
	}
	changed := publicationArtifactData()
	changed["version"] = "1.2.4"
	changed["source_commit"] = strings.Repeat("c", 40)
	changed["checksum"] = "sha512:" + strings.Repeat("d", 128)
	if err := publicationApply(&st, "record_package_artifact", "next", "integration", changed); err != nil {
		t.Fatal(err)
	}
	if st.PackageArtifacts[0] != recorded || len(st.PackageArtifacts) != 2 {
		t.Fatal("prior artifact overwritten")
	}
	changed["version"] = "1.2.3"
	if err := publicationApply(&st, "record_package_artifact", "rewrite", "integration", changed); err == nil {
		t.Fatal("immutable version provenance rewritten")
	}
}
func TestPublicationRejectsKindsScopesAndInvalidReferences(t *testing.T) {
	for _, kind := range []string{"APPLICATION", ""} {
		st := publicationFixture()
		st.Applications[0].Kind = kind
		if err := publicationApply(&st, "configure_publication", "target", "", publicationConfigureData()); err == nil {
			t.Fatal("deployable/default component accepted")
		}
	}
	for _, field := range []string{"format", "registry_url", "package_name"} {
		st := publicationFixture()
		data := publicationConfigureData()
		data[field] = ""
		if err := publicationApply(&st, "configure_publication", "target", "", data); err == nil {
			t.Fatalf("empty %s accepted", field)
		}
	}
	for _, scope := range []string{"component", "target", "integration", "feature"} {
		t.Run(scope, func(t *testing.T) {
			st := publicationFixture()
			if err := publicationApply(&st, "configure_publication", "target", "", publicationConfigureData()); err != nil {
				t.Fatal(err)
			}
			switch scope {
			case "component":
				st.Applications[0].ProductID = "other"
			case "target":
				st.PublicationTargets[0].ProductID = "other"
			case "integration":
				st.Integrations[0].Repositories = []string{"other-repo"}
			case "feature":
				st.Features[0].Repositories = []string{"other-repo"}
			}
			if err := publicationApply(&st, "record_package_artifact", "bad", "integration", publicationArtifactData()); err == nil {
				t.Fatal("out-of-scope artifact accepted")
			}
		})
	}
}
func TestPublicationURLAndDigestGuards(t *testing.T) {
	st := publicationFixture()
	if err := publicationApply(&st, "configure_publication", "target", "", publicationConfigureData()); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"http://registry.test/x", "https://user:secret@registry.test/x", "https://registry.test/x?token=secret", "https://registry.test/x#secret", "https://"} {
		for _, field := range []string{"uri", "build_url"} {
			data := publicationArtifactData()
			data[field] = bad
			if err := publicationApply(&st, "record_package_artifact", "bad", "", data); err == nil {
				t.Fatalf("unsafe %s accepted", field)
			}
		}
		data := publicationConfigureData()
		data["registry_url"] = bad
		if err := publicationApply(&st, "configure_publication", "bad", "", data); err == nil {
			t.Fatal("unsafe registry URL accepted")
		}
	}
	for _, field := range []string{"source_commit", "checksum", "version"} {
		data := publicationArtifactData()
		data[field] = " "
		if err := publicationApply(&st, "record_package_artifact", "bad", "", data); err == nil {
			t.Fatalf("invalid %s accepted", field)
		}
	}
	if len(st.PackageArtifacts) != 0 {
		t.Fatal("invalid records appended")
	}
	data := publicationArtifactData()
	delete(data, "build_url")
	if err := publicationApply(&st, "record_package_artifact", "manual", "", data); err != nil {
		t.Fatal("attested external build should not require CI URL:", err)
	}
}
