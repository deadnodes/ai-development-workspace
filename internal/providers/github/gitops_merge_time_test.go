package github

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"releasecontrol/internal/delivery"
)

func TestGitOpsPRObservationPreservesAuthoritativeMergeTime(t *testing.T) {
	for _, mode := range []string{"timestamp", "missing", "wrong-head"} {
		t.Run(mode, func(t *testing.T) {
			sha := strings.Repeat("a", 40)
			at := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
			p, c, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != "/repos/example/gitops/pulls/7" {
					t.Errorf("unexpected API %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
					return
				}
				head := sha
				if mode == "wrong-head" {
					head = strings.Repeat("b", 40)
				}
				v := map[string]any{"number": 7, "html_url": "https://github.test/example/gitops/pull/7", "state": "closed", "merged": true, "head": map[string]string{"sha": head, "ref": "rcp/gitops/abcdef0123456789"}, "base": map[string]string{"ref": "main"}}
				if mode != "missing" {
					v["merged_at"] = at.Format(time.RFC3339)
				}
				jsonOut(w, v)
			})
			c.Owner = "example"
			got, err := p.ObserveGitOpsPR(context.Background(), c, delivery.GitOpsApply{OperationID: "abcdef0123456789", Request: delivery.GitOpsRequest{Repository: "example/gitops", Ref: "main"}}, delivery.GitOpsPullRequest{Number: 7, HeadSHA: sha})
			if mode == "wrong-head" {
				if err == nil {
					t.Fatal("foreign head supplied merge timestamp")
				}
				return
			}
			if err != nil || !got.Merged {
				t.Fatalf("merged observation failed %+v %v", got, err)
			}
			if mode == "timestamp" && (got.MergedAt == nil || !got.MergedAt.Equal(at)) {
				t.Fatal("authoritative provider merge timestamp lost", got)
			}
			if mode == "missing" && got.MergedAt != nil {
				t.Fatal("missing timestamp was invented", got)
			}
		})
	}
}
