package prodsigner

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"releasecontrol/internal/imagesigning"
	"releasecontrol/internal/imagesigning/signingtest"
	"strings"
	"sync/atomic"
	"testing"
)

type countingTransport struct {
	base http.RoundTripper
	prod atomic.Int64
}

func (t *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.Contains(r.URL.Path, "prod-signatures") {
		t.prod.Add(1)
	}
	return t.base.RoundTrip(r)
}

type fakeSource struct {
	base    http.RoundTripper
	archive []byte
	digest  string
	cache   *fakeCache
}
type fakeCache struct {
	raw   string
	rv    string
	patch []byte
	fail  bool
}

func (f fakeSource) RoundTrip(r *http.Request) (*http.Response, error) {
	w := httptest.NewRecorder()
	switch r.URL.Host {
	case "kube.test":
		if strings.Contains(r.URL.Path, "/configmaps/") {
			if f.cache == nil || f.cache.fail {
				w.WriteHeader(500)
				break
			}
			if r.Method == "PATCH" {
				f.cache.patch, _ = io.ReadAll(r.Body)
				w.WriteHeader(200)
				break
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]string{"resourceVersion": f.cache.rv}, "data": map[string]string{"verified.json": f.cache.raw}})
			break
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": map[string]any{"artifact": Artifact{URL: "http://artifact.test/archive?secret=redacted", Revision: "main@sha1:abc", Digest: f.digest}}})
	case "artifact.test":
		if r.Header.Get("Authorization") != "" {
			return nil, imagesigning.ErrUnavailable
		}
		w.Write(f.archive)
	default:
		return f.base.RoundTrip(r)
	}
	return w.Result(), nil
}
func archive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tr := tar.NewWriter(gz)
	for p, s := range files {
		if e := tr.WriteHeader(&tar.Header{Name: p, Mode: 0600, Size: int64(len(s))}); e != nil {
			t.Fatal(e)
		}
		tr.Write([]byte(s))
	}
	tr.Close()
	gz.Close()
	return b.Bytes()
}
func TestReconcile(t *testing.T) {
	for _, mode := range []string{"normal", "missing", "branch", "builder", "backfill", "already", "digest", "traversal", "revision", "checks", "registry"} {
		t.Run(mode, func(t *testing.T) {
			f := signingtest.New(t)
			client := imagesigning.NewWithClient(f.Client())
			digest := f.Image("abc")
			ref := "registry.deadnodes.internal/deadnodes/app@" + digest
			var c Config
			c.FluxArtifact.Namespace = "flux-system"
			c.FluxArtifact.Name = "flux-system"
			c.FluxArtifact.Kind = "GitRepository"
			c.Paths = []string{"clusters/prod/"}
			c.OwnImagePrefixes = []string{"registry.deadnodes.internal/deadnodes/"}
			c.Registry.Origin = f.Registry.URL
			c.Registry.InsecureHTTP = true
			c.Registry.CredentialsFile = filepath.Join(f.Root, "registry.json")
			c.Registry.Rewrite = map[string]string{"registry.deadnodes.internal/": ""}
			c.SignaturesRepository = "prod-signatures"
			c.DevPublicKeyPEM = f.Key("dev").PublicKeyPEM
			c.ProdKey = f.Key("prod")
			c.BuilderIDs = []string{"https://builder.test/v1"}
			c.BackfillFile = "clusters/prod/backfill.yaml"
			tag, builder, sha := "main-abc", c.BuilderIDs[0], "abc"
			if mode == "branch" {
				tag = "br-abc"
			}
			if mode == "builder" {
				builder = "https://wrong.test"
			}
			if mode == "revision" {
				sha = "wrong"
			}
			state := "success"
			if mode == "checks" {
				state = "failure"
			}
			p := imagesigning.Predicate{Type: imagesigning.ProvenanceType, Body: map[string]any{"buildDefinition": map[string]any{"buildType": "https://deadnodes.dev/rcp/forgejo-build/v1", "externalParameters": map[string]string{"tag": tag, "source_sha": sha}, "internalParameters": map[string]string{"build_id": "build1"}}, "runDetails": map[string]any{"builder": map[string]string{"id": builder}, "byproducts": []any{map[string]any{"name": "required_status_contexts", "annotations": map[string]any{"required": []string{"ci"}, "observed": map[string]string{"ci": state}}}}}}}
			if mode != "missing" && mode != "backfill" {
				_, e := client.PublishIdentity(context.Background(), f.RegistryConfig(), "deadnodes/app", "registry.json", f.Key("dev"), digest, "build1", "abc", "registry.deadnodes.internal/deadnodes/app", p)
				if e != nil {
					t.Fatal(e)
				}
			}
			files := map[string]string{"clusters/prod/app.yaml": "kind: HelmRelease\nspec:\n  values:\n    image:\n      repository: registry.deadnodes.internal/deadnodes/app\n      tag: main-abc@" + digest + "\n"}
			if mode == "backfill" {
				files[c.BackfillFile] = "digests:\n- ref: " + ref + "\n  reason: legacy\n  approved_in: https://flux.test/pull/1\n"
			}
			if mode == "already" {
				_, e := client.PublishIdentity(context.Background(), f.RegistryConfig(), c.SignaturesRepository, "registry.json", c.ProdKey, digest, "", "", "registry.deadnodes.internal/deadnodes/app", imagesigning.Predicate{Type: ApprovalType, Body: map[string]string{}})
				if e != nil {
					t.Fatal(e)
				}
			}
			if mode == "traversal" {
				files["../escape.yaml"] = "image: " + ref
			}
			b := archive(t, files)
			hash := imagesigning.Hash(b)
			if mode == "digest" {
				hash = "sha256:" + strings.Repeat("0", 64)
			}
			transport := fakeSource{base: f.Client().Transport, archive: b, digest: hash}
			source := Source{HTTP: &http.Client{Transport: transport}, API: "http://kube.test", Token: "kube-token"}
			if mode == "registry" {
				f.Unavailable = true
				c.Registry.Origin = "http://unavailable.test"
			}
			s, e := Reconcile(context.Background(), c, source, client, io.Discard)
			switch mode {
			case "digest", "traversal", "registry":
				if e == nil || s.Errors == 0 {
					t.Fatalf("%+v %v", s, e)
				}
			case "missing", "branch", "builder", "revision", "checks":
				if e != nil || s.Unapproved != 1 || s.Signed != 0 {
					t.Fatalf("%+v %v", s, e)
				}
			case "already":
				if e != nil || s.AlreadySigned != 1 {
					t.Fatalf("%+v %v", s, e)
				}
			default:
				if e != nil || s.Signed != 1 {
					t.Fatalf("%+v %v", s, e)
				}
			}
		})
	}
}
func TestStrictConfig(t *testing.T) {
	if _, e := Decode(strings.NewReader("{\"unknown\":true}")); e == nil {
		t.Fatal("unknown field accepted")
	}
}

