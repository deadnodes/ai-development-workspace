package workerrunner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"releasecontrol/internal/application"
	"releasecontrol/internal/artifactspool"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/persistence"
	"releasecontrol/internal/transport"
	"releasecontrol/internal/workerconfig"
)

// RunOne's real watchdog reexecs the current executable. Dispatch that mode in
// the test binary too, without a production bypass or recursive test execution.
func TestMain(m *testing.M) {
	if len(os.Args) == 4 && os.Args[1] == "watchdog" {
		if Watchdog(os.Args[2], os.Args[3]) != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type sourceRoundTrip func(*http.Request) (*http.Response, error)

func (f sourceRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type sourceGitFixture struct {
	t    *testing.T
	root string
	shas []string
}

func (f sourceGitFixture) git(args ...string) []byte {
	f.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = f.root
	cmd.Env = cleanEnv()
	b, e := cmd.CombinedOutput()
	if e != nil {
		f.t.Fatalf("owned fixture Git %v: %v %s", args, e, b)
	}
	return bytes.TrimSpace(b)
}
func newSourceGitFixture(t *testing.T) sourceGitFixture {
	t.Helper()
	f := sourceGitFixture{t: t, root: t.TempDir()}
	write := func(name, data string) {
		if e := os.WriteFile(filepath.Join(f.root, name), []byte(data), 0600); e != nil {
			t.Fatal(e)
		}
	}
	write("main.go", "package main\nimport \"fmt\"\nfunc main(){fmt.Println(A+B)}\n")
	write("a.go", "package main\nconst A=\"A0\"\n")
	write("b.go", "package main\nconst B=\"B0\"\n")
	if err := os.Mkdir(filepath.Join(f.root, "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	write("bin/check", "#!/bin/sh\nexit 0\n")
	if err := os.Chmod(filepath.Join(f.root, "bin/check"), 0700); err != nil {
		t.Fatal(err)
	}
	write("main_test.go", `package main
import("net";"os";"testing";"time")
func TestBoundaries(t *testing.T){if A!="A1"&&A!="A2"||B!="B1"{t.Fatal("wrong composition",A,B)};for _,key:=range []string{"RCP_FETCH_WORKER_TOKEN","RCP_FETCH_SOURCE_TOKEN","RC_TOKEN"}{if os.Getenv(key)!=""{t.Fatal("credential leak")}};for _,path:=range []string{"/source/.git","/var/run/docker.sock"}{if _,err:=os.Stat(path);!os.IsNotExist(err){t.Fatal("unexpected supervisor data")}};if os.WriteFile("/source/main.go",[]byte("tamper"),0600)==nil{t.Fatal("source writable")};c,e:=net.DialTimeout("tcp","192.0.2.1:80",200*time.Millisecond);if e==nil{c.Close();t.Fatal("network available")}}
`)
	f.git("init", "-b", "base")
	f.git("add", ".")
	commit := func(message string) {
		f.git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null", "commit", "-am", message)
	}
	commit("base")
	base := string(f.git("rev-parse", "HEAD"))
	f.git("checkout", "-b", "feature-a")
	write("a.go", "package main\nconst A=\"A1\"\n")
	commit("A1")
	a1 := string(f.git("rev-parse", "HEAD"))
	write("a.go", "package main\nconst A=\"A2\"\n")
	commit("A2")
	a2 := string(f.git("rev-parse", "HEAD"))
	f.git("checkout", "-b", "feature-b", base)
	write("b.go", "package main\nconst B=\"B1\"\n")
	commit("B1")
	b := string(f.git("rev-parse", "HEAD"))
	for i, a := range []string{a1, a2} {
		f.git("checkout", "-b", fmt.Sprintf("generated-%d", i), a)
		f.git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "-c", "commit.gpgsign=false", "merge", "--no-edit", b)
		f.shas = append(f.shas, string(f.git("rev-parse", "HEAD")))
	}
	f.git("branch", "-D", "feature-a", "feature-b")
	return f
}

// Fixture metadata is derived from actual objects using ls-tree/cat-file,
// independently of the reader's object hash and tree reconstruction code.
func (f sourceGitFixture) response(r *http.Request) *http.Response {
	f.t.Helper()
	if r.Method != "GET" || r.URL.Scheme != "https" || r.URL.Host != "api.github.com" || r.Header.Get("Authorization") != "Bearer fixture-source-only" {
		f.t.Errorf("source request authority escaped: %s %s", r.Method, r.URL.Host)
	}
	prefix := "/repos/fixture/source"
	p := strings.TrimPrefix(r.URL.Path, prefix)
	var body []byte
	jsonBody := func(v any) { body, _ = json.Marshal(v) }
	switch {
	case p == "":
		jsonBody(map[string]any{"id": 42, "full_name": "fixture/source"})
	case strings.HasPrefix(p, "/git/commits/"):
		sha := strings.TrimPrefix(p, "/git/commits/")
		jsonBody(map[string]any{"sha": sha, "tree": map[string]any{"sha": string(f.git("rev-parse", sha+"^{tree}"))}})
	case strings.HasPrefix(p, "/git/trees/"):
		sha := strings.TrimPrefix(p, "/git/trees/")
		if r.URL.RawQuery != "recursive=1" {
			f.t.Error("missing complete tree request")
		}
		rows := []map[string]any{}
		for _, raw := range bytes.Split(f.git("ls-tree", "-r", "-t", "-l", "-z", sha), []byte{0}) {
			if len(raw) == 0 {
				continue
			}
			pair := bytes.SplitN(raw, []byte{'\t'}, 2)
			fields := strings.Fields(string(pair[0]))
			row := map[string]any{"mode": fields[0], "type": fields[1], "sha": fields[2], "path": string(pair[1])}
			if fields[3] != "-" {
				n, _ := strconv.ParseInt(fields[3], 10, 64)
				row["size"] = n
			}
			rows = append(rows, row)
		}
		jsonBody(map[string]any{"sha": sha, "truncated": false, "tree": rows})
	case strings.HasPrefix(p, "/git/blobs/"):
		// cat-file output must not be trimmed: source bytes include final newlines.
		cmd := exec.Command("git", "cat-file", "blob", strings.TrimPrefix(p, "/git/blobs/"))
		cmd.Dir = f.root
		cmd.Env = cleanEnv()
		var e error
		body, e = cmd.Output()
		if e != nil {
			f.t.Fatal(e)
		}
	default:
		f.t.Fatalf("unapproved source endpoint %s", p)
	}
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body)), Request: r}
}

type sourceFixtureStore interface {
	application.Store
	Close()
}

func openSourceFixtureStore(t *testing.T, pg bool) func() sourceFixtureStore {
	t.Helper()
	ctx := context.Background()
	if !pg {
		path := filepath.Join(t.TempDir(), "state.db")
		return func() sourceFixtureStore {
			s, e := persistence.OpenLocal(ctx, path)
			if e != nil {
				t.Fatal(e)
			}
			return s
		}
	}
	raw := os.Getenv("TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("TEST_DATABASE_URL required for isolated PostgreSQL fixture")
	}
	conn, e := pgx.Connect(ctx, raw)
	if e != nil {
		t.Fatal(e)
	}
	schema := "source_ingestion_" + strings.ReplaceAll(nonce(), "-", "")
	if _, e = conn.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		_, e := conn.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		if e != nil {
			t.Error(e)
		}
		conn.Close(ctx)
	})
	u, e := url.Parse(raw)
	if e != nil {
		t.Fatal(e)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return func() sourceFixtureStore {
		s, e := persistence.Open(ctx, u.String())
		if e != nil {
			t.Fatal(e)
		}
		return s
	}
}

