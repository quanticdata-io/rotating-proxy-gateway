// Command rotating-proxy-gateway is a local HTTP proxy that forwards each
// connection through an upstream proxy from a list, with rotation, sticky
// sessions and failover.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/quanticdata-io/rotating-proxy-gateway/internal/gateway"
	"github.com/quanticdata-io/rotating-proxy-gateway/internal/pool"
	"github.com/quanticdata-io/rotating-proxy-gateway/internal/quanticdata"
	"github.com/quanticdata-io/rotating-proxy-gateway/internal/upstream"
)

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr, os.Getenv))
}

type options struct {
	listen, admin, auth, file, scheme, strategy, healthTarget, logLevel string
	stickyTTL, cooldown, maxCooldown, timeout, healthInterval           time.Duration
	maxFailures, retries                                                int
	stickyIP, insecure, version, listPlans                              bool

	qdOrder, qdCountry, qdState, qdCity, qdRotation, qdProtocol, qdGateway string
	qdQuantity, qdSessionTime                                              int
}

func parseFlags(args []string, stderr io.Writer) (*options, []string, error) {
	o := &options{}
	fs := flag.NewFlagSet("rotating-proxy-gateway", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: rotating-proxy-gateway [flags] [upstream ...]")
		fmt.Fprintln(stderr, "\nUpstreams come from arguments, -f FILE, or --qd-order. Flags:")
		fs.PrintDefaults()
	}
	fs.StringVar(&o.listen, "listen", "127.0.0.1:8080", "address of the proxy port")
	fs.StringVar(&o.admin, "admin", "127.0.0.1:8081", "address of the status endpoint (empty to disable)")
	fs.StringVar(&o.auth, "auth", "", "require user:pass from clients (or set GATEWAY_AUTH)")
	fs.StringVar(&o.file, "f", "", "file with one upstream proxy per line")
	fs.StringVar(&o.scheme, "scheme", "http", "scheme for upstream lines without one: http, https, socks4, socks5")
	fs.StringVar(&o.strategy, "strategy", "round-robin", "round-robin, random or least-conn")
	fs.DurationVar(&o.stickyTTL, "sticky-ttl", 10*time.Minute, "how long a session keeps its upstream (0 = until it fails)")
	fs.BoolVar(&o.stickyIP, "sticky-client-ip", false, "give each client address its own session when it sends none")
	fs.DurationVar(&o.cooldown, "cooldown", 30*time.Second, "rest after an upstream fails, doubled per failure in a row")
	fs.DurationVar(&o.maxCooldown, "max-cooldown", 10*time.Minute, "upper bound of the cooldown")
	fs.IntVar(&o.maxFailures, "max-failures", 5, "failures in a row before an upstream is dead (0 = never)")
	fs.IntVar(&o.retries, "retries", 2, "other upstreams to try when one fails to connect")
	fs.DurationVar(&o.timeout, "timeout", 15*time.Second, "timeout for connecting through an upstream")
	fs.DurationVar(&o.healthInterval, "health-interval", 0, "check every upstream on this interval (0 = off)")
	fs.StringVar(&o.healthTarget, "health-target", "api.quanticdata.io:443", "host:port the health check connects to through each upstream")
	fs.BoolVar(&o.insecure, "insecure", false, "skip TLS verification of https:// upstreams")
	fs.StringVar(&o.logLevel, "log", "info", "log level: debug, info, warn, error")
	fs.BoolVar(&o.version, "version", false, "print the version and exit")

	fs.StringVar(&o.qdOrder, "qd-order", "", "QuanticData order id: generate upstreams from this plan (needs QUANTICDATA_API_KEY)")
	fs.BoolVar(&o.listPlans, "qd-list-plans", false, "list the QuanticData proxy plans of the account and exit")
	fs.IntVar(&o.qdQuantity, "qd-quantity", 10, "QuanticData: number of proxy strings (1-10000)")
	fs.StringVar(&o.qdCountry, "qd-country", "", "QuanticData: country code, e.g. us")
	fs.StringVar(&o.qdState, "qd-state", "", "QuanticData: state or region slug")
	fs.StringVar(&o.qdCity, "qd-city", "", "QuanticData: city slug")
	fs.StringVar(&o.qdRotation, "qd-rotation", "", "QuanticData: rotating, sticky or static")
	fs.IntVar(&o.qdSessionTime, "qd-session-time", 0, "QuanticData: sticky session minutes")
	fs.StringVar(&o.qdProtocol, "qd-protocol", "http", "QuanticData: http or socks5")
	fs.StringVar(&o.qdGateway, "qd-gateway", "", "QuanticData: entry gateway ww, us, eu or as (plan types that support it)")
	if err := fs.Parse(args); err != nil {
		return nil, nil, err
	}
	return o, fs.Args(), nil
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	o, rest, err := parseFlags(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if o.version {
		fmt.Fprintln(stdout, "rotating-proxy-gateway", gateway.Version)
		return 0
	}
	level := slog.LevelInfo
	if err := level.UnmarshalText([]byte(o.logLevel)); err != nil {
		fmt.Fprintln(stderr, "bad --log level:", o.logLevel)
		return 2
	}
	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: level}))

	if o.listPlans {
		return listPlans(ctx, stdout, stderr, getenv)
	}
	ups, err := loadUpstreams(ctx, o, rest, getenv)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 2
	}
	strategy, err := pool.ParseStrategy(o.strategy)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 2
	}
	p, err := pool.New(ups, pool.Options{
		Strategy: strategy, StickyTTL: o.stickyTTL, Cooldown: o.cooldown,
		MaxCooldown: o.maxCooldown, MaxFailures: o.maxFailures,
	})
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 2
	}
	auth := o.auth
	if auth == "" {
		auth = getenv("GATEWAY_AUTH")
	}
	cfg := gateway.Config{Retries: o.retries, Timeout: o.timeout, StickyByClientIP: o.stickyIP, Insecure: o.insecure, Logger: log}
	if auth != "" {
		user, pass, ok := strings.Cut(auth, ":")
		if !ok || user == "" || strings.Contains(user, "-session-") {
			fmt.Fprintln(stderr, "error: --auth must be user:pass, and the user must not contain -session-")
			return 2
		}
		cfg.Username, cfg.Password = user, pass
	}
	if auth == "" && !isLoopback(o.listen) {
		log.Warn("listening beyond localhost without --auth: anyone who can reach this port can use your upstream proxies", "listen", o.listen)
	}
	gw := gateway.New(p, cfg)

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	ln, err := net.Listen("tcp", o.listen)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 3
	}
	srv := &http.Server{Handler: gw, ReadHeaderTimeout: 30 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	log.Info("proxy listening", "addr", ln.Addr().String(), "upstreams", p.Len(), "strategy", strategy)

	var adminSrv *http.Server
	if o.admin != "" {
		aln, err := net.Listen("tcp", o.admin)
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 3
		}
		adminSrv = &http.Server{Handler: gw.AdminHandler(), ReadHeaderTimeout: 10 * time.Second}
		go func() { _ = adminSrv.Serve(aln) }()
		log.Info("status endpoint", "url", "http://"+aln.Addr().String()+"/status")
	}
	if o.healthInterval > 0 {
		go func() {
			t := time.NewTicker(o.healthInterval)
			defer t.Stop()
			gw.HealthCheck(ctx, o.healthTarget)
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					gw.HealthCheck(ctx, o.healthTarget)
				}
			}
		}()
	}
	<-ctx.Done()
	log.Info("shutting down")
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdown)
	if adminSrv != nil {
		_ = adminSrv.Shutdown(shutdown)
	}
	return 0
}

