package ocipublisher

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"releasecontrol/internal/workerrunner"
)

func fixture(t *testing.T, origin string) Config {
	t.Helper()
	files := map[string][]byte{}
	add := func(media string, b []byte) map[string]any {
		d := hash(b)
		files["blobs/sha256/"+strings.TrimPrefix(d, "sha256:")] = b
		return map[string]any{"mediaType": media, "digest": d, "size": len(b)}
	}
	var layer bytes.Buffer
	tw := tar.NewWriter(&layer)
	_ = tw.WriteHeader(&tar.Header{Name: "app", Size: 3, Mode: 0555})
	_, _ = tw.Write([]byte("app"))
	_ = tw.Close()
	config, _ := json.Marshal(map[string]any{"os": "linux", "architecture": "arm64", "rootfs": map[string]any{"type": "layers", "diff_ids": []string{hash(layer.Bytes())}}})
	m, _ := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": manifestType, "config": add("application/vnd.oci.image.config.v1+json", config), "layers": []any{add("application/vnd.oci.image.layer.v1.tar", layer.Bytes())}})
	idx, _ := json.Marshal(map[string]any{"schemaVersion": 2, "manifests": []any{add(manifestType, m)}})
	files["index.json"] = idx
	files["oci-layout"] = []byte(`{"imageLayoutVersion":"1.0.0"}`)
	var b bytes.Buffer
	tw = tar.NewWriter(&b)
	for name, data := range files {
		_ = tw.WriteHeader(&tar.Header{Name: name, Size: int64(len(data)), Mode: 0600})
		_, _ = tw.Write(data)
	}
	_ = tw.Close()
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	_ = os.Chmod(root, 0700)
	archive := filepath.Join(root, "image.tar")
	_ = os.WriteFile(archive, b.Bytes(), 0600)
	receipt, e := workerrunner.VerifyOCIBytes(b.Bytes(), 1<<20, "linux/arm64")
	if e != nil {
		t.Fatal(e)
	}
	receipt.JobID = "job"
	receipt.AttemptID = "attempt"
	receipt.InputHash = hash([]byte("inputs"))
	receipt.RecipeHash = hash([]byte("recipe"))
	receipt.SourceSHA = strings.Repeat("a", 40)
	receipt.ConfigRevision = "cfg1"
	receipt.ToolchainImage = "golang@" + hash([]byte("compiler"))
	return Config{Plan: Plan{ID: "publication", Approved: true, ProductID: "p", ComponentID: "c", RepositoryID: "r", Origin: origin, Repository: "fixture/image", Receipt: receipt}, AllowedOrigin: origin, AllowedRepository: "fixture/image", Archive: archive, StateDir: root, MaxArchiveBytes: 1 << 20, TimeoutSeconds: 10, FixtureLoopbackHTTP: true}
}

type mockRegistry struct {
	mu       sync.Mutex
	data     map[string][]byte
	puts     atomic.Int32
	lost     bool
	location string
	auth     string
	gets     atomic.Int32
}

