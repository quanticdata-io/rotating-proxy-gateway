// Package gateway is the local HTTP proxy server: it accepts plain HTTP and
// CONNECT requests and forwards each one through an upstream from the pool.
package gateway

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quanticdata-io/rotating-proxy-gateway/internal/pool"
	"github.com/quanticdata-io/rotating-proxy-gateway/internal/upstream"
)

// Version is set at build time.
var Version = "dev"

// Config of the gateway.
type Config struct {
	// Username and Password, when set, are required from clients
	// (Proxy-Authorization: Basic). The username may carry a session suffix:
	// "<user>-session-<id>".
	Username, Password string
	// Retries is how many other upstreams are tried when one fails to connect.
	Retries int
	// Timeout for connecting through an upstream.
	Timeout time.Duration
	// StickyByClientIP makes each client address its own session when it sends no session.
	StickyByClientIP bool
	// Insecure skips TLS verification of https:// upstreams.
	Insecure bool
	Logger   *slog.Logger
}

// Gateway implements http.Handler for the proxy port.
type Gateway struct {
	cfg       Config
	pool      *pool.Pool
	dialer    upstream.Dialer
	transport *http.Transport
	log       *slog.Logger

	requests atomic.Int64
	failures atomic.Int64
	started  time.Time
}

type ctxKey struct{}

// New builds a gateway around a pool.
func New(p *pool.Pool, cfg Config) *Gateway {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 15 * time.Second
	}
	if cfg.Retries < 0 {
		cfg.Retries = 0
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	g := &Gateway{cfg: cfg, pool: p, log: cfg.Logger, started: time.Now()}
	g.dialer = upstream.Dialer{Timeout: cfg.Timeout, Insecure: cfg.Insecure}
	nd := &net.Dialer{Timeout: cfg.Timeout}
	g.transport = &http.Transport{
		// One connection per request: the upstream sees a new connection each
		// time, which is what rotating gateways key their rotation on.
		DisableKeepAlives:     true,
		ResponseHeaderTimeout: 2 * time.Minute,
		TLSHandshakeTimeout:   cfg.Timeout,
		TLSClientConfig:       g.dialer.TLSConfig(),
		Proxy: func(r *http.Request) (*url.URL, error) {
			u := r.Context().Value(ctxKey{}).(upstream.Upstream)
			if u.Scheme == upstream.HTTP || u.Scheme == upstream.HTTPS {
				return u.URL(), nil
			}
			return nil, nil
		},
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			u := ctx.Value(ctxKey{}).(upstream.Upstream)
			if u.Scheme == upstream.SOCKS4 || u.Scheme == upstream.SOCKS5 {
				return g.dialer.Dial(ctx, u, addr)
			}
			return nd.DialContext(ctx, network, addr)
		},
	}
	return g
}

// credentials returns the username and password of a Proxy-Authorization header.
func credentials(r *http.Request) (string, string, bool) {
	h := r.Header.Get("Proxy-Authorization")
	const prefix = "Basic "
	if len(h) < len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", "", false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(h[len(prefix):]))
	if err != nil {
		return "", "", false
	}
	user, pass, ok := strings.Cut(string(raw), ":")
	return user, pass, ok
}

// SessionFromUsername extracts the session id from "session-<id>" or
// "<anything>-session-<id>". It returns "" when there is none.
func SessionFromUsername(user string) string {
	if strings.HasPrefix(user, "session-") {
		return user[len("session-"):]
	}
	if i := strings.LastIndex(user, "-session-"); i >= 0 {
		return user[i+len("-session-"):]
	}
	return ""
}

// authorize checks client credentials and returns the session id, if any.
func (g *Gateway) authorize(r *http.Request) (session string, ok bool) {
	user, pass, has := credentials(r)
	if session = SessionFromUsername(user); session == "" {
		session = strings.TrimSpace(r.Header.Get("X-Gateway-Session"))
	}
	if g.cfg.Username == "" {
		return session, true
	}
	if !has {
		return "", false
	}
	base := user
	if i := strings.LastIndex(user, "-session-"); i >= 0 {
		base = user[:i]
	}
	userOK := subtle.ConstantTimeCompare([]byte(base), []byte(g.cfg.Username)) == 1
	passOK := subtle.ConstantTimeCompare([]byte(pass), []byte(g.cfg.Password)) == 1
	return session, userOK && passOK
}

func (g *Gateway) clientSession(r *http.Request, session string) string {
	if session == "" && g.cfg.StickyByClientIP {
		if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
			return "ip:" + host
		}
	}
	return session
}

// ServeHTTP handles one client request.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.requests.Add(1)
	session, ok := g.authorize(r)
	if !ok {
		w.Header().Set("Proxy-Authenticate", `Basic realm="rotating-proxy-gateway"`)
		http.Error(w, "proxy authentication required", http.StatusProxyAuthRequired)
		return
	}
	session = g.clientSession(r, session)
	if r.Method == http.MethodConnect {
		g.connect(w, r, session)
		return
	}
	if r.URL.Host == "" || !r.URL.IsAbs() {
		http.Error(w, "this is a proxy: send absolute-form requests (http://host/path) or CONNECT", http.StatusBadRequest)
		return
	}
	g.forward(w, r, session)
}

// tryUpstreams runs fn with up to Retries+1 different upstreams until one
// returns nil. It returns the last error.
func (g *Gateway) tryUpstreams(session string, fn func(*pool.Lease) error) error {
	tried := map[string]bool{}
	var last error
	for attempt := 0; attempt <= g.cfg.Retries; attempt++ {
		lease, err := g.pool.Acquire(session, tried)
		if err != nil {
			if last == nil {
				last = err
			}
			break
		}
		tried[lease.Upstream.Key()] = true
		err = fn(lease)
		if err == nil {
			return nil
		}
		var nr noRetry
		if errors.As(err, &nr) {
			return nr.err
		}
		lease.Done(err)
		g.log.Warn("upstream failed", "upstream", lease.Upstream.Redacted(), "error", err)
		last = err
	}
	return last
}

