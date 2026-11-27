package workspace

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

var errPathBudget = errors.New("git output too large")

func CodeIndexPaths(ctx context.Context, checkout string) (string, string, []string, bool, error) {
	_, gitErr := exec.LookPath("git")
	_, markerErr := os.Lstat(filepath.Join(checkout, ".git"))
	if markerErr != nil && !os.IsNotExist(markerErr) {
		return "", "", nil, false, markerErr
	}
	if gitErr == nil && markerErr == nil {
		commit, err := git(ctx, checkout, "rev-parse", "--verify", "HEAD")
		if err != nil || !commitPattern.MatchString(commit) {
			return "", "", nil, false, fmt.Errorf("checkout HEAD unavailable")
		}
		branch, err := git(ctx, checkout, "symbolic-ref", "--quiet", "--short", "HEAD")
		if err != nil {
			branch = "@{detached}"
		}
		output, err := gitOutput(ctx, checkout, 32<<20, "ls-files", "-z", "-co", "--exclude-standard")
		if err != nil && !errors.Is(err, errPathBudget) {
			return "", "", nil, false, fmt.Errorf("list checkout files: %w", err)
		}
		paths := strings.Split(output, "\x00")
		paths = paths[:len(paths)-1]
		slices.Sort(paths)
		return commit, branch, slices.Compact(paths), err != nil, ctx.Err()
	}
	paths := []string{}
	visited := 0
	truncated := false
	err := filepath.WalkDir(checkout, func(full string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if full == checkout || entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "node_modules", "vendor", "dist", "build", "target", ".venv", "__pycache__":
				return filepath.SkipDir
			}
		}
		visited++
		if visited > MaxEntries {
			truncated = true
			return fs.SkipAll
		}
		if !entry.IsDir() {
			relative, err := filepath.Rel(checkout, full)
			if err != nil {
				return err
			}
			paths = append(paths, filepath.ToSlash(relative))
		}
		return nil
	})
	slices.Sort(paths)
	return "", "@{unversioned}", paths, truncated, err
}
