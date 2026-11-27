package transport_test

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"releasecontrol/internal/application"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/imagesigning"
	"releasecontrol/internal/imagesigning/signingtest"
	"releasecontrol/internal/persistence"
	"releasecontrol/internal/prodjwt"
	"releasecontrol/internal/transport"
)

func jwtPart(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
func promoterJWT(t *testing.T, key *rsa.PrivateKey, aud string) string {
	t.Helper()
	now := time.Now().Unix()
	h, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "prod-key", "typ": "JWT"})
	p, _ := json.Marshal(map[string]any{"iss": "https://prod.issuer", "sub": "system:serviceaccount:supply-chain:cosign-prod-signer", "aud": aud, "iat": now, "nbf": now, "exp": now + 600})
	body := jwtPart(h) + "." + jwtPart(p)
	sum := sha256.Sum256([]byte(body))
	sig, e := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if e != nil {
		t.Fatal(e)
	}
	return body + "." + jwtPart(sig)
}

func TestProdPromoterJWTRestrictionAndPublicRead(t *testing.T) {
	key, e := rsa.GenerateKey(rand.Reader, 2048)
	if e != nil {
		t.Fatal(e)
	}
	jwk, _ := json.Marshal(map[string]any{"keys": []any{map[string]any{"kid": "prod-key", "kty": "RSA", "alg": "RS256", "n": jwtPart(key.N.Bytes()), "e": jwtPart(big.NewInt(int64(key.E)).Bytes())}}})
	keys, e := prodjwt.LoadJWKS(jwk)
	if e != nil {
		t.Fatal(e)
	}
	store, e := persistence.OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	svc := application.New(store)
	if _, err := svc.Execute(context.Background(), domain.Command{Action: "create_product", ID: "p", Actor: "test", Data: map[string]any{"name": "Shared"}}); err != nil {
		t.Fatal(err)
	}
	h := transport.New(svc, transport.Options{Token: "control", ProdPromoterKeys: keys, ProdPromoterIssuer: "https://prod.issuer", ProdPromoterAudience: "rcp-prod-promoter", ProdPromoterSubject: "system:serviceaccount:supply-chain:cosign-prod-signer", ProdPromoterScope: "p:prod"})
	call := func(method, path, token string, body []byte) int {
		r := httptest.NewRequest(method, "http://localhost"+path, bytes.NewReader(body))
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		if len(body) > 0 {
			r.Header.Set("Content-Type", "application/json")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	valid := promoterJWT(t, key, "rcp-prod-promoter")
	if got := call(http.MethodPost, "/api/prod-promotions/claim", valid, []byte(`{"wait":0}`)); got != http.StatusNoContent {
		t.Fatalf("claim status %d", got)
	}
	if got := call(http.MethodPost, "/api/prod-promotions/missing/heartbeat", valid, []byte(`{"claim_id":"claim"}`)); got != http.StatusNotFound {
		t.Fatalf("heartbeat must be allowed through promoter restriction, got %d", got)
	}
	if got := call(http.MethodGet, "/api/state", valid, nil); got != http.StatusForbidden {
		t.Fatalf("promoter state status %d", got)
	}
	if got := call(http.MethodGet, "/healthz", valid, nil); got != http.StatusForbidden {
		t.Fatalf("promoter health status %d", got)
	}
	if got := call(http.MethodPost, "/api/commands", valid, []byte(`{}`)); got != http.StatusForbidden {
		t.Fatalf("promoter command status %d", got)
	}
	if got := call(http.MethodGet, "/api/prod-promotions?product_id=p", valid, nil); got != http.StatusForbidden {
		t.Fatalf("promoter read status %d", got)
	}
	if got := call(http.MethodGet, "/api/public/prod-signatures/sha256:"+strings.Repeat("a", 64), "", nil); got != http.StatusNotFound {
		t.Fatalf("public signature status %d", got)
	}
	if got := call(http.MethodGet, "/api/state", "control", nil); got != http.StatusOK {
		t.Fatalf("normal auth fallback status %d", got)
	}
	if got := call(http.MethodPost, "/api/prod-promotions/claim", promoterJWT(t, key, "wrong-audience"), []byte(`{"wait":0}`)); got != http.StatusUnauthorized {
		t.Fatalf("bad promoter token must fall through to normal auth, got %d", got)
	}
}

func TestProdPromoterClaimOutlivesHTTPWriteTimeout(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	jwk, _ := json.Marshal(map[string]any{"keys": []any{map[string]any{"kid": "prod-key", "kty": "RSA", "alg": "RS256", "n": jwtPart(key.N.Bytes()), "e": jwtPart(big.NewInt(int64(key.E)).Bytes())}}})
	keys, err := prodjwt.LoadJWKS(jwk)
	if err != nil {
		t.Fatal(err)
	}
	store, err := persistence.OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	svc := application.New(store)
	if _, err := svc.Execute(context.Background(), domain.Command{Action: "create_product", ID: "p", Actor: "test", Data: map[string]any{"name": "Shared"}}); err != nil {
		t.Fatal(err)
	}
	handler := transport.New(svc, transport.Options{Token: "control", ProdPromoterKeys: keys, ProdPromoterIssuer: "https://prod.issuer", ProdPromoterAudience: "rcp-prod-promoter", ProdPromoterSubject: "system:serviceaccount:supply-chain:cosign-prod-signer", ProdPromoterScope: "p:prod"})
	server := httptest.NewUnstartedServer(handler)
	server.Config.WriteTimeout = 100 * time.Millisecond
	server.Start()
	defer server.Close()
	request, err := http.NewRequest(http.MethodPost, server.URL+"/api/prod-promotions/claim", strings.NewReader(`{"wait":1}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+promoterJWT(t, key, "rcp-prod-promoter"))
	request.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 5 * time.Second}
	started := time.Now()
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent || time.Since(started) < 900*time.Millisecond {
		t.Fatalf("long poll response status=%d elapsed=%s", response.StatusCode, time.Since(started))
	}
}

func TestPublicProdSignatureEndpointReturnsOnlyVerifiedData(t *testing.T) {
	store, e := persistence.OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	svc := application.New(store)
	f := signingtest.New(t)
	key := f.Key("prod")
	if e = svc.ConfigureProdPublicKey(key.PublicKeyPEM); e != nil {
		t.Fatal(e)
	}
	digest := "sha256:" + strings.Repeat("a", 64)
	ref := "registry.deadnodes.internal/deadnodes/app:main-abc@" + digest
	identity, ok := imagesigning.RepositoryIdentity(ref)
	if !ok {
		t.Fatal("invalid production image ref")
	}
	payload, _ := json.Marshal(map[string]any{"critical": map[string]any{"type": "cosign container image signature", "identity": map[string]string{"docker-reference": identity}, "image": map[string]string{"docker-manifest-digest": digest}}, "optional": map[string]any{}})
	sum := sha256.Sum256(payload)
	sig, e := ecdsa.SignASN1(rand.Reader, f.Keys["prod"], sum[:])
	if e != nil {
		t.Fatal(e)
	}
	now := time.Now().UTC()
	promotion := domain.ProdPromotion{Meta: domain.Meta{ID: "promotion-1", ProductID: "p", CreatedAt: now, UpdatedAt: now}, EnvironmentID: "prod", Status: "SUCCEEDED", OwnerApproval: "private approval", FinishedAt: &now, Items: []domain.ProdPromotionItem{{BuildID: "b", ImageID: "app", Digest: digest, ProdImageRef: ref, Result: "SIGNED", ProdSignature: &domain.ProdSignature{SignatureTag: imagesigning.Tag(digest, "sig"), PayloadB64: base64.StdEncoding.EncodeToString(payload), SignatureB64: base64.StdEncoding.EncodeToString(sig), KeyFingerprint: key.Fingerprint()}}}}
	if e = store.Update(context.Background(), func(st *domain.State) error { st.ProdPromotions = append(st.ProdPromotions, promotion); return nil }); e != nil {
		t.Fatal(e)
	}
	handler := transport.New(svc, transport.Options{Token: "must-not-be-required"})
	req := httptest.NewRequest(http.MethodGet, "http://localhost/api/public/prod-signatures/"+digest, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("public signature Cache-Control = %q, want no-store", got)
	}
	if strings.Contains(rec.Body.String(), "owner_approval") || !strings.Contains(rec.Body.String(), "signature_b64") || !strings.Contains(rec.Body.String(), "promotion-1") {
		t.Fatal(rec.Body.String())
	}
}
