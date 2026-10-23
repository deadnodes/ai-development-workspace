package github

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"releasecontrol/internal/delivery"
)

func TestReadGitOpsAtCommitUsesImmutableEndpointAndCachedAuthentication(t *testing.T) {
	for _, mode := range []string{"deleted-branch", "cold", "expired", "wrong-commit", "missing-commit", "invalid-sha", "conflicting-head", "oversized", "ambiguous-image"} {
		t.Run(mode, func(t *testing.T) {
			sha := strings.Repeat("a", 40)
			digest := "sha256:" + strings.Repeat("b", 64)
			content := "image:\n  repository: registry.example/team/dev\n  digest: " + digest + "\n"
			if mode == "oversized" {
				content += strings.Repeat("#", 1<<20)
			}
			if mode == "ambiguous-image" {
				content += "---\nimage:\n  repository: registry.example/team/other\n  digest: " + digest + "\n"
			}
			var paths []string
			writes := 0
			p, c, tokens := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.URL.RequestURI())
				if r.Method != http.MethodGet {
					writes++
					http.Error(w, "mutation forbidden", 405)
					return
				}
				switch r.URL.Path {
				case "/repositories/101":
					jsonOut(w, map[string]any{"id": 101, "full_name": "acme/gitops"})
				case "/repos/acme/gitops/git/commits/" + sha:
					if mode == "missing-commit" {
						http.NotFound(w, r)
						return
					}
					returned := sha
					if mode == "wrong-commit" {
						returned = strings.Repeat("c", 40)
					}
					jsonOut(w, map[string]string{"sha": returned})
				case "/repos/acme/gitops/contents/dev.yaml":
					if r.URL.Query().Get("ref") != sha {
						t.Error("contents read not pinned to exact immutable SHA")
					}
					jsonOut(w, map[string]string{"type": "file", "sha": gitOpsBlobSHA([]byte(content)), "encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(content))})
				default:
					t.Errorf("unexpected endpoint (branch is deleted): %s", r.URL.RequestURI())
					http.NotFound(w, r)
				}
			})
			if mode != "cold" {
				base, _ := apiBase(c)
				key := fmt.Sprintf("%s|%d|%d|%s", base, c.AppID, c.InstallationID, c.PrivateKeyRef)
				expires := time.Now().Add(time.Hour)
				if mode == "expired" {
					expires = time.Now().Add(-time.Hour)
				}
				p.tokens[key] = cachedToken{"installation-secret", expires}
			}
			r := delivery.GitOpsRequest{Repository: "101", Ref: "deleted-proposal", Path: "dev.yaml", ExpectedHeadSHA: sha}
			commit := sha
			if mode == "invalid-sha" {
				commit = "main"
			}
			if mode == "conflicting-head" {
				r.ExpectedHeadSHA = strings.Repeat("d", 40)
			}
			got, err := p.ReadGitOpsAtCommit(context.Background(), c, r, commit)
			if (err == nil) != (mode == "deleted-branch") {
				t.Fatalf("mode=%s result=%+v err=%v", mode, got, err)
			}
			if mode == "deleted-branch" {
				want := []string{"/repositories/101", "/repos/acme/gitops/git/commits/" + sha, "/repos/acme/gitops/contents/dev.yaml?ref=" + sha}
				if !reflect.DeepEqual(paths, want) || got.HeadSHA != sha || got.ImageRepository != "registry.example/team/dev" || got.Digest != digest || got.Content != content {
					t.Fatalf("immutable observation changed paths=%v result=%+v", paths, got)
				}
			}
			if (mode == "cold" || mode == "expired" || mode == "invalid-sha" || mode == "conflicting-head") && len(paths) != 0 {
				t.Fatal("invalid/cold authority performed IO", paths)
			}
			if (mode == "wrong-commit" || mode == "missing-commit") && len(paths) != 2 {
				t.Fatal("unverified commit allowed content read", paths)
			}
			if writes != 0 || tokens.Load() != 0 {
				t.Fatal("immutable read refreshed authentication or wrote", writes, tokens.Load())
			}
		})
	}
}
