package forgejo

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestCommitTimeWithoutSocket(t *testing.T) {
	sha := strings.Repeat("a", 40)
	for _, same := range []bool{true, false} {
		c, cn := inMemoryFixture(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "GET" || r.URL.Path != "/api/v1/repos/o/r/git/commits/"+sha {
				t.Errorf("unexpected request %s", r.URL)
			}
			result := sha
			if !same {
				result = strings.Repeat("b", 40)
			}
			fmt.Fprintf(w, `{"sha":%q,"committer":{"date":"2026-11-15T02:00:00Z"}}`, result)
		})
		at, err := c.CommitTime(context.Background(), cn, "o/r", sha)
		if same && (err != nil || !at.Equal(time.Date(2026, 11, 15, 2, 0, 0, 0, time.UTC))) {
			t.Fatal(at, err)
		}
		if !same && err == nil {
			t.Fatal("mismatched commit accepted")
		}
	}
}
