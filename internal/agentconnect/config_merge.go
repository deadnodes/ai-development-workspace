package agentconnect

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

func claudeEntry(o Options, endpoint string) map[string]any {
	entry := map[string]any{"command": o.Executable}
	args := []string{"mcp", "--url", endpoint}
	if o.Profile != "" {
		args = append(args, "--profile", o.Profile)
	}
	if o.TokenEnv != "" {
		args = append(args, "--token-env", o.TokenEnv)
	}
	entry["args"] = args
	return entry
}

func mergeClaudeMCP(raw []byte, o Options, endpoint string) ([]byte, error) {
	root := map[string]any{}
	if len(bytes.TrimSpace(raw)) != 0 {
		if err := json.Unmarshal(raw, &root); err != nil {
			return nil, errors.New("existing .mcp.json is invalid; no files changed")
		}
	}
	servers, ok := root["mcpServers"]
	if !ok {
		servers = map[string]any{}
		root["mcpServers"] = servers
	}
	table, ok := servers.(map[string]any)
	if !ok {
		return nil, errors.New("existing .mcp.json mcpServers is not an object")
	}
	entry := claudeEntry(o, endpoint)
	if old, exists := table[o.Name]; exists {
		oldJSON, _ := json.Marshal(old)
		newJSON, _ := json.Marshal(entry)
		if !bytes.Equal(oldJSON, newJSON) {
			return nil, errors.New("unmanaged Claude MCP server already uses requested name")
		}
	} else {
		table[o.Name] = entry
	}
	data, err := json.MarshalIndent(root, "", "  ")
	return append(data, '\n'), err
}

func mergeStopHook(raw []byte, o Options, endpoint string) ([]byte, error) {
	root := map[string]any{}
	if len(bytes.TrimSpace(raw)) != 0 && json.Unmarshal(raw, &root) != nil {
		return nil, errors.New("existing Claude settings JSON is invalid; no files changed")
	}
	hooks, ok := root["hooks"].(map[string]any)
	if !ok {
		hooks = map[string]any{}
		root["hooks"] = hooks
	}
	stop, ok := hooks["Stop"].([]any)
	if !ok && hooks["Stop"] != nil {
		return nil, errors.New("existing Claude Stop hooks are invalid")
	}
	config := hookConfigPath(o.ProductID)
	command := fmt.Sprintf("RCP_HARNESS_KIND=claude_code %s --config %s", shellQuote(filepath.Join(homeDir(), ".local/share/release-control/hooks/rcp-turn-end.sh")), shellQuote(config))
	wanted := map[string]any{"matcher": "", "hooks": []any{map[string]any{"type": "command", "command": command}}}
	encoded, _ := json.Marshal(wanted)
	for _, v := range stop {
		b, _ := json.Marshal(v)
		if bytes.Equal(b, encoded) {
			data, err := json.MarshalIndent(root, "", "  ")
			return append(data, '\n'), err
		}
	}
	stop = append(stop, wanted)
	hooks["Stop"] = stop
	data, err := json.MarshalIndent(root, "", "  ")
	return append(data, '\n'), err
}

func homeDir() string { h, _ := os.UserHomeDir(); return h }
func hookConfigPath(product string) string {
	return filepath.Join(homeDir(), ".config/release-control/turn-end-"+product+".json")
}
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }

// userCodexConflict reports a user-level Codex MCP server with the same name
// that uses a URL transport. Codex merges user and project tables, so the
// project's stdio entry would become invalid ("url is not supported for stdio").
func userCodexConflict(name string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	path := filepath.Join(home, ".codex", "config.toml")
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var cfg struct {
		MCPServers map[string]map[string]any `toml:"mcp_servers"`
	}
	if toml.Unmarshal(raw, &cfg) != nil {
		return nil
	}
	if entry, ok := cfg.MCPServers[name]; ok {
		if _, hasURL := entry["url"]; hasURL {
			return fmt.Errorf("%s already defines MCP server %q with a url; Codex would merge it with the project stdio entry and fail. Rerun with --name %s-project, or remove that table from %s; no files changed", path, name, name, path)
		}
	}
	return nil
}

// plaintextCodexBearer reports whether the user-level Codex config stores a
// literal Authorization header. The value is never read into output.
func plaintextCodexBearer() bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	raw, err := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	if err != nil {
		return false
	}
	var cfg struct {
		MCPServers map[string]struct {
			Headers map[string]string `toml:"http_headers"`
		} `toml:"mcp_servers"`
	}
	if toml.Unmarshal(raw, &cfg) != nil {
		return false
	}
	for _, server := range cfg.MCPServers {
		for key, value := range server.Headers {
			if strings.EqualFold(key, "Authorization") && value != "" {
				return true
			}
		}
	}
	return false
}
