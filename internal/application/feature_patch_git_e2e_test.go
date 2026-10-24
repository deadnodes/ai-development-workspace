package application

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	osExec "os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
	githubprovider "releasecontrol/internal/providers/github"
)

// Only source composition is real here. The inherited provider supplies bounded
// configuration/read fixtures; this test stops before build, registry or GitOps IO.
type featurePatchGitProvider struct {
	*fakeFlow
	github      *githubprovider.Provider
	conn        delivery.Connection
	afterEnsure func()
}

func (p *featurePatchGitProvider) ComposeSource(ctx context.Context, _ delivery.Connection, plan delivery.SourcePlan) (delivery.SourceResult, error) {
	p.plans = append(p.plans, plan)
	return p.github.ComposeSource(ctx, p.conn, plan)
}

func (p *featurePatchGitProvider) Compare(ctx context.Context, _ delivery.Connection, repository, base, head string) (delivery.CompareResult, error) {
	return p.github.Compare(ctx, p.conn, repository, base, head)
}

type featurePatchGitAPI struct {
	t                   *testing.T
	dir                 string
	mu                  sync.Mutex
	writes              []string
	merges              int
	loseRetentionCreate bool
}

func (f *featurePatchGitAPI) git(args ...string) (string, error) {
	cmd := osExec.Command("git", args...)
	cmd.Dir = f.dir
	// Ignore user Git configuration, hooks and signing in these owned fixtures.
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %v: %w: %s", args, err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

func (f *featurePatchGitAPI) mustGit(args ...string) string {
	f.t.Helper()
	out, err := f.git(args...)
	if err != nil {
		f.t.Fatal(err)
	}
	return out
}

func (f *featurePatchGitAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	respond := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	fail := func(err error) {
		f.t.Errorf("fixture API %s %s: %v", r.Method, r.URL.Path, err)
		w.WriteHeader(http.StatusInternalServerError)
	}
	if r.Method == "POST" && r.URL.Path == "/app/installations/2/access_tokens" {
		respond(map[string]any{"token": "fixture-only", "expires_at": time.Now().Add(time.Hour)})
		return
	}
	if r.Header.Get("Authorization") != "Bearer fixture-only" {
		fail(fmt.Errorf("missing installation token"))
		return
	}
	if r.Method == "GET" && r.URL.Path == "/repositories/42" {
		respond(map[string]any{"id": 42, "full_name": "owner/source", "default_branch": "main"})
		return
	}
	p := strings.TrimPrefix(r.URL.Path, "/repos/owner/source/")
	switch {
	case r.Method == "GET" && strings.HasPrefix(p, "branches/"):
		branch := strings.TrimPrefix(p, "branches/")
		sha, err := f.git("rev-parse", "--verify", "refs/heads/"+branch)
		if err != nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		respond(map[string]any{"name": branch, "commit": map[string]string{"sha": sha}})
	case r.Method == "GET" && strings.HasPrefix(p, "git/ref/heads/"):
		branch := strings.TrimPrefix(p, "git/ref/heads/")
		sha, err := f.git("rev-parse", "--verify", "refs/heads/"+branch)
		if err != nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		respond(map[string]any{"ref": "refs/heads/" + branch, "object": map[string]string{"type": "commit", "sha": sha}})
	case r.Method == "POST" && p == "git/refs":
		var in struct{ Ref, SHA string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			fail(err)
			return
		}
		if !strings.HasPrefix(in.Ref, "refs/heads/generated/") && !strings.HasPrefix(in.Ref, "refs/heads/rcp/revisions/") {
			fail(fmt.Errorf("non-generated ref write: %s", in.Ref))
			return
		}
		if _, err := f.git("update-ref", in.Ref, in.SHA, strings.Repeat("0", 40)); err != nil {
			fail(err)
			return
		}
		f.writes = append(f.writes, in.Ref)
		if f.loseRetentionCreate && strings.HasPrefix(in.Ref, "refs/heads/rcp/revisions/") {
			f.loseRetentionCreate = false
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusCreated)
	case r.Method == "GET" && strings.HasPrefix(p, "compare/"):
		pair := strings.Split(strings.TrimPrefix(p, "compare/"), "...")
		if len(pair) != 2 {
			fail(fmt.Errorf("invalid compare"))
			return
		}
		counts, err := f.git("rev-list", "--left-right", "--count", pair[0]+"..."+pair[1])
		if err != nil {
			fail(err)
			return
		}
		fields := strings.Fields(counts)
		behind, _ := strconv.Atoi(fields[0])
		ahead, _ := strconv.Atoi(fields[1])
		status := "identical"
		if behind > 0 && ahead > 0 {
			status = "diverged"
		} else if behind > 0 {
			status = "behind"
		} else if ahead > 0 {
			status = "ahead"
		}
		difference, err := f.git("rev-list", pair[0]+".."+pair[1])
		if err != nil {
			fail(err)
			return
		}
		commits := []map[string]string{}
		for _, sha := range strings.Fields(difference) {
			commits = append(commits, map[string]string{"sha": sha})
		}
		respond(map[string]any{"base_commit": map[string]string{"sha": pair[0]}, "ahead_by": ahead, "behind_by": behind, "status": status, "commits": commits})
	case r.Method == "POST" && p == "merges":
		var in map[string]string
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			fail(err)
			return
		}
		if !strings.HasPrefix(in["base"], "generated/") {
			fail(fmt.Errorf("non-generated merge target"))
			return
		}
		if _, err := f.git("checkout", "--quiet", in["base"]); err != nil {
			fail(err)
			return
		}
		if _, err := f.git("merge", "--no-ff", "--no-edit", "-m", in["commit_message"], in["head"]); err != nil {
			fail(err)
			return
		}
		f.merges++
		f.writes = append(f.writes, "refs/heads/"+in["base"])
		w.WriteHeader(http.StatusCreated)
	case r.Method == "GET" && strings.HasPrefix(p, "git/commits/"):
		sha := strings.TrimPrefix(p, "git/commits/")
		message, err := f.git("show", "-s", "--format=%B", sha)
		if err != nil {
			fail(err)
			return
		}
		parents, err := f.git("show", "-s", "--format=%P", sha)
		if err != nil {
			fail(err)
			return
		}
		out := []map[string]string{}
		for _, parent := range strings.Fields(parents) {
			out = append(out, map[string]string{"sha": parent})
		}
		respond(map[string]any{"sha": sha, "message": message, "parents": out})
	default:
		fail(fmt.Errorf("unexpected endpoint"))
	}
}

