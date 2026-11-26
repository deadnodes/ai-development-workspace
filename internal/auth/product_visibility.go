package auth

import (
	"context"
	"slices"
)

// PrivateProductViewer derives the visibility identity, independently of token
// permissions. Only bootstrap and trusted local execution bypass membership.
func PrivateProductViewer(ctx context.Context) (user string, restricted bool) {
	principal, ok := FromContext(ctx)
	if !ok || principal.Kind == "service" && principal.ID == "service/bootstrap" {
		return "", false
	}
	switch principal.Kind {
	case "human":
		return principal.ID, true
	case "agent":
		return principal.Owner, true
	default:
		return "", true
	}
}

func ProductVisible(ctx context.Context, visibility, owner string, members []string) bool {
	user, restricted := PrivateProductViewer(ctx)
	return visibility != "private" || !restricted || user != "" && (user == owner || slices.Contains(members, user))
}
