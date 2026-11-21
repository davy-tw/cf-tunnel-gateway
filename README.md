# cf-tunnel-gateway

A Kubernetes controller that publishes in-cluster Services to the internet
through [Cloudflare Tunnel](https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/),
with no inbound port, NodePort or cloud load balancer. It serves three front
doors:

1. Gateway API `Gateway`s of a managed GatewayClass (HTTPRoute, GRPCRoute,
   TCPRoute)
2. `Ingress`es of the `cf-tunnel-gateway` IngressClass
3. `Service`s of `type: LoadBalancer` with
   `loadBalancerClass: cf-tunnel-gateway`

Ingresses and LoadBalancer Services are translated into Gateways and routes,
so all three share one data plane: per Gateway, one remotely-managed tunnel
whose configuration the controller writes, a `cloudflared` Deployment
running the stock `cloudflare/cloudflared` image, and proxied DNS records
pointing each hostname at the tunnel.

```
visitor ──> Cloudflare edge (TLS, demo.example.com CNAME <tunnel>.cfargotunnel.com)
              └─ tunnel ─> cloudflared pods (outbound only)
                             ├─> api.cf-demo.svc.cluster.local:8080 (/api)
                             └─> web.cf-demo.svc.cluster.local:80   (/)
```

## Requirements

- Kubernetes with the Gateway API **v1.6.0 or newer, Standard channel** CRDs
  (only `v1` APIs are used; tested with v1.6 and v1.7)
- A Cloudflare account with a zone, and API tokens
  ([setup](docs/setup.md))

## Install

With Helm (charts are published to GHCR as OCI artifacts only):

```sh
kubectl apply --server-side -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.6.3/standard-install.yaml
kubectl create namespace cf-tunnel-gateway-system
kubectl -n cf-tunnel-gateway-system create secret generic cf-tunnel-gateway \
  --from-literal=CLOUDFLARE_ACCOUNT_ID=... --from-literal=CLOUDFLARE_API_TOKEN=...
helm install cf-tunnel-gateway oci://ghcr.io/davy-tw/charts/cf-tunnel-gateway \
  --version <version> --namespace cf-tunnel-gateway-system \
  --set cloudflare.existingSecret=cf-tunnel-gateway
kubectl apply -f examples/demo.yaml   # after replacing example.com with your zone
```

