package domain

import "time"

type BuildSignature struct {
	ImageID        string     `json:"image_id"`
	ImageRef       string     `json:"image_ref"`
	Digest         string     `json:"digest"`
	KeyID          string     `json:"key_id"`
	KeyVersion     int        `json:"key_version"`
	KeyFingerprint string     `json:"key_fingerprint"`
	SignatureTag   string     `json:"signature_tag"`
	Attestations   []string   `json:"attestations"`
	Status         string     `json:"status"`
	SignedAt       *time.Time `json:"signed_at,omitempty"`
	FailureCode    string     `json:"failure_code,omitempty"`
	LastError      string     `json:"last_error,omitempty"`
}
type SignArtifactRequest struct {
	ProductID  string   `json:"product_id"`
	ProfileID  string   `json:"profile_id"`
	ImageID    string   `json:"image_id"`
	ImageRef   string   `json:"image_ref"`
	KeyIDs     []string `json:"key_ids"`
	Reason     string   `json:"reason"`
	RequestKey string   `json:"request_key"`
}
type ArtifactSignature struct {
	Meta
	Request       SignArtifactRequest `json:"request"`
	Status        string              `json:"status"`
	Signatures    []BuildSignature    `json:"signatures"`
	Attempts      int                 `json:"attempts"`
	NextAttemptAt time.Time           `json:"next_attempt_at"`
	StartedAt     time.Time           `json:"started_at"`
	Revision      string              `json:"revision,omitempty"`
}
type ListSignaturesRequest struct {
	ProductID string `json:"product_id"`
	Digest    string `json:"digest,omitempty"`
	Limit     int    `json:"limit,omitempty"`
	Cursor    string `json:"cursor,omitempty"`
}
type SignaturePage struct {
	Items      []ArtifactSignature `json:"items"`
	NextCursor string              `json:"next_cursor,omitempty"`
}
