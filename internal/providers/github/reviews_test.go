package github

import (
	"context"
	"net/http"
	"testing"
)

func TestCodexReviewSourcesAndIdentity(t *testing.T) {
	p, c, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/example/source/pulls/5":
			jsonOut(w, map[string]any{"number": 5, "state": "open", "head": map[string]string{"ref": "dev", "sha": "head"}, "base": map[string]string{"ref": "main"}})
		case "/repos/example/source/pulls/5/comments":
			jsonOut(w, []any{
				map[string]any{"id": 1, "body": "[P2] Fix", "user": map[string]string{"login": "chatgpt-codex-connector[bot]", "type": "Bot"}},
				map[string]any{"id": 2, "body": "[P1] Quoted", "user": map[string]string{"login": "human", "type": "User"}},
				map[string]any{"id": 3, "body": "[P1] Reply", "in_reply_to_id": 1, "user": map[string]string{"login": "chatgpt-codex-connector[bot]", "type": "Bot"}},
			})
		case "/repos/example/source/issues/5/comments", "/repos/example/source/pulls/5/reviews":
			jsonOut(w, []any{})
		default:
			t.Errorf("unexpected %s", r.URL.Path)
			w.WriteHeader(404)
		}
	})
	c.Owner = "example"
	result, err := p.PullRequestReview(context.Background(), c, "example/source", 5)
	if err != nil {
		t.Fatal(err)
	}
	if result.Base != "main" || result.Head != "dev" || len(result.Comments) != 1 || result.Comments[0].ID != 1 {
		t.Fatalf("wrong review: %+v", result)
	}
}
