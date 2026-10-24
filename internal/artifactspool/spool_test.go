package artifactspool

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"releasecontrol/internal/domain"
	"sync"
	"testing"
)

func fixture(t *testing.T) (Config, string, domain.WorkerLocalOCI) {
	t.Helper()
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	if e := os.Chmod(root, 0700); e != nil {
		t.Fatal(e)
	}
	c := Config{root, 32, 64, 2}
	p := filepath.Join(t.TempDir(), "archive")
	b := []byte("verified archive fixture")
	if e := os.WriteFile(p, b, 0600); e != nil {
		t.Fatal(e)
	}
	return c, p, domain.WorkerLocalOCI{ArchiveHash: fmt.Sprintf("sha256:%x", sha256.Sum256(b)), ArchiveBytes: int64(len(b))}
}
func TestDurableReplayAndReadOnlyResolve(t *testing.T) {
	c, p, r := fixture(t)
	ctx := context.Background()
	if e := Export(ctx, c, p, r); e != nil {
		t.Fatal(e)
	}
	if e := os.Remove(p); e != nil {
		t.Fatal(e)
	}
	if e := Export(ctx, c, p, r); e != nil {
		t.Fatalf("durable replay must not need old source: %v", e)
	}
	before, _ := os.ReadDir(c.Root)
	if e := os.Chmod(c.Root, 0500); e != nil {
		t.Fatal(e)
	}
	defer os.Chmod(c.Root, 0700)
	got, e := Resolve(c, r)
	if e != nil {
		t.Fatal(e)
	}
	if filepath.Dir(got) != c.Root {
		t.Fatal(got)
	}
	after, _ := os.ReadDir(c.Root)
	if len(before) != len(after) {
		t.Fatal("resolver wrote spool state")
	}
}
func TestNoPassedAcknowledgementUntilBothSyncs(t *testing.T) {
	for _, failAt := range []int{1, 2} {
		t.Run(fmt.Sprint(failAt), func(t *testing.T) {
			c, p, r := fixture(t)
			calls := 0
			injected := errors.New("fsync fault")
			e := export(context.Background(), c, p, r, func(f *os.File) error {
				calls++
				if calls == failAt {
					return injected
				}
				return f.Sync()
			})
			if !errors.Is(e, injected) {
				t.Fatalf("sync failure acknowledged: %v", e)
			}
			if e = Export(context.Background(), c, p, r); e != nil {
				t.Fatalf("retry same immutable bytes: %v", e)
			}
			calls = 0
			e = export(context.Background(), c, p, r, func(f *os.File) error {
				calls++
				if calls == failAt {
					return injected
				}
				return f.Sync()
			})
			if !errors.Is(e, injected) {
				t.Fatal("existing entry replay skipped durability acknowledgement")
			}
		})
	}
}
func TestCorruptionBoundsAndPaths(t *testing.T) {
	for _, kind := range []string{"hash", "bytes", "symlink", "root-symlink", "nonprivate", "corrupt-existing", "cancelled", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			c, p, r := fixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch kind {
			case "hash":
				r.ArchiveHash = "../../archive"
			case "bytes":
				r.ArchiveBytes = c.MaxArchiveBytes + 1
			case "symlink":
				n, _ := name(c, r)
				os.Symlink(p, filepath.Join(c.Root, n))
			case "root-symlink":
				link := filepath.Join(t.TempDir(), "spool")
				os.Symlink(c.Root, link)
				c.Root = link
			case "nonprivate":
				os.Chmod(c.Root, 0755)
			case "corrupt-existing":
				if e := Export(ctx, c, p, r); e != nil {
					t.Fatal(e)
				}
				n, _ := name(c, r)
				os.WriteFile(filepath.Join(c.Root, n), []byte("corrupt"), 0600)
			case "cancelled":
				cancel()
			case "fifo":
				os.Remove(p)
				if e := makeFIFO(p); e != nil {
					t.Fatal(e)
				}
			}
			if e := Export(ctx, c, p, r); e == nil {
				t.Fatal("invalid spool accepted")
			}
			if kind != "cancelled" && kind != "fifo" {
				if _, e := Resolve(c, r); e == nil {
					t.Fatal("invalid receipt resolved")
				}
			}
		})
	}
}
func TestConcurrentWriterGlobalQuotas(t *testing.T) {
	c, p, r := fixture(t)
	c.MaxEntries = 1
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); <-start; errs <- Export(context.Background(), c, p, r) }()
	}
	close(start)
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	other := []byte("another artifact")
	os.WriteFile(p, other, 0600)
	r.ArchiveHash = fmt.Sprintf("sha256:%x", sha256.Sum256(other))
	r.ArchiveBytes = int64(len(other))
	if e := Export(context.Background(), c, p, r); e == nil {
		t.Fatal("entry quota bypass")
	}
	c.MaxEntries = 2
	c.MaxTotalBytes = 32
	if e := Export(context.Background(), c, p, r); e == nil {
		t.Fatal("global byte quota bypass")
	}
}

