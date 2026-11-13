package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"releasecontrol/internal/buildconfig"
	"strings"
	"testing"
	"time"
)

type deploymentTransport func(*http.Request) (*http.Response, error)

func (f deploymentTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestDeployProviderOffline(t *testing.T) {
	for _, already := range []bool{false, true} {
		t.Run(fmt.Sprint(already), func(t *testing.T) {
			g := buildconfig.GitOps{Kind: "github", APIURL: "https://api.github.com", AppID: 1, InstallationID: 2, PrivateKeyRef: "github.pem", Repository: "o/flux", Branch: "main"}
			ctx := context.Background()
			value := "main-aaaaaaaa@sha256:" + strings.Repeat("a", 64)
			before := deployYAML("registry/o/r", "old")
			if already {
				before = deployYAML("registry/o/r", value)
			}
			after, e := RenderDeployment(before, []DeployTag{{"registry/o/r", value, 1, ""}})
			if e != nil {
				t.Fatal(e)
			}
			base, head, tree, newTree := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40), strings.Repeat("d", 40)
			branch := "rcp/deploy/test"
			branchExists := false
			writes, merges := 0, 0
			body := "build: build-1; sha: abc; digest sha256:abc; CI evidence success"
			pr := DeployPR{Number: 1, URL: "https://github.example/pull/1", State: "open"}
			yes := true
			pr.Mergeable = &yes
			pr.Head.SHA = head
			pr.Head.Ref = branch
			pr.Head.Repo.FullName = g.Repository
			pr.Base.SHA = base
			pr.Base.Ref = "main"
			pr.Base.Repo.FullName = g.Repository
			p := NewWithClient(&http.Client{Transport: deploymentTransport(func(r *http.Request) (*http.Response, error) {
				if r.Header.Get("Authorization") != "Bearer existing-token" {
					t.Fatal("wrong App auth")
				}
				path := r.URL.Path
				status := 200
				var result any
				if r.Method != "GET" {
					writes++
				}
				switch {
				case r.Method == "GET" && strings.HasPrefix(path, "/repos/o/flux/git/ref/heads/"):
					sha := base
					if strings.HasSuffix(path, branch) {
						if !branchExists {
							status = 404
						}
						sha = head
					}
					result = map[string]any{"object": map[string]string{"sha": sha}}
				case r.Method == "GET" && path == "/repos/o/flux/contents/app.yaml":
					content := before
					if r.URL.Query().Get("ref") == head {
						content = after
					}
					result = map[string]string{"type": "file", "encoding": "base64", "sha": gitOpsBlobSHA([]byte(content)), "content": base64.StdEncoding.EncodeToString([]byte(content))}
				case r.Method == "GET" && strings.Contains(path, "/git/commits/"):
					sha := base
					treeSHA := tree
					if strings.HasSuffix(path, head) {
						sha = head
						treeSHA = newTree
					}
					result = map[string]any{"sha": sha, "tree": map[string]string{"sha": treeSHA}}
				case r.Method == "GET" && strings.Contains(path, "/git/trees/"):
					treeSHA, blob := tree, gitOpsBlobSHA([]byte(before))
					if strings.HasSuffix(path, newTree) {
						treeSHA = newTree
						blob = gitOpsBlobSHA([]byte(after))
					}
					result = map[string]any{"sha": treeSHA, "truncated": false, "tree": []map[string]string{{"path": "app.yaml", "type": "blob", "mode": "100644", "sha": blob}, {"path": "untouched.yaml", "type": "blob", "mode": "100644", "sha": strings.Repeat("e", 40)}}}
				case r.Method == "POST" && strings.HasSuffix(path, "/git/trees"):
					var in struct {
						Tree []struct{ Path, Content string }
					}
					json.NewDecoder(r.Body).Decode(&in)
					if len(in.Tree) != 1 || in.Tree[0].Path != "app.yaml" || in.Tree[0].Content != after {
						t.Fatalf("unbounded write %+v", in)
					}
					result = map[string]string{"sha": newTree}
				case r.Method == "POST" && strings.HasSuffix(path, "/git/commits"):
					result = map[string]string{"sha": head}
				case r.Method == "POST" && strings.HasSuffix(path, "/git/refs"):
					branchExists = true
					result = map[string]any{}
				case r.Method == "POST" && strings.HasSuffix(path, "/pulls"):
					var in map[string]string
					json.NewDecoder(r.Body).Decode(&in)
					if in["body"] != body || in["base"] != "main" || in["head"] != branch {
						t.Fatal(in)
					}
					result = pr
				case r.Method == "GET" && strings.HasSuffix(path, "/pulls/1"):
					result = pr
				case r.Method == "GET" && strings.HasSuffix(path, "/pulls"):
					result = []DeployPR{pr}
				case r.Method == "PUT" && strings.HasSuffix(path, "/pulls/1/merge"):
					var in map[string]string
					json.NewDecoder(r.Body).Decode(&in)
					if in["sha"] != head || in["merge_method"] != "squash" {
						t.Fatal(in)
					}
					merges++
					result = map[string]any{"merged": true, "sha": strings.Repeat("f", 40)}
				default:
					t.Fatalf("unexpected API %s %s", r.Method, r.URL)
					return nil, fmt.Errorf("unexpected")
				}
				raw, _ := json.Marshal(result)
				return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(string(raw)))}, nil
			})})
			c, e := deployConnection(g)
			if e != nil {
				t.Fatal(e)
			}
			key := fmt.Sprintf("%s|%d|%d|%s", g.APIURL, g.AppID, g.InstallationID, c.PrivateKeyRef)
			p.tokens[key] = cachedToken{"existing-token", time.Now().Add(time.Hour)}
			plan := DeployPlan{ID: "test", Branch: branch, Body: body, Files: []DeployFile{{Path: "app.yaml", Tags: []DeployTag{{"registry/o/r", value, 1, ""}}}}}
			proposal, e := p.ProposeDeployment(ctx, g, plan)
			if e != nil {
				t.Fatal(e)
			}
			if already {
				if !proposal.AlreadyApplied || writes != 0 {
					t.Fatal(proposal, writes)
				}
				return
			}
			plan.ExpectedHead = proposal.HeadSHA
			if e = p.VerifyDeployment(ctx, g, plan, pr); e != nil {
				t.Fatal(e)
			}
			if _, e = p.MergeDeployment(ctx, g, 1, proposal.HeadSHA); e != nil || merges != 1 {
				t.Fatal(e)
			}
			found, e := p.FindDeployment(ctx, g, branch)
			if e != nil || found.Head.SHA != head {
				t.Fatal(found, e)
			}
			pr.Head.SHA = base
			if e = p.VerifyDeployment(ctx, g, plan, pr); e == nil {
				t.Fatal("changed PR head accepted")
			}
		})
	}
}
