# Contributing

Thanks for helping improve rotating-proxy-gateway.

## Development

Requirements: Go 1.22 or newer.

```sh
go build ./...
go vet ./...
go test ./...
go test -race ./...        # needs cgo (a C compiler); CI runs it on Linux and macOS
gofmt -l .                 # must print nothing
```

CI also runs `golangci-lint` v2.6 (with Go 1.25) using `.golangci.yml`, and builds the Docker image and smoke-tests it against a local upstream.

## Tests never use the network

Every test runs against in-process servers from `internal/testutil` (an origin, HTTP/CONNECT upstreams and a SOCKS5 upstream, all on 127.0.0.1). Do not add tests that reach the internet, a real proxy provider or a paid API. `go run ./examples/locallab` starts the same servers for manual testing.

## Ground rules

- Keep the binary dependency-free (standard library only) unless there is a strong reason; discuss it in an issue first.
- Never log or expose an upstream password: use `Upstream.Redacted()`.
- A client request must never leave without an upstream: when none is usable, answer 503.
- Keep the `/status` JSON backwards compatible: add fields, do not rename them.
- Update `CHANGELOG.md` under "Unreleased".
