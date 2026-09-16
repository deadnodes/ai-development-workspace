package application

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"releasecontrol/internal/domain"
)

func TestConfigurationMirrorScopeAndRevision(t *testing.T) {
	st := domain.EmptyState()
	err := json.Unmarshal([]byte(`{
 "products":[{"id":"p","name":"A"},{"id":"other","name":"B"}],
 "applications":[{"id":"app","product_id":"p"},{"id":"foreign-app","product_id":"other"}],
 "repositories":[{"id":"repo","product_id":"p","registered_repository_id":"registered"}],
 "registered_repositories":[{"id":"registered","provider_repository_id":12,"full_name":"example/source","connection_ids":["conn","foreign-conn"]},{"id":"foreign-registry"}],
 "connection_grants":[{"id":"grant","product_id":"p","connection_id":"conn"},{"id":"foreign-grant","product_id":"other","connection_id":"foreign-conn"}],
 "github_connections":[{"id":"conn","name":"App","connected":true,"last_error":"transient","config":{"private_key_ref":"file:/run/secrets/app.pem"}},{"id":"foreign-conn"}],
 "component_builds":[{"id":"z-old","product_id":"p","application_id":"app","workflow":"old.yml"},{"id":"a-new","product_id":"p","application_id":"app","workflow":"new.yml"}],
 "environments":[{"id":"env","product_id":"p","name":"QA","runtime":{"health":"unstable"}}],
 "system_relationships":[{"id":"rel","product_id":"p","external_system_id":"external"}],
 "external_systems":[{"id":"external","name":"Dependency"},{"id":"foreign-system","name":"Private"}],
 "operations":[{"id":"private-operation","product_id":"p","error":"sensitive logs"}]
 }`), &st)
	if err != nil {
		t.Fatal(err)
	}
	first, err := productConfiguration(st, "p")
	if err != nil {
		t.Fatal(err)
	}
	bytes, _ := json.Marshal(first)
	for _, excluded := range []string{"foreign-", "private-operation", "sensitive logs", "old.yml", "unstable", "transient"} {
		if strings.Contains(string(bytes), excluded) {
			t.Fatalf("mirror leaked %s", excluded)
		}
	}
	if len(first.Configuration["component_builds"]) != 1 || first.Configuration["component_builds"][0]["workflow"] != "new.yml" {
		t.Fatal("effective build not selected")
	}
	again, _ := productConfiguration(st, "p")
	if !reflect.DeepEqual(first, again) {
		t.Fatal("export is not deterministic")
	}
	st.GitHubConnections[0].Connected = false
	st.GitHubConnections[0].LastError = "offline"
	observed, _ := productConfiguration(st, "p")
	if first.Revision != observed.Revision {
		t.Fatal("observation changed configuration revision")
	}
	st.ComponentBuilds[1].Workflow = "replacement.yml"
	changed, _ := productConfiguration(st, "p")
	if first.Revision == changed.Revision {
		t.Fatal("configuration change did not change revision")
	}
	if _, err = productConfiguration(st, "unknown"); err == nil {
		t.Fatal("missing product exported")
	}
	if st.ComponentBuilds[0].Workflow != "old.yml" {
		t.Fatal("historical configuration mutated")
	}
}
