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
| `/mcp`                                        | POST/GET| `Authorization: Bearer …`   | MCP Streamable HTTP — `dispatch` tool (open or continue a thread); one stateless call per invocation |
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
- **MCP (agent)** — `ghs_…` installation tokens. Each host plugin's `dispatch`
  tool mints one with `gh auth token` in the session cwd for every call
  (`packages/envoy-client/src/dispatch-client.ts`); dispatch's `/mcp` extracts
  the bearer and uses it verbatim. The server never falls back to a stored token.

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

## Tool contract

`internal/dispatch/mcp/server.go` reads the `Authorization: Bearer` header of each
`tools/call` POST (`req.Extra.Header`) and builds a fresh `*github.Client` for that call; the
endpoint is stateless (`StreamableHTTPOptions{Stateless: true}`). No per-instance state, no
cached tokens.

One tool, two modes (`core.Dispatch`): `subject` opens a thread (`core.CreateThread`),
`thread` continues one as a follow-up comment (`core.ContinueThread`). `thread` cannot be
combined with `subject`, `urgency`, or `parent`. `context` (≤ 1200 chars) and `question`
(≤ 800) are required in both modes and refused, not truncated, when longer.

Repo resolution, first hit wins: a qualified `parent` or `thread` (`owner/name#n`) names its
own repo; otherwise the `repo` argument, which the calling plugin fills from the session's
working directory; neither → an error naming the fix. A bare-number `parent`/`thread`
resolves against the repo those rules pick. See `core/parent.go` (`ParseParent`,
`ParseThread`) and `core/thread.go`.

A follow-up target must be an open issue whose body carries a thread marker (`#N is not a
dispatch thread`, `#N is closed; open a new thread`). Dedupe: opening searches the repo's
issues for the request id (`githubapi.SearchByRequestID`); continuing lists the thread's
comments and returns the existing follow-up when one carries the same request id.

## Marker format

Thread metadata travels as **HTML comments** at the very start of issue bodies and
comments, `<!-- dispatch:<kind>\n<yaml>\n-->`, so GitHub and the dashboard show none of it.
The Go writer (`core/markers.go`) and the dashboard reader
(`packages/dispatch/web/src/markers.ts`) implement the same four kinds and must stay
byte-compatible; both readers also accept the legacy YAML-front-matter encoding.

- **thread** (issue body, written here) — `requestId`, `urgency`, optional `origin`
  (`host`/`machine`/`cwd`/`tmux`/`pane`/`sessionId`/`sessionTitle`, each omitted when
  empty), optional `ask` (each question carrying an `askId`), then `**<subject>**`,
  `## Context`, `## Question`.
- **ask** (comment, written here) — a follow-up turn: `requestId`, `origin` re-stamped at
  post time, `ask` with `askId`s, then `## Context`, `## Question`.
- **answer** (comment, written by the dashboard) — `forThread`, `forAsk`, `answers`, then a
  human-readable summary.
- **urgency** (comment, written by the dashboard) — `urgency`, then `Urgency set to **x**.`

`askId` = the turn's `requestId` for its first ask, `<requestId>.<i>` after that
(`core.AskIDFor`). String values containing `-->`, `<!--`, or `--!>` are emitted
double-quoted with those sequences `\u`-escaped (`commentSafeYAML`), so a marker is always
one comment.

Idempotency: opening `requestId = sha256(repo|parent|subject|context|question|urgency|ask)[:16]`
(`core.ComputeRequestID`), searched with `in:body "<id>"` under the `dispatch-thread` label
(GitHub indexes HTML-comment text); continuing
`requestId = sha256(follow-up|repo|thread|context|question|ask)[:16]`
(`core.ComputeFollowUpRequestID`), matched against the thread's `dispatch:ask` comments.

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

There is no `defaultRepo` or `appClientId` key. Repo comes from the calling
plugin (which fills it from the session's working directory) or an explicit
`repo` argument; App credentials live in `app.json`. Either removed key still
present in an `envoy.json` is a load error naming the key
(`config.InvalidConfigError`), not a silent skip.

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
(qualified parent, repo arg, bare-number parent, no-repo error), the
config load rejecting removed keys, follow-up posts a comment and no issue,
refuses non-threads/PRs/closed threads, dedupes over comments, prose caps,
mixed-mode rejection, both marker encodings, comment-delimiter escaping.
