package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"releasecontrol/internal/delivery"
	"strconv"
	"strings"
)

func decodeBatchResponse(raw []byte, out any) error {
	if json.Unmarshal(raw, out) != nil {
		return errors.New("invalid batch response")
	}
	return nil
}

func (p *Provider) LookupGitOpsBatchProposal(ctx context.Context, c delivery.Connection, b delivery.GitOpsBatchApply) (delivery.GitOpsPullRequest, error) {
	return p.observeGitOpsBatch(ctx, c, b, delivery.GitOpsPullRequest{}, false)
}
func (p *Provider) ObserveGitOpsBatchPR(ctx context.Context, c delivery.Connection, b delivery.GitOpsBatchApply, expected delivery.GitOpsPullRequest) (delivery.GitOpsPullRequest, error) {
	if expected.Number <= 0 || !fullSHA.MatchString(expected.HeadSHA) || expected.URL == "" {
		return delivery.GitOpsPullRequest{}, ErrConflict
	}
	return p.observeGitOpsBatch(ctx, c, b, expected, true)
}
func (p *Provider) observeGitOpsBatch(ctx context.Context, c delivery.Connection, b delivery.GitOpsBatchApply, expected delivery.GitOpsPullRequest, allowMissing bool) (delivery.GitOpsPullRequest, error) {
	return p.observeGitOpsBatchReader(ctx, c, b, expected, allowMissing, nil)
}
func (p *Provider) observeGitOpsBatchReader(ctx context.Context, c delivery.Connection, b delivery.GitOpsBatchApply, expected delivery.GitOpsPullRequest, allowMissing bool, r *proposalReader) (delivery.GitOpsPullRequest, error) {
	var out delivery.GitOpsPullRequest
	if e := validateGitOpsBatch(b); e != nil {
		return out, e
	}
	var e error
	if r == nil {
		r, e = p.batchReader(ctx, c)
		if e != nil {
			return out, e
		}
	}
	repo, e := r.batchRepository(c, b)
	if e != nil {
		return out, e
	}
	id, _ := strconv.ParseInt(b.Repository, 10, 64)
	branch := "rcp/gitops/" + b.OperationID
	var pr lookupPR
	if expected.Number > 0 {
		if e = r.get(fmt.Sprintf("/repos/%s/pulls/%d", repo, expected.Number), &pr); e != nil {
			return out, e
		}
	} else {
		q := url.Values{"state": {"all"}, "head": {strings.Split(repo, "/")[0] + ":" + branch}, "base": {b.Ref}, "per_page": {"2"}, "page": {"1"}}
		var prs []lookupPR
		if e = r.get("/repos/"+repo+"/pulls?"+q.Encode(), &prs); e != nil {
			return out, e
		}
		if len(prs) != 1 || prs[0].Number <= 0 {
			return out, errors.New("proposal absent or ambiguous; unresolved")
		}
		pr = prs[0]
	}
	valid := func(v lookupPR) bool {
		return v.Number > 0 && v.Head.Ref == branch && fullSHA.MatchString(v.Head.SHA) && v.Head.SHA != b.ExpectedHeadSHA && v.Base.Ref == b.Ref && v.Head.Repo.ID == id && v.Base.Repo.ID == id && strings.EqualFold(v.Head.Repo.FullName, repo) && strings.EqualFold(v.Base.Repo.FullName, repo) && (v.State == "open" || v.State == "closed") && (!v.Merged || v.State == "closed") && v.URL != "" && len(v.URL) <= 2048
	}
	if !valid(pr) || (expected.Number > 0 && (pr.Number != expected.Number || pr.Head.SHA != expected.HeadSHA || pr.URL != expected.URL)) {
		return out, ErrConflict
	}
	head := pr.Head.SHA
	checkHead := func() error {
		actual, e := r.batchHead(repo, branch)
		if e != nil {
			var api *APIError
			if allowMissing && expected.Number > 0 && pr.State == "closed" && errors.As(e, &api) && api.Status == http.StatusNotFound {
				return nil
			}
			return e
		}
		if actual != head {
			return ErrConflict
		}
		return nil
	}
	if e = checkHead(); e != nil {
		return out, e
	}
	var base, commit proposalCommit
	if e = r.get("/repos/"+repo+"/git/commits/"+b.ExpectedHeadSHA, &base); e != nil {
		return out, e
	}
	if e = r.get("/repos/"+repo+"/git/commits/"+head, &commit); e != nil {
		return out, e
	}
	if base.SHA != b.ExpectedHeadSHA || commit.SHA != head || len(commit.Parents) != 1 || commit.Parents[0].SHA != b.ExpectedHeadSHA || commit.Message != batchMessage(b) {
		return out, ErrConflict
	}
	leaves, e := r.batchLeaves(repo, base.Tree.SHA, commit.Tree.SHA, batchPaths(b))
	if e != nil {
		return out, e
	}
	if _, e = r.batchRendered(repo, b, leaves, true); e != nil {
		return out, e
	}
	var final lookupPR
	if e = r.get(fmt.Sprintf("/repos/%s/pulls/%d", repo, pr.Number), &final); e != nil {
		return out, e
	}
	if !valid(final) || final.Number != pr.Number || final.Head.SHA != head || final.URL != pr.URL || final.State != pr.State || final.Merged != pr.Merged || !samePRMergeTime(final.MergedAt, pr.MergedAt) {
		return out, ErrConflict
	}
	if e = checkHead(); e != nil {
		return out, e
	}
	return delivery.GitOpsPullRequest{Number: pr.Number, URL: pr.URL, HeadSHA: head, State: pr.State, Merged: pr.Merged, MergedAt: pr.MergedAt}, nil
}