Every setting in [Configuration](#configuration) has a value in
[`charts/cf-tunnel-gateway/values.yaml`](charts/cf-tunnel-gateway/values.yaml).
The chart also installs the `cf-tunnel-gateway` GatewayClass (pointing at the
same Secret) and IngressClass (`gatewayClass.create`, `ingressClass.create`,
`ingressClass.default`). It does not install the Gateway API CRDs. Builds of
`main` are published as prerelease chart versions `0.0.0-main.<commit>`.

Without Helm, `kubectl apply -f dist/install.yaml` installs the same objects
into `cf-tunnel-gateway-system`; create the Secret above and restart the
controller (`kubectl -n cf-tunnel-gateway-system rollout restart
deployment/cf-tunnel-gateway`), since settings are read at start-up.

Once the connector holds a connection, the Gateway reports
`Programmed=True` with the address `<tunnel-id>.cfargotunnel.com`, and its
routes report `DNSRecordsReady=True`.

## Configuration

The controller reads environment variables (loaded from the Secret
`cf-tunnel-gateway`, or set through the chart values).

| Variable | Default | Meaning |
| --- | --- | --- |
| `CLOUDFLARE_ACCOUNT_ID`, `CLOUDFLARE_API_TOKEN` | unset | Account credentials used by every GatewayClass that brings none |
| `INSTANCE_NAME` | `cf-tunnel-gateway` | Identifies the installation; the class names, shared Gateway name and DNS owner ID default to it |
| `GATEWAY_CONTROLLER_NAME` | `cf-tunnel-gateway.davy.tw/gateway-controller` | Every GatewayClass with this `controllerName` is served |
| `GATEWAY_CLASS`, `INGRESS_CLASS`, `LOAD_BALANCER_CLASS` | instance name | Class of generated Gateways, IngressClass and `loadBalancerClass` served |
| `INGRESS_ENABLED`, `LOAD_BALANCER_ENABLED` | `true` | Turn a front door off (its generated objects are deleted) |
| `SHARED_GATEWAY_SCOPE` | `namespace` | `namespace` (one shared Gateway per namespace) or `cluster` |
| `SHARED_GATEWAY_NAME`, `SHARED_GATEWAY_NAMESPACE` | instance name, controller namespace | Shared Gateway name, and its namespace with `cluster` scope |
| `SHARED_GATEWAY_PARAMETERS` | unset | ConfigMap name copied into shared Gateways' infrastructure parameters |
| `DEFAULT_BASE_DOMAIN` | unset | Base domain for [derived hostnames](#hostnames) |
| `DEFAULT_HOSTNAME_TEMPLATE` | `name-namespace` | `name-namespace` or `name` |
| `HOSTNAME_ALLOWLIST_DEFAULT` | `allow` | `deny`: namespaces without `allowed-hostnames` publish nothing |
| `TCP_MODE` | `false` | Lets LoadBalancer Services publish TCP ports |
| `ACCESS_APPLICATIONS` | `false` | Create a deny-all Access application per TCP hostname |
| `DNS_POLICY` | `sync` | `sync` or `upsert-only` (records are never deleted) |
| `DNS_OWNER_ID`, `DNS_TXT_PREFIX`, `DNS_COMMENT` | instance name, `_cf-tunnel-gateway`, `managed by cf-tunnel-gateway` | DNS ownership records |
| `WATCH_NAMESPACES` | all | Comma-separated namespaces to watch |
| `SWITCH_TIMEOUT`, `SWITCH_GRACE_PERIOD` | `5m`, `30s` | Moving an object between Gateways ([design](docs/design.md#switching-between-shared-and-dedicated)) |
| `CLOUDFLARED_IMAGE` | pinned `cloudflare/cloudflared` tag | Connector image |
| `CLUSTER_DOMAIN` | `cluster.local` | Used for backend FQDNs |
| `METRICS_BIND_ADDRESS`, `HEALTH_PROBE_BIND_ADDRESS` | `:8080`, `:8081` | Metrics and `/healthz`, `/readyz` |

### GatewayClass credentials

A GatewayClass's `spec.parametersRef` names a Secret (group `""`, kind
`Secret`, with a namespace) holding `accountId` and `apiToken` (its own
account), `dnsApiToken` (a zone-scoped DNS token), or both. Without
`accountId`/`apiToken` the class uses the controller's account credentials.
Tokens are verified against Cloudflare and the result shows up in the
class's `Accepted` condition. See [docs/setup.md](docs/setup.md) and
`examples/classes.yaml`.

### Gateway infrastructure parameters

`spec.infrastructure.parametersRef` may name a ConfigMap in the Gateway's
namespace with `replicas` (1 to 64), `nodeSelector`, `affinity`,
`tolerations`, `resources` (YAML), `logLevel`, and `disableConnector:
"true"` (no Deployment; run `cloudflared` yourself with the token Secret
`<gateway>-cloudflared-token`).

### Annotations

All keys are prefixed with `cf-tunnel-gateway.davy.tw/`.

| Annotation | On | Meaning |
| --- | --- | --- |
| `tunnel-id` | Gateway | Bind to an existing remotely-managed tunnel |
| `keep-tunnel: "true"` | Gateway | Do not delete the tunnel with the Gateway |
| `dns: disabled` | Gateway | Do not manage DNS records |
| `skip-cleanup: "true"` | Gateway | Skip Cloudflare cleanup on deletion (for lost credentials) |
| `gateway` | Ingress, Service | `shared` (default), `dedicated` (a Gateway and tunnel of its own), or `<namespace>/<name>` |
| `parameters` | Ingress, Service | ConfigMap for a dedicated Gateway's infrastructure parameters |
| `hostname` | Service | Public hostname(s), comma-separated |
| `hostname` | TCPRoute | The hostname the route publishes (TCP listeners cannot carry one) |
| `tcp-hostnames` | Service | TCP mode: `<port>[=<hostname>][,...]` |
| `default-hostname: "false"` | Ingress, Service | Opt out of derived hostnames |
| `allowed-gateway-classes` | Namespace | Classes the namespace's Gateways may use |
| `gateway-class` | Namespace | Class for Gateways generated in the namespace |
| `allowed-hostnames` | Namespace | Hostname patterns (`a.example.com`, `*.example.com`) the namespace may publish |
| `base-domain` | Namespace | Base domain for derived hostnames in the namespace |

Namespace annotations are the tenant policy: they only hold if tenants
cannot edit their own Namespace. Check your RBAC (and multi-tenancy tools
that delegate namespace edits) before relying on them.

### Hostnames

With a base domain, objects without a hostname get
`<name>-<namespace>.<base domain>` (sanitized, at most 63 characters per
label), and TCP ports `<name>-<namespace>-<port>.<base domain>`. A
hostname belongs to the first Gateway that publishes it; newer routes
elsewhere report a conflict. Existing DNS records that the controller did
not create are never overwritten.

## What is supported

A tunnel rule matches a hostname and a path regular expression and sends
the request to one origin. Anything that cannot be expressed is reported on
the route (`Accepted`, `ResolvedRefs`, `PartiallyInvalid`), not silently
dropped.

| Feature | Support |
| --- | --- |
| HTTP and HTTPS listeners | Yes; Cloudflare terminates TLS with its edge certificates, `certificateRefs` are ignored |
| HTTPRoute `PathPrefix`, `Exact`, `RegularExpression` paths | Yes |
| Header, query and method matches, filters (except `URLRewrite` of the hostname), timeouts, retries | No: reported as `PartiallyInvalid` |
| Traffic splitting | No: the highest-weight backend is used |
| GRPCRoute | Yes, to **TLS backends** (`appProtocol: https`): `cloudflared` speaks HTTP/2 only to TLS origins. gRPC must be enabled on the zone |
| TCPRoute | Yes, with the `hostname` annotation; clients use `cloudflared access tcp` |
| TLSRoute, UDPRoute | No |
| Cross-namespace backends | With a ReferenceGrant |
| Ingress | `Prefix`, `Exact`, `ImplementationSpecific` paths, `defaultBackend`; `spec.tls` is not needed |
| LoadBalancer Service | One HTTP port per Service, more ports as TCP in TCP mode |

Origins use `http`, or `https` without certificate verification when the
Service port's `appProtocol` is `https` or `kubernetes.io/wss`. The idle
connection pool to origins is disabled so requests spread over a Service's
Pods.

## Data plane

For a Gateway `N`, the controller creates (owned by the Gateway) a tunnel
named after the instance, namespace and Gateway, the token Secret
`N-cloudflared-token` (key `TUNNEL_TOKEN`), and a Deployment
`N-cloudflared` with a PodDisruptionBudget from two replicas. The connector
pods meet the `restricted` Pod Security Standard and are ready once they
hold a connection to Cloudflare. A HorizontalPodAutoscaler may own the
replica count. Deleting the Gateway deletes its DNS records, Access
applications and tunnel.

## Development

```sh
make test          # unit tests
make test-envtest  # unit + integration tests against kube-apiserver/etcd
make e2e           # against the current kubeconfig context (build tag e2e)
make e2e-kind      # on a throwaway kind cluster with a fake Cloudflare API (as CI does)
make e2e-k3d       # on a throwaway k3s cluster through k3d (as CI does)
make manifests     # regenerate dist/install.yaml
make chart-lint chart-render  # lint and render the Helm chart to bin/chart.yaml
```

With `CHART_MANIFESTS=$PWD/bin/chart.yaml`, `make test-envtest` also checks
that the rendered chart validates against the API server and grants the same
RBAC as `dist/install.yaml`.

### Releases

[`.github/workflows/release.yaml`](.github/workflows/release.yaml) publishes
to GHCR:

- every push to `main`: image `ghcr.io/davy-tw/cf-tunnel-gateway/controller:main`
  and `:sha-<commit>`, and chart `oci://ghcr.io/davy-tw/charts/cf-tunnel-gateway`
  version `0.0.0-main.<commit>` using that image (a prerelease: Helm only
  picks it when asked for that version);
- every tag `vX.Y.Z`: image `:X.Y.Z`, `:X.Y` and `:latest` (amd64 and
  arm64), chart version `X.Y.Z`, and a GitHub release with the chart and an
  `install.yaml` pinned to that image.

It can then deploy the release with `helm upgrade --install`. To turn that
on, set the repository variable `DEPLOY_ENABLED=true` and add a `production`
environment with a `KUBECONFIG` secret. The Cloudflare credentials stay in
the cluster: create the Secret `cf-tunnel-gateway` (or the one named by the
variable `CLOUDFLARE_SECRET`) in the target namespace first. Optional
variables: `DEPLOY_NAMESPACE` (default `cf-tunnel-gateway-system`) and
`HELM_VALUES` (a values file's contents). Running the workflow by hand with
a version redeploys that version.

[`docs/design.md`](docs/design.md) describes the design and its decisions.

## License

[MIT-0](LICENSE-MIT-0) OR [Apache-2.0](LICENSE-APACHE).
