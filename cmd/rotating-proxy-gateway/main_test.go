package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestVersionAndUsage(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"-version"}, &out, &errb, env(nil)); code != 0 || !strings.HasPrefix(out.String(), "rotating-proxy-gateway") {
		t.Fatalf("version: %d %q", code, out.String())
	}
	if code := run(context.Background(), []string{"-nope"}, &out, &errb, env(nil)); code != 2 {
		t.Fatalf("bad flag: %d", code)
	}
	errb.Reset()
	if code := run(context.Background(), nil, &out, &errb, env(nil)); code != 2 || !strings.Contains(errb.String(), "no upstream proxies") {
		t.Fatalf("no upstreams: %d %q", code, errb.String())
	}
}

func TestLoadUpstreamsFromArgsAndFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "list.txt")
	if err := os.WriteFile(path, []byte("# list\nh2:2\nsocks5://u:p@h3:3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	o, rest, err := parseFlags([]string{"-f", path, "-scheme", "socks5", "h1:1"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	ups, err := loadUpstreams(context.Background(), o, rest, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(ups) != 3 || ups[0].Scheme != "socks5" || ups[0].Host != "h1" || ups[2].Username != "u" {
		t.Fatalf("%+v", ups)
	}
	o.scheme = "ftp"
	if _, err := loadUpstreams(context.Background(), o, rest, env(nil)); err == nil {
		t.Fatal("want an error for an unknown scheme")
	}
}

func fakeAPI(t *testing.T) (*httptest.Server, *map[string]any) {
	t.Helper()
	var last map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer qd_test_key" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"type":"error","message":"bad key"}`)
			return
		}
		switch r.URL.Path {
		case "/v1/public/proxies/generate":
			_ = json.NewDecoder(r.Body).Decode(&last)
			_, _ = io.WriteString(w, `{"type":"response","payload":{"proxies":["http://user-country-us-sid-1:pw@gw.example.net:8080","http://user-country-us-sid-2:pw@gw.example.net:8080"]}}`)
		case "/v1/public/proxies":
			_, _ = io.WriteString(w, `{"type":"response","payload":{"proxies":[{"orderId":"ord_1","planType":"mobile_v2","planName":"Mobile 5 GB","isActive":true}],"pagination":{"hasMore":false}}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &last
}

func TestQuanticDataUpstreams(t *testing.T) {
	srv, last := fakeAPI(t)
	o, rest, err := parseFlags([]string{"-qd-order", "ord_1", "-qd-country", "us", "-qd-rotation", "sticky", "-qd-session-time", "30", "-qd-quantity", "2"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	ups, err := loadUpstreams(context.Background(), o, rest, env(map[string]string{
		"QUANTICDATA_API_KEY": "qd_test_key", "QUANTICDATA_API_BASE": srv.URL + "/v1",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(ups) != 2 || ups[0].Username != "user-country-us-sid-1" {
		t.Fatalf("%+v", ups)
	}
	body := *last
	if body["orderId"] != "ord_1" || body["country"] != "us" || body["rotation"] != "sticky" || body["sessionTime"] != float64(30) {
		t.Fatalf("request body %v", body)
	}
}

func TestListPlans(t *testing.T) {
	srv, _ := fakeAPI(t)
	var out, errb bytes.Buffer
	code := run(context.Background(), []string{"-qd-list-plans"}, &out, &errb, env(map[string]string{
		"QUANTICDATA_API_KEY": "qd_test_key", "QUANTICDATA_API_BASE": srv.URL + "/v1",
	}))
	if code != 0 || !strings.Contains(out.String(), "ord_1") || !strings.Contains(out.String(), "mobile_v2") {
		t.Fatalf("%d %q %q", code, out.String(), errb.String())
	}
	if code := run(context.Background(), []string{"-qd-list-plans"}, &out, &errb, env(nil)); code != 2 {
		t.Fatalf("missing key: %d", code)
	}
}

func TestAuthFlagValidation(t *testing.T) {
	var out, errb bytes.Buffer
	code := run(context.Background(), []string{"-auth", "me-session-x:pw", "h1:1"}, &out, &errb, env(nil))
	if code != 2 || !strings.Contains(errb.String(), "-session-") {
		t.Fatalf("%d %q", code, errb.String())
	}
}

func TestIsLoopback(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:8080": true, "localhost:1": true, "[::1]:8080": true, "0.0.0.0:8080": false, ":8080": false,
	} {
		if got := isLoopback(addr); got != want {
			t.Errorf("%s: got %v", addr, got)
		}
	}
}
