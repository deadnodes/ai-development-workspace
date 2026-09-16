package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.test", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.test")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}
func scanRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root
}
func initRepo(t *testing.T, root, name string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "init", "-b", "main")
	runGit(t, dir, "-c", "core.hooksPath=/dev/null", "commit", "--allow-empty", "-m", "Initial")
	return dir
}
func put(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}
func TestScanLocalRepositoryAndSanitizedRemote(t *testing.T) {
	root := scanRoot(t)
	repo := initRepo(t, root, "api")
	runGit(t, repo, "remote", "add", "origin", "https://user:secret@example.test/team/api.git?token=secret#secret")
	put(t, filepath.Join(root, "AGENTS.md"), "Workspace context\n")
	put(t, filepath.Join(repo, "AGENTS.md"), "Do not execute me: $(touch SHOULD_NOT_EXIST)\n")
	// If the scanner inherited Git environment, all repositories would point here.
	t.Setenv("GIT_DIR", filepath.Join(root, "missing"))
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "remote.origin.url")
	t.Setenv("GIT_CONFIG_VALUE_0", "https://secret@example.test/wrong")
	result, err := Scan(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if result.Agents == nil || len(result.Repositories) != 1 {
		t.Fatalf("missing discovery: %+v", result)
	}
	got := result.Repositories[0]
	if got.RelativePath != "api" || got.Branch != "main" || !commitPattern.MatchString(got.Commit) || got.RemoteURL != "https://example.test/team/api.git" || len(got.Errors) > 0 {
		t.Fatalf("bad observation: %+v", got)
	}
	sum := sha256.Sum256([]byte(got.Agents.Content))
	if got.Agents.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatal("instruction provenance hash differs")
	}
	if _, err = os.Stat(filepath.Join(repo, "SHOULD_NOT_EXIST")); !os.IsNotExist(err) {
		t.Fatal("instruction executed")
	}
}
func TestScanWorktreesAndSymlinkBoundaries(t *testing.T) {
	root := scanRoot(t)
	main := initRepo(t, root, "mainrepo")
	runGit(t, main, "worktree", "add", "-b", "test-branch", filepath.Join(root, "worktree"))
	outside := scanRoot(t)
	outsideRepo := initRepo(t, outside, "external")
	if err := os.Symlink(outsideRepo, filepath.Join(root, "symlink")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "escape"), 0755); err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(root, "escape", ".git"), "gitdir: "+filepath.Join(outsideRepo, ".git")+"\n")
	put(t, filepath.Join(outside, "secret"), "private context")
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(main, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	result, err := Scan(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]Repository{}
	for _, r := range result.Repositories {
		found[r.RelativePath] = r
	}
	if len(found) != 3 {
		t.Fatalf("unexpected discovery: %+v", found)
	}
	if found["worktree"].Branch != "test-branch" || found["worktree"].Commit == "" {
		t.Fatalf("worktree missing: %+v", found["worktree"])
	}
	if found["escape"].Commit != "" || len(found["escape"].Errors) == 0 {
		t.Fatal("outside metadata read")
	}
	if found["mainrepo"].Agents != nil || len(found["mainrepo"].Errors) == 0 {
		t.Fatal("symlink instruction read")
	}
	selected, err := Scan(context.Background(), filepath.Join(root, "symlink"))
	if err != nil || selected.Root != outsideRepo || len(selected.Repositories) != 1 || selected.Repositories[0].RelativePath != "." {
		t.Fatalf("explicit root was not canonicalized: %+v %v", selected, err)
	}
}
func TestScanBoundsDependenciesDetachedAndEmptyRepositories(t *testing.T) {
	root := scanRoot(t)
	repo := initRepo(t, root, "detached")
	runGit(t, repo, "checkout", "--detach")
	put(t, filepath.Join(repo, "AGENTS.md"), strings.Repeat("x", MaxInstructionBytes+100))
	initRepo(t, root, "node_modules/dependency")
	initRepo(t, root, ".local/dependency")
	empty := filepath.Join(root, "empty")
	if err := os.Mkdir(empty, 0755); err != nil {
		t.Fatal(err)
	}
	runGit(t, empty, "init", "-b", "main")
	result, err := Scan(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Repositories) != 2 {
		t.Fatalf("dependency repositories leaked: %+v", result.Repositories)
	}
	got := result.Repositories[0]
	if got.RelativePath != "detached" || got.Branch != "" || got.Commit == "" || got.Agents == nil || !got.Agents.Truncated || len(got.Agents.Content) != MaxInstructionBytes {
		t.Fatalf("detached/bound failure: %+v", got)
	}
	if result.Repositories[1].Branch != "main" || result.Repositories[1].Commit != "" || len(result.Repositories[1].Errors) == 0 {
		t.Fatal("empty repo improperly observed")
	}
}
func TestScanDoesNotRunHooksAndHonorsCancellation(t *testing.T) {
	root := scanRoot(t)
	repo := initRepo(t, root, "repo")
	sentinel := filepath.Join(root, "hook-ran")
	hook := filepath.Join(repo, ".git", "hooks", "post-checkout")
	put(t, hook, "#!/bin/sh\ntouch '"+sentinel+"'\n")
	if err := os.Chmod(hook, 0700); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "remote", "add", "origin", "ext::sh -c touch "+sentinel)
	result, err := Scan(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if result.Repositories[0].RemoteURL != "" {
		t.Fatal("executable remote exposed")
	}
	if _, err = os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatal("hook or remote executed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = Scan(ctx, root); err != context.Canceled {
		t.Fatalf("cancellation lost: %v", err)
	}
}
func TestSanitizeRemote(t *testing.T) {
	for _, bad := range []string{"/local/path", "file:///private/repo", "ext::command", "https://host/path\nsecret", "git@host:repo?token=secret"} {
		if _, ok := sanitizeRemote(bad); ok {
			t.Fatalf("unsafe remote accepted %q", bad)
		}
	}
	for input, want := range map[string]string{"git@github.com:team/repo.git": "git@github.com:team/repo.git", "ssh://git:secret@github.com/team/repo.git": "ssh://git@github.com/team/repo.git", "https://token@github.com/team/repo.git?token=secret": "https://github.com/team/repo.git"} {
		got, ok := sanitizeRemote(input)
		if !ok || got != want {
			t.Fatalf("sanitize %q = %q", input, got)
		}
	}
}

func TestScanIncludesNestedReposInsideWorkspaceCheckout(t *testing.T) {
	root := scanRoot(t)
	runGit(t, root, "init", "-b", "workspace-main")
	runGit(t, root, "-c", "core.hooksPath=/dev/null", "commit", "--allow-empty", "-m", "Workspace metadata")
	initRepo(t, root, "products/api")
	initRepo(t, root, "products/api/packages/shared")
	initRepo(t, root, "products/api/node_modules/ignored")
	result, err := Scan(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if result.Truncated {
		t.Fatal("ordinary workspace truncated")
	}
	names := []string{}
	for _, repo := range result.Repositories {
		names = append(names, repo.RelativePath)
	}
	if strings.Join(names, "|") != ".|products/api|products/api/packages/shared" {
		t.Fatalf("nested repositories missing: %v", names)
	}
}
func TestScanDepthBoundIsExplicit(t *testing.T) {
	root := scanRoot(t)
	nested := strings.Repeat("nested/", MaxDepth+1)
	initRepo(t, root, nested)
	result, err := Scan(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Truncated || len(result.Warnings) == 0 || len(result.Repositories) != 0 {
		t.Fatalf("depth limit not visible: %+v", result)
	}
}
