// Package upstream parses upstream proxy strings and opens TCP tunnels
// through them (HTTP CONNECT, HTTPS CONNECT, SOCKS4/4a and SOCKS5).
package upstream

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Scheme is the protocol spoken to an upstream proxy.
type Scheme string

// Supported schemes.
const (
	HTTP   Scheme = "http"
	HTTPS  Scheme = "https"
	SOCKS4 Scheme = "socks4"
	SOCKS5 Scheme = "socks5"
)

// Upstream is one upstream proxy.
type Upstream struct {
	Scheme   Scheme
	Host     string
	Port     int
	Username string
	Password string
	HasAuth  bool
}

// ErrInvalid is wrapped by every parse error.
var ErrInvalid = errors.New("invalid proxy string")

// Address is host:port, with IPv6 literals bracketed.
func (u Upstream) Address() string { return net.JoinHostPort(u.Host, strconv.Itoa(u.Port)) }

// URL is the upstream as a URL, credentials included.
func (u Upstream) URL() *url.URL {
	out := &url.URL{Scheme: string(u.Scheme), Host: u.Address()}
	if u.HasAuth {
		out.User = url.UserPassword(u.Username, u.Password)
	}
	return out
}

// Redacted is the URL with the password masked, for logs and the status page.
func (u Upstream) Redacted() string {
	out := &url.URL{Scheme: string(u.Scheme), Host: u.Address()}
	if u.HasAuth {
		out.User = url.UserPassword(u.Username, "xxxxx")
	}
	return out.String()
}

// Key identifies the upstream (scheme, address and username).
func (u Upstream) Key() string { return string(u.Scheme) + "://" + u.Username + "@" + u.Address() }

func normalize(s string) (Scheme, bool) {
	switch strings.ToLower(s) {
	case "http":
		return HTTP, true
	case "https":
		return HTTPS, true
	case "socks4", "socks4a":
		return SOCKS4, true
	case "socks5", "socks5h", "socks":
		return SOCKS5, true
	}
	return "", false
}

// Parse reads host:port, user:pass@host:port, host:port:user:pass or
// scheme://[user:pass@]host:port. Lines without a scheme use def.
func Parse(line string, def Scheme) (Upstream, error) {
	s := strings.TrimSpace(line)
	if s == "" {
		return Upstream{}, fmt.Errorf("%w: empty", ErrInvalid)
	}
	scheme := def
	if i := strings.Index(s, "://"); i >= 0 {
		sc, ok := normalize(s[:i])
		if !ok {
			return Upstream{}, fmt.Errorf("%w: unsupported scheme %q", ErrInvalid, s[:i])
		}
		scheme = sc
		s = s[i+3:]
		s = strings.TrimSuffix(s, "/")
	}
	var u Upstream
	u.Scheme = scheme
	hostport := s
	if at := strings.LastIndex(s, "@"); at >= 0 {
		creds := s[:at]
		hostport = s[at+1:]
		user, pass, _ := strings.Cut(creds, ":")
		u.Username, u.Password, u.HasAuth = unescape(user), unescape(pass), true
	} else if parts := strings.Split(s, ":"); len(parts) >= 4 && !strings.Contains(s, "[") {
		hostport = parts[0] + ":" + parts[1]
		u.Username, u.Password, u.HasAuth = unescape(parts[2]), unescape(strings.Join(parts[3:], ":")), true
	}
	host, portStr, err := net.SplitHostPort(hostport)
	if err != nil || host == "" {
		return Upstream{}, fmt.Errorf("%w: expected host:port in %q", ErrInvalid, redactLine(line))
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return Upstream{}, fmt.Errorf("%w: bad port in %q", ErrInvalid, redactLine(line))
	}
	u.Host, u.Port = host, port
	return u, nil
}

func unescape(s string) string {
	if v, err := url.PathUnescape(s); err == nil {
		return v
	}
	return s
}

