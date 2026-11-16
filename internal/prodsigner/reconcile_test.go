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
	"testing"
)

type fakeSource struct {
	base    http.RoundTripper
	archive []byte
	digest  string
}

func (f fakeSource) RoundTrip(r *http.Request) (*http.Response, error) {
	w := httptest.NewRecorder()
	switch r.URL.Host {
	case "kube.test":
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
