package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"releasecontrol/internal/delivery"
)

func TestSourceCompositionRetryAndTampering(t *testing.T) {
	a, b, m, x := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40), strings.Repeat("d", 40)
	refs := map[string]string{}
	merges := 0
	message := ""
	p, c, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/repos/acme/repo/")
		switch {
		case strings.HasPrefix(path, "git/ref/heads/"):
			branch := strings.TrimPrefix(path, "git/ref/heads/")
			sha, ok := refs[branch]
			if !ok {
				w.WriteHeader(404)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"ref": "refs/heads/" + branch, "object": map[string]string{"type": "commit", "sha": sha}})
		case path == "git/refs":
			var in struct{ Ref, SHA string }
			json.NewDecoder(r.Body).Decode(&in)
			refs[strings.TrimPrefix(in.Ref, "refs/heads/")] = in.SHA
			w.WriteHeader(201)
		case strings.HasPrefix(path, "compare/"):
			json.NewEncoder(w).Encode(map[string]any{"base_commit": map[string]string{"sha": b}, "status": "diverged", "behind_by": 1, "ahead_by": 1})
		case path == "merges":
			merges++
			var in map[string]string
			json.NewDecoder(r.Body).Decode(&in)
			if in["head"] != b {
				t.Error("wrong source")
			}
			message = in["commit_message"]
			refs[in["base"]] = m
			// Response lost after GitHub committed: adapter must recover from exact parents.
			w.WriteHeader(502)
		case strings.HasPrefix(path, "git/commits/"):
			sha := strings.TrimPrefix(path, "git/commits/")
			json.NewEncoder(w).Encode(map[string]any{"sha": sha, "message": message, "parents": []map[string]string{{"sha": a}, {"sha": b}}})
		default:
			t.Errorf("unexpected %s %s", r.Method, path)
			w.WriteHeader(500)
		}
	})
	plan := delivery.SourcePlan{Repository: "acme/repo", BaseRef: "main", BaseSHA: a, HeadSHAs: []string{b}, OperationID: "op1", TargetBranch: "generated/dev/op1"}
	for range 2 {
		out, err := p.ComposeSource(context.Background(), c, plan)
		if err != nil || out.SHA != m {
			t.Fatalf("%+v %v", out, err)
		}
	}
	if merges != 1 {
		t.Fatalf("redispatched merge %d", merges)
	}
	refs[plan.TargetBranch] = x
	if _, err := p.ComposeSource(context.Background(), c, plan); err == nil {
		t.Fatal("accepted modified final target")
	}
	refs[plan.TargetBranch] = m
	refs[plan.TargetBranch+"-step-1"] = x
	message = "external commit"
	if _, err := p.ComposeSource(context.Background(), c, plan); err == nil {
		t.Fatal("accepted external merge")
	}
}

func TestSourceConflictAndMainGuards(t *testing.T) {
	for _, mode := range []string{"conflict", "main", "moved", "not-ff"} {
		t.Run(mode, func(t *testing.T) {
			a, b := strings.Repeat("a", 40), strings.Repeat("b", 40)
			current := a
			patches := 0
			if mode == "moved" {
				current = strings.Repeat("d", 40)
			}
			p, c, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				path := strings.TrimPrefix(r.URL.Path, "/repos/acme/repo/")
				switch {
				case strings.HasPrefix(path, "git/ref/heads/"):
					branch := strings.TrimPrefix(path, "git/ref/heads/")
					json.NewEncoder(w).Encode(map[string]any{"ref": "refs/heads/" + branch, "object": map[string]string{"sha": current, "type": "commit"}})
				case strings.HasPrefix(path, "compare/"):
					base := a
					status := "ahead"
					behind := 0
					if mode == "conflict" {
						base = b
						status = "diverged"
						behind = 1
					}
					if mode == "not-ff" {
						status = "diverged"
						behind = 1
					}
					json.NewEncoder(w).Encode(map[string]any{"base_commit": map[string]string{"sha": base}, "status": status, "ahead_by": 1, "behind_by": behind})
				case path == "merges":
					w.WriteHeader(409)
				case strings.HasPrefix(path, "git/refs/heads/"):
					var in struct {
						SHA   string
						Force bool
					}
					json.NewDecoder(r.Body).Decode(&in)
					if in.Force {
						t.Fatal("force update")
					}
					patches++
					current = in.SHA
					w.WriteHeader(200)
				default:
					t.Error(fmt.Sprintf("unexpected %s", path))
					w.WriteHeader(500)
				}
			})
			if mode == "conflict" {
				out, err := p.ComposeSource(context.Background(), c, delivery.SourcePlan{Repository: "acme/repo", BaseRef: "main", BaseSHA: a, HeadSHAs: []string{b}, OperationID: "op", TargetBranch: "generated/dev/op"})
				if err != nil || !out.Conflict {
					t.Fatalf("%+v %v", out, err)
				}
				return
			}
			out, err := p.AdvanceMain(context.Background(), c, "acme/repo", "main", a, b)
			if mode == "main" {
				if err != nil || out.SHA != b {
					t.Fatalf("%+v %v", out, err)
				}
				_, err = p.AdvanceMain(context.Background(), c, "acme/repo", "main", a, b)
				if err != nil || patches != 1 {
					t.Fatal("non-idempotent main")
				}
			} else if err == nil || patches != 0 {
				t.Fatal("unsafe main mutation")
			}
		})
	}
}

func TestSourceBranchValidation(t *testing.T) {
	for _, branch := range []string{"@", "-main", "a\x00b", "a\x01b", "a\x1fb", "a\x7fb", "generated//dev", "generated/a.lock/b"} {
		if validSourceBranch(branch) {
			t.Errorf("accepted invalid Git branch %q", branch)
		}
	}
}
