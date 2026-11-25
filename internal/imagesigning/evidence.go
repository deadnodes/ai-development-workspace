package imagesigning

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"releasecontrol/internal/buildconfig"
	"strings"
)

// RepositoryIdentity removes the image tag and digest from a fully qualified ref.
func RepositoryIdentity(ref string) (string, bool) {
	name, digest, ok := strings.Cut(ref, "@")
	if !ok || !DigestPattern.MatchString(digest) {
		return "", false
	}
	colon := strings.LastIndex(name, ":")
	if colon <= strings.LastIndex(name, "/") || colon == len(name)-1 {
		return "", false
	}
	repository := name[:colon]
	if repository == "" || strings.HasSuffix(repository, "/") || strings.ContainsAny(repository, "?#@") {
		return "", false
	}
	return repository, true
}

// Evidence returns only cryptographically verified bodies. Missing tags are not infrastructure failures.
func (c *Client) Evidence(ctx context.Context, reg buildconfig.Registry, repo, credential, digest, suffix, pem string) ([][]byte, error) {
	r, e := c.registry(reg, repo, credential)
	if e != nil {
		return nil, e
	}
	m, e := r.manifest(ctx, Tag(digest, suffix))
	if e == ErrMissing {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	pub, e := buildconfig.PublicKey(pem)
	if e != nil {
		return nil, ErrKeyMismatch
	}
	var out [][]byte
	for _, l := range m.Layers {
		b, e := r.readBlob(ctx, l)
		if e != nil {
			return nil, e
		}
		if suffix == "att" && l.MediaType == EnvelopeType {
			var env Envelope
			if json.Unmarshal(b, &env) == nil && verifyEnvelope(env, buildconfig.SigningKey{PublicKeyPEM: pem}) {
				body, _ := base64.StdEncoding.DecodeString(env.Payload)
				out = append(out, body)
			}
		} else if suffix == "sig" && l.MediaType == SignatureType {
			sig, e := base64.StdEncoding.DecodeString(l.Annotations[SignatureAnnotation])
			sum := sha256.Sum256(b)
			if e == nil && ecdsa.VerifyASN1(pub, sum[:], sig) {
				out = append(out, b)
			}
		}
	}
	return out, nil
}

func SignatureMatches(body []byte, identity, digest string) bool {
	var v struct {
		Critical struct {
			Type     string            `json:"type"`
			Identity map[string]string `json:"identity"`
			Image    map[string]string `json:"image"`
		} `json:"critical"`
	}
	return json.Unmarshal(body, &v) == nil && v.Critical.Type == "cosign container image signature" && v.Critical.Identity["docker-reference"] == identity && v.Critical.Image["docker-manifest-digest"] == digest
}
