package ociverify

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"releasecontrol/internal/domain"
	"sort"
	"strings"
	"syscall"
	"time"
)

const RuntimeBundlePath = "/opt/rcp/runtime-rootfs.tar"
const MaxRuntimeBytes int64 = 128 << 20

type RuntimeEntry struct {
	Name      string
	Mode      int64
	Directory bool
	Data      []byte
}

var errRuntime = errors.New("invalid or unsafe runtime rootfs bundle")

func safeRuntimeName(n string) bool {
	if n == "" || len(n) > 255 || path.Clean(n) != n || n == "." || strings.HasPrefix(n, "/") || n == ".." || strings.HasPrefix(n, "../") || strings.ContainsAny(n, "\\\x00\r\n") {
		return false
	}
	for _, p := range strings.Split(n, "/") {
		if strings.HasPrefix(p, ".wh.") || p == ".git" || p == ".ssh" || p == ".local" || p == ".codex" || p == ".agents" || p == ".env" || strings.HasPrefix(p, ".env.") || p == ".npmrc" || p == ".netrc" || p == "id_rsa" || p == "id_ed25519" || strings.HasSuffix(p, ".key") || p == "state.db" || p == "journal.json" {
			return false
		}
	}
	return true
}
func runtimeIdentity(entries []RuntimeEntry) string {
	type item struct {
		Name      string
		Mode      int64
		Directory bool
		Size      int
		Hash      string
	}
	manifest := []item{}
	for _, e := range entries {
		manifest = append(manifest, item{e.Name, e.Mode, e.Directory, len(e.Data), digestBytes(e.Data)})
	}
	b, _ := json.Marshal(manifest)
	return digestBytes(b)
}
func sortRuntime(entries []RuntimeEntry) {
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
}
func validateTree(entries []RuntimeEntry) error {
	types := map[string]bool{}
	for _, e := range entries {
		if _, ok := types[e.Name]; ok {
			return errRuntime
		}
		types[e.Name] = e.Directory
	}
	for _, e := range entries {
		for p := path.Dir(e.Name); p != "."; p = path.Dir(p) {
			if dir, ok := types[p]; !ok || !dir {
				return errRuntime
			}
		}
	}
	return nil
}

// RuntimeBundle verifies immutable serialization, complete normalized input
// identity and bounded tree. Bundle archives never contain symbolic/hard links.
func RuntimeBundle(b []byte, spec *domain.WorkerRuntimeRootFS) ([]RuntimeEntry, error) {
	if spec == nil || spec.ArchiveBytes < 1 || spec.ArchiveBytes > MaxRuntimeBytes || int64(len(b)) != spec.ArchiveBytes || digestBytes(b) != spec.Hash || spec.MaxExpandedBytes < 1 || spec.MaxExpandedBytes > MaxRuntimeBytes || spec.MaxEntries < 1 || spec.MaxEntries > 20000 {
		return nil, errRuntime
	}
	tr := tar.NewReader(bytes.NewReader(b))
	entries := []RuntimeEntry{}
	var total int64
	for {
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return nil, errRuntime
		}
		n := strings.TrimSuffix(h.Name, "/")
		dir := h.Typeflag == tar.TypeDir
		if !safeRuntimeName(n) || len(entries) >= spec.MaxEntries || h.Size < 0 || h.Size > spec.MaxExpandedBytes-total || h.Mode&07000 != 0 || len(h.PAXRecords) > 0 || len(h.Xattrs) > 0 || h.Linkname != "" || h.Uname != "" || h.Gname != "" || h.Uid != 0 || h.Gid != 0 || h.Devmajor != 0 || h.Devminor != 0 || !h.ModTime.Equal(time.Unix(0, 0)) || !h.AccessTime.IsZero() || !h.ChangeTime.IsZero() || (dir && (h.Mode != 0555 || h.Size != 0)) || (!dir && (h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeRegA || h.Mode != 0444 && h.Mode != 0555)) {
			return nil, errRuntime
		}
		data, e := io.ReadAll(io.LimitReader(tr, h.Size+1))
		if e != nil || int64(len(data)) != h.Size {
			return nil, errRuntime
		}
		total += h.Size
		entries = append(entries, RuntimeEntry{n, h.Mode, dir, data})
	}
	sortRuntime(entries)
	if len(entries) == 0 || validateTree(entries) != nil || runtimeIdentity(entries) != spec.InputsHash {
		return nil, errRuntime
	}
	canonical, e := runtimeTar(entries, MaxRuntimeBytes)
	if e != nil || !bytes.Equal(canonical, b) {
		return nil, errRuntime
	}
	return entries, nil
}
func runtimeTar(entries []RuntimeEntry, max int64) ([]byte, error) {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, e := range entries {
		kind := byte(tar.TypeReg)
		n := e.Name
		if e.Directory {
			kind = tar.TypeDir
			n += "/"
		}
		h := &tar.Header{Name: n, Mode: e.Mode, Size: int64(len(e.Data)), Typeflag: kind, ModTime: time.Unix(0, 0), Format: tar.FormatUSTAR}
		if err := tw.WriteHeader(h); err != nil {
			return nil, errRuntime
		}
		if _, err := tw.Write(e.Data); err != nil {
			return nil, err
		}
		if int64(b.Len()) > max {
			return nil, errRuntime
		}
	}
	if e := tw.Close(); e != nil {
		return nil, e
	}
	if int64(b.Len()) > max {
		return nil, errRuntime
	}
	return b.Bytes(), nil
}

