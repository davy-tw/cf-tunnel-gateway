# Setup

## API tokens

Create tokens under **My Profile → API Tokens** (user tokens) or
**Manage Account → Account API Tokens** (account tokens); both work.

Tunnel permissions can only be granted for a whole account, while DNS
permissions can be limited to zones, so you can use one token or two:

| Token | Permissions | Where it goes |
| --- | --- | --- |
| Account token | Account → **Cloudflare Tunnel: Edit**; plus Account → **Access: Apps and Policies: Edit** if `ACCESS_APPLICATIONS` is on | `CLOUDFLARE_API_TOKEN` (with `CLOUDFLARE_ACCOUNT_ID`) in the controller Secret, or `apiToken` (with `accountId`) in a GatewayClass Secret |
| DNS token (optional) | Zone → **DNS: Edit** and Zone → **Zone: Read**, for the zones the class may publish in | `dnsApiToken` in a GatewayClass Secret |

Without a DNS token, the account token also needs the two zone permissions
for the zones you publish in. The account ID is on the account's overview
page in the dashboard.

The default install uses one Secret, `cf-tunnel-gateway` in
`cf-tunnel-gateway-system`, both as the controller's environment and as the
`cf-tunnel-gateway` GatewayClass's parameters:

```sh
kubectl -n cf-tunnel-gateway-system create secret generic cf-tunnel-gateway \
  --from-literal=CLOUDFLARE_ACCOUNT_ID=<account id> \
  --from-literal=CLOUDFLARE_API_TOKEN=<account token> \
  --from-literal=dnsApiToken=<dns token>   # optional
```

Check the class:

```sh
kubectl get gatewayclass cf-tunnel-gateway -o jsonpath='{.status.conditions}'
```

`Accepted=False` names the token that failed verification and why. Tokens
are verified again every hour and when the Secret changes.

### Separating zones

To let some namespaces publish only in one zone, add a GatewayClass whose
Secret holds a DNS token limited to that zone, and pin the namespaces to it
(`examples/classes.yaml`):

```yaml
metadata:
  annotations:
    cf-tunnel-gateway.davy.tw/allowed-gateway-classes: team-a
    cf-tunnel-gateway.davy.tw/gateway-class: team-a
    cf-tunnel-gateway.davy.tw/allowed-hostnames: "*.team-a.example.com"
```

These annotations only restrict tenants who cannot edit their Namespace.
If a role or a multi-tenancy tool lets them, they can lift the limits.

## Certificates

Cloudflare's Universal SSL certificate covers the zone apex and one level of
subdomains (`demo.example.com`). Deeper names (`demo.team-a.example.com`)
need an Advanced Certificate or Total TLS; the controller warns once when a
base domain is below the zone apex.

## gRPC

Enable gRPC for the zone (**Network → gRPC** in the dashboard). Backends
must serve TLS (any certificate; it is not verified) and say so with
`appProtocol: https` on the Service port, since `cloudflared` uses HTTP/2
only towards TLS origins. Clients connect over TLS on port 443
(`examples/grpc.yaml`).

## TCP

TCP publishing is off by default. Turn it on with `TCP_MODE=true` (chart:
`tcp.mode=true`); the TCPRoute CRD must be installed. A LoadBalancer Service
then publishes ports with `tcp-hostnames`, and a Gateway publishes a
TCPRoute on a TCP listener with the route's `hostname` annotation:

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: TCPRoute
metadata:
  name: ssh
  annotations:
    cf-tunnel-gateway.davy.tw/hostname: ssh.example.com
spec:
  parentRefs: [{name: demo}]   # a Gateway with a TCP listener
  rules: [{backendRefs: [{name: ssh, port: 22}]}]
```

Clients reach the hostname through a local `cloudflared`:

```sh
cloudflared access tcp --hostname ssh.example.com --url localhost:2222
ssh -p 2222 user@localhost
```

Anyone can do that unless an Access application protects the hostname.
With `ACCESS_APPLICATIONS=true` the controller creates a self-hosted Access
application per TCP hostname with no allow policy (so it denies everyone)
until you add a policy in Zero Trust; otherwise create one yourself.

## Existing tunnels and connectors

- `cf-tunnel-gateway.davy.tw/tunnel-id: <id>` on a Gateway binds it to an
  existing remotely-managed tunnel; its configuration is then replaced by
  the controller. The namespace must allow it with the annotation
  `cf-tunnel-gateway.davy.tw/allow-tunnel-id: "true"`, since binding gives
  the namespace that tunnel's connector token.
- With `disableConnector: "true"` in the Gateway's parameters ConfigMap,
  no Deployment is created; run `cloudflared tunnel run` anywhere with the
  `TUNNEL_TOKEN` from Secret `<gateway>-cloudflared-token`.

## Uninstalling

Delete Gateways, Ingresses and LoadBalancer Services first, while the
controller still runs, so it deletes their tunnels and DNS records. If the
credentials are gone, `cf-tunnel-gateway.davy.tw/skip-cleanup: "true"` lets
a Gateway go without cleanup.
