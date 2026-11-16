// Package imagesigning implements pinned Vault Transit signing and classic cosign OCI artifacts.
// It contains protocol adapters only; the application owns eligibility and retry policy.
package imagesigning

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"releasecontrol/internal/buildconfig"
	"strconv"
	"strings"
	"sync"
	"time"
)

var ErrKeyMismatch = errors.New("SIGNING_KEY_MISMATCH")
var ErrUnavailable = errors.New("signing provider unavailable")
var ErrMissing = errors.New("signing artifact missing")

type lease struct {
	token   string
	expires time.Time
}
type Client struct {
	http   *http.Client
	mu     sync.Mutex
	tokens map[string]lease
	writes sync.Mutex
}

func New() *Client { return NewWithClient(http.DefaultClient) }
func NewWithClient(h *http.Client) *Client {
	c := *h
	c.Timeout = 30 * time.Second
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{http: &c, tokens: map[string]lease{}}
}
func bounded(res *http.Response) ([]byte, error) {
	defer res.Body.Close()
	b, e := io.ReadAll(io.LimitReader(res.Body, (4<<20)+1))
	if e != nil || len(b) > 4<<20 {
		return nil, ErrUnavailable
	}
	return b, nil
}
func (c *Client) vault(ctx context.Context, k buildconfig.SigningKey, method, path, token string, in, out any) error {
	var body []byte
	if in != nil {
		body, _ = json.Marshal(in)
	}
	req, e := http.NewRequestWithContext(ctx, method, strings.TrimRight(k.VaultAddr, "/")+"/v1/"+path, bytes.NewReader(body))
	if e != nil {
		return ErrUnavailable
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("X-Vault-Token", token)
	}
	res, e := c.http.Do(req)
	if e != nil {
		return ErrUnavailable
	}
	b, e := bounded(res)
	if e != nil || res.StatusCode != 200 || json.Unmarshal(b, out) != nil {
		return ErrUnavailable
	}
	return nil
}
func (c *Client) login(ctx context.Context, k buildconfig.SigningKey) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	identity := k.VaultAddr + "|" + k.Auth.Mount + "|" + k.Auth.Role + "|" + k.Auth.TokenFile
	if l := c.tokens[identity]; time.Now().Before(l.expires) {
		return l.token, nil
	}
	f, e := os.Open(k.Auth.TokenFile)
	if e != nil {
		return "", ErrUnavailable
	}
	b, e := io.ReadAll(io.LimitReader(f, 65537))
	f.Close()
	jwt := strings.TrimSpace(string(b))
	if e != nil || len(b) > 65536 || jwt == "" || strings.ContainsAny(jwt, "\r\n") {
		return "", ErrUnavailable
	}
	var out struct {
		Auth struct {
			Token string `json:"client_token"`
			Lease int    `json:"lease_duration"`
		} `json:"auth"`
	}
	e = c.vault(ctx, k, "POST", "auth/"+k.Auth.Mount+"/login", "", map[string]string{"role": k.Auth.Role, "jwt": jwt}, &out)
	if e != nil || out.Auth.Token == "" || out.Auth.Lease <= 0 || out.Auth.Lease > 31536000 {
		return "", ErrUnavailable
	}
	c.tokens[identity] = lease{out.Auth.Token, time.Now().Add(time.Duration(out.Auth.Lease) * time.Second * 4 / 5)}
	return out.Auth.Token, nil
}
func (c *Client) Sign(ctx context.Context, k buildconfig.SigningKey, body []byte) ([]byte, int, error) {
	token, e := c.login(ctx, k)
	if e != nil {
		return nil, 0, e
	}
	var keys struct {
		Data struct {
			Latest int `json:"latest_version"`
			Keys   map[string]struct {
				Public string `json:"public_key"`
			} `json:"keys"`
		} `json:"data"`
	}
	if e = c.vault(ctx, k, "GET", k.TransitMount+"/keys/"+k.KeyName, token, nil, &keys); e != nil {
		return nil, 0, e
	}
	pinned, e := buildconfig.PublicKey(k.PublicKeyPEM)
	if e != nil {
		return nil, 0, ErrKeyMismatch
	}
	latest, e := buildconfig.PublicKey(keys.Data.Keys[strconv.Itoa(keys.Data.Latest)].Public)
	if e != nil || !pinned.Equal(latest) {
		return nil, 0, ErrKeyMismatch
	}
	sum := sha256.Sum256(body)
	var out struct {
		Data struct {
			Signature string `json:"signature"`
		} `json:"data"`
	}
	e = c.vault(ctx, k, "POST", k.TransitMount+"/sign/"+k.KeyName+"/sha2-256", token, map[string]any{"input": base64.StdEncoding.EncodeToString(sum[:]), "prehashed": true, "marshaling_algorithm": "asn1"}, &out)
	if e != nil {
		return nil, 0, e
	}
	parts := strings.Split(out.Data.Signature, ":")
	if len(parts) != 3 || parts[0] != "vault" || !strings.HasPrefix(parts[1], "v") {
		return nil, 0, ErrUnavailable
	}
	version, e := strconv.Atoi(strings.TrimPrefix(parts[1], "v"))
	if e != nil || version <= 0 {
		return nil, 0, ErrUnavailable
	}
	sig, e := base64.StdEncoding.DecodeString(parts[2])
	if e != nil || !ecdsa.VerifyASN1(pinned, sum[:], sig) {
		return nil, 0, ErrKeyMismatch
	}
	return sig, version, nil
}
