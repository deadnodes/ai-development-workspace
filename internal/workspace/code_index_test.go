package workspace

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestCodeIndexWalkChargesSkippedEntries(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "node_modules"), 0700); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 300; index++ {
		if err := os.Symlink("missing", filepath.Join(root, fmt.Sprintf("link-%03d", index))); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
	}
	for _, budget := range []int{256, 300, 301} {
		paths, truncated, err := walkCodeIndexPaths(context.Background(), root, budget)
		if err != nil || truncated != (budget < 301) || len(paths) != 0 {
			t.Fatalf("budget %d: %v, truncated=%v, %v", budget, paths, truncated, err)
		}
	}
}

func TestCodeIndexRejectsGitSubdirectoryWithoutOptIn(t *testing.T) {
	root := initRepo(t, scanRoot(t), "project")
	put(t, filepath.Join(root, ".gitignore"), "docs/README.local.md\n")
	subdir := filepath.Join(root, "docs")
	if err := os.Mkdir(subdir, 0700); err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(subdir, "README.local.md"), "private fixture")
	if _, _, _, _, err := CodeIndexPaths(context.Background(), subdir, false); err == nil || !strings.Contains(err.Error(), "checkout root") {
		t.Fatalf("expected Git checkout root requirement, got %v", err)
	}
	if _, _, paths, _, err := CodeIndexPaths(context.Background(), subdir, true); err != nil || !slices.Contains(paths, "README.local.md") {
		t.Fatalf("explicitly selected subdirectory: %v, %v", paths, err)
	}
}

func TestCodeIndexRejectsWorkspaceParentContainingGitCheckout(t *testing.T) {
	for _, marker := range []string{"directory", "worktree file"} {
		t.Run(marker, func(t *testing.T) {
			root := scanRoot(t)
			repo := filepath.Join(root, "projects", "private")
			if err := os.MkdirAll(repo, 0700); err != nil {
				t.Fatal(err)
			}
			if marker == "directory" {
				if err := os.Mkdir(filepath.Join(repo, ".git"), 0700); err != nil {
					t.Fatal(err)
				}
			} else {
				put(t, filepath.Join(repo, ".git"), "gitdir: /elsewhere\n")
			}
			put(t, filepath.Join(repo, ".gitignore"), "README.local.md\n")
			put(t, filepath.Join(repo, "README.local.md"), "private fixture\n")
			put(t, filepath.Join(root, "README.md"), "public fixture\n")
			for _, includeUntracked := range []bool{false, true} {
				if _, _, paths, _, err := CodeIndexPaths(context.Background(), root, includeUntracked); err == nil || !strings.Contains(err.Error(), "--workspace") || len(paths) != 0 {
					t.Fatalf("expected workspace guidance without paths: %v, %v", paths, err)
				}
			}
		})
	}
}

func TestCodeIndexPathsIgnoreGitSymlinkMode(t *testing.T) {
	root := initRepo(t, scanRoot(t), "project")
	put(t, filepath.Join(root, "actual.md"), "real text")
	if err := os.Symlink("actual.md", filepath.Join(root, "README.md")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	runGit(t, root, "-c", "core.symlinks=true", "add", "actual.md", "README.md")
	runGit(t, root, "commit", "-m", "tracked link")
	for _, includeUntracked := range []bool{false, true} {
		_, _, nativePaths, _, err := CodeIndexPaths(context.Background(), root, includeUntracked)
		if err != nil || !slices.Equal(nativePaths, []string{"actual.md"}) {
			t.Fatalf("native symlinks: %v, %v", nativePaths, err)
		}
		if err := os.Remove(filepath.Join(root, "README.md")); err != nil {
			t.Fatal(err)
		}
		runGit(t, root, "-c", "core.symlinks=false", "checkout-index", "--force", "README.md")
		if info, err := os.Lstat(filepath.Join(root, "README.md")); err != nil || !info.Mode().IsRegular() {
			t.Fatalf("expected regular symlink representation: %v, %v", info, err)
		}
		_, _, emulatedPaths, _, err := CodeIndexPaths(context.Background(), root, includeUntracked)
		if err != nil || !slices.Equal(nativePaths, emulatedPaths) {
			t.Fatalf("emulated symlinks: %v, %v", emulatedPaths, err)
		}
		if err := os.Remove(filepath.Join(root, "README.md")); err != nil {
			t.Fatal(err)
		}
		runGit(t, root, "-c", "core.symlinks=true", "checkout-index", "--force", "README.md")
	}
}
