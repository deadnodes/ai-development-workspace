package imagesigning

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"releasecontrol/internal/buildconfig"
	"releasecontrol/internal/secretfile"
	"strings"
)

const ManifestType = "application/vnd.oci.image.manifest.v1+json"
const ConfigType = "application/vnd.oci.image.config.v1+json"
const SignatureType = "application/vnd.dev.cosign.simplesigning.v1+json"
const EnvelopeType = "application/vnd.dsse.envelope.v1+json"
const SignatureAnnotation = "dev.cosignproject.cosign/signature"
const ProvenanceType = "https://slsa.dev/provenance/v1"
const PayloadType = "application/vnd.in-toto+json"

var DigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
var repositoryPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_./-]*$`)

func Hash(b []byte) string             { h := sha256.Sum256(b); return "sha256:" + hex.EncodeToString(h[:]) }
func Tag(digest, suffix string) string { return strings.ReplaceAll(digest, ":", "-") + "." + suffix }

type Descriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int               `json:"size"`
	Annotations map[string]string `json:"annotations,omitempty"`
}
type Manifest struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType"`
	Config        Descriptor        `json:"config"`
	Layers        []Descriptor      `json:"layers"`
	Annotations   map[string]string `json:"annotations,omitempty"`
}
type registry struct {
	client    *Client
	config    buildconfig.Registry
	repo, ref string
}

func (c *Client) registry(r buildconfig.Registry, repo, ref string) (registry, error) {
	u, e := url.Parse(r.Origin)
	if e != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || (u.Scheme != "https" && !(u.Scheme == "http" && r.InsecureHTTP)) || !repositoryPattern.MatchString(repo) || strings.Contains(repo, "..") || strings.Contains(repo, "//") {
		return registry{}, ErrUnavailable
	}
	return registry{c, r, repo, ref}, nil
}
func (r registry) request(ctx context.Context, method, target, media string, body []byte) ([]byte, http.Header, int, error) {
	origin := strings.TrimRight(r.config.Origin, "/")
	u, e := url.Parse(target)
	if e != nil {
		return nil, nil, 0, ErrUnavailable
	}
	base, _ := url.Parse(origin)
	u = base.ResolveReference(u)
	if u.Scheme != base.Scheme || u.Host != base.Host || u.User != nil || u.Fragment != "" || !strings.HasPrefix(u.Path, "/v2/"+r.repo+"/") {
		return nil, nil, 0, ErrUnavailable
	}
	username, password := "", ""
	if r.ref != "" {
		b, e := secretfile.Read(r.ref)
		if e != nil {
			return nil, nil, 0, ErrUnavailable
		}
		var v struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		d := json.NewDecoder(bytes.NewReader(b))
		d.DisallowUnknownFields()
		if d.Decode(&v) != nil || d.Decode(new(any)) != io.EOF || v.Username == "" || v.Password == "" {
			return nil, nil, 0, ErrUnavailable
		}
		username, password = v.Username, v.Password
	}
	perform := func(bearer string) (*http.Response, error) {
		req, e := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
		if e != nil {
			return nil, ErrUnavailable
		}
		req.Header.Set("Accept", ManifestType+", application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.v2+json, application/vnd.docker.distribution.manifest.list.v2+json")
		if media != "" {
			req.Header.Set("Content-Type", media)
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		} else if username != "" {
			req.SetBasicAuth(username, password)
		}
		return r.client.http.Do(req)
	}
	res, e := perform("")
	if e != nil {
		return nil, nil, 0, ErrUnavailable
	}
	if res.StatusCode == 401 {
		challenge := res.Header.Get("WWW-Authenticate")
		res.Body.Close()
		token, e := r.bearer(ctx, challenge, username, password)
		if e != nil {
			return nil, nil, 0, e
		}
		res, e = perform(token)
		if e != nil {
			return nil, nil, 0, ErrUnavailable
		}
	}
	b, e := bounded(res)
	if e != nil {
		return nil, nil, 0, e
	}
	return b, res.Header, res.StatusCode, nil
}
func (r registry) bearer(ctx context.Context, challenge, user, password string) (string, error) {
	if !strings.HasPrefix(challenge, "Bearer ") {
		return "", ErrUnavailable
	}
	fields := map[string]string{}
	re := regexp.MustCompile(`([a-z]+)="([^"\r\n]*)"`)
	for _, m := range re.FindAllStringSubmatch(strings.TrimPrefix(challenge, "Bearer "), -1) {
		fields[m[1]] = m[2]
	}
	u, e := url.Parse(fields["realm"])
	origin, _ := url.Parse(r.config.Origin)
	// Do not forward registry credentials to another origin supplied by a challenge.
	if e != nil || u.Scheme != origin.Scheme || u.Host != origin.Host || u.User != nil || u.Fragment != "" {
		return "", ErrUnavailable
	}
	q := u.Query()
	q.Set("service", fields["service"])
	q.Set("scope", "repository:"+r.repo+":pull,push")
	u.RawQuery = q.Encode()
	req, e := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if e != nil {
		return "", ErrUnavailable
	}
	if user != "" {
		req.SetBasicAuth(user, password)
	}
	res, e := r.client.http.Do(req)
	if e != nil {
		return "", ErrUnavailable
	}
	b, e := bounded(res)
	var out struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if e != nil || res.StatusCode != 200 || json.Unmarshal(b, &out) != nil {
		return "", ErrUnavailable
	}
	if out.Token == "" {
		out.Token = out.AccessToken
	}
	if out.Token == "" {
		return "", ErrUnavailable
	}
	return out.Token, nil
}
func (r registry) base() string { return "/v2/" + r.repo }
func (r registry) blob(ctx context.Context, body []byte) error {
	digest := Hash(body)
	_, _, code, e := r.request(ctx, "HEAD", r.base()+"/blobs/"+digest, "", nil)
	if e != nil {
		return e
	}
	if code == 200 {
		return nil
	}
	if code != 404 {
		return ErrUnavailable
	}
	_, h, code, e := r.request(ctx, "POST", r.base()+"/blobs/uploads/", "", nil)
	if e != nil || code != 202 {
		return ErrUnavailable
	}
	u, e := url.Parse(h.Get("Location"))
	if e != nil || h.Get("Location") == "" {
		return ErrUnavailable
	}
	q := u.Query()
	q.Set("digest", digest)
	u.RawQuery = q.Encode()
	_, _, code, e = r.request(ctx, "PUT", u.String(), "application/octet-stream", body)
	if e != nil || code != 201 {
		return ErrUnavailable
	}
	return nil
}
func (r registry) manifest(ctx context.Context, tag string) (Manifest, error) {
	var m Manifest
	b, _, code, e := r.request(ctx, "GET", r.base()+"/manifests/"+tag, "", nil)
	if e != nil {
		return m, e
	}
	if code == 404 {
		return m, ErrMissing
	}
	if code != 200 || json.Unmarshal(b, &m) != nil || m.SchemaVersion != 2 || m.MediaType != ManifestType || len(m.Layers) > 256 {
		return m, ErrUnavailable
	}
	for _, l := range m.Layers {
		if !DigestPattern.MatchString(l.Digest) || l.Size < 0 || l.Size > 4<<20 {
			return m, ErrUnavailable
		}
	}
	return m, nil
}
func (r registry) readBlob(ctx context.Context, d Descriptor) ([]byte, error) {
	b, _, code, e := r.request(ctx, "GET", r.base()+"/blobs/"+d.Digest, "", nil)
	if e != nil || code != 200 || Hash(b) != d.Digest || len(b) != d.Size {
		return nil, ErrUnavailable
	}
	return b, nil
}