func redactLine(s string) string {
	if at := strings.LastIndex(s, "@"); at >= 0 {
		head := s[:at]
		if c := strings.LastIndex(head, ":"); c >= 0 && !strings.HasSuffix(head[:c+1], "//") {
			head = head[:c] + ":xxxxx"
		}
		return head + s[at:]
	}
	if parts := strings.Split(s, ":"); len(parts) >= 4 {
		return strings.Join(parts[:3], ":") + ":xxxxx"
	}
	return s
}

// ReadList parses one upstream per line; blank lines and # comments are skipped.
func ReadList(r io.Reader, def Scheme) ([]Upstream, error) {
	var out []Upstream
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	n := 0
	for sc.Scan() {
		n++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		u, err := Parse(line, def)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", n, err)
		}
		out = append(out, u)
	}
	return out, sc.Err()
}

// ErrUpstreamAuth means the upstream refused the credentials (407 or SOCKS auth failure).
var ErrUpstreamAuth = errors.New("upstream rejected the credentials")

// ErrTargetRefused means the upstream answered but could not reach the target.
var ErrTargetRefused = errors.New("upstream could not reach the target")

// Dialer opens tunnels through upstreams.
type Dialer struct {
	Timeout  time.Duration
	Insecure bool // skip TLS verification of https:// upstreams
}

// TLSConfig is the client TLS configuration used for https:// upstreams.
func (d Dialer) TLSConfig() *tls.Config {
	return &tls.Config{InsecureSkipVerify: d.Insecure, MinVersion: tls.VersionTLS12} //nolint:gosec // opt-in flag
}

// Dial opens a TCP tunnel to target (host:port) through u.
func (d Dialer) Dial(ctx context.Context, u Upstream, target string) (net.Conn, error) {
	timeout := d.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	nd := &net.Dialer{}
	conn, err := nd.DialContext(ctx, "tcp", u.Address())
	if err != nil {
		return nil, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()
	var out net.Conn
	switch u.Scheme {
	case HTTPS:
		cfg := d.TLSConfig()
		cfg.ServerName = u.Host
		tc := tls.Client(conn, cfg)
		if err = tc.HandshakeContext(ctx); err == nil {
			out, err = httpConnect(tc, u, target)
		}
	case HTTP:
		out, err = httpConnect(conn, u, target)
	case SOCKS5:
		err = socks5Connect(conn, u, target)
		out = conn
	case SOCKS4:
		err = socks4Connect(conn, u, target)
		out = conn
	default:
		err = fmt.Errorf("unsupported scheme %q", u.Scheme)
	}
	if err != nil {
		conn.Close()
		if ctx.Err() != nil && !errors.Is(err, ErrUpstreamAuth) && !errors.Is(err, ErrTargetRefused) {
			return nil, fmt.Errorf("upstream timed out: %w", ctx.Err())
		}
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	return out, nil
}

// bufferedConn returns bytes read ahead by a bufio.Reader before the raw conn.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufferedConn) Read(p []byte) (int, error) {
	if b.r.Buffered() > 0 {
		return b.r.Read(p)
	}
	return b.Conn.Read(p)
}

func httpConnect(conn net.Conn, u Upstream, target string) (net.Conn, error) {
	var sb strings.Builder
	fmt.Fprintf(&sb, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n", target, target)
	if u.HasAuth {
		token := base64.StdEncoding.EncodeToString([]byte(u.Username + ":" + u.Password))
		fmt.Fprintf(&sb, "Proxy-Authorization: Basic %s\r\n", token)
	}
	sb.WriteString("\r\n")
	if _, err := io.WriteString(conn, sb.String()); err != nil {
		return nil, err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		return nil, fmt.Errorf("reading CONNECT reply: %w", err)
	}
	_ = resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusProxyAuthRequired:
		return nil, fmt.Errorf("%w (HTTP 407)", ErrUpstreamAuth)
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return nil, fmt.Errorf("%w (HTTP %d)", ErrTargetRefused, resp.StatusCode)
	}
	return &bufferedConn{Conn: conn, r: br}, nil
}

func splitTarget(target string) (string, int, error) {
	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		return "", 0, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("bad port in %q", target)
	}
	return host, port, nil
}

