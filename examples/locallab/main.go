// Command locallab starts an origin and three upstream proxies on 127.0.0.1,
// so the gateway can be tried with no network and no provider account:
//
//	go run ./examples/locallab
//	go run ./cmd/rotating-proxy-gateway -f examples/locallab/upstreams.txt
//	curl -x http://127.0.0.1:8080 http://127.0.0.1:18090/hello   # "a GET /hello", then b, then c
//	curl -x http://session-x:y@127.0.0.1:8080 http://127.0.0.1:18090/hello   # always the same letter
package main

import (
	"log"
	"net/http"
	"time"

	"github.com/quanticdata-io/rotating-proxy-gateway/internal/testutil"
)

func serve(addr string, h http.Handler) {
	srv := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 10 * time.Second}
	go func() { log.Fatal(srv.ListenAndServe()) }()
}

func main() {
	serve("127.0.0.1:18090", testutil.Origin())
	serve("127.0.0.1:18091", &testutil.HTTPProxy{Name: "a"})
	serve("127.0.0.1:18092", &testutil.HTTPProxy{Name: "b"})
	serve("127.0.0.1:18093", &testutil.HTTPProxy{Name: "c", Username: "demo", Password: "demo"})
	log.Println("origin http://127.0.0.1:18090, upstreams 18091 (a), 18092 (b), 18093 (c, demo:demo)")
	select {}
}