func (m *mockRegistry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.auth != "" && r.Header.Get("Authorization") != m.auth {
		w.Header().Set("WWW-Authenticate", `Bearer realm="https://hostile.invalid/token"`)
		w.WriteHeader(401)
		return
	}
	key := r.URL.Path
	switch r.Method {
	case "GET":
		m.gets.Add(1)
		b, ok := m.data[key]
		if !ok {
			w.WriteHeader(404)
			return
		}
		_, _ = w.Write(b)
	case "POST":
		loc := m.location
		if loc == "" {
			loc = "/v2/fixture/image/blobs/uploads/session?_state=opaque"
		}
		w.Header().Set("Location", loc)
		w.WriteHeader(202)
	case "PUT":
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(key, "/uploads/") {
			key = "/v2/fixture/image/blobs/" + r.URL.Query().Get("digest")
		} else {
			m.puts.Add(1)
		}
		m.data[key] = b
		if m.lost && strings.Contains(key, "/manifests/") {
			m.lost = false
			conn, _, _ := w.(http.Hijacker).Hijack()
			_ = conn.Close()
			return
		}
		w.WriteHeader(201)
	default:
		w.WriteHeader(405)
	}
}
func TestPublishReplayLostResponseAndReobserve(t *testing.T) {
	m := &mockRegistry{data: map[string][]byte{}, lost: true}
	s := httptest.NewServer(m)
	defer s.Close()
	c := fixture(t, s.URL)
	if _, e := Publish(context.Background(), c); e == nil {
		t.Fatal("lost response accepted")
	}
	if m.puts.Load() != 1 {
		t.Fatal(m.puts.Load())
	}
	if _, e := Publish(context.Background(), c); e != nil {
		t.Fatal(e)
	}
	before := m.gets.Load()
	if _, e := Publish(context.Background(), c); e != nil {
		t.Fatal(e)
	}
	if m.puts.Load() != 1 || m.gets.Load() <= before {
		t.Fatal("replay skipped observation or recommitted")
	}
	m.mu.Lock()
	for k := range m.data {
		if strings.Contains(k, "/blobs/") {
			m.data[k] = []byte("corrupt")
			break
		}
	}
	m.mu.Unlock()
	if _, e := Publish(context.Background(), c); e == nil {
		t.Fatal("corrupt registry accepted")
	}
}
func TestSnapshotAndCorruptInputZeroWrites(t *testing.T) {
	m := &mockRegistry{data: map[string][]byte{}}
	var once sync.Once
	var path string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { _ = os.WriteFile(path, []byte("changed after validation"), 0600) })
		m.ServeHTTP(w, r)
	}))
	defer s.Close()
	c := fixture(t, s.URL)
	path = c.Archive
	if _, e := Publish(context.Background(), c); e != nil {
		t.Fatal("mutable source reopen", e)
	}
	before := m.gets.Load()
	if _, e := Publish(context.Background(), c); e == nil {
		t.Fatal("corrupt input accepted")
	}
	if m.gets.Load() != before {
		t.Fatal("network on invalid archive")
	}
}
func TestHostileLocationAndRedirect(t *testing.T) {
	locations := []string{"https://hostile.invalid/v2/fixture/image/blobs/uploads/a", "/v2/other/blobs/uploads/a", "/v2/fixture/image/blobs/uploads/../a", "/v2/fixture/image/blobs/uploads/a?token=secret", "/v2/fixture/image/blobs/uploads/a%2Fb"}
	for _, loc := range locations {
		t.Run(loc, func(t *testing.T) {
			m := &mockRegistry{data: map[string][]byte{}, location: loc}
			s := httptest.NewServer(m)
			defer s.Close()
			c := fixture(t, s.URL)
			if _, e := Publish(context.Background(), c); e == nil {
				t.Fatal("hostile location accepted")
			}
			m.mu.Lock()
			empty := len(m.data) == 0
			m.mu.Unlock()
			if !empty {
				t.Fatal("unexpected write")
			}
		})
	}
	hits := 0
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	defer foreign.Close()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, foreign.URL, 307) }))
	defer s.Close()
	c := fixture(t, s.URL)
	if _, e := Publish(context.Background(), c); e == nil || hits != 0 {
		t.Fatal("redirect followed")
	}
}
func TestTLSAuthProxyAndResponseBound(t *testing.T) {
	m := &mockRegistry{data: map[string][]byte{}, auth: "Bearer fixture-secret"}
	s := httptest.NewTLSServer(m)
	defer s.Close()
	c := fixture(t, s.URL)
	c.BearerEnv = "RCP_PUBLISH_FIXTURE_TOKEN"
	t.Setenv(c.BearerEnv, "fixture-secret")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("NO_PROXY", "")
	if _, e := Publish(context.Background(), c); e == nil {
		t.Fatal("untrusted TLS accepted")
	}
	c.CAFile = filepath.Join(c.StateDir, "ca.pem")
	_ = os.WriteFile(c.CAFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw}), 0600)
	if _, e := Publish(context.Background(), c); e != nil {
		t.Fatal(e)
	}
	t.Setenv(c.BearerEnv, "wrong")
	if _, e := Publish(context.Background(), c); e == nil || strings.Contains(e.Error(), "wrong") {
		t.Fatal("auth or credential sanitization")
	}
	huge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(bytes.Repeat([]byte("x"), 2<<20)) }))
	defer huge.Close()
	cfg := fixture(t, huge.URL)
	if _, e := Publish(context.Background(), cfg); e == nil {
		t.Fatal("response bound ignored")
	}
}
func TestIntentConcurrencyConflictAndSymlinks(t *testing.T) {
	c := fixture(t, "http://127.0.0.1:1234")
	release, e := intent(c.StateDir, "publication", hash([]byte("a")))
	if e != nil {
		t.Fatal(e)
	}
	if _, e = intent(c.StateDir, "publication", hash([]byte("a"))); e == nil {
		t.Fatal("concurrent accepted")
	}
	release()
	if _, e = intent(c.StateDir, "publication", hash([]byte("b"))); e == nil {
		t.Fatal("conflict accepted")
	}
	link := filepath.Join(c.StateDir, "link")
	_ = os.Symlink(c.StateDir, link)
	if _, e = intent(link, "other", hash(nil)); e == nil {
		t.Fatal("symlink dir")
	}
	_ = os.Symlink(c.Archive, filepath.Join(c.StateDir, "other.lock"))
	if _, e = intent(c.StateDir, "other", hash(nil)); e == nil {
		t.Fatal("symlink lock")
	}
}
func TestConfigurationFailsClosed(t *testing.T) {
	for _, origin := range []string{"http://localhost:5000", "http://192.0.2.1:5000", "https://user:secret@example.com", "https://example.com/a", "https://example.com?token=secret"} {
		c := fixture(t, origin)
		if _, e := c.validate(); e == nil {
			t.Fatal(origin)
		}
	}
	c := fixture(t, "http://127.0.0.1:5000")
	c.Plan.Approved = false
	if _, e := c.validate(); e == nil {
		t.Fatal("inactive plan")
	}
	c.Plan.Approved = true
	c.AllowedRepository = "foreign"
	if _, e := c.validate(); e == nil {
		t.Fatal("destination allowlist")
	}
}

