package forgejo

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"releasecontrol/internal/buildconfig"
)

// IsAncestor observes commits reachable from ancestor but absent from descendant.
// The commits API does not require the repository's pull-request unit.
func (c *Client) IsAncestor(ctx context.Context, cn buildconfig.Connection, repo, ancestor, descendant string) (bool, error) {
	if !buildconfig.SHA.MatchString(ancestor) || !buildconfig.SHA.MatchString(descendant) {
		return false, errors.New("invalid ancestry SHA")
	}
	if ancestor == descendant {
		return true, nil
	}
	query := url.Values{"sha": {ancestor}, "not": {descendant}, "limit": {"1"}, "stat": {"false"}, "verification": {"false"}, "files": {"false"}}
	var commits []json.RawMessage
	if err := c.request(ctx, cn, "GET", repoPath(repo)+"/commits?"+query.Encode(), nil, &commits); err != nil {
		return false, err
	}
	if commits == nil {
		return false, errors.New("invalid ancestry commits array")
	}
	return len(commits) == 0, nil
}
