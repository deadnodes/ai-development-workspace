package workerrunner

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/workerconfig"
	"strings"
	"testing"
	"time"
)

func fixtureOCI(t *testing.T, platform string) (string, []byte, *domain.WorkerExecution) {
	t.Helper()
	payload := []byte("compiled-fixture")
	execution := &domain.WorkerExecution{Platform: platform, ImagePath: "app", Entrypoint: []string{"/app"}}
	var layer bytes.Buffer
	tw := tar.NewWriter(&layer)
	_ = tw.WriteHeader(&tar.Header{Name: "app", Mode: 0555, Size: int64(len(payload))})
	_, _ = tw.Write(payload)
	_ = tw.Close()
	data := map[string][]byte{}
	add := func(media string, b []byte) descriptor {
		hash := digestBytes(b)
		data["blobs/sha256/"+strings.TrimPrefix(hash, "sha256:")] = b
		return descriptor{MediaType: media, Digest: hash, Size: int64(len(b))}
	}
	parts := strings.Split(platform, "/")
	config, _ := json.Marshal(map[string]any{"os": parts[0], "architecture": parts[1], "config": map[string]any{"User": "65532:65532", "Entrypoint": []string{"/app"}}, "rootfs": map[string]any{"type": "layers", "diff_ids": []string{digestBytes(layer.Bytes())}}})
	m := imageManifest{SchemaVersion: 2, Config: add("application/vnd.oci.image.config.v1+json", config), Layers: []descriptor{add("application/vnd.oci.image.layer.v1.tar", layer.Bytes())}}
	manifest, _ := json.Marshal(m)
	md := add("application/vnd.oci.image.manifest.v1+json", manifest)
	index, _ := json.Marshal(imageIndex{SchemaVersion: 2, Manifests: []descriptor{md}})
	data["index.json"] = index
	data["oci-layout"] = []byte(`{"imageLayoutVersion":"1.0.0"}`)
	var archive bytes.Buffer
	tw = tar.NewWriter(&archive)
	for name, b := range data {
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0644, Size: int64(len(b))})
		_, _ = tw.Write(b)
	}
	_ = tw.Close()
	file := filepath.Join(t.TempDir(), "image.tar")
	if err := os.WriteFile(file, archive.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	return file, payload, execution
}
func TestOCIClosureAndCompiledOutputContract(t *testing.T) {
	file, binary, execution := fixtureOCI(t, "linux/arm64")
	result, err := verifyOCI(file, 1<<20, "linux/arm64", execution, binary)
	if err != nil || result.ManifestDigest == "" || result.ArchiveHash == "" || result.LayoutHash == "" {
		t.Fatal("valid fixture rejected", err)
	}
	if _, err = VerifyOCI(file, 1<<20, "linux/amd64"); err == nil {
		t.Fatal("platform mismatch accepted")
	}
	if _, err = VerifyOCI(file, 100, "linux/arm64"); err == nil {
		t.Fatal("oversize archive accepted")
	}
	if _, err = verifyOCI(file, 1<<20, "linux/arm64", execution, []byte("other-binary")); err == nil {
		t.Fatal("different compiled output accepted")
	}
	execution.Entrypoint = []string{"/unexpected"}
	if _, err = verifyOCI(file, 1<<20, "linux/arm64", execution, binary); err == nil {
		t.Fatal("entrypoint mismatch accepted")
	}
	b, _ := os.ReadFile(file)
	b[len(b)/3] ^= 0xff
	_ = os.WriteFile(file, b, 0600)
	if _, err = VerifyOCI(file, 1<<20, "linux/arm64"); err == nil {
		t.Fatal("corrupt blob accepted")
	}
}
func TestSnapshotRejectsEscapingLinksMetadataAndOversize(t *testing.T) {
	for _, header := range []*tar.Header{{Name: "../escape", Typeflag: tar.TypeReg, Size: 1}, {Name: "secret-link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}, {Name: ".git/config", Typeflag: tar.TypeReg, Size: 1}, {Name: "large", Typeflag: tar.TypeReg, Size: 1000}} {
		var b bytes.Buffer
		tw := tar.NewWriter(&b)
		_ = tw.WriteHeader(header)
		if header.Size > 0 {
			_, _ = tw.Write(make([]byte, header.Size))
		}
		_ = tw.Close()
		if err := extractSource(&b, t.TempDir(), 100); err == nil {
			t.Fatal("unsafe source archive accepted", header.Name)
		}
	}
}
func TestDurableJournalOwnerAndResourceBoundaries(t *testing.T) {
	root := t.TempDir()
	owner := nonce()
	file := filepath.Join(root, "journal.json")
	j := journal{Owner: owner, ParentPID: os.Getpid(), Deadline: time.Now().Add(time.Minute), LeaseUntil: time.Now().Add(time.Second), Resources: []ownedResource{{Name: "rcp-" + owner + "-check", ID: strings.Repeat("b", 64), CreateState: "CREATED"}}, SourceSHA: strings.Repeat("a", 40)}
	if err := saveJournal(file, j); err != nil {
		t.Fatal(err)
	}
	read, err := readJournal(file)
	if err != nil || read.Owner != j.Owner || read.Resources[0] != j.Resources[0] {
		t.Fatal("journal not durable", err)
	}
	j.Resources = []ownedResource{{Name: "unrelated-production-container", ID: strings.Repeat("b", 64), CreateState: "CREATED"}}
	if err = saveJournal(file, j); err != nil {
		t.Fatal(err)
	}
	if _, err = readJournal(file); err == nil {
		t.Fatal("unowned resource accepted")
	}
}
func TestBoundedLogsAndTrustedBuildContext(t *testing.T) {
	b := &boundedBuffer{remaining: 2}
	if _, err := b.Write([]byte("abc")); err == nil {
		t.Fatal("log limit ignored")
	}
	e := &domain.WorkerExecution{ImagePath: "app", Entrypoint: []string{"/app"}}
	archive, err := buildContext(e, []byte("binary"))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(bytes.NewReader(archive))
	header, err := tr.Next()
	if err != nil || header.Name != "Dockerfile" {
		t.Fatal(err)
	}
	data := make([]byte, header.Size)
	_, _ = tr.Read(data)
	text := string(data)
	if !strings.Contains(text, "FROM scratch") || !strings.Contains(text, "USER 65532:65532") || strings.Contains(text, "RUN ") || strings.Contains(text, "#syntax") {
		t.Fatal("unexpected executable build definition")
	}
}
func TestRealProbeOCIClosureWhenProvided(t *testing.T) {
	file := os.Getenv("RCP_TEST_LOCAL_OCI")
	if file == "" {
		t.Skip("optional real BuildKit compatibility archive not supplied")
	}
	if _, err := VerifyOCI(file, 64<<20, "linux/arm64"); err != nil {
		t.Fatal(err)
	}
}

func TestInactiveOperatorExamplesAgreeAndValidate(t *testing.T) {
	file := "../../examples/workers/supervisor.json"
	cfg, err := Load(file)
	if err != nil {
		t.Fatal("supervisor example", err)
	}
	b, err := os.ReadFile("../../examples/workers/recipe.json")
	if err != nil {
		t.Fatal(err)
	}
	var recipe workerconfig.Recipe
	if json.Unmarshal(b, &recipe) != nil || recipe.Hash != workerconfig.RecipeHash(recipe) || recipe.Hash != cfg.Recipe.Hash {
		t.Fatal("recipe examples drifted")
	}
	t.Setenv("RCP_WORKER_TOKEN", "test-only-worker-credential-00000000000000")
	authority, err := workerconfig.Load("../../examples/workers/authority.json", "test-only-control-credential")
	if err != nil {
		t.Fatal("authority example", err)
	}
	if authority.Revision() != cfg.ConfigRevision {
		t.Fatal("example config revisions disagree")
	}
	if _, err = authority.Authenticate(os.Getenv("RCP_WORKER_TOKEN"), time.Now()); err == nil {
		t.Fatal("inactive example credential unexpectedly authorizes execution")
	}
	configured, ok := authority.Recipe(recipe.ID)
	if !ok || configured.Hash != recipe.Hash {
		t.Fatal("authority recipe example drifted")
	}
}
