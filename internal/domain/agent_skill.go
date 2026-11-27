package domain

import (
	"fmt"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	MaxAgentSkillBodyBytes  = 64 << 10
	MaxAgentSkillFilesBytes = 256 << 10
	MaxAgentSkillFiles      = 64
)

var agentSkillName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}$`)
var agentRoleProductID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)
var agentSkillFilePath = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,240}$`)

func ValidAgentSkillName(name string) bool { return agentSkillName.MatchString(name) }

func ValidAgentRoleProductID(id string) bool { return agentRoleProductID.MatchString(id) }

type AgentRoles struct {
	OrchestratorSkill string `json:"orchestrator_skill"`
	WorkerSkill       string `json:"worker_skill"`
}

type AgentSkillInput struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Body        string            `json:"body"`
	Files       map[string]string `json:"files,omitempty"`
}

func (input AgentSkillInput) Validate() error {
	if !ValidAgentSkillName(input.Name) {
		return fmt.Errorf("skill name must match %s", agentSkillName)
	}
	if strings.TrimSpace(input.Description) == "" || len(input.Description) > 512 || strings.ContainsAny(input.Description, "\r\n\x00") || !utf8.ValidString(input.Description) {
		return fmt.Errorf("skill description must be one nonempty line of at most 512 bytes")
	}
	if len(input.Body) > MaxAgentSkillBodyBytes {
		return fmt.Errorf("skill body exceeds 64 KiB")
	}
	if strings.TrimSpace(input.Body) == "" || !utf8.ValidString(input.Body) || strings.ContainsRune(input.Body, 0) {
		return fmt.Errorf("skill body must be nonempty UTF-8 text")
	}
	if len(input.Files) > MaxAgentSkillFiles {
		return fmt.Errorf("skill files exceed %d entries", MaxAgentSkillFiles)
	}
	total := 0
	for name, text := range input.Files {
		total += len(name) + len(text)
		if total > MaxAgentSkillFilesBytes {
			return fmt.Errorf("skill files exceed 256 KiB total")
		}
		if !agentSkillFilePath.MatchString(name) || path.IsAbs(name) || path.Clean(name) != name || name == "." || name == ".." || strings.HasPrefix(name, "../") || strings.EqualFold(name, "SKILL.md") || !utf8.ValidString(text) || strings.ContainsRune(text, 0) {
			return fmt.Errorf("invalid skill file: expected safe relative path and UTF-8 text")
		}
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			if _, exists := input.Files[parent]; exists {
				return fmt.Errorf("skill files cannot share a file and directory path")
			}
		}
	}
	return nil
}

type ProductAgentSkill struct {
	Meta
	AgentSkillInput
	Version     uint64            `json:"version"`
	SHA256      string            `json:"sha256"`
	BodySHA256  string            `json:"body_sha256"`
	FilesSHA256 map[string]string `json:"files_sha256,omitempty"`
	UpdatedBy   string            `json:"updated_by"`
}
