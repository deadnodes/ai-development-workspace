package github

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"releasecontrol/internal/delivery"
)

func fixture(t *testing.T, handler http.HandlerFunc) (*Provider, delivery.Connection, *atomic.Int32) {
	t.Helper()
	key, e := rsa.GenerateKey(rand.Reader, 2048)
	if e != nil {
		t.Fatal(e)
	}
	t.Setenv("RCP_TEST_APP_KEY", string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})))
	var tokens atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app/installations/22/access_tokens" {
			tokens.Add(1)
			parts := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), ".")
			if len(parts) != 3 {
				t.Error("missing App JWT")
				w.WriteHeader(401)
				return
			}
			sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
			sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
			if e := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, sum[:], sig); e != nil {
				t.Error("invalid JWT signature")
			}
			claims, _ := base64.RawURLEncoding.DecodeString(parts[1])
			var c map[string]any
			json.Unmarshal(claims, &c)
			if c["iss"] != "11" {
				t.Error("wrong App issuer")
			}
			if c["exp"].(float64)-float64(time.Now().Unix()) > 600 {
				t.Error("JWT expiry over10min")
			}
			json.NewEncoder(w).Encode(map[string]any{"token": "installation-secret", "expires_at": time.Now().Add(time.Hour)})
			return
		}
		if r.Header.Get("Authorization") != "Bearer installation-secret" {
			t.Errorf("API request lacks installation token: %s", r.URL.Path)
		}
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	t.Setenv("RCP_GITHUB_API_URL", server.URL)
	p := New()
	return p, delivery.Connection{ID: "conn", AppID: 11, InstallationID: 22, PrivateKeyRef: "env:RCP_TEST_APP_KEY", APIURL: server.URL, Owner: "acme"}, &tokens
}
func jsonOut(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
func TestAppPaginationPinnedCompareAndTokenCache(t *testing.T) {
	base := strings.Repeat("a", 40)
	head := strings.Repeat("b", 40)
	p, c, tokens := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/installation/repositories":
			list := []map[string]any{}
			n := 1
			if r.URL.Query().Get("page") == "1" {
				n = 100
			}
			for i := 0; i < n; i++ {
				list = append(list, map[string]any{"id": int64(123 + i), "full_name": "acme/repo" + strconv.Itoa(i), "default_branch": "main", "private": true})
			}
			jsonOut(w, map[string]any{"repositories": list})
		case r.URL.Path == "/repositories/123":
			jsonOut(w, map[string]any{"id": 123, "full_name": "acme/repo"})
		case r.URL.Path == "/repos/acme/repo/branches":
			jsonOut(w, []any{map[string]any{"name": "main", "protected": true, "commit": map[string]any{"sha": head}}})
		case strings.HasPrefix(r.URL.Path, "/repos/acme/repo/compare/"):
			jsonOut(w, map[string]any{"ahead_by": 1, "behind_by": 2, "status": "diverged", "base_commit": map[string]any{"sha": base}, "commits": []any{map[string]any{"sha": head, "html_url": "https://github.com/acme/repo/commit/" + head, "commit": map[string]any{"message": "Payment change", "author": map[string]any{"name": "Dev", "date": "2026-01-01T00:00:00Z"}}}}})
		default:
			t.Errorf("unexpected %s", r.URL)
			w.WriteHeader(404)
		}
	})
	repos, e := p.DiscoverRepositories(context.Background(), c)
	if e != nil || len(repos) != 101 {
		t.Fatalf("pagination: %d %v", len(repos), e)
	}
	branches, e := p.Branches(context.Background(), c, "123")
	if e != nil || len(branches) != 1 || branches[0].SHA != head {
		t.Fatalf("branches: %+v %v", branches, e)
	}
	compare, e := p.Compare(context.Background(), c, "123", base, head)
	if e != nil || compare.Ahead != 1 || compare.Behind != 2 || len(compare.Commits) != 1 || compare.Commits[0].Message != "Payment change" {
		t.Fatalf("compare: %+v %v", compare, e)
	}
	if tokens.Load() != 1 {
		t.Fatalf("installation token not cached: %d", tokens.Load())
	}
	if _, e = p.Compare(context.Background(), c, "123", "main", head); e == nil {
		t.Fatal("mutable comparison accepted")
	}
}
func reportZIP(t *testing.T, r workflowReport) []byte {
	t.Helper()
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	f, e := z.Create("result.json")
	if e != nil {
		t.Fatal(e)
	}
	if e = json.NewEncoder(f).Encode(r); e != nil {
		t.Fatal(e)
	}
	z.Close()
	return b.Bytes()
}
func TestDispatchCorrelationAndPrivateWorkflowEvidence(t *testing.T) {
	sha := strings.Repeat("b", 40)
	workflowSHA := strings.Repeat("a", 40)
	digest := "sha256:" + strings.Repeat("d", 64)
	now := time.Now().UTC()
	report := workflowReport{OperationID: "op-1", SourceSHA: sha, ImageRepository: "ghcr.io/acme/api", ImageTag: "rcp-tag", Digest: digest, Available: true, ObservedAt: now}
	archive := reportZIP(t, report)
	var dispatched bool
	p, c, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/acme/repo/commits/feature":
			jsonOut(w, map[string]any{"sha": workflowSHA})
		case strings.HasSuffix(r.URL.Path, "/dispatches"):
			var payload struct {
				Ref     string            `json:"ref"`
				Inputs  map[string]string `json:"inputs"`
				Details bool              `json:"return_run_details"`
			}
			json.NewDecoder(r.Body).Decode(&payload)
			if payload.Ref != "feature" || payload.Inputs["source_sha"] != sha || payload.Inputs["operation_id"] != "op-1" || !payload.Details {
				t.Errorf("bad dispatch: %+v", payload)
			}
			dispatched = true
			jsonOut(w, map[string]any{"workflow_run_id": 42})
		case r.URL.Path == "/repos/acme/repo/actions/workflows/build.yml/runs":
			jsonOut(w, map[string]any{"workflow_runs": []any{map[string]any{"id": 99, "display_title": "rcp-unrelated", "status": "completed", "conclusion": "success"}, map[string]any{"id": 42, "display_title": "rcp-op-1", "status": "completed", "conclusion": "success", "head_sha": workflowSHA, "created_at": now}}})
		case r.URL.Path == "/repos/acme/repo/actions/runs/42":
			jsonOut(w, map[string]any{"id": 42, "display_title": "rcp-op-1", "status": "completed", "conclusion": "success"})
		case r.URL.Path == "/repos/acme/repo/actions/runs/42/artifacts":
			jsonOut(w, map[string]any{"artifacts": []any{map[string]any{"id": 77, "name": "rcp-result-op-1", "expired": false}}})
		case r.URL.Path == "/repos/acme/repo/actions/artifacts/77/zip":
			w.Write(archive)
		default:
			t.Errorf("unexpected path %s", r.URL)
			w.WriteHeader(404)
		}
	})
	request := delivery.BuildRequest{Repository: "acme/repo", Workflow: "build.yml", Ref: "feature", WorkflowSHA: workflowSHA, SourceSHA: sha, ImageTag: "rcp-tag", OperationID: "op-1", Inputs: map[string]string{"source_sha": "tampered"}, RequestedAt: now}
	result, e := p.Dispatch(context.Background(), c, request)
	if e != nil || !dispatched || result.ProviderRunID != 42 {
		t.Fatalf("dispatch: %+v %v", result, e)
	}
	run, e := p.FindRun(context.Background(), c, request)
	if e != nil || run.ID != 42 || run.Artifacts["digest"] != digest {
		t.Fatalf("run: %+v %v", run, e)
	}
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Fatal("App token forwarded to GHCR")
		}
		w.WriteHeader(403)
	}))
	defer registry.Close()
	p.registryURL = registry.URL
	artifactRequest := delivery.ArtifactRequest{Repository: "ghcr.io/acme/api", Tag: "rcp-tag", ExpectedDigest: digest, EvidenceRepository: "acme/repo", EvidenceRunID: 42, EvidenceOperationID: "op-1", SourceSHA: sha}
	artifact, e := p.InspectArtifact(context.Background(), c, artifactRequest)
	if e != nil || !artifact.Available || artifact.Digest != digest {
		t.Fatalf("private evidence: %+v %v", artifact, e)
	}
	p.now = func() time.Time { return now.Add(11 * time.Minute) }
	if _, e = p.InspectArtifact(context.Background(), c, artifactRequest); e == nil {
		t.Fatal("stale private evidence accepted")
	}
	artifactRequest.EvidenceRunID = 0
	if _, e = p.InspectArtifact(context.Background(), c, artifactRequest); !errors.Is(e, ErrUnauthorized) {
		t.Fatalf("private unauthorized reported missing: %v", e)
	}
}
func TestGHCRAnonymousManifestAndMissingVersusUnauthorized(t *testing.T) {
	manifest := []byte(`{"schemaVersion":2}`)
	sum := sha256.Sum256(manifest)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	var status atomic.Int32
	status.Store(200)
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			jsonOut(w, map[string]string{"token": "registry-anon"})
			return
		}
		if status.Load() != 200 {
			w.WriteHeader(int(status.Load()))
			return
		}
		if r.Header.Get("Authorization") == "" {
			w.WriteHeader(401)
			return
		}
		if r.Header.Get("Authorization") != "Bearer registry-anon" {
			t.Fatal("wrong registry token")
		}
		w.Header().Set("Docker-Content-Digest", digest)
		w.Write(manifest)
	}))
	defer registry.Close()
	p := New()
	p.registryURL = registry.URL
	r := delivery.ArtifactRequest{Repository: "ghcr.io/acme/api", Tag: "v1"}
	out, e := p.InspectArtifact(context.Background(), delivery.Connection{}, r)
	if e != nil || !out.Available || out.Digest != digest {
		t.Fatalf("manifest: %+v %v", out, e)
	}
	status.Store(404)
	out, e = p.InspectArtifact(context.Background(), delivery.Connection{}, r)
	if e != nil || out.Available {
		t.Fatal("404 not missing")
	}
	status.Store(403)
	_, e = p.InspectArtifact(context.Background(), delivery.Connection{}, r)
	if !errors.Is(e, ErrUnauthorized) {
		t.Fatalf("403 wrong: %v", e)
	}
}
func TestGitOpsBlobCASPreservesYAMLAndHelmTag(t *testing.T) {
	head := strings.Repeat("a", 40)
	blob := strings.Repeat("b", 40)
	newHead := strings.Repeat("c", 40)
	newBlob := strings.Repeat("d", 40)
	digest := "sha256:" + strings.Repeat("e", 64)
	for _, helm := range []bool{false, true} {
		t.Run(fmt.Sprint(helm), func(t *testing.T) {
			source := "# keep this comment\nreplicas: 3\nimage:\n  repository: ghcr.io/acme/api\n  digest: old\nother:\n  enabled: true\n"
			if helm {
				source = "# HelmRelease\napiVersion: helm.toolkit.fluxcd.io/v2\nkind: HelmRelease\nspec:\n  values:\n    replicaCount: 3\n    image:\n      repository: ghcr.io/acme/api\n      tag: latest\n    other: true\n"
			}
			var puts int
			p, c, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/git/ref/heads/") {
					jsonOut(w, map[string]any{"object": map[string]string{"sha": head}})
					return
				}
				if strings.Contains(r.URL.Path, "/contents/") {
					if r.Method == "GET" {
						if r.URL.Query().Get("ref") != head {
							t.Error("file read not pinned to head")
						}
						jsonOut(w, map[string]any{"type": "file", "sha": blob, "encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(source))})
						return
					}
					puts++
					var in struct {
						SHA     string `json:"sha"`
						Content string `json:"content"`
						Message string `json:"message"`
						Branch  string `json:"branch"`
					}
					json.NewDecoder(r.Body).Decode(&in)
					if in.SHA != blob || in.Branch != "main" || !strings.Contains(in.Message, "[rcp:op]") {
						t.Errorf("missing CAS/audit: %+v", in)
					}
					raw, _ := base64.StdEncoding.DecodeString(in.Content)
					if !strings.Contains(string(raw), "other:") || !strings.Contains(string(raw), "#") {
						t.Errorf("unrelated YAML lost: %s", raw)
					}
					if helm && !strings.Contains(string(raw), "rcp@"+digest) {
						t.Errorf("digest tag missing: %s", raw)
					}
					jsonOut(w, map[string]any{"content": map[string]string{"sha": newBlob}, "commit": map[string]string{"sha": newHead}})
					return
				}
				w.WriteHeader(404)
			})
			request := delivery.GitOpsRequest{Repository: "acme/repo", Ref: "main", Path: "values.yaml", ImageRepository: "ghcr.io/acme/api"}
			if helm {
				request.ImageField = "spec.values.image.repository"
				request.DigestField = "spec.values.image.tag"
			}
			apply := delivery.GitOpsApply{Request: request, ExpectedHeadSHA: head, ExpectedBlobSHA: blob, Digest: digest, OperationID: "op"}
			out, e := p.ApplyGitOps(context.Background(), c, apply)
			if e != nil || out.CommitSHA != newHead || puts != 1 {
				t.Fatalf("apply: %+v %v puts%d", out, e, puts)
			}
			apply.ExpectedBlobSHA = newBlob
			if _, e = p.ApplyGitOps(context.Background(), c, apply); !errors.Is(e, ErrConflict) || puts != 1 {
				t.Fatal("stale blob reached mutation")
			}
			apply.ExpectedBlobSHA = blob
			apply.ExpectedHeadSHA = newHead
			if _, e = p.ApplyGitOps(context.Background(), c, apply); !errors.Is(e, ErrConflict) || puts != 1 {
				t.Fatal("stale head reached mutation")
			}
		})
	}
}
func TestCredentialRestrictionsAndRedactedRedirectErrors(t *testing.T) {
	t.Setenv("UNSAFE_PRIVATE_KEY", "sensitive")
	if _, e := secret("env:UNSAFE_PRIVATE_KEY"); e == nil {
		t.Fatal("non-RCP env accepted")
	}
	dir := t.TempDir()
	t.Setenv("RCP_SECRET_DIR", dir)
	path := filepath.Join(dir, "key")
	os.WriteFile(path, []byte("secret"), 0600)
	if b, e := secret("file:" + path); e != nil || string(b) != "secret" {
		t.Fatal(e)
	}
	outside := filepath.Join(t.TempDir(), "other")
	os.WriteFile(outside, []byte("outside"), 0600)
	os.Symlink(outside, filepath.Join(dir, "link"))
	if _, e := secret("file:" + filepath.Join(dir, "link")); e == nil {
		t.Fatal("symlink escaped secret directory")
	}
	var leaked atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Add(1) }))
	defer target.Close()
	p, c, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", target.URL+"/secret")
		w.WriteHeader(302)
		w.Write([]byte("installation-secret"))
	})
	e := p.TestConnection(context.Background(), c)
	if e == nil || strings.Contains(e.Error(), "installation-secret") || leaked.Load() != 0 {
		t.Fatalf("redirect/redaction failed: %v calls%d", e, leaked.Load())
	}
}

