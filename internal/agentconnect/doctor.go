package agentconnect

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
	"releasecontrol/internal/agentguide"
)

type doctorCheck struct {
	name, fix string
	err       error
	warn      bool
}

// RunDoctor checks a connected agent workspace without changing it.
func RunDoctor(ctx context.Context, args []string, out io.Writer) error {
	var raw, profile, product, workspace, tokenEnv string
	f := flag.NewFlagSet("release-control doctor", flag.ContinueOnError)
	f.SetOutput(out)
	f.StringVar(&raw, "url", "", "Control Plane base URL")
	f.StringVar(&profile, "profile", "default", "saved credential profile")
	f.StringVar(&product, "product-id", "", "Product ID")
	f.StringVar(&workspace, "workspace", ".", "project directory")
	f.StringVar(&tokenEnv, "token-env", "", "bearer token environment variable")
	if err := f.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if f.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	endpoint, endpointErr := secureEndpoint(raw)
	root, rootErr := filepath.Abs(workspace)
	checks := []doctorCheck{}
	add := func(name, fix string, err error) {
		checks = append(checks, doctorCheck{name: name, fix: fix, err: err})
	}
	for _, x := range []struct{ name, bin, fix string }{{"Go", "go", "install Go >=1.24; then make build"}, {"Node", "node", "install Node.js; required for web checks"}, {"Git", "git", "install git"}, {"tmux", "tmux", "install tmux"}, {"Codex", "codex", "install Codex CLI"}, {"Claude", "claude", "install Claude Code"}} {
		_, e := exec.LookPath(x.bin)
		v := ""
		if e == nil {
			b, er := exec.CommandContext(ctx, x.bin, "--version").CombinedOutput()
			if er == nil {
				v = strings.TrimSpace(string(b))
			} else {
				e = er
			}
		}
		add(x.name+versionSuffix(v), x.fix, e)
	}
	if p, e := exec.LookPath("claude"); e == nil {
		_, e = exec.CommandContext(ctx, p, "auth", "status").CombinedOutput()
		add("Claude authentication", "claude", e)
	} else {
		add("Claude authentication", "claude", e)
	}
	if p, e := exec.LookPath("codex"); e == nil {
		_, e = exec.CommandContext(ctx, p, "login", "status").CombinedOutput()
		add("Codex authentication", "codex login", e)
	} else {
		add("Codex authentication", "codex login", e)
	}
	if endpointErr != nil {
		add("RCP health", "./bin/release-control doctor --url URL", endpointErr)
		add("MCP initialize + tools/list", "./bin/release-control login --url URL --profile "+profile, endpointErr)
		add("Product exists", "./bin/release-control doctor --url URL --product-id "+product, endpointErr)
	} else {
		client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		resp, e := client.Get(endpoint + "/healthz")
		if e == nil {
			resp.Body.Close()
			if resp.StatusCode != 200 {
				e = fmt.Errorf("HTTP %d", resp.StatusCode)
			}
		}
		add("RCP /healthz", "start/check Control Plane at "+endpoint, e)
		token, e := ResolveCredential(endpoint, profile, tokenEnv)
		if e != nil {
			e = fmt.Errorf("credential unavailable: %w", e)
		}
		if e == nil { // MCP client verifies initialize and tools/list through the same SDK path as discovery.
			_, e = discoverProduct(ctx, endpoint, token)
			if product != "" {
				_, e = productContextCheck(ctx, endpoint, token, product)
			}
		}
		add("MCP initialize + tools/list", "./bin/release-control login --url "+endpoint+" --profile "+profile, e)
		if product != "" {
			_, pe := productContextCheck(ctx, endpoint, token, product)
			add("Product exists", "./bin/release-control doctor --url "+endpoint+" --product-id "+product, pe)
		} else {
			add("Product exists", "./bin/release-control doctor --url "+endpoint+" --product-id PRODUCT_ID", errors.New("--product-id is required"))
		}
	}
	if rootErr == nil {
		for _, h := range []string{"codex", "claude"} {
			paths := []string{".agents/skills/rcp-handoff/SKILL.md", ".agents/skills/rcp-delivery/SKILL.md", ".agents/skills/rcp-execution/SKILL.md"}
			if h == "claude" {
				paths = []string{".claude/skills/rcp-handoff/SKILL.md", ".claude/skills/rcp-delivery/SKILL.md", ".claude/skills/rcp-execution/SKILL.md"}
			}
			var e error
			kit := agentguide.Get()
			expected := map[string]string{kit.Skill.Name: kit.Skill.Content, kit.DeliverySkill.Name: kit.DeliverySkill.Content, kit.ExecutionSkill.Name: kit.ExecutionSkill.Content}
			for _, p := range paths {
				b, er := os.ReadFile(filepath.Join(root, p))
				if er != nil {
					e = er
					break
				}
				name := filepath.Base(filepath.Dir(p))
				sum := sha256.Sum256(b)
				want := sha256.Sum256([]byte(expected[name]))
				if hex.EncodeToString(sum[:]) != hex.EncodeToString(want[:]) {
					e = fmt.Errorf("%s checksum mismatch", p)
					break
				}
			}
			add(h+" installed skill checksums", "./bin/release-control connect --url "+raw+" --workspace "+workspace+" --product-id "+product+" --harness "+h, e)
		}
		for _, x := range []struct{ name, path, fix string }{{"Codex MCP config", ".codex/config.toml", "release-control connect --harness codex"}, {"Claude MCP config", ".mcp.json", "release-control connect --harness claude"}, {"Claude turn-end hook config", ".claude/settings.json", "release-control connect --harness claude"}} {
			_, e := os.Stat(filepath.Join(root, x.path))
			add(x.name, x.fix, e)
		}
		if product != "" {
			home, _ := os.UserHomeDir()
			cfg := filepath.Join(home, ".config/release-control/turn-end-"+product+".json")
			script := filepath.Join(home, ".local/share/release-control/hooks/rcp-turn-end.sh")
			_, ce := os.Stat(cfg)
			_, se := os.Stat(script)
			add("Turn-end hook config", "release-control connect --url "+raw+" --workspace "+workspace+" --product-id "+product, ce)
			if se == nil {
				se = exec.CommandContext(ctx, script, "--config", cfg, "--dry-run").Run()
			}
			add("Turn-end hook runnable (dry run)", "release-control connect --url "+raw+" --workspace "+workspace+" --product-id "+product, se)
			if hookHarness(cfg) == "codex" && hookHasTokenEnv(cfg) {
				checks = append(checks, doctorCheck{name: "Codex hook credential", fix: "use --profile; Codex notify does not inherit env", err: errors.New("Codex notify does not inherit env; use --profile"), warn: true})
			}
		}
		if !codexAutoApproveConfigured(filepath.Join(root, ".codex/config.toml")) {
			checks = append(checks, doctorCheck{name: "Codex MCP tool approval", fix: "connect --codex-auto-approve", err: errors.New("orchestrators running codex -a never need --codex-auto-approve"), warn: true})
		}
	} else {
		add("Workspace checks", "--workspace /path", rootErr)
	}
	home, _ := os.UserHomeDir()
	_, binErr := os.Stat(filepath.Join(home, "bin", "rcp-supervisor"))
	_, cfgErr := os.Stat(filepath.Join(home, ".config/release-control/supervisor.json"))
	_, jobErr := os.Stat(filepath.Join(home, "Library/LaunchAgents/com.deadnodes.rcp-supervisor.plist"))
	checks = append(checks, doctorCheck{name: "Supervisor setup", fix: "see docs/SUPERVISOR.md", err: errors.Join(binErr, cfgErr, jobErr), warn: true})
	fail := false
	for _, c := range checks {
		status := "OK"
		if c.err != nil {
			if c.warn {
				status = "WARN"
			} else {
				status = "FAIL"
				fail = true
			}
		}
		detail := ""
		if c.err != nil {
			detail = " (" + c.err.Error() + ")"
		}
		fmt.Fprintf(out, "%s %s%s; fix: %s\n", status, c.name, detail, c.fix)
	}
	if fail {
		return errors.New("doctor found failing checks")
	}
	return nil
}

