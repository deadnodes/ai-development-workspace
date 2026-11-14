package delivery

import (
	"regexp"
	"strings"
)

// RolloutPlan is immutable desired image identity, scoped to configured workloads.
type RolloutPlan struct{ Targets []RolloutTarget }
type RolloutTarget struct {
	Namespace   string `json:"namespace"`
	HelmRelease string `json:"helm_release,omitempty"`
	Deployment  string `json:"deployment,omitempty"`
	Repository  string `json:"repository"`
	Digest      string `json:"digest"`
	Reference   string `json:"reference"`
}
type RolloutEvidence struct {
	Target RolloutTarget `json:"target"`
	Phase  string        `json:"phase"`
	Reason string        `json:"reason,omitempty"`
}
type RolloutResult struct {
	Phase   string            `json:"phase"`
	Reason  string            `json:"reason,omitempty"`
	Targets []RolloutEvidence `json:"targets"`
}

// Only a controller reason token is accepted as the structured prefix.
var rolloutReasonToken = regexp.MustCompile(`^[A-Z][A-Za-z0-9]{0,127}$`)
var rolloutCredentials = regexp.MustCompile(`(?i)((?:["'][a-z_][a-z0-9_.-]*["']\s*[:=]|[a-z_][a-z0-9_.-]*\s*=|(?:[a-z0-9_-]*(?:token|password|secret|credential|api[_-]?key)|authorization)\s*:)\s*)(?:"(?:\\.|[^"\\])*"|'[^']*'|(?:bearer|basic)\s+[^\s,;]+|[^\s,;\]}]+)`)
var rolloutBearer = regexp.MustCompile(`(?i)\b(?:bearer|basic)\s+[^\s,;]+`)
var rolloutURLCredentials = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^/\s@]+@`)

// SafeRolloutReason allows a reason token and a bounded, credential-redacted message.
func SafeRolloutReason(s string) string {
	s = rolloutCredentials.ReplaceAllString(s, "${1}[REDACTED]")
	s = rolloutBearer.ReplaceAllString(s, "[REDACTED]")
	s = rolloutURLCredentials.ReplaceAllString(s, "${1}[REDACTED]@")
	reason, message, found := strings.Cut(s, ": ")
	prefix := ""
	if found && rolloutReasonToken.MatchString(reason) {
		prefix = reason + ": "
	} else {
		message = s
	}
	r := []rune(message)
	if len(r) > 300 {
		r = r[:300]
	}
	return prefix + string(r)
}
