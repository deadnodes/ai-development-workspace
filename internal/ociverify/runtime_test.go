package ociverify

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"releasecontrol/internal/domain"
	"strings"
	"syscall"
	"testing"
	"time"
)

func runtimeFixture(t *testing.T) ([]byte, *domain.WorkerRuntimeRootFS, []RuntimeEntry) {
	t.Helper()
	entries := []RuntimeEntry{{Name: "etc", Mode: 0555, Directory: true}, {Name: "etc/ca.pem", Mode: 0444, Data: []byte("public CA fixture")}, {Name: "usr", Mode: 0555, Directory: true}, {Name: "usr/bin", Mode: 0555, Directory: true}, {Name: "usr/bin/git", Mode: 0555, Data: []byte("git fixture")}}
	b, e := runtimeTar(entries, 1<<20)
	if e != nil {
		t.Fatal(e)
	}
	s := &domain.WorkerRuntimeRootFS{Hash: digestBytes(b), ArchiveBytes: int64(len(b)), MaxExpandedBytes: 1 << 20, MaxEntries: 32, InputsHash: runtimeIdentity(entries)}
	return b, s, entries
}
func TestRuntimeBundleRoundTripAndDeterministicMergedLayer(t *testing.T) {
	b, s, entries := runtimeFixture(t)
	parsed, e := RuntimeBundle(b, s)
	if e != nil || runtimeIdentity(parsed) != runtimeIdentity(entries) {
		t.Fatal(e)
	}
	one, want, e := MergeRuntime(parsed, "usr/local/bin/server", []byte("new compiled bytes"), 1<<20)
	if e != nil {
		t.Fatal(e)
	}
	two, _, e := MergeRuntime(parsed, "usr/local/bin/server", []byte("new compiled bytes"), 1<<20)
	if e != nil || !bytes.Equal(one, two) {
		t.Fatal("nondeterministic layer", e)
	}
	if e = verifyRuntimeLayer(one, want, 1<<20); e != nil {
		t.Fatal(e)
	}
	for _, collision := range []string{"usr/bin/git", "usr", "etc/ca.pem/compiled"} {
		if _, _, e = MergeRuntime(parsed, collision, []byte("compiled"), 1<<20); e == nil {
			t.Fatal("runtime overwrote executable or linked ancestor", collision)
		}
	}
	if _, _, e = MergeRuntime(parsed, "app", []byte("compiled"), 1); e == nil {
		t.Fatal("merged output quota ignored")
	}
}
func TestRuntimeBundleRejectsPinsBoundsAndUnsafeHeaders(t *testing.T) {
	b, s, _ := runtimeFixture(t)
	for _, kind := range []string{"hash", "size", "expanded", "entries", "manifest", "corrupt", "trailing"} {
		t.Run(kind, func(t *testing.T) {
			c := *s
			data := append([]byte(nil), b...)
			switch kind {
			case "hash":
				c.Hash = digestBytes([]byte("other"))
			case "size":
				c.ArchiveBytes--
			case "expanded":
				c.MaxExpandedBytes = 1
			case "entries":
				c.MaxEntries = 1
			case "manifest":
				c.InputsHash = digestBytes(nil)
			case "corrupt":
				data[512] ^= 1
			case "trailing":
				data = append(data, []byte("hidden extra bytes")...)
				c.Hash = digestBytes(data)
				c.ArchiveBytes = int64(len(data))
			}
			if _, e := RuntimeBundle(data, &c); e == nil {
				t.Fatal("unverified bundle accepted")
			}
		})
	}
	for _, kind := range []string{"escape", "absolute", "duplicate", "symlink", "hardlink", "fifo", "device", "whiteout", "setuid", "metadata", "file-ancestor", "secret", "timestamp"} {
		t.Run(kind, func(t *testing.T) {
			h := &tar.Header{Name: "file", Typeflag: tar.TypeReg, Mode: 0444, Size: 1, ModTime: time.Unix(0, 0), Format: tar.FormatUSTAR}
			switch kind {
			case "escape":
				h.Name = "../file"
			case "absolute":
				h.Name = "/file"
			case "symlink":
				h.Typeflag = tar.TypeSymlink
				h.Linkname = "file"
				h.Size = 0
			case "hardlink":
				h.Typeflag = tar.TypeLink
				h.Linkname = "file"
				h.Size = 0
			case "fifo":
				h.Typeflag = tar.TypeFifo
				h.Size = 0
			case "device":
				h.Typeflag = tar.TypeChar
				h.Size = 0
			case "whiteout":
				h.Name = ".wh.file"
			case "setuid":
				h.Mode = 04444
			case "metadata":
				h.Format = tar.FormatPAX
				h.PAXRecords = map[string]string{"comment": "unexpected metadata"}
			case "secret":
				h.Name = "private.key"
			case "timestamp":
				h.ModTime = time.Unix(10, 0)
			}
			var raw bytes.Buffer
			tw := tar.NewWriter(&raw)
			if e := tw.WriteHeader(h); e != nil {
				t.Fatal(e)
			}
			if h.Size > 0 {
				tw.Write([]byte("x"))
			}
			if kind == "duplicate" {
				tw.WriteHeader(h)
				tw.Write([]byte("x"))
			}
			if kind == "file-ancestor" {
				h.Name = "file/child"
				tw.WriteHeader(h)
				tw.Write([]byte("x"))
			}
			tw.Close()
			c := *s
			c.Hash = digestBytes(raw.Bytes())
			c.ArchiveBytes = int64(raw.Len())
			if _, e := RuntimeBundle(raw.Bytes(), &c); e == nil {
				t.Fatal("unsafe runtime header accepted", kind)
			}
		})
	}
}
func TestNormalizeRuntimeFlattensOnlyInTreeLinksAndNoOverwrite(t *testing.T) {
	root := t.TempDir()
	os.Mkdir(filepath.Join(root, "lib"), 0755)
	os.WriteFile(filepath.Join(root, "lib", "real"), []byte("public runtime library"), 0644)
	os.Symlink("real", filepath.Join(root, "lib", "alias"))
	out := filepath.Join(t.TempDir(), "runtime.tar")
	s, e := NormalizeRuntimeRootFS(root, out, 1<<20, 32)
	if e != nil {
		t.Fatal(e)
	}
	b, _ := os.ReadFile(out)
	entries, e := RuntimeBundle(b, &s)
	if e != nil || len(entries) != 3 {
		t.Fatal("normalization failed", e)
	}
	second := filepath.Join(t.TempDir(), "runtime.tar")
	s2, e := NormalizeRuntimeRootFS(root, second, 1<<20, 32)
	if e != nil || s != s2 {
		t.Fatal("normalization identity drift", e)
	}
	if _, e = NormalizeRuntimeRootFS(root, out, 1<<20, 32); e == nil {
		t.Fatal("operator output overwritten")
	}
	for _, kind := range []string{"escape", "cycle", "fifo", "hardlink", "quota", "entries", "secret", "output-in-tree"} {
		t.Run(kind, func(t *testing.T) {
			r := t.TempDir()
			o := filepath.Join(t.TempDir(), "bundle")
			max := int64(1 << 20)
			count := 32
			switch kind {
			case "escape":
				os.Symlink(out, filepath.Join(r, "external"))
			case "cycle":
				os.Symlink(".", filepath.Join(r, "cycle"))
			case "hardlink":
				if err := os.Link(filepath.Join(root, "lib", "real"), filepath.Join(r, "alias")); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				syscall.Mkfifo(filepath.Join(r, "fifo"), 0600)
			case "quota":
				os.WriteFile(filepath.Join(r, "large"), []byte("too large"), 0600)
				max = 1
			case "entries":
				os.WriteFile(filepath.Join(r, "a"), nil, 0600)
				os.WriteFile(filepath.Join(r, "b"), nil, 0600)
				count = 1
			case "secret":
				os.WriteFile(filepath.Join(r, "private.key"), []byte("excluded"), 0600)
			case "output-in-tree":
				os.WriteFile(filepath.Join(r, "a"), nil, 0600)
				o = filepath.Join(r, "bundle")
			}
			if _, e := NormalizeRuntimeRootFS(r, o, max, count); e == nil {
				t.Fatal("unsafe operator preparation accepted")
			}
		})
	}
}
func ociRuntimeFixture(t *testing.T, layer []byte, e *domain.WorkerExecution, configOverride map[string]any) []byte {
	t.Helper()
	files := map[string][]byte{}
	add := func(media string, b []byte) map[string]any {
		d := digestBytes(b)
		files["blobs/sha256/"+strings.TrimPrefix(d, "sha256:")] = b
		return map[string]any{"mediaType": media, "digest": d, "size": len(b)}
	}
	runtime := map[string]any{"User": "65532:65532", "Entrypoint": e.Entrypoint, "Env": []string{"PATH=/usr/local/bin:/usr/bin:/bin"}, "WorkingDir": "/"}
	for k, v := range configOverride {
		runtime[k] = v
	}
	cfg, _ := json.Marshal(map[string]any{"os": "linux", "architecture": "arm64", "config": runtime, "rootfs": map[string]any{"type": "layers", "diff_ids": []string{digestBytes(layer)}}})
	manifest, _ := json.Marshal(map[string]any{"schemaVersion": 2, "config": add("application/vnd.oci.image.config.v1+json", cfg), "layers": []any{add("application/vnd.oci.image.layer.v1.tar", layer)}})
	idx, _ := json.Marshal(map[string]any{"schemaVersion": 2, "manifests": []any{add("application/vnd.oci.image.manifest.v1+json", manifest)}})
	files["index.json"] = idx
	files["oci-layout"] = []byte(`{"imageLayoutVersion":"1.0.0"}`)
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for n, data := range files {
		tw.WriteHeader(&tar.Header{Name: n, Mode: 0600, Size: int64(len(data))})
		tw.Write(data)
	}
	tw.Close()
	return b.Bytes()
}
func TestRuntimeOCIExactTreeCompiledBytesConfigAndReceipt(t *testing.T) {
	_, s, entries := runtimeFixture(t)
	exe := []byte("current compiled bytes")
	e := &domain.WorkerExecution{RuntimeRootFS: s, ImagePath: "app", Entrypoint: []string{"/app"}}
	layer, want, err := MergeRuntime(entries, e.ImagePath, exe, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	archive := ociRuntimeFixture(t, layer, e, nil)
	r, err := verifyOCIBytesRuntime(archive, 1<<20, "linux/arm64", e, exe, entries)
	if err != nil || r.RuntimeRootFS == nil || *r.RuntimeRootFS != *s {
		t.Fatal("runtime receipt not bound", err)
	}
	for _, kind := range []string{"extra", "corrupt", "missing", "mode", "compiled", "config", "pins", "unexpected-runtime", "trailing"} {
		t.Run(kind, func(t *testing.T) {
			changed := append([]RuntimeEntry(nil), want...)
			override := map[string]any{}
			execCopy := *e
			input := entries
			switch kind {
			case "extra":
				changed = append(changed, RuntimeEntry{Name: "extra", Mode: 0444, Data: []byte("extra")})
			case "corrupt":
				changed[0].Data = []byte("changed")
			case "missing":
				changed = changed[1:]
			case "mode":
				changed[0].Mode = 0777
			case "compiled":
				for i := range changed {
					if changed[i].Name == "app" {
						changed[i].Data = []byte("stale binary")
					}
				}
			case "config":
				override["Env"] = []string{"PATH=/attacker"}
			case "pins":
				copy := *s
				copy.InputsHash = digestBytes(nil)
				execCopy.RuntimeRootFS = &copy
			case "unexpected-runtime":
				execCopy.RuntimeRootFS = nil
			}
			sortRuntime(changed)
			raw, err := runtimeTar(changed, 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "trailing" {
				raw = append(raw, []byte("hidden layer bytes")...)
			}
			archive := ociRuntimeFixture(t, raw, &execCopy, override)
			if _, err = verifyOCIBytesRuntime(archive, 1<<20, "linux/arm64", &execCopy, exe, input); err == nil {
				t.Fatal("runtime tree/config/receipt mismatch accepted")
			}
		})
	}
}