// Config reproduces go-containerregistry's empty cosign image config. Layers are uncompressed.
func Config(layers []Descriptor) []byte {
	diff := []string{}
	for _, l := range layers {
		diff = append(diff, l.Digest)
	}
	b, _ := json.Marshal(struct {
		Architecture string              `json:"architecture"`
		Created      string              `json:"created"`
		History      []map[string]string `json:"history"`
		OS           string              `json:"os"`
		RootFS       struct {
			Type    string   `json:"type"`
			DiffIDs []string `json:"diff_ids"`
		} `json:"rootfs"`
		Config map[string]any `json:"config"`
	}{Created: "0001-01-01T00:00:00Z", History: []map[string]string{{"created": "0001-01-01T00:00:00Z"}}, RootFS: struct {
		Type    string   `json:"type"`
		DiffIDs []string `json:"diff_ids"`
	}{"layers", diff}, Config: map[string]any{}})
	return b
}
func (r registry) append(ctx context.Context, tag string, body []byte, media string, annotations map[string]string, k buildconfig.SigningKey) error {
	m, e := r.manifest(ctx, tag)
	if e == ErrMissing {
		m = Manifest{SchemaVersion: 2, MediaType: ManifestType, Layers: []Descriptor{}}
	} else if e != nil {
		return e
	}
	digest := Hash(body)
	for _, l := range m.Layers {
		if l.Digest == digest && l.MediaType == media {
			if media == SignatureType {
				pub, e := buildconfig.PublicKey(k.PublicKeyPEM)
				sig, decode := base64.StdEncoding.DecodeString(l.Annotations[SignatureAnnotation])
				sum := sha256.Sum256(body)
				if e != nil || decode != nil || !ecdsa.VerifyASN1(pub, sum[:], sig) {
					return ErrUnavailable
				}
			}
			return nil
		}
	}
	if len(m.Layers) >= 256 {
		return ErrUnavailable
	}
	if e = r.blob(ctx, body); e != nil {
		return e
	}
	m.Layers = append(m.Layers, Descriptor{media, digest, len(body), annotations})
	cfg := Config(m.Layers)
	if e = r.blob(ctx, cfg); e != nil {
		return e
	}
	m.Config = Descriptor{MediaType: ConfigType, Digest: Hash(cfg), Size: len(cfg)}
	b, _ := json.Marshal(m)
	_, _, code, e := r.request(ctx, "PUT", r.base()+"/manifests/"+tag, ManifestType, b)
	if e != nil || code != 201 {
		return ErrUnavailable
	}
	return nil
}
func Payload(repository, digest, buildID, sha, keyID string) []byte {
	b, _ := json.Marshal(struct {
		Critical struct {
			Identity map[string]string `json:"identity"`
			Image    map[string]string `json:"image"`
			Type     string            `json:"type"`
		} `json:"critical"`
		Optional map[string]string `json:"optional"`
	}{Critical: struct {
		Identity map[string]string `json:"identity"`
		Image    map[string]string `json:"image"`
		Type     string            `json:"type"`
	}{map[string]string{"docker-reference": repository}, map[string]string{"docker-manifest-digest": digest}, "cosign container image signature"}, Optional: map[string]string{"dev.deadnodes.rcp/build-id": buildID, "dev.deadnodes.rcp/source-sha": sha, "dev.deadnodes.rcp/key-id": keyID}})
	return b
}
func PAE(kind string, body []byte) []byte {
	return append([]byte(fmt.Sprintf("DSSEv1 %d %s %d ", len(kind), kind, len(body))), body...)
}

