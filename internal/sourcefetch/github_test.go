package sourcefetch

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"releasecontrol/internal/domain"
)

type gitFixture struct {
	dir, commit, tree string
	entries           []entry
	blobs             map[string][]byte
}

func gitRun(t *testing.T, dir string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("owned Git fixture %v: %v: %s", args, err, out)
	}
	return out
}

func realGit(t *testing.T) gitFixture {
	t.Helper()
	dir := t.TempDir()
	gitRun(t, dir, "init", "-q", "-b", "main")
	gitRun(t, dir, "config", "user.name", "Fixture")
	gitRun(t, dir, "config", "user.email", "fixture@example.invalid")
	write := func(name, data string) {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(".gitattributes", "ignored.txt export-ignore\nraw.txt export-subst\n")
	write("ignored.txt", "must remain in raw tree\n")
	write("raw.txt", "$Format:%H$\n")
	write("a.c", "ordering\n")
	write("a/nested.txt", "same blob\n")
	write("repeated.txt", "same blob\n")
	write("zz-empty.txt", "")
	write("executable.sh", "#!/bin/sh\nprintf exact\n")
	if err := os.Chmod(filepath.Join(dir, "executable.sh"), 0700); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "-c", "commit.gpgsign=false", "commit", "-qm", "base")
	gitRun(t, dir, "checkout", "-qb", "feature-a")
	write("A.txt", "feature A\n")
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "-c", "commit.gpgsign=false", "commit", "-qm", "A")
	gitRun(t, dir, "checkout", "-qb", "feature-b", "main")
	write("B.txt", "feature B\n")
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "-c", "commit.gpgsign=false", "commit", "-qm", "B")
	gitRun(t, dir, "checkout", "-qb", "generated/dev/composition", "feature-a")
	gitRun(t, dir, "-c", "commit.gpgsign=false", "merge", "--no-ff", "-qm", "A+B", "feature-b")
	gitRun(t, dir, "branch", "-D", "feature-a", "feature-b")
	f := gitFixture{dir: dir, commit: strings.TrimSpace(string(gitRun(t, dir, "rev-parse", "HEAD"))), tree: strings.TrimSpace(string(gitRun(t, dir, "rev-parse", "HEAD^{tree}"))), blobs: map[string][]byte{}}
	for _, record := range bytes.Split(gitRun(t, dir, "ls-tree", "-r", "-t", "-l", "-z", "HEAD"), []byte{0}) {
		if len(record) == 0 {
			continue
		}
		parts := bytes.SplitN(record, []byte{'\t'}, 2)
		meta := strings.Fields(string(parts[0]))
		e := entry{Mode: meta[0], Type: meta[1], SHA: meta[2], Path: string(parts[1])}
		if e.Type == "blob" {
			n, err := strconv.ParseInt(meta[3], 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			e.Size = &n
			f.blobs[e.SHA] = gitRun(t, dir, "cat-file", "blob", e.SHA)
		}
		f.entries = append(f.entries, e)
	}
	return f
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func fixtureClient(t *testing.T, f gitFixture, mode string) (*http.Client, *atomic.Int64, *atomic.Int64) {
	t.Helper()
	var calls, blobCalls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "GET" {
			t.Error("Git mutation attempted")
			w.WriteHeader(405)
			return
		}
		if r.Header.Get("Authorization") != "Bearer fixture-read-only" {
			t.Error("missing separate credential")
		}
		if mode == "redirect" {
			w.Header().Set("Location", "https://untrusted.invalid/credential")
			w.WriteHeader(302)
			return
		}
		if mode == "cancel" {
			<-r.Context().Done()
			return
		}
		switch {
		case r.URL.Path == "/repos/acme/repo":
			if mode == "case-alias-repo" {
				io.WriteString(w, `{"id":999,"ID":123,"full_name":"acme/repo"}`)
				return
			}
			if mode == "unused-case-aliases" {
				io.WriteString(w, `{"id":123,"full_name":"acme/repo","vendor":{"sha":"ignored","SHA":"also ignored"}}`)
				return
			}
			if mode == "metadata-oversize" {
				w.WriteHeader(200)
				w.(http.Flusher).Flush()
				io.CopyN(w, strings.NewReader(strings.Repeat(" ", (8<<20)+1)), (8<<20)+1)
				return
			}
			if mode == "malformed" {
				io.WriteString(w, `{"id":`)
				return
			}
			if mode == "lying-length" {
				w.Header().Set("Content-Length", "1000")
				io.WriteString(w, `{}`)
				return
			}
			id := int64(123)
			name := "acme/repo"
			if mode == "wrong-repo" {
				id++
			}
			if mode == "rename" {
				name = "acme/new"
			}
			if mode == "duplicate-json" {
				io.WriteString(w, `{"id":123,"id":123,"full_name":"acme/repo"}`)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"id": id, "full_name": name})
		case r.URL.Path == "/repos/acme/repo/git/commits/"+f.commit:
			if mode == "case-alias-commit" {
				io.WriteString(w, `{"sha":"wrong","SHA":"`+f.commit+`","tree":{"sha":"`+f.tree+`"}}`)
				return
			}
			if mode == "case-alias-commit-tree" {
				io.WriteString(w, `{"sha":"`+f.commit+`","tree":{"sha":"wrong","SHA":"`+f.tree+`"}}`)
				return
			}
			sha := f.commit
			if mode == "wrong-commit" {
				sha = strings.Repeat("1", 40)
			}
			json.NewEncoder(w).Encode(map[string]any{"sha": sha, "tree": map[string]string{"sha": f.tree}, "author": map[string]string{"name": "transient fixture author"}})
		case r.URL.Path == "/repos/acme/repo/git/trees/"+f.tree:
			if mode == "case-alias-truncated" {
				data, _ := json.Marshal(map[string]any{"sha": f.tree, "tree": f.entries})
				io.WriteString(w, strings.TrimSuffix(string(data), "}")+`,"truncated":true,"TRUNCATED":false}`)
				return
			}
			if r.URL.RawQuery != "recursive=1" {
				t.Error("non-recursive tree request")
			}
			entries := append([]entry(nil), f.entries...)
			switch mode {
			case "tree-hash":
				entries = entries[1:]
			case "unsafe-path":
				entries[0].Path = "../outside"
			case "git-path":
				entries[0].Path = ".GiT/config"
			case "control-path":
				entries[0].Path = "bad\npath"
			case "case-collision":
				e := entries[0]
				e.Path = strings.ToUpper(e.Path)
				entries = append(entries, e)
			case "duplicate-path":
				entries = append(entries, entries[0])
			case "symlink":
				entries[0].Mode = "120000"
			case "submodule":
				entries[0].Mode = "160000"
				entries[0].Type = "commit"
			case "size":
				n := int64(1 << 30)
				entries[0].Size = &n
			case "subtree":
				for i := range entries {
					if entries[i].Type == "tree" {
						entries[i].SHA = strings.Repeat("1", 40)
						break
					}
				}
			case "missing-parent":
				for i := range entries {
					if entries[i].Type == "tree" {
						entries = append(entries[:i], entries[i+1:]...)
						break
					}
				}
			}
			json.NewEncoder(w).Encode(map[string]any{"sha": f.tree, "truncated": mode == "truncated", "tree": entries})
		case strings.HasPrefix(r.URL.Path, "/repos/acme/repo/git/blobs/"):
			blobCalls.Add(1)
			if r.Header.Get("Accept") != "application/vnd.github.raw+json" {
				t.Error("blob must be raw")
			}
			sha := strings.TrimPrefix(r.URL.Path, "/repos/acme/repo/git/blobs/")
			body, ok := f.blobs[sha]
			if !ok {
				w.WriteHeader(404)
				return
			}
			if mode == "missing-blob" {
				w.WriteHeader(404)
				return
			}
			if mode == "tampered-blob" {
				body = bytes.Repeat([]byte{'x'}, len(body))
			}
			if mode == "oversize-blob" {
				w.WriteHeader(200)
				w.(http.Flusher).Flush()
				w.Write(append(body, 'x'))
				return
			}
			if mode == "short-blob" {
				if len(body) > 0 {
					body = body[:len(body)-1]
				}
			}
			if mode == "zero-extra" && len(body) == 0 {
				w.WriteHeader(200)
				w.(http.Flusher).Flush()
				w.Write([]byte{'x'})
				return
			}
			w.Write(body)
		default:
			t.Errorf("unexpected source path %s", r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	t.Cleanup(server.Close)
	endpoint, _ := url.Parse(server.URL)
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme != "https" || r.URL.Host != "api.github.com" {
			t.Errorf("non-allowlisted original URL %s", r.URL)
			return nil, fail("test target")
		}
		clone := r.Clone(r.Context())
		u := *r.URL
		u.Scheme, u.Host = endpoint.Scheme, endpoint.Host
		clone.URL = &u
		return server.Client().Transport.RoundTrip(clone)
	})}
	return client, &calls, &blobCalls
}