func TestFeaturePatchRealGitPreservesOtherFeatureAndPinnedBase(t *testing.T) {
	f := &featurePatchGitAPI{t: t, dir: t.TempDir()}
	f.mustGit("init", "-b", "main")
	f.mustGit("config", "user.name", "RCP fixture")
	f.mustGit("config", "user.email", "fixture@example.invalid")
	f.mustGit("config", "commit.gpgsign", "false")
	f.mustGit("config", "core.hooksPath", os.DevNull)
	content := func(a, b, main string) string {
		return "A=" + a + "\n" + strings.Repeat("unchanged separator\n", 30) + "B=" + b + "\nMAIN=" + main + "\n"
	}
	commit := func(body, message string) string {
		t.Helper()
		if err := os.WriteFile(filepath.Join(f.dir, "shared.txt"), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		f.mustGit("add", "shared.txt")
		f.mustGit("commit", "--quiet", "-m", message)
		return f.mustGit("rev-parse", "HEAD")
	}
	m := commit(content("0", "0", "M"), "M")
	f.mustGit("checkout", "--quiet", "-b", "feature/a")
	a1 := commit(content("A1", "0", "M"), "A1")
	a2 := commit(content("A2", "0", "M"), "A2")
	f.mustGit("checkout", "--quiet", "-b", "feature/b", m)
	b1 := commit(content("0", "B1", "M"), "B1")
	b2 := commit(content("0", "B2", "M"), "B2")
	f.mustGit("checkout", "--quiet", "main")
	m2 := commit(content("0", "0", "M2"), "M2 deliberately not selected")
	protectedBefore := f.mustGit("show-ref", "--heads", "main", "feature/a", "feature/b")

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("RCP_FEATURE_PATCH_FIXTURE_KEY", string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})))
	server := httptest.NewServer(f)
	t.Cleanup(server.Close)
	t.Setenv("RCP_GITHUB_API_URL", server.URL)
	s, store, fake := flowFixture(t)
	provider := &featurePatchGitProvider{fakeFlow: fake, github: githubprovider.NewWithClient(server.Client()), conn: delivery.Connection{AppID: 1, InstallationID: 2, Owner: "owner", APIURL: server.URL, PrivateKeyRef: "env:RCP_FEATURE_PATCH_FIXTURE_KEY"}}
	s.provider = provider
	fake.main = m2
	exec(t, s, domain.Command{Action: "configure_environment", ProductID: "p", Data: map[string]any{"environment_id": "dev", "application_id": "component", "purpose": "DEV", "connection_id": "conn", "repository_id": "gitops", "ref": "main", "path": "app.yaml", "image_field": "spec.values.image.repository", "digest_field": "spec.values.image.tag", "allow_deploy": true, "gitops_mode": "PR"}})
	exec(t, s, domain.Command{Action: "create_feature", ID: "fb", ProductID: "p", Data: map[string]any{"title": "B", "goal": "Preserve B"}})
	exec(t, s, domain.Command{Action: "create_integration", ID: "ib", FeatureID: "fb", Data: map[string]any{"title": "B", "objective": "B"}})
	for _, r := range []struct{ id, integration, branch, head string }{{"a1", "i", "feature/a", a1}, {"b1", "ib", "feature/b", b1}, {"a2", "i", "feature/a", a2}, {"b2", "ib", "feature/b", b2}} {
		exec(t, s, domain.Command{Action: "record_integration_revision", ID: r.id, IntegrationID: r.integration, Data: map[string]any{"repository_id": "source", "branch": r.branch, "base_commit": m, "head_commit": r.head, "commits": strings.Fields(f.mustGit("rev-list", m+".."+r.head))}})
	}
	store.state.GitObservations = append(store.state.GitObservations, domain.GitObservation{Meta: domain.Meta{ID: "observed-m2", ProductID: "p"}, RepositoryID: "source", MainCommit: m2, ObservedAt: time.Now().UTC()})
	exec(t, s, domain.Command{Action: "plan_composition", ID: "git-baseline", ProductID: "p", Data: map[string]any{"name": "A1+B1", "environment_id": "dev", "components": []domain.CompositionComponent{{ApplicationID: "component", BaseRef: "main", BaseCommit: m, TargetBranch: "generated/dev/baseline", RevisionIDs: []string{"a1", "b1"}}}}})
	baselineJSON, _ := json.Marshal(store.state.Compositions[len(store.state.Compositions)-1])
	baselineID := flowStart(t, s, store, "git-baseline")
	sourceReady := func(o domain.ExternalOperation) bool { return len(o.Sources) == 1 && o.Sources[0].Result != nil }
	flowTickUntil(t, s, store, baselineID, sourceReady)
	baseline := *operationByID(&store.state, baselineID)
	baselineSHA := baseline.Sources[0].Result.SHA
	if got := f.mustGit("show", baselineSHA+":shared.txt"); got != strings.TrimSpace(content("A1", "B1", "M")) {
		t.Fatalf("baseline tree: %s", got)
	}
	env := environmentByID(&store.state, "dev")
	exec(t, s, domain.Command{Action: "update_environment_feature", ProductID: "p", FeatureID: "f", Data: map[string]any{"environment_id": "dev", "revision_ids": []string{"a2"}, "idempotency_key": "real-git-a2", "expected_selection": map[string]any{"composition_id": env.DesiredCompositionID, "operation_id": env.DesiredOperationID, "generation": env.SelectionGeneration}}})
	patchID := environmentByID(&store.state, "dev").DesiredOperationID
	flowTickUntil(t, s, store, patchID, sourceReady)
	patched := *operationByID(&store.state, patchID)
	source := patched.Sources[0]
	if source.Plan.BaseSHA != m || !reflect.DeepEqual(source.Plan.HeadSHAs, []string{a2, b1}) || source.Plan.OperationID != patchID || source.Plan.Repository != "42" || source.Result.Branch != source.Plan.TargetBranch || source.Plan.TargetBranch == baseline.Sources[0].Plan.TargetBranch {
		t.Fatalf("incorrect pinned source provenance: %+v", source)
	}
	if got := f.mustGit("show", source.Result.SHA+":shared.txt"); got != strings.TrimSpace(content("A2", "B1", "M")) {
		t.Fatalf("patch tree must exclude B2 and M2: %s", got)
	}
	if f.mustGit("rev-parse", "refs/heads/"+source.Plan.TargetBranch) != source.Result.SHA {
		t.Fatal("result SHA does not identify the generated branch")
	}
	parents := strings.Fields(f.mustGit("show", "-s", "--format=%P", source.Result.SHA))
	if len(parents) != 2 || parents[1] != b1 || f.mustGit("show", "-s", "--format=%P", parents[0]) != m+" "+a2 {
		t.Fatalf("merge parents do not prove ordered base+A2+B1: %v", parents)
	}
	for _, excluded := range []string{b2, m2} {
		if _, err := f.git("merge-base", "--is-ancestor", excluded, source.Result.SHA); err == nil {
			t.Fatalf("unselected commit %s entered patch", excluded)
		}
	}
	priorJSON, _ := json.Marshal(store.state.Compositions[0])
	if string(priorJSON) != string(baselineJSON) || f.mustGit("show", baselineSHA+":shared.txt") != strings.TrimSpace(content("A1", "B1", "M")) {
		t.Fatal("prior composition or source tree changed")
	}
	if protectedBefore != f.mustGit("show-ref", "--heads", "main", "feature/a", "feature/b") {
		t.Fatal("developer/default branch changed during composition")
	}
	// A repeated production-adapter call proves recovery from stored refs and real
	// commit parents, with no additional Git merge or source change.
	mergeCount := f.merges
	retry, err := provider.github.ComposeSource(context.Background(), provider.conn, source.Plan)
	if err != nil || retry != *source.Result || f.merges != mergeCount {
		t.Fatalf("source retry changed output or merged again: %+v %v", retry, err)
	}
	for _, ref := range f.writes {
		if !strings.HasPrefix(ref, "refs/heads/generated/") && !strings.HasPrefix(ref, "refs/heads/rcp/revisions/") {
			t.Fatalf("out-of-scope Git write: %s", ref)
		}
	}
	if len(fake.builds) != 0 || fake.advances != 0 || len(fake.applyCounts) != 0 {
		t.Fatal("source fixture crossed build/main/GitOps boundary")
	}
	t.Logf("real Git source fixture: M=%s A1=%s A2=%s B1=%s excluded B2=%s M2=%s baseline=%s patched=%s merges=%d; no build or live adoption", m, a1, a2, b1, b2, m2, baselineSHA, source.Result.SHA, f.merges)
}

func (p *featurePatchGitProvider) EnsureRef(ctx context.Context, _ delivery.Connection, repository, branch, sha string) (string, error) {
	result, err := p.github.EnsureRef(ctx, p.conn, repository, branch, sha)
	if p.afterEnsure != nil {
		hook := p.afterEnsure
		p.afterEnsure = nil
		hook()
	}
	return result, err
}
func (p *featurePatchGitProvider) ObserveBranch(ctx context.Context, _ delivery.Connection, repository, branch string) (delivery.BranchInfo, error) {
	return p.github.ObserveBranch(ctx, p.conn, repository, branch)
}
func (p *featurePatchGitProvider) VerifyComposedSource(ctx context.Context, _ delivery.Connection, plan delivery.SourcePlan, result delivery.SourceResult) error {
	return p.github.VerifyComposedSource(ctx, p.conn, plan, result)
}
