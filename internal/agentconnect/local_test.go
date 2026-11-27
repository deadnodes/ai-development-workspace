package agentconnect

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

func TestLocalMCPConfigWindowsPaths(t *testing.T) {
	options := Options{Name: "release-control", Executable: `C:\Program Files\RCP\release-control.exe`, DataPath: `D:\Project Data\A #1.sqlite`, Workspace: `D:\Projects\Example Project`}
	tomlData, err := localCodexEntry(options)
	if err != nil {
		t.Fatal(err)
	}
	jsonData, err := mergeClaudeMCP(nil, options, "")
	if err != nil {
		t.Fatal(err)
	}
	var config localMCPConfig
	if err := toml.Unmarshal(tomlData, &config); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(jsonData, &config); err != nil {
		t.Fatal(err)
	}
	for _, entry := range []localMCPEntry{config.Codex[options.Name], config.Claude[options.Name]} {
		if entry.Command != options.Executable || !slices.Equal(entry.Args, []string{"mcp", "--data", options.DataPath, "--workspace", options.Workspace}) {
			t.Fatalf("configuration corrupted native paths: %+v", entry)
		}
	}
}

func TestLocalConnectAndDoctorWithoutDatabaseOrCredentials(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	workspace, database := t.TempDir(), filepath.Join(t.TempDir(), "project data.sqlite")
	args := []string{"--data", database, "--workspace", workspace}
	ctx := context.Background()
	if err := Run(ctx, args, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if err := Run(ctx, args, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if err := RunDoctor(ctx, args, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if err := RunDoctor(ctx, []string{"--data", database + ".other", "--workspace", workspace}, &bytes.Buffer{}); err == nil {
		t.Fatal("doctor accepted the wrong database")
	}
	claudePath := filepath.Join(workspace, ".mcp.json")
	var config localMCPConfig
	if err := json.Unmarshal(readFile(t, workspace, ".mcp.json"), &config); err != nil {
		t.Fatal(err)
	}
	entry := config.Claude["release-control"]
	for _, invalidArgs := range [][]string{append(slices.Clone(entry.Args[:4]), t.TempDir()), entry.Args[:3], append(slices.Clone(entry.Args), "--workspace", workspace)} {
		entry.Args = invalidArgs
		config.Claude["release-control"] = entry
		encoded, err := json.Marshal(config)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(claudePath, encoded, 0600); err != nil {
			t.Fatal(err)
		}
		if err := RunDoctor(ctx, args, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "--workspace") {
			t.Fatalf("doctor did not reject invalid workspace args %q: %v", invalidArgs, err)
		}
	}
	if _, err := os.Stat(database); !os.IsNotExist(err) {
		t.Fatalf("connect or doctor touched the database: %v", err)
	}
	if err := RunMCP(ctx, []string{"--data", database, "--url", "https://example.test"}, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
		t.Fatal("mcp accepted both --data and --url")
	}
}
