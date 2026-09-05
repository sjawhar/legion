# Dispatch HTTP server

Go binary that serves the Dispatch dashboard (`packages/dispatch/web/dist`),
the GitHub OAuth web flow, a REST/GraphQL proxy, a Server-Sent Events
stream, and an MCP Streamable HTTP endpoint.

The dashboard is **multi-user, multi-repo**. Each authenticated GitHub user
sees threads across every repo their own GitHub App installations cover —
no manually maintained repo list — and keeps their own user-to-server
token. One Envoy GitHub App, installed wherever you want dispatch to
operate, backs the whole thing.

## One-time setup: create the Envoy App

Dispatch requires a single GitHub App that handles both:

- The dashboard's **user-to-server** OAuth flow (humans signing in)
- The agent path's **installation tokens** (bot acting on the user's behalf,
  minted by `gh auth token` in the calling session's working directory)

Create the App once at <https://github.com/settings/apps/new> with the
settings below. Replace `https://dispatch.example` with the public origin
your users browse to (e.g. `http://example-host-mx:8766` for a Tailnet
deployment).

| Setting                                              | Value                                          |
| ---------------------------------------------------- | ---------------------------------------------- |
| GitHub App name                                      | `Envoy` (or whatever you want to brand it as)  |
| Homepage URL                                         | `https://dispatch.example`                     |
| **Callback URL**                                     | `https://dispatch.example/auth/callback`       |
| Request user authorization (OAuth) during installation | unchecked — we trigger OAuth explicitly      |
| **Expire user authorization tokens**                 | **checked** — required for refresh tokens     |
| Webhook                                              | unchecked — envoy-listener handles webhooks   |
| Where can this App be installed                      | Any account (or single, your call)            |

### Permissions

Under **Permissions → Repository**:

| Permission     | Access       |
| -------------- | ------------ |
| Issues         | Read & write |
| Pull requests  | Read & write |
| Contents       | Read-only    |
| Metadata       | Read-only    |

No Organization, User, or Account permissions are needed.

### Capture credentials

After the App is created GitHub shows the App ID and Client ID, and you can:

1. Click **Generate a new client secret** → save the secret immediately (only
   shown once).
2. Click **Generate a private key** → downloads a `.pem` file.

### Install on each org you want to use Dispatch in

1. From the App settings page, click **Install App** in the left nav.
2. Pick the account/org. Choose either "All repositories" or "Only select
   repositories" and pick the ones to expose.
3. Repeat for any other accounts/orgs.

You can sanity-check installations with the `gh` CLI:

```sh
gh api /user/installations \
  --jq '.installations[] | {app_slug, account: .account.login, repos: .repository_selection}'
```

## Where credentials and state live

Dispatch supports two configurations: **file-backed** (local dev /
single-host) or **env-driven + NATS KV** (production: multi-replica,
ephemeral filesystem). Pick one; both are first-class.

### File-backed (dev, single host)

Dispatch reads `~/.local/share/dispatch/app.json` on startup. Hand-edited;
dispatch never modifies it. Mode `0600`:

```json
{
  "id": 3203456,
  "slug": "envoy",
  "name": "Envoy",
  "clientId": "Iv23liXXXXXXXXXXXXXX",
  "clientSecret": "REDACTED",
  "webhookSecret": "REDACTED-or-empty",
  "pem": "-----BEGIN RSA PRIVATE KEY-----\n…multiline PEM…\n-----END RSA PRIVATE KEY-----\n",
  "ownerLogin": "sjawhar",
  "permissions": {
    "issues": "write",
    "pullRequests": "write",
    "contents": "read",
    "metadata": "read"
  }
}
```

Per-user records land in `~/.local/share/dispatch/users/<login>.json` and
the HMAC session key in `~/.local/share/dispatch/signing-key`. Restart the
service to pick up `app.json` changes:

```sh
systemctl --user restart dispatch-server
```

### Env-driven + NATS KV (production)

On Fargate / ECS / k8s where the filesystem is ephemeral and you may run
more than one replica, configure dispatch via environment variables (most
sourced from a secrets manager via the task definition):

