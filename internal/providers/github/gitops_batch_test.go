package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"releasecontrol/internal/delivery"
	"strings"
	"testing"
	"time"
)

func batchHelm(name, repo, digest string) string {
	return "apiVersion: helm.toolkit.fluxcd.io/v2\nkind: HelmRelease\nmetadata:\n  name: " + name + "\nspec:\n  values:\n    image:\n      repository: " + repo + "\n      tag: rcp@" + digest + " # retained tag comment\n"
}
func batchTestRequest() (delivery.GitOpsBatchApply, map[string]string, map[string]string) {
	old := "sha256:" + strings.Repeat("1", 64)
	desired := "sha256:" + strings.Repeat("2", 64)
	before := map[string]string{"apps.yaml": "# shared file\n" + batchHelm("a-api", "ghcr.io/acme/dev-a", old) + "---\n" + batchHelm("a-worker", "ghcr.io/acme/dev-a", old) + "---\n" + batchHelm("b-api", "ghcr.io/acme/dev-b", old) + "---\n" + batchHelm("b-worker", "ghcr.io/acme/dev-b", old) + "---\n" + batchHelm("untouched", "ghcr.io/acme/untouched", old), "other.yaml": "image:\n  repository: ghcr.io/acme/dev-c\n  digest: " + old + "\n# retained\n"}
	b := delivery.GitOpsBatchApply{Repository: "101", Ref: "main", ExpectedHeadSHA: strings.Repeat("a", 40), OperationID: "abcdef0123456789", Message: "batch release"}
	for i, name := range []string{"a", "b", "c"} {
		path := "apps.yaml"
		image, digestField := "spec.values.image.repository", "spec.values.image.tag"
		if name == "c" {
			path = "other.yaml"
			image = "image.repository"
			digestField = "image.digest"
		}
		b.Changes = append(b.Changes, delivery.GitOpsImageChange{ID: fmt.Sprintf("abcdef012345678%d", i), Selector: delivery.GitOpsRequest{Repository: "101", Ref: "main", Path: path, ImageRepository: "ghcr.io/acme/dev-" + name, ImageField: image, DigestField: digestField}, ExpectedBlobSHA: gitOpsBlobSHA([]byte(before[path])), ImageRepository: "ghcr.io/acme/prod-" + name, Digest: desired})
	}
	// Independent scalar output, including untouched comments/resources.
	after := map[string]string{}
	for path, raw := range before {
		for _, name := range []string{"a", "b", "c"} {
			raw = strings.ReplaceAll(raw, "repository: ghcr.io/acme/dev-"+name, "repository: \"ghcr.io/acme/prod-"+name+"\"")
		}
		raw = strings.ReplaceAll(raw, "tag: rcp@"+old+" # retained tag comment", "tag: \"rcp@"+desired+"\" # retained tag comment")
		if path == "apps.yaml" {
			untouched := batchHelm("untouched", "ghcr.io/acme/untouched", old)
			wrong := strings.ReplaceAll(untouched, "tag: rcp@"+old, "tag: \"rcp@"+desired+"\"")
			raw = strings.ReplaceAll(raw, wrong, untouched)
		} else {
			raw = strings.ReplaceAll(raw, "digest: "+old, "digest: \""+desired+"\"")
		}
		after[path] = raw
	}
	return b, before, after
}

