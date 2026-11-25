<p align="center">
  <img src="docs/images/logo.svg" alt="github-api-proxy" height="80">
</p>

# github-api-proxy

🔀🐙 A transparent reverse proxy for the GitHub REST and GraphQL APIs. Clients
only change their base URL; every call is forwarded to `api.github.com` unchanged,
and the proxy records **who** called **what**, **when**, how it went and how
much rate limit is left.

- **Transparent**: clients keep their own tokens (PATs, GitHub App installation tokens, app JWTs, OAuth).
  Headers, bodies and status codes pass through untouched; pagination `Link` and redirect `Location`
  headers are rewritten so clients stay on the proxy.
- **Private**: only the networks in `Network:AllowedSources` (vinhedo nodes, osasco, your machines) may call it.
- **Caller identification**: tokens are fingerprinted (SHA-256; never stored) and resolved in the
  background to a GitHub login, `installation:<owner>` or `app:<id>`.
- **MySQL / MariaDB log**: one row per request, written asynchronously in batches. If MySQL is down,
  GitHub traffic keeps flowing and records queue (bounded) until it recovers.
- **Stats REST API** for Projects Monitor, documented with **Swagger UI** at `/swagger/`.
- **Grimoire** holds every setting and secret; the host only knows how to reach Grimoire.
- **LogStream over UDP** (HMAC-signed datagrams) for startup/shutdown, warnings and errors, with the
  release's **GitVersion semVer as `app_id`**.
- **Auto-deploy**: every merge to `main` is versioned by GitVersion, deployed to a vinhedo node as a
  systemd service (amd64 or arm64, detected), and tagged as a GitHub release.

## Contents

