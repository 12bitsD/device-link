package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

type Device struct {
	Name      string  `json:"name"`
	Status    string  `json:"status"`
	OS        string  `json:"os"`
	Arch      string  `json:"arch"`
	Shell     string  `json:"shell"`
	ExpiresAt *string `json:"expires_at"`
	LastSeen  string  `json:"last_seen"`
}

func New(baseURL, token, caPath string) (*Client, error) {
	normalized, err := NormalizeHubURL(baseURL)
	if err != nil {
		return nil, err
	}
	if token == "" {
		return nil, fmt.Errorf("controller token is required (use --token or DL_TOKEN)")
	}
	httpClient, err := HTTPClient(caPath)
	if err != nil {
		return nil, err
	}
	httpClient.Timeout = 15 * time.Second
	return &Client{baseURL: normalized, token: token, http: httpClient}, nil
}

func NormalizeHubURL(raw string) (string, error) {
	trimmed := strings.TrimRight(strings.TrimSpace(raw), "/")
	if trimmed == "" {
		return "", fmt.Errorf("hub URL is required")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return "", fmt.Errorf("invalid hub HTTPS URL %q", raw)
	}
	return trimmed, nil
}

func HTTPClient(caPath string) (*http.Client, error) {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if caPath != "" {
		contents, err := os.ReadFile(caPath)
		if err != nil {
			return nil, fmt.Errorf("read hub CA: %w", err)
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(contents) {
			return nil, fmt.Errorf("parse hub CA")
		}
		tlsConfig.RootCAs = roots
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = tlsConfig
	return &http.Client{Transport: transport}, nil
}

func ttlSeconds(ttl *time.Duration) *int64 {
	if ttl == nil {
		return nil
	}
	seconds := int64((*ttl + time.Second - 1) / time.Second)
	return &seconds
}

func (c *Client) Invite(ctx context.Context, name string, ttl *time.Duration) (string, error) {
	request := struct {
		Name       string `json:"name"`
		TTLSeconds *int64 `json:"ttl_seconds"`
	}{Name: name, TTLSeconds: ttlSeconds(ttl)}
	response, err := c.request(ctx, http.MethodPost, "/v1/invites", request)
	if err != nil {
		return "", err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("invite rejected: %s", response.Status)
	}
	var invite struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(response.Body).Decode(&invite); err != nil {
		return "", fmt.Errorf("decode invite: %w", err)
	}
	return invite.Code, nil
}

func (c *Client) List(ctx context.Context) ([]Device, error) {
	response, err := c.request(ctx, http.MethodGet, "/v1/devices", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list rejected: %s", response.Status)
	}
	devices := make([]Device, 0)
	if err := json.NewDecoder(response.Body).Decode(&devices); err != nil {
		return nil, fmt.Errorf("decode devices: %w", err)
	}
	return devices, nil
}

func (c *Client) SetTTL(ctx context.Context, name string, ttl *time.Duration) error {
	path := "/v1/devices/" + url.PathEscape(name) + "/ttl"
	response, err := c.request(ctx, http.MethodPut, path, map[string]*int64{"ttl_seconds": ttlSeconds(ttl)})
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("TTL update rejected: %s", response.Status)
	}
	return nil
}

func (c *Client) Delete(ctx context.Context, name string) error {
	response, err := c.request(ctx, http.MethodDelete, "/v1/devices/"+url.PathEscape(name), nil)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("delete rejected: %s", response.Status)
	}
	return nil
}

func (c *Client) request(ctx context.Context, method, path string, value any) (*http.Response, error) {
	var body bytes.Buffer
	if value != nil {
		if err := json.NewEncoder(&body).Encode(value); err != nil {
			return nil, fmt.Errorf("encode request: %w", err)
		}
	}
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, &body)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return nil, fmt.Errorf("call hub: %w", err)
	}
	return response, nil
}
