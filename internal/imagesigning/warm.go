package imagesigning

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"releasecontrol/internal/buildconfig"
)

// Warm fetches the immutable image and verifies every child manifest and referenced
// config/layer blob is present in this registry. A tag must resolve to the same digest.
func (c *Client) Warm(ctx context.Context, reg buildconfig.Registry, repo, credential, digest, tag string) error {
	r, e := c.registry(reg, repo, credential)
	if e != nil {
		return e
	}
	if !DigestPattern.MatchString(digest) || tag == "" || strings.ContainsAny(tag, "/@?#") {
		return ErrUnavailable
	}
	tagBody, tagHead, code, e := r.request(ctx, "GET", r.base()+"/manifests/"+tag, "", nil)
	if e != nil || code != 200 || Hash(tagBody) != digest || tagHead.Get("Docker-Content-Digest") != "" && tagHead.Get("Docker-Content-Digest") != digest {
		return fmt.Errorf("tag digest mismatch")
	}
	visited := map[string]bool{}
	var warm func(string, int) error
	warm = func(d string, depth int) error {
		if depth > 4 || visited[d] {
			return nil
		}
		visited[d] = true
		if !DigestPattern.MatchString(d) {
			return ErrUnavailable
		}
		_, hh, hc, he := r.request(ctx, "HEAD", r.base()+"/manifests/"+d, "", nil)
		if he != nil || hc != 200 || hh.Get("Docker-Content-Digest") != "" && hh.Get("Docker-Content-Digest") != d {
			return ErrUnavailable
		}
		body, head, code, e := r.request(ctx, "GET", r.base()+"/manifests/"+d, "", nil)
		if e != nil || code != 200 || Hash(body) != d || head.Get("Docker-Content-Digest") != "" && head.Get("Docker-Content-Digest") != d {
			return ErrUnavailable
		}
		var m struct {
			Schema    int    `json:"schemaVersion"`
			Media     string `json:"mediaType"`
			Manifests []struct {
				Digest string `json:"digest"`
			} `json:"manifests"`
			Config Descriptor   `json:"config"`
			Layers []Descriptor `json:"layers"`
		}
		if json.Unmarshal(body, &m) != nil || m.Schema != 2 || len(m.Manifests) > 128 || len(m.Layers) > 256 {
			return ErrUnavailable
		}
		if len(m.Manifests) > 0 {
			for _, child := range m.Manifests {
				if e := warm(child.Digest, depth+1); e != nil {
					return e
				}
			}
			return nil
		}
		blobs := append([]Descriptor{m.Config}, m.Layers...)
		for _, b := range blobs {
			if !DigestPattern.MatchString(b.Digest) {
				return ErrUnavailable
			}
			_, bh, code, e := r.request(ctx, "HEAD", r.base()+"/blobs/"+b.Digest, "", nil)
			if e != nil || code != 200 || bh.Get("Docker-Content-Digest") != "" && bh.Get("Docker-Content-Digest") != b.Digest {
				return ErrUnavailable
			}
		}
		return nil
	}
	return warm(digest, 0)
}

type SignatureBundle struct {
	Payload   []byte
	Signature []byte
}

// SignatureBundles returns only payload/signature pairs verified by the supplied key.
func (c *Client) SignatureBundles(ctx context.Context, reg buildconfig.Registry, repo, credential, digest, pem string) ([]SignatureBundle, error) {
	r, e := c.registry(reg, repo, credential)
	if e != nil {
		return nil, e
	}
	m, e := r.manifest(ctx, Tag(digest, "sig"))
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
	var out []SignatureBundle
	for _, l := range m.Layers {
		if l.MediaType != SignatureType {
			continue
		}
		payload, e := r.readBlob(ctx, l)
		if e != nil {
			return nil, e
		}
		sig, e := base64.StdEncoding.DecodeString(l.Annotations[SignatureAnnotation])
		if e != nil {
			continue
		}
		sum := sha256.Sum256(payload)
		if !ecdsa.VerifyASN1(pub, sum[:], sig) {
			continue
		}
		out = append(out, SignatureBundle{Payload: payload, Signature: sig})
	}
	return out, nil
}
