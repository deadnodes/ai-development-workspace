// Package artifactspool provides an operator-owned local CAS handoff. It is not
// control-plane storage: only trusted supervisors write it, outside containers.
package artifactspool

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"releasecontrol/internal/domain"
)

type Config struct {
	Root            string `json:"root"`
	MaxArchiveBytes int64  `json:"max_archive_bytes"`
	MaxTotalBytes   int64  `json:"max_total_bytes"`
	MaxEntries      int    `json:"max_entries"`
}

var invalid = errors.New("invalid or unavailable artifact spool")

// stageError keeps operator diagnostics useful without exposing local paths.
func stageError(stage string, err error) error {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return fmt.Errorf("%w: %s: %s", invalid, stage, errno.Error())
	}
	return fmt.Errorf("%w: %s", invalid, stage)
}

var digest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// Validate checks configuration without creating storage.
func Validate(c Config) error {
	if !filepath.IsAbs(c.Root) || filepath.Clean(c.Root) != c.Root || c.MaxArchiveBytes < 1 || c.MaxArchiveBytes > 256<<20 || c.MaxTotalBytes < c.MaxArchiveBytes || c.MaxEntries < 1 || c.MaxEntries > 100000 {
		return invalid
	}
	return nil
}
func open(c Config) (*os.Root, error) {
	if Validate(c) != nil {
		return nil, stageError("configuration", nil)
	}
	p, e := filepath.EvalSymlinks(c.Root)
	if e != nil || p != c.Root {
		return nil, stageError("root canonical path", e)
	}
	r, e := os.OpenRoot(c.Root)
	if e != nil {
		return nil, stageError("root open", e)
	}
	f, e := r.Open(".")
	if e != nil {
		r.Close()
		return nil, stageError("root directory/private", e)
	}
	defer f.Close()
	s, e := f.Stat()
	if e != nil || !s.IsDir() || s.Mode().Perm()&0077 != 0 {
		r.Close()
		return nil, stageError("root directory/private", e)
	}
	return r, nil
}
func name(c Config, receipt domain.WorkerLocalOCI) (string, error) {
	if !digest.MatchString(receipt.ArchiveHash) || receipt.ArchiveBytes < 1 || receipt.ArchiveBytes > c.MaxArchiveBytes {
		return "", invalid
	}
	return strings.TrimPrefix(receipt.ArchiveHash, "sha256:") + ".oci.tar", nil
}
func regular(r *os.Root, n string) (*os.File, error) {
	f, e := r.OpenFile(n, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if e != nil {
		return nil, e
	}
	if e := privateRegular(r, n, f); e != nil {
		f.Close()
		return nil, e
	}
	return f, nil
}

func privateRegular(r *os.Root, n string, f *os.File) error {
	s, e := f.Stat()
	if e != nil || !s.Mode().IsRegular() || s.Mode().Perm()&0077 != 0 {
		return invalid
	}
	// os.Root confines symlinks to its tree; it can still follow an in-root
	// symlink. Reject those too, and prove the opened inode matches the path.
	p, e := r.Lstat(n)
	if e != nil || !p.Mode().IsRegular() || !os.SameFile(s, p) {
		return invalid
	}
	return nil
}

func openLock(r *os.Root) (*os.File, error) {
	// Separate creation from opening. Concurrent non-exclusive O_CREATE through
	// os.Root may return ENOENT on Darwin while the initial name is installed.
	// O_EXCL has one creator; every loser must observe that exact existing inode.
	f, e := r.OpenFile(".lock", os.O_CREATE|os.O_EXCL|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if os.IsExist(e) {
		f, e = r.OpenFile(".lock", os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	}
	if e != nil {
		return nil, stageError("lock open", e)
	}
	if e = privateRegular(r, ".lock", f); e != nil {
		f.Close()
		return nil, stageError("lock regular/private", e)
	}
	return f, nil
}
func verify(ctx context.Context, f *os.File, receipt domain.WorkerLocalOCI) error {
	s, e := f.Stat()
	if e != nil || s.Size() != receipt.ArchiveBytes {
		return invalid
	}
	h := sha256.New()
	n, e := io.Copy(h, &contextReader{ctx, io.LimitReader(f, receipt.ArchiveBytes+1)})
	if e != nil {
		return e
	}
	if n != receipt.ArchiveBytes || "sha256:"+hex.EncodeToString(h.Sum(nil)) != receipt.ArchiveHash {
		return invalid
	}
	return nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if e := r.ctx.Err(); e != nil {
		return 0, e
	}
	return r.r.Read(p)
}

type syncer func(*os.File) error

func syncFile(f *os.File) error { return f.Sync() }

// Export installs immutable bytes without overwrite, under a global quota lock.
// File and directory fsync acknowledgements precede successful return, including replay.
func Export(ctx context.Context, c Config, archive string, receipt domain.WorkerLocalOCI) error {
	return export(ctx, c, archive, receipt, syncFile)
}
func export(ctx context.Context, c Config, archive string, receipt domain.WorkerLocalOCI, sync syncer) error {
	n, e := name(c, receipt)
	if e != nil {
		return e
	}
	r, e := open(c)
	if e != nil {
		return e
	}
	defer r.Close()
	lock, e := openLock(r)
	if e != nil {
		return e
	}
	defer lock.Close()
	for {
		e = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if e == nil {
			break
		}
		if e != syscall.EWOULDBLOCK && e != syscall.EAGAIN {
			return e
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	dir, e := r.Open(".")
	if e != nil {
		return e
	}
	defer dir.Close()
	if f, e := regular(r, n); e == nil {
		defer f.Close()
		if e = verify(ctx, f, receipt); e != nil {
			return stageError("existing entry verification", e)
		}
		if e = sync(f); e != nil {
			return e
		}
		return sync(dir)
	} else if !os.IsNotExist(e) {
		return stageError("existing entry open", e)
	}
	entries, e := dir.ReadDir(c.MaxEntries + 2)
	if e != nil && e != io.EOF {
		return e
	}
	var total int64
	count := 0
	for _, entry := range entries {
		if entry.Name() == ".lock" {
			continue
		}
		// Unknown/stale staging is never removed; it consumes quota and fails closed.
		s, e := r.Lstat(entry.Name())
		if e != nil || !s.Mode().IsRegular() || s.Mode().Perm()&0077 != 0 {
			return stageError("inventory regular/private", e)
		}
		count++
		if s.Size() > c.MaxTotalBytes-total {
			return errors.New("artifact spool byte quota exceeded")
		}
		total += s.Size()
	}
	if count >= c.MaxEntries || receipt.ArchiveBytes > c.MaxTotalBytes-total {
		return errors.New("artifact spool quota exceeded")
	}
	source, e := os.OpenFile(archive, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if e != nil {
		return invalid
	}
	defer source.Close()
	st, e := source.Stat()
	if e != nil || !st.Mode().IsRegular() || st.Size() != receipt.ArchiveBytes {
		return invalid
	}
	sourcePath, e := os.Lstat(archive)
	if e != nil || !sourcePath.Mode().IsRegular() || !os.SameFile(st, sourcePath) {
		return stageError("source archive regular/inode", e)
	}
	var nonce [16]byte
	if _, e = rand.Read(nonce[:]); e != nil {
		return e
	}
	temp := ".staging-" + hex.EncodeToString(nonce[:])
	f, e := r.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if e != nil {
		return e
	}
	defer f.Close()
	defer r.Remove(temp)
	h := sha256.New()
	copied, e := io.Copy(io.MultiWriter(f, h), &contextReader{ctx, io.LimitReader(source, receipt.ArchiveBytes+1)})
	if e != nil {
		return e
	}
	if copied != receipt.ArchiveBytes || fmt.Sprintf("sha256:%x", h.Sum(nil)) != receipt.ArchiveHash {
		return invalid
	}
	if e = sync(f); e != nil {
		return e
	}
	// Hard-link publishes only if absent, atomically; never rename over an entry.
	if e = r.Link(temp, n); e != nil {
		return e
	}
	if e = r.Remove(temp); e != nil {
		return e
	}
	return sync(dir)
}

// Resolve only reads a receipt-derived filename. No locks, staging, journals or
// fsync writes are attempted, so the publisher can mount this root read-only.
func Resolve(c Config, receipt domain.WorkerLocalOCI) (string, error) {
	n, e := name(c, receipt)
	if e != nil {
		return "", e
	}
	r, e := open(c)
	if e != nil {
		return "", e
	}
	defer r.Close()
	f, e := regular(r, n)
	if e != nil {
		return "", invalid
	}
	defer f.Close()
	if e = verify(context.Background(), f, receipt); e != nil {
		return "", e
	}
	return filepath.Join(c.Root, n), nil
}
