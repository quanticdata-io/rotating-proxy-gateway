# rotating-proxy-gateway

[![CI](https://github.com/quanticdata-io/rotating-proxy-gateway/actions/workflows/ci.yml/badge.svg)](https://github.com/quanticdata-io/rotating-proxy-gateway/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

A local proxy that turns a list of upstream proxies into **one endpoint** on your machine or in a container. Point any tool at `http://127.0.0.1:8080` and each new connection leaves through the next upstream, with sticky sessions when you ask for them, failover when an upstream breaks, and backoff before it is used again.

Use it when:

- a tool cannot authenticate to a proxy (Chrome's `--proxy-server`, many CLIs, some Selenium setups): the gateway holds the credentials;
- a tool takes only one proxy setting but you want to rotate through a list;
- several workers, containers or machines should share one rotation and one view of which upstreams are failing;
- you want to swap providers or plans without touching the code that uses the proxy.

Upstreams can be HTTP, HTTPS (TLS to the proxy), SOCKS4/4a or SOCKS5 proxies, with or without credentials, from any provider. It is a single static binary with no dependencies outside the Go standard library.

## Install

From source (Go 1.22 or later):

```sh
go install github.com/quanticdata-io/rotating-proxy-gateway/cmd/rotating-proxy-gateway@latest
```

Or build the container image from this repository (it is not published to a registry):

```sh
git clone https://github.com/quanticdata-io/rotating-proxy-gateway && cd rotating-proxy-gateway
docker build -t rotating-proxy-gateway .
```

## Quickstart

```sh
rotating-proxy-gateway -f proxies.txt
```

```
level=INFO msg="proxy listening" addr=127.0.0.1:8080 upstreams=25 strategy=round-robin
level=INFO msg="status endpoint" url=http://127.0.0.1:8081/status
```

Then use it like any HTTP proxy, with no credentials:

```sh
curl -x http://127.0.0.1:8080 https://api.quanticdata.io/v1/public/ip-echo
chromium --proxy-server=http://127.0.0.1:8080
export HTTPS_PROXY=http://127.0.0.1:8080 HTTP_PROXY=http://127.0.0.1:8080
```

With Docker, publish the port on loopback only, or require credentials:

```sh
docker run --rm -p 127.0.0.1:8080:8080 -p 127.0.0.1:8081:8081 \
  -v "$PWD/proxies.txt:/proxies.txt:ro" rotating-proxy-gateway -f /proxies.txt

docker run --rm -p 8080:8080 -e GATEWAY_AUTH=me:long-random-password \
  -v "$PWD/proxies.txt:/proxies.txt:ro" rotating-proxy-gateway -f /proxies.txt
```

[`examples/docker-compose.yml`](examples/docker-compose.yml) runs it next to a scraper container.

## Upstream list

One proxy per line, as arguments or in a file (`-f`); blank lines and `#` comments are skipped.

| Format | Example |
|---|---|
| `host:port` | `203.0.113.7:3128` |
| `user:pass@host:port` | `user:pass@gw.example.net:8080` |
| `host:port:user:pass` | `gw.example.net:8080:user:pass` |
| `scheme://[user:pass@]host:port` | `socks5://user:pass@198.51.100.4:1080` |

Schemes: `http`, `https`, `socks4`, `socks4a`, `socks5`, `socks5h` (the last two are the same here: the upstream always resolves the target name). Lines without a scheme use `--scheme` (default `http`). Credentials may be percent-encoded.

## Rotation and sessions

Every CONNECT tunnel (HTTPS) and every plain-HTTP request picks an upstream with `--strategy`:

| Strategy | Picks |
|---|---|
| `round-robin` (default) | the next upstream in list order |
| `random` | a random usable upstream |
| `least-conn` | the upstream with the fewest open connections |

Connections to upstreams are never reused (no keep-alive), so an upstream that rotates per connection, like most residential and mobile rotating gateways, rotates through this gateway too.

**Sticky sessions.** Put a session id in the proxy username and every connection with that id uses the same upstream until it fails or `--sticky-ttl` passes:

```sh
curl -x http://session-cart42:x@127.0.0.1:8080 https://shop.example/cart    # always the same upstream
```

| How the client names a session | Example |
|---|---|
| Username `session-<id>` (no `--auth`) | `http://session-acct7:x@127.0.0.1:8080` |
| Username `<user>-session-<id>` (with `--auth user:pass`) | `http://me-session-acct7:pass@127.0.0.1:8080` |
| Header `X-Gateway-Session: <id>` (plain HTTP only) | `curl -H "X-Gateway-Session: acct7" ...` |
| `--sticky-client-ip` | each client address is its own session |

A session moves to another upstream when its upstream fails. Sessions live in memory and are lost on restart.

## Failures and backoff

When an upstream cannot be reached, refuses its credentials (407 or SOCKS auth failure) or times out, the gateway tries another one, up to `--retries` (default 2) for the same client request, then answers **502** (or **504** on a timeout). The failed upstream rests for `--cooldown` (30s), doubled after each failure in a row up to `--max-cooldown` (10m); after `--max-failures` (5) failures in a row it is dead until a health check reaches it again or `POST /revive` is called. When nothing is usable, clients get **503**.

When the upstream works but cannot reach the target (CONNECT answered 403, SOCKS "host unreachable"), the upstream is not punished and the error is passed to the client.

`--health-interval 1m` opens one TCP connection through every upstream to `--health-target` (default `api.quanticdata.io:443`) on that interval; this uses a few bytes of each plan's traffic. It is off by default, so upstreams are judged only by real traffic.

## Status endpoint

`--admin` (default `127.0.0.1:8081`; empty to disable):

| Route | Answer |
|---|---|
| `GET /status` | JSON: version, uptime, request and failure counters, live sessions, and per upstream its state (`available`, `cooling`, `dead`), open connections, uses, successes, failures, last error. Passwords are masked |
| `GET /healthz` | `200 ok` when at least one upstream is usable, `503` otherwise (for container health checks) |
| `POST /revive` | clears every cooldown and dead flag |

## All flags

```
--listen ADDR            proxy port (127.0.0.1:8080)
--admin ADDR             status endpoint (127.0.0.1:8081, "" disables)
--auth USER:PASS         require credentials from clients (or GATEWAY_AUTH)
-f FILE                  upstream list
--scheme S               scheme for lines without one (http)
--strategy S             round-robin | random | least-conn
--sticky-ttl D           session lifetime (10m, 0 = until the upstream fails)
--sticky-client-ip       one session per client address
--cooldown D             first rest after a failure (30s)
--max-cooldown D         longest rest (10m)
--max-failures N         failures in a row before dead (5, 0 = never)
--retries N              other upstreams tried per request (2)
--timeout D              connect timeout through an upstream (15s)
--health-interval D      active health checks (0 = off)
--health-target H:P      what health checks connect to (api.quanticdata.io:443)
--insecure               skip TLS verification of https:// upstreams
--log LEVEL              debug | info | warn | error
--version
```

QuanticData flags are listed in the next section.

## QuanticData proxies

With a QuanticData residential, mobile, ISP, datacenter or IPv6 plan, the gateway can fetch its upstreams from your plan at start-up through the documented [`POST /public/proxies/generate`](https://quanticdata.io/docs/) endpoint. The strings come back with targeting and session already applied and are used as returned.

```sh
export QUANTICDATA_API_KEY=qd_live_...          # dashboard, API keys
rotating-proxy-gateway --qd-list-plans          # order ids and plan types
rotating-proxy-gateway --qd-order ORDER_ID --qd-country us --qd-quantity 20 \
  --qd-rotation sticky --qd-session-time 30     # 20 sticky US sessions, 30 minutes each
```

| Flag | Field of `generate` |
|---|---|
| `--qd-order` | `orderId` |
| `--qd-quantity` | `quantity` (1 to 10000, default 10) |
| `--qd-protocol` | `protocol` (`http` or `socks5`) |
| `--qd-country`, `--qd-state`, `--qd-city` | `country`, `state`, `city` |
| `--qd-rotation` | `rotation` (`rotating`, `sticky`, `static`) |
| `--qd-session-time` | `sessionTime` (minutes) |
| `--qd-gateway` | `gateway` (`ww`, `us`, `eu`, `as`, on the plan types that take it) |

Which fields apply depends on the plan type, as the [API reference](https://quanticdata.io/docs/) lists. Generating strings is free; traffic through them is billed by the plan as usual. A pool of sticky strings gives you one exit IP per gateway session; a single rotating string gives you a new IP per connection with no list at all. `QUANTICDATA_API_BASE` overrides the API host. Plans: [quanticdata.io/pricing](https://quanticdata.io/pricing/).

## Limits

- Clients talk HTTP proxy to the gateway (plain HTTP and CONNECT). There is no SOCKS listener, and no UDP.
- HTTPS goes through a tunnel the gateway cannot read, so it cannot see a 403 or a captcha from the target and cannot retry on blocks. Ban handling belongs in the client (for Python, see [proxy-pool](https://github.com/quanticdata-io/proxy-pool)).
- A plain-HTTP request with a body is sent once: it is not replayed through another upstream after a failure.
- State (sessions, counters, cooldowns) is in memory and per process.
- `--auth` is basic proxy authentication over plain TCP. Expose the port only on networks you trust, or behind a VPN or SSH tunnel.

## Develop

```sh
go vet ./... && go test ./...
go run ./examples/locallab      # an origin and three upstreams on 127.0.0.1, to try the gateway offline
```

Tests run against in-process upstreams (HTTP, CONNECT and SOCKS5) and origins on 127.0.0.1; they never use the network. See [CONTRIBUTING.md](CONTRIBUTING.md).

## Responsible use

Use upstream proxies you own, rent or are allowed to use, and follow the terms of the sites you reach through them.

## License

[MIT](LICENSE)
