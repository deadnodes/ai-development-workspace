package agentconnect

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type filePlan struct {
	relative      string
	before, after []byte
	exists        bool
	mode          os.FileMode
}

func safePath(root *os.Root, relative string) error {
	if filepath.IsAbs(relative) || filepath.Clean(relative) != relative || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("invalid installation path")
	}
	current := ""
	parts := strings.Split(relative, string(filepath.Separator))
	for i, part := range parts {
		current = filepath.Join(current, part)
		info, err := root.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing symlink installation path: %s", relative)
		}
		if i < len(parts)-1 && !info.IsDir() {
			return fmt.Errorf("installation parent is not a directory: %s", relative)
		}
		if i == len(parts)-1 && !info.Mode().IsRegular() {
			return fmt.Errorf("installation target is not a regular file: %s", relative)
		}
	}
	return nil
}
func readPlan(root *os.Root, relative string) (filePlan, error) {
	p := filePlan{relative: relative, mode: 0600}
	if err := safePath(root, relative); err != nil {
		return p, err
	}
	path := relative
	info, err := root.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return p, nil
	}
	if err != nil {
		return p, err
	}
	if info.Size() > 2<<20 {
		return p, errors.New("existing installation file exceeds 2 MiB")
	}
	p.exists = true
	p.mode = info.Mode().Perm()
	p.before, err = root.ReadFile(path)
	return p, err
}

// Stage all outputs before replacing any file. Original files are renamed to
// same-directory backups and restored on any commit failure. This is not a
// multi-file crash transaction: an interrupted process can leave temporary files.
func commitPlans(ctx context.Context, root *os.Root, plans []filePlan) error {
	type staged struct {
		plan            filePlan
		temp, backup    string
		backed, applied bool
	}
	stagedFiles := []*staged{}
	createdDirs := []string{}
	cleanup := func() {
		for _, s := range stagedFiles {
			if s.temp != "" {
				_ = root.Remove(s.temp)
			}
			if s.backup != "" {
				_ = root.Remove(s.backup)
			}
		}
		for i := len(createdDirs) - 1; i >= 0; i-- {
			_ = root.Remove(createdDirs[i])
		}
	}
	defer cleanup()
	for _, plan := range plans {
		if bytes.Equal(plan.before, plan.after) && plan.exists {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := safePath(root, plan.relative); err != nil {
			return err
		}
		directory := ""
		parts := strings.Split(filepath.Dir(plan.relative), string(filepath.Separator))
		for _, part := range parts {
			if part == "." {
				continue
			}
			directory = filepath.Join(directory, part)
			if _, err := root.Lstat(directory); errors.Is(err, os.ErrNotExist) {
				if err = root.Mkdir(directory, 0700); err != nil {
					return err
				}
				createdDirs = append(createdDirs, directory)
			} else if err != nil {
				return err
			}
		}
		if err := safePath(root, plan.relative); err != nil {
			return err
		}
		f, err := createTemp(root, filepath.Dir(plan.relative), ".rcp-connect-stage-")
		if err != nil {
			return err
		}
		s := &staged{plan: plan, temp: filepath.Join(filepath.Dir(plan.relative), filepath.Base(f.Name()))}
		stagedFiles = append(stagedFiles, s)
		if err = f.Chmod(plan.mode); err == nil {
			_, err = f.Write(plan.after)
		}
		if err == nil {
			err = f.Sync()
		}
		closeErr := f.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			return err
		}
		if plan.exists {
			backup, err := createTemp(root, filepath.Dir(plan.relative), ".rcp-connect-backup-")
			if err != nil {
				return err
			}
			s.backup = filepath.Join(filepath.Dir(plan.relative), filepath.Base(backup.Name()))
			if err = backup.Close(); err != nil {
				return err
			}
		}
	}
	// Check optimistic file snapshots once all output bytes are staged.
	for _, s := range stagedFiles {
		now, err := readPlan(root, s.plan.relative)
		if err != nil {
			return err
		}
		if now.exists != s.plan.exists || !bytes.Equal(now.before, s.plan.before) || now.mode != s.plan.mode {
			return errors.New("project files changed during connect; retry")
		}
	}
	rollback := func(cause error) error {
		var failures []error
		for i := len(stagedFiles) - 1; i >= 0; i-- {
			s := stagedFiles[i]
			target := s.plan.relative
			if s.backed {
				if err := root.Rename(s.backup, target); err != nil {
					failures = append(failures, err)
					s.backup = ""
				}
			} else if s.applied {
				if err := root.Remove(target); err != nil {
					failures = append(failures, err)
				}
			}
		}
		if len(failures) > 0 {
			return fmt.Errorf("connect failed; rollback incomplete: %w", errors.Join(append([]error{cause}, failures...)...))
		}
		return cause
	}
	for _, s := range stagedFiles {
		if err := ctx.Err(); err != nil {
			return rollback(err)
		}
		if err := safePath(root, s.plan.relative); err != nil {
			return rollback(err)
		}
		target := s.plan.relative
		if s.plan.exists {
			if err := root.Rename(target, s.backup); err != nil {
				return rollback(err)
			}
			s.backed = true
		}
		if err := root.Rename(s.temp, target); err != nil {
			return rollback(err)
		}
		s.applied = true
		s.temp = ""
	}
	return nil
}

func createTemp(root *os.Root, directory, prefix string) (*os.File, error) {
	return root.OpenFile(filepath.Join(directory, prefix+rand.Text()), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
}