// MergeRuntime produces the sole complete expected image layer. Even directory
// entries cannot collide with, conceal or overwrite the compiled executable.
func MergeRuntime(entries []RuntimeEntry, imagePath string, compiled []byte, max int64) ([]byte, []RuntimeEntry, error) {
	if !safeRuntimeName(imagePath) || len(compiled) == 0 {
		return nil, nil, errRuntime
	}
	merged := append([]RuntimeEntry(nil), entries...)
	known := map[string]bool{}
	for _, e := range entries {
		known[e.Name] = e.Directory
		if e.Name == imagePath || strings.HasPrefix(e.Name, imagePath+"/") || strings.HasPrefix(imagePath, e.Name+"/") && !e.Directory {
			return nil, nil, errRuntime
		}
	}
	for p := path.Dir(imagePath); p != "."; p = path.Dir(p) {
		if _, ok := known[p]; !ok {
			merged = append(merged, RuntimeEntry{Name: p, Mode: 0555, Directory: true})
			known[p] = true
		}
	}
	merged = append(merged, RuntimeEntry{Name: imagePath, Mode: 0555, Data: compiled})
	sortRuntime(merged)
	if validateTree(merged) != nil {
		return nil, nil, errRuntime
	}
	b, e := runtimeTar(merged, max)
	return b, merged, e
}
func verifyRuntimeLayer(b []byte, expected []RuntimeEntry, max int64) error {
	want := map[string]RuntimeEntry{}
	for _, e := range expected {
		want[e.Name] = e
	}
	seen := map[string]bool{}
	reader := bytes.NewReader(b)
	tr := tar.NewReader(reader)
	for {
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return errRuntime
		}
		n := strings.TrimSuffix(h.Name, "/")
		v, ok := want[n]
		if !ok || seen[n] || !safeRuntimeName(n) || h.Mode != v.Mode || h.Uid != 0 || h.Gid != 0 || h.Linkname != "" || len(h.PAXRecords) > 0 || len(h.Xattrs) > 0 || (v.Directory && (h.Typeflag != tar.TypeDir || h.Size != 0)) || (!v.Directory && (h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeRegA || h.Size != int64(len(v.Data)))) {
			return errRuntime
		}
		data, e := io.ReadAll(io.LimitReader(tr, max+1))
		if e != nil || !bytes.Equal(data, v.Data) {
			return errRuntime
		}
		seen[n] = true
	}
	if len(seen) != len(want) {
		return errRuntime
	}
	// A second archive or hidden nonzero payload after tar's end marker is not
	// part of the expected runtime tree. Permit only bounded block padding.
	if reader.Len() > 10240 {
		return errRuntime
	}
	padding := make([]byte, reader.Len())
	reader.Read(padding)
	for _, v := range padding {
		if v != 0 {
			return errRuntime
		}
	}
	return nil
}

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
