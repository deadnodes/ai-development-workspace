package workspace

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// GitState is observation only: upstream SHA is the locally cached remote ref,
// never a claim that a network fetch or deployment occurred.
type GitState struct {
	Path              string    `json:"path"`
	RelativePath      string    `json:"relative_path"`
	Branch            string    `json:"branch"`
	HEAD              string    `json:"head"`
	Origin            string    `json:"origin"`
	Upstream          string    `json:"upstream"`
	UpstreamSHA       string    `json:"upstream_sha"`
	Ahead             int       `json:"ahead"`
	Behind            int       `json:"behind"`
	Dirty             bool      `json:"dirty"`
	Status            string    `json:"status"`
	ObservedAt        time.Time `json:"observed_at"`
	SubmodulesPresent bool      `json:"submodules_present"`
	RemoteRefIsCached bool      `json:"remote_ref_is_cached"`
}

func ObserveGit(ctx context.Context, root, relative string) (GitState, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	absolute, err := filepath.Abs(root)
	if err != nil {
		return GitState{}, err
	}
	root, err = filepath.EvalSymlinks(absolute)
	if err != nil {
		return GitState{}, err
	}
	path := filepath.Join(root, relative)
	if !noLinkPath(root, path) {
		return GitState{}, fmt.Errorf("checkout outside configured workspace or unavailable")
	}
	if err := safeGitDirectory(root, filepath.Join(path, ".git")); err != nil {
		return GitState{}, err
	}
	// A status refresh can invoke clean filters configured by the checkout.
	// Reject executable/include and alternate worktree configuration before it.
	config, configErr := git(ctx, path, "config", "--local", "--no-includes", "--name-only", "--list")
	if configErr != nil {
		return GitState{}, fmt.Errorf("local Git configuration unavailable")
	}
	for _, key := range strings.Split(strings.ToLower(config), "\n") {
		if strings.HasPrefix(key, "filter.") || strings.HasPrefix(key, "include.") || strings.HasPrefix(key, "includeif.") || key == "extensions.worktreeconfig" || key == "core.worktree" || key == "core.alternaterefscommand" {
			return GitState{}, fmt.Errorf("checkout requires external-agent inspection: unsupported Git configuration")
		}
	}
	common, err := git(ctx, path, "rev-parse", "--git-common-dir")
	if err != nil {
		return GitState{}, fmt.Errorf("Git common directory unavailable")
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(path, common)
	}
	if !noLinkPath(root, common) {
		return GitState{}, fmt.Errorf("Git common directory outside workspace")
	}
	for _, name := range []string{"alternates", "http-alternates"} {
		if _, err := os.Lstat(filepath.Join(common, "objects", "info", name)); !os.IsNotExist(err) {
			return GitState{}, fmt.Errorf("alternate object storage requires external-agent inspection")
		}
	}
	v := GitState{Path: path, RelativePath: relative, Status: "UNKNOWN", ObservedAt: time.Now().UTC(), RemoteRefIsCached: true}
	if _, e := os.Lstat(filepath.Join(path, ".gitmodules")); !os.IsNotExist(e) {
		v.SubmodulesPresent = true
	}
	v.HEAD, err = git(ctx, path, "rev-parse", "--verify", "HEAD")
	if err != nil || !commitPattern.MatchString(v.HEAD) {
		return v, fmt.Errorf("HEAD unavailable")
	}
	v.Branch, _ = git(ctx, path, "symbolic-ref", "--quiet", "--short", "HEAD")
	origin, _ := git(ctx, path, "config", "--local", "--no-includes", "--get", "remote.origin.url")
	v.Origin, _ = sanitizeRemote(origin)
	// Disable optional locks, fsmonitor, hooks and network in the shared reader.
	dirty, err := git(ctx, path, "status", "--porcelain=v1", "--untracked-files=normal", "--ignore-submodules=all")
	if err != nil {
		return v, fmt.Errorf("working tree state unavailable")
	}
	v.Dirty = dirty != ""
	v.Upstream, _ = git(ctx, path, "rev-parse", "--symbolic-full-name", "@{upstream}")
	v.UpstreamSHA, _ = git(ctx, path, "rev-parse", "--verify", "@{upstream}^{commit}")
	if !strings.HasPrefix(v.Upstream, "refs/remotes/origin/") || !commitPattern.MatchString(v.UpstreamSHA) {
		v.UpstreamSHA = ""
		return v, nil
	}
	counts, err := git(ctx, path, "rev-list", "--left-right", "--count", v.HEAD+"..."+v.UpstreamSHA)
	if err != nil {
		return v, fmt.Errorf("divergence unavailable")
	}
	parts := strings.Fields(counts)
	if len(parts) != 2 {
		return v, fmt.Errorf("invalid divergence result")
	}
	v.Ahead, _ = strconv.Atoi(parts[0])
	v.Behind, _ = strconv.Atoi(parts[1])
	switch {
	case v.Ahead > 0 && v.Behind > 0:
		v.Status = "DIVERGED"
	case v.Behind > 0:
		v.Status = "BEHIND"
	case v.Ahead > 0:
		v.Status = "AHEAD"
	default:
		v.Status = "CURRENT"
	}
	return v, nil
}
