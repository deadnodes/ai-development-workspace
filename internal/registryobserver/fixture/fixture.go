// Package fixture builds synthetic, non-executable OCI bytes for isolated tests.
// It is not imported by the observer CLI or its execution packages.
package fixture

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
)

const IndexMedia = "application/vnd.oci.image.index.v1+json"
const ManifestMedia = "application/vnd.oci.image.manifest.v1+json"

type Descriptor struct {
	MediaType string            `json:"mediaType"`
	Digest    string            `json:"digest"`
	Size      int64             `json:"size"`
	Platform  map[string]string `json:"platform,omitempty"`
}
type Data struct {
	Repository                                               string
	Manifests, Blobs                                         map[string][]byte
	Tags                                                     map[string]string
	Referrers                                                map[string][]Descriptor
	Roots                                                    []string
	Index, Nested, ARM, AMD, Referrer, SharedBlob, UntTagged string
}

func Digest(b []byte) string { s := sha256.Sum256(b); return "sha256:" + hex.EncodeToString(s[:]) }
func New() *Data {
	f := &Data{Repository: "test/component", Manifests: map[string][]byte{}, Blobs: map[string][]byte{}, Tags: map[string]string{}, Referrers: map[string][]Descriptor{}}
	blob := func(b []byte, media string) Descriptor {
		d := Digest(b)
		f.Blobs[d] = b
		return Descriptor{MediaType: media, Digest: d, Size: int64(len(b))}
	}
	manifest := func(v any, media string) Descriptor {
		b, _ := json.Marshal(v)
		d := Digest(b)
		f.Manifests[d] = b
		return Descriptor{MediaType: media, Digest: d, Size: int64(len(b))}
	}
	shared := blob([]byte("synthetic-shared-layer"), "application/vnd.oci.image.layer.v1.tar")
	f.SharedBlob = shared.Digest
	layer := blob([]byte("synthetic-second-layer"), "application/vnd.oci.image.layer.v1.tar+gzip")
	makeImage := func(arch string) Descriptor {
		config := blob([]byte(`{"os":"linux","architecture":"`+arch+`"}`), "application/vnd.oci.image.config.v1+json")
		return manifest(map[string]any{"schemaVersion": 2, "mediaType": ManifestMedia, "config": config, "layers": []Descriptor{shared, layer}}, ManifestMedia)
	}
	arm, amd := makeImage("arm64"), makeImage("amd64")
	f.ARM = arm.Digest
	f.AMD = amd.Digest
	arm.Platform = map[string]string{"os": "linux", "architecture": "arm64"}
	amd.Platform = map[string]string{"os": "linux", "architecture": "amd64"}
	nested := manifest(map[string]any{"schemaVersion": 2, "mediaType": IndexMedia, "manifests": []Descriptor{arm, amd}}, IndexMedia)
	f.Nested = nested.Digest
	index := manifest(map[string]any{"schemaVersion": 2, "mediaType": IndexMedia, "manifests": []Descriptor{nested}}, IndexMedia)
	f.Index = index.Digest
	empty := blob([]byte("{}"), "application/vnd.oci.empty.v1+json")
	payload := blob([]byte("synthetic-signature"), "application/vnd.example.signature")
	artifact := manifest(map[string]any{"schemaVersion": 2, "mediaType": ManifestMedia, "artifactType": "application/vnd.example.signature", "config": empty, "layers": []Descriptor{payload}, "subject": index}, ManifestMedia)
	f.Referrer = artifact.Digest
	f.Referrers[index.Digest] = []Descriptor{artifact}
	untagged := makeImage("riscv64")
	f.UntTagged = untagged.Digest
	f.Roots = []string{index.Digest, untagged.Digest}
	sort.Strings(f.Roots)
	f.Tags["latest"] = index.Digest
	return f
}
func (f *Data) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			w.WriteHeader(405)
			return
		}
		base := "/v2/" + f.Repository + "/"
		if !strings.HasPrefix(r.URL.Path, base) {
			w.WriteHeader(404)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, base)
		switch {
		case path == "tags/list":
			tags := []string{}
			for tag := range f.Tags {
				tags = append(tags, tag)
			}
			sort.Strings(tags)
			json.NewEncoder(w).Encode(map[string]any{"name": f.Repository, "tags": tags})
		case strings.HasPrefix(path, "manifests/"):
			ref := strings.TrimPrefix(path, "manifests/")
			d := ref
			if tag, ok := f.Tags[ref]; ok {
				d = tag
			}
			b, ok := f.Manifests[d]
			if !ok {
				w.WriteHeader(404)
				return
			}
			var m struct {
				MediaType string `json:"mediaType"`
			}
			json.Unmarshal(b, &m)
			w.Header().Set("Content-Type", m.MediaType)
			w.Header().Set("Docker-Content-Digest", d)
			w.Write(b)
		case strings.HasPrefix(path, "blobs/"):
			d := strings.TrimPrefix(path, "blobs/")
			b, ok := f.Blobs[d]
			if !ok {
				w.WriteHeader(404)
				return
			}
			w.Header().Set("Docker-Content-Digest", d)
			w.Write(b)
		case strings.HasPrefix(path, "referrers/"):
			d := strings.TrimPrefix(path, "referrers/")
			refs := f.Referrers[d]
			if refs == nil {
				refs = []Descriptor{}
			}
			w.Header().Set("Content-Type", IndexMedia)
			json.NewEncoder(w).Encode(map[string]any{"schemaVersion": 2, "mediaType": IndexMedia, "manifests": refs})
		default:
			w.WriteHeader(404)
		}
	})
}
