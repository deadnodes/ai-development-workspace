package application

import (
	"context"
	"encoding/json"
	"os"
	osexec "os/exec"
	"path/filepath"
	"releasecontrol/internal/domain"
	"strings"
	"testing"
)

func TestWorkspaceScanKnowledgeAndPortableSetup(t *testing.T) {
	ctx := context.Background()
	s, m := fixture(t)
	root := t.TempDir()
	repo := filepath.Join(root, "service")
	if e := os.Mkdir(repo, 0700); e != nil {
		t.Fatal(e)
	}
	for _, args := range [][]string{{"init", repo}, {"-C", repo, "remote", "add", "origin", "https://example.test/team/service.git"}} {
		if b, e := osexec.Command("git", args...).CombinedOutput(); e != nil {
			t.Fatalf("%s: %v", b, e)
		}
	}
	if e := os.WriteFile(filepath.Join(repo, "AGENTS.md"), []byte("Service contract: billing API; no execution."), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := s.ScanWorkspace(ctx, "p", "agent"); e == nil {
		t.Fatal("unconfigured filesystem allowed")
	}
	s.SetWorkspaceRoot(root)
	if _, e := s.ScanWorkspace(ctx, "p", "agent"); e != nil {
		t.Fatal(e)
	}
	if _, e := s.ScanWorkspace(ctx, "p", "agent"); e != nil {
		t.Fatal(e)
	}
	if len(m.state.Repositories) != 1 {
		t.Fatalf("scan duplicated repositories: %d", len(m.state.Repositories))
	}
	r := m.state.Repositories[0]
	k := domain.ProjectKnowledge{Overview: "Product", Instructions: "Read relevant area contracts", Parameters: map[string]string{"dev_port": "8080"}, Areas: []domain.ProductArea{{ID: "practice", Name: "Practice"}, {ID: "billing", Name: "Billing", ParentID: "practice", RepositoryIDs: []string{r.ID}}, {ID: "learning", Name: "Learning"}}, Relationships: []domain.AreaRelationship{{From: "billing", To: "learning", Type: "PROVIDES_TO", Contract: "Usage events"}}}
	if _, e := s.SetProjectKnowledge(ctx, "p", "agent", k); e != nil {
		t.Fatal(e)
	}
	k.Areas[0].ParentID = "billing"
	if _, e := s.SetProjectKnowledge(ctx, "p", "agent", k); e == nil {
		t.Fatal("cycle accepted")
	}
	k.Areas[0].ParentID = ""
	exec(t, s, domain.Command{Action: "create_application", ID: "app", ProductID: "p", Data: map[string]any{"name": "API", "repository_id": r.ID, "parameters": map[string]string{"port": "8080"}}})
	exec(t, s, domain.Command{Action: "create_environment", ID: "local", ProductID: "p", Data: map[string]any{"name": "Local", "parameters": map[string]string{"context": "kind-local"}}})
	portable, e := s.ExportWorkspace(ctx, "p")
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(portable)
	if strings.Contains(string(raw), root) {
		t.Fatal("machine path leaked")
	}
	if e := os.Rename(repo, filepath.Join(t.TempDir(), "removed")); e != nil {
		t.Fatal(e)
	}
	if _, e := s.ScanWorkspace(ctx, "p", "agent"); e != nil {
		t.Fatal(e)
	}
	current, e := s.ProjectContext(ctx, "p")
	if e != nil {
		t.Fatal(e)
	}
	if len(current.(map[string]any)["local_checkouts"].([]domain.LocalCheckout)) != 0 {
		t.Fatal("removed repository still advertised as current local checkout")
	}
	destination := New(&memoryStore{state: domain.EmptyState()})
	out, e := destination.ImportWorkspace(ctx, "agent/import", portable)
	if e != nil {
		t.Fatal(e)
	}
	dst, e := destination.State(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if dst.Applications[0].Parameters["port"] != "8080" || dst.Environments[0].Parameters["context"] != "kind-local" {
		t.Fatal("portable setup lost parameters")
	}
	if len(dst.Features) != 0 || len(dst.Operations) != 0 || len(dst.ProjectKnowledge) != 1 {
		t.Fatal("import included history or lost knowledge")
	}
	contextJSON, _ := json.Marshal(out)
	if !strings.Contains(string(contextJSON), "billing API") || !strings.Contains(string(contextJSON), "https://example.test/team/service.git") {
		t.Fatal("agent entrypoint lacks clone/docs")
	}
	if _, e := destination.ImportWorkspace(ctx, "agent", portable); e == nil {
		t.Fatal("duplicate import accepted")
	}
	portable.Product.ID = "second"
	portable.Knowledge.Areas = nil
	portable.Knowledge.Relationships = nil
	if _, e := destination.ImportWorkspace(ctx, "agent", portable); e == nil {
		t.Fatal("global repository ID collision accepted")
	}
	after, _ := destination.State(ctx)
	if len(after.Products) != 1 {
		t.Fatal("failed import partially applied")
	}
}

func TestMatchWorkspacePreservesRegisteredIdentityAndScope(t *testing.T) {
	s, m := fixture(t)
	root := t.TempDir()
	s.SetWorkspaceRoot(root)
	for _, name := range []string{"different-directory-name", "unrelated"} {
		p := filepath.Join(root, name)
		for _, args := range [][]string{{"init", p}, {"-C", p, "remote", "add", "origin", "git@github.com:DeadNodes/" + name + ".git"}} {
			if b, e := osexec.Command("git", args...).CombinedOutput(); e != nil {
				t.Fatalf("%s %v", b, e)
			}
		}
	}
	p := filepath.Join(root, "different-directory-name")
	if b, e := osexec.Command("git", "-C", p, "remote", "set-url", "origin", "git@github.com:DeadNodes/pp-back.git").CombinedOutput(); e != nil {
		t.Fatalf("%s %v", b, e)
	}
	m.state.Repositories = []domain.Repository{{Meta: domain.Meta{ID: "remote-id", ProductID: "p"}, Name: "pp-back", URL: "https://github.com/deadnodes/pp-back", Role: "APPLICATION", RegisteredRepositoryID: "registry-id"}, {Meta: domain.Meta{ID: "other", ProductID: "other"}, URL: "https://github.com/deadnodes/unrelated", Role: "APPLICATION"}}
	for n := 0; n < 2; n++ {
		v, e := s.MatchWorkspace(context.Background(), "p", "agent")
		if e != nil {
			t.Fatal(e)
		}
		cc := v.(map[string]any)["local_checkouts"].([]domain.LocalCheckout)
		if len(cc) != 1 || cc[0].RepositoryID != "remote-id" || cc[0].RelativePath != "different-directory-name" {
			t.Fatalf("bad matches %+v", cc)
		}
	}
	if len(m.state.Repositories) != 2 || m.state.Repositories[0].URL != "https://github.com/deadnodes/pp-back" || m.state.Repositories[0].RegisteredRepositoryID != "registry-id" {
		t.Fatal("registry modified or unrelated repos imported")
	}
	if b, e := osexec.Command("git", "-C", p, "remote", "set-url", "origin", "https://github.com/other/pp-back.git").CombinedOutput(); e != nil {
		t.Fatalf("%s %v", b, e)
	}
	v, e := s.MatchWorkspace(context.Background(), "p", "agent")
	if e != nil {
		t.Fatal(e)
	}
	if len(v.(map[string]any)["local_checkouts"].([]domain.LocalCheckout)) != 0 {
		t.Fatal("rebound unrelated remote by directory")
	}
}

func TestKnowledgeGraphUpsertSearchAndCorrection(t *testing.T) {
	s, m := fixture(t)
	ctx := context.Background()
	node := domain.KnowledgeNode{
		Meta:     domain.Meta{ID: "api-contract"},
		Kind:     "contract",
		Title:    "Learning event contract",
		Content:  "Run lifecycle events are delivered through the integration API.",
		Keywords: []string{"learning", "events", "api"},
	}
	if _, err := s.UpsertKnowledgeNode(ctx, "p", "agent/one", node); err != nil {
		t.Fatal(err)
	}
	node.Content = "Run lifecycle events use the versioned integration API."
	if _, err := s.UpsertKnowledgeNode(ctx, "p", "agent/two", node); err != nil {
		t.Fatal(err)
	}
	result, err := s.SearchKnowledge(ctx, "p", "versioned api", 20)
	if err != nil {
		t.Fatal(err)
	}
	got := result.(map[string]any)["nodes"].([]domain.KnowledgeNode)
	if len(got) != 1 || got[0].Content != node.Content || got[0].Actor != "agent/two" {
		t.Fatalf("unexpected search result: %+v", got)
	}
	if len(m.state.ProjectKnowledge) != 2 {
		t.Fatalf("expected append-only snapshots, got %d", len(m.state.ProjectKnowledge))
	}
	if m.state.Events[len(m.state.Events)-1].Action != "upsert_knowledge_node" {
		t.Fatalf("missing graph audit event: %+v", m.state.Events[len(m.state.Events)-1])
	}
	// The legacy overview editor omits graph fields; that edit must preserve nodes.
	if _, err := s.SetProjectKnowledge(ctx, "p", "human/local", domain.ProjectKnowledge{Overview: "Updated overview"}); err != nil {
		t.Fatal(err)
	}
	result, err = s.SearchKnowledge(ctx, "p", "learning", 20)
	if err != nil || len(result.(map[string]any)["nodes"].([]domain.KnowledgeNode)) != 1 {
		t.Fatalf("overview edit erased graph: %v %+v", err, result)
	}
}
