package github

import (
	"errors"
	"strings"

	"releasecontrol/internal/domain"
)

// GitOps renders registry identity as data. Artifact inspection keeps its
// separate GHCR-only validator and never uses this helper for network access.
func gitopsImageRepository(raw string) (string, error) {
	host, _, _ := strings.Cut(raw, "/")
	if strings.ContainsAny(host, ".:") || host == "localhost" {
		canonical, err := domain.CanonicalImageRepository(raw)
		if err != nil || canonical != raw {
			return "", errors.New("GitOps requires an exact canonical image repository")
		}
		return canonical, nil
	}
	// Preserve the existing unqualified GHCR owner/image spelling only.
	repository, err := registryRepository(raw)
	if err != nil {
		return "", err
	}
	return "ghcr.io/" + repository, nil
}