func definition() domain.WorkerSourceFetch {
	return domain.WorkerSourceFetch{Provider: "github", RepositoryID: 123, TimeoutSeconds: 5, MaxDownloadBytes: 1 << 20, MaxRequests: 100, MaxEntries: 100, MaxDepth: 10}
}

func TestMaterializeActualGitCompositionAndRawTree(t *testing.T) {
	f := realGit(t)
	client, calls, blobs := fixtureClient(t, f, "")
	dest := filepath.Join(t.TempDir(), "source")
	credentials := 0
	err := Materialize(context.Background(), dest, "https://github.com/acme/repo", f.commit, definition(), 1<<20, func() (string, error) { credentials++; return "fixture-read-only", nil }, client)
	if err != nil {
		t.Fatal(err)
	}
	if credentials != int(calls.Load()) || blobs.Load() == 0 {
		t.Fatalf("credentials/request mismatch %d/%d", credentials, calls.Load())
	}
	for _, e := range f.entries {
		if e.Type != "blob" {
			continue
		}
		got, err := os.ReadFile(filepath.Join(dest, e.Path))
		if err != nil || !bytes.Equal(got, f.blobs[e.SHA]) {
			t.Fatalf("raw source mismatch %s: %v", e.Path, err)
		}
		info, _ := os.Stat(filepath.Join(dest, e.Path))
		if info.Mode().Perm()&0222 != 0 {
			t.Fatal("source file writable")
		}
		if e.Mode == "100755" && info.Mode().Perm()&0100 == 0 {
			t.Fatal("executable bit lost")
		}
	}
	if _, err := os.Stat(filepath.Join(dest, ".git")); !os.IsNotExist(err) {
		t.Fatal("Git metadata leaked")
	}
	if got, _ := os.ReadFile(filepath.Join(dest, "raw.txt")); string(got) != "$Format:%H$\n" {
		t.Fatal("export substitution changed source")
	}
	if _, err := os.Stat(filepath.Join(dest, "ignored.txt")); err != nil {
		t.Fatal("export-ignore removed source")
	}
}