func TestSourceFetchExplicitMirrorAndSnapshotPermissions(t *testing.T) {
	f := newSourceGitFixture(t)
	for _, remote := range []bool{false, true} {
		t.Run(fmt.Sprintf("remote=%t", remote), func(t *testing.T) {
			cfg := sourceFetchConfig(t)
			requests := 0
			client := &http.Client{Transport: sourceRoundTrip(func(r *http.Request) (*http.Response, error) { requests++; return f.response(r), nil })}
			if !remote {
				cfg.Recipe.Execution.SourceFetch = nil
				cfg.SourceGitHub = nil
				cfg.SourceMirror = filepath.Join(f.root, ".git")
				cfg.Recipe.Hash = workerconfig.RecipeHash(cfg.Recipe)
			}
			root := filepath.Join(t.TempDir(), "private-attempt")
			if err := os.Mkdir(root, 0700); err != nil {
				t.Fatal(err)
			}
			source, err := materializeSource(context.Background(), cfg, root, f.shas[0], client, func() (string, error) { return "fixture-source-only", nil })
			if err != nil {
				t.Fatal(err)
			}
			if remote && requests == 0 || !remote && requests != 0 {
				t.Fatal("source strategy fallback")
			}
			info, err := os.Stat(root)
			if err != nil || info.Mode().Perm() != 0700 {
				t.Fatal("attempt confidentiality changed")
			}
			for _, name := range []string{"main.go", "a.go", "b.go", "main_test.go"} {
				info, err = os.Stat(filepath.Join(source, name))
				if err != nil || info.Mode().Perm()&0444 != 0444 || info.Mode().Perm()&0222 != 0 {
					t.Fatal("compiler snapshot mode", name, err)
				}
			}
			if _, err = os.Stat(filepath.Join(source, ".git")); !os.IsNotExist(err) {
				t.Fatal("Git metadata reached compiler")
			}
			info, err = os.Stat(filepath.Join(source, "bin/check"))
			if err != nil || info.Mode().Perm() != 0555 {
				t.Fatal("executable mode lost", err)
			}
			info, err = os.Stat(filepath.Join(source, "bin"))
			if err != nil || info.Mode().Perm()&0555 != 0555 {
				t.Fatal("directory not compiler traversable", err)
			}
		})
	}
}

