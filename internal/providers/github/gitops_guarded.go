package github

import (
	"context"
	"errors"
	"net/http"
	"sync"

	"releasecontrol/internal/delivery"
)

var _ delivery.GitOpsGuardedPRProvider = (*Provider)(nil)

type gitOpsWriteGuardKey struct{}
type gitOpsWriteGuard func(context.Context) error

func (p *Provider) ProposeGitOpsGuarded(ctx context.Context, c delivery.Connection, r delivery.GitOpsApply, guard func(context.Context) error) (delivery.GitOpsPullRequest, error) {
	if guard == nil {
		return delivery.GitOpsPullRequest{}, errors.New("GitOps proposal requires an execution guard")
	}
	return p.ProposeGitOps(context.WithValue(ctx, gitOpsWriteGuardKey{}, latchGitOpsGuard(guard)), c, r)
}

// A rejected send poisons this compound invocation even if subsequent reads
// discover an existing ref and legacy recovery continues internally.
func latchGitOpsGuard(guard func(context.Context) error) gitOpsWriteGuard {
	var mu sync.Mutex
	var failure error
	return func(ctx context.Context) error {
		mu.Lock()
		defer mu.Unlock()
		if failure != nil {
			return failure
		}
		failure = guard(ctx)
		return failure
	}
}

// A per-request wrapper also covers redirected HTTP sends. Copying the client
// leaves parallel legacy callers and the shared Transport unchanged.
type guardedGitOpsTransport struct {
	base  http.RoundTripper
	guard gitOpsWriteGuard
}

func (t guardedGitOpsTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		if err := t.guard(r.Context()); err != nil {
			return nil, err
		}
	}
	return t.base.RoundTrip(r)
}

func (p *Provider) guardedClient(ctx context.Context) *http.Client {
	guard, ok := ctx.Value(gitOpsWriteGuardKey{}).(gitOpsWriteGuard)
	if !ok {
		return p.client
	}
	client := *p.client
	base := client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	client.Transport = guardedGitOpsTransport{base: base, guard: guard}
	return &client
}
