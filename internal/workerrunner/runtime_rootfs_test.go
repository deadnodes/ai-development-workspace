package workerrunner

import (
	"archive/tar"
	"bytes"
	"io"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/ociverify"
	"strings"
	"testing"
)

func TestRuntimeBuildContextOnlyGeneratedScratchLocalArchive(t *testing.T) {
	e := &domain.WorkerExecution{RuntimeRootFS: &domain.WorkerRuntimeRootFS{}, ImagePath: "app", Entrypoint: []string{"/app"}, OutputBytes: 1 << 20}
	b, err := buildContextRuntime(e, []byte("compiled"), []ociverify.RuntimeEntry{{Name: "public-ca", Mode: 0444, Data: []byte("CA")}})
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{}
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			t.Fatal(e)
		}
		files[h.Name], _ = io.ReadAll(tr)
	}
	df := string(files["Dockerfile"])
	if len(files) != 2 || files["rootfs.tar"] == nil || !strings.HasPrefix(df, "FROM scratch\nADD [\"rootfs.tar\",\"/\"]\n") || strings.Contains(df, "RUN ") || strings.Contains(df, "https:") {
		t.Fatal("runtime escaped bounded assembly", df)
	}
	e.ImagePath = "public-ca"
	if _, err = buildContextRuntime(e, []byte("compiled"), []ociverify.RuntimeEntry{{Name: "public-ca", Mode: 0444, Data: []byte("old")}}); err == nil {
		t.Fatal("runtime bundle overwrote compiled output")
	}
}