func TestGitOpsBatchHTTPProposalAndReadOnlyRecovery(t *testing.T) {
	for _, mutation := range []string{"valid", "guard-1", "guard-2", "guard-3", "guard-4", "guard-redirect", "lost-tree", "lost-commit", "lost-ref", "lost-pr", "existing-base-ref", "extra-file", "deleted-file", "mode-change", "unrelated-edit", "extra-yaml", "truncated", "pagination", "changed-parent", "changed-message", "changed-head", "foreign-pr-repo", "expired-token", "missing-token", "closed-deleted-branch", "open-deleted-branch", "merged-deleted-branch", "merged-deleted-branch-drift"} {
		t.Run(mutation, func(t *testing.T) {
			b, before, after := batchTestRequest()
			head := strings.Repeat("b", 40)
			baseTree, headTree := strings.Repeat("c", 40), strings.Repeat("d", 40)
			branch := "rcp/gitops/" + b.OperationID
			refExists, prExists := false, false
			writes := []string{}
			gets := 0
			rendered := map[string]string{}
			if strings.Contains(mutation, "deleted-branch") {
				prExists = true
			}
			prReads := 0
			mergedAt := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
			pr := func() map[string]any {
				prReads++
				repoID := 101
				if mutation == "foreign-pr-repo" {
					repoID = 202
				}
				state := "open"
				if mutation == "closed-deleted-branch" || strings.HasPrefix(mutation, "merged-deleted-branch") {
					state = "closed"
				}
				result := map[string]any{"number": 7, "html_url": "https://github.test/acme/source/pull/7", "state": state, "head": map[string]any{"ref": branch, "sha": head, "repo": map[string]any{"id": repoID, "full_name": "acme/source"}}, "base": map[string]any{"ref": "main", "repo": map[string]any{"id": 101, "full_name": "acme/source"}}}
				if strings.HasPrefix(mutation, "merged-deleted-branch") {
					result["merged"] = true
					timestamp := mergedAt
					if mutation == "merged-deleted-branch-drift" && prReads > 1 {
						timestamp = timestamp.Add(time.Second)
					}
					result["merged_at"] = timestamp.Format(time.RFC3339)
				}
				return result
			}
			p, c, tokens := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				var response any
				if r.Method != "GET" {
					writes = append(writes, r.URL.Path)
					switch r.URL.Path {
					case "/repos/acme/source/git/trees":
						var v struct {
							Base string `json:"base_tree"`
							Tree []struct{ Path, Mode, Type, Content string }
						}
						json.NewDecoder(r.Body).Decode(&v)
						if v.Base != baseTree || len(v.Tree) != 2 {
							t.Error("missing full base or paths")
						}
						for _, e := range v.Tree {
							if e.Mode != "100644" || e.Type != "blob" || e.Content != after[e.Path] {
								t.Errorf("bad rendered entry %s", e.Path)
							}
							rendered[e.Path] = e.Content
						}
						response = map[string]string{"sha": headTree}
					case "/repos/acme/source/git/commits":
						var v struct {
							Message, Tree string
							Parents       []string
						}
						json.NewDecoder(r.Body).Decode(&v)
						if v.Message != b.Message+" [rcp:"+b.OperationID+"]" || v.Tree != headTree || len(v.Parents) != 1 || v.Parents[0] != b.ExpectedHeadSHA {
							t.Error("invalid commit")
						}
						response = map[string]string{"sha": head}
					case "/repos/acme/source/git/refs":
						var v map[string]string
						json.NewDecoder(r.Body).Decode(&v)
						if v["ref"] != "refs/heads/"+branch || v["sha"] != head {
							t.Error("wrong ref")
						}
						refExists = true
						response = map[string]any{"ref": "refs/heads/" + branch, "object": map[string]string{"type": "commit", "sha": head}}
					case "/repos/acme/source/pulls":
						prExists = true
						response = pr()
					default:
						t.Errorf("unexpected mutation %s", r.URL.Path)
					}
					if mutation == "guard-redirect" && strings.HasSuffix(r.URL.Path, "/git/trees") {
						w.Header().Set("Location", "/redirect-tree")
						w.WriteHeader(http.StatusTemporaryRedirect)
						return
					}
					lost := map[string]string{"lost-tree": "/git/trees", "lost-commit": "/git/commits", "lost-ref": "/git/refs", "lost-pr": "/pulls"}[mutation]
					if lost != "" && strings.HasSuffix(r.URL.Path, lost) {
						http.Error(w, "lost response", 503)
						return
					}
				} else {
					gets++
					if mutation == "pagination" && strings.Contains(r.URL.Path, "/pulls") {
						w.Header().Set("Link", "next")
					}
					switch r.URL.Path {
					case "/repositories/101":
						response = map[string]any{"id": 101, "full_name": "acme/source"}
					case "/repos/acme/source/git/ref/heads/main":
						response = map[string]any{"ref": "refs/heads/main", "object": map[string]string{"type": "commit", "sha": b.ExpectedHeadSHA}}
					case "/repos/acme/source/git/ref/heads/" + branch:
						if !refExists && mutation != "existing-base-ref" {
							http.NotFound(w, r)
							return
						}
						sha := head
						if mutation == "existing-base-ref" {
							sha = b.ExpectedHeadSHA
						}
						if mutation == "changed-head" {
							sha = strings.Repeat("f", 40)
						}
						response = map[string]any{"ref": "refs/heads/" + branch, "object": map[string]string{"type": "commit", "sha": sha}}
					case "/repos/acme/source/git/commits/" + b.ExpectedHeadSHA:
						response = map[string]any{"sha": b.ExpectedHeadSHA, "tree": map[string]string{"sha": baseTree}}
					case "/repos/acme/source/git/commits/" + head:
						parent := b.ExpectedHeadSHA
						message := b.Message + " [rcp:" + b.OperationID + "]"
						if mutation == "changed-parent" {
							parent = head
						}
						if mutation == "changed-message" {
							message = "foreign"
						}
						response = map[string]any{"sha": head, "tree": map[string]string{"sha": headTree}, "message": message, "parents": []map[string]string{{"sha": parent}}}
					case "/repos/acme/source/git/trees/" + baseTree, "/repos/acme/source/git/trees/" + headTree:
						isHead := strings.HasSuffix(r.URL.Path, headTree)
						sha := baseTree
						files := before
						if isHead {
							sha = headTree
							files = after
						}
						entries := []map[string]string{}
						for _, path := range []string{"apps.yaml", "other.yaml"} {
							if isHead && mutation == "deleted-file" && path == "other.yaml" {
								continue
							}
							mode := "100644"
							if isHead && mutation == "mode-change" {
								mode = "100755"
							}
							raw := files[path]
							if isHead && mutation == "extra-yaml" {
								raw += "injected: true\n"
							}
							entries = append(entries, map[string]string{"path": path, "mode": mode, "type": "blob", "sha": gitOpsBlobSHA([]byte(raw))})
						}
						sibling := strings.Repeat("e", 40)
						if isHead && mutation == "unrelated-edit" {
							sibling = head
						}
						entries = append(entries, map[string]string{"path": "README.md", "mode": "100644", "type": "blob", "sha": sibling})
						if isHead && mutation == "extra-file" {
							entries = append(entries, map[string]string{"path": "extra", "mode": "100644", "type": "blob", "sha": sibling})
						}
						response = map[string]any{"sha": sha, "truncated": mutation == "truncated", "tree": entries}
					case "/repos/acme/source/pulls":
						if mutation == "pagination" {
							w.Header().Set("Link", "next")
						}
						if prExists {
							response = []any{pr()}
						} else {
							response = []any{}
						}
					case "/repos/acme/source/pulls/7":
						if !prExists {
							http.NotFound(w, r)
							return
						}
						response = pr()
					default:
						found := false
						for _, files := range []map[string]string{before, after} {
							for _, raw := range files {
								if mutation == "extra-yaml" && files["apps.yaml"] == after["apps.yaml"] {
									raw += "injected: true\n"
								}
								sha := gitOpsBlobSHA([]byte(raw))
								if r.URL.Path == "/repos/acme/source/git/blobs/"+sha {
									response = map[string]any{"sha": sha, "size": len(raw), "encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(raw))}
									found = true
								}
							}
						}
						if !found {
							t.Errorf("unexpected GET %s", r.URL.Path)
							http.NotFound(w, r)
							return
						}
					}
				}
				json.NewEncoder(w).Encode(response)
			})
			base, _ := apiBase(c)
			key := fmt.Sprintf("%s|%d|%d|%s", base, c.AppID, c.InstallationID, c.PrivateKeyRef)
			expiry := time.Now().Add(time.Hour)
			if mutation == "expired-token" {
				expiry = time.Now().Add(-time.Hour)
			}
			if mutation != "missing-token" {
				p.tokens[key] = cachedToken{"installation-secret", expiry}
			}
			var got delivery.GitOpsPullRequest
			var err error
			guardCalls := 0
			guardStop := 0
			if strings.HasPrefix(mutation, "guard-") {
				fmt.Sscanf(mutation, "guard-%d", &guardStop)
			}
			if mutation == "guard-redirect" {
				guardStop = 2
				p.client.CheckRedirect = nil
			}
			if strings.Contains(mutation, "deleted-branch") {
				got, err = p.ObserveGitOpsBatchPR(context.Background(), c, b, delivery.GitOpsPullRequest{Number: 7, HeadSHA: head, URL: "https://github.test/acme/source/pull/7"})
			} else {
				got, err = p.ProposeGitOpsBatchGuarded(context.Background(), c, b, func(context.Context) error {
					guardCalls++
					if guardCalls == guardStop {
						return errors.New("lease lost")
					}
					return nil
				})
			}
			if mutation == "valid" || mutation == "closed-deleted-branch" || mutation == "merged-deleted-branch" {
				if err != nil || got.Number != 7 {
					t.Fatalf("proposal failed %+v %v", got, err)
				}
				if mutation == "merged-deleted-branch" && (!got.Merged || got.MergedAt == nil || !got.MergedAt.Equal(mergedAt)) {
					t.Fatal("provider dropped authoritative merge timestamp", got)
				}
			} else if err == nil || got.Number != 0 {
				t.Fatalf("invalid proof accepted %+v %v", got, err)
			}
			if guardStop > 0 && (guardCalls != guardStop || len(writes) != guardStop-1) {
				t.Fatalf("guard lost send fence calls=%d writes=%v", guardCalls, writes)
			}
			if mutation == "valid" && len(writes) != 4 {
				t.Fatalf("writes=%v", writes)
			}
			if mutation == "valid" {
				n := len(writes)
				recovered, e := p.LookupGitOpsBatchProposal(context.Background(), c, b)
				if e != nil || recovered != got || len(writes) != n {
					t.Fatalf("exact recovery failed %+v %v", recovered, e)
				}
			}
			if strings.HasPrefix(mutation, "lost-") {
				beforeWrites := len(writes)
				_, _ = p.LookupGitOpsBatchProposal(context.Background(), c, b)
				if len(writes) != beforeWrites {
					t.Fatal("recovery retried mutation")
				}
				want := map[string]int{"lost-tree": 1, "lost-commit": 2, "lost-ref": 3, "lost-pr": 4}[mutation]
				if beforeWrites != want {
					t.Fatalf("continued after unknown response %v", writes)
				}
			}
			if (mutation == "expired-token" || mutation == "missing-token" || mutation == "existing-base-ref" || strings.Contains(mutation, "deleted-branch")) && len(writes) != 0 {
				t.Fatalf("unexpected writes %v", writes)
			}
			if tokens.Load() != 0 || gets > 128 {
				t.Fatalf("auth refresh or unbounded reads tokens=%d reads=%d", tokens.Load(), gets)
			}
		})
	}
}

