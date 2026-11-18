package gateway

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/quanticdata-io/rotating-proxy-gateway/internal/pool"
	"github.com/quanticdata-io/rotating-proxy-gateway/internal/testutil"
	"github.com/quanticdata-io/rotating-proxy-gateway/internal/upstream"
)

type lab struct {
	origin    *httptest.Server
	tlsOrigin *httptest.Server
	proxies   []*testutil.HTTPProxy
	gateway   *httptest.Server
	gw        *Gateway
}

func newLab(t *testing.T, cfg Config, lines ...string) *lab {
	t.Helper()
	l := &lab{origin: httptest.NewServer(testutil.Origin()), tlsOrigin: httptest.NewTLSServer(testutil.Origin())}
	t.Cleanup(l.origin.Close)
	t.Cleanup(l.tlsOrigin.Close)
	var ups []upstream.Upstream
	for _, line := range lines {
		if strings.HasPrefix(line, "proxy:") { // proxy:<name>[:user:pass]
			parts := strings.Split(line, ":")
			p := &testutil.HTTPProxy{Name: parts[1]}
			if len(parts) == 4 {
				p.Username, p.Password = parts[2], parts[3]
			}
			srv := httptest.NewServer(p)
			t.Cleanup(srv.Close)
			l.proxies = append(l.proxies, p)
			line = strings.TrimPrefix(srv.URL, "http://")
			if p.Username != "" {
				line = p.Username + ":" + p.Password + "@" + line
			}
		}
		u, err := upstream.Parse(line, upstream.HTTP)
		if err != nil {
			t.Fatal(err)
		}
		ups = append(ups, u)
	}
	opts := pool.DefaultOptions()
	opts.MaxFailures = 0
	p, err := pool.New(ups, opts)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Timeout = 5 * time.Second
	l.gw = New(p, cfg)
	l.gateway = httptest.NewServer(l.gw)
	t.Cleanup(l.gateway.Close)
	return l
}

// client goes through the gateway. user may carry a session suffix.
func (l *lab) client(user, pass string) *http.Client {
	u, _ := url.Parse(l.gateway.URL)
	if user != "" {
		u.User = url.UserPassword(user, pass)
	}
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			Proxy:             http.ProxyURL(u),
			DisableKeepAlives: true,
			TLSClientConfig:   &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test origin
		},
	}
}

