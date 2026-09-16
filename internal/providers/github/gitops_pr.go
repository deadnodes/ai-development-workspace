package github

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"releasecontrol/internal/delivery"
)

type gitOpsPR struct {
	Number int    `json:"number"`
	URL    string `json:"html_url"`
	State  string `json:"state"`
	Merged bool   `json:"merged"`
	Head   struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

func prResult(pr gitOpsPR) delivery.GitOpsPullRequest {
	return delivery.GitOpsPullRequest{Number: pr.Number, URL: pr.URL, State: pr.State, HeadSHA: pr.Head.SHA, Merged: pr.Merged}
}
func (p *Provider) ProposeGitOps(ctx context.Context, c delivery.Connection, r delivery.GitOpsApply) (delivery.GitOpsPullRequest, error) {
	var out delivery.GitOpsPullRequest
	if !operationID.MatchString(r.OperationID) || !fullSHA.MatchString(r.ExpectedHeadSHA) {
		return out, errors.New("pinned operation/base required")
	}
	repo, err := p.repository(ctx, c, r.Request.Repository)
	if err != nil {
		return out, err
	}
	branch := "rcp/gitops/" + r.OperationID
	head, err := p.ensureSourceRef(ctx, c, repo, branch, r.ExpectedHeadSHA)
	if err != nil {
		return out, err
	}
	req := r
	req.Request.Ref = branch
	req.Request.ExpectedHeadSHA = ""
	current, err := p.ReadGitOps(ctx, c, req.Request)
	if err != nil {
		return out, err
	}
	if head != r.ExpectedHeadSHA {
		if current.Digest != r.Digest || current.ImageRepository != r.Request.ImageRepository {
			return out, ErrConflict
		}
		var commit struct {
			Message string `json:"message"`
			Parents []struct {
				SHA string `json:"sha"`
			} `json:"parents"`
		}
		if e := p.request(ctx, c, "GET", "/repos/"+repo+"/git/commits/"+head, nil, &commit); e != nil {
			return out, e
		}
		if len(commit.Parents) != 1 || commit.Parents[0].SHA != r.ExpectedHeadSHA || commit.Message != r.Message+" [rcp:"+r.OperationID+"]" {
			return out, errors.New("proposal branch changed outside this operation")
		}
	} else {
		if _, err = p.ApplyGitOps(ctx, c, req); err != nil {
			return out, err
		}
	}
	var prs []gitOpsPR
	if err = p.request(ctx, c, "GET", "/repos/"+repo+"/pulls?state=all&head="+url.QueryEscape(c.Owner+":"+branch)+"&base="+url.QueryEscape(r.Request.Ref), nil, &prs); err != nil {
		return out, err
	}
	if len(prs) > 0 {
		return p.ObserveGitOpsPR(ctx, c, r, prResult(prs[0]))
	}
	var pr gitOpsPR
	err = p.request(ctx, c, "POST", "/repos/"+repo+"/pulls", map[string]any{"title": "Release Control Plane " + r.OperationID, "head": branch, "base": r.Request.Ref, "body": fmt.Sprintf("Operation %s\nPinned source/base %s\nImage %s@%s\nOnly configured GitOps image fields are changed. Merge requires external review.", r.OperationID, r.ExpectedHeadSHA, r.Request.ImageRepository, r.Digest)}, &pr)
	if err != nil {
		return out, err
	}
	return p.ObserveGitOpsPR(ctx, c, r, prResult(pr))
}
func (p *Provider) ObserveGitOpsPR(ctx context.Context, c delivery.Connection, r delivery.GitOpsApply, expected delivery.GitOpsPullRequest) (delivery.GitOpsPullRequest, error) {
	var out delivery.GitOpsPullRequest
	repo, err := p.repository(ctx, c, r.Request.Repository)
	if err != nil {
		return out, err
	}
	if expected.Number <= 0 {
		return out, errors.New("PR number required")
	}
	var pr gitOpsPR
	if err = p.request(ctx, c, "GET", fmt.Sprintf("/repos/%s/pulls/%d", repo, expected.Number), nil, &pr); err != nil {
		return out, err
	}
	if pr.Head.Ref != "rcp/gitops/"+r.OperationID || pr.Base.Ref != r.Request.Ref || pr.Head.SHA != expected.HeadSHA || !fullSHA.MatchString(pr.Head.SHA) {
		return out, ErrConflict
	}
	return prResult(pr), nil
}
