package github

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"releasecontrol/internal/delivery"
)

var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
var registryName = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)+$`)

func registryRepository(name string) (string, error) {
	name = strings.TrimPrefix(name, "ghcr.io/")
	if !registryName.MatchString(name) {
		return "", errors.New("only lowercase ghcr.io owner/image repositories supported")
	}
	return name, nil
}
func (p *Provider) registryGet(ctx context.Context, path, token string) ([]byte, http.Header, int, error) {
	req, e := http.NewRequestWithContext(ctx, "GET", p.registryURL+path, nil)
	if e != nil {
		return nil, nil, 0, errors.New("invalid registry request")
	}
	req.Header.Set("Accept", "application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, e := p.client.Do(req)
	if e != nil {
		return nil, nil, 0, errors.New("registry request failed or timed out")
	}
	defer res.Body.Close()
	b, e := io.ReadAll(io.LimitReader(res.Body, 4<<20+1))
	if e != nil || len(b) > 4<<20 {
		return nil, nil, res.StatusCode, errors.New("registry response unreadable or oversized")
	}
	return b, res.Header, res.StatusCode, nil
}
func (p *Provider) InspectArtifact(ctx context.Context, c delivery.Connection, r delivery.ArtifactRequest) (delivery.ArtifactResult, error) {
	result := delivery.ArtifactResult{ObservedAt: p.now().UTC()}
	repo, e := registryRepository(r.Repository)
	if e != nil {
		return result, e
	}
	if r.CredentialRef != "" || c.RegistryCredentialRef != "" {
		return result, fmt.Errorf("%w: GHCR inspection uses public access or fresh Actions GITHUB_TOKEN evidence, not PAT/App registry credentials", ErrUnsupported)
	}
	if r.ExpectedDigest != "" && !digestPattern.MatchString(r.ExpectedDigest) {
		return result, errors.New("invalid expected digest")
	}
	ref := r.Tag
	if r.ExpectedDigest != "" {
		ref = r.ExpectedDigest
	}
	if !imageTag.MatchString(ref) && !digestPattern.MatchString(ref) {
		return result, errors.New("artifact requires a valid tag or digest")
	}
	result.URI = "ghcr.io/" + repo
	b, h, status, e := p.registryGet(ctx, "/v2/"+repo+"/manifests/"+url.PathEscape(ref), "")
	if e != nil {
		return result, e
	}
	if status == 401 {
		var token struct {
			Token string `json:"token"`
		}
		tb, _, ts, te := p.registryGet(ctx, "/token?service=ghcr.io&scope="+url.QueryEscape("repository:"+repo+":pull"), "")
		if te != nil {
			return result, te
		}
		if ts == 200 && json.Unmarshal(tb, &token) == nil && token.Token != "" {
			b, h, status, e = p.registryGet(ctx, "/v2/"+repo+"/manifests/"+url.PathEscape(ref), token.Token)
			if e != nil {
				return result, e
			}
		} else {
			status = ts
			if status == 200 {
				status = 401
			}
		}
	}
	if status == 401 || status == 403 {
		if r.EvidenceRunID > 0 {
			return p.artifactEvidence(ctx, c, r, repo)
		}
		return result, fmt.Errorf("%w: private GHCR requires a fresh successful correlated workflow registry report", ErrUnauthorized)
	}
	if status == 404 {
		return result, nil
	}
	if status != 200 {
		return result, &APIError{Status: status, Operation: "registry inspect"}
	}
	sum := sha256.Sum256(b)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	if header := h.Get("Docker-Content-Digest"); header != "" && header != digest {
		return result, errors.New("registry digest does not match manifest bytes")
	}
	if r.ExpectedDigest != "" && digest != r.ExpectedDigest {
		return result, errors.New("registry returned a different digest")
	}
	result.Available = true
	result.Digest = digest
	result.URI += "@" + digest
	return result, nil
}
func (p *Provider) artifactEvidence(ctx context.Context, c delivery.Connection, r delivery.ArtifactRequest, repo string) (delivery.ArtifactResult, error) {
	var result delivery.ArtifactResult
	name, e := p.repository(ctx, c, r.EvidenceRepository)
	if e != nil {
		return result, e
	}
	report, e := p.resultReport(ctx, c, name, r.EvidenceRunID, r.EvidenceOperationID)
	if e != nil {
		return result, e
	}
	reportedRepo, e := registryRepository(report.ImageRepository)
	if e != nil {
		return result, e
	}
	if reportedRepo != repo || report.SourceSHA != r.SourceSHA || r.Tag != "" && report.ImageTag != r.Tag || report.ObservedAt.Before(p.now().Add(-10*time.Minute)) {
		return result, errors.New("workflow registry evidence is stale or mismatched")
	}
	if report.Available && r.ExpectedDigest != "" && report.Digest != r.ExpectedDigest {
		return result, errors.New("workflow registry digest differs from requested digest")
	}
	result = delivery.ArtifactResult{Available: report.Available, Digest: report.Digest, URI: "ghcr.io/" + repo, ObservedAt: report.ObservedAt}
	if report.Available {
		result.URI += "@" + report.Digest
	}
	return result, nil
}
