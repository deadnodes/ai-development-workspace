package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"releasecontrol/internal/delivery"
	"strings"
	"testing"
	"time"
)

func TestCachedAuthenticationReadsCannotRefresh(t *testing.T) {
	for _, state := range []string{"cold", "expired", "warm"} {
		t.Run(state, func(t *testing.T) {
			head := strings.Repeat("a", 40)
			raw := "image:\n  repository: ghcr.io/acme/dev\n  digest: sha256:" + strings.Repeat("1", 64) + "\n"
			reads, writes := 0, 0
			p, c, tokens := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					writes++
					http.Error(w, "forbidden", 405)
					return
				}
				reads++
				var v any
				switch {
				case r.URL.Path == "/repositories/101":
					v = map[string]any{"id": 101, "full_name": "acme/source"}
				case strings.HasSuffix(r.URL.Path, "/branches"):
					v = []any{map[string]any{"name": "main", "commit": map[string]string{"sha": head}}}
				case strings.Contains(r.URL.Path, "/compare/"):
					v = map[string]any{"status": "identical", "ahead_by": 0, "behind_by": 0, "base_commit": map[string]string{"sha": head}, "commits": []any{}}
				case strings.Contains(r.URL.Path, "/git/ref/heads/"):
					v = map[string]any{"ref": "refs/heads/main", "object": map[string]string{"type": "commit", "sha": head}}
				case strings.Contains(r.URL.Path, "/contents/"):
					v = map[string]string{"type": "file", "sha": gitOpsBlobSHA([]byte(raw)), "encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(raw))}
				default:
					t.Errorf("unexpected GET %s", r.URL.Path)
					http.NotFound(w, r)
					return
				}
				json.NewEncoder(w).Encode(v)
			})
			if state != "cold" {
				base, _ := apiBase(c)
				key := fmt.Sprintf("%s|%d|%d|%s", base, c.AppID, c.InstallationID, c.PrivateKeyRef)
				expires := time.Now().Add(time.Hour)
				if state == "expired" {
					expires = time.Now().Add(-time.Hour)
				}
				p.tokens[key] = cachedToken{"installation-secret", expires}
			}
			ctx := delivery.WithCachedAuthentication(context.Background())
			_, branchesErr := p.Branches(ctx, c, "101")
			_, compareErr := p.Compare(ctx, c, "101", head, head)
			_, readErr := p.ReadGitOps(ctx, c, delivery.GitOpsRequest{Repository: "101", Ref: "main", Path: "app.yaml", ImageRepository: "ghcr.io/acme/dev"})
			_, revisionErr := p.VerifyGitOpsRevision(ctx, c, delivery.GitOpsRevisionRequest{Repository: "101", Ref: "main", HeadSHA: head, AncestorSHAs: []string{head}})
			for _, e := range []error{branchesErr, compareErr, readErr, revisionErr} {
				if (e == nil) != (state == "warm") {
					t.Fatalf("state=%s err=%v", state, e)
				}
			}
			if writes != 0 || tokens.Load() != 0 || state != "warm" && reads != 0 {
				t.Fatalf("auth mutation on bounded read writes=%d tokens=%d reads=%d", writes, tokens.Load(), reads)
			}
		})
	}
}

func TestGitOpsBatchReaderPinsTokenAndExpiryFromSameSnapshot(t *testing.T) {
	reads := 0
	p, c, tokens := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		reads++
		json.NewEncoder(w).Encode(map[string]string{"value": "ok"})
	})
	base, _ := apiBase(c)
	key := fmt.Sprintf("%s|%d|%d|%s", base, c.AppID, c.InstallationID, c.PrivateKeyRef)
	now := time.Now()
	oldExpiry := now.Add(2 * time.Minute)
	p.tokens[key] = cachedToken{"installation-secret", oldExpiry}
	rotated := false
	// proposalReader calls the clock after releasing its cache snapshot lock.
	// Rotate exactly there to make the formerly split reads deterministic.
	p.now = func() time.Time {
		if !rotated {
			p.mu.Lock()
			p.tokens[key] = cachedToken{"new-session", now.Add(time.Hour)}
			p.mu.Unlock()
			rotated = true
		}
		return now
	}
	r, e := p.batchReader(context.Background(), c)
	if e != nil {
		t.Fatal(e)
	}
	if r.token != "installation-secret" || !r.batchExpires.Equal(oldExpiry) {
		t.Fatalf("token and expiry came from different cache snapshots: token=%q expiry=%s want=%s", r.token, r.batchExpires, oldExpiry)
	}
	now = now.Add(3 * time.Minute)
	var response map[string]string
	if e = r.get("/evidence", &response); e == nil {
		t.Fatal("old expired token admitted using rotated session lifetime")
	}
	if reads != 0 || tokens.Load() != 0 {
		t.Fatalf("expired exact session performed IO: reads=%d refreshes=%d", reads, tokens.Load())
	}
}
