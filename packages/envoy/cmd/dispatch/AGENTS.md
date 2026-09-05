# Dispatch HTTP Server (Go)

Go binary serving the Dispatch SPA, GitHub OAuth web flow + REST/GraphQL
proxy, an SSE stream, an MCP Streamable HTTP endpoint, and per-user
addressed-thread state.

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
| `/api/events`                                 | GET     | dsession cookie              | SSE stream scoped to the user's App-installation owners |
| `/api/github/rest/...`                        | any     | dsession cookie              | Proxy to GitHub REST (per-user, auto-refresh)    |
| `/api/github/graphql`                         | POST    | dsession cookie              | Proxy to GitHub GraphQL (per-user, auto-refresh) |
| `/api/installations`                          | GET     | dsession cookie              | Proxy `/user/installations`                      |
| `/api/installations/{id}/repositories`        | GET     | dsession cookie              | Proxy `/user/installations/{id}/repositories`    |
| `/api/view`                                   | GET     | dsession cookie              | Return user's addressed-threads map              |
| `/api/view`                                   | PATCH   | dsession cookie              | Replace user's addressed-threads map             |
| `/mcp`                                        | POST/GET| `Authorization: Bearer …`   | MCP Streamable HTTP — `dispatch` tool            |
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

## Multi-user, owner-scoped discovery

Each authenticated user has:

- A `Tokens` struct (access + refresh + expiries + login)
- An `Addressed` map of `<owner>/<repo>#<number>` → the `updatedAt` at the
  moment they marked the thread addressed

All written to `users/<login>.json` (mode 0600, parent dir 0700).

There is no watched-repos list. `/api/events` calls `/user/installations`
server-side (`githubapi.InstallationOwners`) and registers the SSE client with
the set of GitHub account logins (users/orgs) the caller's App
installations cover. The SSE hub fans out GitHub events to clients whose
owner set contains the event's repo owner. Repo is derived from the NATS
subject (`notifications.github.<owner>.<repo>.…`). Empty owner set → no
events. Operators don't pre-configure which repos or owners dispatch
covers — it's whatever the App is installed on.

The NATS consumer subscribes to `notifications.github.>` (broader than the
old single-repo subscription); filtering happens at the SSE-fanout layer.

## MCP per-request auth pattern

`internal/dispatch/mcp/server.go` defines a `bearerMiddleware` that extracts
the `Authorization: Bearer` header and stashes it in `context.Context`. The
`dispatch` tool handler reads it back and builds a fresh `*github.Client`
for each call. No per-instance state, no cached tokens.

The tool's repo resolution, first hit wins:

- A qualified `parent` (`<owner>/<repo>#42`) names its own repo, overriding
  `repo`.
- Otherwise the `repo` argument is used. The MCP shim fills it from the
  calling session's working directory when it's a GitHub repo.
- Neither present → the tool errors: `no repo: pass repo=owner/name (the
  shim fills it from the working directory when one is a GitHub repo)`.

A bare-number `parent` (`42` or `42#<commentId>`) resolves against
whichever repo the rules above pick. See `core/parent.go` for the regex and
`core/thread.go`'s `CreateThread` for the resolution order.

## Marker format

Thread metadata travels as **YAML frontmatter** at the top of issue bodies and
comments — not HTML comments (the original plan's HTML+base64 scheme was
superseded; see the note atop `.omo/plans/2026-05-22-dispatch.md`). The Go
writer (`core/markers.go`) and the dashboard reader
(`packages/dispatch/web/src/markers.ts`) implement this identically and must
stay byte-compatible.

- **Thread meta** (issue body) — `urgency`, `requestId`, optional `origin`
  (`Origin` — `host`/`machine`/`cwd`/`tmux`, each omitted when empty),
  optional `ask` (`QuestionInfo[]` as raw YAML), then `## Context` and
  `## Question` sections:

  ```
  ---
  urgency: med
  requestId: <16-hex>
  origin:
    host: omp
    cwd: /home/ubuntu/legion
  ---

  **<subject>**

  ## Context

  <context>

  ## Question

  <question>
  ```

- **Urgency change** (comment) — `kind: urgency`, `urgency: <level>`; latest wins.
- **Answer** (comment) — `kind: answer`, `forThread: <n>`,
  `answers: QuestionAnswer[]`.

Idempotency: `requestId = sha256(repo|parent|subject|context|question|urgency|ask)[:16]`
(`core.ComputeRequestID`). The dedupe search looks for that exact token in the
issue body, scoped to the `dispatch-thread` label
(`githubapi.BuildRequestIDQuery`) — the search string and the emitted marker
must reference the same `requestId`, or retries create duplicate threads.

## Answer delivery (AC#4)

The Go server is stateless and has no agent session context, so it does not
route answers itself. Each adapter closes the loop with the shared
`@legion/envoy-client/dispatch-subscribe` helper: the OpenCode plugin's
`tool.execute.after` hook and the OMP extension's `tool_result` hook subscribe
the calling session to `notifications.github.<owner>.<repo>.issue.<thread>.>`
after a successful dispatch call. When a human replies on the thread, the
envoy listener publishes that comment to the topic and Envoy delivers it back
to the originating agent session — including a Legion role's session, which
survives kill/resume because Legion resurrection resumes the same OMP session
file.

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
| `~/.local/share/dispatch/users/<login>.json` | 0600 | Per-user tokens + addressed-threads map            |

**Env + NATS KV** (production / multi-replica / ephemeral filesystem):

- App credentials from `DISPATCH_APP_CLIENT_ID`, `DISPATCH_APP_CLIENT_SECRET`, `DISPATCH_APP_PEM_B64`, etc.
- Signing key from `DISPATCH_SIGNING_KEY` (must be stable across deploys).
- Per-user records in NATS JetStream KV bucket `dispatch_users` when `DISPATCH_USER_STORE=kv`. Uses the same NATS cluster as the envoy listener; `internal/store/kv.go` is the pattern reference.
- Pending OAuth state stays in-memory; sticky-session on the LB covers multi-replica.

Env wins over file when both are present.

## Configuration

Reads `~/.config/opencode/envoy.json` and `<cwd>/.opencode/envoy.json`
(repo overrides user). Relevant keys:

- `dispatch.enabled` — whether the plugin surfaces the `dispatch` tool.
- `dispatch.serverUrl` — the dispatch server's public origin.
- `natsUrls` — list of NATS URLs (defaults to `nats://127.0.0.1:4222`).

There is no `defaultRepo` or `appClientId` key. Repo comes from the MCP
shim (which fills it from the calling session's working directory) or an
explicit `repo` argument; App credentials live in `app.json`. Either
removed key still present in an `envoy.json` is a load error naming the
key (`config.InvalidConfigError`), not a silent skip.

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
SSE hub broadcast/owner-scoping/slow-client drop, meta-marker round trip
(including `origin`), parent regex cases, `CreateThread` repo resolution
(qualified parent, repo arg, bare-number parent, no-repo error), and the
config load rejecting removed keys.
