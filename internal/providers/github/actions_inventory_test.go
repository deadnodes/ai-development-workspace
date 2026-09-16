package github

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestRecentActionsInventoryReadOnlyBoundedAndNoArtifactInference(t *testing.T) {
	p, c, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("unexpected mutation: %s", r.Method)
		}
		if r.URL.Path == "/repositories/123" {
			jsonOut(w, map[string]any{"id": 123, "full_name": "example/source"})
			return
		}
		if r.URL.Path != "/repos/example/source/actions/runs" || r.URL.Query().Get("status") != "success" || r.URL.Query().Get("per_page") != "20" || r.URL.Query().Get("page") != "1" {
			t.Errorf("unexpected inventory request: %s", r.URL)
		}
		jsonOut(w, map[string]any{"workflow_runs": []map[string]any{
			{"id": 45, "workflow_id": 2, "name": "publish", "head_sha": strings.Repeat("a", 40), "head_branch": "main", "status": "completed", "conclusion": "success", "html_url": "https://github.com/example/source/actions/runs/45", "created_at": "2026-09-16T12:00:00Z", "image_tag": "untrusted", "digest": "untrusted"},
			{"id": 46, "status": "in_progress", "conclusion": nil},
			{"id": 47, "status": "completed", "conclusion": "failure"},
		}})
	})
	c.Owner = "example"
	runs, err := p.ListRecentBuilds(context.Background(), c, "123")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].ID != 45 || runs[0].WorkflowID != 2 || runs[0].HeadBranch != "main" || runs[0].HeadSHA != strings.Repeat("a", 40) || runs[0].CreatedAt.IsZero() {
		t.Fatalf("unexpected workflow metadata: %+v", runs)
	}
}

func TestRecentActionsInventoryAuthorizationAndMalformedEvidence(t *testing.T) {
	for _, mode := range []string{"denied", "invalid", "oversized", "empty"} {
		t.Run(mode, func(t *testing.T) {
			p, c, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				if mode == "denied" {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				runs := []map[string]any{}
				if mode == "invalid" {
					runs = append(runs, map[string]any{"id": 1, "status": "completed", "conclusion": "success", "head_sha": "short"})
				}
				if mode == "oversized" {
					runs = make([]map[string]any, 21)
				}
				jsonOut(w, map[string]any{"workflow_runs": runs})
			})
			c.Owner = "example"
			runs, err := p.ListRecentBuilds(context.Background(), c, "example/source")
			if mode == "empty" {
				if err != nil || runs == nil || len(runs) != 0 {
					t.Fatalf("empty observation: %v %v", runs, err)
				}
			} else if err == nil {
				t.Fatal("missing expected failure")
			} else if mode == "denied" && !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("lost authorization error: %v", err)
			}
		})
	}
}
