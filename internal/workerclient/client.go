// Package workerclient provides the isolated worker HTTP channel. It never runs jobs.
package workerclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"releasecontrol/internal/domain"
	"strings"
	"time"
)

type Client struct {
	endpoint string
	token    string
	http     *http.Client
}

func New(endpoint, token string) (*Client, error) {
	u, e := url.Parse(endpoint)
	if e != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" && u.Path != "/" || u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1")) || len(token) < 32 {
		return nil, errors.New("invalid worker endpoint or credential")
	}
	return &Client{endpoint: strings.TrimRight(endpoint, "/"), token: token, http: &http.Client{Transport: &http.Transport{Proxy: nil, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, DisableCompression: true, MaxResponseHeaderBytes: 16384, DialContext: (&net.Dialer{Timeout: 10 * time.Second}).DialContext}, Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("worker redirects forbidden") }}}, nil
}
func (c *Client) call(ctx context.Context, path string, in, out any) error {
	b, e := json.Marshal(in)
	if e != nil {
		return e
	}
	r, e := http.NewRequestWithContext(ctx, "POST", c.endpoint+"/api/worker/"+path, bytes.NewReader(b))
	if e != nil {
		return e
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+c.token)
	res, e := c.http.Do(r)
	if e != nil {
		return errors.New("worker transport failed")
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return errors.New("worker request rejected")
	}
	dec := json.NewDecoder(io.LimitReader(res.Body, 64<<10))
	if e = dec.Decode(out); e != nil {
		return errors.New("invalid worker response")
	}
	if dec.Decode(new(any)) != io.EOF {
		return errors.New("invalid worker response")
	}
	return nil
}
func (c *Client) Claim(ctx context.Context, in domain.WorkerClaim) (*domain.WorkerLease, error) {
	var out *domain.WorkerLease
	e := c.call(ctx, "claim", in, &out)
	return out, e
}
func (c *Client) Start(ctx context.Context, in domain.WorkerAttemptRequest) (domain.WorkerLease, error) {
	var out domain.WorkerLease
	e := c.call(ctx, "start", in, &out)
	return out, e
}
func (c *Client) Heartbeat(ctx context.Context, in domain.WorkerAttemptRequest) (domain.WorkerLease, error) {
	var out domain.WorkerLease
	e := c.call(ctx, "heartbeat", in, &out)
	return out, e
}
func (c *Client) Report(ctx context.Context, in domain.WorkerReport) (domain.WorkerLease, error) {
	var out domain.WorkerLease
	e := c.call(ctx, "report", in, &out)
	return out, e
}
