# Dispatch HTTP Server (Go)

Go binary serving the Dispatch SPA, GitHub OAuth web flow + REST/GraphQL
proxy, an SSE stream, an MCP Streamable HTTP endpoint, and per-user
watched-repos state.

See `README.md` next to this file for the operator setup checklist (creating
the Envoy GitHub App, dropping `app.json` in place, installing on orgs).
This file is the **agent-facing** description of the code.

## Routes

| Path                                          | Method  | Auth                         | Purpose                                          |
| --------------------------------------------- | ------- | ---------------------------- | ------------------------------------------------ |
| `/auth/start`                                 | GET     | none                         | Redirect to github.com/login/oauth/authorize     |
| `/auth/callback`                              | GET     | none (state token)           | Exchange code, persist user, set session cookie  |
| `/auth/logout`                                | POST    | dsession cookie              | Remove user record + clear cookie                |
| `/auth/whoami`                                | GET     | dsession cookie              | Return logged-in GitHub login                    |
| `/api/events`                                 | GET     | dsession cookie              | SSE stream filtered by user's watched-repos      |
| `/api/github/rest/...`                        | any     | dsession cookie              | Proxy to GitHub REST (per-user, auto-refresh)    |
| `/api/github/graphql`                         | POST    | dsession cookie              | Proxy to GitHub GraphQL (per-user, auto-refresh) |
| `/api/installations`                          | GET     | dsession cookie              | Proxy `/user/installations`                      |
| `/api/installations/{id}/repositories`        | GET     | dsession cookie              | Proxy `/user/installations/{id}/repositories`    |
| `/api/view`                                   | GET     | dsession cookie              | Return user's watched-repos                      |
| `/api/view`                                   | PATCH   | dsession cookie              | Replace user's watched-repos list                |
| `/mcp`                                        | POST/GET| `Authorization: Bearer …`   | MCP Streamable HTTP — `envoy_dispatch` tool      |
| `/healthz`                                    | GET     | none                         | Liveness check                                   |
| `/...`                                        | GET     | none                         | SPA from `packages/dispatch/web/dist/`           |

The legacy device-flow endpoints (`POST /auth/login`, `GET /auth/status`)
are gone. So is `routes/setup.go` (manifest-flow bootstrap was wrong for
this deployment shape — operators register the App by hand once, see
README).

## Auth model

Two distinct identity surfaces share the same Envoy App but never share a
token:

- **Dashboard (human)** — `ghu_…` user-to-server tokens issued via the
  web-flow OAuth dance. One per GitHub login, refreshable for ~6 months
  with the App's `clientSecret`. Stored in
  `~/.local/share/dispatch/users/<login>.json`.
- **MCP (agent)** — `ghs_…` installation tokens. The shim
  (`packages/envoy-plugin/bin/dispatch-mcp-shim.ts`) mints them via
  `gh-app-token` for each request; dispatch's `/mcp` extracts the bearer
  and uses it verbatim. The server never falls back to a stored token.

The same Envoy App serves both surfaces. Installation tokens are scoped by
installation; user-to-server tokens are scoped by what the user authorized.
Both inherit the App's permissions.

## Multi-user / multi-repo

Each authenticated user has:

- A `Tokens` struct (access + refresh + expiries + login)
- A `WatchedRepos []string` list of `<owner>/<repo>` slugs

All written to `users/<login>.json` (mode 0600, parent dir 0700).

The SSE hub fans out GitHub events to clients whose `WatchedRepos` set
contains the event's repo. Repo is derived from the NATS subject
(`notifications.github.<owner>.<repo>.…`). Empty watched set → no
events. Operators don't pre-configure which repos dispatch covers; users
add their own from the dashboard.

The NATS consumer subscribes to `notifications.github.>` (broader than the
old single-repo subscription); filtering happens at the SSE-fanout layer.

## MCP per-request auth pattern

`internal/dispatch/mcp/server.go` defines a `bearerMiddleware` that extracts
the `Authorization: Bearer` header and stashes it in `context.Context`. The
`dispatch` tool handler reads it back and builds a fresh `*github.Client`
for each call. No per-instance state, no cached tokens.