// Opt-in real pinned Distribution fixture, paired with the real worker export.
// The caller owns the registry process/container and its cleanup.
func TestRealDistribution(t *testing.T) {
	origin, export := os.Getenv("RCP_TEST_REGISTRY_ORIGIN"), os.Getenv("RCP_TEST_OCI_EXPORT")
	if origin == "" || export == "" {
		t.Skip("explicit real registry and real worker export required")
	}
	c := fixture(t, origin)
	c.Archive = filepath.Join(export, "image.oci.tar")
	b, e := os.ReadFile(filepath.Join(export, "receipt.json"))
	if e != nil || json.Unmarshal(b, &c.Plan.Receipt) != nil {
		t.Fatal("real receipt", e)
	}
	c.MaxArchiveBytes = 32 << 20
	// Proxy records the immutable manifest commit then loses its response exactly
	// once. Each invocation below runs a fresh CLI process.
	target, e := url.Parse(origin)
	if e != nil {
		t.Fatal(e)
	}
	var commits atomic.Int32
	var lost atomic.Bool
	lost.Store(true)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := r.Clone(r.Context())
		req.URL.Scheme = target.Scheme
		req.URL.Host = target.Host
		req.RequestURI = ""
		resp, e := http.DefaultTransport.RoundTrip(req)
		if e != nil {
			http.Error(w, "fixture upstream", 502)
			return
		}
		defer resp.Body.Close()
		if r.Method == "PUT" && strings.Contains(r.URL.Path, "/manifests/") {
			commits.Add(1)
			if lost.Swap(false) {
				conn, _, _ := w.(http.Hijacker).Hijack()
				_ = conn.Close()
				return
			}
		}
		for k, v := range resp.Header {
			if strings.EqualFold(k, "Location") {
				for _, loc := range v {
					u, _ := url.Parse(loc)
					u.Scheme = ""
					u.Host = ""
					w.Header().Add(k, u.String())
				}
				continue
			}
			w.Header()[k] = v
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	defer proxy.Close()
	c.Plan.Origin = proxy.URL
	c.AllowedOrigin = proxy.URL

	binary := filepath.Join(c.StateDir, "publisher")
	build := exec.Command("go", "build", "-o", binary, "../../cmd/publisher")
	if out, e := build.CombinedOutput(); e != nil {
		t.Fatalf("build CLI: %v %s", e, out)
	}
	cfgPath := filepath.Join(c.StateDir, "publisher.json")
	cfgBytes, _ := json.Marshal(c)
	if e := os.WriteFile(cfgPath, cfgBytes, 0600); e != nil {
		t.Fatal(e)
	}
	run := func() ([]byte, error) { return exec.Command(binary, "publish", cfgPath).CombinedOutput() }
	if out, e := run(); e == nil {
		t.Fatalf("lost final response accepted: %s", out)
	}
	for i := 0; i < 2; i++ {
		out, e := run()
		if e != nil {
			t.Fatalf("restart/repeat CLI: %v %s", e, out)
		}
		var observation Observation
		if json.Unmarshal(out, &observation) != nil || observation.Kind != "PUBLISHER_LOCAL_REGISTRY_BYTES" || observation.ManifestDigest != c.Plan.Receipt.ManifestDigest {
			t.Fatal("invalid CLI observation")
		}
	}

	if commits.Load() != 1 {
		t.Fatal("duplicate manifest commit", commits.Load())
	}
	t.Logf("real Distribution: source=%s archive=%s manifest=%s; complete GET/hash pull; lost commit response and replay: commits=%d", c.Plan.Receipt.SourceSHA, c.Plan.Receipt.ArchiveHash, c.Plan.Receipt.ManifestDigest, commits.Load())
}

func TestRealDistributionTLSAuth(t *testing.T) {
	origin, export, ca := os.Getenv("RCP_TEST_TLS_REGISTRY_ORIGIN"), os.Getenv("RCP_TEST_OCI_EXPORT"), os.Getenv("RCP_TEST_TLS_REGISTRY_CA")
	if origin == "" || export == "" || ca == "" {
		t.Skip("explicit owned TLS/auth Distribution fixture required")
	}
	c := fixture(t, origin)
	c.Archive = filepath.Join(export, "image.oci.tar")
	b, e := os.ReadFile(filepath.Join(export, "receipt.json"))
	if e != nil || json.Unmarshal(b, &c.Plan.Receipt) != nil {
		t.Fatal("real receipt", e)
	}
	c.MaxArchiveBytes = 32 << 20
	c.CAFile = ca
	c.UsernameEnv = "RCP_PUBLISH_TEST_USERNAME"
	c.PasswordEnv = "RCP_PUBLISH_TEST_PASSWORD"
	t.Setenv(c.UsernameEnv, "publisher-fixture")
	t.Setenv(c.PasswordEnv, "fixture-only-password")
	if _, e := Publish(context.Background(), c); e != nil {
		t.Fatal(e)
	}
	if _, e := Publish(context.Background(), c); e != nil {
		t.Fatal(e)
	}
	t.Setenv(c.PasswordEnv, "invalid-fixture-password")
	if _, e := Publish(context.Background(), c); e == nil {
		t.Fatal("actual registry accepted wrong password")
	}
	t.Log("real Distribution HTTPS custom CA + Basic authentication + repeated complete GET/hash passed; wrong credential UNKNOWN")
}

func TestInvalidReceiptAndLimitsNeverContactRegistry(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); w.WriteHeader(500) }))
	defer server.Close()
	cases := []func(*Config){
		func(c *Config) { c.Plan.Receipt.ArchiveHash = hash(nil) },
		func(c *Config) { c.Plan.Receipt.LayoutHash = hash(nil) },
		func(c *Config) { c.Plan.Receipt.IndexDigest = hash(nil) },
		func(c *Config) { c.Plan.Receipt.ManifestDigest = hash(nil) },
		func(c *Config) { c.Plan.Receipt.ArchiveBytes++ },
		func(c *Config) { c.Plan.Receipt.Platform = "linux/amd64" },
		func(c *Config) { c.Plan.Receipt.ToolchainImage = "prefix@sha256:garbage" },
		func(c *Config) { c.Plan.Receipt.SourceSHA = "branch" },
		func(c *Config) { c.Plan.Receipt.InputHash = "invalid" },
		func(c *Config) { c.MaxArchiveBytes = -1 },
		func(c *Config) { c.MaxArchiveBytes = 257 << 20 },
		func(c *Config) { c.TimeoutSeconds = 0 },
		func(c *Config) { c.FixtureLoopbackHTTP = false },
	}
	for i, mutate := range cases {
		c := fixture(t, server.URL)
		mutate(&c)
		if _, e := Publish(context.Background(), c); e == nil {
			t.Fatalf("case %d accepted", i)
		}
	}
	if hits.Load() != 0 {
		t.Fatal("invalid input caused registry traffic")
	}
}

