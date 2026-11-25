package prodjwt

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"strings"
	"time"
)

var ErrToken = errors.New("invalid promoter token")

type Config struct {
	Issuer, Audience, Subject string
	Now                       func() time.Time
}
type key struct {
	alg string
	rsa *rsa.PublicKey
	ec  *ecdsa.PublicKey
}
type Keys map[string]key

func LoadJWKS(raw []byte) (Keys, error) {
	var doc struct {
		Keys []map[string]json.RawMessage `json:"keys"`
	}
	if len(raw) == 0 || len(raw) > 1<<20 || json.Unmarshal(raw, &doc) != nil || len(doc.Keys) == 0 || len(doc.Keys) > 64 {
		return nil, ErrToken
	}
	out := Keys{}
	seen := map[string]bool{}
	for _, item := range doc.Keys {
		var kid, kty, alg string
		_ = json.Unmarshal(item["kid"], &kid)
		_ = json.Unmarshal(item["kty"], &kty)
		_ = json.Unmarshal(item["alg"], &alg)
		if kid == "" || seen[kid] {
			return nil, ErrToken
		}
		seen[kid] = true
		for _, privateField := range []string{"d", "p", "q", "dp", "dq", "qi", "oth", "k"} {
			if _, exists := item[privateField]; exists {
				return nil, ErrToken
			}
		}
		var use string
		_ = json.Unmarshal(item["use"], &use)
		if use != "" && use != "sig" {
			continue
		}
		if raw, ok := item["key_ops"]; ok {
			var ops []string
			if json.Unmarshal(raw, &ops) != nil || len(ops) == 0 {
				continue
			}
			verify := false
			for _, op := range ops {
				verify = verify || op == "verify"
			}
			if !verify {
				continue
			}
		}
		if kty == "RSA" && (alg == "" || alg == "RS256") {
			var n, e string
			_ = json.Unmarshal(item["n"], &n)
			_ = json.Unmarshal(item["e"], &e)
			nb, ne := base64.RawURLEncoding.DecodeString(n)
			eb, ee := base64.RawURLEncoding.DecodeString(e)
			if ne != nil || ee != nil || len(nb) < 256 || len(eb) == 0 {
				continue
			}
			exponent := new(big.Int).SetBytes(eb)
			if !exponent.IsInt64() || exponent.Int64() < 3 || exponent.Int64() > 1<<31 {
				continue
			}
			out[kid] = key{alg: "RS256", rsa: &rsa.PublicKey{N: new(big.Int).SetBytes(nb), E: int(exponent.Int64())}}
		} else if kty == "EC" && (alg == "" || alg == "ES256") {
			var crv, x, y string
			_ = json.Unmarshal(item["crv"], &crv)
			_ = json.Unmarshal(item["x"], &x)
			_ = json.Unmarshal(item["y"], &y)
			xb, xe := base64.RawURLEncoding.DecodeString(x)
			yb, ye := base64.RawURLEncoding.DecodeString(y)
			if crv != "P-256" || xe != nil || ye != nil || len(xb) != 32 || len(yb) != 32 {
				continue
			}
			pub := &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(xb), Y: new(big.Int).SetBytes(yb)}
			if !pub.Curve.IsOnCurve(pub.X, pub.Y) {
				continue
			}
			out[kid] = key{alg: "ES256", ec: pub}
		}
	}
	if len(out) == 0 {
		return nil, ErrToken
	}
	return out, nil
}

func Verify(token string, keys Keys, cfg Config) error {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || len(token) > 16384 {
		return ErrToken
	}
	decode := func(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }
	hb, e := decode(parts[0])
	if e != nil {
		return ErrToken
	}
	var h struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
		Typ string `json:"typ"`
	}
	if json.Unmarshal(hb, &h) != nil || h.Kid == "" || (h.Alg != "RS256" && h.Alg != "ES256") {
		return ErrToken
	}
	k, ok := keys[h.Kid]
	if !ok || k.alg != h.Alg {
		return ErrToken
	}
	sig, e := decode(parts[2])
	if e != nil {
		return ErrToken
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if h.Alg == "RS256" {
		if len(sig) != k.rsa.Size() || rsa.VerifyPKCS1v15(k.rsa, crypto.SHA256, sum[:], sig) != nil {
			return ErrToken
		}
	} else {
		if len(sig) != 64 || !ecdsa.Verify(k.ec, sum[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
			return ErrToken
		}
	}
	cb, e := decode(parts[1])
	if e != nil {
		return ErrToken
	}
	var c struct {
		Iss string          `json:"iss"`
		Sub string          `json:"sub"`
		Aud json.RawMessage `json:"aud"`
		Exp json.Number     `json:"exp"`
		Iat json.Number     `json:"iat"`
		Nbf json.Number     `json:"nbf"`
	}
	d := json.NewDecoder(strings.NewReader(string(cb)))
	d.UseNumber()
	if d.Decode(&c) != nil || c.Iss != cfg.Issuer || c.Sub != cfg.Subject {
		return ErrToken
	}
	var aud string
	if json.Unmarshal(c.Aud, &aud) == nil {
		if aud != cfg.Audience {
			return ErrToken
		}
	} else {
		var list []string
		if json.Unmarshal(c.Aud, &list) != nil {
			return ErrToken
		}
		found := false
		for _, v := range list {
			found = found || v == cfg.Audience
		}
		if !found {
			return ErrToken
		}
	}
	parse := func(n json.Number) (int64, bool) {
		if n == "" {
			return 0, false
		}
		v, err := n.Int64()
		return v, err == nil
	}
	exp, okExp := parse(c.Exp)
	iat, okIat := parse(c.Iat)
	if !okExp || !okIat {
		return ErrToken
	}
	nbf, okNbf := parse(c.Nbf)
	now := time.Now()
	if cfg.Now != nil {
		now = cfg.Now()
	}
	unix := now.Unix()
	if exp < unix-60 || iat > unix+60 || exp <= iat || exp-iat > 3600 || okNbf && (nbf > unix+60 || nbf > exp) || !okNbf && c.Nbf != "" {
		return ErrToken
	}
	return nil
}
