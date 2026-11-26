package application

import (
	"errors"
	"testing"

	"releasecontrol/internal/auth"
	"releasecontrol/internal/domain"
)

func TestDeploymentReferenceIsScopeChecked(t *testing.T) {
	command := domain.Command{Action: "record_check_result", ProductID: "allowed", CheckID: "check", Data: map[string]any{"result": "passed", "commit": "abc", "deployment": "deployment-outside"}}
	ids, err := commandReferenceIDs(command)
	if err != nil {
		t.Fatal(err)
	}
	if !projectionContains(ids, "deployment-outside") {
		t.Fatal("deployment reference omitted from projected lookup")
	}

	ctx := auth.WithPrincipal(userCtx("reader", "human", "", "member"), auth.Principal{ID: "reader", Kind: "human", Role: "member", Products: []string{"allowed"}, Permissions: []string{"read", "write"}})
	state := domain.EmptyState()
	state.Products = []domain.Product{{Meta: domain.Meta{ID: "allowed"}}, {Meta: domain.Meta{ID: "outside"}}}
	state.Deployments = []domain.Deployment{{Meta: domain.Meta{ID: "deployment-outside", ProductID: "outside"}}}
	if err := authorizeCommand(ctx, state, command, false); !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("out-of-scope persisted deployment: got %v, want forbidden", err)
	}

	command.Data["deployment"] = "external-label"
	if err := authorizeCommand(ctx, state, command, false); err != nil {
		t.Fatalf("unknown external deployment label rejected: %v", err)
	}
}

func projectionContains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