The tool's `parent` argument accepts:

- `42` or `42#<commentId>` — bare number form, uses `dispatch.defaultRepo`
- `<owner>/<repo>#42` — fully qualified, overrides the default

See `core/parent.go` for the regex.

## Marker format

Thread metadata travels as **YAML frontmatter** at the top of issue bodies and
comments — not HTML comments (the original plan's HTML+base64 scheme was
superseded; see the note atop `.omo/plans/2026-05-22-dispatch.md`). The Go
writer (`core/markers.go`) and the dashboard reader
(`packages/dispatch/web/src/markers.ts`) implement this identically and must
stay byte-compatible.

- **Thread meta** (issue body) — `urgency`, `requestId`, optional `ask`
  (`QuestionInfo[]` as raw YAML):

  ```
  ---
  urgency: med
  requestId: <16-hex>
  ---

  **<subject>**

  <body>
  ```

- **Urgency change** (comment) — `kind: urgency`, `urgency: <level>`; latest wins.
- **Answer** (comment) — `kind: answer`, `forThread: <n>`,
  `answers: QuestionAnswer[]`.

Idempotency: `requestId = sha256(parent|subject|body|urgency|ask)[:16]`
(`core.ComputeRequestID`). The dedupe search looks for that exact token in the
issue body, scoped to the `dispatch-thread` label
(`githubapi.BuildRequestIDQuery`) — the search string and the emitted marker
must reference the same `requestId`, or retries create duplicate threads.

## Answer delivery (AC#4)

The Go server is stateless and has no OpenCode session context, so it does not
route answers itself. The envoy-plugin's `tool.execute.after` hook
(`packages/envoy-plugin/src/dispatch-subscribe.ts`) subscribes the calling
session to `notifications.github.<owner>.<repo>.issue.<thread>.>` after a
successful `envoy_dispatch`. When a human replies on the thread, the envoy
listener publishes that comment to the topic and Envoy delivers it back to the
originating agent session.

## State and credentials

Two storage shapes; selection is in `cmd/dispatch/main.go` via
`loadAppCredentials` and `openUserStore`. The router takes a
`routes.AppContextOptions` bundle and is agnostic to which backend
supplied each piece.

**File-backed** (dev / single-node):

| File                                         | Mode | Purpose                                           |
| -------------------------------------------- | ---- | ------------------------------------------------- |
| `~/.local/share/dispatch/app.json`           | 0600 | Envoy App credentials (operator hand-writes once) |
| `~/.local/share/dispatch/signing-key`        | 0600 | HMAC key for the `dsession` cookie                |
| `~/.local/share/dispatch/users/<login>.json` | 0600 | Per-user tokens + watched-repos                   |

**Env + NATS KV** (production / multi-replica / ephemeral filesystem):

- App credentials from `DISPATCH_APP_CLIENT_ID`, `DISPATCH_APP_CLIENT_SECRET`, `DISPATCH_APP_PEM_B64`, etc.
- Signing key from `DISPATCH_SIGNING_KEY` (must be stable across deploys).
- Per-user records in NATS JetStream KV bucket `dispatch_users` when `DISPATCH_USER_STORE=kv`. Uses the same NATS cluster as the envoy listener; `internal/store/kv.go` is the pattern reference.
- Pending OAuth state stays in-memory; sticky-session on the LB covers multi-replica.

Env wins over file when both are present.

## Configuration

Reads `~/.config/opencode/envoy.json` and `<cwd>/.opencode/envoy.json`
(repo overrides user). Relevant keys:

- `dispatch.defaultRepo` — optional `<owner>/<name>`, used only as the
  fallback target for bare-number parents in the `envoy_dispatch` tool.
  The dashboard ignores this — each user picks their own watched repos.
- `natsUrls` — list of NATS URLs (defaults to `nats://127.0.0.1:4222`).

`dispatch.appClientId` is **ignored**; App credentials live in `app.json`.

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

Covers HMAC session cookies, user record I/O round trip, App config I/O,
SSE hub broadcast/scoping/slow-client drop, meta-marker round trip, parent
regex cases (including the `<owner>/<repo>#<n>` form), and the config
user+repo merge.
