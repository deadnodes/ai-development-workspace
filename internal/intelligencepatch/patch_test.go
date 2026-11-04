package intelligencepatch

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	testBase = "1111111111111111111111111111111111111111"
	testHead = "2222222222222222222222222222222222222222"
)

func hash(s string) string {
	v := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(v[:])
}

func proposal(edits ...Edit) Proposal {
	return Proposal{BaseSHA: testBase, HeadSHA: testHead, Edits: edits}
}

func write(t *testing.T, root, name, content string, mode os.FileMode) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(full, mode); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, root, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func assertNoStages(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(name string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.HasPrefix(d.Name(), ".rcp-intelligence-") {
			t.Errorf("unexpected staging file %s", name)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestApplyMultipleReadOnlyFilesAndPreserveModes(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/one.go", "package old\n", 0444)
	write(t, root, "src/two.py", "old = True\n", 0555)
	write(t, root, "untouched.md", "unchanged\n", 0640)
	p := proposal(
		Edit{Path: "src/one.go", BeforeHash: hash("package old\n"), Content: "package resolved\n// Привет\n"},
		Edit{Path: "src/two.py", BeforeHash: hash("old = True\n"), Content: "resolved = True\n"},
	)
	if err := Apply(root, p, testBase, testHead, []string{"src/one.go", "src/two.py"}); err != nil {
		t.Fatal(err)
	}
	for i, edit := range p.Edits {
		if got := read(t, root, edit.Path); got != edit.Content {
			t.Fatalf("wrong replacement %q", got)
		}
		info, err := os.Stat(filepath.Join(root, edit.Path))
		if err != nil {
			t.Fatal(err)
		}
		want := []os.FileMode{0444, 0555}[i]
		if info.Mode().Perm() != want {
			t.Fatalf("mode = %o, want %o", info.Mode().Perm(), want)
		}
	}
	if got := read(t, root, "untouched.md"); got != "unchanged\n" {
		t.Fatal("unapproved file changed")
	}
	assertNoStages(t, root)
}

func TestValidateRejectsUnsafeProposals(t *testing.T) {
	valid := func() Proposal { return proposal(Edit{Path: "src/main.go", BeforeHash: hash("old"), Content: "new"}) }
	tests := []struct {
		name   string
		change func(*Proposal)
		allow  []string
	}{
		{"wrong base", func(p *Proposal) { p.BaseSHA = strings.Repeat("3", 40) }, nil},
		{"wrong head", func(p *Proposal) { p.HeadSHA = strings.Repeat("3", 40) }, nil},
		{"invalid source", func(p *Proposal) { p.HeadSHA = "HEAD" }, nil},
		{"no files", func(p *Proposal) { p.Edits = nil }, nil},
		{"too many files", func(p *Proposal) { p.Edits = append(p.Edits, p.Edits[0], p.Edits[0], p.Edits[0]) }, nil},
		{"duplicate files", func(p *Proposal) { p.Edits = append(p.Edits, p.Edits[0]) }, nil},
		{"unapproved", func(p *Proposal) { p.Edits[0].Path = "src/other.go" }, nil},
		{"different case", func(p *Proposal) { p.Edits[0].Path = "src/Main.go" }, nil},
		{"bad hash", func(p *Proposal) { p.Edits[0].BeforeHash = "deadbeef" }, nil},
		{"uppercase hash", func(p *Proposal) { p.Edits[0].BeforeHash = "sha256:" + strings.Repeat("A", 64) }, nil},
		{"binary", func(p *Proposal) { p.Edits[0].Content = "new\x00data" }, nil},
		{"control", func(p *Proposal) { p.Edits[0].Content = "new\x1b[32m" }, nil},
		{"invalid utf8", func(p *Proposal) { p.Edits[0].Content = string([]byte{255}) }, nil},
		{"oversized", func(p *Proposal) { p.Edits[0].Content = strings.Repeat("x", MaxBytes+1) }, nil},
		{"unresolved conflict", func(p *Proposal) { p.Edits[0].Content = "<<<<<<< ours\nnew\n=======\nold\n>>>>>>> theirs\n" }, nil},
		{"unresolved diff3 base", func(p *Proposal) { p.Edits[0].Content = "||||||| base\nstill unresolved\n" }, nil},
		{"duplicate allowlist", func(*Proposal) {}, []string{"src/main.go", "src/main.go"}},
		{"casefold allowlist", func(*Proposal) {}, []string{"src/main.go", "src/Main.go"}},
		{"unsafe allowlist", func(*Proposal) {}, []string{"src/main.go", "../outside.go"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := valid()
			tt.change(&p)
			allow := tt.allow
			if allow == nil {
				allow = []string{"src/main.go"}
			}
			if err := Validate(p, testBase, testHead, allow); err == nil {
				t.Fatal("unsafe proposal accepted")
			}
		})
	}
}

func TestValidateRejectsReservedAndNonportablePathsEvenWhenAllowed(t *testing.T) {
	for _, name := range []string{
		"../outside.go", "/tmp/outside.go", "src/../main.go", "src//main.go", "./main.go", "src\\main.go",
		".git/hooks/main.go", ".github/workflows/main.go", "src/.hidden/main.go", "src/main.go.", "src/ünicode.go", "src/a:b.go", "src/space name.go", "src/NUL.go", "src/COM1.go", "aux/a.go",
		"secrets/main.go", "src/credentials.go", "src/password.go", "src/auth.go", "src/authz.go", "src/security.go", "src/crypto.go", "src/config.go", "src/token.go",
		"migrations/001.go", "node_modules/a.js", "vendor/a.go", "deps/a.go", "go.mod", "go.sum", "requirements.txt", "package.json", "Cargo.toml", "src/main.sql", "build.sh", "deploy.yaml", "config.json", ".env",
	} {
		t.Run(name, func(t *testing.T) {
			p := proposal(Edit{Path: name, BeforeHash: hash("old"), Content: "new"})
			if err := Validate(p, testBase, testHead, []string{name}); err == nil {
				t.Fatal("reserved path accepted")
			}
		})
	}
}

func TestCombinedTextLimitExactBoundaryAndFullSourceIDs(t *testing.T) {
	root := t.TempDir()
	before := strings.Repeat("x", MaxBytes/2)
	after := strings.Repeat("y", MaxBytes/2)
	write(t, root, "main.go", before, 0444)
	p := proposal(Edit{Path: "main.go", BeforeHash: hash(before), Content: after})
	p.BaseSHA = strings.Repeat("a", 64)
	p.HeadSHA = strings.Repeat("b", 64)
	if err := Apply(root, p, p.BaseSHA, p.HeadSHA, []string{"main.go"}); err != nil {
		t.Fatal(err)
	}
	if read(t, root, "main.go") != after {
		t.Fatal("boundary replacement missing")
	}
	if err := Validate(p, "base", p.HeadSHA, []string{"main.go"}); err == nil {
		t.Fatal("nonimmutable expected source accepted")
	}
	assertNoStages(t, root)
}

func TestUnchangedDetectsSameInodeContentDrift(t *testing.T) {
	root := t.TempDir()
	write(t, root, "main.go", "old", 0644)
	r, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	content, info, err := readOriginal(r, "main.go")
	if err != nil {
		t.Fatal(err)
	}
	item := &prepared{parent: r, leaf: "main.go", before: content, info: info}
	if err := unchanged(item); err != nil {
		t.Fatal(err)
	}
	write(t, root, "main.go", "new", 0644)
	if err := unchanged(item); err == nil {
		t.Fatal("same inode content drift accepted")
	}
}

func TestApplyPreflightsAllFilesBeforeMutation(t *testing.T) {
	for _, kind := range []string{"wrong hash", "missing", "directory", "symlink file", "symlink parent", "binary", "oversized", "combined bound"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			write(t, root, "one.go", "original", 0644)
			write(t, root, "src/two.go", "second", 0644)
			p := proposal(Edit{Path: "one.go", BeforeHash: hash("original"), Content: "replacement"}, Edit{Path: "src/two.go", BeforeHash: hash("second"), Content: "resolved"})
			switch kind {
			case "wrong hash":
				p.Edits[1].BeforeHash = hash("different")
			case "missing":
				if err := os.Remove(filepath.Join(root, "src/two.go")); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Remove(filepath.Join(root, "src/two.go")); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(filepath.Join(root, "src/two.go"), 0755); err != nil {
					t.Fatal(err)
				}
			case "symlink file":
				if err := os.Remove(filepath.Join(root, "src/two.go")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("../one.go", filepath.Join(root, "src/two.go")); err != nil {
					t.Fatal(err)
				}
			case "symlink parent":
				if err := os.Rename(filepath.Join(root, "src"), filepath.Join(root, "real")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("real", filepath.Join(root, "src")); err != nil {
					t.Fatal(err)
				}
			case "binary":
				write(t, root, "src/two.go", "bad\x00data", 0644)
				p.Edits[1].BeforeHash = hash("bad\x00data")
			case "oversized":
				write(t, root, "src/two.go", strings.Repeat("x", MaxBytes+1), 0644)
			case "combined bound":
				large := strings.Repeat("x", MaxBytes/2)
				write(t, root, "src/two.go", large, 0644)
				p.Edits[1].BeforeHash = hash(large)
				p.Edits[1].Content = large
			}
			if err := Apply(root, p, testBase, testHead, []string{"one.go", "src/two.go"}); err == nil {
				t.Fatal("invalid preimage accepted")
			}
			if got := read(t, root, "one.go"); got != "original" {
				t.Fatal("first file changed before all preimages passed")
			}
			assertNoStages(t, root)
		})
	}
}

func TestApplyRejectsSymlinkRoot(t *testing.T) {
	outer := t.TempDir()
	root := filepath.Join(outer, "root")
	if err := os.Mkdir(root, 0755); err != nil {
		t.Fatal(err)
	}
	write(t, root, "main.go", "old", 0644)
	link := filepath.Join(outer, "link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	p := proposal(Edit{Path: "main.go", BeforeHash: hash("old"), Content: "new"})
	if err := Apply(link, p, testBase, testHead, []string{"main.go"}); err == nil {
		t.Fatal("symlink root accepted")
	}
	if got := read(t, root, "main.go"); got != "old" {
		t.Fatal("symlink root target changed")
	}
}

func TestRollbackRestoresOriginalAndRefusesConcurrentChanges(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(map[bool]string{false: "restore", true: "preserve unknown edit"}[changed], func(t *testing.T) {
			root := t.TempDir()
			write(t, root, "main.go", "replacement", 0444)
			r, err := os.OpenRoot(root)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			backup, err := stage(r, []byte("original"), 0444)
			if err != nil {
				t.Fatal(err)
			}
			item := &prepared{parent: r, leaf: "main.go", edit: Edit{Path: "main.go", Content: "replacement"}, backup: backup, applied: true}
			if changed {
				if err := os.Chmod(filepath.Join(root, "main.go"), 0644); err != nil {
					t.Fatal(err)
				}
				write(t, root, "main.go", "unknown concurrent change", 0644)
			}
			err = rollback([]*prepared{item})
			if changed {
				if err == nil || !strings.Contains(err.Error(), backup) {
					t.Fatal("missing actionable rollback refusal")
				}
				if read(t, root, "main.go") != "unknown concurrent change" {
					t.Fatal("unknown edit overwritten")
				}
				if read(t, root, backup) != "original" {
					t.Fatal("original backup lost")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if read(t, root, "main.go") != "original" {
					t.Fatal("original not restored")
				}
				info, err := os.Stat(filepath.Join(root, "main.go"))
				if err != nil || info.Mode().Perm() != 0444 {
					t.Fatal("original mode not restored")
				}
				assertNoStages(t, root)
			}
		})
	}
}
