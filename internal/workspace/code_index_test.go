package workspace

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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
