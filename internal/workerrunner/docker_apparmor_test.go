package workerrunner

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"releasecontrol/internal/domain"
)

func TestBuildKitLauncherKeepsArgumentsOutsideStaticShellProgram(t *testing.T) {
	dir := t.TempDir()
	capture := filepath.Join(dir, "arguments")
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
	id := strings.Repeat("a", 64)
	fake := "#!/bin/sh\nprintf '%s\\000' \"$@\" > " + quote(capture) + "\nprintf '%s\\n' " + quote(id) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(fake), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	// These values must stay individual Docker arguments, never shell source.
	name := "worker name; $(touch " + filepath.Join(dir, "unexpected") + ")"
	owner := "owner 'quoted' $HOME"
	e := &domain.WorkerExecution{Platform: "linux/amd64", CPUs: 2, MemoryMB: 512, PIDs: 64, StorageMB: 256, BuildKitImage: "moby/buildkit@sha256:" + strings.Repeat("b", 64)}
	gotID, err := (docker{context: "fixture"}).createBuildKit(context.Background(), name, owner, e)
	if err != nil || gotID != id {
		t.Fatalf("createBuildKit: id=%q err=%v", gotID, err)
	}
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00")
	if !reflect.DeepEqual(args[:7], []string{"--context", "fixture", "create", "--name", name, "--label", "rcp.worker.owner=" + owner}) {
		t.Fatalf("Docker argument boundaries changed: %q", args)
	}
	imageIndex := -1
	for i, arg := range args {
		if arg == e.BuildKitImage {
			imageIndex = i
		}
		if arg == "--privileged" || strings.Contains(arg, "no-process-sandbox") {
			t.Fatalf("unexpected sandbox relaxation: %q", arg)
		}
	}
	if imageIndex < 2 || !reflect.DeepEqual(args[imageIndex-2:imageIndex], []string{"--entrypoint", "/bin/sh"}) {
		t.Fatal("initial executable must consume the runtime on-exec profile before RootlessKit")
	}
	want := []string{"-ec", `exec /usr/bin/rootlesskit /usr/bin/buildkitd "$@"`, "rcp-rootless-buildkit", "--oci-worker-snapshotter=native", "--addr", "unix:///run/user/1000/buildkit/buildkitd.sock"}
	if !reflect.DeepEqual(args[imageIndex+1:], want) {
		t.Fatalf("launcher must be fixed shell source with positional arguments: %q", args[imageIndex+1:])
	}
	for _, pair := range [][2]string{{"--user", "1000:1000"}, {"--network", "none"}, {"--cpus", "2"}, {"--memory", "512m"}, {"--memory-swap", "512m"}, {"--pids-limit", "64"}} {
		found := false
		for i := 0; i+1 < imageIndex; i++ {
			if args[i] == pair[0] && args[i+1] == pair[1] {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing isolation argument %q", pair)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "unexpected")); !os.IsNotExist(err) {
		t.Fatal("request data was interpreted as shell source")
	}
}
