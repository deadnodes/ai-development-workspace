package application

import (
	"context"
	"errors"
	"testing"

	"releasecontrol/internal/auth"
	"releasecontrol/internal/domain"
)

func TestStatePageAuthorizationDoesNotReadGlobalState(t *testing.T) {
	store := &pageStore{cards: []domain.StateCard{{ID: "private", Name: "private"}}}
	service := New(store)
	if _, err := service.StatePage(agentContext("write"), domain.StatePageRequest{}); !errors.Is(err, auth.ErrForbidden) || store.calls != 0 {
		t.Fatal("missing read permission accessed metadata")
	}
	if _, err := service.StatePage(agentContext("read"), domain.StatePageRequest{ProductID: "private"}); !errors.Is(err, auth.ErrForbidden) || store.calls != 0 {
		t.Fatal("foreign explicit scope accessed metadata")
	}
	// A defective metadata reader must not turn a trusted restriction into a grant.
	if _, err := service.StatePage(agentContext("read"), domain.StatePageRequest{}); !errors.Is(err, auth.ErrForbidden) || !store.query.RestrictProducts || len(store.query.AllowedProductIDs) != 1 || store.query.AllowedProductIDs[0] != "p" {
		t.Fatal("untrusted metadata escaped the application authorization boundary")
	}
	store.cards = nil
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{ID: "agent/empty", Kind: "agent", Permissions: []string{"read"}})
	// The spy intentionally returns out-of-scope parents; validation fails closed.
	if _, err := service.StatePage(ctx, domain.StatePageRequest{}); !errors.Is(err, auth.ErrForbidden) || !store.query.RestrictProducts || len(store.query.AllowedProductIDs) != 0 {
		t.Fatal("empty authorization scope became unrestricted")
	}
}
