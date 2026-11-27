package domain

import (
	"fmt"
	"strings"
	"testing"
)

func TestAgentSkillValidationBudgetsPathsAndEmptyFiles(t *testing.T) {
	skill := AgentSkillInput{Name: "team-worker", Description: "Worker instructions", Body: "Instructions", Files: map[string]string{}}
	for index := range MaxAgentSkillFiles {
		skill.Files[fmt.Sprintf("empty-%02d", index)] = ""
	}
	if err := skill.Validate(); err != nil {
		t.Fatal(err)
	}
	skill.Files["extra"] = ""
	if err := skill.Validate(); err == nil || !strings.Contains(err.Error(), "64 entries") {
		t.Fatalf("extra empty file not rejected: %v", err)
	}
	name := "notes.txt"
	skill.Files = map[string]string{name: strings.Repeat("a", MaxAgentSkillFilesBytes-len(name))}
	if err := skill.Validate(); err != nil {
		t.Fatal(err)
	}
	skill.Files[name] += "a"
	if err := skill.Validate(); err == nil || !strings.Contains(err.Error(), "256 KiB") {
		t.Fatalf("path bytes not charged: %v", err)
	}
}