func TestMaterializeRejectsUntrustedOrIncompleteSource(t *testing.T) {
	f := realGit(t)
	for _, mode := range []string{"wrong-repo", "rename", "wrong-commit", "tree-hash", "unsafe-path", "git-path", "control-path", "case-collision", "duplicate-path", "symlink", "submodule", "size", "subtree", "missing-parent", "truncated", "duplicate-json", "case-alias-repo", "case-alias-commit", "case-alias-commit-tree", "case-alias-truncated", "redirect", "missing-blob", "tampered-blob", "oversize-blob", "short-blob", "requests", "entries", "depth", "source-bytes", "download", "repeat-budget", "credential", "credential-rotation", "cancel", "sha256", "bad-url", "existing-destination", "metadata-oversize", "malformed", "lying-length"} {
		t.Run(mode, func(t *testing.T) {
			client, _, blobs := fixtureClient(t, f, mode)
			dest := filepath.Join(t.TempDir(), "source")
			spec := definition()
			sourceBytes := int64(1 << 20)
			sha := f.commit
			repoURL := "https://github.com/acme/repo"
			switch mode {
			case "requests":
				spec.MaxRequests = 3
			case "entries":
				spec.MaxEntries = 1
			case "depth":
				spec.MaxDepth = 1
			case "source-bytes":
				sourceBytes = 1
			case "download":
				spec.MaxDownloadBytes = 10
			case "sha256":
				sha = strings.Repeat("a", 64)
			case "bad-url":
				repoURL = "https://github.com/acme/repo?leak=1"
			case "existing-destination":
				os.Mkdir(dest, 0700)
				os.WriteFile(filepath.Join(dest, "marker"), []byte("preserve"), 0600)
			case "repeat-budget":
				var total int64
				for _, e := range f.entries {
					if e.Type == "blob" {
						total += *e.Size
					}
				}
				sourceBytes = total - int64(len([]byte("same blob\n")))
			}
			ctx := context.Background()
			var cancel context.CancelFunc
			if mode == "cancel" {
				ctx, cancel = context.WithTimeout(ctx, 30*time.Millisecond)
				defer cancel()
			}
			credentialCalls := 0
			err := Materialize(ctx, dest, repoURL, sha, spec, sourceBytes, func() (string, error) {
				credentialCalls++
				if mode == "credential" || mode == "credential-rotation" && credentialCalls > 3 {
					return "", fail("secret fixture error must not escape")
				}
				return "fixture-read-only", nil
			}, client)
			if err == nil {
				t.Fatal("accepted invalid source")
			}
			if strings.Contains(err.Error(), "secret fixture") || strings.Contains(err.Error(), "https:") {
				t.Fatal("sensitive error leaked")
			}
			if mode == "existing-destination" {
				if b, _ := os.ReadFile(filepath.Join(dest, "marker")); string(b) != "preserve" {
					t.Fatal("existing destination damaged")
				}
			} else if _, err = os.Stat(dest); !os.IsNotExist(err) {
				t.Fatal("partial source accepted/left behind")
			}
			if mode == "repeat-budget" && blobs.Load() != 0 {
				t.Fatal("expanded repeated-blob budget was checked after download")
			}
		})
	}
}

