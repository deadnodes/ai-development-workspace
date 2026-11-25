package prodsigner

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"releasecontrol/internal/buildconfig"
	"releasecontrol/internal/imagesigning"
	"releasecontrol/internal/imagesigning/signingtest"
)

type promotionRT func(*http.Request) (*http.Response, error)

func (f promotionRT) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func jsonResponse(code int, v any) *http.Response {
	b, _ := json.Marshal(v)
	return &http.Response{StatusCode: code, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(string(b))), Request: nil}
}

type promotionHarness struct {
	mu        sync.Mutex
	promotion Promotion
	result    map[string]any
	cancel    context.CancelFunc
}

func (h *promotionHarness) roundTrip(r *http.Request) (*http.Response, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if r.URL.Host != "rcp.test" {
		return nil, io.EOF
	}
	if r.URL.Path == "/api/prod-promotions/claim" {
		return jsonResponse(200, h.promotion), nil
	}
	if strings.HasSuffix(r.URL.Path, "/result") {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &h.result)
		if h.cancel != nil {
			h.cancel()
		}
		return jsonResponse(200, map[string]any{"status": "FAILED"}), nil
	}
	return jsonResponse(404, nil), nil
}

type observeRegistryRT struct {
	base             http.RoundTripper
	mu               sync.Mutex
	tagGet           bool
	blobHeads        int
	sigManifestReads int
	failReadback     bool
}

func (o *observeRegistryRT) RoundTrip(r *http.Request) (*http.Response, error) {
	o.mu.Lock()
	if strings.Contains(r.URL.Path, "/manifests/main-") && r.Method == http.MethodGet {
		o.tagGet = true
	}
	if strings.Contains(r.URL.Path, "/blobs/") && r.Method == http.MethodHead {
		o.blobHeads++
	}
	if strings.Contains(r.URL.Path, "/v2/prod-signatures/manifests/") && r.Method == http.MethodGet {
		o.sigManifestReads++
		if o.failReadback && o.sigManifestReads >= 2 {
			o.mu.Unlock()
			return jsonResponse(503, map[string]string{"error": "readback failed"}), nil
		}
	}
	o.mu.Unlock()
	return o.base.RoundTrip(r)
}

func promoteFixture(t *testing.T) (Config, *signingtest.Fixture, *imagesigning.Client, Promotion, PromotionItem, *observeRegistryRT) {
	t.Helper()
	f := signingtest.New(t)
	sha := strings.Repeat("a", 40)
	digest := f.Image(sha)
	f.Manifests["main-aaaaaaaa"] = f.Manifests[digest]
	cfg := Config{}
	cfg.RCP.URL = "https://rcp.test"
	cfg.RCP.TokenFile = filepath.Join(f.Root, "rcp-token")
	cfg.RCP.ProductID = "p"
	cfg.RCP.EnvironmentID = "prod"
	if e := os.WriteFile(cfg.RCP.TokenFile, []byte("projected-token"), 0600); e != nil {
		t.Fatal(e)
	}
	cfg.OwnImagePrefixes = []string{"registry.deadnodes.internal/deadnodes/"}
	cfg.BuilderIDs = []string{"https://builder.test/v1"}
	cfg.Registry.Origin = f.Registry.URL
	cfg.Registry.InsecureHTTP = true
	cfg.Registry.CredentialsFile = filepath.Join(f.Root, "registry.json")
	cfg.Registry.Rewrite = map[string]string{"192.168.32.242:5000/": "", "registry.deadnodes.internal/": ""}
	cfg.SignaturesRepository = "prod-signatures"
	cfg.DevPublicKeyPEM = f.Key("dev").PublicKeyPEM
	cfg.ProdKey = f.Key("prod")
	item := PromotionItem{BuildID: "build1", ImageID: "app", Repository: "192.168.32.242:5000/deadnodes/app", DevImageRef: "192.168.32.242:5000/deadnodes/app:main-aaaaaaaa@" + digest, Digest: digest, ProdImageRef: "registry.deadnodes.internal/deadnodes/app:main-aaaaaaaa@" + digest, Tag: "main-aaaaaaaa", SourceSHA: sha, Result: "PENDING"}
	p := Promotion{ID: "promotion-1", ProductID: "p", EnvironmentID: "prod", Status: "CLAIMED", Items: []PromotionItem{item}, OwnerApproval: "yes", ClaimID: "claim-1"}
	pred := imagesigning.Predicate{Type: imagesigning.ProvenanceType, Body: map[string]any{"buildDefinition": map[string]any{"buildType": "https://deadnodes.dev/rcp/forgejo-build/v1", "externalParameters": map[string]string{"tag": item.Tag, "source_sha": sha}, "internalParameters": map[string]string{"build_id": item.BuildID}}, "runDetails": map[string]any{"builder": map[string]string{"id": cfg.BuilderIDs[0]}, "byproducts": []any{map[string]any{"name": "required_status_contexts", "annotations": map[string]any{"required": []string{"ci"}, "observed": map[string]string{"ci": "success"}}}}}}}
	if _, e := imagesigning.NewWithClient(f.Client()).PublishIdentity(context.Background(), f.RegistryConfig(), "deadnodes/app", "registry.json", f.Key("dev"), digest, item.BuildID, sha, item.Repository, pred); e != nil {
		t.Fatal(e)
	}
	o := &observeRegistryRT{base: f.Client().Transport}
	regClient := imagesigning.NewWithClient(&http.Client{Transport: o})
	return cfg, f, regClient, p, item, o
}

