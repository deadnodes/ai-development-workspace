package transport_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"releasecontrol/internal/application"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/persistence"
	"releasecontrol/internal/transport"
)

func TestPostgresEnvironmentComposition(t *testing.T) {
	ctx := context.Background()
	db := isolatedDatabase(t)
	store, err := persistence.Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	svc := application.New(store)
	server := httptest.NewServer(transport.New(svc, transport.Options{}))
	defer server.Close()
	post := func(c domain.Command, want int) json.RawMessage {
		t.Helper()
		c.Actor = "agent/composition-test"
		if c.Data == nil {
			c.Data = map[string]any{}
		}
		b, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		response, err := http.Post(server.URL+"/api/commands", "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != want {
			t.Fatalf("%s: got %d want %d: %s", c.Action, response.StatusCode, want, body)
		}
		return body
	}
	for _, p := range []string{"p1", "p2"} {
		post(domain.Command{Action: "create_product", ID: p, Data: map[string]any{"name": p}}, 200)
		post(domain.Command{Action: "create_repository", ID: p + "-repo", ProductID: p, Data: map[string]any{"name": "Monorepo", "url": "https://example.com/" + p, "provider": "git"}}, 200)
		post(domain.Command{Action: "create_application", ID: p + "-app", ProductID: p, Data: map[string]any{"name": "API", "repository_id": p + "-repo", "path": "services/api"}}, 200)
	}
	for p, n := range map[string]int{"p1": 2, "p2": 4} {
		for i := 0; i < n; i++ {
			post(domain.Command{Action: "create_environment", ID: fmt.Sprintf("%s-env-%d", p, i), ProductID: p, Data: map[string]any{"name": fmt.Sprintf("ENV-%d", i), "cluster": "local-cluster", "namespace": fmt.Sprintf("%s-%d", p, i), "reconciled": map[string]any{"status": "ready", "commit": "previous"}, "runtime": map[string]any{"status": "healthy", "replicas": 2}}}, 200)
		}
	}
	base := strings.Repeat("a", 40)
	headA := strings.Repeat("b", 40)
	headB := strings.Repeat("c", 40)
	headNew := strings.Repeat("d", 40)
	for _, id := range []string{"a", "b"} {
		post(domain.Command{Action: "create_feature", ID: "feature-" + id, ProductID: "p1", Data: map[string]any{"title": "Feature " + id, "problem": "Independent change", "goal": "Ship independently"}}, 200)
		post(domain.Command{Action: "create_integration", ID: "integration-" + id, FeatureID: "feature-" + id, Data: map[string]any{"title": "Integration " + id, "objective": "Implement " + id, "repositories": []string{"p1-repo"}}}, 200)
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "composition-test", Version: "1"}, nil).Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL + "/mcp"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	mcpExecute := func(c domain.Command) json.RawMessage {
		t.Helper()
		c.Actor = "agent/mcp"
		if c.Data == nil {
			c.Data = map[string]any{}
		}
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "execute", Arguments: c})
		if err != nil {
			t.Fatal(err)
		}
		if result.IsError {
			t.Fatalf("MCP %s: %+v", c.Action, result.Content)
		}
		b, err := json.Marshal(result.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	revision := func(id, in, head string) domain.Command {
		return domain.Command{Action: "record_integration_revision", ID: id, IntegrationID: in, Data: map[string]any{"repository_id": "p1-repo", "branch": "feature/" + in, "base_commit": base, "head_commit": head, "commits": []string{head}}}
	}
	var original domain.IntegrationRevision
	if err = json.Unmarshal(post(revision("revision-a", "integration-a", headA), 200), &original); err != nil {
		t.Fatal(err)
	}
	mcpExecute(revision("revision-b", "integration-b", headB))
	component := func(app string) map[string]any {
		return map[string]any{"application_id": app, "base_ref": "main", "base_commit": base, "target_branch": "generated/dev", "revision_ids": []string{"revision-a", "revision-b"}}
	}
	plan := domain.Command{Action: "plan_composition", ID: "composition", ProductID: "p1", Data: map[string]any{"name": "Two independent features", "environment_id": "p1-env-0", "components": []any{component("p1-app")}}}
	var planned domain.Composition
	if err = json.Unmarshal(mcpExecute(plan), &planned); err != nil {
		t.Fatal(err)
	}
	if planned.Status != "planned" || len(planned.ApplicationSnapshots) != 1 || len(planned.RevisionSnapshots) != 2 || planned.EnvironmentSnapshot.Namespace != "p1-0" {
		t.Fatalf("incomplete immutable snapshot: %+v", planned)
	}
	// Foreign product environments/applications/repositories must never cross the composition boundary.
	post(domain.Command{Action: "create_application", ID: "foreign-app", ProductID: "p1", Data: map[string]any{"name": "Wrong scope", "repository_id": "p2-repo"}}, 422)
	post(domain.Command{Action: "plan_composition", ID: "foreign-env-plan", ProductID: "p1", Data: map[string]any{"name": "Wrong environment", "environment_id": "p2-env-0", "components": []any{component("p1-app")}}}, 422)
	post(domain.Command{Action: "plan_composition", ID: "foreign-app-plan", ProductID: "p1", Data: map[string]any{"name": "Wrong application", "environment_id": "p1-env-0", "components": []any{component("p2-app")}}}, 422)
	foreignRevision := revision("foreign-revision", "integration-a", headA)
	foreignRevision.Data["repository_id"] = "p2-repo"
	post(foreignRevision, 422)
	post(revision("revision-a", "integration-a", headNew), 422)
	post(revision("revision-a-new", "integration-a", headNew), 200)
	post(domain.Command{Action: "update_environment", ID: "p1-env-0", Data: map[string]any{"runtime": map[string]any{"status": "healthy", "replicas": 3}}}, 200)
	before, err := svc.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	selected, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "execute", Arguments: domain.Command{Action: "select_composition", ID: "composition", Actor: "agent/mcp", Data: map[string]any{}}})
	if err != nil {
		t.Fatal(err)
	}
	if selected.IsError {
		t.Fatalf("MCP select failed: %+v", selected.Content)
	}
	state, err := svc.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for i, env := range state.Environments {
		counts[env.ProductID]++
		old := before.Environments[i]
		if !reflect.DeepEqual(env.Reconciled, old.Reconciled) || !reflect.DeepEqual(env.Runtime, old.Runtime) {
			t.Fatal("selection changed observed Flux/Kubernetes state")
		}
		if env.ID == "p1-env-0" {
			if env.DesiredCompositionID != "composition" {
				t.Fatal("selected composition not desired")
			}
		} else if env.DesiredCompositionID != "" {
			t.Fatal("selection changed unrelated environment")
		}
	}
	if counts["p1"] != 2 || counts["p2"] != 4 {
		t.Fatalf("product environment isolation lost: %v", counts)
	}
	if len(state.Compositions) != 1 || !reflect.DeepEqual(state.Compositions[0], planned) {
		t.Fatal("later revision capture, environment edit or selection mutated immutable composition")
	}
	if len(state.IntegrationRevisions) != 3 {
		t.Fatal("revision history lost")
	}
	foundOriginal := false
	for _, r := range state.IntegrationRevisions {
		if r.ID == original.ID {
			foundOriginal = reflect.DeepEqual(r, original)
		}
	}
	if !foundOriginal {
		t.Fatal("original revision changed")
	}
	resumed, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "resume", Arguments: map[string]any{"feature_id": "feature-a"}})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.IsError {
		t.Fatalf("MCP resume: %+v", resumed.Content)
	}
	b, err := json.Marshal(resumed.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var resumedContext map[string]any
	if err = json.Unmarshal(b, &resumedContext); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"applications", "integration_revisions", "compositions"} {
		if _, ok := resumedContext[key]; !ok {
			t.Errorf("resume missing %s", key)
		}
	}
	if !strings.Contains(string(b), "composition") || !strings.Contains(string(b), "revision-a") {
		t.Fatal("resume omitted selected composition/revision")
	}
	// Retargeting an environment invalidates the desire and cannot select a stale target snapshot.
	post(domain.Command{Action: "update_environment", ID: "p1-env-0", Data: map[string]any{"namespace": "retargeted"}}, 200)
	post(domain.Command{Action: "select_composition", ID: "composition", Data: map[string]any{}}, 422)
	state, err = svc.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, env := range state.Environments {
		if env.ID == "p1-env-0" && env.DesiredCompositionID != "" {
			t.Fatal("retargeting retained stale desired composition")
		}
	}
	store.Close()
	reopened, err := persistence.Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	persisted, err := application.New(reopened).State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(state, persisted) {
		t.Fatal("composition/environment state changed after restart")
	}
}
