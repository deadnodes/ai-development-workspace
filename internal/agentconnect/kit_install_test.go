package agentconnect

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"releasecontrol/internal/agentguide"
)

func TestBothSkillsAdoptIdenticalRawAndRejectUnmanagedDifferences(t *testing.T) {
	for _, path := range []string{".agents/skills/rcp-handoff/SKILL.md", ".agents/skills/rcp-delivery/SKILL.md", ".claude/skills/rcp-handoff/SKILL.md", ".claude/skills/rcp-delivery/SKILL.md"} {
		for _, identical := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/identical=%t", path, identical), func(t *testing.T) {
				server := kitServer(t, 1, "")
				root := t.TempDir()
				name := strings.Split(path, "/")[2]
				skill, ok := agentguide.Lookup(name)
				if !ok {
					t.Fatal("bad test identity")
				}
				content := skill.Content
				if !identical {
					content += "User customization\n"
				}
				writeFile(t, root, path, content)
				_, err := Install(context.Background(), Options{URL: server.URL, Workspace: root, ProductID: "p0"})
				if identical {
					if err != nil {
						t.Fatal("identical canonical skill not adopted", err)
					}
					if !bytes.HasSuffix(readFile(t, root, path), []byte(skillMarker)) {
						t.Fatal("canonical adoption lacks managed marker")
					}
					info, _ := os.Stat(filepath.Join(root, path))
					if info.Mode().Perm() != 0640 {
						t.Fatal("adoption changed mode")
					}
					if _, err := Install(context.Background(), Options{URL: server.URL, Workspace: root, ProductID: "p0"}); err != nil {
						t.Fatal("adoption not idempotent", err)
					}
				} else {
					if err == nil {
						t.Fatal("custom unmanaged skill overwritten")
					}
					if string(readFile(t, root, path)) != content {
						t.Fatal("unmanaged bytes changed")
					}
					for _, other := range installPaths() {
						if other != path {
							if _, err := os.Stat(filepath.Join(root, other)); !os.IsNotExist(err) {
								t.Fatal("partial install before conflict", other)
							}
						}
					}
				}
			})
		}
	}
}

func TestBothSkillIdentitiesAndChecksumsValidatedBeforeWrites(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cases := []struct {
		name      string
		change    func(*agentguide.Kit)
		duplicate bool
	}{
		{name: "missing delivery", change: func(k *agentguide.Kit) { k.DeliverySkill = agentguide.Skill{} }},
		{name: "missing handoff", change: func(k *agentguide.Kit) { k.Skill = agentguide.Skill{} }},
		{name: "duplicate identity", change: func(k *agentguide.Kit) { k.DeliverySkill = k.Skill }},
		{name: "path identity", change: func(k *agentguide.Kit) { k.DeliverySkill.Name = "../rcp-delivery" }},
		{name: "corrupt delivery hash", change: func(k *agentguide.Kit) { k.DeliverySkill.SHA256 = strings.Repeat("a", 64) }},
		{name: "corrupt handoff hash", change: func(k *agentguide.Kit) { k.Skill.SHA256 = strings.Repeat("a", 64) }},
		{name: "empty content", change: func(k *agentguide.Kit) {
			k.DeliverySkill.Content = ""
			k.DeliverySkill.SHA256 = fmt.Sprintf("%x", sha256.Sum256(nil))
		}},
		{name: "wrong frontmatter", change: func(k *agentguide.Kit) {
			k.DeliverySkill.Content = k.Skill.Content
			k.DeliverySkill.SHA256 = k.Skill.SHA256
		}},
		{name: "missing instructions", change: func(k *agentguide.Kit) { k.Instructions = "" }},
		{name: "reserved markers", change: func(k *agentguide.Kit) { k.Instructions = agentsStart }},
		{name: "duplicate kit field", change: func(*agentguide.Kit) {}, duplicate: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kit := agentguide.Get()
			tc.change(&kit)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/agent-kit" {
					t.Error("invalid kit reached discovery/context")
					w.WriteHeader(500)
					return
				}
				data, _ := json.Marshal(kit)
				if tc.duplicate {
					data = append([]byte(`{"delivery_skill":null,`), data[1:]...)
				}
				w.Write(data)
			}))
			defer server.Close()
			root := t.TempDir()
			if _, err := Install(context.Background(), Options{URL: server.URL, Workspace: root, ProductID: "p0"}); err == nil {
				t.Fatal("invalid kit accepted")
			}
			requireEmptyWorkspace(t, root)
		})
	}
}

