package domain

import "context"

// Provider interfaces are read-only seams. No operation here modifies infrastructure.
type GitProvider interface {
	Repositories(context.Context) ([]Repository, error)
	Branch(context.Context, string, string) (Branch, error)
	Commit(context.Context, string, string) (Commit, error)
	PullRequest(context.Context, string, string) (PullRequest, error)
}
type FluxProvider interface {
	ReconciliationStatus(context.Context, string) (map[string]any, error)
}
type KubernetesProvider interface {
	RuntimeStatus(context.Context, string) (map[string]any, error)
}
