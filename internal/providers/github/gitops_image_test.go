package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"releasecontrol/internal/delivery"
)

type gitOpsDestinationGuard struct {
	base http.RoundTripper
	host string
	t    *testing.T
}

func (g gitOpsDestinationGuard) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host != g.host {
		g.t.Errorf("GitHub token was sent outside configured API host: %s", r.URL.Host)
		return nil, errors.New("unexpected destination")
	}
	return g.base.RoundTrip(r)
}

func TestGitOpsPrivateImageValidationBeforeAuthentication(t *testing.T) {
	invalid := []string{"", "https://registry.example/team/app", "user@registry.example/team/app", "registry.example/team/app:latest", "registry.example/team/app@sha256:dead", "registry.example/team/app?x", "registry.example/team/app#x", "registry.example/team/../app", "registry.example/team//app", "registry.example/Team/app", "registry.example:0/team/app", "registry.example:65536/team/app", "registry.example:bad/team/app", "Registry.Example/team/app", "registry.example:443/team/app", "registry.example:05000/team/app", "registry.example/team/app\n", "registry.example/team/app ", "registry.example/team/\x00app", "[::1]:05000/team/app", "owner/app:latest"}
	for _, image := range invalid {
		t.Run(image, func(t *testing.T) {
			requests, guards := 0, 0
			p, c, tokens := fixture(t, func(w http.ResponseWriter, r *http.Request) { requests++; http.Error(w, "unexpected", 500) })
			req := guardedProposalRequest()
			req.Request.ImageRepository = image
			_, err := p.ProposeGitOpsGuarded(context.Background(), c, req, func(context.Context) error { guards++; return nil })
			if err == nil || requests != 0 || guards != 0 || tokens.Load() != 0 {
				t.Fatalf("invalid input reached provider: err=%v requests=%d guards=%d auth=%d", err, requests, guards, tokens.Load())
			}
			if _, err = p.ApplyGitOps(context.Background(), c, req); err == nil || tokens.Load() != 0 {
				t.Fatalf("invalid apply authenticated: %v", err)
			}
			if image != "" {
				if _, err = p.ReadGitOps(context.Background(), c, req.Request); err == nil || tokens.Load() != 0 {
					t.Fatalf("invalid read authenticated: %v", err)
				}
			}
		})
	}
	for _, field := range []string{"digest", "blob", "head", "operation", "path", "mapping", "ref", "pin", "message"} {
		t.Run(field, func(t *testing.T) {
			p, c, tokens := fixture(t, func(w http.ResponseWriter, r *http.Request) { t.Errorf("invalid input reached %s", r.URL.Path) })
			req := guardedProposalRequest()
			req.Request.ImageRepository = "registry.example:5443/team/app"
			switch field {
			case "digest":
				req.Digest = "latest"
			case "blob":
				req.ExpectedBlobSHA = "bad"
			case "head":
				req.ExpectedHeadSHA = "bad"
			case "operation":
				req.OperationID = "../../bad"
			case "path":
				req.Request.Path = "../app.yaml"
			case "mapping":
				req.Request.ImageField = "metadata.name"
			case "ref":
				req.Request.Ref = "../main"
			case "pin":
				req.Request.ExpectedHeadSHA = strings.Repeat("f", 40)
			case "message":
				req.Message = strings.Repeat("x", 4097)
			}
			if _, err := p.ProposeGitOpsGuarded(context.Background(), c, req, func(context.Context) error { return nil }); err == nil || tokens.Load() != 0 {
				t.Fatalf("invalid %s authenticated: %v", field, err)
			}
		})
	}
}