func TestDeadlineAndUnknownStatus(t *testing.T) {
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer blocked.Close()
	c := fixture(t, blocked.URL)
	c.TimeoutSeconds = 1
	if _, e := Publish(context.Background(), c); e != unknown {
		t.Fatal("deadline not UNKNOWN", e)
	}
	for _, code := range []int{401, 403, 429, 500, 503} {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(code) }))
		c := fixture(t, s.URL)
		if _, e := Publish(context.Background(), c); e != unknown {
			t.Fatal("status not UNKNOWN", code, e)
		}
		s.Close()
	}
}

// Seed exactly the visible state after a previous process renamed its synced
// intent file and died before syncing the directory. Actual power loss is not
// simulated: this deterministically exercises restart adoption of that state.
func seedRenamedIntent(t *testing.T, c Config) {
	t.Helper()
	planBytes, err := json.Marshal(c.Plan)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.CreateTemp(c.StateDir, ".prior-intent-")
	if err != nil {
		t.Fatal(err)
	}
	name := f.Name()
	if _, err = f.WriteString(hash(planBytes) + "\n"); err != nil {
		t.Fatal(err)
	}
	if err = f.Sync(); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(name, filepath.Join(c.StateDir, c.Plan.ID+".intent")); err != nil {
		t.Fatal(err)
	}
}