func socks5Connect(conn net.Conn, u Upstream, target string) error {
	host, port, err := splitTarget(target)
	if err != nil {
		return err
	}
	method := byte(0)
	if u.HasAuth {
		method = 2
	}
	if _, err := conn.Write([]byte{5, 1, method}); err != nil {
		return err
	}
	reply := make([]byte, 2)
	if _, err := io.ReadFull(conn, reply); err != nil {
		return fmt.Errorf("socks5 greeting: %w", err)
	}
	version, chosen := reply[0], reply[1]
	if version != 5 || chosen != method {
		if chosen == 0xff && u.HasAuth {
			return fmt.Errorf("%w (socks5: user/pass not accepted)", ErrUpstreamAuth)
		}
		return fmt.Errorf("socks5: unexpected method 0x%02x", chosen)
	}
	if u.HasAuth {
		if len(u.Username) > 255 || len(u.Password) > 255 {
			return errors.New("socks5: username or password longer than 255 bytes")
		}
		msg := []byte{1, byte(len(u.Username))}
		msg = append(msg, u.Username...)
		msg = append(msg, byte(len(u.Password)))
		msg = append(msg, u.Password...)
		if _, err := conn.Write(msg); err != nil {
			return err
		}
		if _, err := io.ReadFull(conn, reply); err != nil {
			return fmt.Errorf("socks5 auth: %w", err)
		}
		if reply[1] != 0 {
			return fmt.Errorf("%w (socks5)", ErrUpstreamAuth)
		}
	}
	req := []byte{5, 1, 0}
	if ip := net.ParseIP(host); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			req = append(req, 1)
			req = append(req, ip4...)
		} else {
			req = append(req, 4)
			req = append(req, ip.To16()...)
		}
	} else {
		if len(host) > 255 {
			return errors.New("socks5: hostname too long")
		}
		req = append(req, 3, byte(len(host)))
		req = append(req, host...)
	}
	req = binary.BigEndian.AppendUint16(req, uint16(port)) //nolint:gosec // port range checked in splitTarget
	if _, err := conn.Write(req); err != nil {
		return err
	}
	var head [4]byte
	if _, err := io.ReadFull(conn, head[:]); err != nil {
		return fmt.Errorf("socks5 reply: %w", err)
	}
	if head[1] != 0 {
		return fmt.Errorf("%w (socks5 code %d)", ErrTargetRefused, head[1])
	}
	var skip int
	switch head[3] {
	case 1:
		skip = 4
	case 4:
		skip = 16
	case 3:
		var l [1]byte
		if _, err := io.ReadFull(conn, l[:]); err != nil {
			return err
		}
		skip = int(l[0])
	default:
		return fmt.Errorf("socks5: bad address type %d", head[3])
	}
	_, err = io.ReadFull(conn, make([]byte, skip+2))
	return err
}

func socks4Connect(conn net.Conn, u Upstream, target string) error {
	host, port, err := splitTarget(target)
	if err != nil {
		return err
	}
	req := []byte{4, 1}
	req = binary.BigEndian.AppendUint16(req, uint16(port)) //nolint:gosec // port range checked in splitTarget
	ip := net.ParseIP(host).To4()
	hostname := ip == nil
	if hostname {
		ip = net.IPv4(0, 0, 0, 1).To4() // SOCKS4a: the proxy resolves the name
	}
	req = append(req, ip...)
	req = append(req, u.Username...)
	req = append(req, 0)
	if hostname {
		req = append(req, host...)
		req = append(req, 0)
	}
	if _, err := conn.Write(req); err != nil {
		return err
	}
	resp := make([]byte, 8)
	if _, err := io.ReadFull(conn, resp); err != nil {
		return fmt.Errorf("socks4 reply: %w", err)
	}
	if code := resp[1]; code != 0x5a {
		return fmt.Errorf("%w (socks4 code 0x%02x)", ErrTargetRefused, code)
	}
	return nil
}
