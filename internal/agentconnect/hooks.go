package agentconnect

import (
	"context"
	_ "embed"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
)

//go:embed rcp-turn-end.sh
var turnEndScript []byte

type hookInstallSnapshot struct {
	plans       []filePlan
	createdDirs []string
}

func installHookFiles(ctx context.Context, o Options, endpoint string) (hookInstallSnapshot, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return hookInstallSnapshot{}, err
	}
	root, err := os.OpenRoot(home)
	if err != nil {
		return hookInstallSnapshot{}, err
	}
	defer root.Close()
	config := map[string]string{"rcp_url": endpoint, "product_id": o.ProductID, "harness_kind": "codex"}
	if o.Harness == "claude" {
		config["harness_kind"] = "claude_code"
	}
	if o.Harness == "both" {
		config["harness_kind"] = "codex"
	}
	if o.Profile != "" {
		config["profile"] = o.Profile
		config["executable"] = o.Executable
	}
	if o.TokenEnv != "" {
		config["token_env"] = o.TokenEnv
	}
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return hookInstallSnapshot{}, err
	}
	data = append(data, '\n')
	plans := make([]filePlan, 0, 2)
	for _, name := range []string{".config/release-control/turn-end-" + o.ProductID + ".json", ".local/share/release-control/hooks/rcp-turn-end.sh"} {
		p, e := readPlan(root, name)
		if e != nil {
			return hookInstallSnapshot{}, e
		}
		plans = append(plans, p)
	}
	plans[0].after = data
	plans[0].installMode = 0600
	plans[1].after = turnEndScript
	plans[1].installMode = 0700
	var createdDirs []string
	for _, dir := range []string{".config", ".config/release-control", ".local", ".local/share", ".local/share/release-control", ".local/share/release-control/hooks"} {
		if _, err := root.Lstat(dir); os.IsNotExist(err) {
			createdDirs = append(createdDirs, dir)
		} else if err != nil {
			return hookInstallSnapshot{}, err
		}
	}
	if err := commitPlans(ctx, root, plans); err != nil {
		return hookInstallSnapshot{}, err
	}
	return hookInstallSnapshot{plans: plans, createdDirs: createdDirs}, nil
}

func rollbackHookFiles(ctx context.Context, snapshot hookInstallSnapshot) error {
	if len(snapshot.plans) == 0 {
		return nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(home)
	if err != nil {
		return err
	}
	defer root.Close()
	var restore []filePlan
	for _, p := range snapshot.plans {
		if p.exists {
			restore = append(restore, filePlan{relative: p.relative, before: mustRead(root, p.relative), after: p.before, exists: true, mode: p.mode})
		} else {
			if err := root.Remove(p.relative); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	if err := commitPlans(ctx, root, restore); err != nil {
		return err
	}
	for i := len(snapshot.createdDirs) - 1; i >= 0; i-- {
		_ = root.Remove(snapshot.createdDirs[i])
	}
	return nil
}

func mustRead(root *os.Root, path string) []byte { b, _ := root.ReadFile(path); return b }

func codexNotifyLine(o Options) string {
	config := hookConfigPath(o.ProductID)
	return "notify = [" + strconv.Quote(filepath.Join(homeDir(), ".local/share/release-control/hooks/rcp-turn-end.sh")) + ", " + strconv.Quote("--config") + ", " + strconv.Quote(config) + "]"
}