func versionSuffix(v string) string {
	if v == "" {
		return ""
	}
	return " (" + strings.ReplaceAll(v, "\n", " ") + ")"
}

func readHookConfig(path string) map[string]string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var values map[string]string
	if json.Unmarshal(b, &values) != nil {
		return nil
	}
	return values
}
func hookHarness(path string) string   { return readHookConfig(path)["harness_kind"] }
func hookHasTokenEnv(path string) bool { return readHookConfig(path)["token_env"] != "" }
func codexAutoApproveConfigured(path string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var root map[string]any
	if toml.Unmarshal(b, &root) != nil {
		return false
	}
	servers, _ := root["mcp_servers"].(map[string]any)
	rc, _ := servers["release-control"].(map[string]any)
	return rc["default_tools_approval_mode"] == "approve"
}

func productContextCheck(ctx context.Context, endpoint, token, id string) (bool, error) {
	// The scoped endpoint is authenticated and its identity must match exactly.
	client := &http.Client{Timeout: 8 * time.Second}
	req, e := http.NewRequestWithContext(ctx, "GET", endpoint+"/api/products/"+id+"/context", nil)
	if e != nil {
		return false, e
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	r, e := client.Do(req)
	if e != nil {
		return false, e
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		return false, fmt.Errorf("HTTP %d", r.StatusCode)
	}
	return true, nil
}