func TestPromoteLoopWarmsSignsReadsBackAndReports(t *testing.T) {
	c, f, client, p, _, obs := promoteFixture(t)
	if e := client.Warm(context.Background(), buildconfig.Registry{Origin: c.Registry.Origin, InsecureHTTP: true}, "deadnodes/app", "registry.json", p.Items[0].Digest, p.Items[0].Tag); e != nil {
		t.Fatalf("warm: %v", e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := &promotionHarness{promotion: p, cancel: cancel}
	var log strings.Builder
	e := promoteWithHTTPClient(ctx, c, client, &log, &http.Client{Transport: promotionRT(h.roundTrip)})
	if e != context.Canceled {
		t.Fatalf("loop exit %v", e)
	}
	if h.result == nil {
		t.Fatal("no result posted")
	}
	items, ok := h.result["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("result %#v", h.result)
	}
	raw, _ := json.Marshal(items[0])
	var got PromotionItem
	if json.Unmarshal(raw, &got) != nil || got.Result != "SIGNED" || got.ProdSignature == nil {
		t.Fatalf("bad result %s", raw)
	}
	if !obs.tagGet || obs.blobHeads == 0 {
		t.Fatalf("image not warmed tag=%v heads=%d", obs.tagGet, obs.blobHeads)
	}
	if f.Signs == 0 || !strings.Contains(log.String(), `"promotion_id":"promotion-1"`) {
		t.Fatalf("did not sign/log %s signs=%d", log.String(), f.Signs)
	}
	if strings.Contains(log.String(), "payload_b64") || strings.Contains(log.String(), "signature_b64") {
		t.Fatal("logged signature material")
	}
}

func TestPromotionItemAlreadySignedAndRejectEvidence(t *testing.T) {
	t.Run("already signed", func(t *testing.T) {
		c, f, client, p, item, _ := promoteFixture(t)
		identity, ok := imagesigning.RepositoryIdentity(item.ProdImageRef)
		if !ok {
			t.Fatal("invalid production image ref")
		}
		if _, e := client.PublishIdentity(context.Background(), buildconfig.Registry{Origin: c.Registry.Origin, InsecureHTTP: true}, c.SignaturesRepository, "registry.json", c.ProdKey, item.Digest, item.BuildID, item.SourceSHA, identity, imagesigning.Predicate{Type: ApprovalType, Body: map[string]string{}}); e != nil {
			t.Fatal(e)
		}
		got := processPromotionItem(context.Background(), c, client, buildconfig.Registry{Origin: c.Registry.Origin, InsecureHTTP: true}, "registry.json", p, item)
		if got.Result != "ALREADY_SIGNED" || got.ProdSignature == nil {
			t.Fatal(got)
		}
		_ = f
	})
	t.Run("CI rejected", func(t *testing.T) {
		c, f, client, p, item, _ := promoteFixture(t)
		f.Manifests["main-aaaaaaaa"] = f.Manifests[item.Digest] // image warm succeeds
		// Replace verified dev provenance with evidence whose required CI context failed.
		bad := imagesigning.Predicate{Type: imagesigning.ProvenanceType, Body: map[string]any{"buildDefinition": map[string]any{"buildType": "https://deadnodes.dev/rcp/forgejo-build/v1", "externalParameters": map[string]string{"tag": item.Tag, "source_sha": item.SourceSHA}, "internalParameters": map[string]string{"build_id": item.BuildID}}, "runDetails": map[string]any{"builder": map[string]string{"id": c.BuilderIDs[0]}, "byproducts": []any{map[string]any{"name": "required_status_contexts", "annotations": map[string]any{"required": []string{"ci"}, "observed": map[string]string{"ci": "failure"}}}}}}}
		_, e := client.PublishIdentity(context.Background(), buildconfig.Registry{Origin: c.Registry.Origin, InsecureHTTP: true}, "deadnodes/app", "registry.json", f.Key("dev"), item.Digest, item.BuildID, item.SourceSHA, item.Repository, bad)
		if e != nil {
			t.Fatal(e)
		}
		keepLastAttestation(t, f, item.Digest)
		got := processPromotionItem(context.Background(), c, client, buildconfig.Registry{Origin: c.Registry.Origin, InsecureHTTP: true}, "registry.json", p, item)
		if got.Result != "REJECTED" || got.Reason != "CI_NOT_GREEN" {
			t.Fatal(got)
		}
	})
	t.Run("revision mismatch", func(t *testing.T) {
		c, f, client, p, item, _ := promoteFixture(t)
		wrong := f.Image("b" + strings.Repeat("a", 39))
		pred := imagesigning.Predicate{Type: imagesigning.ProvenanceType, Body: map[string]any{"buildDefinition": map[string]any{"buildType": "https://deadnodes.dev/rcp/forgejo-build/v1", "externalParameters": map[string]string{"tag": item.Tag, "source_sha": item.SourceSHA}, "internalParameters": map[string]string{"build_id": item.BuildID}}, "runDetails": map[string]any{"builder": map[string]string{"id": c.BuilderIDs[0]}, "byproducts": []any{map[string]any{"name": "required_status_contexts", "annotations": map[string]any{"required": []string{"ci"}, "observed": map[string]string{"ci": "success"}}}}}}}
		if _, e := client.PublishIdentity(context.Background(), buildconfig.Registry{Origin: c.Registry.Origin, InsecureHTTP: true}, "deadnodes/app", "registry.json", f.Key("dev"), wrong, item.BuildID, item.SourceSHA, item.Repository, pred); e != nil {
			t.Fatal(e)
		}
		f.Manifests["main-aaaaaaaa"] = f.Manifests[wrong]
		item.Digest = wrong
		item.DevImageRef = strings.SplitN(item.DevImageRef, "@", 2)[0] + "@" + wrong
		item.ProdImageRef = strings.SplitN(item.ProdImageRef, "@", 2)[0] + "@" + wrong
		got := processPromotionItem(context.Background(), c, client, buildconfig.Registry{Origin: c.Registry.Origin, InsecureHTTP: true}, "registry.json", p, item)
		if got.Result != "REJECTED" || got.Reason != "REVISION_MISMATCH" {
			t.Fatal(got)
		}
	})
	t.Run("readback failure", func(t *testing.T) {
		c, _, client, p, item, obs := promoteFixture(t)
		obs.failReadback = true
		got := processPromotionItem(context.Background(), c, client, buildconfig.Registry{Origin: c.Registry.Origin, InsecureHTTP: true}, "registry.json", p, item)
		if got.Result != "ERROR" || got.Reason != "INFRASTRUCTURE_ERROR" {
			t.Fatal(got)
		}
	})
}

func keepLastAttestation(t *testing.T, f *signingtest.Fixture, digest string) {
	t.Helper()
	tag := imagesigning.Tag(digest, "att")
	var m imagesigning.Manifest
	if json.Unmarshal(f.Manifests[tag], &m) != nil || len(m.Layers) < 1 {
		t.Fatal("attestation manifest missing")
	}
	m.Layers = m.Layers[len(m.Layers)-1:]
	b, e := json.Marshal(m)
	if e != nil {
		t.Fatal(e)
	}
	f.Manifests[tag] = b
}
