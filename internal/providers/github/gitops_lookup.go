package github

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"releasecontrol/internal/delivery"
)

var _ delivery.GitOpsProposalLookup = (*Provider)(nil)

// Recovery is deliberately stricter than the legacy proposer: no authentication
// refresh, ref creation, image write, or PR creation can happen on this path.
// Ordinary authenticated reads must have populated the connection token cache.
type proposalReader struct {
	p                     *Provider
	ctx                   context.Context
	base, token           string
	requests, bytes       int
	maxRequests, maxBytes int
	sessionExpires        time.Time
	batchExpires          time.Time
}

func (p *Provider) proposalReader(ctx context.Context, c delivery.Connection) (*proposalReader, error) {
	base, err := apiBase(c)
	if err != nil {
		return nil, err
	}
	key := fmt.Sprintf("%s|%d|%d|%s", base, c.AppID, c.InstallationID, c.PrivateKeyRef)
	p.mu.Lock()
	t := p.tokens[key]
	p.mu.Unlock()
	if t.value == "" || !t.expires.After(p.now().Add(time.Minute)) {
		return nil, errors.New("proposal lookup requires an existing unexpired connection session; outcome unresolved")
	}
	return &proposalReader{p: p, ctx: ctx, base: base, token: t.value, sessionExpires: t.expires}, nil
}

func (r *proposalReader) get(path string, out any) error {
	if !r.batchExpires.IsZero() && !r.batchExpires.After(r.p.now().Add(time.Minute)) {
		return errors.New("batch session expired; outcome unresolved")
	}
	r.requests++
	maxRequests, maxBytes := r.maxRequests, r.maxBytes
	if maxRequests == 0 {
		maxRequests = 48
	}
	if maxBytes == 0 {
		maxBytes = 8 << 20
	}
	if r.requests > maxRequests {
		return errors.New("proposal lookup request budget exceeded")
	}
	b, h, _, err := r.p.do(r.ctx, http.MethodGet, r.base+path, r.token, nil)
	if err != nil {
		return err
	}
	r.bytes += len(b)
	if len(b) > 2<<20 || r.bytes > maxBytes || h.Get("Link") != "" {
		return errors.New("proposal lookup evidence oversized or paginated")
	}
	if json.Unmarshal(b, out) != nil {
		return errors.New("invalid proposal lookup evidence")
	}
	return nil
}

type proposalCommit struct {
	SHA     string `json:"sha"`
	Message string `json:"message"`
	Tree    struct {
		SHA string `json:"sha"`
	} `json:"tree"`
	Parents []struct {
		SHA string `json:"sha"`
	} `json:"parents"`
}
type proposalEntry struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
	Type string `json:"type"`
	SHA  string `json:"sha"`
}

func (r *proposalReader) tree(repo, sha string) (map[string]proposalEntry, error) {
	var v struct {
		SHA       string          `json:"sha"`
		Truncated *bool           `json:"truncated"`
		Tree      []proposalEntry `json:"tree"`
	}
	if !fullSHA.MatchString(sha) {
		return nil, ErrConflict
	}
	if err := r.get("/repos/"+repo+"/git/trees/"+sha, &v); err != nil {
		return nil, err
	}
	if v.SHA != sha || v.Truncated == nil || *v.Truncated || len(v.Tree) > 2048 {
		return nil, ErrConflict
	}
	out := map[string]proposalEntry{}
	for _, e := range v.Tree {
		if e.Path == "" || e.Path == "." || e.Path == ".." || strings.ContainsAny(e.Path, "/\\\x00") || !fullSHA.MatchString(e.SHA) {
			return nil, ErrConflict
		}
		if _, duplicate := out[e.Path]; duplicate {
			return nil, ErrConflict
		}
		switch e.Type {
		case "tree":
			if e.Mode != "040000" {
				return nil, ErrConflict
			}
		case "blob":
			if e.Mode != "100644" && e.Mode != "100755" && e.Mode != "120000" {
				return nil, ErrConflict
			}
		case "commit":
			if e.Mode != "160000" {
				return nil, ErrConflict
			}
		default:
			return nil, ErrConflict
		}
		out[e.Path] = e
	}
	return out, nil
}

