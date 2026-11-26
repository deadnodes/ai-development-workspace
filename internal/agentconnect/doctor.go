package agentconnect

import (
	"context"
	"crypto/sha256"
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
	var raw, profile, product, workspace, tokenEnv, data, name string
	f := flag.NewFlagSet("release-control doctor", flag.ContinueOnError)
	f.SetOutput(out)
	f.StringVar(&data, "data", "", "local SQLite file (instead of --url)")
	f.StringVar(&name, "name", "release-control", "local MCP server name")
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
	if data != "" {
		var conflicts bool
		f.Visit(func(flag *flag.Flag) {
			conflicts = conflicts || flag.Name == "url" || flag.Name == "profile" || flag.Name == "token-env" || flag.Name == "product-id"
		})
		if conflicts {
			return errors.New("--data cannot be combined with remote connection options")
		}
		return doctorLocal(data, workspace, name, out)
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
			args := []string{"--version"}
			switch x.bin {
			case "go":
				args = []string{"version"}
			case "tmux":
				args = []string{"-V"}
			}
			b, er := exec.CommandContext(ctx, x.bin, args...).CombinedOutput()
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
	var serverKit *kit
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
		if e == nil {
			if k, ke := fetchAgentKit(ctx, endpoint, token); ke == nil {
				serverKit = &k
			}
		}
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
			if serverKit != nil { // connect installs the server's kit; compare against the same source.
				kit = *serverKit
			}
			expected := map[string]string{kit.Skill.Name: kit.Skill.Content, kit.DeliverySkill.Name: kit.DeliverySkill.Content, kit.ExecutionSkill.Name: kit.ExecutionSkill.Content}
			for _, p := range paths {
				b, er := os.ReadFile(filepath.Join(root, p))
				if er != nil {
					e = er
					break
				}
				name := filepath.Base(filepath.Dir(p))
				sum := sha256.Sum256([]byte(skillLF(string(b))))
				want := sha256.Sum256(installedSkill(expected[name]))
				if sum != want {
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
		approve, stdio := codexAutoApproveConfigured(filepath.Join(root, ".codex/config.toml"), bindingServerName(root))
		if !approve {
			checks = append(checks, doctorCheck{name: "Codex MCP tool approval", fix: "connect --codex-auto-approve", err: errors.New("orchestrators running codex -a never need --codex-auto-approve"), warn: true})
		} else if stdio {
			checks = append(checks, doctorCheck{name: "Codex MCP tool approval", fix: "reconnect with --token-env NAME --codex-auto-approve", err: errors.New("codex exec ignores auto-approval for stdio MCP servers; orchestrators need the direct URL entry that --token-env writes"), warn: true})
		}
	} else {
		add("Workspace checks", "--workspace /path", rootErr)
	}
	if rootErr == nil {
		name := bindingServerName(root)
		add("Codex user config does not shadow project MCP", "rerun connect with --name "+name+"-project or remove the url table from ~/.codex/config.toml", userCodexConflict(name))
	}
	if plaintextCodexBearer() {
		checks = append(checks, doctorCheck{name: "Codex user config credential", fix: "move the token to bearer_token_env_var or use release-control connect --profile", err: errors.New("~/.codex/config.toml contains a literal Authorization header"), warn: true})
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
func codexAutoApproveConfigured(path, name string) (approve, stdio bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return false, false
	}
	var root map[string]any
	if toml.Unmarshal(b, &root) != nil {
		return false, false
	}
	servers, _ := root["mcp_servers"].(map[string]any)
	rc, _ := servers[name].(map[string]any)
	_, hasCommand := rc["command"]
	return rc["default_tools_approval_mode"] == "approve", hasCommand
}

func bindingServerName(root string) string {
	var b Binding
	if raw, err := os.ReadFile(filepath.Join(root, ".release-control.json")); err == nil && json.Unmarshal(raw, &b) == nil && b.ServerName != "" {
		return b.ServerName
	}
	return "release-control"
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

func fetchAgentKit(ctx context.Context, endpoint, token string) (kit, error) {
	var k kit
	request, err := http.NewRequestWithContext(ctx, "GET", endpoint+"/api/agent-kit", nil)
	if err != nil {
		return k, err
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return k, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return k, fmt.Errorf("agent kit HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 2<<20+1))
	if err != nil || len(data) > 2<<20 {
		return k, errors.New("agent kit unreadable or oversized")
	}
	err = decodeUniqueJSON(data, &k)
	return k, err
}
