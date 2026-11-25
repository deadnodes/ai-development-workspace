package prodsigner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"releasecontrol/internal/buildconfig"
	"releasecontrol/internal/imagesigning"
)

type PromotionItem struct {
	BuildID       string              `json:"build_id"`
	ImageID       string              `json:"image_id"`
	Repository    string              `json:"repository"`
	DevImageRef   string              `json:"dev_image_ref"`
	Digest        string              `json:"digest"`
	ProdImageRef  string              `json:"prod_image_ref"`
	Tag           string              `json:"tag"`
	SourceSHA     string              `json:"source_sha"`
	Result        string              `json:"result"`
	Reason        string              `json:"reason,omitempty"`
	ProdSignature *PromotionSignature `json:"prod_signature,omitempty"`
}
type PromotionSignature struct {
	SignatureTag   string `json:"signature_tag"`
	PayloadB64     string `json:"payload_b64"`
	SignatureB64   string `json:"signature_b64"`
	KeyFingerprint string `json:"key_fingerprint"`
}
type Promotion struct {
	ID            string          `json:"id"`
	ProductID     string          `json:"product_id"`
	EnvironmentID string          `json:"environment_id"`
	Status        string          `json:"status"`
	Items         []PromotionItem `json:"items"`
	OwnerApproval string          `json:"owner_approval"`
	ClaimID       string          `json:"claim_id"`
}
type promoterClient struct {
	cfg  Config
	http *http.Client
}

var errTokenUnavailable = errors.New("projected RCP token unavailable")

type promotionFailure struct {
	event  string
	status int
	class  string
	count  int
	last   time.Time
}

func (f *promotionFailure) failed(w io.Writer, event string, status int, class string, backoff time.Duration) {
	if w == nil {
		return
	}
	if f.event != "" && (f.event != event || f.status != status || f.class != class) {
		f.summary(w)
	}
	if f.event == event && f.status == status && f.class == class && time.Since(f.last) < 5*time.Minute {
		f.count++
		return
	}
	f.event, f.status, f.class, f.last = event, status, class, time.Now()
	writeJSONLine(w, map[string]any{"event": event, "status": status, "error": class, "backoff_seconds": int(backoff.Seconds())})
}

func (f *promotionFailure) summary(w io.Writer) {
	if w != nil && f.count > 0 {
		writeJSONLine(w, map[string]any{"event": "failure_summary", "failure_event": f.event, "status": f.status, "error": f.class, "suppressed_count": f.count})
	}
	f.count = 0
}

func writeJSONLine(w io.Writer, v any) {
	b, _ := json.Marshal(v)
	_, _ = fmt.Fprintln(w, string(b))
}

func failureClass(err error, status int) string {
	if errors.Is(err, errTokenUnavailable) {
		return "token_unavailable"
	}
	if status == http.StatusUnauthorized {
		return "unauthorized"
	}
	if status == http.StatusForbidden {
		return "forbidden"
	}
	if status >= 300 {
		return fmt.Sprintf("http_%d", status)
	}
	if status == 0 {
		return "network"
	}
	return "invalid_response"
}

