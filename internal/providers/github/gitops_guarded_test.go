package github

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"releasecontrol/internal/delivery"
)

func guardedProposalRequest() delivery.GitOpsApply {
	op := "abcdef0123456789"
	return delivery.GitOpsApply{Request: delivery.GitOpsRequest{Repository: "acme/source", Ref: "main", Path: "app.yaml", ImageRepository: "ghcr.io/acme/image"}, ExpectedHeadSHA: strings.Repeat("a", 40), ExpectedBlobSHA: strings.Repeat("c", 40), Digest: "sha256:" + strings.Repeat("d", 64), OperationID: op, Message: "release-control: " + op}
}

func cacheProposalTestToken(t *testing.T, p *Provider, c delivery.Connection) {
	t.Helper()
	base, err := apiBase(c)
	if err != nil {
		t.Fatal(err)
	}
	key := fmt.Sprintf("%s|%d|%d|%s", base, c.AppID, c.InstallationID, c.PrivateKeyRef)
	p.mu.Lock()
	p.tokens[key] = cachedToken{"installation-secret", time.Now().Add(time.Hour)}
	p.mu.Unlock()
}

func TestGuardedGitOpsProposalRechecksBeforeEachHTTPMutation(t *testing.T) {
	for _, scenario := range []string{"none", "ref", "commit", "private-none", "private-ref", "private-commit"} {
		t.Run(scenario, func(t *testing.T) {
			invalidateAfter := strings.TrimPrefix(scenario, "private-")
			req := guardedProposalRequest()
			if strings.HasPrefix(scenario, "private-") {
				req.Request.ImageRepository = "registry.example:5443/team/deep/image"
			}
			branch, head := "rcp/gitops/"+req.OperationID, strings.Repeat("b", 40)
			var allowed atomic.Bool
			allowed.Store(true)
			var guards atomic.Int32
			created, applied, prCreated := false, false, false
			writes := []string{}
			pr := func() map[string]any {
				return map[string]any{"number": 7, "html_url": "https://github.test/pr/7", "state": "open", "head": map[string]string{"ref": branch, "sha": head}, "base": map[string]string{"ref": "main"}}
			}
			p, c, tokens := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				path := r.URL.Path
				if r.Method != http.MethodGet {
					writes = append(writes, r.Method+" "+path)
				}
				switch {
				case strings.Contains(path, "/git/ref/heads/"):
					if !created {
						http.NotFound(w, r)
						return
					}
					sha := req.ExpectedHeadSHA
					if applied {
						sha = head
					}
					jsonOut(w, map[string]any{"ref": "refs/heads/" + branch, "object": map[string]string{"type": "commit", "sha": sha}})
				case strings.HasSuffix(path, "/git/refs"):
					created = true
					if invalidateAfter == "ref" {
						allowed.Store(false)
					}
					jsonOut(w, map[string]string{})
				case strings.Contains(path, "/contents/"):
					if r.Method == http.MethodPut {
						applied = true
						if invalidateAfter == "commit" {
							allowed.Store(false)
						}
						jsonOut(w, map[string]any{"commit": map[string]string{"sha": head}, "content": map[string]string{"sha": req.ExpectedBlobSHA}})
						return
					}
					digest := "sha256:" + strings.Repeat("e", 64)
					if applied {
						digest = req.Digest
					}
					content := "image:\n  repository: " + req.Request.ImageRepository + "\n  digest: " + digest + "\n"
					jsonOut(w, map[string]string{"type": "file", "sha": req.ExpectedBlobSHA, "encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(content))})
				case strings.HasSuffix(path, "/pulls"):
					if r.Method == http.MethodPost {
						prCreated = true
						jsonOut(w, pr())
						return
					}
					rows := []map[string]any{}
					if prCreated {
						rows = append(rows, pr())
					}
					jsonOut(w, rows)
				case strings.HasSuffix(path, "/pulls/7"):
					jsonOut(w, pr())
				default:
					t.Errorf("unexpected %s %s", r.Method, path)
					http.NotFound(w, r)
				}
			})
			cacheProposalTestToken(t, p, c)
			result, err := p.ProposeGitOpsGuarded(context.Background(), c, req, func(context.Context) error {
				guards.Add(1)
				if !allowed.Load() {
					return errors.New("execution invalidated")
				}
				return nil
			})
			wantWrites, wantGuards := 3, int32(3)
			if invalidateAfter == "ref" {
				wantWrites, wantGuards = 1, 2
			}
			if invalidateAfter == "commit" {
				wantWrites, wantGuards = 2, 3
			}
			if len(writes) != wantWrites || guards.Load() != wantGuards {
				t.Fatalf("writes=%v guards=%d", writes, guards.Load())
			}
			if invalidateAfter == "none" {
				if err != nil || result.Number != 7 {
					t.Fatalf("successful guarded proposal %+v %v", result, err)
				}
			} else if err == nil || result.Number != 0 || prCreated {
				t.Fatalf("invalidation ignored: %+v %v writes=%v", result, err, writes)
			}
			if tokens.Load() != 0 {
				t.Fatal("unexpected auth refresh")
			}
		})
	}
}

