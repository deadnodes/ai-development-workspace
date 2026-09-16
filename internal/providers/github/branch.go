package github

import (
	"context"
	"errors"
	"releasecontrol/internal/delivery"
	"strings"
)

func (p *Provider) EnsureRef(ctx context.Context, c delivery.Connection, repository, branch, sha string) (string, error) {
	if !validSourceBranch(branch) || !(strings.HasPrefix(branch, "feature/rcp-") || strings.HasPrefix(branch, "rcp/revisions/")) || !fullSHA.MatchString(sha) {
		return "", errors.New("invalid managed source ref")
	}
	repo, err := p.repository(ctx, c, repository)
	if err != nil {
		return "", err
	}
	actual, err := p.ensureSourceRef(ctx, c, repo, branch, sha)
	if err != nil {
		return "", err
	}
	if actual != sha {
		return "", errors.New("managed ref exists with different commit; overwrite prohibited")
	}
	return actual, nil
}
