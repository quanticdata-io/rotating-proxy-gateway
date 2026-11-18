// Package testutil has in-process servers for tests and the local demo:
// an origin, HTTP/CONNECT upstream proxies and a SOCKS5 upstream proxy.
// Everything listens on 127.0.0.1; nothing reaches the network.
package testutil

import (
	"bufio"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// HeaderUpstream is set by test upstreams on forwarded plain-HTTP requests,
// so the origin can tell which upstream a request came through.
const HeaderUpstream = "X-Test-Upstream"

// Origin answers every request with "<upstream> <method> <path>", where
// upstream is the X-Test-Upstream header ("direct" when absent). TLS origins
// can be built with httptest.NewTLSServer(Origin()).
func Origin() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		via := r.Header.Get(HeaderUpstream)
		if via == "" {
			via = "direct"
		}
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w, "%s %s %s", via, r.Method, r.URL.Path)
	})
}

// HTTPProxy is a forward proxy for absolute-form requests and CONNECT.
type HTTPProxy struct {
	Name               string
	Username, Password string
	// Status, when set, is returned for every request instead of proxying.
	Status   int
	Requests atomic.Int64
	Connects atomic.Int64
}

func (p *HTTPProxy) authorized(r *http.Request) bool {
	if p.Username == "" {
		return true
	}
	h := r.Header.Get("Proxy-Authorization")
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(h, "Basic "))
	return err == nil && string(raw) == p.Username+":"+p.Password
}

var forward = &http.Transport{Proxy: nil, DisableKeepAlives: true}

func (p *HTTPProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.Requests.Add(1)
	if !p.authorized(r) {
		w.Header().Set("Proxy-Authenticate", `Basic realm="test"`)
		w.WriteHeader(http.StatusProxyAuthRequired)
		return
	}
	if p.Status != 0 {
		w.WriteHeader(p.Status)
		return
	}
	if r.Method == http.MethodConnect {
		p.Connects.Add(1)
		p.tunnel(w, r)
		return
	}
	out := r.Clone(r.Context())
	out.RequestURI = ""
	out.Header.Del("Proxy-Authorization")
	out.Header.Set(HeaderUpstream, p.Name)
	resp, err := forward.RoundTrip(out)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (p *HTTPProxy) tunnel(w http.ResponseWriter, r *http.Request) {
	up, err := net.Dial("tcp", r.Host)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	client, buf, err := w.(http.Hijacker).Hijack()
	if err != nil {
		up.Close()
		return
	}
	_, _ = client.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
	if n := buf.Reader.Buffered(); n > 0 {
		pending, _ := buf.Peek(n)
		_, _ = up.Write(pending)
	}
	Pipe(client, up)
}

// Pipe copies both ways until both sides are done, then closes them.
func Pipe(a, b net.Conn) {
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

// SOCKS5 is a minimal SOCKS5 server (CONNECT, no auth or user/pass).
type SOCKS5 struct {
	Username, Password string
	Listener           net.Listener
	Connects           atomic.Int64
}

// ListenSOCKS5 starts a SOCKS5 server on a random 127.0.0.1 port.
func ListenSOCKS5(user, pass string) (*SOCKS5, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &SOCKS5{Username: user, Password: pass, Listener: ln}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.handle(c)
		}
	}()
	return s, nil
}

// Addr is host:port.
func (s *SOCKS5) Addr() string { return s.Listener.Addr().String() }

// Close stops the server.
func (s *SOCKS5) Close() error { return s.Listener.Close() }

type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufConn) Read(p []byte) (int, error) { return b.r.Read(p) }

func (s *SOCKS5) handle(c net.Conn) {
	br := bufio.NewReader(c)
	target, err := s.handshake(br, c)
	if err != nil {
		c.Close()
		return
	}
	up, err := net.Dial("tcp", target)
	if err != nil {
		_, _ = c.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
		c.Close()
		return
	}
	s.Connects.Add(1)
	_, _ = c.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0})
	Pipe(&bufConn{Conn: c, r: br}, up)
}

func (s *SOCKS5) handshake(br *bufio.Reader, c net.Conn) (string, error) {
	var head [2]byte
	if _, err := io.ReadFull(br, head[:]); err != nil || head[0] != 5 {
		return "", errors.New("not socks5")
	}
	methods := make([]byte, head[1])
	if _, err := io.ReadFull(br, methods); err != nil {
		return "", err
	}
	want := byte(0)
	if s.Username != "" {
		want = 2
	}
	if !strings.ContainsRune(string(methods), rune(want)) {
		_, _ = c.Write([]byte{5, 0xff})
		return "", errors.New("no acceptable method")
	}
	_, _ = c.Write([]byte{5, want})
	if want == 2 {
		ver, _ := br.ReadByte()
		ul, _ := br.ReadByte()
		u := make([]byte, ul)
		_, _ = io.ReadFull(br, u)
		pl, _ := br.ReadByte()
		pw := make([]byte, pl)
		_, _ = io.ReadFull(br, pw)
		if ver != 1 || string(u) != s.Username || string(pw) != s.Password {
			_, _ = c.Write([]byte{1, 1})
			return "", errors.New("bad credentials")
		}
		_, _ = c.Write([]byte{1, 0})
	}
	var req [4]byte
	if _, err := io.ReadFull(br, req[:]); err != nil || req[1] != 1 {
		return "", errors.New("only CONNECT")
	}
	var host string
	switch req[3] {
	case 1:
		b := make([]byte, 4)
		_, _ = io.ReadFull(br, b)
		host = net.IP(b).String()
	case 3:
		l, _ := br.ReadByte()
		b := make([]byte, l)
		_, _ = io.ReadFull(br, b)
		host = string(b)
	case 4:
		b := make([]byte, 16)
		_, _ = io.ReadFull(br, b)
		host = net.IP(b).String()
	default:
		return "", errors.New("bad address type")
	}
	var pb [2]byte
	if _, err := io.ReadFull(br, pb[:]); err != nil {
		return "", err
	}
	return net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(pb[:])))), nil
}

// ClosedPort returns a 127.0.0.1 address that accepts connections and closes
// them at once: a broken upstream that fails fast on every OS.
func ClosedPort() (string, func()) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	return ln.Addr().String(), func() { ln.Close() }
}
