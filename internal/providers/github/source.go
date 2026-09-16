package github

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"releasecontrol/internal/delivery"
)

var _ delivery.SourceProvider = (*Provider)(nil)

func validSourceBranch(s string) bool {
	if s == "" || s == "@" || strings.HasPrefix(s, "-") || len(s) > 200 || strings.ContainsAny(s, " ~^:?*[\\\t\r\n") || strings.Contains(s, "..") || strings.Contains(s, "@{") || strings.HasSuffix(s, "/") || strings.Contains(s, "//") {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	for _, part := range strings.Split(s, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}
func (p *Provider) sourceRef(ctx context.Context, c delivery.Connection, repo, branch string) (string, error) {
	var out struct {
		Ref    string `json:"ref"`
		Object struct {
			SHA  string `json:"sha"`
			Type string `json:"type"`
		} `json:"object"`
	}
	err := p.request(ctx, c, "GET", "/repos/"+repo+"/git/ref/heads/"+url.PathEscape(branch), nil, &out)
	if err != nil {
		return "", err
	}
	if out.Ref != "refs/heads/"+branch || out.Object.Type != "commit" || !fullSHA.MatchString(out.Object.SHA) {
		return "", errors.New("invalid source ref identity")
	}
	return out.Object.SHA, nil
}
func (p *Provider) ensureSourceRef(ctx context.Context, c delivery.Connection, repo, branch, sha string) (string, error) {
	current, err := p.sourceRef(ctx, c, repo, branch)
	if err == nil {
		return current, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return "", err
	}
	// Read after even an uncertain create response: only the expected SHA is accepted.
	err = p.request(ctx, c, "POST", "/repos/"+repo+"/git/refs", map[string]any{"ref": "refs/heads/" + branch, "sha": sha}, nil)
	current, readErr := p.sourceRef(ctx, c, repo, branch)
	if readErr != nil {
		if err != nil {
			return "", err
		}
		return "", readErr
	}
	return current, nil
}
func (p *Provider) sourceAncestor(ctx context.Context, c delivery.Connection, repo, base, head string) (bool, error) {
	if base == head {
		return true, nil
	}
	result, err := p.Compare(ctx, c, repo, base, head)
	return err == nil && result.Behind == 0 && (result.Status == "ahead" || result.Status == "identical"), err
}
func (p *Provider) proveSourceStep(ctx context.Context, c delivery.Connection, repo, current, previous, head, message string) (bool, error) {
	if current == previous {
		return p.sourceAncestor(ctx, c, repo, head, previous)
	}
	if current == head {
		return p.sourceAncestor(ctx, c, repo, previous, head)
	}
	var commit struct {
		SHA     string `json:"sha"`
		Message string `json:"message"`
		Parents []struct {
			SHA string `json:"sha"`
		} `json:"parents"`
	}
	if err := p.request(ctx, c, "GET", "/repos/"+repo+"/git/commits/"+current, nil, &commit); err != nil {
		return false, err
	}
	return commit.SHA == current && commit.Message == message && len(commit.Parents) == 2 && commit.Parents[0].SHA == previous && commit.Parents[1].SHA == head, nil
}

// ComposeSource only invokes server-side Git merges. Each step has its own ref,
// allowing a retry to prove the exact previous/head parents even after a lost response.
// A tampered branch is rejected instead of reset or force-pushed.
func (p *Provider) ComposeSource(ctx context.Context, c delivery.Connection, plan delivery.SourcePlan) (delivery.SourceResult, error) {
	out := delivery.SourceResult{Branch: plan.TargetBranch}
	if !validSourceBranch(plan.TargetBranch) || !strings.HasPrefix(plan.TargetBranch, "generated/") || !validSourceBranch(plan.BaseRef) || strings.HasPrefix(plan.BaseRef, "generated/") || plan.OperationID == "" || !fullSHA.MatchString(plan.BaseSHA) || len(plan.HeadSHAs) > 20 {
		return out, errors.New("invalid bounded source composition plan")
	}
	for _, head := range plan.HeadSHAs {
		if !fullSHA.MatchString(head) {
			return out, errors.New("composition requires full source SHAs")
		}
	}
	repo, err := p.repository(ctx, c, plan.Repository)
	if err != nil {
		return out, err
	}
	encoded, _ := json.Marshal(plan)
	fingerprint := fmt.Sprintf("%x", sha256.Sum256(encoded))
	previous := plan.BaseSHA
	for i, head := range plan.HeadSHAs {
		branch := fmt.Sprintf("%s-step-%d", plan.TargetBranch, i+1)
		message := fmt.Sprintf("Release Control Plane composition %s step %d", fingerprint, i+1)
		current, err := p.ensureSourceRef(ctx, c, repo, branch, previous)
		if err != nil {
			return out, err
		}
		if current == previous {
			already, err := p.sourceAncestor(ctx, c, repo, head, previous)
			if err != nil {
				return out, err
			}
			if !already {
				mergeErr := p.request(ctx, c, "POST", "/repos/"+repo+"/merges", map[string]string{"base": branch, "head": head, "commit_message": message}, nil)
				current, err = p.sourceRef(ctx, c, repo, branch)
				if err != nil {
					return out, err
				}
				proven, err := p.proveSourceStep(ctx, c, repo, current, previous, head, message)
				if err != nil {
					return out, err
				}
				if !proven {
					var api *APIError
					if errors.As(mergeErr, &api) && api.Status == 409 && current == previous {
						out.Conflict = true
						out.Detail = fmt.Sprintf("merge conflict at source step %d (%s)", i+1, head)
						return out, nil
					}
					if mergeErr != nil {
						return out, mergeErr
					}
					return out, errors.New("generated source changed outside pinned composition")
				}
			}
		} else {
			proven, err := p.proveSourceStep(ctx, c, repo, current, previous, head, message)
			if err != nil {
				return out, err
			}
			if !proven {
				return out, errors.New("existing generated branch does not match pinned composition")
			}
		}
		previous = current
	}
	current, err := p.ensureSourceRef(ctx, c, repo, plan.TargetBranch, previous)
	if err != nil {
		return out, err
	}
	if current != previous {
		return out, errors.New("generated target has unexpected source history")
	}
	out.SHA = current
	return out, nil
}

// AdvanceMain cannot bypass branch protection and never forces history. GitHub
// refs offers no atomic expected-SHA compare-and-swap; preflight plus force:false
// rejects any concurrent history not contained in the candidate.
func (p *Provider) AdvanceMain(ctx context.Context, c delivery.Connection, repository, baseRef, expectedBaseSHA, candidateSHA string) (delivery.SourceResult, error) {
	out := delivery.SourceResult{Branch: baseRef}
	if !validSourceBranch(baseRef) || strings.HasPrefix(baseRef, "generated/") || !fullSHA.MatchString(expectedBaseSHA) || !fullSHA.MatchString(candidateSHA) {
		return out, errors.New("invalid pinned main advancement")
	}
	repo, err := p.repository(ctx, c, repository)
	if err != nil {
		return out, err
	}
	current, err := p.sourceRef(ctx, c, repo, baseRef)
	if err != nil {
		return out, err
	}
	if current == candidateSHA {
		out.SHA = current
		return out, nil
	}
	if current != expectedBaseSHA {
		return out, errors.New("main moved since release was planned")
	}
	safe, err := p.sourceAncestor(ctx, c, repo, expectedBaseSHA, candidateSHA)
	if err != nil {
		return out, err
	}
	if !safe {
		return out, errors.New("release candidate is not a fast-forward of main")
	}
	updateErr := p.request(ctx, c, "PATCH", "/repos/"+repo+"/git/refs/heads/"+url.PathEscape(baseRef), map[string]any{"sha": candidateSHA, "force": false}, nil)
	current, err = p.sourceRef(ctx, c, repo, baseRef)
	if err != nil {
		return out, err
	}
	if current != candidateSHA {
		if updateErr != nil {
			return out, updateErr
		}
		return out, errors.New("main differs from release candidate after update")
	}
	out.SHA = current
	return out, nil
}
