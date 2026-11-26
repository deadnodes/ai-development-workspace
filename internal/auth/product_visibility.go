package auth

import (
	"context"
	"slices"
)

// PrivateProductViewer derives the membership identity independently of grants.
func PrivateProductViewer(ctx context.Context) (user string, restricted bool) {
	principal, ok := FromContext(ctx)
	if !ok {
		return "", false
	}
	switch principal.Kind {
	case "service", "worker":
		return "", false
	case "human":
		return principal.ID, true
	case "agent":
		return principal.Owner, true
	default:
		return "", true
	}
}

// RequiresExistingProduct preserves the separate boundary for retained rows.
func RequiresExistingProduct(ctx context.Context) bool {
	principal, ok := FromContext(ctx)
	return ok && !(principal.Kind == "service" && principal.ID == "service/bootstrap")
}

func ProductVisible(ctx context.Context, visibility, owner string, members []string) bool {
	user, restricted := PrivateProductViewer(ctx)
	return visibility != "private" || !restricted || user != "" && (user == owner || slices.Contains(members, user))
}
