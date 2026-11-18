package quanticdata

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &Client{APIKey: "qd_test_key", BaseURL: srv.URL + "/v1", HTTP: srv.Client()}
}

func TestGenerate(t *testing.T) {
	var got map[string]any
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/public/proxies/generate" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer qd_test_key" {
			t.Errorf("auth header = %q", r.Header.Get("Authorization"))
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"type":"response","message":"success","payload":{"orderId":"ord_1","proxies":["http://u:p@gw.example.net:8080","http://u2:p@gw.example.net:8080"]},"pagination":{}}`))
	})
	lines, err := c.Generate(context.Background(), GenerateRequest{OrderID: "ord_1", Quantity: 2, Country: "us", Rotation: "sticky", SessionTime: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 {
		t.Fatalf("lines = %v", lines)
	}
	want := map[string]any{"orderId": "ord_1", "quantity": float64(2), "country": "us", "rotation": "sticky", "sessionTime": float64(10), "format": "http://user:pass@host:port"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("body[%s] = %v, want %v", k, got[k], v)
		}
	}
	if _, ok := got["protocol"]; ok {
		t.Error("empty protocol must be omitted")
	}
}

func TestGenerateSOCKS5Format(t *testing.T) {
	var got GenerateRequest
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"type":"response","message":"ok","payload":{"proxies":["socks5://u:p@h.example:1080"]}}`))
	})
	if _, err := c.Generate(context.Background(), GenerateRequest{OrderID: "o", Protocol: "socks5"}); err != nil {
		t.Fatal(err)
	}
	if got.Format != "socks5://user:pass@host:port" {
		t.Fatalf("format = %q", got.Format)
	}
}

func TestGenerateErrors(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"type":"error","message":"Order not found"}`))
	})
	_, err := c.Generate(context.Background(), GenerateRequest{OrderID: "missing"})
	if err == nil || !strings.Contains(err.Error(), "Order not found") || !strings.Contains(err.Error(), "404") {
		t.Fatalf("err = %v", err)
	}

	pending := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"type":"response","message":"preparing","payload":{"orderId":"o","pending":true,"proxies":[]}}`))
	})
	if _, err := pending.Generate(context.Background(), GenerateRequest{OrderID: "o"}); err == nil || !strings.Contains(err.Error(), "provisioned") {
		t.Fatalf("err = %v", err)
	}
}

func TestValidate(t *testing.T) {
	bad := []GenerateRequest{
		{},
		{OrderID: "o", Protocol: "socks4"},
		{OrderID: "o", Quantity: 10001},
		{OrderID: "o", Rotation: "random"},
		{OrderID: "o", SessionTime: 2000},
		{OrderID: "o", Gateway: "mars"},
	}
	for i, r := range bad {
		if r.Validate() == nil {
			t.Errorf("case %d should fail", i)
		}
	}
}

func TestListPlansPaginates(t *testing.T) {
	calls := 0
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/v1/public/proxies" || r.URL.Query().Get("active") != "true" {
			t.Errorf("unexpected %s", r.URL)
		}
		if r.URL.Query().Get("offset") == "0" {
			_, _ = w.Write([]byte(`{"type":"response","message":"ok","payload":{"proxies":[{"orderId":"a","planType":"residentialpremium","isActive":true}],"pagination":{"hasMore":true}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"type":"response","message":"ok","payload":{"proxies":[{"orderId":"b","planType":"mobile","isActive":true}],"pagination":{"hasMore":false}}}`))
	})
	plans, err := c.ListPlans(context.Background())
	if err != nil || len(plans) != 2 || plans[1].OrderID != "b" || calls != 2 {
		t.Fatalf("plans = %+v, err = %v, calls = %d", plans, err, calls)
	}
}

func TestNewFromEnv(t *testing.T) {
	t.Setenv(EnvAPIKey, "")
	if _, err := NewFromEnv(); err == nil {
		t.Fatal("expected an error without a key")
	}
	t.Setenv(EnvAPIKey, "qd_live_x")
	t.Setenv(EnvAPIBase, "http://127.0.0.1:1/v1/")
	c, err := NewFromEnv()
	if err != nil || c.BaseURL != "http://127.0.0.1:1/v1" {
		t.Fatalf("client = %+v, err = %v", c, err)
	}
}
