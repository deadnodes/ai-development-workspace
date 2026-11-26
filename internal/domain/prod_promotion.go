package domain

import "time"

type ProdSignature struct {
	SignatureTag   string `json:"signature_tag"`
	PayloadB64     string `json:"payload_b64"`
	SignatureB64   string `json:"signature_b64"`
	KeyFingerprint string `json:"key_fingerprint"`
}
type ProdPromotionItem struct {
	BuildID       string         `json:"build_id"`
	ImageID       string         `json:"image_id"`
	Repository    string         `json:"repository"`
	DevImageRef   string         `json:"dev_image_ref"`
	Digest        string         `json:"digest"`
	ProdImageRef  string         `json:"prod_image_ref"`
	Tag           string         `json:"tag"`
	SourceSHA     string         `json:"source_sha"`
	Result        string         `json:"result"`
	Reason        string         `json:"reason,omitempty"`
	ProdSignature *ProdSignature `json:"prod_signature,omitempty"`
}
type ProdPromotion struct {
	Meta
	EnvironmentID  string              `json:"environment_id"`
	Status         string              `json:"status"`
	Items          []ProdPromotionItem `json:"items"`
	OwnerApproval  string              `json:"owner_approval"`
	RequestedBy    string              `json:"requested_by"`
	RequestKey     string              `json:"request_key"`
	Attempts       int                 `json:"attempts"`
	ClaimedAt      *time.Time          `json:"claimed_at,omitempty"`
	LeaseExpiresAt *time.Time          `json:"lease_expires_at,omitempty"`
	ClaimID        string              `json:"claim_id,omitempty"`
	FinishedAt     *time.Time          `json:"finished_at,omitempty"`
}
type RequestProdPromotion struct {
	ProductID     string   `json:"product_id"`
	BuildIDs      []string `json:"build_ids"`
	OwnerApproval string   `json:"owner_approval"`
	RequestKey    string   `json:"request_key"`
}
type ClaimProdPromotion struct {
	ProductID     string `json:"product_id"`
	EnvironmentID string `json:"environment_id"`
	Wait          int    `json:"wait,omitempty"`
}
type ProdPromotionResult struct {
	ClaimID string              `json:"claim_id"`
	Items   []ProdPromotionItem `json:"items"`
}
type HeartbeatProdPromotion struct {
	ClaimID string `json:"claim_id"`
}
