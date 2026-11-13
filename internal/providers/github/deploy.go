package github

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"releasecontrol/internal/buildconfig"
	"releasecontrol/internal/delivery"
	"sort"
	"strings"
)

type DeployFile struct {
	Path string
	Tags []DeployTag
}
type DeployPlan struct {
	ID, Branch, Body, ExpectedHead, ProposalBaseSHA string
	Files                                           []DeployFile
}
type DeployPR struct {
	Number    int    `json:"number"`
	URL       string `json:"html_url"`
	State     string `json:"state"`
	Merged    bool   `json:"merged"`
	MergeSHA  string `json:"merge_commit_sha"`
	Mergeable *bool  `json:"mergeable"`
	Head      struct {
		SHA  string `json:"sha"`
		Ref  string `json:"ref"`
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		SHA  string `json:"sha"`
		Ref  string `json:"ref"`
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"base"`
}
type DeployProposal struct {
	PR               DeployPR
	BaseSHA, HeadSHA string
	AlreadyApplied   bool
}

func deployConnection(g buildconfig.GitOps) (delivery.Connection, error) {
	if g.Kind != "github" || g.Branch != "main" || !repoName.MatchString(g.Repository) || filepath.Base(g.PrivateKeyRef) != g.PrivateKeyRef || g.PrivateKeyRef == "." || g.PrivateKeyRef == ".." {
		return delivery.Connection{}, errors.New("invalid Dev authority")
	}
	root := os.Getenv("RCP_SECRET_DIR")
	if root == "" {
		root = "/run/secrets"
	}
	root, e := filepath.Abs(root)
	if e != nil {
		return delivery.Connection{}, e
	}
	return delivery.Connection{OperatorAPIURL: true, AppID: g.AppID, InstallationID: g.InstallationID, PrivateKeyRef: "file:" + filepath.Join(root, g.PrivateKeyRef), APIURL: g.APIURL, Owner: strings.Split(g.Repository, "/")[0]}, nil
}
func (p *Provider) deployHead(ctx context.Context, c delivery.Connection, repo, branch string) (string, error) {
	var v struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	e := p.request(ctx, c, "GET", "/repos/"+repo+"/git/ref/heads/"+url.PathEscape(branch), nil, &v)
	if e == nil && !fullSHA.MatchString(v.Object.SHA) {
		e = ErrConflict
	}
	return v.Object.SHA, e
}
func (p *Provider) deployContent(ctx context.Context, c delivery.Connection, repo, path, sha string) (string, string, error) {
	var v struct{ Type, SHA, Encoding, Content string }
	e := p.request(ctx, c, "GET", "/repos/"+repo+"/contents/"+url.PathEscape(path)+"?ref="+url.QueryEscape(sha), nil, &v)
	if e != nil {
		return "", "", e
	}
	b, e := base64.StdEncoding.DecodeString(strings.ReplaceAll(v.Content, "\n", ""))
	if e != nil || v.Type != "file" || v.Encoding != "base64" || len(b) > 1<<20 || gitOpsBlobSHA(b) != v.SHA {
		return "", "", errors.New("invalid GitOps content")
	}
	return string(b), v.SHA, nil
}
func validateDeployPlan(d DeployPlan) error {
	if !operationID.MatchString(d.ID) || d.Branch != "rcp/deploy/"+d.ID || len(d.Files) == 0 || len(d.Files) > 16 || len(d.Body) > 32768 {
		return errors.New("invalid deployment plan")
	}
	seen := map[string]bool{}
	for _, f := range d.Files {
		if seen[f.Path] || len(f.Tags) == 0 {
			return ErrConflict
		}
		seen[f.Path] = true
		if e := gitopsReadPath(delivery.GitOpsRequest{Path: f.Path, Ref: "main"}); e != nil {
			return e
		}
	}
	return nil
}

// ProposeDeployment writes one tree containing only rendered configured image tags.
// Retry commits descend from the expected branch head, so non-force ref updates
// reject competing branch writers instead of replacing their work.
func (p *Provider) ProposeDeployment(ctx context.Context, g buildconfig.GitOps, d DeployPlan) (DeployProposal, error) {
	var out DeployProposal
	if e := validateDeployPlan(d); e != nil {
		return out, e
	}
	c, e := deployConnection(g)
	if e != nil {
		return out, e
	}
	base, e := p.deployHead(ctx, c, g.Repository, "main")
	if e != nil {
		return out, e
	}
	out.BaseSHA = base
	old, e := p.deployHead(ctx, c, g.Repository, d.Branch)
	if d.ExpectedHead == "" {
		if e == nil {
			return out, ErrConflict
		}
		if !errors.Is(e, ErrNotFound) {
			return out, e
		}
	} else if e != nil || old != d.ExpectedHead {
		return out, ErrConflict
	}
	entries := []map[string]string{}
	changed := false
	for _, f := range d.Files {
		content, _, e := p.deployContent(ctx, c, g.Repository, f.Path, base)
		if e != nil {
			return out, e
		}
		rendered, e := RenderDeployment(content, f.Tags)
		if e != nil {
			return out, e
		}
		changed = changed || rendered != content
		entries = append(entries, map[string]string{"path": f.Path, "mode": "100644", "type": "blob", "content": rendered})
	}
	if !changed {
		out.AlreadyApplied = true
		return out, nil
	}
	var commit proposalCommit
	if e = p.request(ctx, c, "GET", "/repos/"+g.Repository+"/git/commits/"+base, nil, &commit); e != nil {
		return out, e
	}
	if commit.SHA != base || !fullSHA.MatchString(commit.Tree.SHA) {
		return out, ErrConflict
	}
	// Verify file modes and existence with the existing bounded tree walker.
	reader, e := p.batchReader(ctx, c)
	if e != nil {
		return out, e
	}
	paths := []string{}
	for _, f := range d.Files {
		paths = append(paths, f.Path)
	}
	leaves, e := reader.batchLeaves(g.Repository, commit.Tree.SHA, commit.Tree.SHA, paths)
	if e != nil {
		return out, e
	}
	for _, entry := range entries {
		entry["mode"] = leaves[entry["path"]].mode
	}
	var tree struct {
		SHA string `json:"sha"`
	}
	prefix := "/repos/" + g.Repository
	if e = p.request(ctx, c, "POST", prefix+"/git/trees", map[string]any{"base_tree": commit.Tree.SHA, "tree": entries}, &tree); e != nil {
		return out, e
	}
	if !fullSHA.MatchString(tree.SHA) {
		return out, ErrConflict
	}
	parents := []string{base}
	if d.ExpectedHead != "" {
		parents = []string{old, base}
	}
	var created struct {
		SHA string `json:"sha"`
	}
	if e = p.request(ctx, c, "POST", prefix+"/git/commits", map[string]any{"message": "RCP Dev deployment " + d.ID, "tree": tree.SHA, "parents": parents}, &created); e != nil {
		return out, e
	}
	if !fullSHA.MatchString(created.SHA) {
		return out, ErrConflict
	}
	current, e := p.deployHead(ctx, c, g.Repository, "main")
	if e != nil || current != base {
		return out, ErrConflict
	}
	if d.ExpectedHead == "" {
		e = p.request(ctx, c, "POST", prefix+"/git/refs", map[string]string{"ref": "refs/heads/" + d.Branch, "sha": created.SHA}, nil)
	} else {
		current, e = p.deployHead(ctx, c, g.Repository, d.Branch)
		if e != nil || current != old {
			return out, ErrConflict
		}
		e = p.request(ctx, c, "PATCH", prefix+"/git/refs/heads/"+url.PathEscape(d.Branch), map[string]any{"sha": created.SHA, "force": false}, nil)
	}
	if e != nil {
		return out, e
	}
	out.HeadSHA = created.SHA
	if d.ExpectedHead == "" {
		e = p.request(ctx, c, "POST", prefix+"/pulls", map[string]string{"title": "RCP Dev deployment " + d.ID, "head": d.Branch, "base": "main", "body": d.Body}, &out.PR)
	} else {
		out.PR, e = p.FindDeployment(ctx, g, d.Branch)
	}
	if e != nil {
		return out, e
	}
	if out.PR.Number <= 0 || out.PR.Head.SHA != created.SHA {
		return out, ErrConflict
	}
	return out, nil
}
func (p *Provider) FindDeployment(ctx context.Context, g buildconfig.GitOps, branch string) (DeployPR, error) {
	var out DeployPR
	c, e := deployConnection(g)
	if e != nil {
		return out, e
	}
	var list []DeployPR
	q := url.Values{"state": {"all"}, "head": {c.Owner + ":" + branch}, "base": {"main"}, "per_page": {"100"}}
	if e = p.request(ctx, c, "GET", "/repos/"+g.Repository+"/pulls?"+q.Encode(), nil, &list); e != nil {
		return out, e
	}
	if len(list) != 1 || list[0].Head.Ref != branch || list[0].Head.Repo.FullName != g.Repository || list[0].Base.Ref != "main" || list[0].Base.Repo.FullName != g.Repository {
		return out, ErrConflict
	}
	return p.ReadDeployment(ctx, g, list[0].Number)
}
func (p *Provider) ReadDeployment(ctx context.Context, g buildconfig.GitOps, n int) (DeployPR, error) {
	var out DeployPR
	c, e := deployConnection(g)
	if e != nil {
		return out, e
	}
	if n <= 0 {
		return out, ErrConflict
	}
	e = p.request(ctx, c, "GET", fmt.Sprintf("/repos/%s/pulls/%d", g.Repository, n), nil, &out)
	if e == nil && (out.Number != n || !fullSHA.MatchString(out.Head.SHA) || len(out.URL) > 2048) {
		return DeployPR{}, ErrConflict
	}
	return out, e
}

// VerifyDeployment checks the complete tree diff and exact rendered file bytes.
func (p *Provider) VerifyDeployment(ctx context.Context, g buildconfig.GitOps, d DeployPlan, pr DeployPR) error {
	if e := validateDeployPlan(d); e != nil {
		return e
	}
	if (!pr.Merged && pr.State != "open") || pr.Head.Ref != d.Branch || pr.Head.SHA != d.ExpectedHead || pr.Base.Ref != "main" || pr.Head.Repo.FullName != g.Repository || pr.Base.Repo.FullName != g.Repository {
		return ErrConflict
	}
	// GitHub's PR base follows main. Scope belongs to the immutable base used
	// to render our proposal, even when unrelated main commits arrive later.
	base := d.ProposalBaseSHA
	if base == "" {
		return &APIError{Status: 409, Operation: "proposal base missing"}
	}
	if !fullSHA.MatchString(base) || !fullSHA.MatchString(pr.Head.SHA) {
		return ErrConflict
	}
	c, e := deployConnection(g)
	if e != nil {
		return e
	}
	r, e := p.batchReader(ctx, c)
	if e != nil {
		return e
	}
	commits := []proposalCommit{{}, {}}
	for i, sha := range []string{base, pr.Head.SHA} {
		if e = r.get("/repos/"+g.Repository+"/git/commits/"+sha, &commits[i]); e != nil {
			return e
		}
		if commits[i].SHA != sha {
			return ErrConflict
		}
	}
	paths := []string{}
	for _, f := range d.Files {
		paths = append(paths, f.Path)
	}
	sort.Strings(paths)
	if _, e = r.batchLeaves(g.Repository, commits[0].Tree.SHA, commits[1].Tree.SHA, paths); e != nil {
		return e
	}
	for _, f := range d.Files {
		before, _, e := p.deployContent(ctx, c, g.Repository, f.Path, base)
		if e != nil {
			return e
		}
		expected, e := RenderDeployment(before, f.Tags)
		if e != nil {
			return e
		}
		actual, _, e := p.deployContent(ctx, c, g.Repository, f.Path, pr.Head.SHA)
		if e != nil {
			return e
		}
		if expected != actual {
			return ErrConflict
		}
	}
	return nil
}
func (p *Provider) MergeDeployment(ctx context.Context, g buildconfig.GitOps, n int, sha string) (string, error) {
	if n <= 0 || !fullSHA.MatchString(sha) {
		return "", ErrConflict
	}
	c, e := deployConnection(g)
	if e != nil {
		return "", e
	}
	var out struct {
		Merged bool   `json:"merged"`
		SHA    string `json:"sha"`
	}
	e = p.request(ctx, c, "PUT", fmt.Sprintf("/repos/%s/pulls/%d/merge", g.Repository, n), map[string]string{"merge_method": "squash", "sha": sha}, &out)
	if e != nil {
		return "", e
	}
	if !out.Merged || !fullSHA.MatchString(out.SHA) {
		return "", errors.New("merge outcome unresolved")
	}
	return out.SHA, nil
}
