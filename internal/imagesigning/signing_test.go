package imagesigning_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"releasecontrol/internal/buildconfig"
	s "releasecontrol/internal/imagesigning"
	"releasecontrol/internal/imagesigning/signingtest"
	"strings"
	"testing"
)

func TestGoldenPayloadAndPAE(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	want := `{"critical":{"identity":{"docker-reference":"registry.example/o/r"},"image":{"docker-manifest-digest":"` + digest + `"},"type":"cosign container image signature"},"optional":{"dev.deadnodes.rcp/build-id":"build","dev.deadnodes.rcp/key-id":"dev","dev.deadnodes.rcp/source-sha":"abc"}}`
	if string(s.Payload("registry.example/o/r", digest, "build", "abc", "dev")) != want {
		t.Fatal(string(s.Payload("registry.example/o/r", digest, "build", "abc", "dev")))
	}
	if string(s.PAE("type", []byte("hé"))) != "DSSEv1 4 type 3 hé" {
		t.Fatal("PAE must count bytes")
	}
}
func TestClassicCosignTwoKeysAndIndependentVerification(t *testing.T) {
	f := signingtest.New(t)
	f.Bearer = true
	c := s.NewWithClient(f.Client())
	ctx := context.Background()
	digest := f.Image("abc")
	predicate := s.Predicate{Type: s.ProvenanceType, Body: map[string]any{"buildDefinition": map[string]string{"buildType": "https://deadnodes.dev/rcp/forgejo-build/v1"}}}
	for _, id := range []string{"dev", "prod", "dev", "prod"} {
		v, e := c.Publish(ctx, f.RegistryConfig(), "o/r", "registry.json", f.Key(id), digest, "build", "abc", predicate)
		if e != nil || v != 1 {
			t.Fatal(v, e)
		}
	}
	f.Mu.Lock()
	defer f.Mu.Unlock()
	if f.Logins != 1 {
		t.Fatalf("login lease not shared: %d", f.Logins)
	}
	for _, suffix := range []string{"sig", "att"} {
		var m s.Manifest
		if json.Unmarshal(f.Manifests[s.Tag(digest, suffix)], &m) != nil || len(m.Layers) != 2 || m.MediaType != s.ManifestType || m.Config.MediaType != s.ConfigType {
			t.Fatalf("bad manifest: %+v", m)
		}
		if string(f.Blobs[m.Config.Digest]) != string(s.Config(m.Layers)) {
			t.Fatal("config layout mismatch")
		}
		var cfg struct {
			RootFS struct {
				DiffIDs []string `json:"diff_ids"`
			} `json:"rootfs"`
		}
		_ = json.Unmarshal(f.Blobs[m.Config.Digest], &cfg)
		for i, l := range m.Layers {
			body := f.Blobs[l.Digest]
			if s.Hash(body) != l.Digest || len(body) != l.Size || cfg.RootFS.DiffIDs[i] != l.Digest {
				t.Fatal("blob identity mismatch")
			}
			sig := l.Annotations[s.SignatureAnnotation]
			signed := body
			if suffix == "att" {
				if l.MediaType != s.EnvelopeType || sig != "" || l.Annotations["predicateType"] != s.ProvenanceType {
					t.Fatal(l)
				}
				var env s.Envelope
				_ = json.Unmarshal(body, &env)
				statement, e := base64.StdEncoding.DecodeString(env.Payload)
				if e != nil || env.PayloadType != s.PayloadType || env.Signatures[0].KeyID != "" {
					t.Fatal(env)
				}
				signed = append([]byte(fmt.Sprintf("DSSEv1 %d %s %d ", len(env.PayloadType), env.PayloadType, len(statement))), statement...)
				sig = env.Signatures[0].Sig
				var v map[string]any
				_ = json.Unmarshal(statement, &v)
				if v["_type"] != "https://in-toto.io/Statement/v0.1" || v["predicateType"] != s.ProvenanceType {
					t.Fatal(v)
				}
				subject := v["subject"].([]any)[0].(map[string]any)
				if subject["digest"].(map[string]any)["sha256"] != strings.TrimPrefix(digest, "sha256:") {
					t.Fatal(subject)
				}
			} else {
				if l.MediaType != s.SignatureType {
					t.Fatal(l)
				}
				var p struct {
					Critical struct {
						Image map[string]string `json:"image"`
					} `json:"critical"`
				}
				_ = json.Unmarshal(body, &p)
				if p.Critical.Image["docker-manifest-digest"] != digest {
					t.Fatal(p)
				}
			}
			keyID := []string{"dev", "prod"}[i]
			pub, e := buildconfig.PublicKey(f.Key(keyID).PublicKeyPEM)
			if e != nil {
				t.Fatal(e)
			}
			der, e := base64.StdEncoding.DecodeString(sig)
			hash := sha256.Sum256(signed)
			if e != nil || !ecdsa.VerifyASN1(pub, hash[:], der) {
				t.Fatal("independent cosign/DSSE verification failed")
			}
		}
	}
}
func TestKeyMismatchNeverPushes(t *testing.T) {
	f := signingtest.New(t)
	f.Mismatch = true
	c := s.NewWithClient(f.Client())
	_, e := c.Publish(context.Background(), f.RegistryConfig(), "o/r", "registry.json", f.Key("dev"), "sha256:"+strings.Repeat("a", 64), "b", "sha", s.Predicate{Type: s.ProvenanceType})
	if !errors.Is(e, s.ErrKeyMismatch) || f.Writes != 0 {
		t.Fatal(e, f.Writes)
	}
}
func TestVerifyAndTamper(t *testing.T) {
	f := signingtest.New(t)
	c := s.NewWithClient(f.Client())
	ctx := context.Background()
	digest := f.Image("sha")
	_, e := c.Publish(ctx, f.RegistryConfig(), "o/r", "registry.json", f.Key("dev"), digest, "b", "sha", s.Predicate{Type: s.ProvenanceType})
	if e != nil {
		t.Fatal(e)
	}
	if !c.Verify(ctx, f.RegistryConfig(), "o/r", "registry.json", digest, f.Key("dev")) || c.Verify(ctx, f.RegistryConfig(), "o/r", "registry.json", digest, f.Key("prod")) {
		t.Fatal("verification key binding")
	}
	f.Mu.Lock()
	var m s.Manifest
	_ = json.Unmarshal(f.Manifests[s.Tag(digest, "sig")], &m)
	f.Blobs[m.Layers[0].Digest] = []byte("tampered")
	f.Mu.Unlock()
	if c.Verify(ctx, f.RegistryConfig(), "o/r", "registry.json", digest, f.Key("dev")) {
		t.Fatal("tampering accepted")
	}
}

