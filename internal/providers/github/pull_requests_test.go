package github

import (
	"context"
	"net/http"
	"testing"
)

func TestObservePullRequestPreservesDeletedBranchAndOnlyRealMerge(t *testing.T) {
	for _, merged := range []bool{false, true} {
		t.Run(map[bool]string{false: "open", true: "merged"}[merged], func(t *testing.T) {
			p, c, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != "/repos/example/source/pulls/7" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(404)
					return
				}
				state := "open"
				if merged {
					state = "closed"
				}
				jsonOut(w, map[string]any{"number": 7, "title": "change", "state": state, "merged": merged, "merge_commit_sha": "merge", "head": map[string]any{"ref": "deleted", "sha": "head", "repo": nil}, "base": map[string]string{"ref": "main"}, "created_at": "2026-09-01T00:00:00Z"})
			})
			c.Owner = "example"
			got, err := p.ObservePullRequest(context.Background(), c, "example/source", 7)
			if err != nil {
				t.Fatal(err)
			}
			if got.Head != "deleted" || got.Base != "main" || got.HeadSHA != "head" || got.CreatedAt.IsZero() {
				t.Fatal(got)
			}
			if merged && (got.State != "MERGED" || got.MergeSHA != "merge") {
				t.Fatal(got)
			}
			if !merged && (got.State != "OPEN" || got.MergeSHA != "") {
				t.Fatal("test merge falsely recorded", got)
			}
		})
	}
}