func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func loadUpstreams(ctx context.Context, o *options, args []string, getenv func(string) string) ([]upstream.Upstream, error) {
	def := upstream.Scheme(o.scheme)
	switch def {
	case upstream.HTTP, upstream.HTTPS, upstream.SOCKS4, upstream.SOCKS5:
	default:
		return nil, fmt.Errorf("unknown --scheme %q", o.scheme)
	}
	var out []upstream.Upstream
	for _, a := range args {
		u, err := upstream.Parse(a, def)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	if o.file != "" {
		f, err := os.Open(o.file)
		if err != nil {
			return nil, err
		}
		list, err := upstream.ReadList(f, def)
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", o.file, err)
		}
		out = append(out, list...)
	}
	if o.qdOrder != "" {
		client, err := quanticdata.NewFromEnvFunc(getenv)
		if err != nil {
			return nil, err
		}
		lines, err := client.Generate(ctx, quanticdata.GenerateRequest{
			OrderID: o.qdOrder, Protocol: o.qdProtocol, Quantity: o.qdQuantity,
			Country: o.qdCountry, State: o.qdState, City: o.qdCity,
			Rotation: o.qdRotation, SessionTime: o.qdSessionTime, Gateway: o.qdGateway,
		})
		if err != nil {
			return nil, err
		}
		for _, l := range lines {
			u, err := upstream.Parse(l, def)
			if err != nil {
				return nil, fmt.Errorf("QuanticData returned an unexpected line: %w", err)
			}
			out = append(out, u)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no upstream proxies: pass them as arguments, with -f FILE or with --qd-order")
	}
	return out, nil
}

func listPlans(ctx context.Context, stdout, stderr io.Writer, getenv func(string) string) int {
	client, err := quanticdata.NewFromEnvFunc(getenv)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 2
	}
	plans, err := client.ListPlans(ctx)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 3
	}
	for _, p := range plans {
		fmt.Fprintf(stdout, "%-24s %-20s %s\n", p.OrderID, p.PlanType, p.PlanName)
	}
	return 0
}