func TestForeignLayersPreservedAndRedactedErrors(t *testing.T) {
	f := signingtest.New(t)
	c := s.NewWithClient(f.Client())
	ctx := context.Background()
	digest := f.Image("sha")
	foreign := []byte("foreign payload")
	fd := s.Hash(foreign)
	f.Blobs[fd] = foreign
	layer := s.Descriptor{MediaType: "application/example", Digest: fd, Size: len(foreign), Annotations: map[string]string{"foreign": "keep"}}
	cfg := s.Config([]s.Descriptor{layer})
	f.Blobs[s.Hash(cfg)] = cfg
	m := s.Manifest{SchemaVersion: 2, MediaType: s.ManifestType, Config: s.Descriptor{MediaType: s.ConfigType, Digest: s.Hash(cfg), Size: len(cfg)}, Layers: []s.Descriptor{layer}}
	raw, _ := json.Marshal(m)
	f.Manifests[s.Tag(digest, "sig")] = raw
	if _, e := c.Publish(ctx, f.RegistryConfig(), "o/r", "registry.json", f.Key("dev"), digest, "b", "sha", s.Predicate{Type: s.ProvenanceType}); e != nil {
		t.Fatal(e)
	}
	_ = json.Unmarshal(f.Manifests[s.Tag(digest, "sig")], &m)
	if len(m.Layers) != 2 || m.Layers[0].Digest != fd || m.Layers[0].Annotations["foreign"] != "keep" {
		t.Fatal(m)
	}
	f.Unavailable = true
	_, _, e := c.Sign(ctx, f.Key("prod"), []byte("payload"))
	if e == nil || strings.Contains(e.Error(), "secret") || strings.Contains(e.Error(), "private") {
		t.Fatal(e)
	}
}
