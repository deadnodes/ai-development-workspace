package prodjwt

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"strings"
	"testing"
	"time"
)

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
func token(t *testing.T, alg string, priv any, claims map[string]any, kid string) string {
	t.Helper()
	h, _ := json.Marshal(map[string]any{"alg": alg, "kid": kid, "typ": "JWT"})
	p, _ := json.Marshal(claims)
	a := b64(h) + "." + b64(p)
	sum := sha256.Sum256([]byte(a))
	var sig []byte
	switch k := priv.(type) {
	case *rsa.PrivateKey:
		sig, _ = rsa.SignPKCS1v15(rand.Reader, k, cryptoHash(), sum[:])
	case *ecdsa.PrivateKey:
		r, s, _ := ecdsa.Sign(rand.Reader, k, sum[:])
		sig = make([]byte, 64)
		r.FillBytes(sig[:32])
		s.FillBytes(sig[32:])
	}
	return a + "." + b64(sig)
}
func cryptoHash() crypto.Hash { return crypto.SHA256 }

func TestPromoterTokenValidation(t *testing.T) {
	now := time.Unix(1800000000, 0)
	cfg := Config{Issuer: "https://kubernetes.default.svc.cluster.local", Audience: "rcp-prod-promoter", Subject: "system:serviceaccount:supply-chain:cosign-prod-signer", Now: func() time.Time { return now }}
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	ecKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	jwkRSA := map[string]any{"kid": "r1", "kty": "RSA", "alg": "RS256", "n": b64(rsaKey.N.Bytes()), "e": b64(big.NewInt(int64(rsaKey.E)).Bytes())}
	jwkEC := map[string]any{"kid": "e1", "kty": "EC", "alg": "ES256", "crv": "P-256", "x": b64(ecKey.X.FillBytes(make([]byte, 32))), "y": b64(ecKey.Y.FillBytes(make([]byte, 32)))}
	raw, _ := json.Marshal(map[string]any{"keys": []any{jwkRSA, jwkEC}})
	keys, e := LoadJWKS(raw)
	if e != nil {
		t.Fatal(e)
	}
	claims := func(aud any) map[string]any {
		return map[string]any{"iss": cfg.Issuer, "sub": cfg.Subject, "aud": aud, "iat": now.Unix() - 10, "nbf": now.Unix() - 10, "exp": now.Unix() + 600}
	}
	valid := token(t, "RS256", rsaKey, claims(cfg.Audience), "r1")
	if e = Verify(valid, keys, cfg); e != nil {
		t.Fatalf("valid token: %v", e)
	}
	es := token(t, "ES256", ecKey, claims([]string{"other", cfg.Audience}), "e1")
	if e = Verify(es, keys, cfg); e != nil {
		t.Fatalf("valid ES token: %v", e)
	}
	cases := []struct {
		name, alg, kid string
		mutate         func(map[string]any)
		rawHeader      bool
	}{
		{"audience", "RS256", "r1", func(c map[string]any) { c["aud"] = "wrong" }, false},
		{"issuer", "RS256", "r1", func(c map[string]any) { c["iss"] = "wrong" }, false},
		{"subject", "RS256", "r1", func(c map[string]any) { c["sub"] = "wrong" }, false},
		{"expired", "RS256", "r1", func(c map[string]any) { c["exp"] = now.Unix() - 61 }, false},
		{"unknown kid", "RS256", "unknown", nil, false},
		{"lifetime", "RS256", "r1", func(c map[string]any) { c["exp"] = now.Unix() + 3700; c["iat"] = now.Unix() }, false},
		{"nbf future", "RS256", "r1", func(c map[string]any) { c["nbf"] = now.Unix() + 61 }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := claims(cfg.Audience)
			if tc.mutate != nil {
				tc.mutate(c)
			}
			v := token(t, tc.alg, rsaKey, c, tc.kid)
			if Verify(v, keys, cfg) == nil {
				t.Fatal("accepted invalid token")
			}
		})
	}
	for _, alg := range []string{"none", "HS256"} {
		t.Run(alg, func(t *testing.T) {
			h, _ := json.Marshal(map[string]string{"alg": alg, "kid": "r1"})
			p, _ := json.Marshal(claims(cfg.Audience))
			v := b64(h) + "." + b64(p) + "." + b64([]byte(strings.Repeat("x", 64)))
			if Verify(v, keys, cfg) == nil {
				t.Fatal("accepted forbidden algorithm")
			}
		})
	}
}