func TestGitOpsBatchRendererRejectsOverlapCollisionAndWrongCurrent(t *testing.T) {
	b, before, _ := batchTestRequest()
	for _, mutation := range []string{"overlap", "collision-selected", "collision-unselected", "wrong-current", "wrong-root-current", "inconsistent-roles", "tagged", "anchor", "alias", "multiline"} {
		t.Run(mutation, func(t *testing.T) {
			changes := append([]delivery.GitOpsImageChange{}, b.Changes[:2]...)
			raw := before["apps.yaml"]
			switch mutation {
			case "overlap":
				changes[1].Selector = changes[0].Selector
			case "collision-selected":
				changes[1].ImageRepository = changes[0].ImageRepository
			case "collision-unselected":
				changes[0].ImageRepository = "ghcr.io/acme/untouched"
			case "wrong-current":
				changes[0].Selector.ImageRepository = "ghcr.io/acme/missing"
			case "wrong-root-current":
				changes = b.Changes[2:]
				raw = strings.ReplaceAll(before["other.yaml"], "dev-c", "missing")
			case "inconsistent-roles":
				raw = strings.Replace(raw, "tag: rcp@sha256:"+strings.Repeat("1", 64), "tag: rcp@sha256:"+strings.Repeat("3", 64), 1)
			case "tagged":
				raw = strings.Replace(raw, "repository: ghcr.io/acme/dev-a", "repository: !!str ghcr.io/acme/dev-a", 1)
			case "anchor":
				raw = strings.Replace(raw, "repository: ghcr.io/acme/dev-a", "repository: &image ghcr.io/acme/dev-a", 1)
			case "alias":
				raw = strings.Replace(raw, "repository: ghcr.io/acme/dev-a", "repository: *image", 1)
			case "multiline":
				raw = strings.Replace(raw, "repository: ghcr.io/acme/dev-a", "repository: >\n        ghcr.io/acme/dev-a", 1)
			}
			if _, e := renderGitOpsBatchFile(raw, changes); e == nil {
				t.Fatal("unsafe batch accepted")
			}
		})
	}
}

