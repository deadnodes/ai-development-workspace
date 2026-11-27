package agentconnect

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"releasecontrol/internal/agentguide"
	"releasecontrol/internal/domain"
)

func RunSkill(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		_, err := fmt.Fprintln(out, "Usage: release-control skill push --product P --dir DIR [--url URL] [--token-env ENV | --profile PROFILE] [--dry-run]")
		return err
	}
	if args[0] != "push" {
		return errors.New("expected skill push")
	}
	flags := flag.NewFlagSet("release-control skill push", flag.ContinueOnError)
	flags.SetOutput(out)
	product := flags.String("product", "", "Product ID")
	dir := flags.String("dir", "", "skill directory containing SKILL.md")
	rawURL := flags.String("url", os.Getenv("RCP_URL"), "Control Plane URL (defaults to RCP_URL)")
	tokenEnv := flags.String("token-env", "", "environment variable containing an owner or admin token")
	profile := flags.String("profile", "", "saved credential profile")
	dryRun := flags.Bool("dry-run", false, "validate the directory without contacting RCP")
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 || *product == "" || *dir == "" || (*tokenEnv != "" && *profile != "") {
		return errors.New("skill push requires --product and --dir; choose at most one of --token-env or --profile")
	}
	skill, err := readSkillDirectory(*dir)
	if err != nil {
		return err
	}
	if *dryRun {
		_, err = fmt.Fprintf(out, "Would upload skill %s to Product %q (%d body bytes, %d files).\n", skill.Name, *product, len(skill.Body), len(skill.Files))
		return err
	}
	if *rawURL == "" {
		return errors.New("skill push requires --url or RCP_URL unless using --dry-run")
	}
	endpoint, err := secureEndpoint(*rawURL)
	if err != nil {
		return err
	}
	var token string
	if *tokenEnv != "" || *profile != "" {
		token, err = ResolveCredential(endpoint, *profile, *tokenEnv)
	} else {
		token, err = loadCredential(endpoint, "default")
		if errors.Is(err, os.ErrNotExist) {
			err = nil
		}
	}
	if err != nil {
		return err
	}
	payload, err := json.Marshal(struct {
		Action    string                 `json:"action"`
		ProductID string                 `json:"product_id"`
		Data      domain.AgentSkillInput `json:"data"`
	}{"upsert_agent_skill", *product, skill})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/api/commands", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("upload skill: %w", err)
	}
	defer response.Body.Close()
	decoder := json.NewDecoder(io.LimitReader(response.Body, 4<<20))
	if response.StatusCode != http.StatusOK {
		var failure struct {
			Error string `json:"error"`
		}
		_ = decoder.Decode(&failure)
		return fmt.Errorf("upload skill: HTTP %d %s", response.StatusCode, failure.Error)
	}
	var receipt domain.ProductAgentSkill
	if err := decoder.Decode(&receipt); err != nil || receipt.Name != skill.Name || receipt.ProductID != *product || receipt.Version == 0 {
		return errors.New("skill upload returned an invalid receipt")
	}
	if digest, err := hex.DecodeString(receipt.SHA256); err != nil || len(digest) != 32 || strings.ToLower(receipt.SHA256) != receipt.SHA256 {
		return errors.New("skill upload returned an invalid receipt")
	}
	_, err = fmt.Fprintf(out, "Uploaded %s to Product %q (version %d, SHA-256 %s).\n", receipt.Name, receipt.ProductID, receipt.Version, receipt.SHA256)
	return err
}

func readSkillDirectory(dir string) (domain.AgentSkillInput, error) {
	var skill domain.AgentSkillInput
	root, err := os.OpenRoot(dir)
	if err != nil {
		return skill, err
	}
	defer root.Close()
	read := func(path string, limit int) (string, error) {
		info, err := root.Lstat(path)
		if err != nil {
			return "", err
		}
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("skill file %q must be a regular file, not a symlink", path)
		}
		file, err := root.Open(path)
		if err != nil {
			return "", err
		}
		defer file.Close()
		data, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
		return string(data), err
	}
	const maxFrontmatter = 8 << 10
	markdown, err := read("SKILL.md", domain.MaxAgentSkillBodyBytes+maxFrontmatter)
	if err != nil {
		return skill, err
	}
	if len(markdown) > domain.MaxAgentSkillBodyBytes+maxFrontmatter {
		return skill, errors.New("SKILL.md exceeds the 64 KiB body and 8 KiB frontmatter budget")
	}
	first, markdown, found := strings.Cut(skillLF(markdown), "\n")
	front, body, closed := strings.Cut(markdown, "\n---\n")
	if !found || first != "---" || !closed || len(front) > maxFrontmatter {
		return skill, errors.New("SKILL.md needs YAML frontmatter with name and description")
	}
	var header struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
	}
	if err := yaml.Unmarshal([]byte(front), &header); err != nil {
		return skill, errors.New("invalid SKILL.md YAML frontmatter")
	}
	skill = domain.AgentSkillInput{Name: header.Name, Description: strings.TrimSpace(header.Description), Body: body, Files: map[string]string{}}
	if agentguide.BuiltInSkill(skill.Name) {
		return skill, errors.New("built-in skill names cannot be shadowed")
	}
	remaining := domain.MaxAgentSkillFilesBytes
	err = fs.WalkDir(root.FS(), ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || path == "SKILL.md" {
			return err
		}
		text, err := read(path, remaining)
		if err != nil {
			return err
		}
		remaining -= len(text)
		if remaining < 0 {
			return errors.New("skill files exceed 256 KiB total")
		}
		skill.Files[path] = text
		return nil
	})
	if err != nil {
		return skill, err
	}
	return skill, skill.Validate()
}