func newPromoterClient(c Config) *promoterClient {
	return &promoterClient{cfg: c, http: &http.Client{Timeout: 65 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}
func (p *promoterClient) request(ctx context.Context, method, path string, body any) ([]byte, int, error) {
	token, e := os.ReadFile(p.cfg.RCP.TokenFile)
	if e != nil {
		return nil, 0, errTokenUnavailable
	}
	tokenText := strings.TrimSpace(string(token))
	if tokenText == "" || len(tokenText) > 16<<10 {
		return nil, 0, errTokenUnavailable
	}
	var reader io.Reader
	if body != nil {
		b, e := json.Marshal(body)
		if e != nil {
			return nil, 0, e
		}
		reader = bytes.NewReader(b)
	}
	u, e := url.Parse(strings.TrimRight(p.cfg.RCP.URL, "/") + path)
	if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
		return nil, 0, errors.New("invalid RCP endpoint")
	}
	req, e := http.NewRequestWithContext(ctx, method, u.String(), reader)
	if e != nil {
		return nil, 0, e
	}
	req.Header.Set("Authorization", "Bearer "+tokenText)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, e := p.http.Do(req)
	if e != nil {
		return nil, 0, e
	}
	defer res.Body.Close()
	b, e := io.ReadAll(io.LimitReader(res.Body, (1<<20)+1))
	if e != nil || len(b) > 1<<20 {
		return nil, res.StatusCode, errors.New("RCP response exceeds bounds")
	}
	if res.StatusCode >= 300 && res.StatusCode != http.StatusNoContent {
		return b, res.StatusCode, fmt.Errorf("RCP request returned %d", res.StatusCode)
	}
	return b, res.StatusCode, nil
}
func (p *promoterClient) claim(ctx context.Context) (*Promotion, int, error) {
	b, status, e := p.request(ctx, http.MethodPost, "/api/prod-promotions/claim", map[string]int{"wait": 50})
	if e != nil {
		return nil, status, e
	}
	if status == http.StatusNoContent {
		return nil, status, nil
	}
	var v Promotion
	if json.Unmarshal(b, &v) != nil || v.ID == "" || v.ClaimID == "" || v.Status != "CLAIMED" || v.ProductID != p.cfg.RCP.ProductID || v.EnvironmentID != p.cfg.RCP.EnvironmentID || len(v.Items) < 1 || len(v.Items) > 20 {
		return nil, status, errors.New("invalid promotion claim response")
	}
	return &v, status, nil
}
func (p *promoterClient) result(ctx context.Context, id string, claim string, items []PromotionItem) (int, error) {
	b, _ := json.Marshal(map[string]any{"claim_id": claim, "items": items})
	_, status, e := p.request(ctx, http.MethodPost, "/api/prod-promotions/"+url.PathEscape(id)+"/result", json.RawMessage(b))
	return status, e
}

func Promote(ctx context.Context, c Config, client *imagesigning.Client, log io.Writer) error {
	return promoteWithHTTPClient(ctx, c, client, log, http.DefaultClient)
}
func promoteWithHTTPClient(ctx context.Context, c Config, client *imagesigning.Client, log io.Writer, rcpHTTP *http.Client) error {
	if e := c.ValidateMode("promote"); e != nil {
		return e
	}
	promoter := newPromoterClient(c)
	promoter.http = rcpHTTP
	reg := buildconfig.Registry{Origin: c.Registry.Origin, InsecureHTTP: c.Registry.InsecureHTTP}
	credential := filepath.Base(c.Registry.CredentialsFile)
	backoff := 5 * time.Second
	var claimFailure, resultFailure promotionFailure
	if log != nil {
		writeJSONLine(log, map[string]any{"event": "promoter_started", "rcp_url": c.RCP.URL, "product_id": c.RCP.ProductID, "environment_id": c.RCP.EnvironmentID})
	}
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		promotion, status, e := promoter.claim(ctx)
		if e != nil {
			claimFailure.failed(log, "claim_failed", status, failureClass(e, status), backoff)
			if !sleepContext(ctx, backoff) {
				claimFailure.summary(log)
				return ctx.Err()
			}
			backoff = min(backoff*2, 5*time.Minute)
			continue
		}
		if claimFailure.event != "" {
			claimFailure.summary(log)
			if log != nil {
				writeJSONLine(log, map[string]any{"event": "claim_recovered"})
			}
			claimFailure = promotionFailure{}
		}
		backoff = 5 * time.Second
		if promotion == nil {
			continue
		}
		results := make([]PromotionItem, len(promotion.Items))
		for i, item := range promotion.Items {
			results[i] = processPromotionItem(ctx, c, client, reg, credential, *promotion, item)
		}
		line, _ := json.Marshal(map[string]any{"promotion_id": promotion.ID, "product_id": promotion.ProductID, "items": summarize(results)})
		if log != nil {
			fmt.Fprintln(log, string(line))
		}
		for {
			status, e = promoter.result(ctx, promotion.ID, promotion.ClaimID, results)
			if e == nil {
				resultFailure.summary(log)
				resultFailure = promotionFailure{}
				break
			}
			resultFailure.failed(log, "result_failed", status, failureClass(e, status), backoff)
			if !sleepContext(ctx, backoff) {
				resultFailure.summary(log)
				return ctx.Err()
			}
			backoff = min(backoff*2, 5*time.Minute)
		}
		backoff = 5 * time.Second
	}
}
func sleepContext(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
func summarize(items []PromotionItem) []map[string]string {
	o := make([]map[string]string, 0, len(items))
	for _, i := range items {
		o = append(o, map[string]string{"build_id": i.BuildID, "image_id": i.ImageID, "result": i.Result, "reason": i.Reason})
	}
	return o
}
func processPromotionItem(ctx context.Context, c Config, client *imagesigning.Client, reg buildconfig.Registry, credential string, p Promotion, item PromotionItem) PromotionItem {
	reject := func(code string) PromotionItem { item.Result = "REJECTED"; item.Reason = code; return item }
	infra := func() PromotionItem { item.Result = "ERROR"; item.Reason = "INFRASTRUCTURE_ERROR"; return item }
	if !imagesigning.DigestPattern.MatchString(item.Digest) || !strings.HasPrefix(item.ProdImageRef, "registry.deadnodes.internal/deadnodes/") || !matchesPrefix(item.ProdImageRef, c.OwnImagePrefixes) {
		return reject("INVALID_IMAGE")
	}
	prodName, prodDigest, ok := strings.Cut(item.ProdImageRef, "@")
	if !ok || prodDigest != item.Digest {
		return reject("INVALID_IMAGE")
	}
	colon := strings.LastIndex(prodName, ":")
	if colon < strings.LastIndex(prodName, "/") || prodName[colon+1:] != item.Tag || !strings.HasPrefix(item.Tag, "main-") || len(item.Tag) > 128 || strings.ContainsAny(item.Tag, "/:@?#") {
		return reject("INVALID_IMAGE")
	}
	devName, devDigest, ok := strings.Cut(item.DevImageRef, "@")
	if !ok || devDigest != item.Digest {
		return reject("INVALID_IMAGE")
	}
	di := strings.LastIndex(devName, ":")
	if di < strings.LastIndex(devName, "/") || devName[di+1:] != item.Tag {
		return reject("INVALID_IMAGE")
	}
	devRepo := devName[:di]
	if item.Repository != "" && item.Repository != devRepo {
		return reject("INVALID_IMAGE")
	}
	repo := strings.TrimPrefix(prodName, "registry.deadnodes.internal/")
	registryTag := strings.LastIndex(repo, ":")
	if registryTag <= strings.LastIndex(repo, "/") {
		return reject("INVALID_IMAGE")
	}
	repo = repo[:registryTag]
	if strings.Contains(repo, "..") {
		return reject("INVALID_IMAGE")
	}
	// On-demand zot pulls are bounded to five minutes and every attempt checks the immutable digest.
	warmCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	deadline := time.Now().Add(5 * time.Minute)
	var warmErr error
	for {
		warmErr = client.Warm(warmCtx, reg, repo, credential, item.Digest, item.Tag)
		if warmErr == nil {
			break
		}
		if !time.Now().Before(deadline) {
			return infra()
		}
		if !sleepContext(warmCtx, 5*time.Second) {
			return infra()
		}
	}
	// Idempotent retry: reuse an existing signature only after verifying its payload/key/tag.
	bundles, e := client.SignatureBundles(ctx, reg, c.SignaturesRepository, credential, item.Digest, c.ProdKey.PublicKeyPEM)
	if e != nil {
		return infra()
	}
	for _, b := range bundles {
		if signaturePayloadMatches(b.Payload, item) {
			item.Result = "ALREADY_SIGNED"
			item.ProdSignature = &PromotionSignature{imagesigning.Tag(item.Digest, "sig"), base64.StdEncoding.EncodeToString(b.Payload), base64.StdEncoding.EncodeToString(b.Signature), c.ProdKey.Fingerprint()}
			return item
		}
	}
	sourceRepo := rewriteRepository(c, item.Repository)
	buildID, sha, reason, e := VerifyDevEvidence(ctx, c, client, sourceRepo, item.Repository, item.Digest, item.BuildID, item.SourceSHA)
	if e != nil {
		return infra()
	}
	if reason != "" {
		return reject(reason)
	}
	ownerHash := sha256.Sum256([]byte(p.OwnerApproval))
	predicate := map[string]any{"promotion_id": p.ID, "owner_approval_sha256": hex.EncodeToString(ownerHash[:]), "build_id": item.BuildID, "source_sha": item.SourceSHA, "prod_image_ref": item.ProdImageRef}
	identity, ok := imagesigning.RepositoryIdentity(item.ProdImageRef)
	if !ok {
		return reject("INVALID_IMAGE")
	}
	_, e = client.PublishIdentity(ctx, reg, c.SignaturesRepository, credential, c.ProdKey, item.Digest, buildID, sha, identity, imagesigning.Predicate{Type: ApprovalType, Body: predicate})
	if e != nil {
		return infra()
	}
	bundles, e = client.SignatureBundles(ctx, reg, c.SignaturesRepository, credential, item.Digest, c.ProdKey.PublicKeyPEM)
	if e != nil {
		return infra()
	}
	for _, b := range bundles {
		if signaturePayloadMatches(b.Payload, item) {
			item.Result = "SIGNED"
			item.ProdSignature = &PromotionSignature{imagesigning.Tag(item.Digest, "sig"), base64.StdEncoding.EncodeToString(b.Payload), base64.StdEncoding.EncodeToString(b.Signature), c.ProdKey.Fingerprint()}
			return item
		}
	}
	return infra()
}
func signaturePayloadMatches(payload []byte, item PromotionItem) bool {
	var v struct {
		Critical struct {
			Type     string            `json:"type"`
			Identity map[string]string `json:"identity"`
			Image    map[string]string `json:"image"`
		} `json:"critical"`
	}
	id, ok := imagesigning.RepositoryIdentity(item.ProdImageRef)
	return ok && json.Unmarshal(payload, &v) == nil && v.Critical.Type == "cosign container image signature" && v.Critical.Image["docker-manifest-digest"] == item.Digest && v.Critical.Identity["docker-reference"] == id
}
func matchesPrefix(ref string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(ref, p) {
			return true
		}
	}
	return false
}
func rewriteRepository(c Config, devRepository string) string {
	keys := make([]string, 0, len(c.Registry.Rewrite))
	for k := range c.Registry.Rewrite {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	for _, k := range keys {
		if strings.HasPrefix(devRepository, k) {
			return c.Registry.Rewrite[k] + strings.TrimPrefix(devRepository, k)
		}
	}
	if _, rest, ok := strings.Cut(devRepository, "/"); ok {
		return rest
	}
	return devRepository
}
