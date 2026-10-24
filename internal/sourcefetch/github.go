// Package sourcefetch materializes pinned GitHub file trees on the supervisor.
// GitHub is trusted for commit-to-tree mapping; tree and blob identities are
// independently reconstructed. No source content or credentials enter errors.
package sourcefetch

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"releasecontrol/internal/domain"
	"releasecontrol/internal/workerconfig"
)

var objectID = regexp.MustCompile(`^[0-9a-f]{40}$`)

type entry struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
	Type string `json:"type"`
	SHA  string `json:"sha"`
	Size *int64 `json:"size"`
}

type reader struct {
	ctx        context.Context
	client     *http.Client
	credential func() (string, error)
	spec       domain.WorkerSourceFetch
	remaining  int64
	requests   int
}

func fail(category string) error { return errors.New("source fetch: " + category) }

// Materialize creates a fresh private destination, removing it on any failure.
// The injected client is a test seam, never an alternate endpoint configuration.
func Materialize(ctx context.Context, destination, repositoryURL, sourceSHA string, spec domain.WorkerSourceFetch, maxSourceBytes int64, credential func() (string, error), client *http.Client) (err error) {
	if !domain.ValidWorkerSourceFetch(&spec, maxSourceBytes) || !objectID.MatchString(sourceSHA) || credential == nil {
		return fail("invalid definition")
	}
	if !workerconfig.ValidGitHubSourceRepositoryURL(repositoryURL) {
		return fail("invalid repository")
	}
	u, _ := url.Parse(repositoryURL)
	repo := strings.TrimPrefix(u.Path, "/")
	ctx, cancel := context.WithTimeout(ctx, time.Duration(spec.TimeoutSeconds)*time.Second)
	defer cancel()
	cl := http.Client{}
	if client != nil {
		cl = *client
	}
	cl.Jar = nil
	cl.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	r := reader{ctx: ctx, client: &cl, credential: credential, spec: spec, remaining: spec.MaxDownloadBytes}
	var identity struct {
		ID       int64  `json:"id"`
		FullName string `json:"full_name"`
	}
	if err = r.metadata("/repos/"+repo, &identity, repositoryMetadata); err != nil {
		return err
	}
	if identity.ID != spec.RepositoryID || identity.FullName != repo {
		return fail("repository identity")
	}
	var commit struct {
		SHA  string `json:"sha"`
		Tree struct {
			SHA string `json:"sha"`
		} `json:"tree"`
	}
	if err = r.metadata("/repos/"+repo+"/git/commits/"+sourceSHA, &commit, commitMetadata); err != nil {
		return err
	}
	if commit.SHA != sourceSHA || !objectID.MatchString(commit.Tree.SHA) {
		return fail("commit identity")
	}
	var tree struct {
		SHA       string  `json:"sha"`
		Truncated *bool   `json:"truncated"`
		Tree      []entry `json:"tree"`
	}
	if err = r.metadata("/repos/"+repo+"/git/trees/"+commit.Tree.SHA+"?recursive=1", &tree, treeMetadata); err != nil {
		return err
	}
	if tree.SHA != commit.Tree.SHA || tree.Truncated == nil || *tree.Truncated || tree.Tree == nil {
		return fail("incomplete tree")
	}
	if err = validateTree(tree.Tree, commit.Tree.SHA, spec, maxSourceBytes); err != nil {
		return err
	}
	var fileBytes int64
	fileRequests := 0
	for _, item := range tree.Tree {
		if item.Type == "blob" {
			fileBytes += *item.Size
			fileRequests++
		}
	}
	if fileBytes > r.remaining || fileRequests > spec.MaxRequests-r.requests {
		return fail("declared download budget")
	}
	if err = os.Mkdir(destination, 0700); err != nil {
		return fail("destination unavailable")
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(destination)
		}
	}()
	// Parents are validated first; sorting ensures directories precede children.
	sort.Slice(tree.Tree, func(i, j int) bool { return tree.Tree[i].Path < tree.Tree[j].Path })
	for _, item := range tree.Tree {
		if err = ctx.Err(); err != nil {
			return fail("cancelled")
		}
		target := filepath.Join(destination, filepath.FromSlash(item.Path))
		if item.Type == "tree" {
			if err = os.Mkdir(target, 0700); err != nil {
				return fail("snapshot write")
			}
			continue
		}
		if err = r.blob("/repos/"+repo+"/git/blobs/"+item.SHA, target, item); err != nil {
			return err
		}
	}
	if ctx.Err() != nil {
		return fail("cancelled")
	}
	return nil
}

