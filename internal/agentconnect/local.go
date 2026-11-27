package agentconnect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"

	"github.com/pelletier/go-toml/v2"
	"releasecontrol/internal/sqlitefile"
)

func executableFile(info os.FileInfo) bool {
	return info.Mode().IsRegular() && (runtime.GOOS == "windows" || info.Mode().Perm()&0111 != 0)
}

func localMCPArgs(o Options) []string {
	return []string{"mcp", "--data", o.DataPath, "--workspace", o.Workspace}
}

func localCodexEntry(o Options) ([]byte, error) {
	entry := claudeEntry(o, "")
	if o.CodexAutoApprove {
		entry["default_tools_approval_mode"] = "approve"
	}
	return toml.Marshal(map[string]any{"mcp_servers": map[string]any{o.Name: entry}})
}

func installLocal(ctx context.Context, o Options) (Binding, error) {
	if o.URL != "" || o.ProductID != "" || o.TokenEnv != "" || o.Profile != "" || o.CodexNotify {
		return Binding{}, errors.New("--data cannot be combined with remote connection or turn-end hook options")
	}
	if o.Name == "" {
		o.Name = "release-control"
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_-]+$`).MatchString(o.Name) {
		return Binding{}, errors.New("invalid MCP server name")
	}
	if o.Harness == "" {
		o.Harness = "both"
	}
	if o.Harness != "both" && o.Harness != "codex" && o.Harness != "claude" {
		return Binding{}, errors.New("--harness must be codex, claude, or both")
	}
	var err error
	if o.Executable == "" {
		o.Executable, err = os.Executable()
		if err != nil {
			return Binding{}, err
		}
	}
	if !filepath.IsAbs(o.Executable) {
		return Binding{}, errors.New("--executable must be absolute")
	}
	info, err := os.Stat(o.Executable)
	if err != nil || !executableFile(info) {
		return Binding{}, errors.New("MCP executable must be an existing executable file")
	}
	if o.Workspace == "" {
		o.Workspace = "."
	}
	if o.Workspace, err = filepath.Abs(o.Workspace); err != nil {
		return Binding{}, err
	}
	if o.DataPath, err = filepath.Abs(o.DataPath); err != nil {
		return Binding{}, err
	}
	if _, err = sqlitefile.Inspect(o.DataPath); err != nil {
		return Binding{}, err
	}
	root, err := os.OpenRoot(o.Workspace)
	if err != nil {
		return Binding{}, err
	}
	defer root.Close()
	var plans []filePlan
	if o.Harness != "claude" {
		if err := userCodexConflict(o.Name); err != nil {
			return Binding{}, err
		}
		plan, err := readPlan(root, ".codex/config.toml")
		if err != nil {
			return Binding{}, err
		}
		outside, err := stripBlock(string(plan.before), configStart, configEnd)
		if err != nil {
			return Binding{}, err
		}
		var parsed localMCPConfig
		if err := toml.Unmarshal([]byte(outside), &parsed); err != nil {
			return Binding{}, err
		}
		if _, exists := parsed.Codex[o.Name]; exists {
			return Binding{}, errors.New("unmanaged Codex MCP server already uses requested name")
		}
		entry, err := localCodexEntry(o)
		if err != nil {
			return Binding{}, err
		}
		plan.after = []byte(appendBlock(outside, configStart, configEnd, string(entry)))
		if err := toml.Unmarshal(plan.after, &parsed); err != nil {
			return Binding{}, err
		}
		plans = append(plans, plan)
	}
	if o.Harness != "codex" {
		plan, err := readPlan(root, ".mcp.json")
		if err != nil {
			return Binding{}, err
		}
		plan.after, err = mergeClaudeMCP(plan.before, o, "")
		if err != nil {
			return Binding{}, err
		}
		plans = append(plans, plan)
	}
	return Binding{ServerName: o.Name}, commitPlans(ctx, root, plans)
}

type localMCPEntry struct {
	Command string   `json:"command" toml:"command"`
	Args    []string `json:"args" toml:"args"`
}
type localMCPConfig struct {
	Claude map[string]localMCPEntry `json:"mcpServers" toml:"-"`
	Codex  map[string]localMCPEntry `json:"-" toml:"mcp_servers"`
}

func doctorLocal(data, workspace, name string, out io.Writer) error {
	data, err := filepath.Abs(data)
	if err != nil {
		return err
	}
	exists, err := sqlitefile.Inspect(data)
	if err != nil {
		return err
	}
	if exists {
		fmt.Fprintf(out, "SQLite file: %s\n", data)
	} else {
		fmt.Fprintf(out, "SQLite file will be created at first launch: %s\n", data)
	}
	workspace, err = filepath.Abs(workspace)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return err
	}
	defer root.Close()
	workspaceInfo, err := root.Stat(".")
	if err != nil {
		return err
	}
	found := false
	for _, file := range []string{".codex/config.toml", ".mcp.json"} {
		plan, err := readPlan(root, file)
		if err != nil {
			return err
		}
		if !plan.exists {
			continue
		}
		var cfg localMCPConfig
		var entry localMCPEntry
		var ok bool
		if file == ".mcp.json" {
			err = json.Unmarshal(plan.before, &cfg)
			entry, ok = cfg.Claude[name]
		} else {
			err = toml.Unmarshal(plan.before, &cfg)
			entry, ok = cfg.Codex[name]
		}
		if err != nil {
			return fmt.Errorf("check %s: %w", file, err)
		}
		if !ok {
			continue
		}
		found = true
		if len(entry.Args) < 3 || !slices.Equal(entry.Args[:3], []string{"mcp", "--data", data}) || !filepath.IsAbs(entry.Command) {
			return fmt.Errorf("%s: %s does not launch the requested local SQLite file", file, name)
		}
		if len(entry.Args) != 5 || entry.Args[3] != "--workspace" || !filepath.IsAbs(entry.Args[4]) {
			return fmt.Errorf("%s: %s must pass exactly one absolute --workspace argument", file, name)
		}
		configuredWorkspace, err := os.Stat(entry.Args[4])
		if err != nil || !os.SameFile(workspaceInfo, configuredWorkspace) {
			return fmt.Errorf("%s: %s does not launch the requested --workspace directory", file, name)
		}
		info, err := os.Stat(entry.Command)
		if err != nil || !executableFile(info) {
			return fmt.Errorf("%s: configured release-control executable is unavailable", file)
		}
		fmt.Fprintf(out, "OK %s: %s\n", file, entry.Command)
	}
	if !found {
		return errors.New("no matching local MCP configuration; run release-control connect --data PATH")
	}
	return nil
}
