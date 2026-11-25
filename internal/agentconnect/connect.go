// Package agentconnect installs an explicitly requested project-local agent kit.
package agentconnect

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
	"gopkg.in/yaml.v3"
	"releasecontrol/internal/agentguide"
)

const configStart = "# BEGIN release-control managed MCP"
const configEnd = "# END release-control managed MCP"
const agentsStart = "<!-- BEGIN release-control managed agent instructions -->"
const agentsEnd = "<!-- END release-control managed agent instructions -->"
const skillMarker = "<!-- release-control managed skill -->\n"

type Options struct {
	URL, Workspace, ProductID, TokenEnv, Name, Profile, Executable, Harness string
	CodexNotify                                                             bool
	CodexAutoApprove                                                        bool
}
type Binding struct {
	URL        string `json:"url"`
	ProductID  string `json:"product_id"`
	ServerName string `json:"server_name"`
	TokenEnv   string `json:"token_env,omitempty"`
	Profile    string `json:"credential_profile,omitempty"`
}
type kit = agentguide.Kit

func installPaths() []string {
	return []string{".codex/config.toml", ".agents/skills/rcp-handoff/SKILL.md", "AGENTS.md", ".release-control.json", ".agents/skills/rcp-delivery/SKILL.md", ".claude/skills/rcp-handoff/SKILL.md", ".claude/skills/rcp-delivery/SKILL.md", ".agents/skills/rcp-execution/SKILL.md", ".claude/skills/rcp-execution/SKILL.md", ".mcp.json", ".claude/settings.json"}
}

func Run(ctx context.Context, args []string, out io.Writer) error {
	var o Options
	flags := flag.NewFlagSet("release-control connect", flag.ContinueOnError)
	flags.SetOutput(out)
	flags.StringVar(&o.URL, "url", "", "Control Plane base URL")
	flags.StringVar(&o.Workspace, "workspace", "", "project directory")
	flags.StringVar(&o.ProductID, "product-id", "", "Product ID (optional only when server has one Product)")
	flags.StringVar(&o.TokenEnv, "token-env", "", "environment variable containing bearer token; only its name is saved")
	flags.StringVar(&o.Profile, "profile", "", "saved agent credential profile (default if available)")
	flags.StringVar(&o.Executable, "executable", "", "absolute release-control executable for the MCP subprocess (defaults to this executable)")
	flags.StringVar(&o.Name, "name", "release-control", "project MCP server name")
	flags.StringVar(&o.Harness, "harness", "both", "install for codex, claude, or both")
	flags.BoolVar(&o.CodexNotify, "codex-notify", false, "wire the Codex turn-end hook in project config")
	flags.BoolVar(&o.CodexAutoApprove, "codex-auto-approve", false, "allow Codex MCP tools under approval policy never")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if o.Harness == "" {
		o.Harness = "both"
	}
	binding, err := Install(ctx, o)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "Connected project to Product %s at %s.\nInstalled MCP configuration and all three skills for %s, plus turn-end hook wiring and AGENTS.md instructions.\n", binding.ProductID, binding.URL, o.Harness)
	if err == nil && (o.Harness == "codex" || o.Harness == "both") {
		_, err = fmt.Fprintf(out, "Codex turn-end hook line: %s\n", codexNotifyLine(o))
	}
	return err
}