type Envelope struct {
	PayloadType string              `json:"payloadType"`
	Payload     string              `json:"payload"`
	Signatures  []EnvelopeSignature `json:"signatures"`
}
type EnvelopeSignature struct {
	KeyID string `json:"keyid"`
	Sig   string `json:"sig"`
}

// Predicate is the extension point for future CycloneDX statements; fetching CI SBOMs is separate.
type Predicate struct {
	Type string
	Body any
}

func Statement(repo, digest string, p Predicate) []byte {
	b, _ := json.Marshal(struct {
		Type          string `json:"_type"`
		PredicateType string `json:"predicateType"`
		Subject       []any  `json:"subject"`
		Predicate     any    `json:"predicate"`
	}{"https://in-toto.io/Statement/v0.1", p.Type, []any{map[string]any{"name": repo, "digest": map[string]string{"sha256": strings.TrimPrefix(digest, "sha256:")}}}, p.Body})
	return b
}

// Publish serializes read/append/write within this installation. Registry tags have no CAS;
// separate writers must use separate signing authority or coordinate externally.
func (c *Client) Publish(ctx context.Context, reg buildconfig.Registry, repo, credential string, k buildconfig.SigningKey, digest, buildID, sha string, p Predicate) (int, error) {
	c.writes.Lock()
	defer c.writes.Unlock()
	if !DigestPattern.MatchString(digest) {
		return 0, ErrUnavailable
	}
	r, e := c.registry(reg, repo, credential)
	if e != nil {
		return 0, e
	}
	u, _ := url.Parse(reg.Origin)
	payload := Payload(u.Host+"/"+repo, digest, buildID, sha, k.ID)
	sig, version, e := c.Sign(ctx, k, payload)
	if e != nil {
		return 0, e
	}
	if e = r.append(ctx, Tag(digest, "sig"), payload, SignatureType, map[string]string{SignatureAnnotation: base64.StdEncoding.EncodeToString(sig)}, k); e != nil {
		return version, e
	}
	statement := Statement(u.Host+"/"+repo, digest, p)
	sig, v, e := c.Sign(ctx, k, PAE(PayloadType, statement))
	if e != nil {
		return version, e
	}
	if v != version {
		return version, ErrKeyMismatch
	}
	// A stable statement and verifying key identify an attestation even with randomized ECDSA.
	m, e := r.manifest(ctx, Tag(digest, "att"))
	if e != nil && e != ErrMissing {
		return version, e
	}
	for _, l := range m.Layers {
		if l.MediaType != EnvelopeType {
			continue
		}
		b, e := r.readBlob(ctx, l)
		if e != nil {
			return version, e
		}
		var env Envelope
		if json.Unmarshal(b, &env) == nil && env.Payload == base64.StdEncoding.EncodeToString(statement) && verifyEnvelope(env, k) {
			return version, nil
		}
	}
	b, _ := json.Marshal(Envelope{PayloadType, base64.StdEncoding.EncodeToString(statement), []EnvelopeSignature{{"", base64.StdEncoding.EncodeToString(sig)}}})
	e = r.append(ctx, Tag(digest, "att"), b, EnvelopeType, map[string]string{SignatureAnnotation: "", "predicateType": p.Type}, k)
	return version, e
}
func verifyEnvelope(env Envelope, k buildconfig.SigningKey) bool {
	pub, e := buildconfig.PublicKey(k.PublicKeyPEM)
	if e != nil || env.PayloadType != PayloadType {
		return false
	}
	b, e := base64.StdEncoding.DecodeString(env.Payload)
	if e != nil {
		return false
	}
	sum := sha256.Sum256(PAE(env.PayloadType, b))
	for _, s := range env.Signatures {
		sig, e := base64.StdEncoding.DecodeString(s.Sig)
		if e == nil && ecdsa.VerifyASN1(pub, sum[:], sig) {
			return true
		}
	}
	return false
}
func (c *Client) Verify(ctx context.Context, reg buildconfig.Registry, repo, credential, digest string, k buildconfig.SigningKey) bool {
	if !DigestPattern.MatchString(digest) {
		return false
	}
	r, e := c.registry(reg, repo, credential)
	if e != nil {
		return false
	}
	m, e := r.manifest(ctx, Tag(digest, "sig"))
	if e != nil {
		return false
	}
	pub, e := buildconfig.PublicKey(k.PublicKeyPEM)
	if e != nil {
		return false
	}
	u, _ := url.Parse(reg.Origin)
	for _, l := range m.Layers {
		if l.MediaType != SignatureType {
			continue
		}
		body, e := r.readBlob(ctx, l)
		if e != nil {
			continue
		}
		var p struct {
			Critical struct {
				Identity map[string]string `json:"identity"`
				Image    map[string]string `json:"image"`
				Type     string            `json:"type"`
			} `json:"critical"`
			Optional map[string]string `json:"optional"`
		}
		if json.Unmarshal(body, &p) != nil || p.Critical.Type != "cosign container image signature" || p.Critical.Image["docker-manifest-digest"] != digest || p.Critical.Identity["docker-reference"] != u.Host+"/"+repo || p.Optional["dev.deadnodes.rcp/key-id"] != k.ID {
			continue
		}
		sig, e := base64.StdEncoding.DecodeString(l.Annotations[SignatureAnnotation])
		sum := sha256.Sum256(body)
		if e == nil && ecdsa.VerifyASN1(pub, sum[:], sig) {
			return true
		}
	}
	return false
}