func TestMetadataAliasesOnlyForConsumedFields(t *testing.T) {
	for shape, fields := range consumedMetadataFields {
		for field := range fields {
			data := `{"` + field + `":null,"` + strings.ToUpper(field) + `":null}`
			if err := uniqueJSON(json.NewDecoder(strings.NewReader(data)), 0, shape); err == nil {
				t.Fatalf("accepted consumed-field alias %d %s", shape, field)
			}
		}
	}
	if err := uniqueJSON(json.NewDecoder(strings.NewReader(`{"tree":[{"path":"a","PATH":"b"}]}`)), 0, treeMetadata); err == nil {
		t.Fatal("accepted entry field alias")
	}
	f := realGit(t)
	client, _, _ := fixtureClient(t, f, "unused-case-aliases")
	if err := Materialize(context.Background(), filepath.Join(t.TempDir(), "source"), "https://github.com/acme/repo", f.commit, definition(), 1<<20, func() (string, error) { return "fixture-read-only", nil }, client); err != nil {
		t.Fatalf("rejected unused vendor metadata: %v", err)
	}
}

func TestTreeSafetyChecksPrecedeObjectIdentity(t *testing.T) {
	size := int64(0)
	for _, path := range []string{"../outside", ".GiT/config", "a//b", "a\\b", "a\x00b", "a/../b", "/absolute"} {
		err := validateTree([]entry{{Path: path, Mode: "100644", Type: "blob", SHA: strings.Repeat("a", 40), Size: &size}}, strings.Repeat("b", 40), definition(), 1024)
		if err == nil || strings.Contains(err.Error(), "identity") {
			t.Fatalf("unsafe path reached hash-only rejection: %q %v", path, err)
		}
	}
	for _, mode := range []string{"120000", "160000", "100600"} {
		err := validateTree([]entry{{Path: "safe", Mode: mode, Type: "blob", SHA: strings.Repeat("a", 40), Size: &size}}, strings.Repeat("b", 40), definition(), 1024)
		if err == nil || !strings.Contains(err.Error(), "mode") {
			t.Fatalf("unsafe mode reached hash-only rejection: %s %v", mode, err)
		}
	}
}

