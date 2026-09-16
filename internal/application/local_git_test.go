package application

import (
	"context"
	"os"
	osexec "os/exec"
	"path/filepath"
	"releasecontrol/internal/domain"
	"strings"
	"testing"
)

func TestLocalGitPlanAndResultAreScopedAndObserved(t *testing.T) {
	ctx := context.Background()
	s, m := fixture(t)
	root := t.TempDir()
	path := filepath.Join(root, "repo")
	if e := os.Mkdir(path, 0700); e != nil {
		t.Fatal(e)
	}
	run := func(args ...string) string {
		t.Helper()
		cmd := osexec.Command("git", append([]string{"-C", path}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
		b, e := cmd.CombinedOutput()
		if e != nil {
			t.Fatalf("git %v: %s %v", args, b, e)
		}
		return strings.TrimSpace(string(b))
	}
	run("init", "-b", "main")
	run("config", "user.name", "Test")
	run("config", "user.email", "test@example.test")
	run("commit", "--allow-empty", "-m", "one")
	head := run("rev-parse", "HEAD")
	run("remote", "add", "origin", "git@github.com:org/repo.git")
	run("commit", "--allow-empty", "-m", "two")
	target := run("rev-parse", "HEAD")
	run("update-ref", "refs/remotes/origin/main", target)
	run("branch", "--set-upstream-to=origin/main", "main")
	run("reset", "--hard", head)
	root, _ = filepath.EvalSymlinks(root)
	s.SetWorkspaceRoot(root)
	m.state.Repositories = append(m.state.Repositories, domain.Repository{Meta: domain.Meta{ID: "r", ProductID: "p"}, URL: "https://github.com/org/repo"})
	m.state.LocalCheckouts = append(m.state.LocalCheckouts, domain.LocalCheckout{Meta: domain.Meta{ID: "checkout", ProductID: "p"}, RepositoryID: "r", Workspace: root, RelativePath: "repo"})
	if _, e := s.LocalGitState(ctx, "other", "checkout"); e == nil {
		t.Fatal("cross-product allowed")
	}
	req := LocalGitRequest{ProductID: "p", CheckoutID: "checkout", Actor: "agent", Mode: "FAST_FORWARD", ExpectedHEAD: head}
	plan, e := s.PlanLocalGitSync(ctx, req)
	if e != nil {
		t.Fatal(e)
	}
	if plan.Status != "PLANNED" || plan.Executor != "EXTERNAL_AGENT" || run("rev-parse", "HEAD") != head {
		t.Fatal("plan performed mutation or claimed completion")
	}
	req.PlanID = plan.ID
	if _, e = s.RecordLocalGitSync(ctx, req); e == nil {
		t.Fatal("unexecuted fastforward accepted")
	}
	run("merge", "--ff-only", target)
	if _, e = s.RecordLocalGitSync(ctx, req); e != nil {
		t.Fatal(e)
	}
	if m.state.Events[len(m.state.Events)-1].Action != "record_local_git_sync" {
		t.Fatal("missing audit")
	}
	req.Mode = "FETCH"
	req.ExpectedHEAD = target
	plan, e = s.PlanLocalGitSync(ctx, req)
	if e != nil {
		t.Fatal(e)
	}
	req.PlanID = plan.ID
	if _, e = s.RecordLocalGitSync(ctx, req); e != nil {
		t.Fatal(e)
	}
	req.ExpectedHEAD = head
	if _, e = s.PlanLocalGitSync(ctx, req); e == nil {
		t.Fatal("stale HEAD allowed")
	}
	run("remote", "set-url", "origin", "https://github.com/other/repo")
	if _, e = s.LocalGitState(ctx, "p", "checkout"); e == nil {
		t.Fatal("origin mismatch allowed")
	}
}