func TestGitOpsBatchWriteGuardBeforeEverySend(t *testing.T) {
	// A latched guard is shared by all physical sends, including redirects.
	calls := 0
	g := latchGitOpsGuard(func(context.Context) error {
		calls++
		if calls == 2 {
			return errors.New("lease lost")
		}
		return nil
	})
	if g(context.Background()) != nil || g(context.Background()) == nil || g(context.Background()) == nil || calls != 2 {
		t.Fatal("guard did not latch")
	}
}

func TestGitOpsBatchInvalidRequestBeforeAnyIO(t *testing.T) {
	for _, mutation := range []string{"repository", "head", "operation", "count", "id", "desired", "current", "digest", "path", "depth", "mapping", "pin", "blob", "request-size"} {
		t.Run(mutation, func(t *testing.T) {
			b, _, _ := batchTestRequest()
			switch mutation {
			case "repository":
				b.Repository = "acme/source"
			case "head":
				b.ExpectedHeadSHA = "bad"
			case "operation":
				b.OperationID = "../bad"
			case "count":
				for len(b.Changes) < 17 {
					b.Changes = append(b.Changes, b.Changes[0])
				}
			case "id":
				b.Changes[1].ID = b.Changes[0].ID
			case "desired":
				b.Changes[0].ImageRepository = "acme/prod"
			case "current":
				b.Changes[0].Selector.ImageRepository = "acme/dev"
			case "digest":
				b.Changes[0].Digest = "latest"
			case "path":
				b.Changes[0].Selector.Path = "../file"
			case "depth":
				b.Changes[0].Selector.Path = strings.Repeat("dir/", 16) + "file"
			case "mapping":
				b.Changes[0].Selector.ImageField = "spec.arbitrary"
			case "pin":
				b.Changes[0].Selector.ExpectedHeadSHA = strings.Repeat("f", 40)
			case "blob":
				b.Changes[0].ExpectedBlobSHA = "bad"
			case "request-size":
				b.Changes[0].Selector.Path = strings.Repeat("x", 64<<10)
			}
			calls := 0
			p, c, tokens := fixture(t, func(w http.ResponseWriter, r *http.Request) { calls++; http.Error(w, "forbidden", 405) })
			_, e := p.ProposeGitOpsBatchGuarded(context.Background(), c, b, func(context.Context) error { t.Fatal("invalid batch reached guard"); return nil })
			if e == nil || calls != 0 || tokens.Load() != 0 {
				t.Fatalf("invalid request reached IO %v %d", e, calls)
			}
		})
	}
}

