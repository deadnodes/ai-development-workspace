package agentconnect

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type deviceAuthorization struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}
type tokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	Error       string `json:"error"`
}

func authClient() *http.Client {
	return &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
func postAuth(ctx context.Context, client *http.Client, endpoint string, body any, result any) (int, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return 0, errors.New("invalid authentication URL")
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 0, errors.New("authentication request failed")
	}
	defer response.Body.Close()
	data, err = io.ReadAll(io.LimitReader(response.Body, 65537))
	if err != nil || len(data) > 65536 {
		return 0, errors.New("authentication response unreadable or oversized")
	}
	if err = json.Unmarshal(data, result); err != nil {
		return 0, errors.New("invalid authentication response")
	}
	return response.StatusCode, nil
}

func RunLogin(ctx context.Context, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("release-control login", flag.ContinueOnError)
	flags.SetOutput(out)
	var raw, profile, name, scope, products string
	flags.StringVar(&raw, "url", "", "Control Plane base URL (HTTPS, or loopback HTTP)")
	flags.StringVar(&profile, "profile", "default", "local credential profile")
	flags.StringVar(&name, "name", "Codex", "name shown during human approval")
	flags.StringVar(&scope, "scope", "read write", "requested permissions: read write deploy")
	flags.StringVar(&products, "products", "", "comma-separated Product IDs; omitted uses server grants")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	endpoint, err := secureEndpoint(raw)
	if err != nil {
		return err
	}
	if _, err = credentialName(endpoint, profile); err != nil {
		return err
	}
	var ids []string
	for _, p := range strings.Split(products, ",") {
		if p = strings.TrimSpace(p); p != "" {
			ids = append(ids, p)
		}
	}
	var device deviceAuthorization
	client := authClient()
	status, err := postAuth(ctx, client, endpoint+"/auth/device/code", map[string]any{"name": name, "scope": scope, "products": ids}, &device)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("device authorization failed (HTTP %d)", status)
	}
	if device.DeviceCode == "" || device.UserCode == "" || device.ExpiresIn <= 0 || device.ExpiresIn > 3600 || device.Interval < 0 || device.Interval > 300 {
		return errors.New("invalid device authorization response")
	}
	// Never direct a user to a different origin supplied by a malformed response.
	verification := device.VerificationURIComplete
	if verification == "" {
		verification = device.VerificationURI
	}
	target, e := url.Parse(verification)
	base, _ := url.Parse(endpoint)
	if e != nil || target.User != nil || target.Scheme != base.Scheme || target.Host != base.Host {
		return errors.New("invalid device verification URL")
	}
	fmt.Fprintf(out, "Open %s\nConfirm code: %s\nApprove this independent agent in your browser. Waiting for approval…\n", verification, device.UserCode)
	token, err := pollDevice(ctx, client, endpoint, device, waitDevice)
	if err != nil {
		return err
	}
	if err = saveCredential(endpoint, profile, token); err != nil {
		return fmt.Errorf("save agent credential: %w", err)
	}
	_, err = fmt.Fprintf(out, "Agent credential saved securely for profile %s. Run release-control connect with the same URL and profile.\n", profile)
	return err
}

func waitDevice(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
func pollDevice(ctx context.Context, client *http.Client, endpoint string, device deviceAuthorization, wait func(context.Context, time.Duration) error) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(device.ExpiresIn)*time.Second)
	defer cancel()
	interval := time.Duration(device.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	for {
		if err := wait(ctx, interval); err != nil {
			return "", err
		}
		var result tokenResponse
		status, err := postAuth(ctx, client, endpoint+"/auth/device/token", map[string]string{"grant_type": "urn:ietf:params:oauth:grant-type:device_code", "device_code": device.DeviceCode}, &result)
		if err != nil {
			return "", err
		}
		if status == http.StatusOK && result.AccessToken != "" && strings.EqualFold(result.TokenType, "Bearer") {
			return result.AccessToken, nil
		}
		switch result.Error {
		case "authorization_pending":
			continue
		case "slow_down":
			interval += 5 * time.Second
		case "expired_token":
			return "", errors.New("device code expired; run login again")
		case "access_denied":
			return "", errors.New("device authorization was denied")
		default:
			return "", fmt.Errorf("device token request failed (HTTP %d)", status)
		}
	}
}
