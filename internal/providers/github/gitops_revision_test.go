package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"releasecontrol/internal/delivery"
)

func TestGitOpsRevisionBoundedGETOnly(t *testing.T) {
	for _, mode := range []string{"valid", "pinned", "same", "two-ancestors", "max-ancestors", "aggregate-budget", "missing-auth", "expired-auth", "numeric", "leading-zero", "owner", "repository-id", "repository-name", "repository-rebind", "ref-name", "ref-kind", "ref-sha", "pin-mismatch", "ref-moved", "foreign-base", "foreign-mergebase", "behind", "diverged", "missing-commits", "last-commit", "duplicate-commit", "short-commit", "overflow", "paginated", "oversized", "error", "bad-ref", "bad-head", "empty-ancestors", "duplicate-ancestor", "too-many-ancestors"} {
		t.Run(mode, func(t *testing.T) {
			base, head := strings.Repeat("a", 40), strings.Repeat("b", 40)
			q := delivery.GitOpsRevisionRequest{Repository: "101", Ref: "main", AncestorSHAs: []string{base}}
			if mode == "pinned" {
				q.HeadSHA = head
			}
			if mode == "same" {
				q.AncestorSHAs = []string{head}
			}
			if mode == "two-ancestors" {
				q.AncestorSHAs = append(q.AncestorSHAs, head)
			}
			if mode == "max-ancestors" || mode == "aggregate-budget" {
				q.AncestorSHAs = nil
				for i := 0; i < 16; i++ {
					q.AncestorSHAs = append(q.AncestorSHAs, fmt.Sprintf("%040x", i+1))
				}
			}
			refs, repos, reads, writes := 0, 0, 0, 0
			p, c, tokens := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					writes++
					http.Error(w, "forbidden", 405)
					return
				}
				reads++
				if mode == "error" {
					w.WriteHeader(503)
					return
				}
				var response any
				switch {
				case r.URL.Path == "/repositories/101":
					repos++
					id, name := 101, "acme/source"
					if mode == "repository-id" {
						id = 202
					}
					if mode == "repository-name" || (mode == "repository-rebind" && repos > 1) {
						name = "other/source"
					}
					response = map[string]any{"id": id, "full_name": name}
				case r.URL.Path == "/repos/acme/source/git/ref/heads/main":
					refs++
					sha, ref, kind := head, "refs/heads/main", "commit"
					if mode == "ref-moved" && refs > 1 {
						sha = base
					}
					if mode == "ref-sha" {
						sha = "abc"
					}
					if mode == "ref-name" {
						ref = "refs/heads/other"
					}
					if mode == "ref-kind" {
						kind = "tag"
					}
					response = map[string]any{"ref": ref, "object": map[string]any{"sha": sha, "type": kind}}
				case strings.HasPrefix(r.URL.Path, "/repos/acme/source/compare/"):
					ancestor := strings.Split(strings.TrimPrefix(r.URL.Path, "/repos/acme/source/compare/"), "...")[0]
					status, ahead, total, behind := "ahead", 1, 1, 0
					commitbase, mergebase := ancestor, ancestor
					commits := []map[string]string{{"sha": head}}
					if ancestor == head {
						status = "identical"
						ahead = 0
						total = 0
						commits = nil
					}
					switch mode {
					case "foreign-base":
						commitbase = head
					case "foreign-mergebase":
						mergebase = head
					case "behind":
						behind = 1
					case "diverged":
						status = "diverged"
					case "missing-commits":
						commits = nil
					case "last-commit":
						commits[0]["sha"] = base
					case "short-commit":
						commits[0]["sha"] = "abc"
					case "duplicate-commit":
						commits = append(commits, commits[0])
						ahead = 2
						total = 2
					case "overflow":
						ahead = 101
						total = 101
					case "paginated":
						w.Header().Set("Link", "<next>; rel=next")
					case "oversized":
						w.Write([]byte(strings.Repeat(" ", 2<<20+1)))
						return
					}
					response = map[string]any{"status": status, "ahead_by": ahead, "behind_by": behind, "total_commits": total, "base_commit": map[string]string{"sha": commitbase}, "merge_base_commit": map[string]string{"sha": mergebase}, "commits": commits}
					if mode == "aggregate-budget" {
						response.(map[string]any)["padding"] = strings.Repeat("x", (2<<20)-1024)
					}
				default:
					t.Errorf("unexpected GET %s", r.URL.Path)
					w.WriteHeader(404)
					return
				}
				json.NewEncoder(w).Encode(response)
			})
			key := fmt.Sprintf("%s|%d|%d|%s", c.APIURL, c.AppID, c.InstallationID, c.PrivateKeyRef)
			if mode != "missing-auth" {
				expiry := time.Now().Add(time.Hour)
				if mode == "expired-auth" {
					expiry = time.Now().Add(-time.Hour)
				}
				p.tokens[key] = cachedToken{"installation-secret", expiry}
			}
			switch mode {
			case "numeric":
				q.Repository = "acme/source"
			case "leading-zero":
				q.Repository = "0101"
			case "owner":
				c.Owner = "other"
			case "pin-mismatch":
				q.HeadSHA = base
			case "bad-ref":
				q.Ref = "../main"
			case "bad-head":
				q.HeadSHA = "abc"
			case "empty-ancestors":
				q.AncestorSHAs = nil
			case "duplicate-ancestor":
				q.AncestorSHAs = append(q.AncestorSHAs, base)
			case "too-many-ancestors":
				for i := 0; i < 17; i++ {
					q.AncestorSHAs = append(q.AncestorSHAs, fmt.Sprintf("%040x", i))
				}
			}
			proof, err := p.VerifyGitOpsRevision(context.Background(), c, q)
			want := mode == "valid" || mode == "pinned" || mode == "same" || mode == "two-ancestors" || mode == "max-ancestors"
			if (err == nil) != want {
				t.Fatalf("proof=%+v error=%v", proof, err)
			}
			if want && (proof.Repository != "101" || proof.Ref != "main" || proof.HeadSHA != head || len(proof.AncestorSHAs) != len(q.AncestorSHAs) || refs != 2 || repos != 2) {
				t.Fatalf("incomplete proof %+v", proof)
			}
			if writes != 0 || tokens.Load() != 0 || reads > 22 {
				t.Fatalf("non read-only or unbounded: reads=%d writes=%d auth=%d", reads, writes, tokens.Load())
			}
			if (mode == "missing-auth" || mode == "expired-auth") && reads != 0 {
				t.Fatal("missing auth performed IO")
			}
		})
	}
}