// Compare every sibling at every level, descending only along the configured
// file path. Equal sibling tree SHAs prove the entire unrelated subtree equal.
func (r *proposalReader) patchLeaves(repo, before, after, path string) (string, string, error) {
	parts := strings.Split(path, "/")
	if len(parts) > 16 {
		return "", "", errors.New("proposal lookup path depth exceeded")
	}
	for i, part := range parts {
		base, err := r.tree(repo, before)
		if err != nil {
			return "", "", err
		}
		head, err := r.tree(repo, after)
		if err != nil {
			return "", "", err
		}
		if len(base) != len(head) {
			return "", "", ErrConflict
		}
		b, bok := base[part]
		h, hok := head[part]
		if !bok || !hok || b.Mode != h.Mode || b.Type != h.Type {
			return "", "", ErrConflict
		}
		for name, old := range base {
			if name == part {
				continue
			}
			if current, ok := head[name]; !ok || current != old {
				return "", "", ErrConflict
			}
		}
		if i == len(parts)-1 {
			if b.Type != "blob" || (b.Mode != "100644" && b.Mode != "100755") {
				return "", "", ErrConflict
			}
			return b.SHA, h.SHA, nil
		}
		if b.Type != "tree" {
			return "", "", ErrConflict
		}
		before, after = b.SHA, h.SHA
	}
	return "", "", ErrConflict
}

func gitOpsBlobSHA(raw []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", len(raw))
	h.Write(raw)
	return fmt.Sprintf("%x", h.Sum(nil))
}

func (r *proposalReader) blob(repo, sha string) (string, error) {
	var v struct {
		SHA      string `json:"sha"`
		Encoding string `json:"encoding"`
		Size     *int   `json:"size"`
		Content  string `json:"content"`
	}
	if err := r.get("/repos/"+repo+"/git/blobs/"+sha, &v); err != nil {
		return "", err
	}
	if v.SHA != sha || v.Encoding != "base64" || v.Size == nil || *v.Size < 0 || *v.Size > 1<<20 {
		return "", ErrConflict
	}
	b, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(v.Content, "\n", ""))
	if err != nil || len(b) != *v.Size || gitOpsBlobSHA(b) != sha {
		return "", ErrConflict
	}
	return string(b), nil
}

