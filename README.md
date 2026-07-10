# tachyne-ingress

> tachyne is an unofficial fan project, not affiliated with Mojang,
> Microsoft, or Minecraft's developer/publisher in any way. See the
> Disclaimer at the bottom.

## Project status

**Work in progress.** tachyne is young and moving fast: a full survival game
runs today, but expect rough edges, missing vanilla features, and breaking
changes between updates. **Bug reports are genuinely useful** — please open a
GitHub Issue with your client version/edition and what you saw. Contributions
are welcome too: see [CONTRIBUTING.md](CONTRIBUTING.md).



The tachyne cluster's single public Minecraft entrypoint. Everything enters
here, so hardening (rate limits, allow-lists, DDoS mitigation) lives in one
place and the gateway pods are never directly exposed. Two protocols:

- **Java (TCP `<server-ip>:25565`)** — a protocol-version router: reads the
  Handshake, picks the gateway that serves that protocol, **replays the
  handshake**, and splices the TCP streams byte-for-byte. Unknown versions get
  an honest local answer (status ping / login disconnect naming what's
  supported).
- **Bedrock (UDP `<server-ip>:19132`, RakNet)** — a stateful UDP forwarder to
  the internal Bedrock gateway (per-client session table, `cmd/ingress/udp.go`).
  No handshake to route on: Bedrock is latest-only, one gateway.

Renamed from `tachyne-dispatch` (2026-07-09) when Bedrock was pulled in behind
it — previously Bedrock clients hit the gateway pod directly on 19132/udp.

Deliberately dumb, by design:

- **Version-blind beyond the handshake, identity-blind always.** Never parses
  login, never sees names/UUIDs, does no authorization — policy lives in the
  gateways (via tachyne-access).
- **One path per connection**: peek handshake → route → splice (Java) /
  per-client UDP session (Bedrock). Adding a Java protocol is an
  `INGRESS_ROUTES` entry plus a gateway that serves it.
- **PROXY protocol v1** (`INGRESS_PROXY=1`) on the Java splice: gateways parse
  it (`tachyne-common/proxyproto`) so access checks + logs see real client IPs.
  The Service uses `externalTrafficPolicy: Local`, or kube-proxy SNAT hides the
  source. (Bedrock UDP does not carry PROXY v1 yet — see the tradeoff below.)

## Bedrock UDP forwarder

Per distinct client address, ingress opens a dedicated socket to the internal
Bedrock service and pumps datagrams both ways (idle sessions reaped after 60s).
RakNet keeps a session pinned to one path, so a client must always reach the
same ingress pod — the Service sets `sessionAffinity: ClientIP` (ingress is
`replicas: 2`). **v1 tradeoff**: UDP SNAT means the Bedrock gateway sees ingress
as the peer, not the real client IP; Bedrock identity comes from the XBL login
so access still works, and hardening happens here where the client IP *is*
visible. Full IP preservation to the gateway is a later PROXY-for-UDP upgrade.

## Edge IP firewall

The ingress is the only place a client's IP is visible before login, so the
network allow/deny lives here. On a new connection it asks **tachyne-access**
`POST /v1/check-ip {ip}` and drops a denied source — Java in the accept loop
before a goroutine is even spawned, Bedrock non-blocking (no session, no RakNet
reply). Rules are a firewall-style **ordered ACL in tachyne-access**
(first-match-wins; `allow 192.168.0.0/24` before `deny 0.0.0.0/0` admits the LAN
and blocks the rest), managed via its `GET/POST/DELETE /v1/ip-rules`. Verdicts
are cached per pod — **denies for 5 min**, so repeat offenders don't hammer
ingress or access — and the check **fails open** (the gateways' fail-closed
identity check is the backstop). Empty `INGRESS_ACCESS_URL` disables it. Identity
whitelist/blacklist stays at the gateways (the only place a decrypted identity —
especially Bedrock's, encrypted in RakNet — exists).

## Configuration (env)

```
INGRESS_LISTEN           TCP listen address                  (default ":25565")
INGRESS_ROUTES           "770-772=host:port,776=host:port"   (required)
INGRESS_SUPPORTED        human-readable list for unknown versions
INGRESS_PROXY            "1" = prefix PROXY protocol v1 to Java backends
INGRESS_BEDROCK_LISTEN   UDP listen address                  (default ":19132")
INGRESS_BEDROCK_BACKEND  internal Bedrock gateway host:port  ("" = Bedrock off)
INGRESS_ACCESS_URL       tachyne-access base URL             ("" = IP firewall off)
INGRESS_ACCESS_TOKEN     bearer token for the access API
```

Live routing: `770-772 → tachyne-gw-java-770 svc :25570`, `776 →
tachyne-gw-java-776 svc :25565`, Bedrock UDP → `tachyne-gw-bedrock.tachyne.svc
:19132`. All gateways are cluster-internal; ingress owns
`<server-ip>:{25565/tcp, 19132/udp}` via `externalIPs`. Protocols 773–775 are
deliberately unrouted (deprioritized).

## Build / deploy

```bash
go build ./... && go vet ./... && go test ./...
kubectl apply -f deploy/
```

CI builds + pushes the image on every push to main (`REGISTRY_TOKEN` org
secret, dind via the job network's default gateway). Cutover: `19132/udp` moved
off the `tachyne-gw-bedrock` Service onto this one — apply this repo's `deploy/`
together with the bedrock repo's internal-only service, then smoke with
`bedrockprobe` against `<server-ip>:19132`.

## Deployment

`Dockerfile` builds a static Go binary into a minimal image. `deploy/` holds
working Kubernetes manifests (the ones this project actually runs) — treat
them as examples: substitute your own image registry, hostnames, namespaces
and secrets before applying them to your cluster.

## Credits

No third-party dependencies beyond the shared `tachyne-common` library (the
PROXY-protocol reader and access client live there — see its credits).

## Development transparency

tachyne is built by its maintainer working with an AI coding agent
(Anthropic's Claude): substantial portions of the implementation were written
by the model under human direction, and every change is reviewed, tested and
deployed by the maintainer. The project's engineering discipline is designed
for exactly this workflow — byte-oracle tests pin the wire format, full test
suites gate every image build, and real-client verification signs off
gameplay. Disclosed here for transparency; judge the code on its behavior.

## License

Licensed under the **Apache License, Version 2.0** — see [LICENSE](LICENSE)
and [NOTICE](NOTICE). Note §6: the license grants no rights to the tachyne
name or any trademarks.

## Disclaimer

tachyne is an unofficial, independent project. It is **not** affiliated with,
endorsed, sponsored, or approved by Mojang Studios, Mojang Synergies AB,
Microsoft Corporation, or any of their subsidiaries — the developer and
publisher of Minecraft have no involvement with this project. "Minecraft" is
a trademark of Mojang Synergies AB. This project contains no Minecraft game
code; all game behavior is independently reimplemented, and data tables are
built from openly licensed community datasets (see Credits).
