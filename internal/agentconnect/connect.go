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
)

const configStart = "# BEGIN release-control managed MCP"
const configEnd = "# END release-control managed MCP"
const agentsStart = "<!-- BEGIN release-control managed agent instructions -->"
const agentsEnd = "<!-- END release-control managed agent instructions -->"
const skillMarker = "<!-- release-control managed skill -->\n"

type Options struct{ URL, Workspace, ProductID, TokenEnv, Name string }
type Binding struct {
	URL        string `json:"url"`
	ProductID  string `json:"product_id"`
	ServerName string `json:"server_name"`
	TokenEnv   string `json:"token_env,omitempty"`
}
type kit struct {
	Skill        struct{ Name, Content, SHA256 string }
	Instructions string
}

func Run(ctx context.Context, args []string, out io.Writer) error {
	var o Options
	flags := flag.NewFlagSet("release-control connect", flag.ContinueOnError)
	flags.SetOutput(out)
	flags.StringVar(&o.URL, "url", "", "Control Plane base URL")
	flags.StringVar(&o.Workspace, "workspace", "", "project directory")
	flags.StringVar(&o.ProductID, "product-id", "", "Product ID (optional only when server has one Product)")
	flags.StringVar(&o.TokenEnv, "token-env", "", "environment variable containing bearer token; only its name is saved")
	flags.StringVar(&o.Name, "name", "release-control", "project MCP server name")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	binding, err := Install(ctx, o)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "Connected project to Product %s at %s.\nInstalled project MCP configuration, rcp-handoff skill and AGENTS.md instructions.\nTrust this project in Codex, then start a new chat or reload MCP configuration.\n", binding.ProductID, binding.URL)
	return err
}

func Install(ctx context.Context, o Options) (Binding, error) {
	var binding Binding
	base, err := url.Parse(o.URL)
	if err != nil || base.Hostname() == "" || (base.Scheme != "http" && base.Scheme != "https") || base.User != nil || base.RawQuery != "" || base.Fragment != "" || strings.ContainsAny(o.URL, "\r\n\t") {
		return binding, errors.New("provide an HTTP(S) base URL without credentials, query or fragment")
	}
	if o.Workspace == "" {
		return binding, errors.New("--workspace is required")
	}
	if o.Name == "" {
		o.Name = "release-control"
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_-]+$`).MatchString(o.Name) {
		return binding, errors.New("MCP name must contain only letters, digits, underscore or hyphen")
	}
	token := ""
	if o.TokenEnv != "" {
		if !regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`).MatchString(o.TokenEnv) {
			return binding, errors.New("invalid token environment variable name")
		}
		token = os.Getenv(o.TokenEnv)
		if token == "" {
			return binding, errors.New("token environment variable is empty")
		}
	}
	absolute, err := filepath.Abs(o.Workspace)
	if err != nil {
		return binding, err
	}
	root, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return binding, err
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return binding, errors.New("workspace must be an existing directory")
	}
	rootFS, err := os.OpenRoot(root)
	if err != nil {
		return binding, err
	}
	defer rootFS.Close()
	endpoint := strings.TrimRight(base.String(), "/")
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
		if err = json.Unmarshal(data, out); err != nil {
			return errors.New("server returned invalid JSON")
		}
		return nil
	}
	var agentKit kit
	if err = get("/api/agent-kit", &agentKit); err != nil {
		return binding, err
	}
	digest := sha256.Sum256([]byte(agentKit.Skill.Content))
	if agentKit.Skill.Name != "rcp-handoff" || agentKit.Skill.Content == "" || agentKit.Instructions == "" || agentKit.Skill.SHA256 != hex.EncodeToString(digest[:]) {
		return binding, errors.New("agent kit identity or checksum is invalid")
	}
	if strings.Contains(agentKit.Instructions, agentsStart) || strings.Contains(agentKit.Instructions, agentsEnd) {
		return binding, errors.New("agent kit contains reserved installation markers")
	}
	if o.ProductID == "" {
		var state struct {
			Products []struct {
				ID string `json:"id"`
			} `json:"products"`
		}
		if err = get("/api/state", &state); err != nil {
			return binding, err
		}
		if len(state.Products) != 1 || state.Products[0].ID == "" {
			return binding, errors.New("provide --product-id: server must have exactly one Product for automatic selection")
		}
		o.ProductID = state.Products[0].ID
	}
	if strings.ContainsAny(o.ProductID, "\r\n\t") || strings.TrimSpace(o.ProductID) == "" {
		return binding, errors.New("invalid Product ID")
	}
	var productContext map[string]any
	if err = get("/api/products/"+url.PathEscape(o.ProductID)+"/context", &productContext); err != nil {
		return binding, err
	}
	binding = Binding{URL: endpoint, ProductID: o.ProductID, ServerName: o.Name, TokenEnv: o.TokenEnv}
	paths := []string{".codex/config.toml", ".agents/skills/rcp-handoff/SKILL.md", "AGENTS.md", ".release-control.json"}
	plans := make([]filePlan, 0, len(paths))
	for _, path := range paths {
		plan, err := readPlan(rootFS, path)
		if err != nil {
			return Binding{}, err
		}
		plans = append(plans, plan)
	}
	outside, err := stripBlock(string(plans[0].before), configStart, configEnd)
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
	entry := "[mcp_servers." + strconv.Quote(o.Name) + "]\nurl = " + strconv.Quote(endpoint+"/mcp") + "\n"
	if o.TokenEnv != "" {
		entry += "bearer_token_env_var = " + strconv.Quote(o.TokenEnv) + "\n"
	}
	plans[0].after = []byte(appendBlock(outside, configStart, configEnd, entry))
	if err = toml.Unmarshal(plans[0].after, &parsed); err != nil {
		return Binding{}, errors.New("managed MCP entry conflicts with existing TOML")
	}
	if plans[1].exists && !bytes.HasSuffix(plans[1].before, []byte(skillMarker)) {
		return Binding{}, errors.New("unmanaged rcp-handoff skill exists; no files changed")
	}
	plans[1].after = []byte(strings.TrimRight(agentKit.Skill.Content, "\n") + "\n\n" + skillMarker)
	outside, err = stripBlock(string(plans[2].before), agentsStart, agentsEnd)
	if err != nil {
		return Binding{}, err
	}
	instructions := fmt.Sprintf("Control Plane: %s\nProduct ID: %s\nMCP server: %s\nBinding: `.release-control.json`.\n\n%s\n\nUse the installed `rcp-handoff` skill when pausing, finishing, or transferring context. Record work checkpoints in the Control Plane through MCP; resume the Product/Feature before changing code.\n", endpoint, o.ProductID, o.Name, agentKit.Instructions)
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
	if err = commitPlans(ctx, rootFS, plans); err != nil {
		return Binding{}, err
	}
	return binding, nil
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
