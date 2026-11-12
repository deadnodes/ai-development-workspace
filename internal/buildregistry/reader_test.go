package buildregistry

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"releasecontrol/internal/buildconfig"
	"strings"
	"testing"
)

func TestRegistryProvenance(t *testing.T) {
	for _, index := range []bool{false, true} {
		t.Run(fmt.Sprint(index), func(t *testing.T) {
			sha := strings.Repeat("b", 40)
			cfg := []byte(fmt.Sprintf(`{"os":"linux","architecture":"amd64","config":{"Labels":{"org.opencontainers.image.revision":%q}}}`, sha))
			manifest := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":%q,"size":%d}}`, hash(cfg), len(cfg)))
			top := manifest
			if index {
				top = []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"digest":%q,"size":%d,"platform":{"os":"linux","architecture":"amd64"}},{"digest":%q,"platform":{"os":"unknown","architecture":"unknown"}}]}`, hash(manifest), len(manifest), "sha256:"+strings.Repeat("c", 64)))
			}
			objects := map[string][]byte{"/v2/o/r/manifests/tag": top, "/v2/o/r/manifests/" + hash(manifest): manifest, "/v2/o/r/blobs/" + hash(cfg): cfg}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, ok := objects[r.URL.Path]
				if !ok {
					w.WriteHeader(404)
					return
				}
				w.Header().Set("Docker-Content-Digest", hash(b))
				w.Write(b)
			}))
			defer srv.Close()
			reg := buildconfig.Registry{Origin: srv.URL, InsecureHTTP: true}
			v, e := New().Resolve(context.Background(), reg, "o/r", "tag")
			if e != nil || v.Digest != hash(top) || v.Revision != sha {
				t.Fatal(v, e)
			}
			reg.InsecureHTTP = false
			if _, e = New().Resolve(context.Background(), reg, "o/r", "tag"); e == nil {
				t.Fatal("implicit HTTP allowed")
			}
		})
	}
}
func TestRegistryRejectsCorruption(t *testing.T) {
	for _, mode := range []string{"header", "config", "missing", "size", "platform", "oversize"} {
		t.Run(mode, func(t *testing.T) {
			cfg := []byte(`{"os":"linux","architecture":"amd64","config":{"Labels":{}}}`)
			d := hash(cfg)
			if mode == "config" {
				d = "sha256:" + strings.Repeat("a", 64)
			}
			size := len(cfg)
			if mode == "size" {
				size++
			}
			if mode == "platform" {
				cfg = []byte(`{"os":"unknown","architecture":"unknown"}`)
				d = hash(cfg)
				size = len(cfg)
			}
			m, _ := json.Marshal(map[string]any{"schemaVersion": 2, "config": map[string]any{"digest": d, "size": size}})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "missing" {
					w.WriteHeader(404)
					return
				}
				b := m
				if strings.Contains(r.URL.Path, "blobs") {
					b = cfg
				}
				if mode == "oversize" {
					b = []byte(strings.Repeat("x", (4<<20)+1))
				}
				header := hash(b)
				if mode == "header" {
					header = "sha256:" + strings.Repeat("a", 64)
				}
				w.Header().Set("Docker-Content-Digest", header)
				w.Write(b)
			}))
			defer srv.Close()
			_, e := New().Resolve(context.Background(), buildconfig.Registry{Origin: srv.URL, InsecureHTTP: true}, "o/r", "tag")
			if e == nil {
				t.Fatal("corruption accepted")
			}
		})
	}
}
