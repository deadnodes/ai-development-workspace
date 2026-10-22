package workerrunner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"releasecontrol/internal/delivery"
	githubprovider "releasecontrol/internal/providers/github"
	"strings"
	"sync"
	"testing"
	"time"
)

// Only the test transport substitutes GitHub. The production provider retains
// its fixed HTTPS authority and performs real ref/merge/object operations.
type composedGitAPI struct {
	sourceGitFixture
	mu       sync.Mutex
	merges   int
	requests int
}

func (f *composedGitAPI) response(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests++
	if r.URL.Scheme != "https" || r.URL.Host != "api.github.com" {
		return nil, fmt.Errorf("unexpected authority")
	}
	reply := func(code int, v any) *http.Response {
		b, _ := json.Marshal(v)
		return &http.Response{StatusCode: code, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(bytes.NewReader(b)), ContentLength: int64(len(b)), Request: r}
	}
	if r.Method == "POST" && r.URL.Path == "/app/installations/2/access_tokens" {
		return reply(201, map[string]any{"token": "fixture-compose-only", "expires_at": time.Now().Add(time.Hour)}), nil
	}
	source := r.Header.Get("Authorization") == "Bearer fixture-source-only"
	if !source && r.Header.Get("Authorization") != "Bearer fixture-compose-only" {
		return nil, fmt.Errorf("wrong fixture credential")
	}
	if r.Method == "GET" && r.URL.Path == "/repositories/42" {
		return reply(200, map[string]any{"id": 42, "full_name": "fixture/source", "default_branch": "main"}), nil
	}
	p := strings.TrimPrefix(r.URL.Path, "/repos/fixture/source/")
	switch {
	case r.Method == "GET" && strings.HasPrefix(p, "git/commits/"):
		sha := strings.TrimPrefix(p, "git/commits/")
		parents := []map[string]string{}
		for _, s := range strings.Fields(string(f.git("show", "-s", "--format=%P", sha))) {
			parents = append(parents, map[string]string{"sha": s})
		}
		return reply(200, map[string]any{"sha": sha, "message": string(f.git("show", "-s", "--format=%B", sha)), "parents": parents, "tree": map[string]string{"sha": string(f.git("rev-parse", sha+"^{tree}"))}}), nil
	case source:
		return f.sourceGitFixture.response(r), nil
	case r.Method == "GET" && p == "branches":
		rows := []map[string]any{}
		for _, line := range strings.Split(string(f.git("for-each-ref", "--format=%(refname:short) %(objectname)", "refs/heads")), "\n") {
			v := strings.Fields(line)
			if len(v) == 2 {
				rows = append(rows, map[string]any{"name": v[0], "commit": map[string]string{"sha": v[1]}})
			}
		}
		return reply(200, rows), nil
	case r.Method == "GET" && (strings.HasPrefix(p, "git/ref/heads/") || strings.HasPrefix(p, "branches/")):
		branch := strings.TrimPrefix(strings.TrimPrefix(p, "git/ref/heads/"), "branches/")
		cmd := exec.Command("git", "rev-parse", "--verify", "refs/heads/"+branch)
		cmd.Dir = f.root
		cmd.Env = cleanEnv()
		out, err := cmd.Output()
		if err != nil {
			return reply(404, map[string]string{"message": "absent"}), nil
		}
		sha := strings.TrimSpace(string(out))
		if strings.HasPrefix(p, "branches/") {
			return reply(200, map[string]any{"name": branch, "commit": map[string]string{"sha": sha}}), nil
		}
		return reply(200, map[string]any{"ref": "refs/heads/" + branch, "object": map[string]string{"type": "commit", "sha": sha}}), nil
	case r.Method == "POST" && p == "git/refs":
		var v struct{ Ref, SHA string }
		if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
			return nil, err
		}
		if !strings.HasPrefix(v.Ref, "refs/heads/generated/") && !strings.HasPrefix(v.Ref, "refs/heads/rcp/revisions/") {
			return nil, fmt.Errorf("forbidden ref write %s", v.Ref)
		}
		f.git("update-ref", v.Ref, v.SHA, strings.Repeat("0", 40))
		return reply(201, map[string]string{}), nil
	case r.Method == "POST" && p == "merges":
		var v map[string]string
		if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
			return nil, err
		}
		if !strings.HasPrefix(v["base"], "generated/") {
			return nil, fmt.Errorf("forbidden merge target")
		}
		f.git("checkout", "--quiet", v["base"])
		f.git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null", "merge", "--no-ff", "--no-edit", "-m", v["commit_message"], v["head"])
		f.merges++
		return reply(201, map[string]string{"sha": string(f.git("rev-parse", "HEAD"))}), nil
	case r.Method == "GET" && strings.HasPrefix(p, "compare/"):
		v := strings.Split(strings.TrimPrefix(p, "compare/"), "...")
		if len(v) != 2 {
			return nil, fmt.Errorf("invalid compare")
		}
		counts := strings.Fields(string(f.git("rev-list", "--left-right", "--count", v[0]+"..."+v[1])))
		var behind, ahead int
		fmt.Sscan(counts[0], &behind)
		fmt.Sscan(counts[1], &ahead)
		status := "identical"
		if behind > 0 && ahead > 0 {
			status = "diverged"
		} else if behind > 0 {
			status = "behind"
		} else if ahead > 0 {
			status = "ahead"
		}
		commits := []map[string]string{}
		for _, sha := range strings.Fields(string(f.git("rev-list", v[0]+".."+v[1]))) {
			commits = append(commits, map[string]string{"sha": sha})
		}
		return reply(200, map[string]any{"base_commit": map[string]string{"sha": v[0]}, "ahead_by": ahead, "behind_by": behind, "status": status, "commits": commits}), nil
	case r.Method == "GET" && p == "pulls":
		return reply(200, []any{}), nil
	}
	return nil, fmt.Errorf("unexpected GitHub endpoint %s %s", r.Method, r.URL.Path)
}