func TestPromoteModeConfigIsStrictWithoutFluxFields(t *testing.T) {
	c, _, _, _, _, _ := promoteFixture(t)
	if err := c.ValidateMode("promote"); err != nil {
		t.Fatalf("valid promote config: %v", err)
	}
	if err := c.ValidateMode("flux"); err == nil {
		t.Fatal("promote-only config accepted for flux mode")
	}
	c.RCP.URL = "http://rcp.example"
	if err := c.ValidateMode("promote"); err == nil {
		t.Fatal("non-HTTPS RCP accepted")
	}
}

func TestVerifiedDigestCache(t *testing.T) {
	for _, mode := range []string{"cold", "fresh", "expired", "pruned", "unavailable", "future"} {
		t.Run(mode, func(t *testing.T) {
			f := signingtest.New(t)
			digest := f.Image("abc")
			ref := "registry.deadnodes.internal/deadnodes/app@" + digest
			var c Config
			c.FluxArtifact.Namespace = "flux-system"
			c.FluxArtifact.Name = "flux-system"
			c.FluxArtifact.Kind = "GitRepository"
			c.Paths = []string{"clusters/prod/"}
			c.OwnImagePrefixes = []string{"registry.deadnodes.internal/deadnodes/"}
			c.Registry.Origin = f.Registry.URL
			c.Registry.InsecureHTTP = true
			c.Registry.CredentialsFile = filepath.Join(f.Root, "registry.json")
			c.Registry.Rewrite = map[string]string{"registry.deadnodes.internal/": ""}
			c.SignaturesRepository = "prod-signatures"
			c.DevPublicKeyPEM = f.Key("dev").PublicKeyPEM
			c.ProdKey = f.Key("prod")
			c.BuilderIDs = []string{"https://builder.test/v1"}
			c.Cache.ConfigMap = "prod-signer-cache"
			c.Cache.TTLSeconds = 3600
			if err := c.Validate(); err != nil {
				t.Fatal(err)
			}
			for _, invalid := range []struct {
				name string
				ttl  int
			}{{"Uppercase", 60}, {"bad/name", 60}, {"too-short", 59}, {"too-long", 86401}} {
				c.Cache.ConfigMap, c.Cache.TTLSeconds = invalid.name, invalid.ttl
				if c.Validate() == nil {
					t.Fatalf("accepted invalid cache config: %+v", c.Cache)
				}
			}
			c.Cache.ConfigMap, c.Cache.TTLSeconds = "", 0
			if err := c.Validate(); err != nil {
				t.Fatal(err)
			}
			c.Cache.TTLSeconds = 60
			if c.Validate() == nil {
				t.Fatal("accepted TTL without ConfigMap")
			}
			c.Cache.ConfigMap, c.Cache.TTLSeconds = "prod-signer-cache", 3600
			seedClient := imagesigning.NewWithClient(f.Client())
			predicate := imagesigning.Predicate{Type: imagesigning.ProvenanceType, Body: map[string]any{
				"buildDefinition": map[string]any{"buildType": "https://deadnodes.dev/rcp/forgejo-build/v1", "externalParameters": map[string]string{"tag": "main-abc", "source_sha": "abc"}, "internalParameters": map[string]string{"build_id": "build1"}},
				"runDetails":      map[string]any{"builder": map[string]string{"id": c.BuilderIDs[0]}, "byproducts": []any{map[string]any{"name": "required_status_contexts", "annotations": map[string]any{"required": []string{"ci"}, "observed": map[string]string{"ci": "success"}}}}},
			}}
			_, err := seedClient.PublishIdentity(context.Background(), f.RegistryConfig(), "deadnodes/app", "registry.json", f.Key("dev"), digest, "build1", "abc", "registry.deadnodes.internal/deadnodes/app", predicate)
			if err != nil {
				t.Fatal(err)
			}
			files := map[string]string{"clusters/prod/app.yaml": "image: " + ref}
			b := archive(t, files)
			cache := &fakeCache{rv: "7"}
			ts := now().Unix()
			if mode == "fresh" {
				raw, _ := json.Marshal(map[string]int64{digest: ts})
				cache.raw = string(raw)
			}
			if mode == "expired" {
				raw, _ := json.Marshal(map[string]int64{digest: ts - 7200})
				cache.raw = string(raw)
				_, err = seedClient.PublishIdentity(context.Background(), f.RegistryConfig(), c.SignaturesRepository, "registry.json", c.ProdKey, digest, "", "", "registry.deadnodes.internal/deadnodes/app", imagesigning.Predicate{Type: ApprovalType, Body: map[string]string{}})
				if err != nil {
					t.Fatal(err)
				}
			}
			if mode == "pruned" {
				files = map[string]string{}
				b = archive(t, files)
				raw, _ := json.Marshal(map[string]int64{"sha256:" + strings.Repeat("f", 64): ts})
				cache.raw = string(raw)
			}
			if mode == "unavailable" {
				cache.fail = true
			}
			if mode == "future" {
				raw, _ := json.Marshal(map[string]int64{digest: ts + 7200})
				cache.raw = string(raw)
			}
			count := &countingTransport{base: f.Client().Transport}
			client := imagesigning.NewWithClient(&http.Client{Transport: count})
			source := Source{HTTP: &http.Client{Transport: fakeSource{base: count, archive: b, digest: imagesigning.Hash(b), cache: cache}}, API: "http://kube.test", Token: "kube-token", Namespace: "supply-chain"}
			s, err := Reconcile(context.Background(), c, source, client, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "fresh":
				if s.Cached != 1 || s.AlreadySigned != 1 || s.Signed != 0 || count.prod.Load() != 0 {
					t.Fatalf("%+v prod=%d", s, count.prod.Load())
				}
			case "future":
				if s.Cached != 0 || count.prod.Load() == 0 {
					t.Fatalf("future entry trusted: %+v prod=%d", s, count.prod.Load())
				}
			case "unavailable":
				if s.Errors != 0 || len(cache.patch) != 0 {
					t.Fatalf("%+v patch=%s", s, cache.patch)
				}
			case "cold":
				var body struct {
					Data map[string]string `json:"data"`
				}
				if json.Unmarshal(cache.patch, &body) != nil || !strings.Contains(body.Data["verified.json"], digest) || s.Cached != 0 {
					t.Fatalf("%+v patch=%s", s, cache.patch)
				}
			case "expired":
				var body struct {
					Data map[string]string `json:"data"`
				}
				json.Unmarshal(cache.patch, &body)
				var saved map[string]int64
				json.Unmarshal([]byte(body.Data["verified.json"]), &saved)
				if saved[digest] <= ts-7200 || s.AlreadySigned != 1 {
					t.Fatalf("%+v saved=%v", s, saved)
				}
			case "pruned":
				var body struct {
					Data map[string]string `json:"data"`
				}
				json.Unmarshal(cache.patch, &body)
				if body.Data["verified.json"] != "{}" {
					t.Fatalf("patch=%s", cache.patch)
				}
			}
		})
	}
}