func body(t *testing.T, c *http.Client, target string) (int, string) {
	t.Helper()
	resp, err := c.Get(target)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestPlainHTTPRotates(t *testing.T) {
	l := newLab(t, Config{}, "proxy:a", "proxy:b", "proxy:c:u:p@ss")
	c := l.client("", "")
	var vias []string
	for i := 0; i < 6; i++ {
		status, b := body(t, c, l.origin.URL+"/page")
		if status != 200 {
			t.Fatalf("status %d: %s", status, b)
		}
		vias = append(vias, strings.Fields(b)[0])
	}
	if strings.Join(vias, ",") != "a,b,c,a,b,c" {
		t.Fatalf("rotation: %v", vias)
	}
}

func TestCONNECTRotatesAndSticks(t *testing.T) {
	l := newLab(t, Config{}, "proxy:a", "proxy:b")
	c := l.client("", "")
	for i := 0; i < 4; i++ {
		if status, b := body(t, c, l.tlsOrigin.URL+"/secure"); status != 200 || !strings.HasSuffix(b, "GET /secure") {
			t.Fatalf("status %d: %s", status, b)
		}
	}
	if a, b := l.proxies[0].Connects.Load(), l.proxies[1].Connects.Load(); a != 2 || b != 2 {
		t.Fatalf("CONNECT spread a=%d b=%d", a, b)
	}
	sticky := l.client("session-abc", "x")
	for i := 0; i < 4; i++ {
		body(t, sticky, l.tlsOrigin.URL+"/secure")
	}
	a, b := l.proxies[0].Connects.Load(), l.proxies[1].Connects.Load()
	if (a != 6 || b != 2) && (a != 2 || b != 6) {
		t.Fatalf("session did not stick: a=%d b=%d", a, b)
	}
}

func TestSessionHeaderForPlainHTTP(t *testing.T) {
	l := newLab(t, Config{}, "proxy:a", "proxy:b", "proxy:c")
	c := l.client("", "")
	first := ""
	for i := 0; i < 4; i++ {
		req, _ := http.NewRequest(http.MethodGet, l.origin.URL+"/", nil)
		req.Header.Set("X-Gateway-Session", "cart-1")
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		via := strings.Fields(string(b))[0]
		if first == "" {
			first = via
		} else if via != first {
			t.Fatalf("session moved from %s to %s", first, via)
		}
	}
}

func TestFailoverToNextUpstream(t *testing.T) {
	dead, stop := testutil.ClosedPort()
	defer stop()
	l := newLab(t, Config{Retries: 2}, dead, "proxy:good")
	c := l.client("", "")
	for i := 0; i < 3; i++ {
		if status, b := body(t, c, l.origin.URL+"/"); status != 200 || !strings.HasPrefix(b, "good") {
			t.Fatalf("status %d: %s", status, b)
		}
		if status, _ := body(t, c, l.tlsOrigin.URL+"/"); status != 200 {
			t.Fatalf("CONNECT failover: status %d", status)
		}
	}
	st, _ := l.gw.pool.Snapshot()
	if st[0].Failures == 0 || st[0].State != "cooling" {
		t.Fatalf("dead upstream not rested: %+v", st[0])
	}
}

func TestUpstreamAuthFailureFailsOver(t *testing.T) {
	l := newLab(t, Config{Retries: 1}, "proxy:locked:u:right", "proxy:open")
	// Replace the first upstream's password with a wrong one.
	wrong := l.gw.pool.Upstreams()[0]
	wrong.Password = "wrong"
	p, _ := pool.New([]upstream.Upstream{wrong, l.gw.pool.Upstreams()[1]}, pool.DefaultOptions())
	l.gw.pool = p
	if status, b := body(t, l.client("", ""), l.origin.URL+"/"); status != 200 || !strings.HasPrefix(b, "open") {
		t.Fatalf("status %d: %s", status, b)
	}
}

func TestAllUpstreamsDown(t *testing.T) {
	dead, stop := testutil.ClosedPort()
	defer stop()
	l := newLab(t, Config{Retries: 3}, dead)
	status, b := body(t, l.client("", ""), l.origin.URL+"/")
	if status != http.StatusBadGateway {
		t.Fatalf("first failure: want 502, got %d %s", status, b)
	}
	status, b = body(t, l.client("", ""), l.origin.URL+"/")
	if status != http.StatusServiceUnavailable || !strings.Contains(b, "cooling down") {
		t.Fatalf("while cooling: want 503, got %d %s", status, b)
	}
}

func TestClientAuth(t *testing.T) {
	l := newLab(t, Config{Username: "me", Password: "pw"}, "proxy:a", "proxy:b")
	if status, _ := body(t, l.client("", ""), l.origin.URL+"/"); status != http.StatusProxyAuthRequired {
		t.Fatalf("no credentials: want 407, got %d", status)
	}
	if status, _ := body(t, l.client("me", "bad"), l.origin.URL+"/"); status != http.StatusProxyAuthRequired {
		t.Fatalf("bad password: want 407, got %d", status)
	}
	if status, _ := body(t, l.client("me", "pw"), l.origin.URL+"/"); status != 200 {
		t.Fatalf("good credentials: got %d", status)
	}
	c := l.client("me-session-42", "pw")
	_, first := body(t, c, l.origin.URL+"/")
	_, second := body(t, c, l.origin.URL+"/")
	if strings.Fields(first)[0] != strings.Fields(second)[0] {
		t.Fatalf("authenticated session did not stick: %s / %s", first, second)
	}
}

func TestClientCredentialsAreNotForwarded(t *testing.T) {
	l := newLab(t, Config{Username: "me", Password: "pw"}, "proxy:a:up:secret")
	if status, b := body(t, l.client("me", "pw"), l.origin.URL+"/"); status != 200 {
		t.Fatalf("status %d: %s", status, b) // upstream wants up:secret, not me:pw
	}
}

func TestStickyByClientIP(t *testing.T) {
	l := newLab(t, Config{StickyByClientIP: true}, "proxy:a", "proxy:b")
	c := l.client("", "")
	_, first := body(t, c, l.origin.URL+"/")
	for i := 0; i < 3; i++ {
		if _, b := body(t, c, l.origin.URL+"/"); strings.Fields(b)[0] != strings.Fields(first)[0] {
			t.Fatal("client IP session did not stick")
		}
	}
}

func TestSOCKS5Upstream(t *testing.T) {
	socks, err := testutil.ListenSOCKS5("su", "sp")
	if err != nil {
		t.Fatal(err)
	}
	defer socks.Close()
	l := newLab(t, Config{}, "socks5://su:sp@"+socks.Addr())
	if status, b := body(t, l.client("", ""), l.origin.URL+"/s"); status != 200 || b != "direct GET /s" {
		t.Fatalf("plain over socks: %d %q", status, b)
	}
	if status, _ := body(t, l.client("", ""), l.tlsOrigin.URL+"/s"); status != 200 {
		t.Fatalf("CONNECT over socks: %d", status)
	}
	if socks.Connects.Load() != 2 {
		t.Fatalf("socks connects: %d", socks.Connects.Load())
	}
}

func TestNotAProxyRequest(t *testing.T) {
	l := newLab(t, Config{}, "proxy:a")
	resp, err := http.Get(l.gateway.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
}

func TestAdminEndpoints(t *testing.T) {
	l := newLab(t, Config{}, "proxy:a:user:topsecret")
	body(t, l.client("", ""), l.origin.URL+"/")
	admin := httptest.NewServer(l.gw.AdminHandler())
	defer admin.Close()
	resp, err := http.Get(admin.URL + "/status")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(raw), "topsecret") {
		t.Fatal("status leaks the upstream password")
	}
	var st struct {
		Requests  int64         `json:"requests"`
		Upstreams []pool.Status `json:"upstreams"`
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	if st.Requests != 1 || st.Upstreams[0].Successes != 1 || st.Upstreams[0].State != "available" {
		t.Fatalf("%s", raw)
	}
	resp, _ = http.Get(admin.URL + "/healthz")
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("healthz: %d", resp.StatusCode)
	}
}

func TestHealthCheckRestsBrokenUpstreams(t *testing.T) {
	dead, stop := testutil.ClosedPort()
	defer stop()
	l := newLab(t, Config{}, dead, "proxy:good")
	l.gw.HealthCheck(context.Background(), strings.TrimPrefix(l.origin.URL, "http://"))
	st, _ := l.gw.pool.Snapshot()
	if st[0].State != "cooling" || st[1].State != "available" || st[1].Successes != 1 {
		t.Fatalf("%+v", st)
	}
}

func TestSessionFromUsername(t *testing.T) {
	cases := map[string]string{
		"session-abc": "abc", "me-session-42": "42", "me": "", "": "", "a-session-b-session-c": "c",
	}
	for in, want := range cases {
		if got := SessionFromUsername(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}
