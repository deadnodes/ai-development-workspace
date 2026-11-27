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
	"runtime"
	"slices"
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
	return scanWorkspace(ctx, root, true, MaxEntries)
}

func ScanIndexCheckouts(ctx context.Context, root string) (Result, error) {
	return scanWorkspace(ctx, root, false, MaxEntries)
}

func scanWorkspace(ctx context.Context, root string, withDocuments bool, budget int) (Result, error) {
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
	if withDocuments {
		result.Agents, err = instructions(real, filepath.Join(real, "AGENTS.md"))
		if err != nil {
			result.Warnings = append(result.Warnings, "workspace AGENTS.md unavailable or unsafe")
		}
	}
	skip := map[string]bool{"node_modules": true, "vendor": true, ".venv": true, "venv": true, ".cache": true, ".local": true, ".git": true, "__pycache__": true}
	type directory struct {
		path  string
		depth int
	}
	observe := func(path string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
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
		if err := safeGitDirectory(ctx, real, marker); err != nil {
			repo.Errors = append(repo.Errors, "Git metadata is outside workspace or unsafe")
			result.Repositories = append(result.Repositories, repo)
			return nil
		}
		if value, e := git(ctx, path, "symbolic-ref", "--quiet", "--short", "HEAD"); e == nil {
			repo.Branch = value
		}
		if value, e := git(ctx, path, "config", "--local", "--no-includes", "--get", "remote.origin.url"); e == nil {
			if clean, ok := sanitizeRemote(value); ok && RemoteIdentity(clean) != "" {
				repo.RemoteURL = clean
			} else {
				repo.Errors = append(repo.Errors, "origin remote URL is unsupported or unsafe")
			}
		}
		if withDocuments {
			if value, e := git(ctx, path, "rev-parse", "--verify", "HEAD"); e == nil && commitPattern.MatchString(value) {
				repo.Commit = value
			} else {
				repo.Errors = append(repo.Errors, "HEAD commit unavailable (repository may be empty)")
			}
			repo.Agents, err = instructions(real, filepath.Join(path, "AGENTS.md"))
			if err != nil {
				repo.Errors = append(repo.Errors, "repository AGENTS.md unavailable or unsafe")
			}
		}
		result.Repositories = append(result.Repositories, repo)
		return nil
	}
	if err := observe(real); err != nil {
		return result, err
	}
	budget--
	pending := []directory{{real, 0}}
	for len(pending) != 0 {
		current := pending[0]
		pending[0] = directory{}
		pending = pending[1:]
		err := readWorkspaceDir(ctx, current.path, &budget, func(child fs.DirEntry) error {
			if !child.IsDir() || skip[child.Name()] {
				return nil
			}
			if current.depth == MaxDepth {
				if !result.Truncated {
					result.Warnings = append(result.Warnings, "workspace depth limit reached")
				}
				result.Truncated = true
				return nil
			}
			path := filepath.Join(current.path, child.Name())
			if err := observe(path); err != nil {
				return err
			}
			pending = append(pending, directory{path, current.depth + 1})
			return nil
		})
		if err == filepath.SkipAll {
			if !result.Truncated {
				result.Warnings = append(result.Warnings, "workspace entry limit reached")
			}
			result.Truncated = true
			break
		}
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		if err != nil {
			if current.depth == 0 {
				return result, err
			}
			rel, _ := filepath.Rel(real, current.path)
			result.Warnings = append(result.Warnings, filepath.ToSlash(rel)+": unreadable path")
		}
	}
	slices.SortFunc(result.Repositories, func(left, right Repository) int {
		if leftDepth, rightDepth := strings.Count(left.RelativePath, "/"), strings.Count(right.RelativePath, "/"); leftDepth != rightDepth {
			return leftDepth - rightDepth
		}
		return strings.Compare(left.RelativePath, right.RelativePath)
	})
	return result, ctx.Err()
}

func readWorkspaceDir(ctx context.Context, path string, budget *int, visit func(fs.DirEntry) error) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, err := dir.ReadDir(min(256, max(1, *budget+1)))
		for _, entry := range entries {
			if *budget <= 0 {
				return filepath.SkipAll
			}
			*budget -= 1
			if err := visit(entry); err != nil {
				return err
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

var commitPattern = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

func safeGitDirectory(ctx context.Context, root, marker string) error {
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
			if err := safeRefs(ctx, p); err != nil {
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
					if err := safeRefs(ctx, sharedPath); err != nil {
						return err
					}
				}

			}
		}
	}
	return nil
}

// Reference symlinks must not make local observation read files outside the root.
func safeRefs(ctx context.Context, path string) error {
	budget := 9999
	pending := []string{path}
	for len(pending) != 0 {
		current := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if err := readWorkspaceDir(ctx, current, &budget, func(entry fs.DirEntry) error {
			if entry.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("unsafe reference tree")
			}
			if entry.IsDir() {
				pending = append(pending, filepath.Join(current, entry.Name()))
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

type cappedOutput struct {
	data     []byte
	limit    int
	overflow bool
}

func (w *cappedOutput) Write(p []byte) (int, error) {
	if len(w.data)+len(p) > w.limit {
		remaining := w.limit - len(w.data)
		w.data = append(w.data, p[:remaining]...)
		w.overflow = true
		return remaining, errPathBudget
	}
	w.data = append(w.data, p...)
	return len(p), nil
}
func git(ctx context.Context, path string, args ...string) (string, error) {
	value, err := gitOutput(ctx, path, maxGitOutput, args...)
	return strings.TrimSpace(value), err
}

func gitOutput(ctx context.Context, path string, limit int, args ...string) (string, error) {
	return runGitOutput(ctx, gitReadCommand(ctx, path, args...), limit)
}

func gitReadCommand(ctx context.Context, path string, args ...string) *exec.Cmd {
	fixed := []string{"--no-pager", "-c", "safe.directory=" + filepath.ToSlash(path), "-c", "core.hooksPath=" + os.DevNull, "-c", "core.fsmonitor=false", "-c", "protocol.allow=never", "-C", path}
	cmd := exec.CommandContext(ctx, "git", append(fixed, args...)...)
	// Do not inherit injected GIT_DIR/GIT_CONFIG_COUNT/credential helpers or user
	// configuration. These are plumbing reads and cannot start network operations.
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0", "GIT_CEILING_DIRECTORIES=" + filepath.Dir(path), "LC_ALL=C"}
	if runtime.GOOS == "windows" {
		cmd.Env = append(cmd.Env, "SystemRoot="+os.Getenv("SystemRoot"))
	}
	return cmd
}

func runGitOutput(ctx context.Context, cmd *exec.Cmd, limit int) (string, error) {
	output := &cappedOutput{limit: limit}
	cmd.Stdout = output
	cmd.Stderr = io.Discard
	err := cmd.Run()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if output.overflow {
		return string(output.data), errPathBudget
	}
	return string(output.data), err
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
