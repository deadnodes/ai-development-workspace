package supervisor

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"
	"time"
)

type ExecAdapter struct{ Config Config }

func (a ExecAdapter) Wake(ctx context.Context, req WakeRequest) WakeResult {
	wakeCtx, cancelWake := context.WithTimeout(ctx, a.Config.WakeFor)
	defer cancelWake()
	if req.TmuxTarget != "" && !ValidTmuxTarget(req.TmuxTarget) {
		return WakeResult{Outcome: Unsupported, Detail: "invalid tmux target"}
	}
	if req.TmuxTarget != "" {
		if ok, _ := a.tmuxHas(wakeCtx, req.TmuxTarget); ok {
			return a.wakeTmux(wakeCtx, req)
		}
	}
	if !a.headlessAllowed(req.Kind) {
		return WakeResult{Outcome: Unsupported, Detail: "headless fallback disabled"}
	}
	if !ValidSessionRef(req.SessionRef) {
		return WakeResult{Outcome: Unsupported, Detail: "invalid or missing session_ref"}
	}
	path, args := "", []string{}
	switch req.Kind {
	case "codex":
		path = a.configPath(a.Config.CodexPath, "codex")
		args = []string{"exec", "resume", req.SessionRef, req.Message}
	case "claude_code":
		path = a.configPath(a.Config.ClaudePath, "claude")
		args = []string{"-p", "--resume", req.SessionRef, req.Message}
	default:
		return WakeResult{Outcome: Unsupported, Detail: "no headless adapter for harness kind"}
	}
	if live, err := a.sessionLive(wakeCtx, req.SessionRef); err != nil {
		return WakeResult{Outcome: Failed, Detail: "process check failed"}
	} else if live {
		return WakeResult{Outcome: Failed, Detail: "busy"}
	}
	cmdCtx, cancel := context.WithTimeout(wakeCtx, a.Config.WakeFor)
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, path, args...)
	if err := cmd.Run(); err != nil {
		if errors.Is(cmdCtx.Err(), context.DeadlineExceeded) {
			return WakeResult{Outcome: Failed, Detail: "wake timed out"}
		}
		return WakeResult{Outcome: Failed, Detail: "headless wake failed"}
	}
	return WakeResult{Outcome: Delivered, Detail: "headless wake accepted"}
}

func (a ExecAdapter) wakeTmux(ctx context.Context, req WakeRequest) WakeResult {
	if !ValidTmuxTarget(req.TmuxTarget) {
		return WakeResult{Outcome: Unsupported, Detail: "invalid tmux target"}
	}
	before, err := a.capture(ctx, req.TmuxTarget)
	if err != nil {
		return WakeResult{Outcome: Failed, Detail: "tmux pane unavailable"}
	}
	if paneBusy(before) {
		return WakeResult{Outcome: Failed, Detail: "busy"}
	}
	if err = waitDuration(ctx, a.Config.IdleFor); err != nil {
		return WakeResult{Outcome: Failed, Detail: "idle check interrupted"}
	}
	after, err := a.capture(ctx, req.TmuxTarget)
	if err != nil {
		return WakeResult{Outcome: Failed, Detail: "tmux pane unavailable"}
	}
	if paneBusy(after) || paneHash(before) != paneHash(after) {
		return WakeResult{Outcome: Failed, Detail: "busy"}
	}
	if !containsLastLines(after, "[RCP supervisor]", 8) {
		if err = a.run(ctx, a.configPath(a.Config.TmuxPath, "tmux"), "send-keys", "-t", req.TmuxTarget, "-l", req.Message); err != nil {
			return WakeResult{Outcome: Failed, Detail: "tmux send failed"}
		}
		if err = waitDuration(ctx, a.Config.SubmitFor); err != nil {
			return WakeResult{Outcome: Failed, Detail: "tmux submit interrupted"}
		}
	}
	if err = a.run(ctx, a.configPath(a.Config.TmuxPath, "tmux"), "send-keys", "-t", req.TmuxTarget, "Enter"); err != nil {
		return WakeResult{Outcome: Failed, Detail: "tmux enter failed"}
	}
	verifyFor := a.Config.VerifyFor
	if verifyFor <= 0 {
		verifyFor = a.Config.WakeFor
	}
	verifyCtx, cancelVerify := context.WithTimeout(ctx, verifyFor)
	defer cancelVerify()
	retryAt := time.Now().Add(3 * time.Second)
	retried := false
	for {
		pane, e := a.capture(verifyCtx, req.TmuxTarget)
		if e == nil && paneBusy(pane) {
			return WakeResult{Outcome: Delivered, Detail: "tmux wake observed"}
		}
		if !retried && !time.Now().Before(retryAt) {
			if err = a.run(verifyCtx, a.configPath(a.Config.TmuxPath, "tmux"), "send-keys", "-t", req.TmuxTarget, "Enter"); err != nil {
				return WakeResult{Outcome: Failed, Detail: "tmux retry enter failed"}
			}
			retried = true
		}
		select {
		case <-verifyCtx.Done():
			return WakeResult{Outcome: Failed, Detail: "wake not observed"}
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (a ExecAdapter) headlessAllowed(kind string) bool {
	for _, k := range a.Config.HeadlessKinds {
		if k == kind {
			return true
		}
	}
	return false
}

func containsLastLines(p []byte, message string, count int) bool {
	lines := strings.Split(strings.TrimRight(string(p), "\n"), "\n")
	if len(lines) > count {
		lines = lines[len(lines)-count:]
	}
	for _, line := range lines {
		if strings.Contains(line, message) {
			return true
		}
	}
	return false
}

func (a ExecAdapter) tmuxHas(ctx context.Context, target string) (bool, error) {
	if !ValidTmuxTarget(target) {
		return false, nil
	}
	err := exec.CommandContext(ctx, a.configPath(a.Config.TmuxPath, "tmux"), "has-session", "-t", target).Run()
	return err == nil, err
}
func (a ExecAdapter) capture(ctx context.Context, target string) ([]byte, error) {
	return a.output(ctx, a.configPath(a.Config.TmuxPath, "tmux"), "capture-pane", "-p", "-t", target)
}
func (a ExecAdapter) sessionLive(ctx context.Context, ref string) (bool, error) {
	cmd := exec.CommandContext(ctx, a.configPath(a.Config.PgrepPath, "pgrep"), "-f", ref)
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return false, nil
	}
	return false, err
}
func (a ExecAdapter) output(ctx context.Context, path string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, path, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &bytes.Buffer{}
	err := cmd.Run()
	if err != nil {
		return nil, err
	}
	if out.Len() > 1<<20 {
		return nil, errors.New("command output too large")
	}
	return out.Bytes(), nil
}
func (a ExecAdapter) run(ctx context.Context, path string, args ...string) error {
	return exec.CommandContext(ctx, path, args...).Run()
}
func (a ExecAdapter) configPath(configured, fallback string) string {
	if configured != "" {
		return configured
	}
	return fallback
}
func paneBusy(p []byte) bool {
	lines := strings.Split(strings.TrimRight(strings.ToLower(string(p)), "\n"), "\n")
	if len(lines) > 15 {
		lines = lines[len(lines)-15:]
	}
	for _, line := range lines {
		if strings.Contains(line, "esc to interrupt") {
			return true
		}
	}
	return false
}
func waitDuration(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
