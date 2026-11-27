package agentconnect

import (
	"bytes"
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"releasecontrol/internal/application"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/persistence"
	"releasecontrol/internal/transport"
)

func TestReadSkillDirectoryBudgetsPathsAndEmptyFiles(t *testing.T) {
	newSkill := func(t *testing.T) string {
		t.Helper()
		root := t.TempDir()
		writeFile(t, root, "SKILL.md", "---\nname: team-worker\ndescription: Worker instructions\n---\nInstructions.\n")
		return root
	}
	t.Run("entry count", func(t *testing.T) {
		root := newSkill(t)
		for index := range domain.MaxAgentSkillFiles {
			writeFile(t, root, fmt.Sprintf("empty-%02d", index), "")
		}
		if skill, err := readSkillDirectory(root); err != nil || len(skill.Files) != domain.MaxAgentSkillFiles {
			t.Fatalf("max entries rejected: %v", err)
		}
		writeFile(t, root, "extra", "")
		if _, err := readSkillDirectory(root); err == nil || !strings.Contains(err.Error(), "64 entries") {
			t.Fatalf("extra empty file not rejected: %v", err)
		}
	})
	t.Run("path bytes", func(t *testing.T) {
		root := newSkill(t)
		name := "notes.txt"
		body := strings.Repeat("a", domain.MaxAgentSkillFilesBytes-len(name))
		writeFile(t, root, name, body)
		if _, err := readSkillDirectory(root); err != nil {
			t.Fatalf("exact byte budget rejected: %v", err)
		}
		writeFile(t, root, name, body+"a")
		if _, err := readSkillDirectory(root); err == nil || !strings.Contains(err.Error(), "256 KiB") {
			t.Fatalf("path bytes not charged: %v", err)
		}
	})
}

func TestSkillPushDryRunAndUpload(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	setTestHome(t, t.TempDir())
	writeFile(t, root, "SKILL.md", "---\nname: team-worker\ndescription: Worker instructions\n---\nPut check.sh in scripts/.\n")
	writeFile(t, root, "scripts/check.sh", "echo ready\n")
	input := []string{"push", "--product", "team", "--dir", root}
	var out bytes.Buffer
	if err := RunSkill(ctx, append(input, "--url", "not-a-url", "--dry-run"), &out); err != nil || !strings.Contains(out.String(), "Would upload skill team-worker") {
		t.Fatalf("dry-run: %v %s", err, &out)
	}
	store, err := persistence.OpenSQLite(ctx, filepath.Join(t.TempDir(), "skills.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	app := application.New(store)
	if _, err := app.Execute(ctx, domain.Command{Action: "create_product", Actor: "owner", ID: "team", Data: map[string]any{"name": "Team"}}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(transport.New(app, transport.Options{}))
	defer server.Close()
	out.Reset()
	if err := RunSkill(ctx, append(input, "--url", server.URL), &out); err != nil || !strings.Contains(out.String(), "version 1, SHA-256") {
		t.Fatalf("upload: %v %s", err, &out)
	}
	value, err := app.AgentSkill(ctx, "team", "team-worker")
	if err != nil {
		t.Fatal(err)
	}
	skill := value.(domain.ProductAgentSkill)
	if skill.Body != "Put check.sh in scripts/.\n" || skill.Files["scripts/check.sh"] != "echo ready\n" || skill.FilesSHA256["scripts/check.sh"] == "" {
		t.Fatalf("stored skill: %+v", skill)
	}
	if err := os.Symlink("SKILL.md", filepath.Join(root, "linked.md")); err != nil {
		t.Fatal(err)
	}
	if err := RunSkill(ctx, append(input, "--dry-run"), &out); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("accepted symlink: %v", err)
	}
}
