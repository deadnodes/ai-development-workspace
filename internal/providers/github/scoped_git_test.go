package github

import (
	"context"
	"net/http"
	"testing"
)

func TestScopedGitRequests(t *testing.T) {
	p, c, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Fatal("non-read request")
		}
		switch r.URL.Path {
		case "/repos/example/source/branches/feature/work":
			jsonOut(w, map[string]any{"name": "feature/work", "commit": map[string]string{"sha": "head"}})
		case "/repos/example/source/pulls":
			if r.URL.Query().Get("head") != "example:feature/work" || r.URL.Query().Get("per_page") != "10" {
				t.Error("unscoped discovery")
			}
			jsonOut(w, []any{map[string]any{"number": 7, "head": map[string]any{"ref": "feature/work", "repo": map[string]string{"full_name": "example/source"}}}, map[string]any{"number": 8, "head": map[string]any{"ref": "feature/work", "repo": map[string]string{"full_name": "other/source"}}}})
		case "/repos/example/source/pulls/7":
			jsonOut(w, map[string]any{"number": 7, "state": "open", "head": map[string]string{"ref": "feature/work", "sha": "head"}, "base": map[string]string{"ref": "main"}})
		default:
			t.Errorf("unexpected unscoped endpoint %s", r.URL.Path)
			w.WriteHeader(404)
		}
	})
	c.Owner = "example"
	if b, e := p.ObserveBranch(context.Background(), c, "example/source", "feature/work"); e != nil || b.SHA != "head" {
		t.Fatal(b, e)
	}
	if prs, e := p.DiscoverBranchPullRequests(context.Background(), c, "example/source", "feature/work"); e != nil || len(prs) != 1 || prs[0].Number != 7 {
		t.Fatal(prs, e)
	}
}
