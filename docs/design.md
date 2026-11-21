# cf-tunnel-gateway design

## Goal

Publish in-cluster HTTP Services to the public internet through Cloudflare
Tunnel, driven by Kubernetes objects. No inbound port, NodePort or cloud load
balancer is needed: an in-cluster `cloudflared` connector dials out to
Cloudflare, Cloudflare's edge terminates TLS with its edge certificates and
forwards requests through the tunnel.

One controller serves three front doors:

1. Gateway API `Gateway`s of a GatewayClass served by this controller
2. `Ingress`es of a dedicated IngressClass
3. `Service`s of `type: LoadBalancer` with a dedicated `loadBalancerClass`

Ingresses and LoadBalancer Services are translated into Gateway API objects,
so all three share one data plane driven by Gateway API.

| Object | Cloudflare and cluster effect |
| --- | --- |
| GatewayClass | Points at Cloudflare credentials, validated against the Cloudflare API |
| Gateway | One remotely-managed Cloudflare Tunnel, one connector token Secret, one `cloudflared` Deployment |
| HTTPRoute, GRPCRoute, TCPRoute | Ingress rules in the tunnel's remote configuration, plus proxied DNS CNAME records for their hostnames |

The design is built only on public interfaces: the Kubernetes Gateway API
specification, the Kubernetes core APIs, Cloudflare's public API
documentation (API tokens and token verification, Cloudflare Tunnel, tunnel
configuration and connections, Zones, DNS records, Access applications), and
the public `cloudflared` client (its ingress rule semantics, origin request
options, environment variables, metrics and readiness endpoint, and the
official `cloudflare/cloudflared` image).

## Names

Everything this project owns lives under one domain,
`cf-tunnel-gateway.davy.tw`. The default instance name is
`cf-tunnel-gateway`.