func TestAllSevenFixedPathsRollbackAtEveryReplacement(t *testing.T) {
	for failure := 8; failure <= 14; failure++ {
		t.Run(fmt.Sprint(failure), func(t *testing.T) {
			directory := t.TempDir()
			root, err := os.OpenRoot(directory)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			var plans []filePlan
			for _, path := range installPaths() {
				writeFile(t, directory, path, "original")
				plan, err := readPlan(root, path)
				if err != nil {
					t.Fatal(err)
				}
				plan.after = []byte("replacement")
				plans = append(plans, plan)
			}
			ctx := &replacementFailureContext{Context: context.Background(), failAt: failure}
			if err := commitPlans(ctx, root, plans); err == nil {
				t.Fatal("injected failure ignored")
			}
			for _, path := range installPaths() {
				if string(readFile(t, directory, path)) != "original" {
					t.Fatal("partial seven-file replacement survived", path)
				}
				info, _ := os.Stat(filepath.Join(directory, path))
				if info.Mode().Perm() != 0640 {
					t.Fatal("rollback changed mode")
				}
			}
			regular := 0
			err = filepath.WalkDir(directory, func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if strings.HasPrefix(d.Name(), ".rcp-connect-") {
					t.Error("temporary file remained", path)
				}
				if d.Type().IsRegular() {
					regular++
				}
				return nil
			})
			if err != nil || regular != 7 {
				t.Fatal("rollback left unexpected files", regular, err)
			}
		})
	}
}

type replacementFailureContext struct {
	context.Context
	calls, failAt int
}

func (c *replacementFailureContext) Err() error {
	c.calls++
	if c.calls >= c.failAt {
		return context.Canceled
	}
	return nil
}

func TestSkillTargetSymlinkRefusesAllWrites(t *testing.T) {
	server := kitServer(t, 1, "")
	for _, path := range []string{".agents/skills/rcp-handoff/SKILL.md", ".agents/skills/rcp-delivery/SKILL.md", ".claude/skills/rcp-handoff/SKILL.md", ".claude/skills/rcp-delivery/SKILL.md"} {
		root := t.TempDir()
		outside := filepath.Join(t.TempDir(), "owned")
		if err := os.WriteFile(outside, []byte("outside"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, path)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(root, path)); err != nil {
			t.Fatal(err)
		}
		if _, err := Install(context.Background(), Options{URL: server.URL, Workspace: root, ProductID: "p0"}); err == nil {
			t.Fatal("symlink target accepted")
		}
		if string(readFile(t, filepath.Dir(outside), "owned")) != "outside" {
			t.Fatal("symlink destination altered")
		}
		for _, other := range installPaths() {
			if other != path {
				if _, err := os.Stat(filepath.Join(root, other)); !os.IsNotExist(err) {
					t.Fatal("partial install before symlink refusal", other)
				}
			}
		}
	}
}

func TestFreshSevenPathInstallRollsBackAllDirectories(t *testing.T) {
	for failure := 8; failure <= 14; failure++ {
		t.Run(fmt.Sprint(failure), func(t *testing.T) {
			directory := t.TempDir()
			root, err := os.OpenRoot(directory)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			var plans []filePlan
			for _, path := range installPaths() {
				plan, err := readPlan(root, path)
				if err != nil {
					t.Fatal(err)
				}
				plan.after = []byte("new installation")
				plans = append(plans, plan)
			}
			if err := commitPlans(&replacementFailureContext{Context: context.Background(), failAt: failure}, root, plans); err == nil {
				t.Fatal("injected fresh-install failure ignored")
			}
			requireEmptyWorkspace(t, directory)
		})
	}
}

func TestSavedProfileBothSkillsRemainSecretFree(t *testing.T) {
	const secret = "fixture-profile-token-never-installed"
	server := kitServer(t, 1, secret)
	if err := saveCredential(server.URL, "default", secret); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	binding, err := Install(context.Background(), Options{URL: server.URL, Workspace: root, Executable: executable})
	if err != nil || binding.Profile != "default" {
		t.Fatal("saved profile semantics changed", binding, err)
	}
	for _, path := range installPaths() {
		if bytes.Contains(readFile(t, root, path), []byte(secret)) {
			t.Fatal("saved credential persisted", path)
		}
	}
}
