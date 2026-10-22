package github

import (
	"context"
	"errors"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"releasecontrol/internal/delivery"
)

var _ delivery.GitOpsRevisionVerifier = (*Provider)(nil)

// VerifyGitOpsRevision pins a current ref using bounded complete GET responses.
// Authentication must already be cached; this path never refreshes or writes.
func (p *Provider) VerifyGitOpsRevision(ctx context.Context, c delivery.Connection, q delivery.GitOpsRevisionRequest) (delivery.GitOpsRevisionProof, error) {
	var out delivery.GitOpsRevisionProof
	id, err := strconv.ParseInt(q.Repository, 10, 64)
	if err != nil || id <= 0 || strconv.FormatInt(id, 10) != q.Repository || !validSourceBranch(q.Ref) || (q.HeadSHA != "" && !fullSHA.MatchString(q.HeadSHA)) || len(q.AncestorSHAs) == 0 || len(q.AncestorSHAs) > 16 {
		return out, errors.New("invalid GitOps revision scope")
	}
	ancestors := append([]string(nil), q.AncestorSHAs...)
	sort.Strings(ancestors)
	for i, sha := range ancestors {
		if !fullSHA.MatchString(sha) || (i > 0 && sha == ancestors[i-1]) {
			return out, errors.New("invalid or duplicate GitOps ancestor")
		}
	}
	r, err := p.proposalReader(ctx, c)
	if err != nil {
		return out, err
	}
	var repository delivery.RepositoryInfo
	if err = r.get("/repositories/"+q.Repository, &repository); err != nil {
		return out, err
	}
	name := repository.FullName
	if repository.ID != id || !repoName.MatchString(name) || (c.Owner != "" && !strings.EqualFold(strings.Split(name, "/")[0], c.Owner)) {
		return out, errors.New("GitOps repository identity mismatch")
	}
	for _, s := range strings.Split(name, "/") {
		if s == "." || s == ".." {
			return out, ErrConflict
		}
	}
	refPath := "/repos/" + name + "/git/ref/heads/" + url.PathEscape(q.Ref)
	readHead := func() (string, error) {
		var v struct {
			Ref    string `json:"ref"`
			Object struct {
				Type string `json:"type"`
				SHA  string `json:"sha"`
			} `json:"object"`
		}
		if e := r.get(refPath, &v); e != nil {
			return "", e
		}
		if v.Ref != "refs/heads/"+q.Ref || v.Object.Type != "commit" || !fullSHA.MatchString(v.Object.SHA) {
			return "", ErrConflict
		}
		return v.Object.SHA, nil
	}
	head, err := readHead()
	if err != nil {
		return out, err
	}
	if q.HeadSHA != "" && head != q.HeadSHA {
		return out, ErrConflict
	}
	for _, ancestor := range ancestors {
		if ancestor == head {
			continue // A freshly pinned commit contains itself; no compare needed.
		}
		var v struct {
			Status string `json:"status"`
			Ahead  int    `json:"ahead_by"`
			Behind int    `json:"behind_by"`
			Total  int    `json:"total_commits"`
			Base   struct {
				SHA string `json:"sha"`
			} `json:"base_commit"`
			MergeBase struct {
				SHA string `json:"sha"`
			} `json:"merge_base_commit"`
			Commits []struct {
				SHA string `json:"sha"`
			} `json:"commits"`
		}
		if err = r.get("/repos/"+name+"/compare/"+ancestor+"..."+head+"?per_page=100", &v); err != nil {
			return out, err
		}
		if v.Base.SHA != ancestor || v.MergeBase.SHA != ancestor || v.Behind != 0 || v.Ahead < 0 || v.Ahead > 100 || v.Total != v.Ahead || len(v.Commits) != v.Total {
			return out, ErrConflict
		}
		if v.Status != "ahead" || v.Ahead == 0 || v.Commits[len(v.Commits)-1].SHA != head {
			return out, ErrConflict
		}
		seen := map[string]bool{}
		for _, commit := range v.Commits {
			if !fullSHA.MatchString(commit.SHA) || seen[commit.SHA] {
				return out, ErrConflict
			}
			seen[commit.SHA] = true
		}
	}
	final, err := readHead()
	if err != nil {
		return out, err
	}
	if final != head {
		return out, ErrConflict
	}
	var finalRepository delivery.RepositoryInfo
	if err = r.get("/repositories/"+q.Repository, &finalRepository); err != nil {
		return out, err
	}
	if finalRepository.ID != id || finalRepository.FullName != name {
		return out, ErrConflict
	}
	return delivery.GitOpsRevisionProof{Repository: q.Repository, Ref: q.Ref, HeadSHA: head, AncestorSHAs: ancestors}, nil
}