// noRetry wraps an error that must not trigger another upstream (the lease
// was already settled, or the client went away).
type noRetry struct{ err error }

func (n noRetry) Error() string { return n.err.Error() }

func (g *Gateway) connect(w http.ResponseWriter, r *http.Request, session string) {
	target := r.Host
	if _, _, err := net.SplitHostPort(target); err != nil {
		http.Error(w, "CONNECT needs host:port", http.StatusBadRequest)
		return
	}
	var conn net.Conn
	var lease *pool.Lease
	err := g.tryUpstreams(session, func(l *pool.Lease) error {
		c, err := g.dialer.Dial(r.Context(), l.Upstream, target)
		if err != nil {
			if errors.Is(err, upstream.ErrTargetRefused) {
				// The upstream works; the target refused. Do not punish the upstream.
				l.Done(nil)
				return noRetry{err}
			}
			return err
		}
		conn, lease = c, l
		return nil
	})
	if err != nil {
		g.fail(w, err)
		return
	}
	defer lease.Done(nil) // after the tunnel closes, so least-conn sees it as active
	hj, ok := w.(http.Hijacker)
	if !ok {
		conn.Close()
		http.Error(w, "hijacking not supported", http.StatusInternalServerError)
		return
	}
	client, buf, err := hj.Hijack()
	if err != nil {
		conn.Close()
		return
	}
	if _, err := client.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n")); err != nil {
		client.Close()
		conn.Close()
		return
	}
	if n := buf.Reader.Buffered(); n > 0 {
		pending, _ := buf.Peek(n)
		if _, err := conn.Write(pending); err != nil {
			client.Close()
			conn.Close()
			return
		}
	}
	pipe(client, conn)
}

var hopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
	"X-Gateway-Session",
}

func (g *Gateway) forward(w http.ResponseWriter, r *http.Request, session string) {
	replayable := r.Body == nil || r.Body == http.NoBody || r.ContentLength == 0
	var resp *http.Response
	var lease *pool.Lease
	err := g.tryUpstreams(session, func(l *pool.Lease) error {
		out := r.Clone(context.WithValue(r.Context(), ctxKey{}, l.Upstream))
		out.RequestURI = ""
		for _, h := range hopHeaders {
			out.Header.Del(h)
		}
		res, err := g.transport.RoundTrip(out)
		if err != nil {
			if r.Context().Err() != nil {
				l.Done(nil)
				return noRetry{err}
			}
			if !replayable {
				l.Done(err)
				return noRetry{err}
			}
			return err
		}
		if res.StatusCode == http.StatusProxyAuthRequired {
			res.Body.Close()
			return fmt.Errorf("%w (HTTP 407)", upstream.ErrUpstreamAuth)
		}
		resp, lease = res, l
		return nil
	})
	if err != nil {
		g.fail(w, err)
		return
	}
	defer resp.Body.Close()
	defer lease.Done(nil)
	for _, h := range hopHeaders {
		resp.Header.Del(h)
	}
	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (g *Gateway) fail(w http.ResponseWriter, err error) {
	g.failures.Add(1)
	status := http.StatusBadGateway
	switch {
	case errors.Is(err, pool.ErrNoUpstream):
		status = http.StatusServiceUnavailable
	case errors.Is(err, upstream.ErrTargetRefused):
		status = http.StatusBadGateway
	case errors.Is(err, context.DeadlineExceeded), strings.Contains(err.Error(), "timed out"):
		status = http.StatusGatewayTimeout
	}
	http.Error(w, "rotating-proxy-gateway: "+err.Error(), status)
}

func pipe(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	cp := func(dst, src net.Conn) {
		defer wg.Done()
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		} else {
			_ = dst.Close()
		}
	}
	go cp(a, b)
	go cp(b, a)
	wg.Wait()
	a.Close()
	b.Close()
}

// AdminHandler serves GET /status (JSON) and GET /healthz.
func (g *Gateway) AdminHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		statuses, _ := g.pool.Snapshot()
		for _, s := range statuses {
			if s.State == "available" {
				_, _ = io.WriteString(w, "ok\n")
				return
			}
		}
		http.Error(w, "no upstream available", http.StatusServiceUnavailable)
	})
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) {
		statuses, sessions := g.pool.Snapshot()
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{
			"version":        Version,
			"uptime_seconds": int(time.Since(g.started).Seconds()),
			"requests":       g.requests.Load(),
			"failed":         g.failures.Load(),
			"sessions":       sessions,
			"upstreams":      statuses,
		})
	})
	mux.HandleFunc("POST /revive", func(w http.ResponseWriter, _ *http.Request) {
		g.pool.Revive()
		_, _ = io.WriteString(w, "ok\n")
	})
	return mux
}

// HealthCheck dials target through every upstream once and reports the
// outcome to the pool. It is meant to run on a ticker.
func (g *Gateway) HealthCheck(ctx context.Context, target string) {
	var wg sync.WaitGroup
	sem := make(chan struct{}, 16)
	for _, u := range g.pool.Upstreams() {
		lease := g.pool.LeaseFor(u)
		if lease == nil {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(l *pool.Lease) {
			defer wg.Done()
			defer func() { <-sem }()
			conn, err := g.dialer.Dial(ctx, l.Upstream, target)
			if err == nil {
				conn.Close()
			}
			if errors.Is(err, upstream.ErrTargetRefused) {
				err = nil
			}
			l.Done(err)
		}(lease)
	}
	wg.Wait()
}