type lookupPR struct {
	Number   int        `json:"number"`
	URL      string     `json:"html_url"`
	State    string     `json:"state"`
	Merged   bool       `json:"merged"`
	MergedAt *time.Time `json:"merged_at"`
	Head     struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo struct {
			ID       int64  `json:"id"`
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref  string `json:"ref"`
		Repo struct {
			ID       int64  `json:"id"`
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"base"`
}

func (p *Provider) LookupGitOpsProposal(ctx context.Context, c delivery.Connection, request delivery.GitOpsApply) (delivery.GitOpsPullRequest, error) {
	var out delivery.GitOpsPullRequest
	if !operationID.MatchString(request.OperationID) || !fullSHA.MatchString(request.ExpectedHeadSHA) || !fullSHA.MatchString(request.ExpectedBlobSHA) || !digestPattern.MatchString(request.Digest) || !validSourceBranch(request.Request.Ref) || len(request.Message) > 4096 {
		return out, errors.New("proposal lookup requires pinned operation, base, blob and digest")
	}
	if request.Request.ExpectedHeadSHA != "" && request.Request.ExpectedHeadSHA != request.ExpectedHeadSHA {
		return out, ErrConflict
	}
	if err := gitopsPath(request.Request); err != nil {
		return out, err
	}
	id, err := strconv.ParseInt(request.Request.Repository, 10, 64)
	if err != nil || id <= 0 || strconv.FormatInt(id, 10) != request.Request.Repository {
		return out, errors.New("proposal lookup requires numeric repository identity")
	}
	r, err := p.proposalReader(ctx, c)
	if err != nil {
		return out, err
	}
	var identity delivery.RepositoryInfo
	if err = r.get("/repositories/"+request.Request.Repository, &identity); err != nil {
		return out, err
	}
	repo := identity.FullName
	if identity.ID != id || !repoName.MatchString(repo) || c.Owner == "" || !strings.EqualFold(strings.Split(repo, "/")[0], c.Owner) {
		return out, ErrConflict
	}
	for _, part := range strings.Split(repo, "/") {
		if part == "." || part == ".." {
			return out, ErrConflict
		}
	}
	branch := "rcp/gitops/" + request.OperationID
	if request.Request.Ref == branch {
		return out, ErrConflict
	}
	readHead := func() (string, error) {
		var ref struct {
			Ref    string `json:"ref"`
			Object struct {
				SHA  string `json:"sha"`
				Type string `json:"type"`
			} `json:"object"`
		}
		err := r.get("/repos/"+repo+"/git/ref/heads/"+url.PathEscape(branch), &ref)
		if err != nil {
			return "", err
		}
		if ref.Ref != "refs/heads/"+branch || ref.Object.Type != "commit" || !fullSHA.MatchString(ref.Object.SHA) {
			return "", ErrConflict
		}
		return ref.Object.SHA, nil
	}
	head, err := readHead()
	if err != nil {
		return out, err
	}
	if head == request.ExpectedHeadSHA {
		return out, errors.New("proposal branch has no confirmed patch; outcome unresolved")
	}
	var baseCommit, headCommit proposalCommit
	if err = r.get("/repos/"+repo+"/git/commits/"+request.ExpectedHeadSHA, &baseCommit); err != nil {
		return out, err
	}
	if err = r.get("/repos/"+repo+"/git/commits/"+head, &headCommit); err != nil {
		return out, err
	}
	message := request.Message
	if message == "" {
		message = "Release Control Plane image update"
	}
	if baseCommit.SHA != request.ExpectedHeadSHA || headCommit.SHA != head || len(headCommit.Parents) != 1 || headCommit.Parents[0].SHA != request.ExpectedHeadSHA || headCommit.Message != message+" [rcp:"+request.OperationID+"]" {
		return out, ErrConflict
	}
	oldBlob, newBlob, err := r.patchLeaves(repo, baseCommit.Tree.SHA, headCommit.Tree.SHA, request.Request.Path)
	if err != nil {
		return out, err
	}
	if oldBlob != request.ExpectedBlobSHA {
		return out, ErrConflict
	}
	before, err := r.blob(repo, oldBlob)
	if err != nil {
		return out, err
	}
	after, err := r.blob(repo, newBlob)
	if err != nil {
		return out, err
	}
	docs, err := imageDocuments(before, request.Request)
	if err != nil {
		return out, err
	}
	image, _ := gitopsImageRepository(request.Request.ImageRepository)
	value := request.Digest
	if request.Request.DigestField == "spec.values.image.tag" {
		value = "rcp@" + value
	}
	expected, err := replaceImageScalars(before, docs, image, value)
	if err != nil {
		return out, err
	}
	if after != expected || gitOpsBlobSHA([]byte(expected)) != newBlob {
		return out, ErrConflict
	}
	q := url.Values{"state": {"all"}, "head": {strings.Split(repo, "/")[0] + ":" + branch}, "base": {request.Request.Ref}, "per_page": {"2"}, "page": {"1"}}
	var prs []lookupPR
	if err = r.get("/repos/"+repo+"/pulls?"+q.Encode(), &prs); err != nil {
		return out, err
	}
	if len(prs) != 1 || prs[0].Number <= 0 {
		return out, errors.New("proposal PR absent or ambiguous; outcome unresolved")
	}
	var pr lookupPR
	if err = r.get(fmt.Sprintf("/repos/%s/pulls/%d", repo, prs[0].Number), &pr); err != nil {
		return out, err
	}
	validPR := func(v lookupPR) bool {
		return v.Number == prs[0].Number && v.Head.Ref == branch && v.Head.SHA == head && v.Head.Repo.ID == id && v.Base.Ref == request.Request.Ref && v.Base.Repo.ID == id && strings.EqualFold(v.Head.Repo.FullName, repo) && strings.EqualFold(v.Base.Repo.FullName, repo) && (v.State == "open" || v.State == "closed") && v.URL != "" && len(v.URL) <= 2048 && (!v.Merged || v.State == "closed")
	}
	if !validPR(prs[0]) || !validPR(pr) || prs[0].Merged != pr.Merged || prs[0].State != pr.State || !samePRMergeTime(prs[0].MergedAt, pr.MergedAt) {
		return out, ErrConflict
	}
	finalHead, err := readHead()
	if err != nil {
		return out, err
	}
	if finalHead != head {
		return out, ErrConflict
	}
	return delivery.GitOpsPullRequest{Number: pr.Number, URL: pr.URL, HeadSHA: head, State: pr.State, Merged: pr.Merged, MergedAt: pr.MergedAt}, nil
}
