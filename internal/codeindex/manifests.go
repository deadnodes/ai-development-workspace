package codeindex

import (
	"encoding/json"
	"path"
	"regexp"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"gopkg.in/yaml.v3"
	"releasecontrol/internal/domain"
)

var pyRequirement = regexp.MustCompile(`^([A-Za-z0-9][\w.-]*)(?:\[[^\]]+\])?\s*([<>=!~][^;# ]*)?`)

func collectManifest(file *domain.CodeFile, body []byte) {
	base := strings.ToLower(path.Base(file.Path))
	switch {
	case base == "go.mod":
		block := false
		for _, line := range strings.Split(string(body), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 0 {
				continue
			}
			switch fields[0] {
			case "require":
				block = len(fields) >= 2 && fields[1] == "("
				if len(fields) >= 3 && !block {
					addDep(file, "go", fields[1], fields[2])
				}
			case ")":
				block = false
			default:
				if block && len(fields) >= 2 && !strings.HasPrefix(fields[0], "//") {
					addDep(file, "go", fields[0], fields[1])
				}
			}
		}
	case base == "package.json":
		var manifest struct {
			Name                                                                           string
			Dependencies, DevDependencies, PeerDependencies, OptionalDependencies, Scripts map[string]string
			Bin                                                                            json.RawMessage
		}
		if json.Unmarshal(body, &manifest) != nil {
			return
		}
		for kind, group := range map[string]map[string]string{"npm": manifest.Dependencies, "npm_dev": manifest.DevDependencies, "npm_peer": manifest.PeerDependencies, "npm_optional": manifest.OptionalDependencies} {
			for name, version := range group {
				addDep(file, kind, name, version)
			}
		}
		for name := range manifest.Scripts {
			addEntry(file, "npm_script", name)
		}
		var bins map[string]string
		var single string
		if json.Unmarshal(manifest.Bin, &bins) == nil {
			for name, target := range bins {
				addEntry(file, "npm_bin", name+": "+target)
			}
		} else if json.Unmarshal(manifest.Bin, &single) == nil {
			name := manifest.Name
			if name == "" {
				name = path.Base(single)
			}
			addEntry(file, "npm_bin", name+": "+single)
		}
	case base == "pyproject.toml", base == "cargo.toml":
		collectTOML(file, base, body)
	case strings.HasPrefix(base, "requirements") && strings.HasSuffix(base, ".txt"):
		for _, line := range strings.Split(string(body), "\n") {
			requirement(file, "python", strings.TrimSpace(line))
		}
	case base == "setup.cfg":
		console := false
		for _, line := range strings.Split(string(body), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "[") {
				console = false
			}
			if key, _, found := strings.Cut(line, "="); found && !console {
				console = strings.TrimSpace(key) == "console_scripts"
				continue
			}
			if console && strings.Contains(line, "=") {
				addEntry(file, "python_console", line)
			}
		}
	case base == "chart.yaml" || base == "kustomization.yaml" || base == "kustomization.yml" || base == "kustomization" || strings.HasPrefix(base, "docker-compose") || strings.HasPrefix(base, "compose."):
		var manifest struct {
			Services                     map[string]struct{ Image string }
			Dependencies                 []struct{ Name, Version string }
			Resources, Components, Bases []string
		}
		if yaml.Unmarshal(body, &manifest) != nil {
			return
		}
		for name, service := range manifest.Services {
			addDep(file, "compose_service", name, service.Image)
		}
		for _, dep := range manifest.Dependencies {
			addDep(file, "helm", dep.Name, dep.Version)
		}
		for _, items := range [][]string{manifest.Resources, manifest.Components, manifest.Bases} {
			for _, name := range items {
				addDep(file, "kustomize", name, "")
			}
		}
	}
}

func requirement(file *domain.CodeFile, kind, value string) {
	if matched := pyRequirement.FindStringSubmatch(value); matched != nil {
		addDep(file, kind, matched[1], matched[2])
	}
}

func collectTOML(file *domain.CodeFile, base string, body []byte) {
	var manifest map[string]any
	if toml.Unmarshal(body, &manifest) != nil {
		return
	}
	object := func(parent map[string]any, key string) map[string]any {
		value, _ := parent[key].(map[string]any)
		return value
	}
	deps := func(group map[string]any, kind string) {
		for name, value := range group {
			version, _ := value.(string)
			if fields, ok := value.(map[string]any); ok {
				version, _ = fields["version"].(string)
			}
			addDep(file, kind, name, version)
		}
	}
	if base == "cargo.toml" {
		groups := []map[string]any{manifest, object(manifest, "workspace")}
		for target := range object(manifest, "target") {
			groups = append(groups, object(object(manifest, "target"), target))
		}
		for _, group := range groups {
			for _, kind := range []string{"dependencies", "dev-dependencies", "build-dependencies"} {
				deps(object(group, kind), "cargo_"+kind)
			}
		}
		return
	}
	project := object(manifest, "project")
	requires := func(values any, kind string) {
		if items, ok := values.([]any); ok {
			for _, raw := range items {
				if value, ok := raw.(string); ok {
					requirement(file, kind, value)
				}
			}
		}
	}
	requires(project["dependencies"], "python")
	for _, values := range object(project, "optional-dependencies") {
		requires(values, "python_optional")
	}
	poetry := object(object(manifest, "tool"), "poetry")
	deps(object(poetry, "dependencies"), "poetry")
	for _, scripts := range []map[string]any{object(project, "scripts"), object(poetry, "scripts"), object(object(project, "entry-points"), "console_scripts")} {
		for name, target := range scripts {
			if value, ok := target.(string); ok {
				addEntry(file, "python_console", name+": "+value)
			}
		}
	}
}
