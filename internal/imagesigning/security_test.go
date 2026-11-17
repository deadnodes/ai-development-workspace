package imagesigning_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"releasecontrol/internal/buildconfig"
	s "releasecontrol/internal/imagesigning"
	"strings"
	"testing"
)

func TestSecurityEvidenceReferrers(t *testing.T) {
	subject := "sha256:" + strings.Repeat("a", 64)
	sbomManifestDigest := "sha256:" + strings.Repeat("b", 64)
	goodSummary := func(digest, sha string) []byte {
		return []byte(fmt.Sprintf(`{"schema":"deadnodes.security-gate/v1","verdict":"PASS","digest":%q,"sha":%q,"created_at":"2026-11-18T10:00:00Z","gitleaks":{"findings":0},"trivy":{"blocking":0},"sbom":{"digest":%q}}`, digest, sha, sbomManifestDigest))
	}
	sha := strings.Repeat("c", 40)
	for _, tc := range []struct {
		name string
		mode string
		want string
		err  bool
	}{
		{"happy path", "ok", "PASS", false},
		{"no referrers", "none", "", true},
		{"newest created", "newest", "PASS", false},
		{"summary digest mismatch", "digest-mismatch", "ERROR", false},
		{"SBOM referrer missing", "sbom-missing", "ERROR", false},
		{"invalid schema", "invalid-schema", "ERROR", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			summaryDigest := "sha256:" + strings.Repeat("d", 64)
			oldDigest := "sha256:" + strings.Repeat("e", 64)
			summary := goodSummary(subject, sha)
			if tc.mode == "digest-mismatch" {
				summary = goodSummary("sha256:"+strings.Repeat("f", 64), sha)
			}
			if tc.mode == "invalid-schema" {
				summary = []byte(`{"schema":"wrong","verdict":"PASS"}`)
			}
			created := "2026-11-18T10:00:00Z"
			if tc.mode == "newest" {
				created = "2026-11-18T11:00:00Z"
			}
			summaryManifest, _ := json.Marshal(s.Manifest{SchemaVersion: 2, MediaType: s.ManifestType, ArtifactType: s.SecurityArtifactType, Subject: &s.Descriptor{Digest: subject}, Layers: []s.Descriptor{{MediaType: s.SecurityArtifactType, Digest: s.Hash(summary), Size: len(summary)}}})
			sbom := []byte(`{"bomFormat":"CycloneDX"}`)
			sbomManifest, _ := json.Marshal(s.Manifest{SchemaVersion: 2, MediaType: s.ManifestType, ArtifactType: s.SBOMArtifactType, Subject: &s.Descriptor{Digest: subject}, Layers: []s.Descriptor{{MediaType: s.SBOMArtifactType, Digest: s.Hash(sbom), Size: len(sbom)}}})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.Contains(r.URL.Path, "/referrers/") && r.URL.Query().Get("artifactType") == s.SecurityArtifactType:
					if tc.mode == "none" {
						_, _ = w.Write([]byte(`{"manifests":[]}`))
						return
					}
					manifests := []map[string]any{{"artifactType": s.SecurityArtifactType, "digest": summaryDigest, "annotations": map[string]string{"org.opencontainers.image.created": created}}}
					if tc.mode == "newest" {
						manifests = append(manifests, map[string]any{"artifactType": s.SecurityArtifactType, "digest": oldDigest, "annotations": map[string]string{"org.opencontainers.image.created": "2026-11-17T10:00:00Z"}})
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"manifests": manifests})
				case strings.Contains(r.URL.Path, "/referrers/"):
					if tc.mode == "sbom-missing" {
						_, _ = w.Write([]byte(`{"manifests":[]}`))
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"manifests": []map[string]any{{"artifactType": s.SBOMArtifactType, "digest": sbomManifestDigest}}})
				case strings.HasSuffix(r.URL.Path, "/manifests/"+summaryDigest):
					_, _ = w.Write(summaryManifest)
				case strings.HasSuffix(r.URL.Path, "/manifests/"+oldDigest):
					_, _ = w.Write(summaryManifest)
				case strings.HasSuffix(r.URL.Path, "/manifests/"+sbomManifestDigest):
					_, _ = w.Write(sbomManifest)
				case strings.HasSuffix(r.URL.Path, "/blobs/"+s.Hash(summary)):
					_, _ = w.Write(summary)
				case strings.HasSuffix(r.URL.Path, "/blobs/"+s.Hash(sbom)):
					_, _ = w.Write(sbom)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			cfg := buildconfig.Registry{ID: "test", Origin: server.URL, InsecureHTTP: true}
			got, err := s.New().SecurityEvidence(context.Background(), cfg, "o/r", "", subject)
			if tc.err {
				if err != s.ErrEvidenceNotFound {
					t.Fatalf("error=%v", err)
				}
				return
			}
			if err != nil || got.Verdict != tc.want {
				t.Fatalf("evidence=%+v error=%v", got, err)
			}
			if tc.mode == "newest" && got.SummaryDigest != summaryDigest {
				t.Fatalf("selected %s, want newest %s", got.SummaryDigest, summaryDigest)
			}
		})
	}
}
