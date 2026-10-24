package registryobserver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"releasecontrol/internal/domain"
	"releasecontrol/internal/registryobserver/fixture"
	"releasecontrol/internal/workerconfig"
)

func TestImageContentCompleteWithHiddenUntaggedReferrerAndUnknownFallbackMetadata(t *testing.T) {
	f := fixture.New()
	f.Tags = map[string]string{}
	empty := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`)
	emptyDigest := fixture.Digest(empty)
	f.Manifests[emptyDigest] = empty
	var writes, hiddenReads atomic.Int64
	base := f.Handler()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			writes.Add(1)
		}
		if strings.Contains(r.URL.Path, "/referrers/") {
			w.WriteHeader(404)
			return
		}
		if strings.Contains(r.URL.Path, "/manifests/sha256-") {
			w.Header().Set("Content-Type", ociIndex)
			w.Header().Set("Docker-Content-Digest", emptyDigest)
			w.Write(empty)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/manifests/"+f.Referrer) {
			hiddenReads.Add(1)
		}
		base.ServeHTTP(w, r)
	}))
	defer server.Close()
	// Establish that the omitted referrer really exists at its digest and points
	// to the subject. The observer gets no synthetic discovery of that object.
	response, e := http.Get(server.URL + "/v2/" + f.Repository + "/manifests/" + f.Referrer)
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := io.ReadAll(response.Body)
	response.Body.Close()
	var hidden manifest
	json.Unmarshal(raw, &hidden)
	if response.StatusCode != 200 || fixture.Digest(raw) != f.Referrer || hidden.Subject == nil || hidden.Subject.Digest != f.Index {
		t.Fatal("hidden subject referrer fixture absent")
	}
	hiddenReads.Store(0)
	r := recipeFor(server.URL)
	in := inputsFor(r, []string{f.Index})
	report, e := Observe(context.Background(), r, in, time.Now().UTC())
	if e != nil {
		t.Fatal(e)
	}
	if _, e = domain.RegistryImageContentProofHash(report.Graph, r.Limits, in.Roots, f.Index); e != nil {
		t.Fatal("complete actual pull bytes rejected", e, report.Graph.Blockers)
	}
	if report.Graph.Discovery != "PARTIAL" || report.Graph.GlobalInventoryComplete || hiddenReads.Load() != 0 || writes.Load() != 0 {
		t.Fatal("hidden referrer changed inventory truth")
	}
	unknown := false
	for _, code := range report.Graph.Blockers {
		unknown = unknown || code == "registry_referrers_unknown"
	}
	if !unknown {
		t.Fatal("incoming referrer uncertainty lost")
	}
	for _, root := range report.Graph.Roots {
		if root.Closure == "COMPLETE" {
			t.Fatal("fallback became complete metadata")
		}
	}
}

func TestImageContentRequiredBytesAndLimitsFailClosedDespiteSameMetadata404(t *testing.T) {
	for _, mode := range []string{"complete", "missing-root", "missing-config", "missing-layer", "corrupt-config", "node-limit", "edge-limit", "depth-limit", "request-limit", "byte-limit", "auth", "redirect", "foreign-descriptor", "unsupported-manifest", "tag-drift", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			f := fixture.New()
			f.Tags = map[string]string{}
			var arm manifest
			json.Unmarshal(f.Manifests[f.ARM], &arm)
			root := f.Index
			if mode == "foreign-descriptor" || mode == "unsupported-manifest" {
				var changed map[string]any
				json.Unmarshal(f.Manifests[f.ARM], &changed)
				if mode == "foreign-descriptor" {
					changed["layers"].([]any)[0].(map[string]any)["urls"] = []string{"https://foreign.invalid/blob"}
				} else {
					changed["mediaType"] = "application/unsupported"
				}
				b, _ := json.Marshal(changed)
				root = fixture.Digest(b)
				f.Manifests[root] = b
			}
			var foreign atomic.Int64
			other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { foreign.Add(1); w.WriteHeader(200) }))
			defer other.Close()
			var tagCalls atomic.Int64
			base := f.Handler()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					t.Error("registry mutation")
				}
				if mode == "auth" {
					w.WriteHeader(401)
					return
				}
				if mode == "redirect" && strings.HasSuffix(r.URL.Path, "/manifests/"+root) {
					http.Redirect(w, r, other.URL+"/stolen", 302)
					return
				}
				if (mode == "missing-root" && strings.HasSuffix(r.URL.Path, "/manifests/"+root)) || (mode == "missing-config" && strings.HasSuffix(r.URL.Path, "/blobs/"+arm.Config.Digest)) || (mode == "missing-layer" && strings.HasSuffix(r.URL.Path, "/blobs/"+f.SharedBlob)) {
					w.WriteHeader(404)
					return
				}
				if mode == "corrupt-config" && strings.HasSuffix(r.URL.Path, "/blobs/"+arm.Config.Digest) {
					w.Write([]byte("corrupt"))
					return
				}
				if strings.Contains(r.URL.Path, "/referrers/") {
					w.WriteHeader(404)
					return
				}
				if mode == "tag-drift" && strings.HasSuffix(r.URL.Path, "/tags/list") && tagCalls.Add(1) > 1 {
					json.NewEncoder(w).Encode(map[string]any{"name": f.Repository, "tags": []string{"unexpected"}})
					return
				}
				base.ServeHTTP(w, r)
			}))
			defer server.Close()
			r := recipeFor(server.URL)
			switch mode {
			case "node-limit":
				r.Limits.MaxNodes = 3
			case "edge-limit":
				r.Limits.MaxEdges = 1
			case "depth-limit":
				r.Limits.MaxDepth = 1
			case "request-limit":
				r.Limits.MaxRequests = 2
			case "byte-limit":
				r.Limits.MaxTotalBytes = 1024
				r.Limits.MaxResponseBytes = 1024
			}
			r.Hash = workerconfig.ObservationHash(r)
			in := inputsFor(r, []string{root})
			ctx := context.Background()
			if mode == "cancel" {
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			}
			report, e := Observe(ctx, r, in, time.Now().UTC())
			if e != nil {
				if mode == "complete" {
					t.Fatal(e)
				}
				return
			}
			_, e = domain.RegistryImageContentProofHash(report.Graph, r.Limits, in.Roots, root)
			if (e == nil) != (mode == "complete") {
				t.Fatalf("mode=%s error=%v blockers=%v proofs=%+v", mode, e, report.Graph.Blockers, report.Graph.ImageContentRoots)
			}
			if foreign.Load() != 0 {
				t.Fatal("foreign URL contacted")
			}
			if report.Graph.Discovery != "PARTIAL" {
				t.Fatal("404 metadata upgraded")
			}
		})
	}
}
