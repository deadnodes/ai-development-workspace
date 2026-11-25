package prodsigner

import (
	"context"
	"encoding/json"
	"releasecontrol/internal/buildconfig"
	"releasecontrol/internal/imagesigning"
	"strings"
)

// VerifyDevEvidence applies the shared dev trust checks used by Flux reconcile and
// explicit promotion. A nonempty reason is a deterministic rejection; err is I/O.
func VerifyDevEvidence(ctx context.Context, c Config, client *imagesigning.Client, repo, expectedIdentity, digest, expectedBuild, expectedSHA string) (buildID, sourceSHA, reason string, err error) {
	reg := buildconfig.Registry{Origin: c.Registry.Origin, InsecureHTTP: c.Registry.InsecureHTTP}
	credential := pathBase(c.Registry.CredentialsFile)
	sigs, e := client.Evidence(ctx, reg, repo, credential, digest, "sig", c.DevPublicKeyPEM)
	if e != nil {
		return "", "", "", e
	}
	signature := false
	for _, b := range sigs {
		var p struct {
			Critical struct {
				Identity map[string]string `json:"identity"`
			} `json:"critical"`
		}
		if json.Unmarshal(b, &p) != nil {
			continue
		}
		identity := p.Critical.Identity["docker-reference"]
		if expectedIdentity != "" {
			identity = expectedIdentity
		}
		if imagesigning.SignatureMatches(b, identity, digest) {
			signature = true
		}
	}
	if !signature {
		return "", "", "DEV_SIGNATURE_MISSING", nil
	}
	atts, e := client.Evidence(ctx, reg, repo, credential, digest, "att", c.DevPublicKeyPEM)
	if e != nil {
		return "", "", "", e
	}
	if len(atts) == 0 {
		return "", "", "PROVENANCE_INVALID", nil
	}
	ref := &Ref{Repo: repo, Digest: digest}
	found := false
	for _, body := range atts {
		bid, sha, ok := approved(body, ref, c)
		if !ok {
			if provenanceCINotGreen(body, c) {
				reason = "CI_NOT_GREEN"
			}
			if provenanceSecurityFailed(body, c) {
				reason = "SECURITY_GATE_FAILED"
			}
			continue
		}
		found = true
		buildID, sourceSHA = bid, sha
		break
	}
	if !found {
		if reason == "" {
			reason = "PROVENANCE_INVALID"
		}
		return "", "", reason, nil
	}
	if expectedBuild != "" && buildID != expectedBuild {
		return buildID, sourceSHA, "BUILD_ID_MISMATCH", nil
	}
	if expectedSHA != "" && sourceSHA != expectedSHA {
		return buildID, sourceSHA, "SOURCE_SHA_MISMATCH", nil
	}
	revision, e := client.Inspect(ctx, reg, repo, credential, digest)
	if e != nil {
		return "", "", "", e
	}
	if revision != sourceSHA {
		return buildID, sourceSHA, "REVISION_MISMATCH", nil
	}
	return buildID, sourceSHA, "", nil
}

func provenanceSecurityFailed(body []byte, c Config) bool {
	if !c.RequireSecurityScan {
		return false
	}
	var p provenance
	if json.Unmarshal(body, &p) != nil {
		return false
	}
	found := false
	for _, b := range p.Predicate.RunDetails.Byproducts {
		if b.Name == "security_scan" {
			found = true
			if b.Annotations.Verdict != "PASS" || b.Annotations.Blocking != 0 || b.Annotations.GitleaksFindings != 0 {
				return true
			}
		}
	}
	return !found
}

func provenanceCINotGreen(body []byte, c Config) bool {
	var p provenance
	if json.Unmarshal(body, &p) != nil {
		return false
	}
	for _, b := range p.Predicate.RunDetails.Byproducts {
		if b.Name != "required_status_contexts" {
			continue
		}
		for _, n := range b.Annotations.Required {
			if b.Annotations.Observed[n] != "success" {
				return true
			}
		}
		for _, v := range b.Annotations.Observed {
			if v != "success" {
				return true
			}
		}
	}
	return false
}
func pathBase(p string) string {
	parts := strings.Split(strings.TrimRight(p, "/"), "/")
	if len(parts) == 0 {
		return ""
	}
	return parts[len(parts)-1]
}