func TestConcurrentDistinctArchivesRespectGlobalQuota(t *testing.T) {
	c, _, _ := fixture(t)
	c.MaxEntries = 2
	c.MaxTotalBytes = 64
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		b := []byte(fmt.Sprintf("bounded archive number %d", i))
		p := filepath.Join(t.TempDir(), "archive")
		if e := os.WriteFile(p, b, 0600); e != nil {
			t.Fatal(e)
		}
		r := domain.WorkerLocalOCI{ArchiveHash: fmt.Sprintf("sha256:%x", sha256.Sum256(b)), ArchiveBytes: int64(len(b))}
		wg.Add(1)
		go func() { defer wg.Done(); <-start; errs <- Export(context.Background(), c, p, r) }()
	}
	close(start)
	wg.Wait()
	close(errs)
	successes := 0
	for e := range errs {
		if e == nil {
			successes++
		}
	}
	if successes != 2 {
		t.Fatalf("expected two admitted immutable archives, got %d", successes)
	}
	entries, e := os.ReadDir(c.Root)
	if e != nil {
		t.Fatal(e)
	}
	count := 0
	var total int64
	for _, entry := range entries {
		if entry.Name() == ".lock" {
			continue
		}
		s, e := entry.Info()
		if e != nil {
			t.Fatal(e)
		}
		count++
		total += s.Size()
	}
	if count != 2 || total > c.MaxTotalBytes {
		t.Fatalf("global quota violated: entries=%d bytes=%d", count, total)
	}
}

func TestRejectInRootSymlinksForArchivesAndLock(t *testing.T) {
	for _, kind := range []string{"archive", "lock"} {
		t.Run(kind, func(t *testing.T) {
			c, p, r := fixture(t)
			b, e := os.ReadFile(p)
			if e != nil {
				t.Fatal(e)
			}
			if e = os.WriteFile(filepath.Join(c.Root, "private-target"), b, 0600); e != nil {
				t.Fatal(e)
			}
			n, _ := name(c, r)
			if kind == "lock" {
				n = ".lock"
			}
			if e = os.Symlink("private-target", filepath.Join(c.Root, n)); e != nil {
				t.Fatal(e)
			}
			if e = Export(context.Background(), c, p, r); e == nil {
				t.Fatal("in-root symlink accepted")
			}
			if kind == "archive" {
				if _, e = Resolve(c, r); e == nil {
					t.Fatal("read-only resolver accepted in-root symlink")
				}
			}
			got, e := os.ReadFile(filepath.Join(c.Root, "private-target"))
			if e != nil || string(got) != string(b) {
				t.Fatal("symlink target modified", e)
			}
		})
	}
}
func TestStagingIsNotGarbageCollected(t *testing.T) {
	c, p, r := fixture(t)
	stale := filepath.Join(c.Root, ".staging-foreign")
	os.WriteFile(stale, make([]byte, 64), 0600)
	if e := Export(context.Background(), c, p, r); e == nil {
		t.Fatal("staging bypassed quota")
	}
	if _, e := os.Stat(stale); e != nil {
		t.Fatal("foreign staging removed")
	}
}
