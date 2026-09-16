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