func TestApprovedSecurityScanByproduct(t *testing.T) {
	ref := &Ref{Digest: "sha256:" + strings.Repeat("a", 64)}
	base := map[string]any{"_type": "https://in-toto.io/Statement/v0.1", "predicateType": imagesigning.ProvenanceType, "subject": []any{map[string]any{"name": "x", "digest": map[string]string{"sha256": strings.Repeat("a", 64)}}}, "predicate": map[string]any{"buildDefinition": map[string]any{"buildType": "https://deadnodes.dev/rcp/forgejo-build/v1", "externalParameters": map[string]string{"tag": "main-abc", "source_sha": "abc"}, "internalParameters": map[string]string{"build_id": "b"}}, "runDetails": map[string]any{"builder": map[string]string{"id": "builder"}, "byproducts": []any{}}}}
	c := Config{BuilderIDs: []string{"builder"}}
	check := func(t *testing.T, require bool, byproducts []any, want bool) {
		t.Helper()
		c.RequireSecurityScan = require
		pred := base["predicate"].(map[string]any)
		run := pred["runDetails"].(map[string]any)
		contexts := map[string]any{"name": "required_status_contexts", "annotations": map[string]any{"required": []string{"ci"}, "observed": map[string]string{"ci": "success"}}}
		byproducts = append([]any{contexts}, byproducts...)
		run["byproducts"] = byproducts
		raw, _ := json.Marshal(base)
		_, _, ok := approved(raw, ref, c)
		if ok != want {
			t.Fatalf("approved=%v want %v", ok, want)
		}
	}
	check(t, false, nil, true)
	check(t, true, nil, false)
	pass := map[string]any{"name": "security_scan", "annotations": map[string]any{"verdict": "PASS", "blocking": 0, "gitleaks_findings": 0}}
	check(t, true, []any{pass}, true)
	fail := map[string]any{"name": "security_scan", "annotations": map[string]any{"verdict": "FAIL", "blocking": 0, "gitleaks_findings": 0}}
	check(t, true, []any{fail}, false)
}