func (r *reader) request(path, accept string) (*http.Response, error) {
	if r.ctx.Err() != nil {
		return nil, fail("cancelled")
	}
	// A final empty Git blob is valid at the inclusive decoded-byte limit.
	// Its bounded body read still rejects any unexpected nonzero byte.
	if r.requests >= r.spec.MaxRequests || r.remaining < 0 {
		return nil, fail("request budget")
	}
	r.requests++
	token, err := r.credential()
	if err != nil {
		return nil, fail("credential unavailable")
	}
	if len(token) > 16384 {
		return nil, fail("credential invalid")
	}
	for _, b := range []byte(token) {
		if b <= 0x20 || b >= 0x7f {
			return nil, fail("credential invalid")
		}
	}
	req, err := http.NewRequestWithContext(r.ctx, http.MethodGet, "https://api.github.com"+path, nil)
	if err != nil {
		return nil, fail("request invalid")
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "rcp-source-reader")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := r.client.Do(req)
	if err != nil {
		return nil, fail("request failed")
	}
	if res.StatusCode != http.StatusOK {
		res.Body.Close()
		return nil, fail("provider status")
	}
	return res, nil
}

func (r *reader) body(res *http.Response, limit int64) ([]byte, error) {
	defer res.Body.Close()
	if limit > r.remaining {
		limit = r.remaining
	}
	if res.ContentLength > limit {
		return nil, fail("download budget")
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	r.remaining -= int64(len(body))
	if err != nil || int64(len(body)) > limit || res.ContentLength >= 0 && res.ContentLength != int64(len(body)) {
		return nil, fail("response invalid")
	}
	return body, nil
}

type metadataShape uint8

const (
	unusedMetadata metadataShape = iota
	repositoryMetadata
	commitMetadata
	commitTreeMetadata
	treeMetadata
	treeEntriesMetadata
	treeEntryMetadata
)

// Only fields consumed by this reader have canonical spelling requirements.
// Unused provider metadata (including nested author data) remains ignored.
var consumedMetadataFields = map[metadataShape]map[string]metadataShape{
	repositoryMetadata: {"id": unusedMetadata, "full_name": unusedMetadata},
	commitMetadata:     {"sha": unusedMetadata, "tree": commitTreeMetadata},
	commitTreeMetadata: {"sha": unusedMetadata},
	treeMetadata:       {"sha": unusedMetadata, "truncated": unusedMetadata, "tree": treeEntriesMetadata},
	treeEntryMetadata:  {"path": unusedMetadata, "mode": unusedMetadata, "type": unusedMetadata, "sha": unusedMetadata, "size": unusedMetadata},
}

func (r *reader) metadata(path string, out any, shape metadataShape) error {
	res, err := r.request(path, "application/vnd.github+json")
	if err != nil {
		return err
	}
	body, err := r.body(res, 8<<20)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	if err = uniqueJSON(dec, 0, shape); err != nil {
		return fail("metadata invalid")
	}
	if _, err = dec.Token(); err != io.EOF {
		return fail("metadata invalid")
	}
	if json.Unmarshal(body, out) != nil {
		return fail("metadata invalid")
	}
	return nil
}

// Reject duplicate keys, consumed-field case aliases and excessive nesting.
func uniqueJSON(d *json.Decoder, depth int, shape metadataShape) error {
	if depth > 64 {
		return fail("metadata depth")
	}
	tok, err := d.Token()
	if err != nil {
		return err
	}
	switch tok {
	case json.Delim('{'):
		seen := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return err
			}
			s, ok := key.(string)
			if !ok || seen[s] {
				return fail("metadata duplicate")
			}
			seen[s] = true
			fields := consumedMetadataFields[shape]
			for canonical := range fields {
				if s != canonical && strings.EqualFold(s, canonical) {
					return fail("metadata field alias")
				}
			}
			if err = uniqueJSON(d, depth+1, fields[s]); err != nil {
				return err
			}
		}
		end, err := d.Token()
		if err != nil || end != json.Delim('}') {
			return fail("metadata invalid")
		}
	case json.Delim('['):
		childShape := unusedMetadata
		if shape == treeEntriesMetadata {
			childShape = treeEntryMetadata
		}
		for d.More() {
			if err = uniqueJSON(d, depth+1, childShape); err != nil {
				return err
			}
		}
		end, err := d.Token()
		if err != nil || end != json.Delim(']') {
			return fail("metadata invalid")
		}
	case json.Delim('}'), json.Delim(']'):
		return fail("metadata invalid")
	}
	return nil
}