// Inspect checks existence by immutable digest and reads revision evidence without trusting tags.
func (c *Client) Inspect(ctx context.Context, reg buildconfig.Registry, repo, credential, digest string) (string, error) {
	if !DigestPattern.MatchString(digest) {
		return "", ErrUnavailable
	}
	r, e := c.registry(reg, repo, credential)
	if e != nil {
		return "", e
	}
	_, h, code, e := r.request(ctx, "HEAD", r.base()+"/manifests/"+digest, "", nil)
	if e != nil || code != 200 {
		return "", ErrUnavailable
	}
	if d := h.Get("Docker-Content-Digest"); d != "" && d != digest {
		return "", ErrUnavailable
	}
	get := func(d string) ([]byte, error) {
		b, _, code, e := r.request(ctx, "GET", r.base()+"/manifests/"+d, "", nil)
		if e != nil || code != 200 || Hash(b) != d {
			return nil, ErrUnavailable
		}
		return b, nil
	}
	b, e := get(digest)
	if e != nil {
		return "", e
	}
	var m struct {
		Config    Descriptor `json:"config"`
		Manifests []struct {
			Digest   string `json:"digest"`
			Size     int    `json:"size"`
			Platform struct {
				OS   string `json:"os"`
				Arch string `json:"architecture"`
			} `json:"platform"`
		} `json:"manifests"`
	}
	if json.Unmarshal(b, &m) != nil {
		return "", ErrUnavailable
	}
	if len(m.Manifests) > 0 {
		selected := ""
		size := 0
		for _, d := range m.Manifests {
			if d.Platform.OS == "linux" && d.Platform.Arch == "amd64" {
				if selected != "" {
					return "", ErrUnavailable
				}
				selected = d.Digest
				size = d.Size
			}
		}
		if !DigestPattern.MatchString(selected) {
			return "", ErrUnavailable
		}
		b, e = get(selected)
		if e != nil || len(b) != size || json.Unmarshal(b, &m) != nil {
			return "", ErrUnavailable
		}
	}
	if !DigestPattern.MatchString(m.Config.Digest) {
		return "", ErrUnavailable
	}
	b, e = r.readBlob(ctx, m.Config)
	if e != nil {
		return "", e
	}
	var cfg struct {
		Config struct {
			Labels map[string]string `json:"Labels"`
		} `json:"config"`
	}
	if json.Unmarshal(b, &cfg) != nil {
		return "", ErrUnavailable
	}
	return cfg.Config.Labels["org.opencontainers.image.revision"], nil
}