func TestReaderCumulativeDecodedBodyBudget(t *testing.T) {
	r := reader{remaining: 5}
	response := func(body string) *http.Response {
		return &http.Response{Body: io.NopCloser(strings.NewReader(body)), ContentLength: -1}
	}
	if _, err := r.body(response("123"), 8<<20); err != nil || r.remaining != 2 {
		t.Fatalf("first response: %d %v", r.remaining, err)
	}
	if _, err := r.body(response("456"), 8<<20); err == nil {
		t.Fatal("accepted cumulative chunked-body overflow")
	}
}

func TestMaterializeExactInclusiveBudgetsWithFinalEmptyBlob(t *testing.T) {
	f := realGit(t)
	encodedSize := func(value any) int64 {
		var b bytes.Buffer
		if err := json.NewEncoder(&b).Encode(value); err != nil {
			t.Fatal(err)
		}
		return int64(b.Len())
	}
	metadataBytes := encodedSize(map[string]any{"id": int64(123), "full_name": "acme/repo"}) +
		encodedSize(map[string]any{"sha": f.commit, "tree": map[string]string{"sha": f.tree}, "author": map[string]string{"name": "transient fixture author"}}) +
		encodedSize(map[string]any{"sha": f.tree, "truncated": false, "tree": f.entries})
	var expanded int64
	files := 0
	for _, e := range f.entries {
		if e.Type == "blob" {
			expanded += *e.Size
			files++
		}
	}
	for _, mode := range []string{"exact", "insufficient-requests", "zero-extra"} {
		t.Run(mode, func(t *testing.T) {
			client, calls, blobCalls := fixtureClient(t, f, mode)
			spec := definition()
			spec.MaxDownloadBytes = metadataBytes + expanded
			spec.MaxRequests = 3 + files
			if mode == "insufficient-requests" {
				spec.MaxRequests--
			}
			dest := filepath.Join(t.TempDir(), "source")
			err := Materialize(context.Background(), dest, "https://github.com/acme/repo", f.commit, spec, expanded, func() (string, error) { return "fixture-read-only", nil }, client)
			if mode == "exact" {
				if err != nil {
					t.Fatalf("exact inclusive budgets rejected: %v", err)
				}
				if calls.Load() != int64(spec.MaxRequests) || blobCalls.Load() != int64(files) {
					t.Fatalf("did not fetch exact request/file counts: %d/%d, %d/%d", calls.Load(), spec.MaxRequests, blobCalls.Load(), files)
				}
				if b, err := os.ReadFile(filepath.Join(dest, "zz-empty.txt")); err != nil || len(b) != 0 {
					t.Fatalf("empty Git file missing: %v", err)
				}
			} else {
				if err == nil {
					t.Fatal("accepted source exceeding inclusive budget")
				}
				if _, err := os.Stat(dest); !os.IsNotExist(err) {
					t.Fatal("failed bounded snapshot remains")
				}
				if mode == "insufficient-requests" && blobCalls.Load() != 0 {
					t.Fatal("request shortage was not rejected before blobs")
				}
				if mode == "zero-extra" && (calls.Load() != int64(spec.MaxRequests) || blobCalls.Load() != int64(files) || err.Error() != "source fetch: blob identity") {
					t.Fatalf("unexpected byte was not rejected at final empty body: requests=%d blobs=%d error=%v", calls.Load(), blobCalls.Load(), err)
				}
			}
		})
	}
}