// Explicitly opt-in, read-only live verification. Never runs from ordinary tests.
func TestLiveAppReadOnly(t *testing.T) {
	if os.Getenv("RCP_GITHUB_LIVE_TEST") != "1" {
		t.Skip("set RCP_GITHUB_LIVE_TEST=1 and App secret reference for live reads")
	}
	app, e := strconv.ParseInt(os.Getenv("RCP_GITHUB_APP_ID"), 10, 64)
	if e != nil {
		t.Fatal("invalid RCP_GITHUB_APP_ID")
	}
	installation, e := strconv.ParseInt(os.Getenv("RCP_GITHUB_INSTALLATION_ID"), 10, 64)
	if e != nil {
		t.Fatal("invalid RCP_GITHUB_INSTALLATION_ID")
	}
	c := delivery.Connection{AppID: app, InstallationID: installation, PrivateKeyRef: os.Getenv("RCP_GITHUB_PRIVATE_KEY_REF"), Owner: os.Getenv("RCP_GITHUB_OWNER"), APIURL: os.Getenv("RCP_GITHUB_API_URL")}
	repos, e := New().DiscoverRepositories(context.Background(), c)
	if e != nil {
		t.Fatal(e)
	}
	t.Logf("discovered %d installation repositories", len(repos))
	testRepository := os.Getenv("RCP_GITHUB_TEST_REPOSITORY")
	testBranch := os.Getenv("RCP_GITHUB_TEST_BRANCH")
	if testRepository == "" || testBranch == "" {
		t.Fatal("live test requires RCP_GITHUB_TEST_REPOSITORY (numeric ID or owner/name) and RCP_GITHUB_TEST_BRANCH")
	}
	var selected delivery.RepositoryInfo
	for _, repo := range repos {
		if testRepository == strconv.FormatInt(repo.ID, 10) || strings.EqualFold(testRepository, repo.FullName) {
			selected = repo
		}
	}
	if selected.ID == 0 {
		t.Fatal("test repository is not accessible to installation")
	}
	provider := New()
	locator := strconv.FormatInt(selected.ID, 10)
	branches, err := provider.Branches(context.Background(), c, locator)
	if err != nil {
		t.Fatal(err)
	}
	var base, head string
	for _, branch := range branches {
		if branch.Name == selected.DefaultBranch {
			base = branch.SHA
		}
		if branch.Name == testBranch {
			head = branch.SHA
		}
	}
	if base == "" || head == "" {
		t.Fatal("configured base or test branch missing")
	}
	comparison, err := provider.Compare(context.Background(), c, locator, base, head)
	if err != nil {
		t.Fatal(err)
	}
	if comparison.BaseSHA != base || comparison.HeadSHA != head || comparison.Ahead < 0 || comparison.Behind < 0 {
		t.Fatal("invalid live branch comparison")
	}
	t.Logf("repository %d branch %s ahead=%d behind=%d", selected.ID, testBranch, comparison.Ahead, comparison.Behind)
}
