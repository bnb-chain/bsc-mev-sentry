# Exposing a Validator's MEV gRPC Endpoint

Validators can expose MEV RPC and gRPC through an AWS Application Load Balancer (ALB) or Network Load Balancer (NLB). Only `SendBidBlock` supports gRPC among the MEV business APIs; other MEV APIs continue to use JSON-RPC.

## Enable gRPC on the Components

Merge these settings into an existing MEV deployment. Keep the other required settings unchanged.

### Validator

The gRPC bind IP reuses `Node.HTTPHost`. Set it to a private IP reachable from the sentry, rather than localhost. Existing JSON-RPC calls can continue over WebSocket.

```toml
[Node]
HTTPHost = "<validator-private-ip>"

[Eth.Miner.Mev]
Enabled = true
BidBlockEnabled = true
GRPCDisabled = false
GRPCPort = 8552
GRPCConcurrency = 50 # Recommended maximum concurrent gRPC BidBlock requests
```

If using command-line flags instead, set `--http --http.addr <private-ip>` together. Restrict the HTTP and gRPC ports to trusted access.

### MEV Sentry

Enable the gRPC listener under `[Service]`, and add the downstream gRPC address to the corresponding **existing** `[[Validators]]` entry:

```toml
[Service]
GRPCListenAddr = ":8552"
GRPCConcurrency = 50 # Recommended maximum concurrent gRPC BidBlock requests

[[Validators]]
PublicHostName = "mev.example.org"
GRPCURL = "<validator-private-host>:8552"
```

An empty `GRPCListenAddr` disables gRPC. Keep `PrivateURL` unchanged (WebSocket or HTTP); configure `GRPCURL` separately as the validator's private `host:port`, without a URL scheme. This hop uses plaintext gRPC. The builder request's `validator_host_name` must match `PublicHostName`.

Restart each component after changing its configuration. If both run on the same host, avoid binding the same address and port; for example, use validator gRPC port `8553` and update the sentry's `GRPCURL` accordingly.

## Public Endpoint Requirements

The examples below assume sentry HTTP JSON-RPC on **8545** and plaintext HTTP/2 gRPC on **8552**. Replace these with your actual ports.

Both public deployment options require a **TLS-capable gRPC client**. The internal Builder implementation reviewed at commit `6363f2c3d` uses `insecure.NewCredentials()` and cannot connect directly to these TLS listeners. It needs TLS support with certificate and hostname verification, or a client-side TLS-origination proxy. Changing the target address alone does not enable TLS.

## Option 1: ALB — Share Port 443

Use this option to serve RPC and gRPC through the same domain and public port, with nginx behind the ALB.

```text
Client → ALB HTTPS:443
           ├─ gRPC paths → nginx HTTPS:443 (HTTP/2) → sentry:8552 → validator:8552
           └─ Other paths → nginx HTTP:80 (HTTP/1.1) → sentry:8545 → validator:8546 (WS)
```

Configure these ALB rules for the MEV domain, ahead of the default HTTP route:

| Path | Backend |
|---|---|
| `/mev.v1.BidBlockService/*` | nginx HTTPS target group with protocol version HTTP2 |
| `/grpc.health.v1.Health/*` | Same HTTP/2 target group |
| All other paths | Existing HTTP/1.1 target group |

In nginx Ingress, use the corresponding literal prefixes:

```text
/mev.v1.BidBlockService/
/grpc.health.v1.Health/
```

Set `pathType: ImplementationSpecific` and this annotation on the gRPC Ingress:

```yaml
nginx.ingress.kubernetes.io/backend-protocol: GRPC
```

Forward both prefixes to sentry **8552**, without rewriting the URI. Keep normal RPC on **8545**, in a separate Ingress so the GRPC annotation does not affect it. Configure request-size limits to accommodate bid/block messages.

ALB terminates client TLS. The HTTPS connection from ALB to nginx also requires TLS on nginx. Keep a separate HTTP/1.1 target group for HTTP JSON-RPC traffic.

Client endpoints:

- RPC: `https://mev.example.org`
- gRPC: `mev.example.org:443`, with TLS enabled

Only the listed gRPC services are exposed. Add rules when introducing another service. Path matching does not validate the request protocol.

## Option 2: NLB — Separate Ports

Use this option for port-based forwarding without nginx or path rules.

```text
Client → NLB TLS:443  → TCP target group → sentry:8545 → validator:8546 (WS)
Client → NLB TLS:8552 → TCP target group → sentry:8552 → validator:8552
```

Configure an **internet-facing NLB** with a certificate matching the public domain:

| Listener | ALPN policy | Backend target group |
|---|---|---|
| TLS:443 | HTTP1Only | TCP:8545 |
| TLS:8552 | HTTP2Only | TCP:8552 |

ALPN negotiates the application protocol during the client TLS handshake. `HTTP2Only` advertises `h2` for the gRPC connection. NLB terminates TLS and forwards decrypted traffic; it does not convert HTTP versions. The gRPC backend must accept plaintext HTTP/2.

Client endpoints:

- HTTP JSON-RPC: `https://mev.example.org`
- gRPC: `mev.example.org:8552`, with TLS enabled

This option exposes all gRPC services registered by the sentry. ALPN is protocol negotiation, not method filtering or authorization.

## Verification

First verify sentry-to-validator connectivity. Then call `/grpc.health.v1.Health/Check` on the public endpoint, if implemented, specifying service `mev.v1.BidBlockService`. Require both **grpc-status: 0** and a decoded response of **status: SERVING**. HTTP 200 alone is insufficient. Sentry health does not verify downstream validator connectivity in the reviewed implementation.

Finally, submit a valid, signed `SendBidBlock` in an authorized test environment. Correlate the bid hash with the sentry's gRPC forwarding logs and the validator response to confirm the full path, rather than a direct builder-to-validator call or JSON-RPC fallback. A successful submission does not guarantee that the bid wins and is sealed.

## Note: WAF Troubleshooting

If an ALB has a WAF attached, a passing Health check does not prove that BidBlock requests will pass. Investigate payload-dependent failures using WAF logs or sampled requests, ALB logs, and sentry logs; identify the terminating rule and matching field where available before changing policy.

Compare a zero-filled payload with a known reproducer using the same method, headers and payload size in an approved test. Confirm whether the request reaches the backend. Do not require a particular error such as `InvalidArgument`: authentication or other validation may fail first. HTTP 403 / gRPC `PermissionDenied` alone does not establish which component rejected the request.

[AWS documents that gRPC requests skip WAF body inspection](https://docs.aws.amazon.com/waf/latest/developerguide/web-acl-setting-body-inspection-limit.html). Other applicable WAF rules can still take effect. Do not assume that a SQLi rule match proves a body match. Investigate discrepancies in the actual deployment. If an exception is justified, scope it to the required hosts, service paths and content type, preserving other protections. Repeat the reproducer and a valid signed BidBlock test after deployment.

Once configured, submit your public gRPC endpoint via a pull request by adding `GRPC = "<your-public-grpc-endpoint>"` to your validator's entry in [validator-list.toml](https://github.com/bnb-chain/bsc-mev-info/blob/main/mainnet/validator-list.toml); you can also view other validators' published gRPC endpoints there.
