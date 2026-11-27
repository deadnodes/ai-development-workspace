//go:build !windows

package ociverify

import (
	"io"
	"os"
	"path/filepath"
	"releasecontrol/internal/domain"
	"strings"
	"syscall"
)

// NormalizeRuntimeRootFS is operator-only preparation. It flattens links only
// after proving their resolved target remains inside the selected runtime tree.
func NormalizeRuntimeRootFS(sourceDir, outputFile string, maxExpanded int64, maxEntries int) (domain.WorkerRuntimeRootFS, error) {
	var spec domain.WorkerRuntimeRootFS
	if maxExpanded < 1 || maxExpanded > MaxRuntimeBytes || maxEntries < 1 || maxEntries > 20000 {
		return spec, errRuntime
	}
	root, e := filepath.EvalSymlinks(sourceDir)
	if e != nil {
		return spec, errRuntime
	}
	root, e = filepath.Abs(root)
	if e != nil {
		return spec, errRuntime
	}
	out, e := filepath.Abs(outputFile)
	if e == nil {
		parent, err := filepath.EvalSymlinks(filepath.Dir(out))
		if err != nil {
			return spec, errRuntime
		}
		out = filepath.Join(parent, filepath.Base(out))
	}
	if e != nil || out == root || strings.HasPrefix(out, root+string(os.PathSeparator)) {
		return spec, errRuntime
	}
	var total int64
	entries := []RuntimeEntry{}
	active := map[string]bool{}
	var walk func(string, string) error
	walk = func(n, physical string) error {
		resolved, e := filepath.EvalSymlinks(physical)
		if e != nil || resolved != root && !strings.HasPrefix(resolved, root+string(os.PathSeparator)) {
			return errRuntime
		}
		st, e := os.Stat(resolved)
		if e != nil || st.Mode()&os.ModeSetuid != 0 || st.Mode()&os.ModeSetgid != 0 || st.Mode()&os.ModeSticky != 0 {
			return errRuntime
		}
		if n != "" {
			if !safeRuntimeName(n) || len(entries) >= maxEntries {
				return errRuntime
			}
		}
		if st.IsDir() {
			if active[resolved] {
				return errRuntime
			}
			active[resolved] = true
			defer delete(active, resolved)
			if n != "" {
				entries = append(entries, RuntimeEntry{Name: n, Mode: 0555, Directory: true})
			}
			directory, e := os.Open(resolved)
			if e != nil {
				return errRuntime
			}
			children, e := directory.ReadDir(maxEntries + 1)
			directory.Close()
			if e == io.EOF {
				e = nil
			}
			if e != nil {
				return errRuntime
			}
			if len(children) > maxEntries {
				return errRuntime
			}
			for _, child := range children {
				next := child.Name()
				if n != "" {
					next = n + "/" + next
				}
				if e = walk(next, filepath.Join(resolved, child.Name())); e != nil {
					return e
				}
			}
			return nil
		}
		if !st.Mode().IsRegular() || st.Size() < 0 || st.Size() > maxExpanded-total {
			return errRuntime
		}
		if stat, ok := st.Sys().(*syscall.Stat_t); !ok || stat.Nlink != 1 {
			return errRuntime
		}
		f, e := os.OpenFile(resolved, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if e != nil {
			return errRuntime
		}
		actual, e := f.Stat()
		if e != nil || !os.SameFile(st, actual) {
			f.Close()
			return errRuntime
		}
		data, e := io.ReadAll(io.LimitReader(f, maxExpanded-total+1))
		f.Close()
		if e != nil || int64(len(data)) != st.Size() {
			return errRuntime
		}
		total += int64(len(data))
		mode := int64(0444)
		if st.Mode().Perm()&0111 != 0 {
			mode = 0555
		}
		entries = append(entries, RuntimeEntry{Name: n, Mode: mode, Data: data})
		return nil
	}
	if e = walk("", root); e != nil {
		return spec, e
	}
	sortRuntime(entries)
	if len(entries) == 0 || validateTree(entries) != nil {
		return spec, errRuntime
	}
	b, e := runtimeTar(entries, MaxRuntimeBytes)
	if e != nil {
		return spec, e
	}
	spec = domain.WorkerRuntimeRootFS{Hash: digestBytes(b), ArchiveBytes: int64(len(b)), MaxExpandedBytes: maxExpanded, MaxEntries: maxEntries, InputsHash: runtimeIdentity(entries)}
	f, e := os.OpenFile(out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return spec, e
	}
	ok := false
	defer func() {
		f.Close()
		if !ok {
			os.Remove(out)
		}
	}()
	if _, e = f.Write(b); e == nil {
		e = f.Sync()
	}
	if e != nil {
		return spec, e
	}
	if e = f.Close(); e != nil {
		return spec, e
	}
	d, e := os.Open(filepath.Dir(out))
	if e != nil {
		return spec, e
	}
	e = d.Sync()
	d.Close()
	ok = e == nil
	return spec, e
}
