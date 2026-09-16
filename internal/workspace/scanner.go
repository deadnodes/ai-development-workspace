// Package workspace observes local source checkouts without cloning, fetching,
// executing project commands, or treating repository instructions as executable.
package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

const MaxInstructionBytes = 128 << 10
const maxGitOutput = 16 << 10
const MaxDepth = 32
const MaxRepositories = 1000
const MaxEntries = 100000

type Document struct {
	Path      string `json:"path"`
	Content   string `json:"content"`
	SHA256    string `json:"sha256"`
	Truncated bool   `json:"truncated"`
}
type Repository struct {
	RelativePath string    `json:"relative_path"`
	Name         string    `json:"name"`
	RemoteURL    string    `json:"remote_url,omitempty"`
	Branch       string    `json:"branch,omitempty"`
	Commit       string    `json:"commit,omitempty"`
	Agents       *Document `json:"agents,omitempty"`
	Errors       []string  `json:"errors,omitempty"`
}
type Result struct {
	Truncated    bool         `json:"truncated"`
	Root         string       `json:"root"`
	Agents       *Document    `json:"agents,omitempty"`
	Repositories []Repository `json:"repositories"`
	Warnings     []string     `json:"warnings,omitempty"`
}

func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
func noLinkPath(root, path string) bool {
	if !within(root, path) {
		return false
	}
	rel, _ := filepath.Rel(root, path)
	current := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "." {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return false
		}
	}
	return true
}
func instructions(root, path string) (*Document, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || !noLinkPath(root, path) {
		return nil, fmt.Errorf("instruction file is not a regular in-root file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxInstructionBytes+1))
	if err != nil {
		return nil, err
	}
	truncated := len(data) > MaxInstructionBytes
	if truncated {
		data = data[:MaxInstructionBytes]
	}
	if truncated {
		for trim := 0; trim < utf8.UTFMax && !utf8.Valid(data) && len(data) > 0; trim++ {
			data = data[:len(data)-1]
		}
	}
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("instruction file must be UTF-8")
	}
	hash := sha256.Sum256(data)
	rel, _ := filepath.Rel(root, path)
	return &Document{Path: filepath.ToSlash(rel), Content: string(data), SHA256: hex.EncodeToString(hash[:]), Truncated: truncated}, nil
}

// Scan discovers checkouts throughout the workspace, including nested Git
// repositories when the workspace itself is a checkout. Traversal is bounded.
// Hashes identify the exact returned instruction bytes, including truncation.
func Scan(ctx context.Context, root string) (Result, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return Result{}, err
	}
	real, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return Result{}, err
	}
	// Resolve the explicitly selected root (for example macOS /var ->
	// /private/var) once; every discovered path is bounded by this canonical root.
	info, err := os.Stat(real)
	if err != nil || !info.IsDir() {
		return Result{}, fmt.Errorf("workspace root must be an existing directory")
	}
	result := Result{Root: real, Repositories: []Repository{}}
	result.Agents, err = instructions(real, filepath.Join(real, "AGENTS.md"))
	if err != nil {
		result.Warnings = append(result.Warnings, "workspace AGENTS.md unavailable or unsafe")
	}
	skip := map[string]bool{"node_modules": true, "vendor": true, ".venv": true, "venv": true, ".cache": true, ".local": true, ".git": true, "__pycache__": true}
	visited := 0
	observed := map[string]bool{}
	visit := func(path string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			rel, _ := filepath.Rel(real, path)
			result.Warnings = append(result.Warnings, filepath.ToSlash(rel)+": unreadable path")
			return nil
		}
		visited++
		if visited > MaxEntries {
			result.Truncated = true
			result.Warnings = append(result.Warnings, "workspace entry limit reached")
			return filepath.SkipAll
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if !entry.IsDir() {
			return nil
		}
		if path != real && skip[entry.Name()] {
			return filepath.SkipDir
		}
		relative, _ := filepath.Rel(real, path)
		if relative != "." && len(strings.Split(relative, string(filepath.Separator))) > MaxDepth {
			if !result.Truncated {
				result.Warnings = append(result.Warnings, "workspace depth limit reached")
			}
			result.Truncated = true
			return filepath.SkipDir
		}
		if observed[path] {
			return nil
		}
		observed[path] = true
		marker := filepath.Join(path, ".git")
		if _, err := os.Lstat(marker); os.IsNotExist(err) {
			return nil
		} else if err != nil {
			return nil
		}
		if len(result.Repositories) >= MaxRepositories {
			result.Truncated = true
			result.Warnings = append(result.Warnings, "repository count limit reached")
			return filepath.SkipAll
		}
		rel, _ := filepath.Rel(real, path)
		repo := Repository{RelativePath: filepath.ToSlash(rel), Name: filepath.Base(path)}
		if err := safeGitDirectory(real, marker); err != nil {
			repo.Errors = append(repo.Errors, "Git metadata is outside workspace or unsafe")
			result.Repositories = append(result.Repositories, repo)
			return nil
		}
		if value, e := git(ctx, path, "symbolic-ref", "--quiet", "--short", "HEAD"); e == nil {
			repo.Branch = value
		}
		if value, e := git(ctx, path, "rev-parse", "--verify", "HEAD"); e == nil && commitPattern.MatchString(value) {
			repo.Commit = value
		} else {
			repo.Errors = append(repo.Errors, "HEAD commit unavailable (repository may be empty)")
		}
		if value, e := git(ctx, path, "config", "--local", "--no-includes", "--get", "remote.origin.url"); e == nil {
			if clean, ok := sanitizeRemote(value); ok {
				repo.RemoteURL = clean
			} else {
				repo.Errors = append(repo.Errors, "origin remote URL is unsupported or unsafe")
			}
		}
		repo.Agents, err = instructions(real, filepath.Join(path, "AGENTS.md"))
		if err != nil {
			repo.Errors = append(repo.Errors, "repository AGENTS.md unavailable or unsafe")
		}
		result.Repositories = append(result.Repositories, repo)
		return nil
	}
	// Observe the root and its direct repositories before descending into large
	// temporary worktrees/vendor trees, which can exhaust the bounded traversal.
	if err = visit(real, fs.FileInfoToDirEntry(info), nil); err != nil {
		return result, err
	}
	children, e := os.ReadDir(real)
	if e != nil {
		return result, e
	}
	for _, child := range children {
		if !child.IsDir() || skip[child.Name()] {
			continue
		}
		if e = visit(filepath.Join(real, child.Name()), child, nil); e != nil && e != filepath.SkipDir {
			return result, e
		}
	}
	err = filepath.WalkDir(real, visit)
	return result, err
}