| Variable | Purpose |
|---|---|
| `DISPATCH_APP_CLIENT_ID` | Envoy App client_id |
| `DISPATCH_APP_CLIENT_SECRET` | Envoy App OAuth client_secret |
| `DISPATCH_APP_PEM_B64` | Base64-encoded private key PEM (multiline PEM through env is awkward, so it's base64 on the wire) |
| `DISPATCH_APP_ID` | Optional integer App ID |
| `DISPATCH_APP_SLUG` | Optional App slug |
| `DISPATCH_APP_NAME` | Optional App name |
| `DISPATCH_APP_WEBHOOK_SECRET` | Optional webhook secret |
| `DISPATCH_SIGNING_KEY` | HMAC signing key for `dsession` cookies. **Must be stable across deploys.** A fresh random value invalidates every session cookie on next container roll. |
| `DISPATCH_USER_STORE=kv` | Switch to NATS KV-backed user storage |
| `DISPATCH_USER_STORE_REPLICAS=3` | KV bucket replica count on first creation (default 1) |
| `NATS_URLS` | Picked up from envoy.json or `dispatch.natsUrls`; should point at the same cluster the envoy listener uses |
| `DISPATCH_WEB_DIST=/app/dispatch/web/dist` | Override the SPA build dir if you ship it elsewhere |

Env wins over file when both are present. A typical Fargate task pulls the
secret variables (`DISPATCH_APP_CLIENT_SECRET`, `DISPATCH_APP_PEM_B64`,
`DISPATCH_SIGNING_KEY`) from Secrets Manager and leaves the rest as plain
config.

User records in NATS KV live in bucket `dispatch_users`. Replication keeps
them durable across NATS peers; the bucket auto-creates on first start. No
filesystem state is required.

Pending OAuth state (the random `state` token attached to each
`/auth/start` redirect) is in-memory only — it has a multi-second lifetime,
so sticky-session on the load balancer handles multi-replica deployments.

If neither env nor file provides an App config, dispatch boots fine but
responds 503 on `/auth/start`, `/api/github/*`, and similar paths. The MCP
endpoint and `/healthz` continue to work.

## How users sign in

1. Open the dashboard at the public origin.
2. Click "Sign in with GitHub" → redirected to
   `github.com/login/oauth/authorize` for the Envoy App.
3. Approve → GitHub redirects to `/auth/callback?code=…&state=…`.
4. Dispatch exchanges the code for a user-to-server token, persists it to
   `~/.local/share/dispatch/users/<login>.json` (mode 0600), and sets the
   `dsession` cookie.
5. Dashboard loads, scoped to every repo the user's own App installations
   cover — no manual repo picker.

Token refresh is automatic and uses the user's stored refresh token plus the
App's `clientSecret`.

## On-disk layout

```
~/.local/share/dispatch/
├── app.json                   # Envoy App credentials (you write this once)
├── signing-key                # HMAC for dsession cookies (auto-created)
└── users/
    ├── sjawhar.json           # one file per authenticated GitHub user
    └── …
```

## Routes

| Path                                          | Method  | Auth                         | Purpose                                          |
| --------------------------------------------- | ------- | ---------------------------- | ------------------------------------------------ |
| `/auth/start`                                 | GET     | none                         | Redirect to GitHub OAuth (web flow)              |
| `/auth/callback`                              | GET     | none (state token)           | Exchange code, persist user, set session cookie  |
| `/auth/logout`                                | POST    | dsession cookie              | Remove user record + clear cookie                |
| `/auth/whoami`                                | GET     | dsession cookie              | Return logged-in GitHub login                    |
| `/api/events`                                 | GET     | dsession cookie              | SSE stream scoped to the user's App-installation owners |
| `/api/github/rest/...`                        | any     | dsession cookie              | Proxy to GitHub REST (auto-refresh)              |
| `/api/github/graphql`                         | POST    | dsession cookie              | Proxy to GitHub GraphQL (auto-refresh)           |
| `/api/installations`                          | GET     | dsession cookie              | Proxy `/user/installations`                      |
| `/api/installations/{id}/repositories`        | GET     | dsession cookie              | Proxy `/user/installations/{id}/repositories`    |
| `/api/view`                                   | GET     | dsession cookie              | Return current user's addressed-threads map      |
| `/api/view`                                   | PATCH   | dsession cookie              | Replace user's addressed-threads map              |
| `/mcp`                                        | POST/GET| `Authorization: Bearer …`   | MCP Streamable HTTP — `dispatch` tool (open or continue a thread); one stateless call per invocation |
| `/healthz`                                    | GET     | none                         | Liveness check                                   |
| `/...`                                        | GET     | none                         | SPA from `packages/dispatch/web/dist`            |

## MCP per-request auth

The `/mcp` endpoint is stateless and authenticates **per-request** via the
`Authorization: Bearer` header of each `tools/call` POST. The token is the
agent's own GitHub identity — an installation token the calling plugin's
`dispatch` tool mints with `gh auth token` for that one call — and is used
verbatim for every GitHub call made on behalf of the invocation. No MCP
session is kept between calls, and the server never falls back to the
dashboard user's stored token.

The one tool has two modes: `subject` opens a thread as a GitHub issue;
`thread` continues an existing one as a follow-up comment. Repo resolution,
first hit wins:

- A qualified `parent` or `thread` (`<owner>/<repo>#42`) names its own repo.
  The App must be installed on `<owner>/<repo>` with `issues: write`.
- Otherwise the `repo` argument (`<owner>/<name>`) is used. The calling
  plugin fills this from the session's working directory when it's a
  GitHub repo.
- Neither present → the tool errors naming the fix.

A bare-number `parent` or `thread` (`42`, or `42#1234567` for a parent)
resolves against whichever repo the rules above pick.

## Configuration

Reads `~/.config/opencode/envoy.json` and `<cwd>/.opencode/envoy.json` (repo
overrides user). Relevant keys:

- `dispatch.enabled` — whether the plugin surfaces the `dispatch` tool.
- `dispatch.serverUrl` — the dispatch server's public origin.
- `natsUrls` — list of NATS URLs (defaults to `nats://127.0.0.1:4222`).

There is no `defaultRepo` or `appClientId` key: repo comes from the calling
plugin (which fills it from the session's working directory) or an explicit
`repo` argument, and App credentials live in `app.json` (see above). An
`envoy.json` that still sets either key is rejected at load with an error
naming the key.

## Building and running

```sh
cd packages/envoy
go build -o dispatch ./cmd/dispatch
./dispatch
```

The systemd unit at `~/.config/systemd/user/dispatch-server.service` runs
this binary. Restart with `systemctl --user restart dispatch-server`.

`DISPATCH_INSECURE_COOKIE=1` removes the `Secure` cookie flag for plain-HTTP
Tailnet testing. Do **not** set this in production.

## Tests

```sh
go test -timeout 30s ./internal/dispatch/...
```

Covers HMAC session cookies, user record I/O, app config I/O, SSE
hub broadcast/owner-scoping/slow-client drop, meta-marker round trip
(including `origin`), parent regex cases, `CreateThread` repo resolution
(qualified parent, repo arg, bare-number parent, no-repo error), the
config load rejecting removed keys, follow-up comments (posted without a new
issue, refused on non-threads/PRs/closed threads, deduped over comments),
prose caps, mixed-mode rejection, both marker encodings, and
comment-delimiter escaping.
