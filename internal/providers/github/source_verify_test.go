package github

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"releasecontrol/internal/delivery"
)

func TestVerifyComposedSourceHonorsCancelledContext(t *testing.T) {
	p, c, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("cancelled verifier reached Git API: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(500)
	})
	plan := delivery.SourcePlan{Repository: "acme/repo", BaseRef: "main", BaseSHA: strings.Repeat("a", 40), OperationID: "op", TargetBranch: "generated/dev/op"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := p.VerifyComposedSource(ctx, c, plan, delivery.SourceResult{Branch: plan.TargetBranch, SHA: plan.BaseSHA})
	// The shared provider client deliberately normalizes transport errors.
	if err == nil || ctx.Err() != context.Canceled {
		t.Fatalf("cancelled verification unexpectedly succeeded: %v", err)
	}
}

// Every non-auth request must be a read, including error paths.
func TestVerifyComposedSourceReadOnly(t *testing.T) {
	for _, mode := range []string{"merge", "base-only", "contained", "fast-forward", "duplicate", "resolution", "missing-step", "missing-final", "moved-step", "moved-final", "wrong-parent", "wrong-head", "wrong-message", "wrong-result", "wrong-branch", "conflict-result", "detail-result", "missing-head", "resolution-missing-head", "compare-error"} {
		t.Run(mode, func(t *testing.T) {
			a, b, m, x := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40), strings.Repeat("d", 40)
			plan := delivery.SourcePlan{Repository: "acme/repo", BaseRef: "main", BaseSHA: a, HeadSHAs: []string{b}, OperationID: "op1", TargetBranch: "generated/dev/op1"}
			refs := map[string]string{plan.TargetBranch + "-step-1": m, plan.TargetBranch: m}
			expected := delivery.SourceResult{Branch: plan.TargetBranch, SHA: m}
			switch mode {
			case "base-only":
				plan.HeadSHAs = nil
				refs[plan.TargetBranch], expected.SHA = a, a
			case "contained", "missing-head", "compare-error":
				refs[plan.TargetBranch+"-step-1"], refs[plan.TargetBranch], expected.SHA = a, a, a
			case "fast-forward":
				refs[plan.TargetBranch+"-step-1"], refs[plan.TargetBranch], expected.SHA = b, b, b
			case "duplicate":
				plan.HeadSHAs = []string{b, b}
				refs[plan.TargetBranch+"-step-2"] = m
			case "resolution", "resolution-missing-head":
				plan.ResolutionSHA, plan.ResolutionConflictID = x, "conflict1"
				refs[plan.TargetBranch], expected.SHA = x, x
			case "missing-step":
				delete(refs, plan.TargetBranch+"-step-1")
			case "missing-final":
				delete(refs, plan.TargetBranch)
			case "moved-step":
				refs[plan.TargetBranch+"-step-1"] = x
			case "moved-final":
				refs[plan.TargetBranch] = x
			case "wrong-result":
				expected.SHA = x
			case "wrong-branch":
				expected.Branch = "generated/other"
			case "conflict-result":
				expected.Conflict = true
			case "detail-result":
				expected.Detail = "unsuccessful result"
			}
			reads, writes := 0, 0
			p, c, tokens := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					writes++
					t.Errorf("verifier attempted Git mutation: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(405)
					return
				}
				reads++
				path := strings.TrimPrefix(r.URL.Path, "/repos/acme/repo/")
				switch {
				case strings.HasPrefix(path, "git/ref/heads/"):
					branch := strings.TrimPrefix(path, "git/ref/heads/")
					sha, ok := refs[branch]
					if !ok {
						w.WriteHeader(404)
						return
					}
					json.NewEncoder(w).Encode(map[string]any{"ref": "refs/heads/" + branch, "object": map[string]string{"sha": sha, "type": "commit"}})
				case strings.HasPrefix(path, "git/commits/"):
					sha := strings.TrimPrefix(path, "git/commits/")
					_, message := sourceStepIdentity(plan, 0)
					parent := a
					selectedHead := b
					if mode == "wrong-parent" {
						parent = x
					}
					if mode == "wrong-head" {
						selectedHead = x
					}
					if mode == "wrong-message" || mode == "moved-step" {
						message = "foreign merge"
					}
					json.NewEncoder(w).Encode(map[string]any{"sha": sha, "message": message, "parents": []map[string]string{{"sha": parent}, {"sha": selectedHead}}})
				case strings.HasPrefix(path, "compare/"):
					if mode == "compare-error" {
						w.WriteHeader(503)
						return
					}
					pair := strings.Split(strings.TrimPrefix(path, "compare/"), "...")
					status, behind := "ahead", 0
					if mode == "missing-head" || mode == "resolution-missing-head" && pair[0] == b {
						status, behind = "diverged", 1
					}
					json.NewEncoder(w).Encode(map[string]any{"base_commit": map[string]string{"sha": pair[0]}, "status": status, "behind_by": behind, "ahead_by": 1})
				default:
					t.Errorf("unexpected read %s", path)
					w.WriteHeader(500)
				}
			})
			err := p.VerifyComposedSource(context.Background(), c, plan, expected)
			valid := mode == "merge" || mode == "base-only" || mode == "contained" || mode == "fast-forward" || mode == "duplicate" || mode == "resolution"
			if (err == nil) != valid {
				t.Fatalf("valid=%v error=%v", valid, err)
			}
			if writes != 0 {
				t.Fatalf("Git writes=%d", writes)
			}
			if valid && (reads == 0 || tokens.Load() != 1) {
				t.Fatalf("missing current provider reads/auth: %d %d", reads, tokens.Load())
			}
		})
	}
}

