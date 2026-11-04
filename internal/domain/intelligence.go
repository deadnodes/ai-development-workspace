package domain

import "releasecontrol/internal/intelligence"

const IntelligenceOperationKind = "INTELLIGENCE"

// IntelligenceTask stores authenticated ciphertext, never prompts, excerpts,
// generated prose or model patches in ordinary state/history/backup records.
type IntelligenceTask struct {
	ConflictID        string             `json:"conflict_id,omitempty"`
	SourceOperationID string             `json:"source_operation_id,omitempty"`
	Kind              string             `json:"kind"`
	ConfigRevision    string             `json:"config_revision"`
	PromptVersion     string             `json:"prompt_version"`
	Model             string             `json:"model"`
	InputFingerprint  string             `json:"input_fingerprint"`
	ResultFingerprint string             `json:"result_fingerprint,omitempty"`
	InputCiphertext   string             `json:"input_ciphertext"`
	ResultCiphertext  string             `json:"result_ciphertext,omitempty"`
	Usage             intelligence.Usage `json:"usage"`
}

// Only an immutable reference is persisted in a worker job. The proposal itself
// is decrypted into an authenticated lease response and never its journal.
type IntelligencePatchBinding struct {
	TaskID            string   `json:"task_id"`
	ResultFingerprint string   `json:"result_fingerprint"`
	BaseSHA           string   `json:"base_sha"`
	HeadSHA           string   `json:"head_sha"`
	AllowedPaths      []string `json:"allowed_paths"`
}
