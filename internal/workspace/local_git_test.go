package workspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func localFixture(t *testing.T) (string, string, func(...string) string) {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "repo")
	if e := os.Mkdir(path, 0700); e != nil {
		t.Fatal(e)
	}
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", path}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
		b, e := cmd.CombinedOutput()
		if e != nil {
			t.Fatalf("git %v: %s: %v", args, b, e)
		}
		return string(b)
	}
	run("init", "-b", "main")
	run("config", "user.name", "Test")
	run("config", "user.email", "test@example.test")
	run("commit", "--allow-empty", "-m", "one")
	run("remote", "add", "origin", "git@github.com:org/repo.git")
	run("update-ref", "refs/remotes/origin/main", "HEAD")
	run("branch", "--set-upstream-to=origin/main", "main")
	return root, path, run
}
func TestObserveLocalGitCachedDivergenceAndDirty(t *testing.T) {
	root, path, run := localFixture(t)
	ctx := context.Background()
	v, e := ObserveGit(ctx, root, "repo")
	if e != nil || v.Status != "CURRENT" || v.Dirty {
		t.Fatalf("%+v %v", v, e)
	}
	run("commit", "--allow-empty", "-m", "two")
	run("update-ref", "refs/remotes/origin/main", "HEAD")
	run("reset", "--hard", "HEAD~1")
	v, e = ObserveGit(ctx, root, "repo")
	if e != nil || v.Status != "BEHIND" || v.Behind != 1 || !v.RemoteRefIsCached {
		t.Fatalf("%+v %v", v, e)
	}
	if e = os.WriteFile(filepath.Join(path, "new"), []byte("untracked"), 0600); e != nil {
		t.Fatal(e)
	}
	v, e = ObserveGit(ctx, root, "repo")
	if e != nil || !v.Dirty {
		t.Fatalf("%+v %v", v, e)
	}
	if _, e = ObserveGit(ctx, root, "../outside"); e == nil {
		t.Fatal("path escape accepted")
	}
}
func TestObserveRejectsExecutableFiltersAndIncludes(t *testing.T) {
	for _, key := range []string{"filter.danger.clean", "include.path", "extensions.worktreeConfig", "core.worktree"} {
		t.Run(key, func(t *testing.T) {
			root, _, run := localFixture(t)
			run("config", key, "true")
			if _, e := ObserveGit(context.Background(), root, "repo"); e == nil {
				t.Fatal("unsafe config accepted")
			}
		})
	}
}
