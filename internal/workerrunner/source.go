package workerrunner

import (
	"archive/tar"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

var fullCommit = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

func cleanEnv() []string {
	return []string{"PATH=" + os.Getenv("PATH"), "HOME=/nonexistent", "LANG=C", "LC_ALL=C", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "GIT_NO_REPLACE_OBJECTS=1", "GIT_SSH_COMMAND=false"}
}
func materialize(ctx context.Context, root, mirror, repository, sha string, max int64) (string, error) {
	if !fullCommit.MatchString(sha) {
		return "", errors.New("invalid source identity")
	}
	gitDir := filepath.Join(root, "object-view.git")
	gitEnv := cleanEnv()
	run := func(args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Env = gitEnv
		out := &boundedBuffer{remaining: 1 << 20}
		cmd.Stdout = out
		cmd.Stderr = &boundedBuffer{remaining: 1 << 20}
		e := cmd.Run()
		if e != nil {
			return nil, errors.New("source unavailable")
		}
		return out.Bytes(), nil
	}
	// The first offline executor uses operator-preseeded object storage. An
	// arbitrary public fetch would need its own enforced pack/storage budget.
	if mirror == "" {
		return "", errors.New("bounded offline execution requires an operator Git mirror")
	}
	objects := filepath.Join(mirror, "objects")
	info, e := os.Stat(objects)
	if e != nil || !info.IsDir() {
		return "", errors.New("operator Git object mirror unavailable")
	}
	// Read objects through a fresh bare view. Never load the mirror's config,
	// hooks, info/attributes or worktree metadata into source materialization.
	if _, e = run("init", "--bare", gitDir); e != nil {
		return "", e
	}
	gitEnv = append(gitEnv, "GIT_OBJECT_DIRECTORY="+objects)
	b, e := run("--git-dir", gitDir, "rev-parse", "--verify", sha+"^{commit}")
	if e != nil || strings.TrimSpace(string(b)) != sha {
		return "", errors.New("source object mismatch")
	}
	if _, e = run("--git-dir", gitDir, "-c", "core.hooksPath=/dev/null", "fsck", "--strict", "--no-reflogs", "--no-dangling", sha); e != nil {
		return "", errors.New("source object verification failed")
	}
	destination := filepath.Join(root, "source")
	if e = os.Mkdir(destination, 0755); e != nil {
		return "", e
	}
	cmd := exec.CommandContext(ctx, "git", "--git-dir", gitDir, "-c", "core.hooksPath=/dev/null", "archive", "--format=tar", sha)
	cmd.Env = gitEnv
	pipe, e := cmd.StdoutPipe()
	if e != nil {
		return "", e
	}
	if e = cmd.Start(); e != nil {
		return "", e
	}
	e = extractSource(pipe, destination, max)
	if e != nil {
		_ = cmd.Process.Kill()
	}
	wait := cmd.Wait()
	if e != nil {
		return "", e
	}
	if wait != nil {
		return "", errors.New("source extraction failed")
	}
	return destination, nil
}
func extractSource(reader io.Reader, destination string, max int64) error {
	limited := &io.LimitedReader{R: reader, N: max + 1}
	tr := tar.NewReader(limited)
	var total int64
	count := 0
	for {
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return errors.New("invalid source archive")
		}
		count++
		if h.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		name := strings.TrimSuffix(h.Name, "/")
		if count > 20000 || name == "" || filepath.IsAbs(name) || filepath.Clean(name) != name || name == ".." || strings.HasPrefix(name, "../") || strings.Contains(name, "\\") {
			return errors.New("unsafe source path")
		}
		for _, part := range strings.Split(name, "/") {
			if part == ".git" {
				return errors.New("git metadata forbidden in snapshot")
			}
		}
		p := filepath.Join(destination, name)
		switch h.Typeflag {
		case tar.TypeDir:
			if e = os.MkdirAll(p, 0755); e != nil {
				return e
			}
		case tar.TypeReg, tar.TypeRegA:
			total += h.Size
			if h.Size < 0 || total > max {
				return errors.New("source exceeds bound")
			}
			if e = os.MkdirAll(filepath.Dir(p), 0755); e != nil {
				return e
			}
			f, e := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, os.FileMode(h.Mode)&0555|0444)
			if e != nil {
				return e
			}
			_, e = io.Copy(f, tr)
			closeErr := f.Close()
			if e != nil || closeErr != nil {
				return errors.New("source write failed")
			}
		default:
			return errors.New("links and special files forbidden in snapshot")
		}
	}
	if limited.N <= 0 {
		return errors.New("source archive exceeds bound")
	}
	return nil
}
