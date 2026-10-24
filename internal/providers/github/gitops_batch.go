package github

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"releasecontrol/internal/delivery"
	"strconv"
	"strings"
	"time"
)

var _ delivery.GitOpsBatchPRProvider = (*Provider)(nil)

func (p *Provider) batchReader(ctx context.Context, c delivery.Connection) (*proposalReader, error) {
	r, e := p.proposalReader(ctx, c)
	if e == nil {
		r.maxRequests = 128
		r.maxBytes = 16 << 20
		r.batchExpires = r.sessionExpires
	}
	return r, e
}
func (r *proposalReader) batchRepository(c delivery.Connection, b delivery.GitOpsBatchApply) (string, error) {
	var v delivery.RepositoryInfo
	if e := r.get("/repositories/"+b.Repository, &v); e != nil {
		return "", e
	}
	id, _ := strconv.ParseInt(b.Repository, 10, 64)
	if v.ID != id || !repoName.MatchString(v.FullName) || c.Owner == "" || !strings.EqualFold(strings.Split(v.FullName, "/")[0], c.Owner) {
		return "", ErrConflict
	}
	for _, part := range strings.Split(v.FullName, "/") {
		if part == "." || part == ".." {
			return "", ErrConflict
		}
	}
	return v.FullName, nil
}
func (r *proposalReader) batchHead(repo, branch string) (string, error) {
	var ref struct {
		Ref    string `json:"ref"`
		Object struct {
			SHA  string `json:"sha"`
			Type string `json:"type"`
		} `json:"object"`
	}
	if e := r.get("/repos/"+repo+"/git/ref/heads/"+url.PathEscape(branch), &ref); e != nil {
		return "", e
	}
	if ref.Ref != "refs/heads/"+branch || ref.Object.Type != "commit" || !fullSHA.MatchString(ref.Object.SHA) {
		return "", ErrConflict
	}
	return ref.Object.SHA, nil
}

type batchLeaf struct{ before, after, mode string }

// The allowed-path trie bounds descent; every sibling must stay identical.
func (r *proposalReader) batchLeaves(repo, before, after string, paths []string) (map[string]batchLeaf, error) {
	out := map[string]batchLeaf{}
	var walk func(string, string, string, []string) error
	walk = func(oldSHA, newSHA, prefix string, relative []string) error {
		old, e := r.tree(repo, oldSHA)
		if e != nil {
			return e
		}
		current := old
		if newSHA != oldSHA {
			current, e = r.tree(repo, newSHA)
			if e != nil {
				return e
			}
		}
		if len(old) != len(current) {
			return ErrConflict
		}
		allowed := map[string][]string{}
		for _, path := range relative {
			first, rest, _ := strings.Cut(path, "/")
			allowed[first] = append(allowed[first], rest)
		}
		for name, entry := range old {
			next, ok := current[name]
			if !ok || next.Mode != entry.Mode || next.Type != entry.Type {
				return ErrConflict
			}
			suffixes, selected := allowed[name]
			if !selected {
				if entry != next {
					return ErrConflict
				}
				continue
			}
			delete(allowed, name)
			full := prefix + name
			if len(suffixes) == 1 && suffixes[0] == "" {
				if entry.Type != "blob" || (entry.Mode != "100644" && entry.Mode != "100755") {
					return ErrConflict
				}
				out[full] = batchLeaf{entry.SHA, next.SHA, entry.Mode}
				continue
			}
			if entry.Type != "tree" {
				return ErrConflict
			}
			for _, s := range suffixes {
				if s == "" {
					return ErrConflict
				}
			}
			if e := walk(entry.SHA, next.SHA, full+"/", suffixes); e != nil {
				return e
			}
		}
		if len(allowed) != 0 {
			return ErrConflict
		}
		return nil
	}
	if e := walk(before, after, "", paths); e != nil {
		return nil, e
	}
	return out, nil
}
func batchMessage(b delivery.GitOpsBatchApply) string {
	m := b.Message
	if m == "" {
		m = "Release Control Plane image update"
	}
	return m + " [rcp:" + b.OperationID + "]"
}

func (r *proposalReader) batchRendered(repo string, b delivery.GitOpsBatchApply, leaves map[string]batchLeaf, verifyAfter bool) (map[string]string, error) {
	rendered := map[string]string{}
	originalBytes, renderedBytes := 0, 0
	for _, path := range batchPaths(b) {
		leaf, ok := leaves[path]
		if !ok {
			return nil, ErrConflict
		}
		changes := batchChanges(b, path)
		for _, ch := range changes {
			if ch.ExpectedBlobSHA != leaf.before {
				return nil, ErrConflict
			}
		}
		original, e := r.blob(repo, leaf.before)
		if e != nil {
			return nil, e
		}
		originalBytes += len(original)
		if originalBytes > 4<<20 {
			return nil, errors.New("GitOps original batch oversized")
		}
		after, e := renderGitOpsBatchFile(original, changes)
		if e != nil {
			return nil, e
		}
		renderedBytes += len(after)
		if renderedBytes > 4<<20 {
			return nil, errors.New("GitOps rendered batch oversized")
		}
		if verifyAfter {
			actual, e := r.blob(repo, leaf.after)
			if e != nil {
				return nil, e
			}
			if actual != after || gitOpsBlobSHA([]byte(after)) != leaf.after {
				return nil, ErrConflict
			}
		}
		rendered[path] = after
	}
	return rendered, nil
}