func TestSourcePlanVerificationBounds(t *testing.T) {
	base := delivery.SourcePlan{Repository: "acme/repo", BaseRef: "main", BaseSHA: strings.Repeat("a", 40), HeadSHAs: []string{strings.Repeat("b", 40)}, OperationID: "op", TargetBranch: "generated/dev/op"}
	for _, mode := range []string{"repository", "branch", "base-ref", "base-sha", "head-sha", "mixed-hash", "too-many-heads", "long-operation", "invalid-operation", "step-too-long", "resolution-sha", "resolution-id", "resolution-without-sha"} {
		t.Run(mode, func(t *testing.T) {
			plan := base
			switch mode {
			case "repository":
				plan.Repository = strings.Repeat("r", 257)
			case "branch":
				plan.TargetBranch = "main"
			case "base-ref":
				plan.BaseRef = "generated/base"
			case "base-sha":
				plan.BaseSHA = "short"
			case "head-sha":
				plan.HeadSHAs = []string{"short"}
			case "mixed-hash":
				plan.HeadSHAs = []string{strings.Repeat("b", 64)}
			case "too-many-heads":
				plan.HeadSHAs = make([]string, 21)
			case "long-operation":
				plan.OperationID = strings.Repeat("o", 129)
			case "invalid-operation":
				plan.OperationID = "op\n"
			case "step-too-long":
				plan.TargetBranch = "generated/" + strings.Repeat("x", 185)
			case "resolution-sha":
				plan.ResolutionSHA, plan.ResolutionConflictID = "short", "c"
			case "resolution-id":
				plan.ResolutionSHA, plan.ResolutionConflictID = plan.BaseSHA, strings.Repeat("c", 129)
			case "resolution-without-sha":
				plan.ResolutionConflictID = "c"
			}
			p := New()
			expected := delivery.SourceResult{Branch: plan.TargetBranch, SHA: base.BaseSHA}
			if err := p.VerifyComposedSource(context.Background(), delivery.Connection{}, plan, expected); err == nil {
				t.Fatal("accepted invalid verification plan")
			}
			if _, err := p.ComposeSource(context.Background(), delivery.Connection{}, plan); err == nil {
				t.Fatal("accepted invalid composition plan")
			}
		})
	}
}