func TestGitOpsPrivateRepositoryExactHTTPRendering(t *testing.T) {
	for _, image := range []string{"registry.example:5443/team/deep/app", "localhost:5000/team/app", "127.0.0.1:5000/team/app", "[::1]:5000/team/app", "ghcr.io/acme/app", "acme/app"} {
		for _, helm := range []bool{false, true} {
			name := image + "/root"
			if helm {
				name = image + "/helm"
			}
			t.Run(name, func(t *testing.T) {
				desired := image
				if image == "acme/app" {
					desired = "ghcr.io/acme/app"
				}
				old := "sha256:" + strings.Repeat("e", 64)
				digest := "sha256:" + strings.Repeat("d", 64)
				before := "# untouched comment\nimage:\n  repository: '" + desired + "' # repo comment\n  digest: " + old + " # digest comment\nother: [1, 2]\n"
				after := "# untouched comment\nimage:\n  repository: \"" + desired + "\" # repo comment\n  digest: \"" + digest + "\" # digest comment\nother: [1, 2]\n"
				request := delivery.GitOpsRequest{Repository: "acme/source", Ref: "main", Path: "app.yaml", ImageRepository: image}
				if helm {
					request.ImageField = "spec.values.image.repository"
					request.DigestField = "spec.values.image.tag"
					prefix := "# untouched comment\napiVersion: helm.toolkit.fluxcd.io/v2\nkind: HelmRelease\nmetadata:\n  name: "
					resource := func(name, repo, value string) string {
						return prefix + name + "\nspec:\n  values:\n    image:\n      repository: '" + repo + "' # repo comment\n      tag: " + value + " # tag comment\n    other: [1, 2]\n"
					}
					unrelated := resource("other", "other.example/team/other", "main")
					before = resource("api", desired, "rcp@"+old) + "---\n" + resource("worker", desired, "rcp@"+old) + "---\n" + unrelated
					after = strings.ReplaceAll(before, "repository: '"+desired+"'", "repository: \""+desired+"\"")
					after = strings.ReplaceAll(after, "tag: rcp@"+old, "tag: \"rcp@"+digest+"\"")
				}
				puts := 0
				written := ""
				p, c, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
					switch {
					case strings.Contains(r.URL.Path, "/git/ref/heads/"):
						jsonOut(w, map[string]any{"object": map[string]string{"sha": strings.Repeat("a", 40)}})
					case strings.Contains(r.URL.Path, "/contents/"):
						if r.Method == http.MethodPut {
							puts++
							var body map[string]string
							if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
								t.Error(err)
								http.Error(w, "bad body", 400)
								return
							}
							raw, err := base64.StdEncoding.DecodeString(body["content"])
							if err != nil {
								t.Error(err)
								http.Error(w, "bad content", 400)
								return
							}
							written = string(raw)
							if body["branch"] != "main" || body["message"] != "release [rcp:abcdef0123456789]" {
								t.Errorf("unexpected body: %+v", body)
							}
							jsonOut(w, map[string]any{"commit": map[string]string{"sha": strings.Repeat("b", 40)}, "content": map[string]string{"sha": strings.Repeat("c", 40)}})
							return
						}
						jsonOut(w, map[string]string{"type": "file", "sha": strings.Repeat("c", 40), "encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(before))})
					default:
						t.Errorf("unexpected GitHub request: %s", r.URL)
						http.NotFound(w, r)
					}
				})
				api, err := url.Parse(c.APIURL)
				if err != nil {
					t.Fatal(err)
				}
				transport := p.client.Transport
				if transport == nil {
					transport = http.DefaultTransport
				}
				p.client.Transport = gitOpsDestinationGuard{base: transport, host: api.Host, t: t}
				req := delivery.GitOpsApply{Request: request, ExpectedHeadSHA: strings.Repeat("a", 40), ExpectedBlobSHA: strings.Repeat("c", 40), Digest: digest, OperationID: "abcdef0123456789", Message: "release"}
				if _, err := p.ApplyGitOps(context.Background(), c, req); err != nil {
					t.Fatal(err)
				}
				if puts != 1 || written != after {
					t.Fatalf("exact byte-preserving write failed puts=%d\ngot=%q\nwant=%q", puts, written, after)
				}
				before = after
				result, err := p.ApplyGitOps(context.Background(), c, req)
				if err != nil || !result.AlreadyApplied || puts != 1 {
					t.Fatalf("already-applied wrote again: %+v %v puts=%d", result, err, puts)
				}
			})
		}
	}
}
