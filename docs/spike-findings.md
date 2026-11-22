# Spike: how cloudflared applies a generated tunnel configuration

Date: 2026-11-21. `cloudflared` source: `cloudflare/cloudflared` release
2026.11.1. Method: feed remote configurations of the shape the controller
generates through `cloudflared`'s own remote-configuration parser
(`ingress.RemoteConfig`, the code path a remotely-managed tunnel uses when
Cloudflare pushes a new configuration), then match requests with its rule
matcher and send requests through its origin proxies to local test origins.
The test file is
[`hack/spike/ingress_spike_test.go.txt`](../hack/spike/ingress_spike_test.go.txt);
drop it into `ingress/` of a `cloudflared` checkout and run
`go test ./ingress -run TestSpike -v`.

Not covered: Cloudflare's API and edge (needs an account). Whether the
Cloudflare API accepts every value below is noted where it matters.

## Results

| Case | Result |
| --- | --- |
| `Exact` as `^<escaped>$` | `/exact` matches; `/exact/` does not |
| `PathPrefix` as `^<escaped>(?:/.*)?$` | `/api`, `/api/`, `/api/v1` match; `/apix` does not, so whole segments match as Gateway API asks |
| Regex metacharacters in a path (`/a.b`) | Escaped values match literally (`/aXb` does not match) |
| Rule order | Top to bottom, first match wins; a rule without a path matches every path |
| Query strings | Not part of the matched path |
| `*.example.com` | Matches `b.example.com` and `x.y.example.com`, not `example.com` |
| Wildcard not leading (`a.*.example.com`) | Configuration rejected |
| Catch-all missing or not last | Configuration rejected |
| `tcp://<host>:<port>` service | Accepted (served through `cloudflared access tcp`) |
| `httpHostHeader` | Origin sees the configured `Host`, original host in `X-Forwarded-Host` |
| Default origin settings, 20 sequential requests | 1 origin connection (idle pool reused) |
| `keepAliveConnections: 0` | 1 connection: **does not disable reuse** (Go's per-host idle default applies) |
| `keepAliveConnections: 1` | 1 connection |
| `keepAliveConnections: -1` | 20 connections: no idle pool, each request opens a connection |
| `http2Origin` with an `http://` origin that supports h2c | Origin sees **HTTP/1.1**: `cloudflared` never speaks h2c |
| `http2Origin` with an `https://` origin | Origin sees HTTP/2 |
| `https://` origin without `http2Origin` | HTTP/1.1 |

## Conclusions for the implementation

1. **Path translation works as designed** with the regular expressions
   above, and rule order must follow Gateway API precedence because the
   first match wins.
2. **Disabling connection reuse needs `keepAliveConnections: -1`**, not 0.
   Durations in the remote configuration are whole seconds, so a short
   `keepAliveTimeout` cannot do it either. The Cloudflare API documents the
   field as an integer without a minimum; if an account rejects -1, the
   controller reports the API error on the Gateway.
3. **`cloudflared` cannot reach h2c origins.** `kubernetes.io/h2c` therefore
   maps to a plain HTTP/1.1 origin, and GRPCRoute backends must serve TLS
   (`appProtocol: https`) so that `http2Origin` gives HTTP/2. A GRPCRoute
   with a cleartext backend is reported instead of being published broken.
4. Wildcard hostnames, the 404 catch-all and `tcp://` origins are accepted
   as generated.
