package workspace

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

var errPathBudget = errors.New("git output too large")

func CodeIndexPaths(ctx context.Context, checkout string, includeUntracked bool) (string, string, []string, bool, error) {
	checkout, err := filepath.EvalSymlinks(checkout)
	if err != nil {
		return "", "", nil, false, err
	}
	checkout, err = filepath.Abs(checkout)
	if err != nil {
		return "", "", nil, false, err
	}
	_, gitErr := exec.LookPath("git")
	_, markerErr := os.Lstat(filepath.Join(checkout, ".git"))
	if markerErr != nil && !os.IsNotExist(markerErr) {
		return "", "", nil, false, markerErr
	}
	if markerErr == nil {
		if gitErr != nil {
			return "", "", nil, false, fmt.Errorf("Git is required to index this checkout: %w", gitErr)
		}
		commit, err := git(ctx, checkout, "rev-parse", "--verify", "HEAD")
		if err != nil || !commitPattern.MatchString(commit) {
			return "", "", nil, false, fmt.Errorf("checkout HEAD unavailable")
		}
		branch, err := git(ctx, checkout, "symbolic-ref", "--quiet", "--short", "HEAD")
		if err != nil {
			branch = "@{detached}"
		}
		output, err := gitOutput(ctx, checkout, 32<<20, "ls-files", "-z", "--stage")
		if err != nil && !errors.Is(err, errPathBudget) {
			return "", "", nil, false, fmt.Errorf("list tracked files: %w", err)
		}
		partial := err != nil
		paths := []string{}
		excluded := map[string]bool{}
		for record := range gitPathRecords(output) {
			meta, name, ok := strings.Cut(record, "\t")
			mode, _, _ := strings.Cut(meta, " ")
			if !ok || name == "" {
				return "", "", nil, false, fmt.Errorf("invalid Git index record")
			}
			switch mode {
			case "100644", "100755":
				paths = append(paths, name)
			default:
				excluded[name] = true
			}
		}
		if includeUntracked && !partial {
			exclude, err := codeIndexGitExclude(ctx, checkout)
			if err != nil {
				return "", "", nil, false, err
			}
			output, err = gitOutput(ctx, checkout, 32<<20, "-c", "core.excludesFile="+exclude, "ls-files", "-z", "--others", "--exclude-standard")
			if err != nil && !errors.Is(err, errPathBudget) {
				return "", "", nil, false, fmt.Errorf("list untracked files: %w", err)
			}
			partial = err != nil
			for name := range gitPathRecords(output) {
				paths = append(paths, name)
			}
		}
		paths = slices.DeleteFunc(paths, func(name string) bool { return excluded[name] })
		slices.Sort(paths)
		return commit, branch, slices.Compact(paths), partial, ctx.Err()
	}
	if !includeUntracked {
		for ancestor := checkout; filepath.Dir(ancestor) != ancestor; {
			ancestor = filepath.Dir(ancestor)
			if err := ctx.Err(); err != nil {
				return "", "", nil, false, err
			}
			if _, err := os.Lstat(filepath.Join(ancestor, ".git")); err == nil {
				return "", "", nil, false, fmt.Errorf("selected path is inside a Git worktree; use its checkout root or --include-untracked")
			} else if !os.IsNotExist(err) {
				return "", "", nil, false, err
			}
		}
	}
	paths, truncated, err := walkCodeIndexPaths(ctx, checkout, MaxEntries)
	return "", "@{unversioned}", paths, truncated, err
}

func gitPathRecords(output string) func(func(string) bool) {
	return func(yield func(string) bool) {
		for {
			record, rest, ok := strings.Cut(output, "\x00")
			if !ok || !yield(record) {
				return
			}
			output = rest
		}
	}
}

func CodeIndexTrackedSize(ctx context.Context, checkout, name string) (int, bool) {
	value, err := git(ctx, checkout, "cat-file", "-s", ":"+name)
	if err != nil {
		return 0, false
	}
	size, err := strconv.Atoi(value)
	return size, err == nil && size >= 0
}

func codeIndexGitExclude(ctx context.Context, checkout string) (string, error) {
	home, _ := os.UserHomeDir()
	xdg := os.Getenv("XDG_CONFIG_HOME")
	cmd := gitReadCommand(ctx, checkout, "config", "--includes", "--null", "--path", "--get", "core.excludesFile")
	cmd.Env = slices.DeleteFunc(cmd.Env, func(value string) bool {
		return strings.HasPrefix(value, "GIT_CONFIG_GLOBAL=") || strings.HasPrefix(value, "GIT_CONFIG_NOSYSTEM=")
	})
	cmd.Env = append(cmd.Env, "HOME="+home, "XDG_CONFIG_HOME="+xdg, "USERPROFILE="+home)
	for _, key := range []string{"GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM", "GIT_CONFIG_NOSYSTEM"} {
		if value, ok := os.LookupEnv(key); ok {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
	}
	value, err := runGitOutput(ctx, cmd, maxGitOutput)
	if err == nil {
		return strings.TrimSuffix(value, "\x00"), nil
	}
	var status *exec.ExitError
	if !errors.As(err, &status) || status.ExitCode() != 1 {
		return "", fmt.Errorf("read Git core.excludesFile: %w", err)
	}
	if xdg == "" && home != "" {
		xdg = filepath.Join(home, ".config")
	}
	if xdg != "" {
		return filepath.Join(xdg, "git", "ignore"), nil
	}
	return "", nil
}

func walkCodeIndexPaths(ctx context.Context, checkout string, budget int) ([]string, bool, error) {
	root, err := os.OpenRoot(checkout)
	if err != nil {
		return nil, false, err
	}
	defer root.Close()
	paths := []string{}
	pending := []string{"."}
	for len(pending) != 0 {
		dir := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		file, err := root.Open(dir)
		if err != nil {
			return nil, false, err
		}
		for {
			if err := ctx.Err(); err != nil {
				file.Close()
				return nil, false, err
			}
			entries, err := file.ReadDir(min(256, budget+1))
			for _, entry := range entries {
				if budget == 0 {
					file.Close()
					slices.Sort(paths)
					return paths, true, nil
				}
				budget--
				if entry.Type()&os.ModeSymlink != 0 {
					continue
				}
				name := filepath.Join(dir, entry.Name())
				if !entry.IsDir() {
					paths = append(paths, filepath.ToSlash(name))
					continue
				}
				switch entry.Name() {
				case ".git", "node_modules", "vendor", "dist", "build", "target", ".venv", "__pycache__":
				default:
					pending = append(pending, name)
				}
			}
			if err != nil {
				file.Close()
				if err != io.EOF {
					return nil, false, err
				}
				break
			}
		}
	}
	slices.Sort(paths)
	return paths, false, nil
}
