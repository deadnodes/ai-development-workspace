package secretfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfinedAndBounded(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	t.Setenv("RCP_SECRET_DIR", root)
	if e := os.WriteFile(filepath.Join(root, "valid"), []byte("value"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(outside, "secret"), []byte("private"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(root, "escape")); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(root, "oversize"), make([]byte, 65537), 0600); e != nil {
		t.Fatal(e)
	}
	for _, ref := range []string{"..", "../secret", "escape", "oversize", ""} {
		if _, e := Read(ref); e == nil {
			t.Fatalf("accepted %q", ref)
		}
	}
	b, e := Read("valid")
	if e != nil || string(b) != "value" {
		t.Fatal(string(b), e)
	}
}
