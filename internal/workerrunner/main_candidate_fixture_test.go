package workerrunner

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// This fixture models already reviewed/merged source; it never writes main in
// response to an RCP request. PR review/check acceptance remains an explicitly
// attributed RCP gate, separate from these provider-observed identities.
type mainCandidateGitFixture struct {
	*composedGitAPI
	base, aHead, bHead, mainA, mainB string
	mergedAt                         time.Time
	writes, pulls                    atomic.Int32
}

func newMainCandidateGitFixture(t *testing.T) *mainCandidateGitFixture {
	t.Helper()
	f := &mainCandidateGitFixture{composedGitAPI: &composedGitAPI{sourceGitFixture: sourceGitFixture{t: t, root: t.TempDir()}}}
	f.mergedAt = time.Now().UTC().Add(-30 * time.Minute)
	write := func(name, value string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(f.root, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module fixture/candidate\n\ngo 1.24.0\n")
	write("main.go", "package main\nimport \"fmt\"\nvar Profile=\"dev\"\nfunc main(){fmt.Println(A+B+\"-\"+Profile)}\n")
	write("a.go", "package main\nconst A=\"A0\"\n")
	write("b.go", "package main\nconst B=\"B0\"\n")
	write("main_test.go", `package main
import("os";"testing")
func TestSourceIsolation(t *testing.T){if A!="A1"||(B!="B0"&&B!="B1"){t.Fatal("source changed",A,B)};for _,k:=range []string{"RC_TOKEN","RCP_FETCH_WORKER_TOKEN","RCP_FETCH_SOURCE_TOKEN","MAIN_CANDIDATE_PUBLISH_TOKEN","RCP_MAIN_FIXTURE_KEY"}{if os.Getenv(k)!=""{t.Fatal("credential leak")}};if _,e:=os.Stat("/source/.git");!os.IsNotExist(e){t.Fatal("Git metadata leaked")};if e:=os.WriteFile("/source/a.go",[]byte("tamper"),0600);e==nil{t.Fatal("source writable")}}
`)
	write("prod_scope_test.go", "//go:build prod\n\npackage main\nimport \"testing\"\nfunc TestProductionCandidateScope(t *testing.T){if A!=\"A1\"||B!=\"B0\"||Profile!=\"prod\"{t.Fatal(\"wrong production scope/profile\",A,B,Profile)}}\n")
	f.git("init", "-b", "base")
	f.git("add", ".")
	commit := func(message string) string {
		f.git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null", "commit", "-am", message)
		return string(f.git("rev-parse", "HEAD"))
	}
	f.base = commit("Baseline")
	f.git("checkout", "-b", "feature/a")
	write("a.go", "package main\nconst A=\"A1\"\n")
	f.aHead = commit("A")
	f.mainA = string(f.git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit-tree", string(f.git("rev-parse", f.aHead+"^{tree}")), "-p", f.base, "-m", "Squash selected A"))
	f.git("branch", "main", f.mainA)
	f.git("checkout", "-b", "feature/b", f.mainA)
	write("b.go", "package main\nconst B=\"B1\"\n")
	f.bHead = commit("B")
	f.mainB = string(f.git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit-tree", string(f.git("rev-parse", f.bHead+"^{tree}")), "-p", f.mainA, "-m", "Squash later B"))
	f.git("update-ref", "refs/heads/main", f.mainB, f.mainA)
	f.git("branch", "-D", "feature/a")
	return f
}

func (f *mainCandidateGitFixture) roundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme != "https" || r.URL.Host != "api.github.com" {
		return nil, fmt.Errorf("foreign authority")
	}
	if r.Method != "GET" && !(r.Method == "POST" && r.URL.Path == "/app/installations/2/access_tokens") {
		f.writes.Add(1)
		return nil, fmt.Errorf("source writes forbidden in main candidate")
	}
	prefix := "/repos/fixture/source/pulls/"
	if r.Method == "GET" && strings.HasPrefix(r.URL.Path, prefix) {
		if r.Header.Get("Authorization") != "Bearer fixture-compose-only" {
			return nil, fmt.Errorf("wrong observer credential")
		}
		f.pulls.Add(1)
		n := strings.TrimPrefix(r.URL.Path, prefix)
		head, merge, branch, number := f.aHead, f.mainA, "feature/a", 1
		if n == "2" {
			head, merge, branch, number = f.bHead, f.mainB, "feature/b", 2
		} else if n != "1" {
			return nil, fmt.Errorf("unexpected PR")
		}
		body, _ := json.Marshal(map[string]any{"number": number, "html_url": fmt.Sprintf("https://github.com/fixture/source/pull/%d", number), "state": "closed", "merged": true, "merge_commit_sha": merge, "created_at": f.mergedAt.Add(-time.Hour), "merged_at": f.mergedAt, "head": map[string]any{"ref": branch, "sha": head, "repo": map[string]any{"id": 42}}, "base": map[string]any{"ref": "main", "sha": f.base, "repo": map[string]any{"id": 42}}})
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body)), Request: r}, nil
	}
	return f.composedGitAPI.response(r)
}
