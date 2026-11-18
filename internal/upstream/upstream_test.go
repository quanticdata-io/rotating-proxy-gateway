package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/quanticdata-io/rotating-proxy-gateway/internal/testutil"
)

func TestParse(t *testing.T) {
	cases := []struct {
		in, url string
		scheme  Scheme
	}{
		{"203.0.113.7:3128", "http://203.0.113.7:3128", HTTP},
		{"user:pass@gw.example.net:8080", "http://user:pass@gw.example.net:8080", HTTP},
		{"gw.example.net:8080:user:pa:ss", "http://user:pa%3Ass@gw.example.net:8080", HTTP},
		{"socks5h://u:p@198.51.100.4:1080", "socks5://u:p@198.51.100.4:1080", SOCKS5},
		{"socks4a://198.51.100.4:1080", "socks4://198.51.100.4:1080", SOCKS4},
		{"https://u:p%40x@gw.example.net:443/", "https://u:p%40x@gw.example.net:443", HTTPS},
		{"http://[2001:db8::1]:8080", "http://[2001:db8::1]:8080", HTTP},
	}
	for _, c := range cases {
		u, err := Parse(c.in, HTTP)
		if err != nil {
			t.Fatalf("%s: %v", c.in, err)
		}
		if got := u.URL().String(); got != c.url || u.Scheme != c.scheme {
			t.Errorf("%s: got %s (%s), want %s", c.in, got, u.Scheme, c.url)
		}
	}
}

func TestParseErrorsAndRedaction(t *testing.T) {
	for _, bad := range []string{"", "ftp://h:1", "nohost", "h:0", "user:topsecret@h:notaport"} {
		_, err := Parse(bad, HTTP)
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("%q: want ErrInvalid, got %v", bad, err)
		}
		if err != nil && strings.Contains(err.Error(), "topsecret") {
			t.Errorf("error leaks the password: %v", err)
		}
	}
	u, _ := Parse("user:secret@h:1", HTTP)
	if strings.Contains(u.Redacted(), "secret") {
		t.Fatalf("Redacted leaks the password: %s", u.Redacted())
	}
}

func TestReadList(t *testing.T) {
	list, err := ReadList(strings.NewReader("# c\n\nh1:1\nsocks5://h2:2\n"), HTTP)
	if err != nil || len(list) != 2 || list[1].Scheme != SOCKS5 {
		t.Fatalf("got %v %v", list, err)
	}
	if _, err := ReadList(strings.NewReader("h1:1\nbad\n"), HTTP); err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("want a line number, got %v", err)
	}
}

func get(t *testing.T, conn io.ReadWriter, host string) string {
	t.Helper()
	if _, err := io.WriteString(conn, "GET /x HTTP/1.1\r\nHost: "+host+"\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(conn)
	return string(b)
}

func TestDialHTTPAndSOCKS5(t *testing.T) {
	origin := httptest.NewServer(testutil.Origin())
	defer origin.Close()
	target := strings.TrimPrefix(origin.URL, "http://")

	hp := &testutil.HTTPProxy{Name: "h", Username: "u", Password: "p:w"}
	hs := httptest.NewServer(hp)
	defer hs.Close()
	socks, err := testutil.ListenSOCKS5("su", "sp")
	if err != nil {
		t.Fatal(err)
	}
	defer socks.Close()

	d := Dialer{Timeout: 5 * time.Second}
	for _, line := range []string{
		"http://u:p%3Aw@" + strings.TrimPrefix(hs.URL, "http://"),
		"socks5://su:sp@" + socks.Addr(),
	} {
		u, err := Parse(line, HTTP)
		if err != nil {
			t.Fatal(err)
		}
		conn, err := d.Dial(context.Background(), u, target)
		if err != nil {
			t.Fatalf("%s: %v", u.Redacted(), err)
		}
		if body := get(t, conn, target); !strings.Contains(body, "direct GET /x") {
			t.Fatalf("%s: unexpected answer %q", u.Redacted(), body)
		}
		conn.Close()
	}
	if hp.Connects.Load() != 1 || socks.Connects.Load() != 1 {
		t.Fatalf("connects: http %d socks %d", hp.Connects.Load(), socks.Connects.Load())
	}
}

func TestDialErrors(t *testing.T) {
	hs := httptest.NewServer(&testutil.HTTPProxy{Username: "u", Password: "right"})
	defer hs.Close()
	d := Dialer{Timeout: 5 * time.Second}
	wrong, _ := Parse("u:wrong@"+strings.TrimPrefix(hs.URL, "http://"), HTTP)
	if _, err := d.Dial(context.Background(), wrong, "127.0.0.1:9"); !errors.Is(err, ErrUpstreamAuth) {
		t.Fatalf("want ErrUpstreamAuth, got %v", err)
	}
	right, _ := Parse("u:right@"+strings.TrimPrefix(hs.URL, "http://"), HTTP)
	addr, stop := testutil.ClosedPort()
	stop() // nothing listens there now
	if _, err := d.Dial(context.Background(), right, addr); !errors.Is(err, ErrTargetRefused) {
		t.Fatalf("want ErrTargetRefused, got %v", err)
	}
	socks, _ := testutil.ListenSOCKS5("su", "sp")
	defer socks.Close()
	bad, _ := Parse("socks5://su:no@"+socks.Addr(), HTTP)
	if _, err := d.Dial(context.Background(), bad, "127.0.0.1:9"); !errors.Is(err, ErrUpstreamAuth) {
		t.Fatalf("socks: want ErrUpstreamAuth, got %v", err)
	}
	status := httptest.NewServer(&testutil.HTTPProxy{Status: http.StatusForbidden})
	defer status.Close()
	forbidden, _ := Parse(strings.TrimPrefix(status.URL, "http://"), HTTP)
	if _, err := d.Dial(context.Background(), forbidden, "127.0.0.1:9"); !errors.Is(err, ErrTargetRefused) {
		t.Fatalf("403 CONNECT: want ErrTargetRefused, got %v", err)
	}
}
