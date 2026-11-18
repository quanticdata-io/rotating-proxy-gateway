# Security policy

## Supported versions

Security fixes are released for the latest minor version.

## Reporting a vulnerability

Please do not open a public issue for security problems. Email **hello@quanticdata.io** with "rotating-proxy-gateway security" in the subject, a description of the issue and, if possible, steps to reproduce. You will get an answer within 3 working days, and we will agree on a disclosure date with you once a fix is ready.

## Deployment notes

- The gateway forwards traffic for anyone who can reach its proxy port. It listens on 127.0.0.1 by default; when you listen on other interfaces, set `--auth` (or `GATEWAY_AUTH`) and restrict the port with a firewall. Basic proxy authentication is not encrypted.
- The status endpoint has no authentication: keep `--admin` on 127.0.0.1 or disable it with `--admin ""`.
- Upstream passwords are masked in logs and on the status page. The QuanticData flags read the API key only from `QUANTICDATA_API_KEY` and send it only to the API host (`QUANTICDATA_API_BASE`, default `https://api.quanticdata.io/v1`).

Report any path where a credential is printed, logged or sent elsewhere, or where a client can reach a destination without going through an upstream.
