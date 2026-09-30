# Authentication and the HTTP API

`skill://dispatch` sends you here when you set up a Dispatch token, or need a route the tools do
not cover.

## Agent authentication

Use a personal Dispatch token: a human mints it in Dispatch **Settings → Agent tokens** and supplies
it to the agent through `dispatch.token` in `~/.config/opencode/envoy.json` or `DISPATCH_TOKEN`.
The server records the minting human as the owner of that session's writes. `DISPATCH_AGENT_TOKEN`
is the shared devbox fallback; do not configure it for an individual agent.

The deployed Dispatch server's browser origin is configured separately with
`DISPATCH_SERVER_URL` in the deployment `compose/.env`. Do not change an
agent's `envoy.json` to set the GitHub OAuth callback origin: the value must
be the exact URL humans type in their browser, and the GitHub App callback is
`<DISPATCH_SERVER_URL>/auth/callback`.

### Finding a route

The tools cover the everyday surface. For anything else, ask the server: `GET /api/v1` (no
credential) returns every route as `{method, path, auth, description}` sorted by path — `auth`
is `public`, `any` (a human or a bearer), `human` (a bearer gets `403 HUMAN_ONLY`), or `bearer`.
A path Dispatch does not serve under `/api` or `/v1` answers
`404 {"code":"NOT_FOUND","error":"no route for GET /v1/issues","hint":"GET /api/v1 lists every
route"}`; when you see that, you typed the path wrong — read the index rather than guessing. Every
`/api/v1` error body carries a `code`; branch on the code, never on the text.

A `GET` route that does not page refuses `limit`, `offset` and `cursor` with `400 INVALID_QUERY`
rather than ignoring them. `GET /api/v1/issues` is unpaginated and answers every matching issue;
to page it, call `dispatch_issues` with its `limit` and `offset`. The event logs page with `after`
or `before` and `limit`, and `GET /api/v1/search` takes a `limit` of at most 50 and has no next page.

