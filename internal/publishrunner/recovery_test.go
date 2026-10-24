package publishrunner

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/ociverify"
	"releasecontrol/internal/workerconfig"
	"strings"
	"sync"
	"testing"
	"time"
)

func testDigest(b []byte) string { return fmt.Sprintf("sha256:%x", sha256.Sum256(b)) }
func archiveFixture(t *testing.T) (string, domain.WorkerLocalOCI, map[string][]byte) {
	t.Helper()
	files := map[string][]byte{}
	add := func(media string, b []byte) map[string]any {
		d := testDigest(b)
		files["blobs/sha256/"+strings.TrimPrefix(d, "sha256:")] = b
		return map[string]any{"mediaType": media, "digest": d, "size": len(b)}
	}
	var layer bytes.Buffer
	tw := tar.NewWriter(&layer)
	tw.WriteHeader(&tar.Header{Name: "app", Size: 3, Mode: 0555})
	tw.Write([]byte("app"))
	tw.Close()
	cfg, _ := json.Marshal(map[string]any{"os": "linux", "architecture": "arm64", "rootfs": map[string]any{"type": "layers", "diff_ids": []string{testDigest(layer.Bytes())}}})
	manifest, _ := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json", "config": add("application/vnd.oci.image.config.v1+json", cfg), "layers": []any{add("application/vnd.oci.image.layer.v1.tar", layer.Bytes())}})
	idx, _ := json.Marshal(map[string]any{"schemaVersion": 2, "manifests": []any{add("application/vnd.oci.image.manifest.v1+json", manifest)}})
	files["index.json"] = idx
	files["oci-layout"] = []byte(`{"imageLayoutVersion":"1.0.0"}`)
	var archive bytes.Buffer
	tw = tar.NewWriter(&archive)
	for n, b := range files {
		tw.WriteHeader(&tar.Header{Name: n, Size: int64(len(b)), Mode: 0600})
		tw.Write(b)
	}
	tw.Close()
	p := filepath.Join(t.TempDir(), "image.tar")
	if e := os.WriteFile(p, archive.Bytes(), 0600); e != nil {
		t.Fatal(e)
	}
	r, e := ociverify.VerifyOCIBytes(archive.Bytes(), 1<<20, "linux/arm64")
	if e != nil {
		t.Fatal(e)
	}
	r.SourceSHA = strings.Repeat("a", 40)
	r.RecipeHash = "sha256:" + strings.Repeat("b", 64)
	r.JobID = "build"
	r.AttemptID = "build-attempt"
	r.InputHash = testDigest([]byte("build"))
	r.ConfigRevision = "cfg1"
	r.ToolchainImage = "golang@" + testDigest([]byte("compiler"))
	remote := map[string][]byte{}
	for n, b := range files {
		if strings.HasPrefix(n, "blobs/sha256/") {
			d := "sha256:" + strings.TrimPrefix(n, "blobs/sha256/")
			kind := "blobs"
			if d == r.ManifestDigest {
				kind = "manifests"
			}
			remote["/v2/fixture/app/"+kind+"/"+d] = b
		}
	}
	return p, r, remote
}
func testLease(c Config, r domain.WorkerLocalOCI) domain.WorkerLease {
	in := domain.WorkerInputs{RecipeID: c.Recipe.ID, RecipeRevision: c.Recipe.Revision, RecipeHash: c.Recipe.Hash, ConfigRevision: c.ConfigRevision, RepositoryID: c.Recipe.RepositoryID, SourceSHA: r.SourceSHA, Publication: &domain.WorkerPublication{ComponentID: c.Recipe.ComponentID, Profile: c.Recipe.Profile, Origin: c.Recipe.Origin, Repository: c.Recipe.Repository, Receipt: r}}
	h := workerconfig.Fingerprint(in)
	return domain.WorkerLease{Job: domain.WorkerJob{Meta: domain.Meta{ID: "publication", ProductID: c.Recipe.ProductID}, Capability: "registry_publish", Inputs: in, InputHash: h, Fence: 1, Deadline: time.Now().Add(time.Minute)}, Attempt: domain.WorkerAttempt{Meta: domain.Meta{ID: "attempt"}, JobID: "publication", InputHash: h, Fence: 1, LeaseUntil: time.Now().Add(time.Minute)}}
}
func TestUncertainPublicationReadOnlyRecovery(t *testing.T) {
	for _, mode := range []string{"complete-lost-report", "missing", "unauthorized", "corrupt", "expired", "missing-phase"} {
		t.Run(mode, func(t *testing.T) {
			archive, r, data := archiveFixture(t)
			var mu sync.Mutex
			gets, writes, reports, starts := 0, 0, 0, 0
			registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if q.Method != "GET" {
					writes++
					w.WriteHeader(500)
					return
				}
				gets++
				if mode == "unauthorized" {
					w.WriteHeader(401)
					return
				}
				if mode == "missing" {
					w.WriteHeader(404)
					return
				}
				b := data[q.URL.Path]
				if mode == "corrupt" {
					b = []byte("corrupt")
				}
				if b == nil {
					w.WriteHeader(404)
					return
				}
				w.Write(b)
			}))
			defer registry.Close()
			c := pinnedConfig(t)
			c.Publisher.Archive = archive
			c.Publisher.StateDir = t.TempDir()
			c.Publisher.StateDir, _ = filepath.EvalSymlinks(c.Publisher.StateDir)
			os.Chmod(c.Publisher.StateDir, 0700)
			c.Recipe.Origin = registry.URL
			c.Recipe.Pull.FixtureLoopbackHTTP = true
			c.Recipe.MaxBytes = 1 << 20
			c.Recipe.BuildRecipeHash = r.RecipeHash
			c.Recipe.Hash = workerconfig.PublicationHash(c.Recipe)
			c.Publisher.AllowedOrigin = registry.URL
			c.Publisher.MaxArchiveBytes = 1 << 20
			c.Publisher.FixtureLoopbackHTTP = true
			l := testLease(c, r)
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
				switch q.URL.Path {
				case "/api/worker/start":
					starts++
					if mode == "expired" {
						w.WriteHeader(409)
						return
					}
				case "/api/worker/report":
					reports++
					if reports == 1 {
						w.WriteHeader(500)
						return
					}
				case "/api/worker/claim":
					t.Error("uncertain execution claimed new job")
				}
				json.NewEncoder(w).Encode(l)
			}))
			defer api.Close()
			c.Endpoint = api.URL
			t.Setenv(c.TokenEnv, strings.Repeat("x", 40))
			j := journal{Phase: "PUBLISH_INTENT", ConfigHash: workerconfig.Fingerprint(c), Lease: l}
			if mode == "missing-phase" {
				j.Phase = ""
			}
			file := filepath.Join(c.Publisher.StateDir, "native.json")
			if e := durable(file, j); e != nil {
				t.Fatal(e)
			}
			if _, e := RunOnce(context.Background(), c); e == nil {
				t.Fatal("first recovery should fail closed or retain lost report")
			}
			mu.Lock()
			before := gets
			mu.Unlock()
			if mode == "complete-lost-report" {
				if reports != 1 {
					t.Fatal("complete closure did not produce report")
				}
				if _, e := RunOnce(context.Background(), c); e != nil {
					t.Fatal(e)
				}
				mu.Lock()
				if gets != before {
					t.Error("known report replay repeated registry observation")
				}
				mu.Unlock()
			} else if reports != 0 {
				t.Fatal("uncertain bytes admitted")
			}
			mu.Lock()
			if writes != 0 {
				t.Error("unknown publication replayed registry writes")
			}
			mu.Unlock()
			if mode == "missing-phase" && starts != 0 {
				t.Fatal("missing phase treated as safe")
			}
		})
	}
}
func TestLostStartBeforeIntentAndCrashBeforeIntent(t *testing.T) {
	for _, phase := range []string{"CLAIMED", "STARTED"} {
		t.Run(phase, func(t *testing.T) {
			archive, r, _ := archiveFixture(t)
			var mu sync.Mutex
			data := map[string][]byte{}
			writes := 0
			registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				switch q.Method {
				case "GET":
					b := data[q.URL.Path]
					if b == nil {
						w.WriteHeader(404)
						return
					}
					w.Write(b)
				case "POST":
					writes++
					w.Header().Set("Location", "/v2/fixture/app/blobs/uploads/session")
					w.WriteHeader(202)
				case "PUT":
					writes++
					p := q.URL.Path
					if strings.Contains(p, "uploads") {
						p = "/v2/fixture/app/blobs/" + q.URL.Query().Get("digest")
					}
					data[p], _ = io.ReadAll(q.Body)
					w.WriteHeader(201)
				}
			}))
			defer registry.Close()
			c := pinnedConfig(t)
			c.Publisher.Archive = archive
			c.Publisher.StateDir = t.TempDir()
			c.Publisher.StateDir, _ = filepath.EvalSymlinks(c.Publisher.StateDir)
			os.Chmod(c.Publisher.StateDir, 0700)
			c.Recipe.Origin = registry.URL
			c.Recipe.Pull.FixtureLoopbackHTTP = true
			c.Recipe.MaxBytes = 1 << 20
			c.Recipe.BuildRecipeHash = r.RecipeHash
			c.Recipe.Hash = workerconfig.PublicationHash(c.Recipe)
			c.Publisher.AllowedOrigin = registry.URL
			c.Publisher.MaxArchiveBytes = 1 << 20
			c.Publisher.FixtureLoopbackHTTP = true
			l := testLease(c, r)
			starts := 0
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
				if q.URL.Path == "/api/worker/start" {
					starts++
					if starts == 1 {
						conn, _, _ := w.(http.Hijacker).Hijack()
						conn.Close()
						return
					}
				}
				json.NewEncoder(w).Encode(l)
			}))
			defer api.Close()
			c.Endpoint = api.URL
			t.Setenv(c.TokenEnv, strings.Repeat("x", 40))
			if e := durable(filepath.Join(c.Publisher.StateDir, "native.json"), journal{Phase: phase, ConfigHash: workerconfig.Fingerprint(c), Lease: l}); e != nil {
				t.Fatal(e)
			}
			if _, e := RunOnce(context.Background(), c); e == nil {
				t.Fatal("lost start response accepted")
			}
			mu.Lock()
			if writes != 0 {
				t.Error("publication before START acknowledgement")
			}
			mu.Unlock()
			if _, e := RunOnce(context.Background(), c); e != nil {
				t.Fatal(e)
			}
			mu.Lock()
			if writes == 0 {
				t.Error("first publication never executed after acknowledged START")
			}
			mu.Unlock()
		})
	}
}
