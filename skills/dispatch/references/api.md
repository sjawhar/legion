# Authentication and the HTTP API

`skill://dispatch` sends you here when you set up a Dispatch token, or need a route the `dispatch`
commands do not cover.

## Agent authentication

Use a personal Dispatch token: a human mints it in Dispatch **Settings → Agent tokens** and supplies
it to the agent through `DISPATCH_TOKEN` or `dispatch.token` in the `envoy.json` the agent's
session reads. Under an Oh My Pi profile other than `default`, a Claude Code `CLAUDE_CONFIG_DIR` or
an OpenCode `OPENCODE_CONFIG_DIR`, that is the `envoy.json` in the session's own directory; any
other session reads the file `ENVOY_CONFIG_FILE` names when it is set, else
`~/.config/legion/envoy.json` (under `$XDG_CONFIG_HOME` when set). The plugin hands the path it
chose to the session's commands in `ENVOY_CONFIG_FILE` beside `DISPATCH_HOST`, and the `dispatch`
command reads the file `ENVOY_CONFIG_FILE` names whenever `DISPATCH_HOST` is set, else its host
plugin's rule; in a Claude Code Bash that lacks both, as a subagent's can,
`CLAUDE_CODE_CHILD_SESSION=1` makes the host `claude`, so the command reads `CLAUDE_CONFIG_DIR`'s
file. A `dispatch.token` written `!<command>` is a command whose output is the token. A checkout's
`.envoy/envoy.json` may set `dispatch.serverUrl` only with the `dispatch.token` beside it, and
`dispatch.token` only with its own `dispatch.serverUrl` beside it; an `envoy` section there is not
read. The file's token goes only to its own `dispatch.serverUrl`: a
`DISPATCH_URL` naming another server needs `DISPATCH_TOKEN` or `DISPATCH_TOKEN_FILE`. The server
records the minting human as the owner of that session's writes.
`DISPATCH_AGENT_TOKEN` is the shared devbox fallback; do not configure it for an individual agent.

The deployed Dispatch server's browser origin is configured separately with
`DISPATCH_SERVER_URL` in the deployment `compose/.env`. Do not change an
agent's `envoy.json` to set the sign-in callback origin: the value must be the
exact URL humans type in their browser, and `<DISPATCH_SERVER_URL>/auth/callback`
is the callback registered on the sign-in pool's app client
(`DISPATCH_SIGNIN_CLIENT_ID`).

### Finding a route

The `dispatch` commands cover the everyday surface. For anything else, ask the server: `GET /api/v1` (no
credential) returns every route as `{method, path, auth, description}` sorted by path — `auth`
is `public`, `any` (a human or a bearer), `human` (a bearer gets `403 HUMAN_ONLY`), or `bearer`.
A path Dispatch does not serve under `/api` or `/v1` answers
`404 {"code":"NOT_FOUND","error":"no route for GET /v1/issues","hint":"GET /api/v1 lists every
route"}`; when you see that, you typed the path wrong — read the index rather than guessing. Every
`/api/v1` error body carries a `code`; branch on the code, never on the text.

`GET /api/v1/issues` answers every matching issue as an array, or one page when you name `limit`
(1–250) or `offset` (alone it pages 50): `{issues, total, limit, offset}`, where `total` counts
every issue your filters match. Walking the pages with the next `offset` is exact only while the
listing does not change: an issue that enters or leaves what the filters match, or whose status or
rank changes, between two reads shifts rows across a page boundary, and one issue comes back twice
and another never, even when you stop at `total`. The unpaged array is the only exact set one read
gives. `cursor` is `400 INVALID_QUERY`. `dispatch issues` pages for you.
The event logs page with `after` or `before` and `limit`, and `GET /api/v1/search` takes a `limit`
of at most 50 and has no next page. Every other route answers without paging and ignores a paging
parameter.