func TestGuardedGitOpsProposalGuardsAuthenticationAndRejectsNil(t *testing.T) {
	for _, mode := range []string{"nil", "reject-auth", "reject-after-auth"} {
		t.Run(mode, func(t *testing.T) {
			writes := 0
			p, c, tokens := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					writes++
				}
				http.NotFound(w, r)
			})
			calls := 0
			var guard func(context.Context) error
			if mode != "nil" {
				guard = func(context.Context) error {
					calls++
					if mode == "reject-after-auth" && calls == 1 {
						return nil
					}
					return errors.New("revoked")
				}
			}
			_, err := p.ProposeGitOpsGuarded(context.Background(), c, guardedProposalRequest(), guard)
			if err == nil || writes != 0 {
				t.Fatalf("unapproved HTTP: %v writes=%d", err, writes)
			}
			wantTokens, wantCalls := int32(0), 0
			if mode == "reject-auth" {
				wantCalls = 1
			}
			if mode == "reject-after-auth" {
				wantTokens, wantCalls = 1, 2
			}
			if tokens.Load() != wantTokens || calls != wantCalls {
				t.Fatalf("tokens=%d calls=%d", tokens.Load(), calls)
			}
		})
	}
}

func TestGuardedGitOpsHTTPRedirectRechecksBeforePhysicalSend(t *testing.T) {
	var allowed atomic.Bool
	allowed.Store(true)
	var received, guards atomic.Int32
	p, c, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		received.Add(1)
		if r.URL.Path == "/first" {
			allowed.Store(false)
			http.Redirect(w, r, "/second", http.StatusTemporaryRedirect)
			return
		}
		t.Error("redirected mutation reached HTTP server after invalidation")
		jsonOut(w, map[string]string{})
	})
	base, err := apiBase(c)
	if err != nil {
		t.Fatal(err)
	}
	// Production clients already reject redirects. Enable them only on this
	// isolated fixture to prove the wrapper guards each physical send as well.
	p.client.CheckRedirect = nil
	ctx := context.WithValue(context.Background(), gitOpsWriteGuardKey{}, gitOpsWriteGuard(func(context.Context) error {
		guards.Add(1)
		if !allowed.Load() {
			return errors.New("revoked")
		}
		return nil
	}))
	_, _, _, err = p.do(ctx, http.MethodPost, base+"/first", "installation-secret", map[string]string{"intent": "test"})
	if err == nil || received.Load() != 1 || guards.Load() != 2 {
		t.Fatalf("err=%v HTTP=%d guards=%d", err, received.Load(), guards.Load())
	}
}