func TestIntentDurabilityBeforeRegistryIO(t *testing.T) {
	for _, restart := range []bool{false, true} {
		for _, failAt := range []int{0, 1, 2} {
			name := "new"
			if restart {
				name = "renamed-restart"
			}
			t.Run(fmt.Sprintf("%s/fail-sync-%d", name, failAt), func(t *testing.T) {
				var mu sync.Mutex
				var calls []string
				var requests atomic.Int32
				var premature atomic.Bool
				var c Config
				registry := &mockRegistry{data: map[string][]byte{}}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					mu.Lock()
					ready := len(calls) == 2 && calls[1] == c.StateDir
					if ready && restart {
						ready = calls[0] == filepath.Join(c.StateDir, c.Plan.ID+".intent")
					}
					if ready && !restart {
						ready = strings.HasPrefix(calls[0], filepath.Join(c.StateDir, ".intent-"))
					}
					mu.Unlock()
					if !ready {
						premature.Store(true)
					}
					registry.ServeHTTP(w, r)
				}))
				defer server.Close()
				c = fixture(t, server.URL)
				if restart {
					seedRenamedIntent(t, c)
				}
				syncOperation := func(f *os.File) error {
					mu.Lock()
					calls = append(calls, f.Name())
					n := len(calls)
					mu.Unlock()
					if n == failAt {
						return fmt.Errorf("injected sync failure; internal path and details must stay private")
					}
					return f.Sync()
				}
				observation, err := publishWithSync(context.Background(), c, syncOperation)
				if failAt != 0 {
					if err != invalid || observation != nil || requests.Load() != 0 {
						t.Fatalf("sync failure authorized I/O: error=%v requests=%d", err, requests.Load())
					}
					mu.Lock()
					count := len(calls)
					mu.Unlock()
					if count != failAt {
						t.Fatalf("unexpected sync calls: %d", count)
					}
					// The lock must be released even when either durability barrier fails.
					release, err := intent(c.StateDir, c.Plan.ID, func() string { b, _ := json.Marshal(c.Plan); return hash(b) }())
					if err != nil {
						t.Fatal("recovery after failed barrier", err)
					}
					release()
					return
				}
				if err != nil || observation == nil || requests.Load() == 0 || premature.Load() {
					t.Fatalf("durability ordering: error=%v requests=%d premature=%v", err, requests.Load(), premature.Load())
				}
			})
		}
	}
}
