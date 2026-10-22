package workerrunner

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// candidateGitOpsHTTPFixture is a GitHub HTTP peer, never a delivery provider.
// Production code performs rendering, guarded writes, recovery and observation.
type candidateGitOpsHTTPFixture struct {
	mu                          sync.Mutex
	server                      *httptest.Server
	refs                        map[string]string
	commits                     map[string]map[string]any
	trees                       map[string][]map[string]string
	blobs                       map[string]string
	initial, repo, branch, head string
	baselineDigest              string
	pr                          map[string]any
	lost, recovering            bool
	mutations                   []string
	recoveryWrites              int
}

func candidateGitHash(kind, value string) string {
	return fmt.Sprintf("%x", sha1.Sum([]byte(fmt.Sprintf("%s %d\x00%s", kind, len(value), value))))
}
func newCandidateGitOpsHTTPFixture(t *testing.T) *candidateGitOpsHTTPFixture {
	t.Helper()
	f := &candidateGitOpsHTTPFixture{refs: map[string]string{}, commits: map[string]map[string]any{}, trees: map[string][]map[string]string{}, blobs: map[string]string{}}
	f.server = httptest.NewServer(http.HandlerFunc(f.serveHTTP))
	t.Cleanup(f.server.Close)
	return f
}
func (f *candidateGitOpsHTTPFixture) configure(repo string) {
	f.configureBaseline(repo, "sha256:"+strings.Repeat("a", 64))
}
func (f *candidateGitOpsHTTPFixture) configureBaseline(repo, digest string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.repo = repo
	f.baselineDigest = digest
	// Both roles move together; comments, Unicode and unrelated resources stay.
	role := func(name string) string {
		return "apiVersion: helm.toolkit.fluxcd.io/v2\nkind: HelmRelease\nmetadata:\n  name: " + name + "\nspec:\n  values:\n    note: 'оставить # untouched'\n    image:\n      repository: '" + repo + "' # selected own repository\n      tag: 'old@sha256:" + strings.TrimPrefix(digest, "sha256:") + "' # exact scalar only\n"
	}
	f.initial = "# preserved header\n" + role("api") + "---\n" + role("worker") + "---\napiVersion: v1\nkind: ConfigMap\nmetadata: {name: untouched}\ndata: {key: 'preserve me'}\n"
	blob := candidateGitHash("blob", f.initial)
	f.blobs[blob] = f.initial
	sibling := candidateGitHash("blob", "untouched sibling\n")
	f.blobs[sibling] = "untouched sibling\n"
	tree := candidateGitHash("tree", "base"+blob)
	f.trees[tree] = []map[string]string{{"path": "dev.yaml", "mode": "100644", "type": "blob", "sha": blob}, {"path": "prod.yaml", "mode": "100644", "type": "blob", "sha": blob}, {"path": "README.md", "mode": "100644", "type": "blob", "sha": sibling}}
	base := candidateGitHash("commit", "base"+tree)
	f.commits[base] = map[string]any{"sha": base, "message": "Fixture preconfigured own registry", "tree": map[string]string{"sha": tree}, "parents": []any{}}
	f.refs["main"] = base
}
func (f *candidateGitOpsHTTPFixture) handles(r *http.Request) bool {
	return r.URL.Scheme == "https" && r.URL.Host == "api.github.com" && (r.URL.Path == "/repositories/43" || strings.HasPrefix(r.URL.Path, "/repos/fixture/gitops/"))
}
func (f *candidateGitOpsHTTPFixture) roundTrip(r *http.Request) (*http.Response, error) {
	if !f.handles(r) {
		return nil, fmt.Errorf("GitOps fixture rejects foreign authority")
	}
	// Image strings never become request destinations. Only these exact GitOps
	// API paths reach the explicitly owned local HTTP server.
	target, _ := url.Parse(f.server.URL)
	copy := r.Clone(r.Context())
	u := *r.URL
	copy.URL = &u
	copy.URL.Scheme = target.Scheme
	copy.URL.Host = target.Host
	copy.Host = target.Host
	return http.DefaultTransport.RoundTrip(copy)
}
func (f *candidateGitOpsHTTPFixture) loseNextPRReply() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lost = true
}
func (f *candidateGitOpsHTTPFixture) serveHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	reply := func(code int, v any) { w.WriteHeader(code); json.NewEncoder(w).Encode(v) }
	if r.Header.Get("Authorization") != "Bearer fixture-compose-only" {
		reply(403, map[string]string{"message": "wrong fixture GitOps credential"})
		return
	}
	if r.Method != "GET" && r.Method != "HEAD" {
		f.mutations = append(f.mutations, r.Method+" "+r.URL.Path)
		if f.recovering {
			f.recoveryWrites++
		}
	}
	if r.URL.Path == "/repositories/43" && r.Method == "GET" {
		reply(200, map[string]any{"id": 43, "full_name": "fixture/gitops", "default_branch": "main"})
		return
	}
	p := strings.TrimPrefix(r.URL.Path, "/repos/fixture/gitops/")
	switch {
	case r.Method == "GET" && strings.HasPrefix(p, "git/ref/heads/"):
		branch := strings.TrimPrefix(p, "git/ref/heads/")
		sha := f.refs[branch]
		if sha == "" {
			reply(404, map[string]string{"message": "missing ref"})
			return
		}
		reply(200, map[string]any{"ref": "refs/heads/" + branch, "object": map[string]string{"sha": sha, "type": "commit"}})
	case r.Method == "POST" && p == "git/trees":
		var v struct {
			BaseTree string `json:"base_tree"`
			Tree     []struct{ Path, Mode, Type, Content string }
		}
		if json.NewDecoder(r.Body).Decode(&v) != nil || f.trees[v.BaseTree] == nil || len(v.Tree) == 0 {
			reply(422, nil)
			return
		}
		entries := make([]map[string]string, 0, len(f.trees[v.BaseTree]))
		patches := map[string]string{}
		for _, patch := range v.Tree {
			if patch.Mode != "100644" || patch.Type != "blob" || patches[patch.Path] != "" {
				reply(422, nil)
				return
			}
			blob := candidateGitHash("blob", patch.Content)
			f.blobs[blob] = patch.Content
			patches[patch.Path] = blob
		}
		for _, old := range f.trees[v.BaseTree] {
			next := map[string]string{}
			for k, x := range old {
				next[k] = x
			}
			if blob := patches[next["path"]]; blob != "" {
				next["sha"] = blob
				delete(patches, next["path"])
			}
			entries = append(entries, next)
		}
		if len(patches) != 0 {
			reply(422, nil)
			return
		}
		encoded, _ := json.Marshal(entries)
		sha := candidateGitHash("tree", string(encoded))
		f.trees[sha] = entries
		reply(201, map[string]any{"sha": sha, "tree": entries, "truncated": false})
	case r.Method == "POST" && p == "git/commits":
		var v struct {
			Message, Tree string
			Parents       []string
		}
		if json.NewDecoder(r.Body).Decode(&v) != nil || f.trees[v.Tree] == nil || len(v.Parents) != 1 || f.commits[v.Parents[0]] == nil {
			reply(422, nil)
			return
		}
		sha := candidateGitHash("commit", v.Parents[0]+v.Tree+v.Message)
		f.commits[sha] = map[string]any{"sha": sha, "message": v.Message, "tree": map[string]string{"sha": v.Tree}, "parents": []map[string]string{{"sha": v.Parents[0]}}}
		f.head = sha
		reply(201, f.commits[sha])
	case r.Method == "POST" && p == "git/refs":
		var v struct{ Ref, SHA string }
		if json.NewDecoder(r.Body).Decode(&v) != nil || f.commits[v.SHA] == nil {
			reply(422, map[string]string{"message": "invalid ref"})
			return
		}
		branch := strings.TrimPrefix(v.Ref, "refs/heads/")
		if !strings.HasPrefix(branch, "rcp/gitops/") || f.refs[branch] != "" {
			reply(422, map[string]string{"message": "invalid branch"})
			return
		}
		f.refs[branch] = v.SHA
		f.branch = branch
		reply(201, map[string]any{"ref": v.Ref, "object": map[string]string{"sha": v.SHA, "type": "commit"}})
	case r.Method == "GET" && strings.HasPrefix(p, "contents/"):
		name := strings.TrimPrefix(p, "contents/")
		sha := r.URL.Query().Get("ref")
		if h := f.refs[sha]; h != "" {
			sha = h
		}
		blob := f.fileBlob(sha, name)
		raw, ok := f.blobs[blob]
		if !ok {
			reply(404, map[string]string{"message": "file missing"})
			return
		}
		reply(200, map[string]any{"type": "file", "sha": blob, "encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(raw))})
	case r.Method == "PUT" && strings.HasPrefix(p, "contents/"):
		var v struct{ Branch, SHA, Content, Message string }
		if json.NewDecoder(r.Body).Decode(&v) != nil {
			reply(422, nil)
			return
		}
		name := strings.TrimPrefix(p, "contents/")
		base := f.refs[v.Branch]
		if name != "dev.yaml" || !strings.HasPrefix(v.Branch, "rcp/gitops/") || v.SHA != f.fileBlob(base, name) {
			reply(409, map[string]string{"message": "wrong pinned file"})
			return
		}
		raw, e := base64.StdEncoding.DecodeString(v.Content)
		if e != nil {
			reply(422, nil)
			return
		}
		blob := candidateGitHash("blob", string(raw))
		f.blobs[blob] = string(raw)
		old := f.commits[base]["tree"].(map[string]string)["sha"]
		entries := make([]map[string]string, 0, len(f.trees[old]))
		for _, entry := range f.trees[old] {
			next := map[string]string{}
			for k, x := range entry {
				next[k] = x
			}
			if next["path"] == name {
				next["sha"] = blob
			}
			entries = append(entries, next)
		}
		tree := candidateGitHash("tree", old+blob)
		f.trees[tree] = entries
		head := candidateGitHash("commit", base+tree+v.Message)
		f.commits[head] = map[string]any{"sha": head, "message": v.Message, "tree": map[string]string{"sha": tree}, "parents": []map[string]string{{"sha": base}}}
		f.refs[v.Branch] = head
		f.head = head
		reply(200, map[string]any{"content": map[string]string{"sha": blob}, "commit": map[string]string{"sha": head, "html_url": "https://github.com/fixture/gitops/commit/" + head}})
	case r.Method == "GET" && strings.HasPrefix(p, "git/commits/"):
		v := f.commits[strings.TrimPrefix(p, "git/commits/")]
		if v == nil {
			reply(404, nil)
			return
		}
		reply(200, v)
	case r.Method == "GET" && strings.HasPrefix(p, "git/trees/"):
		sha := strings.TrimPrefix(p, "git/trees/")
		entries, ok := f.trees[sha]
		if !ok {
			reply(404, nil)
			return
		}
		reply(200, map[string]any{"sha": sha, "truncated": false, "tree": entries})
	case r.Method == "GET" && strings.HasPrefix(p, "git/blobs/"):
		sha := strings.TrimPrefix(p, "git/blobs/")
		raw, ok := f.blobs[sha]
		if !ok {
			reply(404, nil)
			return
		}
		reply(200, map[string]any{"sha": sha, "encoding": "base64", "size": len(raw), "content": base64.StdEncoding.EncodeToString([]byte(raw))})
	case r.Method == "GET" && p == "pulls":
		if f.pr == nil {
			reply(200, []any{})
			return
		}
		reply(200, []any{f.pr})
	case r.Method == "POST" && p == "pulls":
		var v struct{ Head, Base, Body string }
		if json.NewDecoder(r.Body).Decode(&v) != nil || v.Head != f.branch || v.Base != "main" || f.head == "" || f.pr != nil {
			reply(422, nil)
			return
		}
		identity := map[string]any{"id": 43, "full_name": "fixture/gitops"}
		f.pr = map[string]any{"number": 1, "html_url": "https://github.com/fixture/gitops/pull/1", "state": "open", "merged": false, "head": map[string]any{"ref": f.branch, "sha": f.head, "repo": identity}, "base": map[string]any{"ref": "main", "repo": identity}}
		if f.lost {
			f.lost = false
			f.recovering = true
			reply(500, map[string]string{"message": "fixture lost successful PR response"})
			return
		}
		reply(201, f.pr)
	case r.Method == "GET" && p == "pulls/1" && f.pr != nil:
		reply(200, f.pr)
	default:
		reply(404, map[string]string{"message": "unexpected fixture endpoint " + r.Method + " " + p})
	}
}
func (f *candidateGitOpsHTTPFixture) fileBlob(sha, name string) string {
	c := f.commits[sha]
	if c == nil {
		return ""
	}
	for _, e := range f.trees[c["tree"].(map[string]string)["sha"]] {
		if e["path"] == name {
			return e["sha"]
		}
	}
	return ""
}
func (f *candidateGitOpsHTTPFixture) assertExactPatch(t *testing.T, repo, digest string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if repo != f.repo || f.pr == nil || f.recoveryWrites != 0 || len(f.mutations) != 4 || f.mutations[0] != "POST /repos/fixture/gitops/git/trees" || f.mutations[1] != "POST /repos/fixture/gitops/git/commits" || f.mutations[2] != "POST /repos/fixture/gitops/git/refs" || f.mutations[3] != "POST /repos/fixture/gitops/pulls" {
		t.Fatalf("wrong production GitOps effects repo=%s want=%s mutations=%v recoveryWrites=%d", repo, f.repo, f.mutations, f.recoveryWrites)
	}
	expected := strings.ReplaceAll(f.initial, "'"+repo+"'", strconv.Quote(repo))
	expected = strings.ReplaceAll(expected, "'old@"+f.baselineDigest+"'", strconv.Quote("rcp@"+digest))
	actual := f.blobs[f.fileBlob(f.head, "dev.yaml")]
	if actual != expected {
		t.Fatalf("production adapter did not preserve exact private OCI YAML bytes\nactual=%s\nexpected=%s", actual, expected)
	}
	if !strings.Contains(actual, "repository: "+strconv.Quote(repo)) || strings.Contains(actual, "ghcr.io/") {
		t.Fatal("registry origin substituted")
	}
	if f.blobs[f.fileBlob(f.refs["main"], "prod.yaml")] != f.initial {
		t.Fatal("unrelated Prod file changed")
	}
}
func (f *candidateGitOpsHTTPFixture) externalMerge(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pr == nil || f.head == "" {
		t.Fatal("no exact proposal to externally merge")
	}
	f.pr["state"] = "closed"
	f.pr["merged"] = true
	f.refs["main"] = f.head
}
