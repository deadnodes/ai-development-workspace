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
		args := []string{"ls-files", "-z", "--cached"}
		if includeUntracked {
			exclude, err := codeIndexGitExclude(ctx, checkout)
			if err != nil {
				return "", "", nil, false, err
			}
			args = append([]string{"-c", "core.excludesFile=" + exclude}, args...)
			args = append(args, "--others", "--exclude-standard")
		}
		output, err := gitOutput(ctx, checkout, 32<<20, args...)
		if err != nil && !errors.Is(err, errPathBudget) {
			return "", "", nil, false, fmt.Errorf("list checkout files: %w", err)
		}
		paths := strings.Split(output, "\x00")
		paths = paths[:len(paths)-1]
		slices.Sort(paths)
		return commit, branch, slices.Compact(paths), err != nil, ctx.Err()
	}
	paths, truncated, err := walkCodeIndexPaths(ctx, checkout, MaxEntries)
	return "", "@{unversioned}", paths, truncated, err
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
