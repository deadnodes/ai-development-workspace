package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"releasecontrol/internal/delivery"
)

func TestGitOpsProposalLookupExactReadOnlyRecovery(t *testing.T) {
	for _, mutation := range []string{"valid", "default-message", "missing-auth", "expired-auth", "numeric-repository-required", "repository-id", "head-repository", "base-repository", "foreign-name", "ambiguous-pr", "no-pr", "paginated-pr", "truncated-tree", "missing-truncation", "duplicate-entry", "oversized-tree", "extra-file", "unrelated-edit", "mode-change", "target-mode-change", "target-extra-text", "marker", "parents", "base-identity", "base-blob", "blob-content-hash", "blob-size", "missing-blob-size", "branch-only", "moved-branch", "moved-pr", "missing-branch", "provider-error", "bad-ref", "bad-digest", "bad-blob", "bad-head", "bad-operation", "deep-path", "conflicting-base-pin", "same-branch", "private-domain", "private-loopback", "private-ipv6", "bare-ghcr", "private-wrong-host", "private-wrong-repo", "private-wrong-digest"} {
		t.Run(mutation, func(t *testing.T) {
			base, head := strings.Repeat("a", 40), strings.Repeat("b", 40)
			baseTree, headTree := strings.Repeat("c", 40), strings.Repeat("d", 40)
			baseSub, headSub := strings.Repeat("e", 40), strings.Repeat("f", 40)
			digest := "sha256:" + strings.Repeat("1", 64)
			op := "abcdef0123456789"
			branch := "rcp/gitops/" + op
			image := "ghcr.io/acme/image"
			switch mutation {
			case "private-domain", "private-wrong-host", "private-wrong-repo", "private-wrong-digest":
				image = "registry.example:5443/team/deep/image"
			case "private-loopback":
				image = "127.0.0.1:5000/team/image"
			case "private-ipv6":
				image = "[::1]:5000/team/image"
			}
			before := "# retained comment\nimage:\n  repository: ghcr.io/acme/image\n  digest: sha256:" + strings.Repeat("2", 64) + "\nextra: retained\n"
			// The production writer quotes both scalar tokens and preserves every
			// other byte. Spell that expected output independently in the fixture.
			after := "# retained comment\nimage:\n  repository: \"ghcr.io/acme/image\"\n  digest: \"" + digest + "\"\nextra: retained\n"
			if mutation == "target-extra-text" {
				after += "injected: true\n"
			}
			before = strings.ReplaceAll(before, "ghcr.io/acme/image", image)
			after = strings.ReplaceAll(after, "ghcr.io/acme/image", image)
			if mutation == "private-ipv6" {
				before = strings.ReplaceAll(before, "repository: "+image, "repository: '"+image+"'")
			}
			if mutation == "private-wrong-host" {
				after = strings.ReplaceAll(after, "registry.example", "other.example")
			}
			if mutation == "private-wrong-repo" {
				after = strings.ReplaceAll(after, "team/deep/image", "team/deep/other")
			}
			if mutation == "private-wrong-digest" {
				after = strings.ReplaceAll(after, digest, "sha256:"+strings.Repeat("9", 64))
			}
			oldBlob, newBlob := gitOpsBlobSHA([]byte(before)), gitOpsBlobSHA([]byte(after))
			req := delivery.GitOpsApply{Request: delivery.GitOpsRequest{Repository: "101", Ref: "main", Path: "apps/app.yaml", ImageRepository: image}, ExpectedHeadSHA: base, ExpectedBlobSHA: oldBlob, Digest: digest, OperationID: op, Message: "release"}
			if mutation == "bare-ghcr" {
				req.Request.ImageRepository = "acme/image"
			}
			if mutation == "default-message" {
				req.Message = ""
			}
			writes, reads, refReads := 0, 0, 0
			paths := []string{}
			makePR := func() map[string]any {
				headID, baseID := 101, 101
				name := "acme/source"
				if mutation == "head-repository" {
					headID = 202
				}
				if mutation == "base-repository" {
					baseID = 202
				}
				if mutation == "foreign-name" {
					name = "acme/foreign"
				}
				prHead := head
				return map[string]any{"number": 7, "html_url": "https://github.test/acme/source/pull/7", "state": "open", "head": map[string]any{"ref": branch, "sha": prHead, "repo": map[string]any{"id": headID, "full_name": name}}, "base": map[string]any{"ref": "main", "repo": map[string]any{"id": baseID, "full_name": "acme/source"}}}
			}
			p, c, tokens := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					writes++
					http.Error(w, "write forbidden", 405)
					return
				}
				reads++
				paths = append(paths, r.URL.Path)
				if mutation == "provider-error" {
					http.Error(w, "failed", 503)
					return
				}
				var response any
				switch r.URL.Path {
				case "/repositories/101":
					id := 101
					if mutation == "repository-id" {
						id = 202
					}
					response = map[string]any{"id": id, "full_name": "acme/source"}
				case "/repos/acme/source/git/ref/heads/" + branch:
					refReads++
					if mutation == "missing-branch" {
						http.NotFound(w, r)
						return
					}
					sha := head
					if mutation == "branch-only" || (mutation == "moved-branch" && refReads > 1) {
						sha = base
					}
					response = map[string]any{"ref": "refs/heads/" + branch, "object": map[string]any{"type": "commit", "sha": sha}}
				case "/repos/acme/source/git/commits/" + base:
					sha := base
					if mutation == "base-identity" {
						sha = head
					}
					response = map[string]any{"sha": sha, "tree": map[string]any{"sha": baseTree}}
				case "/repos/acme/source/git/commits/" + head:
					message := req.Message
					if message == "" {
						message = "Release Control Plane image update"
					}
					message += " [rcp:" + op + "]"
					if mutation == "marker" {
						message = "other"
					}
					parents := []map[string]string{{"sha": base}}
					if mutation == "parents" {
						parents = append(parents, map[string]string{"sha": head})
					}
					response = map[string]any{"sha": head, "message": message, "parents": parents, "tree": map[string]string{"sha": headTree}}
				case "/repos/acme/source/git/trees/" + baseTree, "/repos/acme/source/git/trees/" + headTree:
					sha, sub := baseTree, baseSub
					if strings.HasSuffix(r.URL.Path, headTree) {
						sha, sub = headTree, headSub
					}
					siblingSHA, siblingMode := strings.Repeat("3", 40), "100644"
					if sha == headTree && mutation == "unrelated-edit" {
						siblingSHA = strings.Repeat("4", 40)
					}
					if sha == headTree && mutation == "mode-change" {
						siblingMode = "100755"
					}
					entries := []map[string]string{{"path": "apps", "type": "tree", "mode": "040000", "sha": sub}, {"path": "README.md", "type": "blob", "mode": siblingMode, "sha": siblingSHA}}
					if sha == headTree && mutation == "extra-file" {
						entries = append(entries, map[string]string{"path": "extra", "type": "blob", "mode": "100644", "sha": siblingSHA})
					}
					if mutation == "duplicate-entry" {
						entries = append(entries, entries[0])
					}
					if mutation == "oversized-tree" {
						for len(entries) < 2049 {
							entries = append(entries, map[string]string{"path": fmt.Sprintf("extra%d", len(entries)), "type": "blob", "mode": "100644", "sha": siblingSHA})
						}
					}
					v := map[string]any{"sha": sha, "truncated": mutation == "truncated-tree", "tree": entries}
					if mutation == "missing-truncation" {
						delete(v, "truncated")
					}
					response = v
				case "/repos/acme/source/git/trees/" + baseSub, "/repos/acme/source/git/trees/" + headSub:
					sha, blob, mode := baseSub, oldBlob, "100644"
					if strings.HasSuffix(r.URL.Path, headSub) {
						sha, blob = headSub, newBlob
						if mutation == "target-mode-change" {
							mode = "100755"
						}
					}
					response = map[string]any{"sha": sha, "truncated": false, "tree": []map[string]string{{"path": "app.yaml", "mode": mode, "type": "blob", "sha": blob}}}
				case "/repos/acme/source/git/blobs/" + oldBlob, "/repos/acme/source/git/blobs/" + newBlob:
					sha, content := oldBlob, before
					if strings.HasSuffix(r.URL.Path, newBlob) {
						sha, content = newBlob, after
					}
					size := len(content)
					if mutation == "blob-content-hash" {
						content += "tampered"
						size = len(content)
					}
					if mutation == "blob-size" {
						size++
					}
					v := map[string]any{"sha": sha, "size": size, "encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(content))}
					if mutation == "missing-blob-size" {
						delete(v, "size")
					}
					response = v
				case "/repos/acme/source/pulls":
					if r.URL.Query().Get("head") != "acme:"+branch || r.URL.Query().Get("base") != "main" || r.URL.Query().Get("per_page") != "2" {
						t.Error("lookup scope missing")
					}
					prs := []map[string]any{makePR()}
					if mutation == "ambiguous-pr" {
						prs = append(prs, makePR())
					}
					if mutation == "no-pr" {
						prs = nil
					}
					if mutation == "paginated-pr" {
						w.Header().Set("Link", "<https://github.test/next>; rel=\"next\"")
					}
					response = prs
				case "/repos/acme/source/pulls/7":
					pr := makePR()
					if mutation == "moved-pr" {
						pr["head"].(map[string]any)["sha"] = base
					}
					response = pr
				default:
					t.Errorf("unexpected GET %s", r.URL.Path)
					http.NotFound(w, r)
					return
				}
				json.NewEncoder(w).Encode(response)
			})
			baseURL, err := apiBase(c)
			if err != nil {
				t.Fatal(err)
			}
			key := fmt.Sprintf("%s|%d|%d|%s", baseURL, c.AppID, c.InstallationID, c.PrivateKeyRef)
			if mutation != "missing-auth" {
				expiry := time.Now().Add(time.Hour)
				if mutation == "expired-auth" {
					expiry = time.Now().Add(-time.Hour)
				}
				p.tokens[key] = cachedToken{"installation-secret", expiry}
			}
			switch mutation {
			case "numeric-repository-required":
				req.Request.Repository = "acme/source"
			case "base-blob":
				req.ExpectedBlobSHA = strings.Repeat("5", 40)
			case "bad-ref":
				req.Request.Ref = "../main"
			case "bad-digest":
				req.Digest = "latest"
			case "bad-blob":
				req.ExpectedBlobSHA = "bad"
			case "bad-head":
				req.ExpectedHeadSHA = "bad"
			case "bad-operation":
				req.OperationID = "../../bad"
			case "deep-path":
				req.Request.Path = strings.Repeat("apps/", 17) + "app.yaml"
			case "conflicting-base-pin":
				req.Request.ExpectedHeadSHA = head
			case "same-branch":
				req.Request.Ref = branch
			}
			result, err := p.LookupGitOpsProposal(context.Background(), c, req)
			if mutation == "valid" || mutation == "default-message" || mutation == "private-domain" || mutation == "private-loopback" || mutation == "private-ipv6" || mutation == "bare-ghcr" {
				if err != nil || result.Number != 7 || result.HeadSHA != head || result.State != "open" {
					t.Fatalf("exact recovery: %+v %v requests=%v", result, err, paths)
				}
			} else if err == nil || result.Number != 0 {
				t.Fatalf("incomplete or foreign evidence admitted: %+v %v", result, err)
			}
			if writes != 0 || tokens.Load() != 0 {
				t.Fatalf("lookup made mutation/auth writes: %d %d", writes, tokens.Load())
			}
			if reads > 48 {
				t.Fatalf("unbounded requests: %d", reads)
			}
		})
	}
}
