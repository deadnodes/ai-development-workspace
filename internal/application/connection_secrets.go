package application

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"releasecontrol/internal/domain"
)

// Diagnostic only: credential readers retain their own enforcement. Resolve
// symlinks as github.secret does; missing paths still permit a lexical warning.
func connectionSecretOutsideDir(ref string) bool {
	if !strings.HasPrefix(ref, "file:") {
		return false
	}
	path := strings.TrimPrefix(ref, "file:")
	if !filepath.IsAbs(path) {
		return false
	}
	root := os.Getenv("RCP_SECRET_DIR")
	if root == "" {
		root = "/run/secrets"
	}
	path = diagnosticSecretPath(path)
	root = diagnosticSecretPath(root)
	rel, err := filepath.Rel(root, path)
	return err == nil && (rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// Resolve existing ancestors too, so an absent file beneath /var (a symlink
// on macOS) or another directory alias is compared to the same canonical root.
func diagnosticSecretPath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	suffix := ""
	for {
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			return filepath.Join(resolved, suffix)
		}
		parent := filepath.Dir(path)
		if parent == path {
			return filepath.Join(path, suffix)
		}
		suffix = filepath.Join(filepath.Base(path), suffix)
		path = parent
	}
}

func connectionSecretWarnings(st *domain.State, product string) []map[string]any {
	items := []map[string]any{}
	for _, c := range st.GitHubConnections {
		visible := product == "" || c.ProductID == product
		for _, grant := range st.ConnectionGrants {
			visible = visible || grant.ProductID == product && grant.ConnectionID == c.ID
		}
		if !visible {
			continue
		}
		paths := []string{}
		for _, ref := range []string{c.Config.PrivateKeyRef, c.Config.RegistryCredentialRef} {
			if connectionSecretOutsideDir(ref) {
				path := strings.TrimPrefix(ref, "file:")
				if len(paths) == 0 || paths[0] != path {
					paths = append(paths, path)
				}
			}
		}
		if len(paths) > 0 {
			items = append(items, map[string]any{"id": "connection-secret-" + c.ID, "reason": "CONNECTION_SECRET_OUTSIDE_DIR", "connection_id": c.ID, "name": c.Name, "paths": paths})
		}
	}
	return items
}

// CheckConnectionSecrets runs once at startup, without reading secret contents.
func (s *Service) CheckConnectionSecrets(ctx context.Context) error {
	warn := func(st *domain.State) error {
		for _, item := range connectionSecretWarnings(st, "") {
			slog.Warn("connection secret outside configured directory", "connection_id", item["connection_id"], "name", item["name"], "paths", item["paths"])
		}
		return nil
	}
	if reader, ok := s.store.(interface {
		ReadGitHubConnections(context.Context) ([]domain.GitHubConnection, error)
	}); ok {
		connections, err := reader.ReadGitHubConnections(ctx)
		if err != nil {
			return err
		}
		return warn(&domain.State{GitHubConnections: connections})
	}
	return s.view(ctx, warn)
}

// Collapse only refresh failures with the specific confinement error and a
// matching connection. Other failures remain actionable; audit is untouched.
func connectionSecretRefreshFailures(st *domain.State, product string, warnings []map[string]any) map[string]bool {
	affected := map[string]bool{}
	for _, warning := range warnings {
		affected[warning["connection_id"].(string)] = true
	}
	repositories := map[string]bool{}
	for _, binding := range st.RepositoryBindings {
		if binding.ProductID == product && affected[binding.ConnectionID] {
			repositories[binding.RepositoryID] = true
		}
	}
	suppressed := map[string]bool{}
	for _, op := range st.Operations {
		if op.ProductID != product || op.Status != "FAILED" || (op.Kind != "REFRESH_GIT" && op.Kind != "REFRESH_REPOSITORY_GIT") || !strings.Contains(op.Detail, "secret file outside configured directory") {
			continue
		}
		matches := repositories[op.RepositoryID]
		if in := integration(st, op.IntegrationID); in != nil && in.ProductID == product {
			for _, branch := range in.Branches {
				matches = matches || repositories[branch.RepositoryID]
			}
			for _, pr := range in.PullRequests {
				matches = matches || repositories[pr.RepositoryID]
			}
		}
		if matches {
			suppressed[op.ID] = true
		}
	}
	return suppressed
}