- [Using it from clients](#using-it-from-clients)
- [Configuration (Grimoire)](#configuration-grimoire)
- [Network allow-list](#network-allow-list)
- [Who is the caller?](#who-is-the-caller)
- [Stats API and Swagger](#stats-api-and-swagger)
- [LogStream](#logstream)
- [Versioning and deployment](#versioning-and-deployment)
- [Security notes](#security-notes)
- [Development](#development)

## Using it from clients

Replace `https://api.github.com` with the proxy URL. Both the github.com layout and the GitHub
Enterprise layout (`/api/v3`, `/api/graphql`) are accepted, because some SDKs add those paths
themselves when the host isn't github.com.

| Client | Change |
| --- | --- |
| **Pancake** (PHP) | `GitHub::setDefaultEndpoint('https://github-proxy.example.com/')` once at bootstrap, or set `GITHUB_API_URL` — every `new GitHub(...)` then uses the proxy (needs [pancake#690](https://github.com/guibranco/pancake/pull/690)) |
| curl / any HTTP client | `https://github-proxy.example.com/repos/owner/repo` |
| Octokit.NET | `new GitHubClient(header, new Uri("https://github-proxy.example.com"))` (it adds `/api/v3`, which is handled) |
| Octokit.js | `new Octokit({ baseUrl: "https://github-proxy.example.com" })` |
| PyGithub | `Github(base_url="https://github-proxy.example.com", auth=...)` |
| GraphQL | `POST https://github-proxy.example.com/graphql` |

**Not covered.** `git clone`/`push` go to `github.com`, not the API. Release asset uploads go to
`uploads.github.com`, and some response bodies contain absolute `api.github.com` URLs (for example
`url` fields). A client that follows those URLs leaves the proxy for that call.

## Configuration (Grimoire)

In production every setting and secret lives in [Grimoire](https://github.com/guibranco/grimoire-api).
The host only has the three bootstrap values, written to `/etc/github-api-proxy/env` by the Deploy
workflow:

| Variable | |
| --- | --- |
| `Grimoire__BaseUrl` | Grimoire API root (`GRIMOIRE_BASE_URL` repository secret) |
| `Grimoire__ApiKey` | The application's consumer API key (`GRIMOIRE_API_KEY`) |
| `Grimoire__Environment` | Environment slug, default `production` (`GRIMOIRE_ENVIRONMENT`) |

At startup the service reads every **configuration** of the environment
(`GET consumer/configurations`) and these **secrets** by name (`GET consumer/secrets/{name}`;
Grimoire cannot list secrets, so names are fixed, `--` standing for `:`):

| Secret in Grimoire | Required | |
| --- | :---: | --- |
| `MySql--ConnectionString` | ✅ | Go MySQL DSN: `user:pass@tcp(host:3306)/db?tls=preferred` (UTC and `parseTime` are forced) |
| `Admin--ApiToken` | ✅ | Bearer token for the stats API (≥ 24 chars; `openssl rand -hex 32`) |
| `LogStream--ApiToken` | | The LogStream client's token (signs UDP packets) |

| Configuration in Grimoire | Default | |
| --- | --- | --- |
| `Network:AllowedSources` | **required** | IPs, CIDRs and host names allowed to call the proxy — see [below](#network-allow-list) |
| `Network:RefreshInterval` | `5m` | How often allow-list host names are re-resolved (dynamic DNS) |
| `Grimoire:RefreshInterval` | `30s` | How often Grimoire's `Sentinel` / `Global:Sentinel` are checked for published changes (a `304` while nothing changed) |
| `Network:PublicHealth` | `false` | `true` lets anyone reach `GET /health` on the admin listener (e.g. UptimeRobot) |
| `Proxy:ListenAddr` | `127.0.0.1:8080` | Proxy listener (behind NGINX) |
| `Proxy:AdminListenAddr` | `127.0.0.1:8081` | Stats API, `/health`, Swagger UI |
| `Proxy:PublicBaseUrl` | derived per request | Base used when rewriting `Link`/`Location` |
| `Proxy:TrustedProxies` | `127.0.0.1,::1` | Peers allowed to set `X-Forwarded-*` |
| `Proxy:UpstreamUrl` | `https://api.github.com` | |
| `Proxy:UpstreamTimeout` | `60s` | Max wait for GitHub's response headers |
| `Proxy:ResolveIdentities` | `true` | Look up each new token's account (one call on that token's own quota) |
| `Proxy:RateLimitWarnPercent` | `10` | Warn when a caller's remaining quota drops below this % |
| `Proxy:SlowRequestThreshold` | `10s` | Warn on slower calls |
| `Storage:RetentionDays` | `30` | Hourly cleanup of old rows |
| `Storage:QueueSize` / `Storage:BatchSize` / `Storage:FlushInterval` | `10000` / `200` / `1s` | MySQL writer |
| `LogStream:Server` | – | LogStream base URL (also the default UDP host); empty disables LogStream |
| `LogStream:ApiKey` | – | The LogStream client's key (its app_key), e.g. `github-api-proxy` |
| `LogStream:AppId` | the release semVer | Override the `app_id` |
| `LogStream:Transport` | `Udp` | `Udp` or `Http` |
| `LogStream:UdpHost` / `LogStream:UdpPort` | `LogStream:Server`'s host / `8082` | Use `127.0.0.1` on the LogStream host (vinhedo3) |
| `LogStream:UdpMaxPacketSize` | `8000` | Larger entries go over HTTPS |
| `LogStream:MinimumLevel` | `info` | Lowest level sent (everything still goes to journald) |
| `LogStream:Enabled` | `true` | Switch LogStream off without deleting anything |

Values are loaded once at startup — restart the service to apply changes — except
`Network:AllowedSources`, which is re-read whenever Grimoire publishes changes (needs grimoire-api
2.22+, see [below](#network-allow-list)). If Grimoire is
unreachable, rejects the key or does not know the environment, the service **refuses to start**, so a
deploy rolls back instead of running on partial settings. An environment variable with the key's
name (`:` written as `__`, e.g. `Proxy__ListenAddr`) still overrides a single Grimoire value; without
`Grimoire__BaseUrl`, everything is read from the environment (local development).

The database tables (`github_proxy_requests`, `github_proxy_tokens`, `github_proxy_migrations`) are
created on startup from `internal/store/migrations`.

## Network allow-list

Only sources in `Network:AllowedSources` may use the proxy **and** the stats API; everyone else gets a
GitHub-style `403` (`{"message":"github-api-proxy: requests from this address are not allowed"}`).
Refused proxy calls are stored (status 403, error `blocked by network allow-list`, counted as `blocked`
in the summary) and reported to LogStream as a warning at most once per address every 10 minutes.
Loopback is always allowed.

Entries are separated by commas, semicolons, spaces or new lines; `#` starts a comment:

```text
node1.example.com, node2.example.com, node3.example.com,
node4.example.com, node5.example.com, node6.example.com,
198.51.100.30           # cPanel host
10.6.0.0/24             # WireGuard VPN subnet
home.example.dyndns.org # local machines via dynamic DNS
203.0.113.25            # or the home IP itself
```

- **IPs and CIDRs** match directly (IPv4 and IPv6).
- **Host names** are resolved (A and AAAA) on every refresh, so a dynamic-DNS name follows a changing
  home IP. During a DNS hiccup the last good addresses are kept. Don't use names proxied by
  Cloudflare (orange cloud): they resolve to Cloudflare, not to the machine.
- **Local machines**: put your home IP (or a DDNS name) in the list, or reach the proxy through the
  WireGuard tunnel and allow its subnet. Changing the value in Grimoire applies once it is
  published, no restart (see below).
- `*` disables the check (not recommended).

**How changes reach the proxy.** The proxy watches Grimoire's sentinel keys (`Sentinel` for this
application, `Global:Sentinel` for inherited globals) every `Grimoire:RefreshInterval` with
`If-None-Match`, so an unchanged check is two `304`s. When one changes it re-reads
`Network:AllowedSources` and applies it straight away. With auto-publish on (Grimoire's default) that
is within ~30 seconds of saving; with it off, after **Publish changes**. If Grimoire is unreachable
the current list stays and checks back off up to 5 minutes. Host names are still re-resolved every
`Network:RefreshInterval` without calling Grimoire. Setting `Network__AllowedSources` in the
environment pins the list and turns the watching off.

The client address is the TCP peer, or — when the peer is in `Proxy:TrustedProxies` (the local NGINX) —
the right-most untrusted `X-Forwarded-For` entry, so it can't be spoofed by a header.

## Who is the caller?

| Token | Resolved as | Cost |
| --- | --- | --- |
| `ghp_`, `github_pat_`, `gho_`, `ghu_` | GitHub login, via `GET /user` | 1 call per new token |
| `ghs_` (App installation) | `installation:<account>`, via `GET /installation/repositories?per_page=1` | 1 call per new token (they rotate hourly) |
| App JWT | `app:<app id>` from the `iss` claim | free |
| none | `anonymous` (see `client_ip` / `user_agent`) | – |
| rejected by GitHub | `invalid:<fingerprint>` | – |

Until resolution finishes (milliseconds, in the background), rows show `token:<fingerprint>`; rows are
joined with `github_proxy_tokens` when queried, so earlier rows pick up the name automatically.

## Stats API and Swagger

Served on `Proxy:AdminListenAddr` and limited to the allow-list. **Swagger UI** is at `/swagger/`
and the OpenAPI 3 document at `/openapi.yaml`. All endpoints except `/health` need
`Authorization: Bearer <Admin--ApiToken>`; `period` accepts `90m`, `24h`, `7d`… (max `90d`).

| Endpoint | Returns |
| --- | --- |
| `GET /health` | status (`ok`/`degraded`), version, uptime, DB state, writer queue/drops, allow-list size |
| `GET /api/v1/summary?period=24h` | totals, 4xx/5xx, upstream failures, rate-limited, blocked, latency, bytes |
| `GET /api/v1/callers?period=24h` | per caller: requests, errors, first/last seen, token kinds, IPs, user agents |
| `GET /api/v1/routes?period=24h&caller=` | busiest normalised endpoints (`/repos/{owner}/{repo}/pulls/{id}`) |
| `GET /api/v1/route-usage?period=24h&caller=` | per token, method, route and status: requests, last time, avg duration, total size |
| `GET /api/v1/timeline?period=24h&bucket=hour` | requests/errors/latency per `minute`, `hour` or `day` |
| `GET /api/v1/requests?status=5xx&caller=&before_id=` | latest requests (keyset pagination) |
| `GET /api/v1/rate-limits` | latest quota per caller and resource, most exhausted first |

A ready-made Projects Monitor client is in
[`integrations/projects-monitor`](integrations/projects-monitor) (same pattern as
`WebhooksHandler.php`, secrets in `Src/secrets/githubApiProxy.secrets.php`).

The admin host also serves the project icon at `/favicon.ico`, `/favicon.svg` and
`/apple-touch-icon.png` (embedded in the binary, cached for a day). The proxy port serves none of them,
so it stays a byte-for-byte pass-through to GitHub. Logo sources are in [`docs/images`](docs/images),
including a `social-preview.png` for the repository's social preview setting.

## LogStream

Startup (version, settings origin, transport), shutdown, the allow-list loading, new callers identified,
and warnings/errors — low or exhausted rate limits, GitHub 5xx, slow calls, refused addresses, MySQL or
Grimoire failures — go to [LogStream](https://github.com/guibranco/logstream-server). Repeated events are
throttled; every entry of one process shares a `trace_id`, so a run can be followed from start to stop.

By default each entry is one **HMAC-signed UDP datagram** to `LogStream:UdpHost:UdpPort` (`8082`): the
`LogStream--ApiToken` signs the packet and never travels on the wire. The vinhedo nodes are allowed by
LogStream's `UDP_ALLOWED_SOURCES`; on the LogStream host itself use `LogStream:UdpHost=127.0.0.1`.
Entries larger than `LogStream:UdpMaxPacketSize` are posted over HTTPS instead, which is why
`LogStream:Server` stays required. UDP is fire-and-forget: rejected packets (e.g. a clock more than
30 s off) show up in LogStream's `GET /api/health` as `udp.rejected`, so keep the node NTP-synced.

The **`app_id` is the release version** computed by GitVersion (e.g. `1.4.0`), baked into the binary,
so the LogStream UI can tell releases apart. `LogStream:AppId` overrides it.

## Versioning and deployment

**GitVersion** (`GitVersion.yml`, GitHubFlow, continuous deployment) computes the version from the
commits on `main`: add `+semver: minor` (or `major`) to a commit message to bump; the default is a
patch. `github-api-proxy --version` prints it.

| Workflow | When | What |
| --- | --- | --- |
| `build.yml` | pull requests | gofmt, vet, tests (race), coverage, ShellCheck, amd64 + arm64 builds |
| `deploy.yml` | push to `main` | build & test → deploy to the vinhedo node → tag `vX.Y.Z` and GitHub release |

The deploy detects the node's CPU (`x86_64` → amd64 for vinhedo1–4, `aarch64` → arm64 for vinhedo5–6),
uploads that build with a checksum and runs the root-owned installer, which swaps the release
atomically, waits for systemd's `READY=1` (sent once both listeners are up) and **rolls back** to the
previous release if the new one does not come up. The tag is created only after a successful deploy.

### One-time host setup

```bash
# On the chosen vinhedo node, from a checkout or an extracted release:
sudo deploy/scripts/bootstrap.sh deploy github-proxy.example.com github-proxy-api.example.com
sudo certbot --nginx -d github-proxy.example.com -d github-proxy-api.example.com
```

`bootstrap.sh [deploy-user] [proxy-domain] [admin-domain]` creates the `github-proxy` service user,
`/opt/github-api-proxy`, `/etc/github-api-proxy`, the systemd unit, the trusted installer with its
sudoers rule and, when domains are given, the NGINX site. It prints the host key fingerprints for
`SSH_KNOWN_HOSTS`.

### Repository secrets (environment `production`)

| Secret | |
| --- | --- |
| `SSH_HOST`, `SSH_USER`, `SSH_PRIVATE_KEY`, `SSH_PORT` (optional) | The vinhedo node and the deploy user |
| `SSH_KNOWN_HOSTS` | `ssh-keyscan -p PORT HOST`, checked against bootstrap.sh's fingerprints |
| `GRIMOIRE_BASE_URL`, `GRIMOIRE_API_KEY`, `GRIMOIRE_ENVIRONMENT` (optional) | Rendered into `/etc/github-api-proxy/env` |

Moving the proxy to another node is a matter of running `bootstrap.sh` there and changing the `SSH_*`
secrets.

## Security notes

- Raw tokens are never stored or logged, only their SHA-256 fingerprint. Credentials passed as
  `access_token` / `client_secret` query parameters are redacted before storage.
- Request and response bodies are never stored. For GraphQL, only the operation name, or the first
  field of the query, is extracted.
- The proxy carries everyone's tokens in transit: serve it only over TLS (certbot) and keep the
  allow-list tight. The service runs as an unprivileged user with a hardened systemd unit.
- The deploy pins the host key, renders secrets with `umask 077`, and the installer never executes
  anything from the upload as root.

## Development

```bash
go test -race ./...
go vet ./...
# run locally without Grimoire: every key from the environment
Network__AllowedSources='*' MySql__ConnectionString='u:p@tcp(127.0.0.1:3306)/proxy' \
Admin__ApiToken=$(openssl rand -hex 32) go run ./cmd/github-api-proxy
```
