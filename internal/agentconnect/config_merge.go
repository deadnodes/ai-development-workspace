package agentconnect

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
