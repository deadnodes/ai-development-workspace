package workerrunner

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func paddedSourceArchive(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	if err := tw.WriteHeader(&tar.Header{Name: "input.txt", Mode: 0644, Size: 5}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("input")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return append(b.Bytes(), make([]byte, 128<<10)...)
}

func TestSourceArchiveDrainsProducerBeforeWaiting(t *testing.T) {
	archive := paddedSourceArchive(t)
	r, w := io.Pipe()
	var wg sync.WaitGroup
	done := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := w.Write(archive)
		_ = w.CloseWithError(err)
		done <- err
	}()
	t.Cleanup(func() {
		_ = r.Close()
		_ = w.Close()
		wg.Wait()
	})
	dir := t.TempDir()
	if err := extractSource(r, dir, int64(len(archive))); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("tar EOF left producer blocked writing archive tail before Wait")
	}
	if b, err := os.ReadFile(filepath.Join(dir, "input.txt")); err != nil || string(b) != "input" {
		t.Fatal("source changed", string(b), err)
	}
}

type sourceTailFailure struct{ err error }

func (r sourceTailFailure) Read([]byte) (int, error) { return 0, r.err }

func TestSourceArchiveTailUsesOriginalBudgetAndErrors(t *testing.T) {
	archive := paddedSourceArchive(t)
	t.Run("exact budget", func(t *testing.T) {
		if err := extractSource(bytes.NewReader(archive), t.TempDir(), int64(len(archive))); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("budget plus one", func(t *testing.T) {
		r := bytes.NewReader(archive)
		if err := extractSource(r, t.TempDir(), int64(len(archive)-1)); err == nil {
			t.Fatal("archive tail exceeded original budget")
		}
	})
	t.Run("tail read error", func(t *testing.T) {
		want := errors.New("fixture tail read failure")
		r := io.MultiReader(bytes.NewReader(archive), sourceTailFailure{want})
		if err := extractSource(r, t.TempDir(), int64(len(archive)+1024)); !errors.Is(err, want) {
			t.Fatal("tail read error lost", err)
		}
	})
	t.Run("tail stops at budget", func(t *testing.T) {
		data := append(append([]byte{}, archive...), make([]byte, 1<<20)...)
		r := bytes.NewReader(data)
		limit := len(archive) + 1024
		if err := extractSource(r, t.TempDir(), int64(limit)); err == nil {
			t.Fatal("oversized tail accepted")
		}
		if consumed := len(data) - r.Len(); consumed != limit+1 {
			t.Fatal("tail read exceeded original limit plus sentinel", consumed)
		}
	})
}

func TestSourceArchiveTailCancellation(t *testing.T) {
	archive := paddedSourceArchive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	r, w := io.Pipe()
	// Close the producer: its error is delivered to the blocked reader. Closing
	// the reader instead delivers ctx.Err to writes and makes reads ErrClosedPipe.
	stop := context.AfterFunc(ctx, func() { _ = w.CloseWithError(ctx.Err()) })
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = w.Write(archive)
		// Keep stdout open, as a producer that stops making progress would.
		<-ctx.Done()
		_ = w.CloseWithError(ctx.Err())
	}()
	t.Cleanup(func() {
		stop()
		cancel()
		_ = r.Close()
		_ = w.Close()
		wg.Wait()
	})
	if err := extractSource(r, t.TempDir(), 1<<20); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("tail cancellation lost", err)
	}
}

func TestSourceArchiveTailCancellationDirection(t *testing.T) {
	for _, producer := range []bool{false, true} {
		name := "reader-close-control"
		want := error(io.ErrClosedPipe)
		if producer {
			name, want = "producer-close-cancellation", context.Canceled
		}
		t.Run(name, func(t *testing.T) {
			archive := paddedSourceArchive(t)
			ctx, cancel := context.WithCancel(context.Background())
			r, w := io.Pipe()
			stop := context.AfterFunc(ctx, func() {
				if producer {
					_ = w.CloseWithError(ctx.Err())
				} else {
					_ = r.CloseWithError(ctx.Err())
				}
			})
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _ = w.Write(archive)
				// Cancel only once the complete archive was consumed. No timer or
				// concurrent writer-close can hide which pipe direction is tested.
				cancel()
			}()
			t.Cleanup(func() {
				stop()
				cancel()
				_ = r.Close()
				_ = w.Close()
				wg.Wait()
			})
			if err := extractSource(r, t.TempDir(), 1<<20); !errors.Is(err, want) {
				t.Fatalf("tail error direction: got %v, want %v", err, want)
			}
		})
	}
}

func TestMaterializeRealGitArchiveCompletes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = repo
		cmd.Env = cleanEnv()
		b, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatal("fixture Git", err, string(b))
		}
		return strings.TrimSpace(string(b))
	}
	git("init")
	payload := bytes.Repeat([]byte("input"), 40000)
	if err := os.WriteFile(filepath.Join(repo, "input.txt"), payload, 0600); err != nil {
		t.Fatal(err)
	}
	git("add", "input.txt")
	git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null", "commit", "-m", "Fixture")
	sha := git("rev-parse", "HEAD")
	source, err := materialize(ctx, t.TempDir(), filepath.Join(repo, ".git"), "fixture", sha, 1<<20)
	if err != nil {
		t.Fatal("real Git materialization", err)
	}
	if b, err := os.ReadFile(filepath.Join(source, "input.txt")); err != nil || !bytes.Equal(b, payload) {
		t.Fatal("real source bytes changed", err)
	}
}
