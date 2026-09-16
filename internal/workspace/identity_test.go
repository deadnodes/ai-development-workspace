package workspace

import "testing"

func TestRemoteIdentity(t *testing.T) {
	for _, raw := range []string{"git@github.com:DeadNodes/pp-back.git", "https://github.com/deadnodes/pp-back", "ssh://git@github.com:22/deadnodes/pp-back.git"} {
		if got := RemoteIdentity(raw); got != "github.com/deadnodes/pp-back" {
			t.Fatalf("%s: %s", raw, got)
		}
	}
	for _, raw := range []string{"https://other.example/deadnodes/pp-back", "ssh://git@github.com:2222/deadnodes/pp-back.git", "https://github.com/other/pp-back"} {
		if RemoteIdentity(raw) == "github.com/deadnodes/pp-back" {
			t.Fatal("unrelated identity matched")
		}
	}
	if RemoteIdentity("/tmp/pp-back") != "" {
		t.Fatal("filesystem path treated as remote identity")
	}
}
