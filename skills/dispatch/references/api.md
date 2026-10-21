# Authentication and the HTTP API

Read this to set up a Dispatch token, or to call a route no `dispatch` command covers.

## Agent authentication

- Use a personal token. A human mints it in Dispatch **Settings → Agent tokens**. Supply it as `DISPATCH_TOKEN` or as `dispatch.token` in the session's `envoy.json`. The server records that human as the owner of your writes.
- Which `envoy.json`: the session's own directory under an Oh My Pi profile other than `default`, a Claude Code `CLAUDE_CONFIG_DIR` or an OpenCode `OPENCODE_CONFIG_DIR`. Otherwise the file `ENVOY_CONFIG_FILE` names, else `~/.config/legion/envoy.json` (under `$XDG_CONFIG_HOME` when set).
- The plugin passes its choice in `ENVOY_CONFIG_FILE` with `DISPATCH_HOST`. In a Claude Code Bash that lacks both, `CLAUDE_CODE_CHILD_SESSION=1` makes the command read `CLAUDE_CONFIG_DIR`'s file. A Legion pane reads the `DISPATCH_TOKEN_FILE` path the daemon sets.
- A `dispatch.token` written `!<command>` is a command that prints the token.
- A checkout's `.envoy/envoy.json` sets `dispatch.serverUrl` and `dispatch.token` only together. Its `envoy` section is not read. A file's token goes only to its own server: a `DISPATCH_URL` for another server needs `DISPATCH_TOKEN` or `DISPATCH_TOKEN_FILE`.
- Never configure `DISPATCH_AGENT_TOKEN` (the shared devbox fallback) for one agent.
- Never set the sign-in callback origin in an agent's `envoy.json`. It is `DISPATCH_SERVER_URL` in the deployment `compose/.env`: the exact URL humans type. `<DISPATCH_SERVER_URL>/auth/callback` is registered on the sign-in client (`DISPATCH_SIGNIN_CLIENT_ID`).

## Finding a route

- `GET /api/v1` (no credential) lists every route as `{method, path, auth, description}`. `auth` is `public`, `any`, `human` (a bearer gets `403 HUMAN_ONLY`) or `bearer`.
- `404 NOT_FOUND` with "GET /api/v1 lists every route" means a wrong path. Read the index, do not guess.
- Branch on an error's `code`, never its text.
- `GET /api/v1/issues` returns every match unpaged, or one page with `limit` (1–250) or `offset`: `{issues, total, limit, offset}`. Only the unpaged array is exact: a page walk can repeat or skip an issue that changes. `cursor` is `400 INVALID_QUERY`. `dispatch issues` pages for you.
- Event logs page with `after` or `before` and `limit`. `GET /api/v1/search` takes `limit` up to 50 with no next page. Other routes do not page.
