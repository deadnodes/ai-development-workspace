package forgejo

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"releasecontrol/internal/buildconfig"
)

// PR is the source provenance returned for a commit's pull request.
type PR struct {
	Number                           int
	Merged                           bool
	BaseRef, HeadSHA, MergeCommitSHA string
}

func (c *Client) CommitPullRequest(ctx context.Context, cn buildconfig.Connection, repo, sha string) (PR, bool, error) {
	if !buildconfig.SHA.MatchString(sha) {
		return PR{}, false, errors.New("invalid PR commit SHA")
	}
	var v struct {
		Number int  `json:"number"`
		Merged bool `json:"merged"`
		Base   struct {
			Ref string `json:"ref"`
		} `json:"base"`
		Head struct {
			SHA string `json:"sha"`
		} `json:"head"`
		MergeCommitSHA string `json:"merge_commit_sha"`
	}
	err := c.request(ctx, cn, "GET", repoPath(repo)+"/commits/"+url.PathEscape(sha)+"/pull", nil, &v)
	var api *APIError
	if errors.As(err, &api) && api.Status == http.StatusNotFound {
		return PR{}, false, nil
	}
	if err != nil {
		return PR{}, false, err
	}
	if v.Number <= 0 || !buildconfig.SHA.MatchString(v.Head.SHA) || !buildconfig.Branch(v.Base.Ref) {
		return PR{}, false, errors.New("invalid PR evidence")
	}
	return PR{v.Number, v.Merged, v.Base.Ref, v.Head.SHA, v.MergeCommitSHA}, true, nil
}

type treeEntry struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
	Type string `json:"type"`
	SHA  string `json:"sha"`
}

// TreesEqual compares complete top-level Merkle entries, never Forgejo's tree SHA
// (which some versions incorrectly populate with the commit SHA).
func (c *Client) TreesEqual(ctx context.Context, cn buildconfig.Connection, repo, a, b string) (bool, error) {
	if !buildconfig.SHA.MatchString(a) || !buildconfig.SHA.MatchString(b) {
		return false, errors.New("invalid tree commit SHA")
	}
	var trees [2][]treeEntry
	for i, sha := range []string{a, b} {
		var v struct {
			Tree       []treeEntry `json:"tree"`
			Truncated  *bool       `json:"truncated"`
			TotalCount *int        `json:"total_count"`
		}
		if err := c.request(ctx, cn, "GET", repoPath(repo)+"/git/trees/"+url.PathEscape(sha)+"?per_page=1000", nil, &v); err != nil {
			return false, err
		}
		if v.Truncated == nil || *v.Truncated || v.TotalCount == nil || *v.TotalCount > 1000 || *v.TotalCount != len(v.Tree) {
			return false, nil
		}
		slices.SortFunc(v.Tree, func(a, b treeEntry) int { return strings.Compare(a.Path, b.Path) })
		for j, e := range v.Tree {
			if e.Path == "" || strings.Contains(e.Path, "/") || !buildconfig.SHA.MatchString(e.SHA) || (e.Type != "blob" && e.Type != "tree" && e.Type != "commit") || e.Mode == "" || j > 0 && v.Tree[j-1].Path == e.Path {
				return false, nil
			}
		}
		trees[i] = v.Tree
	}
	return slices.Equal(trees[0], trees[1]), nil
}
