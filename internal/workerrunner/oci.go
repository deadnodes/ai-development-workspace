package workerrunner

import (
	"crypto/sha256"
	"encoding/hex"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/ociverify"
)

func digestBytes(b []byte) string { s := sha256.Sum256(b); return "sha256:" + hex.EncodeToString(s[:]) }

type descriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
	Platform  *struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
	} `json:"platform,omitempty"`
}
type imageIndex struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	Manifests     []descriptor `json:"manifests"`
}
type imageManifest struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	Config        descriptor   `json:"config"`
	Layers        []descriptor `json:"layers"`
}

func VerifyOCI(file string, max int64, platform string) (domain.WorkerLocalOCI, error) {
	return ociverify.VerifyOCI(file, max, platform)
}
func VerifyOCIBytes(b []byte, max int64, platform string) (domain.WorkerLocalOCI, error) {
	return ociverify.VerifyOCIBytes(b, max, platform)
}
func verifyOCI(file string, max int64, platform string, e *domain.WorkerExecution, b []byte) (domain.WorkerLocalOCI, error) {
	return ociverify.VerifyWithExecution(file, max, platform, e, b)
}
