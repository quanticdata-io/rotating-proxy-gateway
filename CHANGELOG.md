# Changelog

All notable changes are recorded here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses [Semantic Versioning](https://semver.org/).

## [Unreleased]

Planned, not implemented yet:

- A SOCKS5 listener for clients.
- Reloading the upstream list without a restart.
- Refreshing QuanticData sticky strings on a schedule.

## [0.1.0] - 2026-11-18

### Added

- Local HTTP proxy (plain HTTP and CONNECT) that forwards through HTTP, HTTPS, SOCKS4/4a and SOCKS5 upstreams with or without credentials.
- Round-robin, random and least-connections strategies; sticky sessions from the proxy username, a header or the client address, with a TTL.
- Failover to other upstreams, exponential cooldown, dead upstreams, optional active health checks.
- Optional client authentication, status, health and revive endpoints.
- QuanticData upstreams generated from a plan at start-up.
- Dockerfile, a docker compose example and an offline local lab.

[Unreleased]: https://github.com/quanticdata-io/rotating-proxy-gateway/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/quanticdata-io/rotating-proxy-gateway/releases/tag/v0.1.0