func TestGitOpsBatchFullNestedTreeProof(t *testing.T) {
	for _, mutation := range []string{"valid", "nested-sibling", "root-sibling", "addition", "deletion", "symlink", "truncated"} {
		t.Run(mutation, func(t *testing.T) {
			rootOld, rootNew, subOld, subNew := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40), strings.Repeat("d", 40)
			blobOld, blobNew := strings.Repeat("e", 40), strings.Repeat("f", 40)
			p, c, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				sha := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
				entries := []proposalEntry{}
				switch sha {
				case rootOld, rootNew:
					sub := subOld
					if sha == rootNew {
						sub = subNew
					}
					sibling := blobOld
					if sha == rootNew && mutation == "root-sibling" {
						sibling = blobNew
					}
					entries = []proposalEntry{{"apps", "040000", "tree", sub}, {"other", "100644", "blob", sibling}}
				case subOld, subNew:
					blob := blobOld
					if sha == subNew {
						blob = blobNew
					}
					mode := "100644"
					if sha == subNew && mutation == "symlink" {
						mode = "120000"
					}
					entries = []proposalEntry{{"a.yaml", mode, "blob", blob}, {"b.yaml", "100755", "blob", blob}, {"untouched", "100644", "blob", blobOld}}
					if sha == subNew {
						switch mutation {
						case "nested-sibling":
							entries[2].SHA = blobNew
						case "addition":
							entries = append(entries, proposalEntry{"injected", "100644", "blob", blobOld})
						case "deletion":
							entries = entries[:2]
						}
					}
				default:
					t.Errorf("unexpected tree %s", sha)
				}
				json.NewEncoder(w).Encode(map[string]any{"sha": sha, "truncated": mutation == "truncated", "tree": entries})
			})
			cacheProposalTestToken(t, p, c)
			reader, e := p.batchReader(context.Background(), c)
			if e != nil {
				t.Fatal(e)
			}
			leaves, e := reader.batchLeaves("acme/source", rootOld, rootNew, []string{"apps/a.yaml", "apps/b.yaml"})
			if mutation == "valid" {
				if e != nil || len(leaves) != 2 || leaves["apps/b.yaml"].mode != "100755" {
					t.Fatalf("valid trie proof failed %+v %v", leaves, e)
				}
			} else if e == nil {
				t.Fatal("unexpected tree change admitted")
			}
		})
	}
}