func (p *Provider) ProposeGitOpsBatchGuarded(ctx context.Context, c delivery.Connection, b delivery.GitOpsBatchApply, guard func(context.Context) error) (delivery.GitOpsPullRequest, error) {
	var out delivery.GitOpsPullRequest
	if e := validateGitOpsBatch(b); e != nil {
		return out, e
	}
	if guard == nil {
		return out, errors.New("GitOps batch requires execution guard")
	}
	ctx = context.WithValue(ctx, gitOpsWriteGuardKey{}, latchGitOpsGuard(guard))
	r, e := p.batchReader(ctx, c)
	if e != nil {
		return out, e
	}
	repo, e := r.batchRepository(c, b)
	if e != nil {
		return out, e
	}
	branch := "rcp/gitops/" + b.OperationID
	// An existing ref, even at the base, never permits another write.
	_, e = r.batchHead(repo, branch)
	if e == nil {
		return out, errors.New("proposal ref exists; reconcile only")
	}
	var api *APIError
	if !errors.As(e, &api) || api.Status != http.StatusNotFound {
		return out, e
	}
	head, e := r.batchHead(repo, b.Ref)
	if e != nil {
		return out, e
	}
	if head != b.ExpectedHeadSHA {
		return out, ErrConflict
	}
	var base proposalCommit
	if e = r.get("/repos/"+repo+"/git/commits/"+head, &base); e != nil {
		return out, e
	}
	if base.SHA != head || !fullSHA.MatchString(base.Tree.SHA) {
		return out, ErrConflict
	}
	leaves, e := r.batchLeaves(repo, base.Tree.SHA, base.Tree.SHA, batchPaths(b))
	if e != nil {
		return out, e
	}
	rendered, e := r.batchRendered(repo, b, leaves, false)
	if e != nil {
		return out, e
	}
	stable, e := r.batchHead(repo, b.Ref)
	if e != nil {
		return out, e
	}
	if stable != head {
		return out, ErrConflict
	}
	write := func(path string, input, out any) error {
		if !r.batchExpires.After(p.now().Add(time.Minute)) {
			return errors.New("batch session expired; outcome unresolved")
		}
		raw, h, _, e := p.do(ctx, http.MethodPost, r.base+path, r.token, input)
		if e != nil {
			return e
		}
		if len(raw) > 2<<20 || h.Get("Link") != "" {
			return errors.New("invalid batch mutation response")
		}
		return decodeBatchResponse(raw, out)
	}
	entries := []map[string]string{}
	for _, path := range batchPaths(b) {
		entries = append(entries, map[string]string{"path": path, "mode": leaves[path].mode, "type": "blob", "content": rendered[path]})
	}
	var tree struct {
		SHA string `json:"sha"`
	}
	if e = write("/repos/"+repo+"/git/trees", map[string]any{"base_tree": base.Tree.SHA, "tree": entries}, &tree); e != nil {
		return out, e
	}
	if !fullSHA.MatchString(tree.SHA) {
		return out, ErrConflict
	}
	var commit struct {
		SHA string `json:"sha"`
	}
	if e = write("/repos/"+repo+"/git/commits", map[string]any{"message": batchMessage(b), "tree": tree.SHA, "parents": []string{head}}, &commit); e != nil {
		return out, e
	}
	if !fullSHA.MatchString(commit.SHA) {
		return out, ErrConflict
	}
	var ref struct {
		Ref    string `json:"ref"`
		Object struct {
			SHA  string `json:"sha"`
			Type string `json:"type"`
		} `json:"object"`
	}
	if e = write("/repos/"+repo+"/git/refs", map[string]string{"ref": "refs/heads/" + branch, "sha": commit.SHA}, &ref); e != nil {
		return out, e
	}
	if ref.Ref != "refs/heads/"+branch || ref.Object.Type != "commit" || ref.Object.SHA != commit.SHA {
		return out, ErrConflict
	}
	var pr lookupPR
	if e = write("/repos/"+repo+"/pulls", map[string]string{"title": "Release Control Plane " + b.OperationID, "head": branch, "base": b.Ref, "body": "Operation " + b.OperationID + "; configured image fields only. External review and merge required."}, &pr); e != nil {
		return out, e
	}
	if pr.Number <= 0 || pr.Head.SHA != commit.SHA {
		return out, ErrConflict
	}
	return p.observeGitOpsBatchReader(ctx, c, b, delivery.GitOpsPullRequest{Number: pr.Number, HeadSHA: commit.SHA, URL: pr.URL}, false, r)
}