type composedPipelineProvider struct {
	*githubprovider.Provider
	t         *testing.T
	proposals []delivery.GitOpsApply
	forbidden int
}

func (p *composedPipelineProvider) DiscoverRepositories(context.Context, delivery.Connection) ([]delivery.RepositoryInfo, error) {
	return []delivery.RepositoryInfo{{ID: 42, FullName: "fixture/source", URL: "https://github.com/fixture/source", DefaultBranch: "main"}, {ID: 43, FullName: "fixture/gitops", URL: "https://github.com/fixture/gitops", DefaultBranch: "main"}}, nil
}
func (p *composedPipelineProvider) Dispatch(context.Context, delivery.Connection, delivery.BuildRequest) (delivery.DispatchResult, error) {
	p.forbidden++
	return delivery.DispatchResult{}, fmt.Errorf("Actions forbidden")
}
func (p *composedPipelineProvider) FindRun(context.Context, delivery.Connection, delivery.BuildRequest) (delivery.RunResult, error) {
	p.forbidden++
	return delivery.RunResult{}, fmt.Errorf("Actions forbidden")
}
func (p *composedPipelineProvider) InspectArtifact(context.Context, delivery.Connection, delivery.ArtifactRequest) (delivery.ArtifactResult, error) {
	p.forbidden++
	return delivery.ArtifactResult{}, fmt.Errorf("artifact fallback forbidden")
}
func (p *composedPipelineProvider) AdvanceMain(context.Context, delivery.Connection, string, string, string, string) (delivery.SourceResult, error) {
	p.forbidden++
	return delivery.SourceResult{}, fmt.Errorf("main write forbidden")
}
func (p *composedPipelineProvider) ApplyGitOps(context.Context, delivery.Connection, delivery.GitOpsApply) (delivery.GitOpsResult, error) {
	p.forbidden++
	return delivery.GitOpsResult{}, fmt.Errorf("direct apply forbidden")
}
func (p *composedPipelineProvider) ReadGitOps(context.Context, delivery.Connection, delivery.GitOpsRequest) (delivery.GitOpsSnapshot, error) {
	return delivery.GitOpsSnapshot{HeadSHA: strings.Repeat("c", 40), BlobSHA: strings.Repeat("e", 40)}, nil
}
func (p *composedPipelineProvider) ProposeGitOps(_ context.Context, _ delivery.Connection, a delivery.GitOpsApply) (delivery.GitOpsPullRequest, error) {
	if a.Digest == "" || a.ExpectedHeadSHA != strings.Repeat("c", 40) || a.ExpectedBlobSHA != strings.Repeat("e", 40) {
		return delivery.GitOpsPullRequest{}, fmt.Errorf("unbound PR")
	}
	p.proposals = append(p.proposals, a)
	return delivery.GitOpsPullRequest{Number: len(p.proposals), URL: fmt.Sprintf("https://github.com/fixture/gitops/pull/%d", len(p.proposals)), HeadSHA: strings.Repeat("d", 40), State: "open"}, nil
}
func (p *composedPipelineProvider) ObserveGitOpsPR(_ context.Context, _ delivery.Connection, _ delivery.GitOpsApply, r delivery.GitOpsPullRequest) (delivery.GitOpsPullRequest, error) {
	return r, nil
}