func TestGitOpsBatchReadAndContentBounds(t *testing.T) {
	for _, kind := range []string{"request-budget", "response-budget", "original-budget", "file-budget"} {
		t.Run(kind, func(t *testing.T) {
			raw := "image:\n  repository: ghcr.io/acme/dev\n  digest: sha256:" + strings.Repeat("1", 64) + "\n#" + strings.Repeat("x", 900000) + "\n"
			if kind == "file-budget" {
				raw += strings.Repeat("x", 1<<20)
			}
			sha := gitOpsBlobSHA([]byte(raw))
			reads := 0
			p, c, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				reads++
				if strings.Contains(r.URL.Path, "/git/blobs/") {
					json.NewEncoder(w).Encode(map[string]any{"sha": sha, "size": len(raw), "encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(raw))})
					return
				}
				value := "ok"
				if kind == "response-budget" {
					value = strings.Repeat("x", 1<<20)
				}
				json.NewEncoder(w).Encode(map[string]string{"value": value})
			})
			cacheProposalTestToken(t, p, c)
			r, e := p.batchReader(context.Background(), c)
			if e != nil {
				t.Fatal(e)
			}
			switch kind {
			case "request-budget", "response-budget":
				limit := 129
				if kind == "response-budget" {
					limit = 17
				}
				for i := 0; i < limit; i++ {
					var v map[string]string
					e = r.get("/evidence", &v)
					if e != nil {
						break
					}
				}
				if e == nil {
					t.Fatal("unbounded read accepted")
				}
				if kind == "request-budget" && reads != 128 {
					t.Fatalf("requests=%d", reads)
				}
			case "original-budget":
				b := delivery.GitOpsBatchApply{}
				leaves := map[string]batchLeaf{}
				for i := 0; i < 5; i++ {
					path := fmt.Sprintf("file%d.yaml", i)
					b.Changes = append(b.Changes, delivery.GitOpsImageChange{ExpectedBlobSHA: sha, Selector: delivery.GitOpsRequest{Path: path, ImageRepository: "ghcr.io/acme/dev"}, ImageRepository: "ghcr.io/acme/prod", Digest: "sha256:" + strings.Repeat("2", 64)})
					leaves[path] = batchLeaf{sha, sha, "100644"}
				}
				if _, e = r.batchRendered("acme/source", b, leaves, false); e == nil {
					t.Fatal("oversized total originals accepted")
				}
			case "file-budget":
				if _, e = r.blob("acme/source", sha); e == nil {
					t.Fatal("oversized file accepted")
				}
			}
		})
	}
}