func TestSourceFetchRealGitRunOneOCI(t *testing.T) {
	if os.Getenv("RCP_TEST_DOCKER") != "1" {
		t.Skip("owned real Docker execution requires explicit opt-in")
	}
	f := newSourceGitFixture(t)
	for _, pg := range []bool{false, true} {
		t.Run(fmt.Sprintf("postgres=%t", pg), func(t *testing.T) {
			open := openSourceFixtureStore(t, pg)
			store := open()
			defer func() { store.Close() }()
			ctx := context.Background()
			cfg := sourceFetchConfig(t)
			e := cfg.Recipe.Execution
			e.Kind = "local_oci_build"
			cfg.Recipe.Capability = e.Kind
			e.ToolchainImage = "golang@sha256:b305420a68d0f229d91eb3b3ed9e519fcf2cf5461da4bef997bf927e8c0bfd2b"
			e.BuildKitImage = "moby/buildkit@sha256:04cf92d6e86fcfdc7f37b7b5098978081952c11e6ac16769ab1aad95035c2441"
			e.CheckArgv = []string{"go", "test", "/source/main.go", "/source/a.go", "/source/b.go", "/source/main_test.go"}
			e.BuildArgv = []string{"go", "build", "-trimpath", "-buildvcs=false", "-ldflags=-buildid=", "-o", "/output/app", "/source/main.go", "/source/a.go", "/source/b.go"}
			e.OutputFile = "app"
			e.ImagePath = "app"
			e.Entrypoint = []string{"/app"}
			e.TimeoutSeconds = 300
			e.MemoryMB = 1536
			e.StorageMB = 512
			e.CPUs = 2
			e.PIDs = 128
			e.OutputBytes = 16 << 20
			e.LogBytes = 1 << 20
			cfg.Recipe.Hash = workerconfig.RecipeHash(cfg.Recipe)
			cfg.DockerContext = "orbstack"
			home, _ := os.UserHomeDir()
			cfg.DockerConfig = filepath.Join(home, ".docker")
			cfg.AllowRootlessUnconfined = true
			spool := filepath.Join(t.TempDir(), "spool")
			if err := os.Mkdir(spool, 0700); err != nil {
				t.Fatal(err)
			}
			spool, _ = filepath.EvalSymlinks(spool)
			cfg.ArtifactSpool = &artifactspool.Config{Root: spool, MaxArchiveBytes: 16 << 20, MaxTotalBytes: 64 << 20, MaxEntries: 4}
			token := strings.Repeat("w", 32)
			t.Setenv(cfg.TokenEnv, token)
			t.Setenv(cfg.SourceGitHub.TokenEnv, "fixture-source-only")
			t.Setenv("RC_TOKEN", "fixture-control-only")
			auth, err := workerconfig.New(workerconfig.Config{Revision: "cfg1", LeaseSeconds: 20, Recipes: []workerconfig.Recipe{cfg.Recipe}, Credentials: []workerconfig.Credential{{ID: "fixture", Principal: "worker/fixture", TokenEnv: cfg.TokenEnv, ProductIDs: []string{"p"}, RepositoryIDs: []string{"r"}, Capabilities: []string{"local_oci_build"}, ExpiresAt: time.Now().Add(time.Hour)}}}, "fixture-control-only", func(string) string { return token })
			if err != nil {
				t.Fatal(err)
			}
			service := application.New(store)
			service.ConfigureWorkers(auth)
			for _, c := range []domain.Command{{Action: "create_product", ID: "p", Data: map[string]any{"name": "p"}}, {Action: "create_feature", ID: "f", ProductID: "p", Data: map[string]any{"title": "fixture", "goal": "build"}}, {Action: "create_integration", ID: "i", FeatureID: "f", Data: map[string]any{"title": "fixture", "objective": "build"}}, {Action: "create_repository", ID: "r", ProductID: "p", Data: map[string]any{"name": "r", "url": cfg.Recipe.RepositoryURL}}} {
				c.Actor = "fixture/operator"
				if _, err = service.Execute(ctx, c); err != nil {
					t.Fatal(err)
				}
			}
			var manifests []string
			for n, sha := range f.shas {
				rev := fmt.Sprintf("rev%d", n)
				if _, err = service.Execute(ctx, domain.Command{Action: "record_integration_revision", Actor: "fixture/operator", ID: rev, IntegrationID: "i", Data: map[string]any{"repository_id": "r", "branch": "generated", "base_commit": sha, "head_commit": sha, "commits": []string{sha}}}); err != nil {
					t.Fatal(err)
				}
				if _, err = service.Execute(ctx, domain.Command{Action: "queue_worker_job", Actor: "fixture/operator", ProductID: "p", IntegrationID: "i", Data: map[string]any{"request_key": "source-ingestion-" + rev, "recipe_id": cfg.Recipe.ID, "recipe_revision": cfg.Recipe.Revision, "recipe_hash": cfg.Recipe.Hash, "config_revision": "cfg1", "revision_id": rev, "source_sha": sha, "parameters": map[string]any{}, "deadline": time.Now().Add(10 * time.Minute), "max_attempts": 1}}); err != nil {
					t.Fatal(err)
				}
				server := httptest.NewServer(transport.New(service, transport.Options{Token: "fixture-control-only", Workers: auth}))
				cfg.Endpoint = server.URL
				report, runErr := runOne(ctx, cfg, &http.Client{Transport: sourceRoundTrip(func(r *http.Request) (*http.Response, error) { return f.response(r), nil })})
				server.Close()
				if runErr != nil || report == nil || report.Result != "PASSED" || report.LocalOCI == nil {
					t.Fatalf("source RunOne failed report=%+v err=%v", report, runErr)
				}
				if report.LocalOCI.SourceSHA != sha {
					t.Fatal("output source identity lost")
				}
				if _, err = artifactspool.Resolve(*cfg.ArtifactSpool, *report.LocalOCI); err != nil {
					t.Fatal(err)
				}
				manifests = append(manifests, report.LocalOCI.ManifestDigest)
				assertOwnedResourcesGone(t, cfg)
				entries, _ := filepath.Glob(filepath.Join(cfg.StateDir, "*", "source", "a.go"))
				if len(entries) != n+1 {
					t.Fatal("snapshot missing")
				}
				want := fmt.Sprintf("A%d", n+1)
				found := false
				for _, path := range entries {
					b, _ := os.ReadFile(path)
					if strings.Contains(string(b), want) {
						found = true
					}
				}
				if !found {
					t.Fatal("actual generated snapshot lost selected feature")
				}
				before, err := store.Read(ctx)
				if err != nil {
					t.Fatal(err)
				}
				saved, _ := json.Marshal(before.WorkerAttempts)
				store.Close()
				store = open()
				after, err := store.Read(ctx)
				if err != nil {
					t.Fatal(err)
				}
				reopened, _ := json.Marshal(after.WorkerAttempts)
				if !bytes.Equal(saved, reopened) {
					t.Fatal("reopen changed accepted receipt")
				}
				service = application.New(store)
				service.ConfigureWorkers(auth)
				for _, job := range after.WorkerJobs {
					if workerconfig.Fingerprint(job.Inputs.Execution.SourceFetch) != workerconfig.Fingerprint(e.SourceFetch) || job.InputHash != workerconfig.Fingerprint(job.Inputs) {
						t.Fatal("source definition absent from immutable inputs")
					}
				}
			}
			if manifests[0] == manifests[1] {
				t.Fatal("changed generated source did not change real OCI output")
			}
			// Fail the production RunOne source phase after a real HTTP claim/start.
			// No negative case may reach Docker resource creation or a success report.
			for _, mode := range []string{"wrong_repository", "missing_blob", "truncated", "tampered_blob", "redirect", "credential_rotation", "cancelled"} {
				t.Run(mode, func(t *testing.T) {
					t.Setenv(cfg.SourceGitHub.TokenEnv, "fixture-source-only")
					_, err := service.Execute(ctx, domain.Command{Action: "queue_worker_job", Actor: "fixture/operator", ProductID: "p", IntegrationID: "i", Data: map[string]any{"request_key": "source-failure-" + mode, "recipe_id": cfg.Recipe.ID, "recipe_revision": cfg.Recipe.Revision, "recipe_hash": cfg.Recipe.Hash, "config_revision": "cfg1", "revision_id": "rev1", "source_sha": f.shas[1], "parameters": map[string]any{}, "deadline": time.Now().Add(10 * time.Minute), "max_attempts": 1}})
					if err != nil {
						t.Fatal(err)
					}
					server := httptest.NewServer(transport.New(service, transport.Options{Token: "fixture-control-only", Workers: auth}))
					defer server.Close()
					negative := cfg
					negative.StateDir = filepath.Join(t.TempDir(), "failed-attempts")
					negative.Endpoint = server.URL
					runCtx, cancel := context.WithCancel(ctx)
					defer cancel()
					client := &http.Client{Transport: sourceRoundTrip(func(r *http.Request) (*http.Response, error) {
						if mode == "cancelled" {
							cancel()
							return nil, context.Canceled
						}
						res := f.response(r)
						if mode == "wrong_repository" && r.URL.Path == "/repos/fixture/source" {
							res.Body = io.NopCloser(strings.NewReader(`{"id":43,"full_name":"fixture/source"}`))
							res.ContentLength = -1
						}
						if mode == "redirect" {
							res.StatusCode = 302
							res.Header.Set("Location", "https://example.invalid/secret")
						}
						if mode == "truncated" && strings.Contains(r.URL.Path, "/git/trees/") {
							b, _ := io.ReadAll(res.Body)
							res.Body.Close()
							res.Body = io.NopCloser(bytes.NewReader(bytes.Replace(b, []byte(`"truncated":false`), []byte(`"truncated":true`), 1)))
							res.ContentLength = -1
						}
						if strings.Contains(r.URL.Path, "/git/blobs/") {
							if mode == "missing_blob" {
								res.StatusCode = 404
							}
							if mode == "tampered_blob" {
								res.Body.Close()
								res.Body = io.NopCloser(strings.NewReader("tampered"))
								res.ContentLength = 8
							}
						}
						if mode == "credential_rotation" {
							if err := os.Setenv(cfg.SourceGitHub.TokenEnv, token); err != nil {
								t.Fatal(err)
							}
						}
						return res, nil
					})}
					report, runErr := runOne(runCtx, negative, client)
					if runErr == nil || report == nil || report.Result != "EXECUTOR_FAILED" || report.LocalOCI != nil {
						t.Fatalf("unsafe source outcome admitted: %+v %v", report, runErr)
					}
					journals, _ := filepath.Glob(filepath.Join(negative.StateDir, "*", "journal.json"))
					if len(journals) != 1 {
						t.Fatal("expected one source attempt")
					}
					j, err := readJournal(journals[0])
					if err != nil || len(j.Resources) != 0 || j.Report == nil || j.Report.LocalOCI != nil {
						t.Fatal("source failure reached compiler/artifact", err)
					}
					state, err := store.Read(ctx)
					if err != nil {
						t.Fatal(err)
					}
					for _, a := range state.WorkerAttempts {
						if a.ID == report.AttemptID && a.Report != nil && (a.Report.Result == "PASSED" || a.Report.LocalOCI != nil) {
							t.Fatal("server accepted failed source as success")
						}
					}
				})
			}
			state, err := store.Read(ctx)
			if err != nil || len(state.WorkerAttempts) != 9 || len(state.DeliveryArtifacts) != 0 {
				t.Fatal("incorrect standalone local build state", err)
			}
			successes := 0
			for _, a := range state.WorkerAttempts {
				if a.Status == "SUCCEEDED" {
					successes++
					if a.Report == nil || a.Report.LocalOCI == nil {
						t.Fatal("accepted receipt missing")
					}
				}
			}
			if successes != 2 {
				t.Fatal("failure altered successful receipts")
			}
			t.Logf("standalone real Git generated A1+B1→A2+B1; deleted developer branches; no mirror; HTTP/reopen; manifests %v", manifests)
		})
	}
}