var commitPattern = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

func safeGitDirectory(root, marker string) error {
	if !noLinkPath(root, marker) {
		return fmt.Errorf("unsafe marker")
	}
	info, err := os.Stat(marker)
	if err != nil {
		return err
	}
	dir := marker
	if !info.IsDir() {
		if !info.Mode().IsRegular() || info.Size() > 4096 {
			return fmt.Errorf("invalid marker")
		}
		data, err := os.ReadFile(marker)
		if err != nil {
			return err
		}
		text := strings.TrimSpace(string(data))
		if !strings.HasPrefix(text, "gitdir: ") {
			return fmt.Errorf("invalid worktree marker")
		}
		dir = strings.TrimPrefix(text, "gitdir: ")
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(filepath.Dir(marker), dir)
		}
		dir = filepath.Clean(dir)
	}
	if !noLinkPath(root, dir) {
		return fmt.Errorf("git directory outside root")
	}
	for _, name := range []string{"HEAD", "config", "commondir", "packed-refs", "refs", "objects"} {
		p := filepath.Join(dir, name)
		info, err := os.Lstat(p)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil || info.Mode()&os.ModeSymlink != 0 || (!info.Mode().IsRegular() && name != "refs" && name != "objects") {
			return fmt.Errorf("unsafe metadata")
		}
		if name == "refs" {
			if err := safeRefs(p); err != nil {
				return err
			}
		}
		if name == "commondir" {
			if info.Size() > 4096 {
				return fmt.Errorf("invalid common directory")
			}
			b, e := os.ReadFile(p)
			if e != nil {
				return e
			}
			common := strings.TrimSpace(string(b))
			if !filepath.IsAbs(common) {
				common = filepath.Join(dir, common)
			}
			if !noLinkPath(root, filepath.Clean(common)) {
				return fmt.Errorf("common directory outside root")
			}
			for _, shared := range []string{"HEAD", "config", "packed-refs", "refs", "objects"} {
				sharedPath := filepath.Join(common, shared)
				if _, e := os.Lstat(sharedPath); os.IsNotExist(e) {
					continue
				}
				if !noLinkPath(root, sharedPath) {
					return fmt.Errorf("unsafe common metadata")
				}
				if shared == "refs" {
					if err := safeRefs(sharedPath); err != nil {
						return err
					}
				}

			}
		}
	}
	return nil
}

// Reference symlinks must not make local observation read files outside the root.
func safeRefs(path string) error {
	count := 0
	return filepath.WalkDir(path, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		count++
		if count > 10000 || entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("unsafe or oversized reference tree")
		}
		return nil
	})
}

type cappedOutput struct{ data []byte }

func (w *cappedOutput) Write(p []byte) (int, error) {
	if len(w.data)+len(p) > maxGitOutput {
		return 0, fmt.Errorf("git output too large")
	}
	w.data = append(w.data, p...)
	return len(p), nil
}
func git(ctx context.Context, path string, args ...string) (string, error) {
	fixed := []string{"--no-pager", "-c", "safe.directory=" + path, "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "protocol.allow=never", "-C", path}
	cmd := exec.CommandContext(ctx, "git", append(fixed, args...)...)
	// Do not inherit injected GIT_DIR/GIT_CONFIG_COUNT/credential helpers or user
	// configuration. These are plumbing reads and cannot start network operations.
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=/nonexistent", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0", "GIT_CEILING_DIRECTORIES=" + filepath.Dir(path), "LC_ALL=C"}
	output := &cappedOutput{}
	cmd.Stdout = output
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output.data)), nil
}
func sanitizeRemote(value string) (string, bool) {
	if value == "" || strings.ContainsAny(value, "\r\n\t ") {
		return "", false
	}
	if strings.HasPrefix(value, "git@") && !strings.Contains(value, "://") {
		parts := strings.SplitN(strings.TrimPrefix(value, "git@"), ":", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" || strings.ContainsAny(parts[0], "/@?#") || strings.ContainsAny(parts[1], "?#") {
			return "", false
		}
		return "git@" + parts[0] + ":" + parts[1], true
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Hostname() == "" || parsed.Path == "" || !strings.Contains("|https|http|ssh|git|", "|"+parsed.Scheme+"|") {
		return "", false
	}
	keepGitUser := parsed.Scheme == "ssh" && parsed.User != nil && parsed.User.Username() == "git"
	parsed.User = nil
	if keepGitUser {
		parsed.User = url.User("git")
	}
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	return parsed.String(), true
}