func Install(ctx context.Context, o Options) (Binding, error) {
	var binding Binding
	endpoint, err := secureEndpoint(o.URL)
	if err != nil {
		return binding, err
	}
	if o.Workspace == "" {
		return binding, errors.New("--workspace is required")
	}
	if o.Name == "" {
		o.Name = "release-control"
	}
	if o.Harness == "" {
		o.Harness = "both"
	}
	if o.Harness != "codex" && o.Harness != "claude" && o.Harness != "both" {
		return binding, errors.New("--harness must be codex, claude, or both")
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_-]+$`).MatchString(o.Name) {
		return binding, errors.New("MCP name must contain only letters, digits, underscore or hyphen")
	}
	token := ""
	if o.TokenEnv != "" && o.Profile != "" {
		return binding, errors.New("choose --profile or --token-env")
	}
	if o.TokenEnv != "" {
		token, err = ResolveCredential(endpoint, "", o.TokenEnv)
	} else {
		profile := o.Profile
		if profile == "" {
			profile = "default"
		}
		token, err = loadCredential(endpoint, profile)
		if err == nil {
			o.Profile = profile
		} else if errors.Is(err, os.ErrNotExist) && o.Profile == "" {
			err = nil
		}
	}
	if err != nil {
		return binding, err
	}
	if o.Executable == "" {
		o.Executable, err = os.Executable()
		if err != nil {
			return binding, err
		}
	}
	if !filepath.IsAbs(o.Executable) || strings.ContainsAny(o.Executable, "\r\n") {
		return binding, errors.New("MCP executable must be an absolute path")
	}
	info, statErr := os.Stat(o.Executable)
	if statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return binding, errors.New("MCP executable must be an existing executable file")
	}
	absolute, err := filepath.Abs(o.Workspace)
	if err != nil {
		return binding, err
	}
	root, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return binding, err
	}
	info, err = os.Stat(root)
	if err != nil || !info.IsDir() {
		return binding, errors.New("workspace must be an existing directory")
	}
	rootFS, err := os.OpenRoot(root)
	if err != nil {
		return binding, err
	}
	defer rootFS.Close()
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(path string, out any) error {
		request, err := http.NewRequestWithContext(ctx, "GET", endpoint+path, nil)
		if err != nil {
			return errors.New("invalid server request")
		}
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		response, err := client.Do(request)
		if err != nil {
			return errors.New("Control Plane request failed")
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return fmt.Errorf("Control Plane %s returned HTTP %d", path, response.StatusCode)
		}
		data, err := io.ReadAll(io.LimitReader(response.Body, 2<<20+1))
		if err != nil || len(data) > 2<<20 {
			return errors.New("server response unreadable or oversized")
		}
		if err = decodeUniqueJSON(data, out); err != nil {
			return errors.New("server returned invalid JSON")
		}
		return nil
	}
	var kitJSON json.RawMessage
	if err = get("/api/agent-kit", &kitJSON); err != nil {
		return binding, err
	}
	var agentKit kit
	if err = decodeUniqueJSON(kitJSON, &agentKit); err != nil {
		return binding, errors.New("invalid agent kit JSON")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(kitJSON, &fields) != nil || !exactFields(fields, "skill", "delivery_skill", "execution_skill", "instructions") {
		return binding, errors.New("invalid agent kit fields")
	}
	for _, role := range []string{"skill", "delivery_skill", "execution_skill"} {
		var identity map[string]json.RawMessage
		if json.Unmarshal(fields[role], &identity) != nil || !exactFields(identity, "name", "content", "sha256") {
			return binding, errors.New("invalid agent skill fields")
		}
	}
	for _, expected := range []struct {
		skill agentguide.Skill
		name  string
	}{{agentKit.Skill, agentguide.SkillName}, {agentKit.DeliverySkill, agentguide.DeliverySkillName}, {agentKit.ExecutionSkill, agentguide.ExecutionSkillName}} {
		digest := sha256.Sum256([]byte(expected.skill.Content))
		if expected.skill.Name != expected.name || expected.skill.Content == "" || expected.skill.SHA256 != hex.EncodeToString(digest[:]) {
			return binding, errors.New("agent kit identity or checksum is invalid")
		}
		front, ok := strings.CutPrefix(expected.skill.Content, "---\n")
		front, _, closed := strings.Cut(front, "\n---\n")
		var identity struct {
			Name string `yaml:"name"`
		}
		if !ok || !closed || yaml.Unmarshal([]byte(front), &identity) != nil || identity.Name != expected.name {
			return binding, errors.New("agent skill frontmatter identity is invalid")
		}
	}
	if agentKit.Instructions == "" {
		return binding, errors.New("agent kit instructions missing")
	}
	if strings.Contains(agentKit.Instructions, agentsStart) || strings.Contains(agentKit.Instructions, agentsEnd) {
		return binding, errors.New("agent kit contains reserved installation markers")
	}
	if o.ProductID == "" {
		o.ProductID, err = discoverProduct(ctx, endpoint, token)
		if err != nil {
			return binding, err
		}
	}
	if !validProductID(o.ProductID) {
		return binding, errors.New("invalid Product ID")
	}
	var productContext map[string]any
	if err = get("/api/products/"+url.PathEscape(o.ProductID)+"/context", &productContext); err != nil {
		return binding, err
	}
	productIdentity, ok := productContext["product"].(map[string]any)
	if !ok || productIdentity["id"] != o.ProductID {
		return binding, errors.New("Product context identity mismatch; provide --product-id")
	}
	binding = Binding{URL: endpoint, ProductID: o.ProductID, ServerName: o.Name, TokenEnv: o.TokenEnv, Profile: o.Profile}
	paths := installPaths()
	plans := make([]filePlan, 0, len(paths))
	var outside string
	for _, path := range paths {
		plan, err := readPlan(rootFS, path)
		if err != nil {
			return Binding{}, err
		}
		plans = append(plans, plan)
	}
	plans[0].after = plans[0].before
	if o.Harness != "claude" {
		outside, err = stripBlock(string(plans[0].before), configStart, configEnd)
		if err != nil {
			return Binding{}, err
		}
		var parsed map[string]any
		if err = toml.Unmarshal([]byte(outside), &parsed); err != nil {
			return Binding{}, errors.New("existing project TOML is invalid; no files changed")
		}
		if servers, exists := parsed["mcp_servers"]; exists {
			table, ok := servers.(map[string]any)
			if !ok {
				return Binding{}, errors.New("existing mcp_servers is not a TOML table")
			}
			if _, exists = table[o.Name]; exists {
				return Binding{}, errors.New("unmanaged MCP server already uses requested name")
			}
		}
		args := "\"mcp\", \"--url\", " + strconv.Quote(endpoint)
		if o.Profile != "" {
			args += ", \"--profile\", " + strconv.Quote(o.Profile)
		}
		if o.TokenEnv != "" {
			args += ", \"--token-env\", " + strconv.Quote(o.TokenEnv)
		}
		entry := "[mcp_servers." + strconv.Quote(o.Name) + "]\ncommand = " + strconv.Quote(o.Executable) + "\nargs = [" + args + "]\n"
		if o.CodexAutoApprove {
			entry += "default_tools_approval_mode = \"approve\"\n"
		}
		plans[0].after = []byte(appendBlock(outside, configStart, configEnd, entry))
		if err = toml.Unmarshal(plans[0].after, &parsed); err != nil {
			return Binding{}, errors.New("managed MCP entry conflicts with existing TOML")
		}
		if o.CodexNotify {
			parsed["notify"] = []string{filepath.Join(homeDir(), ".local/share/release-control/hooks/rcp-turn-end.sh"), "--config", hookConfigPath(o.ProductID)}
			plans[0].after, err = toml.Marshal(parsed)
			if err != nil {
				return Binding{}, errors.New("could not merge Codex notify setting")
			}
		}
	}
	for _, target := range []struct {
		index int
		skill agentguide.Skill
	}{{1, agentKit.Skill}, {4, agentKit.DeliverySkill}, {5, agentKit.Skill}, {6, agentKit.DeliverySkill}, {7, agentKit.ExecutionSkill}, {8, agentKit.ExecutionSkill}} {
		plan := &plans[target.index]
		if plan.exists && !bytes.HasSuffix(plan.before, []byte(skillMarker)) && !bytes.Equal(plan.before, []byte(target.skill.Content)) {
			return Binding{}, fmt.Errorf("unmanaged %s skill exists; no files changed", target.skill.Name)
		}
		plan.after = installedSkill(target.skill.Content)
	}
	// Claude Code reads project MCP servers from .mcp.json. Decode and merge the
	// JSON object so unrelated servers and settings survive reconnects.
	plans[9].after = plans[9].before
	if o.Harness != "codex" {
		claudeMCP, err := mergeClaudeMCP(plans[9].before, o, endpoint)
		if err != nil {
			return Binding{}, err
		}
		plans[9].after = claudeMCP
	}
	plans[10].after = plans[10].before
	if o.Harness != "codex" {
		claudeSettings, err := mergeStopHook(plans[10].before, o, endpoint)
		if err != nil {
			return Binding{}, err
		}
		plans[10].after = claudeSettings
	}
	outside, err = stripBlock(string(plans[2].before), agentsStart, agentsEnd)
	if err != nil {
		return Binding{}, err
	}
	instructions := fmt.Sprintf("Control Plane: %s\nProduct ID: %s\nMCP server: %s\nBinding: `.release-control.json`.\n\n%s\n\nUse the installed `rcp-handoff` skill for checkpoints, pause and context transfer, and `rcp-delivery` for Feature delivery within existing policy and authority. Record work checkpoints in the Control Plane through MCP; resume the Product/Feature before changing code.\n", endpoint, o.ProductID, o.Name, agentKit.Instructions)
	plans[2].after = []byte(appendBlock(outside, agentsStart, agentsEnd, instructions))
	if plans[3].exists {
		var prior Binding
		decoder := json.NewDecoder(bytes.NewReader(plans[3].before))
		decoder.DisallowUnknownFields()
		if err = decoder.Decode(&prior); err != nil || prior.URL == "" || prior.ProductID == "" || prior.ServerName == "" {
			return Binding{}, errors.New("unmanaged or invalid .release-control.json exists")
		}
		if decoder.Decode(new(any)) != io.EOF {
			return Binding{}, errors.New("invalid trailing binding content")
		}
	}
	plans[3].after, err = json.MarshalIndent(binding, "", "  ")
	if err != nil {
		return Binding{}, err
	}
	plans[3].after = append(plans[3].after, '\n')
	hookSnapshot, hookErr := installHookFiles(ctx, o, endpoint)
	if hookErr != nil {
		return Binding{}, hookErr
	}
	if err = commitPlans(ctx, rootFS, plans); err != nil {
		if rollbackErr := rollbackHookFiles(context.Background(), hookSnapshot); rollbackErr != nil {
			return Binding{}, fmt.Errorf("connect failed and hook rollback was incomplete: %w", errors.Join(err, rollbackErr))
		}
		return Binding{}, err
	}
	return binding, nil
}

func exactFields(fields map[string]json.RawMessage, names ...string) bool {
	if len(fields) != len(names) {
		return false
	}
	for _, name := range names {
		if _, ok := fields[name]; !ok {
			return false
		}
	}
	return true
}

func stripBlock(content, start, end string) (string, error) {
	starts, ends := strings.Count(content, start), strings.Count(content, end)
	if starts == 0 && ends == 0 {
		return content, nil
	}
	if starts != 1 || ends != 1 {
		return "", errors.New("ambiguous managed block markers")
	}
	i, j := strings.Index(content, start), strings.Index(content, end)
	if j < i || (i > 0 && content[i-1] != '\n') {
		return "", errors.New("invalid managed block boundaries")
	}
	j += len(end)
	if j < len(content) && content[j] != '\n' && content[j] != '\r' {
		return "", errors.New("invalid managed block end")
	}
	if j < len(content) && content[j] == '\r' {
		j++
	}
	if j < len(content) && content[j] == '\n' {
		j++
	}
	return content[:i] + content[j:], nil
}
func appendBlock(content, start, end, body string) string {
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	return content + start + "\n" + strings.TrimRight(body, "\n") + "\n" + end + "\n"
}

// installedSkill is the exact file content connect writes for a managed skill;
// doctor compares installed files against the same bytes.
func installedSkill(content string) []byte {
	return []byte(strings.TrimRight(content, "\n") + "\n\n" + skillMarker)
}
