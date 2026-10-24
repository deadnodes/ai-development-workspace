package ocipublisher

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/ociverify"
	"strings"
	"time"
)

// ReadConfig contains only operator-owned, exact read authority. Read never sends
// a write request and never asserts the tar serialization hash of local output.
type ReadConfig struct {
	Origin, Repository, CAFile, UsernameEnv, PasswordEnv, BearerEnv string
	MaxBytes                                                        int64
	TimeoutSeconds                                                  int
	FixtureLoopbackHTTP                                             bool
}

func Read(ctx context.Context, c ReadConfig, d, platform string) (domain.NativeObservation, error) {
	var out domain.NativeObservation
	if !digest.MatchString(d) || !repository.MatchString(c.Repository) || len(c.Repository) > 255 || c.MaxBytes < 1 || c.MaxBytes > 256<<20 || c.TimeoutSeconds < 1 || c.TimeoutSeconds > 600 {
		return out, invalid
	}
	origin, e := validateRegistry(c.Origin, c.Repository, c.MaxBytes, c.TimeoutSeconds, c.UsernameEnv, c.PasswordEnv, c.BearerEnv, c.FixtureLoopbackHTTP)
	if e != nil {
		return out, e
	}
	client, e := newClient(Config{CAFile: c.CAFile})
	if e != nil {
		return out, e
	}
	defer client.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(ctx, time.Duration(c.TimeoutSeconds)*time.Second)
	defer cancel()
	r := registryClient{client: client, origin: origin, repository: c.Repository, limit: c.MaxBytes}
	if c.BearerEnv != "" {
		v := os.Getenv(c.BearerEnv)
		if v == "" || strings.ContainsAny(v, "\r\n") {
			return out, invalid
		}
		r.auth = "Bearer " + v
	}
	if c.UsernameEnv != "" {
		u, p := os.Getenv(c.UsernameEnv), os.Getenv(c.PasswordEnv)
		if u == "" || p == "" || strings.ContainsAny(u+p, "\r\n") || strings.Contains(u, ":") {
			return out, invalid
		}
		q, _ := http.NewRequest("GET", "https://unused.invalid", nil)
		q.SetBasicAuth(u, p)
		r.auth = q.Header.Get("Authorization")
	}
	fetch := func(kind, d string) ([]byte, error) {
		if !digest.MatchString(d) {
			return nil, invalid
		}
		v, e := r.request(ctx, "GET", r.path(kind, d), nil, "")
		if e != nil || v.status != 200 || hash(v.body) != d {
			return nil, unknown
		}
		return v.body, nil
	}
	manifest, e := fetch("manifests", d)
	if e != nil {
		return out, e
	}
	type descriptor struct {
		MediaType string `json:"mediaType"`
		Digest    string `json:"digest"`
		Size      int64  `json:"size"`
	}
	var m struct {
		SchemaVersion int          `json:"schemaVersion"`
		Config        descriptor   `json:"config"`
		Layers        []descriptor `json:"layers"`
	}
	if json.Unmarshal(manifest, &m) != nil || m.SchemaVersion != 2 || len(m.Layers) != 1 {
		return out, invalid
	}
	total := int64(len(manifest))
	files := map[string][]byte{"oci-layout": []byte(`{"imageLayoutVersion":"1.0.0"}`), "blobs/sha256/" + strings.TrimPrefix(d, "sha256:"): manifest}
	for _, desc := range []descriptor{m.Config, m.Layers[0]} {
		if desc.Size < 1 || desc.Size > c.MaxBytes {
			return out, invalid
		}
		b, e := fetch("blobs", desc.Digest)
		if e != nil || int64(len(b)) != desc.Size {
			return out, unknown
		}
		total += int64(len(b))
		if total > c.MaxBytes {
			return out, invalid
		}
		files["blobs/sha256/"+strings.TrimPrefix(desc.Digest, "sha256:")] = b
	}
	index, _ := json.Marshal(map[string]any{"schemaVersion": 2, "manifests": []descriptor{{manifestType, d, int64(len(manifest))}}})
	files["index.json"] = index
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	for name, b := range files {
		if e = tw.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: int64(len(b))}); e != nil {
			return out, e
		}
		if _, e = tw.Write(b); e != nil {
			return out, e
		}
	}
	if e = tw.Close(); e != nil {
		return out, e
	}
	// Tar overhead is bounded independently; receipt.ArchiveHash is deliberately
	// retained as authenticated build assertion rather than reconstructed evidence.
	if _, e = ociverify.VerifyOCIBytes(archive.Bytes(), c.MaxBytes+32<<10, platform); e != nil {
		return out, invalid
	}
	return domain.NativeObservation{ManifestDigest: d, ConfigDigest: m.Config.Digest, LayerDigest: m.Layers[0].Digest, Platform: platform, Bytes: total, ObservedAt: time.Now().UTC()}, nil
}
