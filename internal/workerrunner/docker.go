package workerrunner

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/ociverify"
	"strings"
	"time"
)

func stringTrim(b []byte) string { return strings.TrimSpace(string(b)) }

type docker struct {
	context string
	config  string
}
type boundedBuffer struct {
	bytes.Buffer
	remaining int64
}

func (w *boundedBuffer) Write(b []byte) (int, error) {
	if int64(len(b)) > w.remaining {
		return 0, errors.New("execution output exceeds bound")
	}
	n, e := w.Buffer.Write(b)
	w.remaining -= int64(n)
	return n, e
}
func (d docker) command(ctx context.Context, args ...string) *exec.Cmd {
	prefix := []string{}
	if d.context != "" {
		prefix = append(prefix, "--context", d.context)
	}
	cmd := exec.CommandContext(ctx, "docker", append(prefix, args...)...)
	cmd.Env = cleanEnv()
	if d.config != "" {
		cmd.Env = append(cmd.Env, "DOCKER_CONFIG="+d.config)
	}
	return cmd
}

var errCheckFailed = errors.New("source check failed")

func (d docker) output(ctx context.Context, input io.Reader, max int64, args ...string) ([]byte, error) {
	cmd := d.command(ctx, args...)
	cmd.Stdin = input
	out := &boundedBuffer{remaining: max}
	cmd.Stdout = out
	cmd.Stderr = &boundedBuffer{remaining: 1 << 16}
	if e := cmd.Run(); e != nil {
		return nil, errors.New("isolated execution command failed")
	}
	return out.Bytes(), nil
}
func limits(e *domain.WorkerExecution) []string {
	return []string{"--platform", e.Platform, "--pull", "never", "--restart", "no", "--network", "none", "--read-only", "--cpus", fmt.Sprint(e.CPUs), "--memory", fmt.Sprintf("%dm", e.MemoryMB), "--memory-swap", fmt.Sprintf("%dm", e.MemoryMB), "--pids-limit", fmt.Sprint(e.PIDs), "--log-driver", "none"}
}
func (d docker) createCheck(ctx context.Context, name, owner, source string, e *domain.WorkerExecution) (string, error) {
	args := append([]string{"create", "--name", name, "--label", "rcp.worker.owner=" + owner}, limits(e)...)
	args = append(args, "--cap-drop", "ALL", "--security-opt", "no-new-privileges")
	args = append(args, "--user", "65532:65532", "--workdir", "/work", "--mount", "type=bind,src="+source+",dst=/source,readonly", "--tmpfs", fmt.Sprintf("/work:rw,nosuid,nodev,exec,size=%dm,uid=65532,gid=65532", e.StorageMB), "--tmpfs", fmt.Sprintf("/output:rw,nosuid,nodev,size=%dm,uid=65532,gid=65532", e.StorageMB), "--tmpfs", fmt.Sprintf("/tmp:rw,nosuid,nodev,exec,size=%dm,mode=1777", e.StorageMB), "--env", "HOME=/tmp", "--env", "GOCACHE=/tmp/go-cache", "--env", "GOMODCACHE=/tmp/go-mod", "--env", "GOTOOLCHAIN=local", "--env", "CGO_ENABLED=0", "--entrypoint", "/bin/sleep", e.ToolchainImage, "2147483647")
	b, err := d.output(ctx, nil, 1024, args...)
	id := stringTrim(b)
	if err != nil || !containerIDPattern.MatchString(id) {
		return "", errors.New("container create outcome not confirmed")
	}
	return id, nil
}
func (d docker) execCheck(ctx context.Context, name string, argv []string, max int64) error {
	_, e := d.output(ctx, nil, max, append([]string{"exec", name}, argv...)...)
	return e
}
func (d docker) verifyLimits(ctx context.Context, name string, e *domain.WorkerExecution) error {
	b, err := d.output(ctx, nil, 1<<16, "inspect", name)
	if err != nil {
		return err
	}
	var inspected []struct {
		HostConfig struct {
			Privileged     bool
			ReadonlyRootfs bool
			Memory         int64
			MemorySwap     int64
			NanoCPUs       int64
			PidsLimit      int64
			NetworkMode    string
			RestartPolicy  struct{ Name string }
			Tmpfs          map[string]string
		}
		Mounts []struct {
			Type        string
			Destination string
			RW          bool
		}
	}
	if json.Unmarshal(b, &inspected) != nil || len(inspected) != 1 {
		return errors.New("isolation inspection failed")
	}
	h := inspected[0].HostConfig
	if h.Privileged || !h.ReadonlyRootfs || h.Memory != int64(e.MemoryMB)<<20 || h.MemorySwap != h.Memory || h.NanoCPUs != int64(e.CPUs)*1000000000 || h.PidsLimit != int64(e.PIDs) || h.NetworkMode != "none" || h.RestartPolicy.Name != "no" {
		return errors.New("container isolation limits differ from recipe")
	}
	for _, m := range inspected[0].Mounts {
		if m.Type == "volume" || m.Type == "bind" && (m.Destination != "/source" || m.RW) {
			return errors.New("unexpected host or anonymous volume")
		}
	}
	b, err = d.output(ctx, nil, 1024, "exec", name, "cat", "/sys/fs/cgroup/memory.max", "/sys/fs/cgroup/memory.swap.max", "/sys/fs/cgroup/cpu.max", "/sys/fs/cgroup/pids.max")
	expected := fmt.Sprintf("%d\n0\n%d 100000\n%d", int64(e.MemoryMB)<<20, e.CPUs*100000, e.PIDs)
	if err != nil || stringTrim(b) != expected {
		return errors.New("kernel resource limits not proven")
	}
	return nil
}
func (d docker) createBuildKit(ctx context.Context, name, owner string, e *domain.WorkerExecution) (string, error) {
	args := append([]string{"create", "--name", name, "--label", "rcp.worker.owner=" + owner}, limits(e)...)
	args = append(args, "--user", "1000:1000", "--security-opt", "seccomp=unconfined", "--security-opt", "apparmor=unconfined", "--security-opt", "systempaths=unconfined", "--tmpfs", "/tmp:rw,nosuid,nodev,size=64m,mode=1777", "--tmpfs", "/run/user/1000:rw,nosuid,nodev,size=16m,uid=1000,gid=1000", "--tmpfs", fmt.Sprintf("/home/user/.local/share/buildkit:rw,nosuid,nodev,size=%dm,uid=1000,gid=1000", e.StorageMB), "--tmpfs", "/home/user/.local/tmp:rw,nosuid,nodev,size=16m,uid=1000,gid=1000", "--tmpfs", fmt.Sprintf("/context:rw,nosuid,nodev,size=%dm,uid=1000,gid=1000", e.StorageMB), "--tmpfs", fmt.Sprintf("/output:rw,nosuid,nodev,size=%dm,uid=1000,gid=1000", e.StorageMB), "--env", "TMPDIR=/home/user/.local/tmp", e.BuildKitImage, "--oci-worker-snapshotter=native", "--addr", "unix:///run/user/1000/buildkit/buildkitd.sock")
	b, err := d.output(ctx, nil, 1024, args...)
	id := stringTrim(b)
	if err != nil || !containerIDPattern.MatchString(id) {
		return "", errors.New("container create outcome not confirmed")
	}
	return id, nil
}
func (d docker) startOwned(ctx context.Context, id, owner string) error {
	if !containerIDPattern.MatchString(id) {
		return errors.New("full container identity required")
	}
	identity, e := d.output(ctx, nil, 256, "inspect", "--format", `{{.Id}} {{ index .Config.Labels "rcp.worker.owner" }}`, id)
	if e != nil || stringTrim(identity) != id+" "+owner {
		return errors.New("container ownership changed before start")
	}
	_, e = d.output(ctx, nil, 1024, "start", id)
	return e
}
func (d docker) waitBuildKit(ctx context.Context, id string) error {
	for {
		b, err := d.output(ctx, nil, 4096, "exec", id, "buildctl", "--addr", "unix:///run/user/1000/buildkit/buildkitd.sock", "debug", "workers", "--format", "{{json .}}")
		if err == nil {
			if !bytes.Contains(b, []byte(`org.mobyproject.buildkit.worker.oci.process-mode`)) || !bytes.Contains(b, []byte(`sandbox`)) || bytes.Contains(b, []byte(`no-sandbox`)) {
				return errors.New("BuildKit process sandbox not proven")
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}
func buildContext(e *domain.WorkerExecution, binary []byte) ([]byte, error) {
	return buildContextRuntime(e, binary, nil)
}
func buildContextRuntime(e *domain.WorkerExecution, binary []byte, runtime []ociverify.RuntimeEntry) ([]byte, error) {
	entrypoint, _ := json.Marshal(e.Entrypoint)
	copyPair, _ := json.Marshal([]string{"payload", "/" + e.ImagePath})
	dockerfile := "FROM scratch\nCOPY --chmod=0555 " + string(copyPair) + "\nUSER 65532:65532\nENTRYPOINT " + string(entrypoint) + "\n"
	payloadName := "payload"
	payload := binary
	if e.RuntimeRootFS != nil {
		var err error
		payload, _, err = ociverify.MergeRuntime(runtime, e.ImagePath, binary, e.OutputBytes)
		if err != nil {
			return nil, err
		}
		payloadName = "rootfs.tar"
		dockerfile = "FROM scratch\nADD [\"rootfs.tar\",\"/\"]\nENV PATH=/usr/local/bin:/usr/bin:/bin\nWORKDIR /\nUSER 65532:65532\nENTRYPOINT " + string(entrypoint) + "\n"
	}
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, f := range []struct {
		name string
		data []byte
		mode int64
	}{{"Dockerfile", []byte(dockerfile), 0644}, {payloadName, payload, 0755}} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: f.mode, Size: int64(len(f.data)), ModTime: time.Unix(0, 0)}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(f.data); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