func TestGuardedGitOpsRejectedSendPoisonsCompoundContext(t *testing.T) {
	var received atomic.Int32
	p, c, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		received.Add(1)
		jsonOut(w, map[string]string{})
	})
	base, err := apiBase(c)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	guard := latchGitOpsGuard(func(context.Context) error {
		calls++
		if calls == 2 {
			return errors.New("revoked once")
		}
		return nil
	})
	ctx := context.WithValue(context.Background(), gitOpsWriteGuardKey{}, guard)
	for i := 0; i < 3; i++ {
		_, _, _, err := p.do(ctx, http.MethodPost, base+"/mutation", "installation-secret", map[string]string{})
		if (i == 0 && err != nil) || (i > 0 && err == nil) {
			t.Fatalf("send %d error=%v", i, err)
		}
	}
	if received.Load() != 1 || calls != 2 {
		t.Fatalf("HTTP=%d callbacks=%d", received.Load(), calls)
	}
}

func TestGuardedGitOpsContextDoesNotAffectConcurrentLegacyProposal(t *testing.T) {
	req := guardedProposalRequest()
	branch, head := "rcp/gitops/"+req.OperationID, strings.Repeat("b", 40)
	created, applied, prCreated := false, false, false
	writes := 0
	pr := func() map[string]any {
		return map[string]any{"number": 7, "html_url": "https://github.test/pr/7", "state": "open", "head": map[string]string{"ref": branch, "sha": head}, "base": map[string]string{"ref": "main"}}
	}
	p, c, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writes++
		}
		switch {
		case strings.Contains(r.URL.Path, "/git/ref/heads/"):
			if !created {
				http.NotFound(w, r)
				return
			}
			sha := req.ExpectedHeadSHA
			if applied {
				sha = head
			}
			jsonOut(w, map[string]any{"ref": "refs/heads/" + branch, "object": map[string]string{"type": "commit", "sha": sha}})
		case strings.HasSuffix(r.URL.Path, "/git/refs"):
			created = true
			jsonOut(w, map[string]string{})
		case strings.Contains(r.URL.Path, "/contents/"):
			if r.Method == http.MethodPut {
				applied = true
				jsonOut(w, map[string]any{"commit": map[string]string{"sha": head}, "content": map[string]string{"sha": req.ExpectedBlobSHA}})
				return
			}
			content := "image:\n  repository: ghcr.io/acme/image\n  digest: sha256:" + strings.Repeat("e", 64) + "\n"
			jsonOut(w, map[string]string{"type": "file", "sha": req.ExpectedBlobSHA, "encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(content))})
		case strings.HasSuffix(r.URL.Path, "/pulls"):
			if r.Method == http.MethodPost {
				prCreated = true
				jsonOut(w, pr())
				return
			}
			rows := []map[string]any{}
			if prCreated {
				rows = append(rows, pr())
			}
			jsonOut(w, rows)
		case strings.HasSuffix(r.URL.Path, "/pulls/7"):
			jsonOut(w, pr())
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	})
	cacheProposalTestToken(t, p, c)
	entered, release := make(chan struct{}), make(chan struct{})
	guardedDone := make(chan error, 1)
	go func() {
		_, err := p.ProposeGitOpsGuarded(context.Background(), c, req, func(context.Context) error { close(entered); <-release; return errors.New("revoked") })
		guardedDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("guard not reached")
	}
	legacyDone := make(chan error, 1)
	go func() {
		result, err := p.ProposeGitOps(context.Background(), c, req)
		if err == nil && result.Number != 7 {
			err = fmt.Errorf("unexpected PR %d", result.Number)
		}
		legacyDone <- err
	}()
	select {
	case err := <-legacyDone:
		if err != nil {
			t.Errorf("legacy affected by guarded call: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("legacy proposal blocked by private guard")
	}
	close(release)
	select {
	case err := <-guardedDone:
		if err == nil {
			t.Error("revoked guarded proposal succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("guarded call did not finish")
	}
	if writes != 3 {
		t.Fatalf("HTTP writes=%d; expected only legacy ref/commit/PR", writes)
	}
}
