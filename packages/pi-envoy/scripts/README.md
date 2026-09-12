# Pi Envoy diagnostic scripts

## smoke-delivery.sh

Real end-to-end delivery smoke test against the *installed* pi-legion-envoy
plugin. It launches a real, interactive `omp` session (TUI mode, never `-p`)
in a scratch tmux session and a throwaway directory outside any repo
checkout, so the only extension that loads is whatever is materialized at
`~/.omp/plugins/node_modules` — never a local source checkout via a repo's
own `omp.extensions` manifest. It drives the session with `tmux send-keys` to
call `envoy_role_set` and wait for a message, publishes to that role's
namespaced topic through the live Envoy HTTP API, asserts the session wrote
the exact payload to disk, then kills the session and asserts teardown: the
tmux session disappears and the role stops resolving (404) within 15 seconds.

### Usage

```bash
packages/pi-envoy/scripts/smoke-delivery.sh
```

Env overrides:

- `ENVOY_URL` — Envoy listener base URL (default `http://127.0.0.1:9020`).
- `OMP_BIN` — the `omp` binary to run (default `omp`, resolved on `PATH` —
  the dotfiles wrapper that injects provider auth).

Run from a host with:

- A live Envoy listener reachable at `ENVOY_URL`, with NATS behind it.
- The `pi-legion-envoy` plugin installed for every `omp` session on the
  machine, materialized at
  `~/.omp/plugins/node_modules/@sjawhar/pi-legion-envoy`. The script fails
  fast and names the path if that package.json is missing, is not valid
  JSON, or has no `.version` — the smoke is meaningless without the real
  installed artifact.
- A configured model provider: the script makes a real model call inside the
  spawned session, so it costs tokens and needs live credentials.

CI does not run this script — `.github/workflows/pr-and-main.yaml` has no
step for it, and none should be added while these prerequisites are unmet in
CI's sandbox.

## smoke-btw.sh

Real targeted Dispatch smoke against the installed plugin. It starts one isolated OMP TUI session,
waits for that session's exact work directory to appear in the live listener registry, and targets
only that session with a message on an existing open Dispatch issue. A correlated reply must appear
on the same issue before the script tears down the session and confirms its registry entry clears.
It never targets a role or an existing session.

### Usage

```bash
MODE=btw DISPATCH_SMOKE_ISSUE=CORE-1 \
  DISPATCH_AUTH_HEADER='Cookie: dispatch_session=…' \
  packages/pi-envoy/scripts/smoke-btw.sh
```

Set `MODE=btw` (the default) for an ephemeral reply, or `MODE=steer` to prove the primary-turn
delivery path. `ENVOY_URL` defaults to `http://127.0.0.1:9020`, `DISPATCH_URL` defaults to
`http://127.0.0.1:8766`, and `OMP_BIN` defaults to `omp`. `DISPATCH_SMOKE_ISSUE` names the open
issue that will receive the smoke card; `DISPATCH_AUTH_HEADER` supplies one human-authenticated
HTTP header, such as the current Dispatch cookie or a configured trusted identity header.

The script requires live Envoy, Dispatch, and model credentials; it is never a fake-listener test or
a CI step. A 404 from the targeted message path prints `SKIP` and exits zero, meaning the installed
Dispatch server predates these routes and the live smoke cannot be performed until this lane deploys.

### Timeouts

Role claim (60s) and post-kill teardown (15s) bound infrastructure round
trips only. Delivery (90s) is generous by comparison because that window
spans real Envoy delivery *plus* the time for a live model turn to notice the
steered message and write `received.txt` — an actual model call, not a fixed
mechanical step. Every wait is a wall-clock deadline (not a fixed iteration
count), so a blackholed Envoy fails at its stated timeout instead of quietly
stretching it.

### Exit codes

| Code | Meaning |
|------|---------|
| 0 | Role claimed, message delivered, and teardown (session gone, role 404) all verified within their timeouts. |
| 1 | Any assertion failed — see the `FAIL:` line for which one, plus a captured pane or HTTP status. |

Cleanup (`tmux kill-session`, `rm -rf` the temp dir) runs in an exit trap on
every exit path, so a failed run leaves nothing behind either.