func fold(s string) string {
	return strings.Map(func(r rune) rune {
		m := r
		for n := unicode.SimpleFold(r); n != r; n = unicode.SimpleFold(n) {
			if n < m {
				m = n
			}
		}
		return m
	}, s)
}

func validateTree(items []entry, root string, spec domain.WorkerSourceFetch, maxBytes int64) error {
	if len(items) > spec.MaxEntries {
		return fail("tree entry budget")
	}
	byPath := map[string]entry{}
	folded := map[string]bool{}
	children := map[string][]entry{}
	var expanded int64
	for _, item := range items {
		if len(item.Path) == 0 || len(item.Path) > 4096 || !utf8.ValidString(item.Path) || strings.Contains(item.Path, "\\") || !objectID.MatchString(item.SHA) {
			return fail("tree entry invalid")
		}
		parts := strings.Split(item.Path, "/")
		if len(parts) > spec.MaxDepth {
			return fail("tree depth budget")
		}
		for _, part := range parts {
			if part == "" || part == "." || part == ".." || len(part) > 255 || strings.EqualFold(part, ".git") {
				return fail("unsafe path")
			}
			for _, c := range part {
				if unicode.IsControl(c) {
					return fail("unsafe path")
				}
			}
		}
		key := fold(item.Path)
		if folded[key] {
			return fail("duplicate path")
		}
		folded[key] = true
		switch item.Mode {
		case "040000":
			if item.Type != "tree" || item.Size != nil {
				return fail("tree mode invalid")
			}
		case "100644", "100755":
			if item.Type != "blob" || item.Size == nil || *item.Size < 0 || *item.Size > maxBytes-expanded {
				return fail("source byte budget")
			}
			expanded += *item.Size
		default:
			return fail("unsupported source mode")
		}
		byPath[item.Path] = item
		parent := ""
		if i := strings.LastIndexByte(item.Path, '/'); i >= 0 {
			parent = item.Path[:i]
		}
		children[parent] = append(children[parent], item)
	}
	for path := range byPath {
		if i := strings.LastIndexByte(path, '/'); i >= 0 {
			parent, ok := byPath[path[:i]]
			if !ok || parent.Type != "tree" {
				return fail("tree closure invalid")
			}
		}
	}
	for path, item := range byPath {
		if item.Type == "tree" && treeHash(children[path]) != item.SHA {
			return fail("subtree identity")
		}
	}
	if treeHash(children[""]) != root {
		return fail("tree identity")
	}
	return nil
}

func treeHash(items []entry) string {
	ordered := append([]entry(nil), items...)
	name := func(e entry) string {
		s := e.Path[strings.LastIndexByte(e.Path, '/')+1:]
		if e.Type == "tree" {
			s += "/"
		}
		return s
	}
	sort.Slice(ordered, func(i, j int) bool { return name(ordered[i]) < name(ordered[j]) })
	var tree bytes.Buffer
	for _, item := range ordered {
		mode := item.Mode
		if mode == "040000" {
			mode = "40000"
		}
		tree.WriteString(mode + " " + item.Path[strings.LastIndexByte(item.Path, '/')+1:])
		tree.WriteByte(0)
		id, _ := hex.DecodeString(item.SHA)
		tree.Write(id)
	}
	h := sha1.New()
	fmt.Fprintf(h, "tree %d\x00", tree.Len())
	h.Write(tree.Bytes())
	return hex.EncodeToString(h.Sum(nil))
}

func (r *reader) blob(path, target string, item entry) error {
	res, err := r.request(path, "application/vnd.github.raw+json")
	if err != nil {
		return err
	}
	defer res.Body.Close()
	size := *item.Size
	if size > r.remaining || res.ContentLength >= 0 && res.ContentLength != size {
		return fail("blob size")
	}
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fail("snapshot write")
	}
	h := sha1.New()
	io.WriteString(h, "blob "+strconv.FormatInt(size, 10)+"\x00")
	written, copyErr := io.Copy(io.MultiWriter(f, h), io.LimitReader(res.Body, size+1))
	r.remaining -= written
	closeErr := f.Close()
	if copyErr != nil || closeErr != nil || written != size || hex.EncodeToString(h.Sum(nil)) != item.SHA {
		return fail("blob identity")
	}
	mode := os.FileMode(0400)
	if item.Mode == "100755" {
		mode = 0500
	}
	if os.Chmod(target, mode) != nil {
		return fail("snapshot mode")
	}
	return nil
}