| Name | Value |
| --- | --- |
| GatewayClass `controllerName` | `cf-tunnel-gateway.davy.tw/gateway-controller` (other instances: `cf-tunnel-gateway.davy.tw/gateway-controller-<instance>`) |
| IngressClass `controller` | `cf-tunnel-gateway.davy.tw/ingress-controller` |
| Finalizer (Gateways, LoadBalancer Services) | `cf-tunnel-gateway.davy.tw/finalizer` (other instances: `cf-tunnel-gateway.davy.tw/finalizer-<instance>`, so installations never finalize each other's objects) |
| GatewayClass finalizer | `gateway-exists-finalizer.gateway.networking.k8s.io` (the standard one) |
| Default GatewayClass, IngressClass, LoadBalancer class, shared Gateway | the instance name, `cf-tunnel-gateway` |

Annotations a user sets:

| Annotation | On | Meaning |
| --- | --- | --- |
| `tunnel-id` | Gateway | Bind to an existing remotely-managed tunnel |
| `keep-tunnel: "true"` | Gateway | Do not delete the tunnel with the Gateway |
| `dns: disabled` | Gateway | Do not manage DNS records for this Gateway |
| `skip-cleanup: "true"` | Gateway | Skip all Cloudflare cleanup on deletion (credentials gone) |
| `gateway` | Ingress, Service | `shared` (default), `dedicated`, or `<namespace>/<name>` |
| `parameters` | Ingress, Service | ConfigMap copied to a dedicated Gateway's `spec.infrastructure.parametersRef` |
| `hostname` | Service, TCPRoute | Public hostname(s) of a Service, comma-separated; the hostname a TCPRoute publishes |
| `tcp-hostnames` | Service | TCP mode: `<port>[=<hostname>][,...]` |
| `default-hostname: "false"` | Ingress, Service | Opt out of derived default hostnames |
| `allowed-gateway-classes` | Namespace | Classes the namespace's Gateways may use |
| `gateway-class` | Namespace | Class for Gateways generated in the namespace |
| `allowed-hostnames` | Namespace | Hostname patterns the namespace may publish |
| `base-domain` | Namespace | Base domain for derived hostnames in the namespace |
| `allow-tunnel-id: "true"` | Namespace | The namespace's Gateways may bind existing tunnels with `tunnel-id` |

Annotations the controller writes:

| Annotation | On | Meaning |
| --- | --- | --- |
| `tunnel` | Gateway | ID of the tunnel the Gateway currently owns |
| `previous-tunnel` | Gateway | Tunnel being replaced after a `tunnel-id` change |
| `token-secret` | Gateway | Name of the connector token Secret |
| `hostnames` | Gateway | Hostnames the Gateway publishes, and the routes that publish them |
| `source: ingress` or `service` | generated Gateways and routes | The kind of object they were generated from |
| `managed: shared` | shared Gateways | Created and owned by the controller |
| `dns-parent` | generated routes | During a switch, the Gateway that should own the DNS records |
| `default-hostname` | generated routes | A hostname the controller derived (section [Default hostnames](#default-hostnames)) |
| `gateway` | generated routes | The parent Gateway the route should end up on |
| `switch` | generated routes | Progress of a switch between parents |
| `dns-hostnames` | Gateway | Hostnames whose DNS records the Gateway may own, for cleanup |
| `signature` | Gateway | HMAC of `tunnel`, `previous-tunnel`, `hostnames` and `dns-hostnames` (see below) |
| `grpc-notified` | Gateway | The gRPC zone-setting event was emitted |
| `instance` | IngressClass | Installation that shipped the class |

All annotation keys are prefixed with `cf-tunnel-gateway.davy.tw/`.

Anyone who can edit a Gateway or route can also write the annotations the
controller writes. The Gateway annotations that decide what the controller
may delete or which Gateway keeps a hostname (`tunnel`, `previous-tunnel`,
`hostnames`, `dns-hostnames`) are therefore signed: `signature` is an
HMAC-SHA256 over the Gateway's UID and their values, keyed with a random
key the controller keeps in Secret `<instance>-signing-key` in its own
namespace (created on first use). Values whose signature does not match are
ignored and dropped on the next write, and the controller never signs a
value it did not verify or write itself. Tunnels are only deleted when
their name or a signed record ties them to the Gateway, and
`default-hostname` and `dns-parent` are only trusted on generated routes
whose derived hostname matches their source (see the sections below).
Losing the key only drops the records: tunnels are found again by name, a
replaced tunnel being switched away from is released rather than deleted,
and hostname claims are rebuilt on the next reconcile of each Gateway.

## Controller configuration

The controller runs as one Deployment with leader election (a Lease named
after the instance), so more than one replica is safe. Settings come from
environment variables; the Cloudflare account credentials come from a
Secret in the controller's namespace.

| Setting | Default | Meaning |
| --- | --- | --- |
| Account ID and API token (`CLOUDFLARE_ACCOUNT_ID`, `CLOUDFLARE_API_TOKEN`) | unset | Controller-level account credentials ("Cloudflare Tunnel: Edit"), used by every GatewayClass that brings none |
| Instance name | `cf-tunnel-gateway` | Identifies the installation; see [Several installations](#several-installations) |
| Controller name | derived from the instance | Matched against `GatewayClass.spec.controllerName` |
| GatewayClass name | instance name | Class of generated Gateways |
| IngressClass name | instance name | `spec.ingressClassName` served |
| LoadBalancer class | instance name | `spec.loadBalancerClass` served |
| Ingress / LoadBalancer translation | both on | Each front door can be turned off |
| Shared Gateway scope | `namespace` | `namespace` or `cluster` |
| Shared Gateway name | instance name | Name of the shared Gateway(s) |
| Shared Gateway namespace | controller namespace | Only with `cluster` scope |
| Shared Gateway parameters | unset | ConfigMap name copied into each shared Gateway's `parametersRef` |
| Default base domain | unset | Base domain for derived hostnames |
| Default hostname template | `name-namespace` | `name-namespace` or `name` |
| TCP mode | off | Lets LoadBalancer Services opt in to TCP publishing |
| Access applications | off | Create a deny-all Access application per TCP hostname |
| DNS policy | `sync` | `sync` or `upsert-only` |
| DNS owner ID | instance name | Written into DNS ownership records |
| DNS TXT prefix | `_cf-tunnel-gateway` | Label prefixed to a hostname to name its ownership TXT record |
| DNS comment | `managed by cf-tunnel-gateway` | Comment on managed records, for humans |
| Hostname allowlist default | allow | `deny` makes namespaces without `allowed-hostnames` publish nothing |
| Watched namespaces | all | Comma-separated list to limit the controller |
| Switch timeout, switch grace period | `5m`, `30s` | See [Switching](#switching-between-shared-and-dedicated) |
| `cloudflared` image | pinned `cloudflare/cloudflared` tag | Connector image |
| Cluster domain | `cluster.local` | Used for backend FQDNs |
| Cloudflare API URL | `https://api.cloudflare.com/client/v4` | For tests |
| Metrics and probe addresses | `:8080`, `:8081` | Prometheus metrics, `/healthz` and `/readyz` |

### Several installations

One controller serves any number of GatewayClasses, each with its own
credentials, so zone separation does not need more than one controller.
Several installations are still supported, for separate upgrades or failure
domains. Each needs a distinct instance name, from which these are derived:
the controller name, the IngressClass, LoadBalancer class, GatewayClass and
shared Gateway names, the leader-election Lease, the labels on generated
objects, the tunnel name prefix and the DNS owner ID. Two installations that
share a Cloudflare account therefore never adopt or delete each other's
tunnels or records. An installation logs an error at startup when the
IngressClass of its name was shipped by another installation.

Separate installations separate failures, not credentials, unless an
installation is limited to a list of namespaces with namespaced RBAC.

## Gateway API versions

The minimum is Gateway API **v1.6.0, Standard channel**. Every resource used
(GatewayClass, Gateway, HTTPRoute, GRPCRoute, TCPRoute, ReferenceGrant) is
served at `v1` there, so the controller uses `v1` only; older bundles that
serve some kinds only at `v1beta1` or `v1alpha2` are not supported.

At startup the controller checks which kinds are served. If GatewayClass,
Gateway or HTTPRoute is missing, it logs that Gateway API v1.6.0 or newer is
needed and keeps retrying instead of crash-looping. A missing GRPCRoute,
TCPRoute or ReferenceGrant disables that feature with one log line (without
ReferenceGrant, cross-namespace backends are never permitted). The
controller rechecks every minute, so installing TCPRoute later enables TCP
mode without a restart.

GatewayClasses get `SupportedVersion` from the
`gateway.networking.k8s.io/bundle-version` annotation of the installed
GatewayClass CRD: `True` for v1.6 and newer v1 releases, `False` otherwise.

## Credentials

A GatewayClass's `spec.parametersRef` names a core `Secret` with a
namespace. It may hold:

| Key | Meaning |
| --- | --- |
| `accountId` | Cloudflare account ID |
| `apiToken` | Account token: "Account: Cloudflare Tunnel: Edit" (plus "Access: Apps and Policies: Edit" for Access applications) |
| `dnsApiToken` | Zone token: "Zone: DNS: Edit" and "Zone: Zone: Read", limited to the zones the class may publish in |

Values are trimmed. Tunnel permissions can only be granted account-wide,
while DNS permissions can be limited to zones, so credentials are split:

- If the class Secret has `accountId` and `apiToken`, the class uses them
  (the combined form, and the way to put a class on another account).
  Otherwise it uses the controller-level account credentials; without those
  the class is `Accepted=False` / `InvalidParameters`.
- `dnsApiToken` is used for zone discovery and DNS records. Without it, the
  account token is used for DNS too.
- A leaked class Secret with only a zone token exposes DNS in that class's
  zones, never tunnels. The account token is rotated in one place.

Key names are plain Secret keys, so a parameters custom resource added later
can point at the same Secret.

## GatewayClass

- Classes with another `controllerName` are ignored.
- Each token in use is verified with Cloudflare's token-verification
  endpoint (the user-token endpoint first, then the account-token endpoint);
  a token that verifies but is not `active` is invalid. The account token
  must be able to list the account's tunnels, the zone token must see at
  least one zone. The `Accepted` message says which token failed. Tokens
  never appear in conditions, events or logs.
- `Accepted=True` / `Accepted`, or `Accepted=False` / `InvalidParameters`
  with the reason: missing reference, missing Secret or key, failed
  verification.
- The class is re-validated when its Secret changes (the controller watches
  Secret metadata only, then reads the Secret from the API server), and
  every hour, so revoked or expired tokens surface.
- While Gateways use the class, it carries the standard
  `gateway-exists-finalizer.gateway.networking.k8s.io` finalizer.
- The install bundle ships a GatewayClass and an IngressClass named after the
  instance; the user only creates the credentials Secret.

## Gateway

### Listeners

| Listener | Result |
| --- | --- |
| `HTTP` on 80 | Accepted. The recommended listener |
| `HTTPS` on 443 | Accepted, with a message that edge certificates are used and `tls.certificateRefs` are ignored |
| `HTTP` or `HTTPS` on another port | `Accepted=False` / `PortUnavailable`: Cloudflare serves a proxied hostname on all its ports at once, so a port cannot restrict anything |
| `TCP` | Accepted when TCP mode is on and TCPRoute is served; the port is not exposed or checked (Gateway API does not allow a hostname on TCP listeners, so each TCPRoute names its hostname, see [TCPRoute](#tcproute)) |
| anything else | `Accepted=False` / `UnsupportedProtocol` |

HTTP and HTTPS listeners carry HTTPRoute and GRPCRoute (when served); TCP
listeners carry TCPRoute. Other kinds in `allowedRoutes.kinds` give
`ResolvedRefs=False` / `InvalidRouteKinds`. Listeners do not change routing:
Cloudflare serves every published hostname on 80 and 443. If no listener is
valid, the Gateway is `Accepted=False` / `ListenersNotValid` and nothing is
provisioned.

`allowedRoutes.namespaces` (`Same`, `All`, `Selector`), listener `hostname`
and parentRef `sectionName` and `port` are all honored, with hostname
intersection as the Gateway API specifies.

### Tunnel

- Each Gateway owns exactly one remotely-managed tunnel in its class's
  account, named `<instance>-<namespace>-<name>-<hash>`, where the hash
  covers the cluster's identity (the `kube-system` namespace UID), the
  namespace and the name; long names are truncated before the hash. The
  name is the ownership marker: an existing tunnel is adopted only if it has
  exactly this name. The tunnel ID is recorded in the Gateway's `tunnel`
  annotation, and later lookups are by ID.
- `tunnel-id: <id>` binds the Gateway to an existing remotely-managed
  tunnel, if the namespace allows it (Namespace annotation
  `allow-tunnel-id: "true"`; otherwise `Accepted=False` /
  `InvalidParameters`): binding hands the namespace that tunnel's
  configuration and connector token. The controller then owns its
  configuration and deletes it with the Gateway unless `keep-tunnel:
  "true"` is also set. An unknown or deleted ID, or one already bound to
  another Gateway, gives `Accepted=False` / `InvalidParameters`. Changing
  `tunnel-id` switches tunnels make-before-break: the new tunnel is
  configured, the token Secret and connectors move to it, DNS records are
  repointed once it has a connection, and the previous tunnel is deleted
  if it carries the Gateway's tunnel name; a previously bound tunnel is
  released, not deleted.
- A tunnel is only ever deleted when it is the Gateway's own: it has the
  Gateway's tunnel name, or it is the tunnel bound by an allowed
  `tunnel-id`. IDs in the `tunnel` and `previous-tunnel` annotations that
  do not pass this check are left alone with a Warning event. On deletion
  the controller also looks for a tunnel of the Gateway's name whose ID was
  never recorded.
- The tunnel's remote configuration is a deterministic function of cluster
  state, written only by the Gateway reconciler and replaced as a whole. It
  always ends with a catch-all `http_status:404` rule.
- Its default origin request settings set `keepAliveConnections: -1`, which
  disables the idle connection pool, so that a Service in front of several
  Pods spreads requests over them instead of pinning one long-lived
  connection to one Pod. (`0` does not disable it; see
  [spike-findings.md](spike-findings.md).)
- Cloudflare rate limiting (HTTP 429) on any call backs off for the
  `Retry-After` duration, or ten minutes without one.

### Connector token Secret

The connector token is stored in Secret `<gateway>-cloudflared-token` (a
long Gateway name is shortened with a hash to fit), owned
by the Gateway, under `TUNNEL_TOKEN`, the variable `cloudflared` reads, so
the Secret can be loaded wholesale as environment variables. The name is a
documented contract, also published in the Gateway's `token-secret`
annotation: users may run extra connectors elsewhere with it, and Cloudflare
spreads traffic over all connectors. It is rewritten on every reconcile, so
it follows token rotation.

### Connector Deployment

Deployment `<gateway>-cloudflared` (shortened with a hash when needed so
its Pod names fit), owned by the Gateway (`-cloudflared`
keeps it from colliding with an application's own Deployment, since
generated Gateways are named after Services and Ingresses):

- `cloudflared tunnel run` with the configured image; the token Secret as
  environment, `NO_AUTOUPDATE=true`, metrics on `0.0.0.0:2000`, and the log
  level.
- Startup probe (about 50 seconds), liveness probe (every 10 seconds, 3
  failures) and readiness probe on `/ready` on the metrics port, which
  answers once the connector holds a connection to Cloudflare's edge.
- Non-root with UID 65532, no privilege escalation, all capabilities
  dropped, a read-only root filesystem, no service account token,
  `RuntimeDefault` seccomp, and the safe sysctl
  `net.ipv4.ping_group_range` so `cloudflared` can open ICMP sockets. The
  pods meet the `restricted` Pod Security Standard.
- Required node affinity to Linux on amd64 or arm64, a topology spread over
  nodes (`ScheduleAnyway`), and rolling updates with no unavailable Pod.
- A Pod-template annotation holds a hash of the token, so a new token rolls
  the Pods.
- Labels name the app, the instance and the Gateway; the Gateway's
  `spec.infrastructure.labels` and `annotations` are copied to the
  Deployment and Pods.
- Server-side applied. When a HorizontalPodAutoscaler targets the
  Deployment, `replicas` is left out so the autoscaler owns it.
- A PodDisruptionBudget (`minAvailable: 1`) when there is more than one
  replica.
- There is no DaemonSet mode: traffic goes to the Service's cluster IP
  anyway, so a connector per node adds no locality.
- An existing object with one of these names that the Gateway does not
  control is never taken over; the Gateway stays `Programmed=False` and
  says why.

### Infrastructure parameters

`spec.infrastructure.parametersRef` may name a ConfigMap (group `""`) in the
Gateway's namespace:

| Key | Effect |
| --- | --- |
| `disableConnector` | `"true"`: no Deployment; an existing one is deleted. The token Secret is kept for user-run connectors |
| `replicas` | 1 to 64, default 1 |
| `nodeSelector` | YAML map |
| `affinity` | YAML Affinity, merged over the default (user fields win) |
| `tolerations` | YAML list |
| `resources` | YAML ResourceRequirements |
| `logLevel` | `debug`, `info`, `warn`, `error` or `fatal` |

A missing ConfigMap or an invalid value sets `Accepted=False` /
`InvalidParameters` naming the key, and the last good Deployment keeps
running. The ConfigMap is watched. Other kinds are ignored.

### Status

- `Accepted` as above.
- `Programmed=True` / `Programmed` once the tunnel, configuration, token
  Secret and Deployment are written, a connector replica is ready and the
  tunnel reports at least one active connection (with the connector
  disabled: once the tunnel, configuration and token Secret are written;
  the message gives the number of connections from user-run connectors).
  Until then `Programmed=False` / `Pending`, rechecked every few seconds.
- Listeners report `supportedKinds`, `attachedRoutes` (attached routes per
  listener, as the spec defines) and their conditions.
- `status.addresses` holds a `Hostname` address `<tunnel-id>.cfargotunnel.com`.
- Every condition carries `observedGeneration`. Status conflicts requeue.

### Deletion

Gateways carry the finalizer. On deletion the controller sets
`Accepted=Unknown` / `Pending`, then, unless `skip-cleanup: "true"` is set:

1. deletes the DNS records this Gateway owns (not under `upsert-only`) and
   its Access applications;
2. unless `keep-tunnel` is set, replaces the tunnel secret with a fresh
   random value (so running connectors cannot reconnect), deletes the
   tunnel's connections, then deletes the tunnel;
3. emits a Warning event, clears its entries from route status, and removes
   the finalizer. The Secret and Deployment go with the owner references.

Any Cloudflare error requeues and retries. A Gateway that moves to another
controller's class is cleaned up the same way.

## Routes

### Attachment

A route attaches to a Gateway when one of its `parentRefs` names it (the
namespace defaults to the route's) and a listener allows it. Each route
gets a `status.parents[]` entry per parentRef of ours with `Accepted`,
`ResolvedRefs` and, when DNS is managed, `DNSRecordsReady`; entries for
parents no longer served are removed. Route, Service, ReferenceGrant,
Namespace and class Secret changes enqueue the affected Gateways.

A route without hostnames inherits the listener hostname; without either it
is rejected on that parent (`Accepted=False` / `UnsupportedValue`), because
a tunnel rule needs a hostname and an empty one would capture everything.

### HTTPRoute translation

`cloudflared` ingress rules match a hostname (exact or a leading `*.`) and
an optional path regular expression against the request path, top to bottom,
first match wins.

- Each effective hostname × each match path becomes one rule.
- `Exact` → `^<escaped value>$`.
- `PathPrefix` → `^<escaped value>(?:/.*)?$`, matching whole path segments;
  `/` is a rule without a path.
- `RegularExpression` → passed through.
- Header, query parameter and method matches, timeouts, retries, session
  persistence, backendRef filters and every filter except `URLRewrite`
  with only a `hostname` are not expressible: that rule is skipped, and the
  route is `Accepted=False` / `UnsupportedValue` when nothing is left, or
  carries `PartiallyInvalid=True` naming the field.
- `URLRewrite` with a `hostname` sets the rule's `httpHostHeader` (the
  original host goes in `X-Forwarded-Host`).
- One backend per rule: the highest weight wins, ties by order, weight 0
  is skipped; the `Accepted` message says when traffic splitting was
  reduced to one backend.
- Backends must be Services (`InvalidKind`), exist, not be of type
  `ExternalName` (whose name resolves wherever its author points it,
  around ReferenceGrants), and expose the port (`BackendNotFound`), and be
  allowed by a ReferenceGrant when in another namespace
  (`RefNotPermitted`). An unusable backend's rule answers
  `http_status:503`, so its requests do not fall through to a shorter
  prefix that belongs to another backend.
- Origins are `<scheme>://<service>.<namespace>.svc.<cluster domain>:<port>`.
  The scheme follows the Service port's `appProtocol`: `https` and
  `kubernetes.io/wss` give `https` with `noTLSVerify` (in-cluster
  certificates are rarely publicly trusted, and the hop stays in the
  cluster); everything else, including `kubernetes.io/h2c`, gives `http`,
  since `cloudflared` speaks only HTTP/1.1 to cleartext origins.

Rules are ordered by Gateway API precedence: TCP rules first, then exact
hostnames before wildcards (longer suffixes first), then per hostname:
`Exact` paths, regular expressions (longer first), longer prefixes before
shorter ones; ties go to the older route, then namespace/name, then rule
order. The 404 catch-all is last. Two rules with the same hostname and path
conflict: the older route keeps it and the newer route reports it
(`Accepted=False` / `Conflicted` when nothing else is left).

### GRPCRoute

GRPCRoute attaches to HTTP and HTTPS listeners like HTTPRoute, and its
rules are ordered with the HTTP rules. Method matches become path regular
expressions, since a gRPC call's path is `/<service>/<method>`: service and
method → that path; service only → any method of the service; method only →
that method on any service; `RegularExpression` → the two patterns combined;
no match → every path. Header matches and filters are unsupported.

`cloudflared` only uses HTTP/2 towards TLS origins, so GRPCRoute backends
must serve TLS and say so with `appProtocol: https` on the Service port; the
rule gets `http2Origin: true`. A cleartext backend gets
`ResolvedRefs=False` / `UnsupportedProtocol` and its rule answers 503. gRPC
also has to be enabled in the zone's network settings in Cloudflare, and
clients use TLS on 443; the controller does not change zone settings and
emits one Normal event per Gateway when a GRPCRoute first attaches.

### TCPRoute

An accepted TCPRoute on a TCP listener becomes a rule for the hostname in
its `hostname` annotation (one non-wildcard hostname; without it the route
is `Accepted=False` / `UnsupportedValue`) with origin `tcp://<service FQDN>:<port>` (first backend of the
first rule). These hostnames are not reachable by ordinary visitors: a
client runs `cloudflared access tcp`. They get the same DNS handling as HTTP
hostnames.

## DNS

### Zones

For each hostname the zone is found by trying the hostname and each parent
suffix, longest first, against the active zones the class's DNS token can
see. Lookups are cached per reconcile. A hostname without a zone is reported
on its route's parent (`DNSRecordsReady=False`).

### Records and ownership

Every published hostname (unless the Gateway has `dns: disabled`) gets a
proxied CNAME to `<tunnel-id>.cfargotunnel.com` (a wildcard hostname gets a
wildcard CNAME), marked twice:

- a TXT record named `<prefix>.<hostname>` (for `*.<domain>`:
  `<prefix>-wildcard.<domain>`) with content
  `heritage=cf-tunnel-gateway,owner=<owner ID>,gateway=<namespace>/<name>`.
  A record is ours only if this TXT record matches;
- a comment on the CNAME for humans.

An existing CNAME that is not ours is never overwritten: the conflict is
reported on the route's parent status. The first Gateway to publish a
hostname keeps it (see [Tenant isolation](#tenant-isolation)).

With DNS disabled, tunnel rules are still written, and the Gateway's address
is the CNAME target to point records at.

### Cleanup

Under the default `sync` policy, owned records of hostnames the Gateway no
longer publishes are deleted on every reconcile, and all owned records are
deleted with the Gateway. `upsert-only` never deletes records.

## Shared Gateway

Ingresses and LoadBalancer Services attach their generated routes to a
shared Gateway by default, so many objects share one tunnel and one
`cloudflared` Deployment.

- `namespace` scope (default): one shared Gateway in each namespace that
  needs one. The tunnel token stays in that namespace, and a failure only
  affects it. The Gateway is deleted once the last object using it is gone.
- `cluster` scope: one shared Gateway in the configured namespace serves
  every namespace; `cloudflared` reaches Services across namespaces. It is
  never deleted automatically.
- Shared Gateways are annotated `managed: shared`, have no owner, and the
  controller rewrites their listeners: an `http` listener on 80 for
  HTTPRoutes (`from: Same` or `All`), one per allowed hostname pattern when
  the namespace has `allowed-hostnames` (plus `http-default` for
  `*.<base domain>`, since derived hostnames are exempt from the
  allowlist), and in TCP mode one `tcp` listener (port 1, which nothing
  listens on) for the TCPRoutes.
- A Gateway with the shared name that lacks the annotation is never
  touched; generated routes wait and their sources get a Warning event.
- A shared Gateway that is no longer the configured one (after a scope or
  name change) is deleted once nothing uses it.

The `gateway` annotation overrides this per object: `dedicated` generates a
Gateway of the object's name in its namespace, owned by it, with its own
tunnel (only then does `parameters` apply); `<namespace>/<name>` attaches to
an existing Gateway of one of our classes, whose listeners must allow it.

### Switching between shared and dedicated

Every switch moves the hostnames to another tunnel, so it is
make-before-break:

1. The new parent is brought up (dedicated Gateway created, or listeners
   added). The routes stay on the old parent.
2. The controller waits until the new parent is `Programmed`, emitting a
   Normal event. After the switch timeout it emits a Warning event, keeps
   the old parent and tries again later.
3. The new parent is added to the routes, with `dns-parent` naming it.
4. The new parent repoints the DNS records: a record owned by another
   Gateway of this installation may be taken over when the route that
   publishes it is attached to both. The route's `DNSRecordsReady`
   condition for the new parent says when this is done.
5. After the grace period the old parent is removed from the routes; a
   dedicated old Gateway is deleted (its cleanup skips records it no longer
   owns), and listeners kept only for this object go.
6. The load balancer status moves to the new tunnel.

Changing the shared scope or name is a switch for every object using the
shared Gateway. Turning a front door off is not a switch: its routes and
dedicated Gateways are deleted.

## Default hostnames

With a default base domain (controller setting, or the namespace's
`base-domain` annotation, which must be inside a zone the class can see),
objects without a hostname get `<label>.<base domain>`, where `<label>` is
`<name>-<namespace>` (or `<name>`): lower-cased, invalid characters
replaced with `-`, truncated with a hash at 63 characters. A single label
directly under a zone is covered by Cloudflare's Universal SSL certificate;
a base domain below the zone apex needs an advanced certificate, which the
controller warns about once.

- Ingress rules without a host and a lone `defaultBackend` use it.
- LoadBalancer Services without `hostname` use it for their HTTP port, and
  `tcp-hostnames` entries with only a port use `<label>-<port>.<base>`.
- `default-hostname: "false"` opts out. Collisions go to the older object;
  an explicitly written hostname always wins. Derived hostnames are exempt
  from `allowed-hostnames`, appear in the route, the source's status, and a
  Normal event.

## Tenant isolation

Cloudflare API tokens can be limited to accounts and zones, not to
hostnames or single tunnels, so isolation is layered:

1. API tokens live only in Secrets referenced by GatewayClasses or the
   controller; app namespaces only hold connector tokens, which can run a
   connector for that one tunnel and nothing else.
2. Zone separation by GatewayClass, each with a zone-scoped DNS token.
   Namespace annotation `allowed-gateway-classes` limits user Gateways
   (others get `Accepted=False` / `NotAllowed`); `gateway-class` picks the
   class for generated Gateways.
3. `allowed-hostnames: <pattern>[,...]` (exact names or `*.<domain>` for
   any depth) limits the hostnames a namespace publishes: others are left
   out of the tunnel and DNS, and reported on the route. Shared Gateways
   copy the patterns into their listener hostnames. With the `deny`
   default, a namespace without the annotation publishes nothing.
4. The first Gateway to publish a hostname keeps it: routes elsewhere,
   on older Gateways too, are `Accepted=False` naming the conflict. Claims
   come from the signed `hostnames` annotations of accepted Gateways
   (cleared when a Gateway stops being accepted), so they survive
   controller restarts and cannot be forged; when two Gateways started
   publishing a hostname at once, the older one keeps it. DNS records move
   to another Gateway only when the switching route is attached to the
   current owner and every route the owner publishes the hostname for
   moves too.
5. `tunnel-id` needs the namespace's `allow-tunnel-id: "true"`, so that a
   namespace cannot take over a tunnel it was not given.
6. Namespace annotations are the policy surface because tenants normally
   cannot edit their Namespace. Some multi-tenancy tools let them; this
   policy is only as strong as that RBAC. A policy resource is on the
   roadmap.

## Ingress translation

Applies to Ingresses of the configured IngressClass (also when it is the
cluster default and none is named), while that class names this
controller. The parent Gateway follows the [Shared Gateway](#shared-gateway)
rules; a dedicated one has a single `http` listener on 80.

- One HTTPRoute per host (`<ingress>` or `<ingress>-<i>`), owned by the
  Ingress and annotated `source: ingress`, with one rule per path:
  `Prefix`, `ImplementationSpecific` and unset → `PathPrefix`, `Exact` →
  `Exact`. Grouping by host keeps one host's paths off another host.
- Named ports are resolved through the Service. A host with a backend that
  cannot be used (not a Service, unknown named port) is not served until it
  resolves, with a Warning event: leaving out only that path would send its
  requests to a shorter prefix of another backend.
- `defaultBackend` takes requests no rule matches: a `/` rule is added to
  every host that has none, and it serves the derived hostname.
- `spec.tls` is not used (edge certificates); a Normal event says so.
- An existing object of a generated name not owned by the Ingress is left
  alone with a Warning event.
- `status.loadBalancer.ingress[]` gets the parent Gateway's
  `<tunnel-id>.cfargotunnel.com` once it is programmed.
- When the Ingress is deleted or no longer served, its routes (and its
  dedicated Gateway) are deleted.

## LoadBalancer Service translation

Applies to Services of `type: LoadBalancer` whose `loadBalancerClass` is the
configured one; Kubernetes keeps cloud load balancers away from them. The
controller adds its finalizer.

- `hostname` names the public hostname(s); otherwise the derived one is
  used. Without either nothing is generated and a Warning event explains
  that a tunnel publishes by hostname only.
- The HTTP port is the first TCP port whose `appProtocol` is `http`,
  `kubernetes.io/h2c`, `kubernetes.io/ws`, `https` or `kubernetes.io/wss`,
  else the first TCP port. HTTPRoute `<svc>-http-<port>` sends `/` to the
  Service on it.
- Other ports are skipped with one Warning event, unless listed in
  `tcp-hostnames` in TCP mode. UDP and SCTP ports are always skipped.
- `status.loadBalancer.ingress[]` holds one entry per HTTP hostname (ports
  443 and 80) and one per TCP hostname (the Service port).
- On deletion, a class change, or when no hostname is left, the generated
  objects are deleted, the status cleared and the finalizer removed.

### TCP mode

With TCP mode on and TCPRoute served, `tcp-hostnames: <port>[=<hostname>]`
publishes TCP ports: per port a TCPRoute `<svc>-tcp-<port>` with the
hostname in its `hostname` annotation, attached to the parent Gateway's
`tcp` listener (a TCP listener cannot carry a hostname). Each hostname belongs to one
port of one Service (the oldest claim wins). Clients need
`cloudflared access tcp`, and an Access policy should protect these
hostnames: the controller emits a Normal event saying so, or, with Access
applications turned on, creates a self-hosted Access application per
hostname with no allow policy (deny all) until the user adds one, deleted
with the hostname.

## RBAC

Cluster-wide: GatewayClasses, Gateways, HTTPRoutes, GRPCRoutes, TCPRoutes
(read, write, status, finalizers), ReferenceGrants (read), Secrets (read and
write, including the signing key in its own namespace; Secrets are cached as metadata only, plus the controller's own token
Secrets), ConfigMaps, Services and their status and finalizers, Ingresses
and their status and finalizers, IngressClasses, Namespaces, Deployments,
PodDisruptionBudgets, HorizontalPodAutoscalers (read), the GatewayClass CRD
(read, for the bundle version), Events, and a Lease in its own namespace.

## Observability

- Controller metrics: Cloudflare API calls by operation and result,
  latency, rate-limit hits, and per-Gateway tunnel rule and DNS record
  counts.
- Events for tunnel creation and deletion, DNS conflicts, switches and
  cleanup.
- The `cloudflared` metrics port is named, so a ServiceMonitor or
  PodMonitor can scrape it.

## Out of scope

- Gateway API conformance: header routing, traffic splitting and most
  filters cannot be expressed in a tunnel rule.
- TLSRoute, UDPRoute, UDP exposure, private networks (WARP routing), WAF and
  other zone features, Access policies.
- Parameters and policy custom resources (roadmap).

## Risks checked before implementation

How `cloudflared` applies a remote configuration of this shape was
prototyped first; see [spike-findings.md](spike-findings.md).
