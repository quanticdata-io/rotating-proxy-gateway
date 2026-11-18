// Package quanticdata fetches proxy strings from a QuanticData account so the
// gateway can use them as upstreams. Only two documented endpoints are used:
// GET /public/proxies (list plans) and POST /public/proxies/generate.
package quanticdata

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// DefaultBaseURL is the documented API host.
const DefaultBaseURL = "https://api.quanticdata.io/v1"

// Environment variables read by NewFromEnv.
const (
	EnvAPIKey  = "QUANTICDATA_API_KEY" //nolint:gosec // the name of an environment variable, not a credential
	EnvAPIBase = "QUANTICDATA_API_BASE"
)

// Client is a minimal QuanticData API client.
type Client struct {
	APIKey  string
	BaseURL string
	HTTP    *http.Client
	// UserAgent sent with every request.
	UserAgent string
}

// NewFromEnv builds a client from QUANTICDATA_API_KEY and QUANTICDATA_API_BASE.
func NewFromEnv() (*Client, error) { return NewFromEnvFunc(os.Getenv) }

// NewFromEnvFunc is NewFromEnv with a custom environment lookup.
func NewFromEnvFunc(getenv func(string) string) (*Client, error) {
	key := strings.TrimSpace(getenv(EnvAPIKey))
	if key == "" {
		return nil, fmt.Errorf("%s is not set: create an API key in the QuanticData dashboard", EnvAPIKey)
	}
	base := strings.TrimSpace(getenv(EnvAPIBase))
	if base == "" {
		base = DefaultBaseURL
	}
	return &Client{APIKey: key, BaseURL: strings.TrimRight(base, "/"), HTTP: &http.Client{Timeout: 60 * time.Second}}, nil
}

// GenerateRequest is the body of POST /public/proxies/generate. Zero values are omitted.
type GenerateRequest struct {
	OrderID     string `json:"orderId"`
	Protocol    string `json:"protocol,omitempty"` // http | socks5
	Format      string `json:"format,omitempty"`
	Quantity    int    `json:"quantity,omitempty"` // 1..10000
	Country     string `json:"country,omitempty"`
	State       string `json:"state,omitempty"`
	City        string `json:"city,omitempty"`
	Rotation    string `json:"rotation,omitempty"`    // rotating | sticky | static
	SessionTime int    `json:"sessionTime,omitempty"` // minutes, 1..1440
	Gateway     string `json:"gateway,omitempty"`     // ww | us | eu | as
}

// Validate checks the request against the documented ranges and enums.
func (r GenerateRequest) Validate() error {
	if strings.TrimSpace(r.OrderID) == "" {
		return errors.New("orderId is required (see `rotating-proxy-gateway --qd-list-plans`)")
	}
	if r.Protocol != "" && r.Protocol != "http" && r.Protocol != "socks5" {
		return fmt.Errorf("protocol must be http or socks5, got %q", r.Protocol)
	}
	if r.Quantity < 0 || r.Quantity > 10000 {
		return fmt.Errorf("quantity must be between 1 and 10000, got %d", r.Quantity)
	}
	switch r.Rotation {
	case "", "rotating", "sticky", "static":
	default:
		return fmt.Errorf("rotation must be rotating, sticky or static, got %q", r.Rotation)
	}
	if r.SessionTime < 0 || r.SessionTime > 1440 {
		return fmt.Errorf("sessionTime must be between 1 and 1440 minutes, got %d", r.SessionTime)
	}
	switch r.Gateway {
	case "", "ww", "us", "eu", "as":
	default:
		return fmt.Errorf("gateway must be ww, us, eu or as, got %q", r.Gateway)
	}
	return nil
}

// Plan is one proxy plan of the account (subset of the documented ProxyPlan).
type Plan struct {
	OrderID       string  `json:"orderId"`
	PlanName      string  `json:"planName"`
	PlanType      string  `json:"planType"`
	IsActive      bool    `json:"isActive"`
	BandwidthLeft float64 `json:"bandwidthLeftGB"`
	Expiry        string  `json:"expiresAt"`
}

// APIError is an error envelope returned by the API.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("QuanticData API: %s (HTTP %d)", e.Message, e.Status)
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) (int, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rdr)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, fmt.Errorf("cannot reach the QuanticData API at %s: %w", c.BaseURL, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return resp.StatusCode, err
	}
	var env struct {
		Type    string          `json:"type"`
		Message string          `json:"message"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return resp.StatusCode, fmt.Errorf("QuanticData API: unexpected answer (HTTP %d)", resp.StatusCode)
	}
	if resp.StatusCode >= 400 || env.Type == "error" {
		msg := env.Message
		if msg == "" {
			msg = http.StatusText(resp.StatusCode)
		}
		return resp.StatusCode, &APIError{Status: resp.StatusCode, Message: msg}
	}
	if out != nil && len(env.Payload) > 0 {
		if err := json.Unmarshal(env.Payload, out); err != nil {
			return resp.StatusCode, fmt.Errorf("QuanticData API: cannot decode payload: %w", err)
		}
	}
	return resp.StatusCode, nil
}

// ListPlans returns the active proxy plans of the account (GET /public/proxies).
func (c *Client) ListPlans(ctx context.Context) ([]Plan, error) {
	var all []Plan
	for offset := 0; ; {
		var page struct {
			Proxies    []Plan `json:"proxies"`
			Pagination struct {
				HasMore bool `json:"hasMore"`
			} `json:"pagination"`
		}
		path := fmt.Sprintf("/public/proxies?limit=100&offset=%d&active=true", offset)
		if _, err := c.do(ctx, http.MethodGet, path, nil, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Proxies...)
		if !page.Pagination.HasMore || len(page.Proxies) == 0 {
			return all, nil
		}
		offset += len(page.Proxies)
	}
}

// Generate returns ready-to-use proxy strings for an order (POST /public/proxies/generate).
// The strings are requested as URLs (http://... or socks5://...) so they parse unambiguously.
func (c *Client) Generate(ctx context.Context, r GenerateRequest) ([]string, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	if r.Format == "" {
		if r.Protocol == "socks5" {
			r.Format = "socks5://user:pass@host:port"
		} else {
			r.Format = "http://user:pass@host:port"
		}
	}
	var payload struct {
		Proxies []string `json:"proxies"`
		Pending bool     `json:"pending"`
	}
	status, err := c.do(ctx, http.MethodPost, "/public/proxies/generate", r, &payload)
	if err != nil {
		return nil, err
	}
	if status == http.StatusAccepted || payload.Pending {
		return nil, errors.New("the order is still being provisioned: try again in a few minutes")
	}
	if len(payload.Proxies) == 0 {
		return nil, errors.New("the API returned no proxy strings for this order")
	}
	return payload.Proxies, nil
}
