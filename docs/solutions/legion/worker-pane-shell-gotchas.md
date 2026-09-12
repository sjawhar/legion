---
title: "Worker-pane shell gotchas: stacked LEGION_GRANT exports and their 60-second lifetime, env-dependent tests in the daemon suite, jj split's bookmark placement, the box's hanging git credential helper, a role topic with no Envoy holder, a bash-bridge outage, and a daemon outage blocking every bash call"
category: legion
tags:
  - legion
  - worker
  - legion-grant
  - bash
  - jj
  - bun-test
  - rig
date: 2026-09-12
status: active
module: legion
related_issues:
  - "LEGION-9"
  - "sjawhar/legion#945"
  - "LEGION-22"
  - "sjawhar/legion#967"
  - "LEGION-18"
  - "sjawhar/legion#953"
  - "LEGION-12"
  - "LEGION-14"
  - "sjawhar/legion#952"
  - "LEGION-29"
  - "sjawhar/legion#970"
symptoms:
  - "git: Unable to redeem LEGION_GRANT (403) on jj git push / legion gh / legion handoff complete"
  - "the same 403 on the FIRST grant of a call, after a slow jj command ran ahead of the push"
  - "bun test from the repository root: hundreds of 'document is not defined' and ECONNREFUSED failures outside the changed package"
  - "Refusing to move bookmark backwards or sideways: legion/<KEY> after jj split"
  - "rig daemon's first jj git clone killed at the 30 s runner timeout; launchFailures 1; tree queued"
  - "legion handoff write: Handoff data field schemaVersion is not allowed"
  - "legion handoff complete: Unable to report phase completion (403): Invalid or expired grant"
  - "[handoff] Warning: phase recorded; no architect was live to receive the summary"
  - "envoy_publish: no holder for role legion-<project>-<KEY>-architect"
  - "bash tool: Unable to connect. Is the computer able to access the url?"
  - "Unable to connect. Is the computer able to access the url? on every bash tool call, whatever the command"
---

# Worker-Pane Shell Gotchas

Things every phase worker on `sjawhar/legion` hits in a worker pane or on the smoke rig. Sections 1 and 3 are from
LEGION-9 (planner, implementer, tester, and reviewer each rediscovered the first one); 4–6 and the §1 alternative are
from LEGION-22; 7–8 and the §1 per-call workaround are from LEGION-18; the 60-second grant lifetime in §1, the
`packages/daemon` note in §2, and §9 are from LEGION-14, whose four workers hit §1–§3 again; the §1 attribution
finding and the `packages/pi-envoy` note in §2 are from LEGION-29. None is part of any
issue's scope; §1 is filed as LEGION-12 (a rig bug in the pi-envoy extension's tool-call hook) and §7 as LEGION-29.
Until they are fixed, these are the workarounds.

## 1. Stacked `export LEGION_GRANT=…` lines: only the first per call redeems

The pi-envoy `legion.ts` tool-call hook prepends `export LEGION_GRANT='<uuid>'` (plus `unset GH_TOKEN …`,
`GH_CONFIG_DIR`, `PATH`) to every bash call. The number of prepended blocks grows over a session — observed 1 → 10 in
one implementer session. Only the **first** grant of each call redeems; the rest 403 (`Unable to redeem LEGION_GRANT
(403)`). Since the last `export` wins, `jj git push` (credential helper), `legion gh`, and `legion handoff complete`
fail once N > 1.

A `trap … DEBUG` in the persistent shell does **not** observe the injected lines, so it cannot capture the first
grant. A shell function shadowing `export` does. Source this once per session (kept at `/tmp/legion9-grant-trap.sh`
on the rig; recreate it if gone):

```bash
export() {
  case "$1" in
    LEGION_GRANT=*)
      if [ -z "${_legion_grant_lock:-}" ]; then
        _legion_grant_lock=1; _legion_grant_n=1
        LEGION_GRANT="${1#LEGION_GRANT=}"; builtin export LEGION_GRANT
      else
        _legion_grant_n=$((_legion_grant_n + 1))
      fi ;;
    *) builtin export "$@" ;;
  esac
}
grant_release() {
  echo "[grants this call: ${_legion_grant_n:-0}; used ${LEGION_GRANT:0:8}]"
  unset _legion_grant_lock; _legion_grant_n=0
}
```

End every command body with `; grant_release` so the next call's first grant is accepted. Verify a grant without
side effects: `printf 'protocol=https\nhost=github.com\n' | LEGION_GRANT=<g> legion credential get` — a
`username=…` line is good, `403` is stale.

**Grants also expire 60 seconds after they are minted** (`GRANT_TTL_MS` in `packages/daemon/src/daemon/api.ts`), and
the hook mints them at the start of the bash call, before your command runs. So a slow command ahead of the
grant-consuming one can burn the whole lifetime: on LEGION-14, `jj bookmark set … && jj git push …` in one call, with
the `bookmark set` taking about forty seconds on a loaded box, made the push's first credential-helper call return the
same `Unable to redeem LEGION_GRANT (403)` even though the shadowing function above had correctly kept the first
grant. (That particular push still landed on a later helper call; do not count on it.) Put the command that redeems
the grant — `legion gh`, `jj git push`, `legion handoff complete`, `legion credential` — **first** in its bash call, or
alone in one. The recipe in section 3 below puts the bookmark move and the push in the same call; on a loaded box, split
them.

Two observations for whoever fixes the hook: the count is per session, not per tool; and in this shell (bash
5.2.37), inside a function, `builtin export "$@"` with an expanded `NAME=value` word returned 0 without binding the
variable — a plain assignment followed by `builtin export NAME` did. Cause not investigated.

**Simpler per-call workaround (LEGION-18):** an injected grant that was never redeemed stays valid for minutes. When
a call 403s, read the *first* `export LEGION_GRANT='…'` line the tool prepended to that call and name it explicitly on
the next one — `LEGION_GRANT=<that-uuid> legion gh -- …`, `LEGION_GRANT=<that-uuid> jj -R "$LEGION_WORKSPACE" git push …`,
`LEGION_GRANT=<that-uuid> legion handoff complete …`. An explicit assignment on the command line outranks every
prepended `export`. This worked on every retry across three LEGION-18 rounds (pushes, thread replies, thread resolves,
`pr edit`, `pr checks --watch`, `handoff complete`).

**Do not paste the preamble yourself.** The hook prepends its block to whatever command text you send. If your own
text contains a copy of an earlier call's block (easy when re-running a previous command verbatim), that stale copy
is the last `export` and wins — the same 403 with only one *injected* grant in sight. Command text starts at
`cd -- "$LEGION_WORKSPACE" && …`.

**LEGION-29 evidence on where the extra blocks come from.** Its implementer and tester both saw 2–4
`export LEGION_GRANT='…'` blocks per call and traced every block after the first to the *model's own command text*:
the assistant re-emits the previous call's injected prelude, and the extra blocks often carry UUIDs that never
existed on the daemon (a `POST /legion/v1/gh-token` probe of each returned `Invalid or expired grant` for all but
the first block, which redeemed for its whole 60 s). The same command issued once through eval's `tool.bash`
redeemed first time. So in that session the hook injected exactly one grant per call and only the replayed copies
403'd; whether LEGION-9's 1 → 10 growth had the same cause is not established by either record. The per-call
workaround above (pin the first block's uuid inline) covers both cases.

**Alternative (LEGION-22): probe the grants instead of locking on the first.** Grants are reusable for their whole
60 s TTL (`GRANT_TTL_MS` in `api.ts`; `resolveGrant` checks expiry only), so a shell can record every grant the hook
injected and, right before a credentialed command, keep the first one the daemon accepts:

```bash
export() { case "$1" in LEGION_GRANT=*) LEGION_GRANTS_SEEN="${LEGION_GRANTS_SEEN:+$LEGION_GRANTS_SEEN }${1#LEGION_GRANT=}";; esac; builtin export "$@"; }
pickgrant() {
  local g
  for g in ${LEGION_GRANTS_SEEN:-}; do
    if LEGION_GRANT="$g" legion gh -- api rate_limit >/dev/null 2>&1; then builtin export LEGION_GRANT="$g"; unset LEGION_GRANTS_SEEN; return 0; fi
  done
  unset LEGION_GRANTS_SEEN; return 1
}
```

Define both once (the persistent shell can be reset between rounds — if `type -t pickgrant` prints nothing, define them
again), then `pickgrant && jj git push …` / `pickgrant && legion gh -- …` / `pickgrant && legion handoff complete …`.
It needs no `grant_release` bookkeeping and self-heals if a later grant is the live one. Observed 1 → 10 stacked
blocks over one implementer session; the first block redeemed every time.

## 2. `bun test` in a pane: inject the env the code reads, and run it from `packages/daemon`

Every Legion pane carries `DISPATCH_URL` and `DISPATCH_TOKEN_FILE` without `DISPATCH_TOKEN`, plus the `LEGION_*` and
`ENVOY_*` families, so a test that reaches `process.env` through a helper with no env seam fails wherever the pane's
env differs from CI's. `legion start --check-config` takes its environment as an argument
(`cmdCheckConfig(project, configPath, env)` in `src/cli/index.ts`; the citty `start`/`restart` handlers are the only
callers that pass `process.env`), and its tests hand it `{ PATH, HOME }`. Prefer that seam whenever the code under
test can take one; the pane's shape is then irrelevant to the suite.

The same leak hit `packages/pi-envoy`, whose extension entry points read `process.env` themselves, on LEGION-29
(fixed in sjawhar/legion#970): `envoy.test.ts` cleared `DISPATCH_TOKEN` but not `DISPATCH_TOKEN_FILE`, which
`resolveDispatchConfig` reads *ahead* of the token (see
[secret-file-pointer-precedence](../integration-patterns/secret-file-pointer-precedence.md)), so three Dispatch-tool
tests registered real tools against the fixture's stub zod; `legion.test.ts` inherited the pane's
`LEGION_TREE`/`LEGION_ROLE`/`LEGION_ISSUE` markers, so nine tests died on `Legion session has both controller and
tree launch markers`. Both suites now clear the whole variable family in `beforeEach` (the existing `environmentKeys`
list in `legion.test.ts`; `DISPATCH_TOKEN_FILE` beside `DISPATCH_URL`/`DISPATCH_TOKEN` in `envoy.test.ts`) and restore
it in `afterEach`. Clear the variable *family the resolver consumes*, in its precedence order — clearing the familiar
name and leaving the file-pointer alive disables nothing.

Run it from `packages/daemon`, which is the `working-directory` of the `test` job in
`.github/workflows/pr-and-main.yaml` (that job also sets `LEGION_E2E=1` and `LEGION_TMUX_LIVE=1`). There is no root
test script, and `bun test` from the repository root is not a CI entry point: it picks up every package, and the
per-package `bunfig.toml` preloads do not apply from the root, so `packages/dispatch/web` fails by the hundreds with
`document is not defined`, and the `pi-envoy` and `claude-envoy-bridge` suites fail with `ECONNREFUSED` for want of a
live NATS broker. LEGION-14's tester spent a diagnosis cycle on 262 such failures, none in the changed package. When
an assignment says "run the root suite", run the daemon package's suite and say which invocation you used.

## 3. `jj split` leaves the bookmark on the empty working copy

Workers commit with `jj split -m '…' <explicit paths>` so the extension-provisioned, uncommitted `.omp/config.yml`
never enters a commit. After a split the issue bookmark sits on the **remaining** half — the new, undescribed working
copy — not on the commit you just described. `jj bookmark set legion/<KEY>` then refuses (`Refusing to move bookmark
backwards or sideways`). Before every push:

```bash
jj -R "$LEGION_WORKSPACE" bookmark set legion/<KEY> -r @- --allow-backwards
jj -R "$LEGION_WORKSPACE" git push --bookmark legion/<KEY>
```

The remote still moves **forward** — jj 0.45's push summary reads `Changes to push to origin:` followed by
`bookmark: legion/<KEY> [move forward from <old> to <new>]`; `--allow-backwards` only
concerns the local pointer stepping from the empty child to its described parent. Check `jj diff -r @- --stat` first:
the described commit must hold exactly the paths you named.

Related: a reviewer's local commit in the shared workspace (its push 403s — the review App has no `contents`
permission) rides along on the implementer's next push; verify with `jj log` that it is an ancestor before building
on it.

## 4. The box's global git credential helper hangs; the rig daemon's clone dies at the runner timeout

`~/.gitconfig` on the rig box sets `credential.helper = !gh auth git-credential`. When `gh` blocks on the D-Bus secret
service (observed 30–90 s, and an 8 s probe timing out), any git operation the smoke-rig daemon runs *through the
global config* — its first `jj git clone` of the workspace — is killed at the 30 s runner timeout: `launchFailures 1`,
the tree re-queued, a half-written clone directory left behind. Legion's own credential path (`legion credential`,
configured per workspace) is unaffected; only the daemon's global-config fallback hits it.

Workaround used by the LEGION-21 and LEGION-22 testers: run the rig daemon with
`GIT_CONFIG_GLOBAL=<helper-free copy of ~/.gitconfig>` from its first boot (copy `~/.gitconfig`, drop the
`[credential]` section). If the clone already died, remove the half-written workspace directory before restarting;
admission reconciliation re-spawns the tree on boot. Diagnose with `timeout 8 git credential fill <<<$'protocol=https\nhost=github.com'`
— a hang, not a prompt, is the symptom.

## 5. Smoke-rig root issues: create them only once the daemon and bridge are live

`up.sh` in `envoy` mode creates the root Dispatch issue **after** the daemon and the envoy bridge report ready, for a
reason: the bridge relays live NATS traffic only, and `reduceIssueUpdated` ignores keys the daemon has never seen.
A root created by hand before `RIG READY` (e.g. to dodge `ensure_root_issue`'s `POSSIBLE_DUPLICATE` 409 against earlier
smoke roots) never enters the daemon's state and the controller never triages it; checkpoints 1–4 then wait forever.
Create it after `RIG READY` with `force: true` and write the key to `${SMOKE_DIR}/root-issue`, or ice the stray one
and create another. The rig is single-occupancy (shared ports 19370/19371/19020/14222, shared `LEGSMOKE` project,
shared private tmux server): message the other worker before `up.sh` and run `down.sh` when finished.

## 6. `legion handoff write` rejects the ledger's own fields

Re-writing a phase handoff from the existing `.legion/<phase>.json` (e.g. adding a `round2` key) fails with
`Handoff data field schemaVersion is not allowed`: the ledger adds `schemaVersion`, `phase`, and `completed` itself.
Strip them first — `jq 'del(.schemaVersion, .phase, .completed)'` — and pass the rest as `--data`.

## 7. Your role topic has no Envoy holder

After the daemon or the Envoy listener restarts (LEGION-29: a listener restart drops every role claim), `envoy_role_get`
can return `no holder` for both your own role and the tree's architect. Two consequences:

- `legion handoff complete` prints `[handoff] Warning: phase recorded; no architect was live to receive the summary` and
  exits 0. The completion **is** recorded: the daemon captured and cleared the phase, PATCHed the issue's Dispatch
  status (`legion state` showed `LEGION-18` at `testing` right after), and parked the summary for the architect's
  catch-up (`phases[<KEY>].completed`, the API's 202 path). Do not re-run it; do not write a second handoff.
- `envoy_publish` to `notifications.role.legion-<project>-<KEY>-architect` fails with `no holder for role …`. Fall back
  to the architect's session id: `legion state` → `roles["legion-<project>-<KEY>-architect"].sessionId`, then
  `envoy_send(session_id=<that id>, message=…)`. Direct session delivery does not depend on the role claim. The
  architect reached this implementer the same way, and asked for the final-push summary by `envoy_send` as well.

## 8. The bash tool bridge drops out mid-phase

For several minutes during LEGION-18 every `bash` call returned `Unable to connect. Is the computer able to access
the url?`. The `eval` Python kernel kept working but is spawned **without** the pane environment (no `LEGION_*`,
no `JJ_CONFIG`, no `GH_CONFIG_DIR`). Recover it from the OMP process, which is the kernel's parent:

```python
import os, subprocess
raw = open(f"/proc/{os.getppid()}/environ", "rb").read().split(b"\0")
env = dict(kv.decode().split("=", 1) for kv in raw if b"=" in kv)
ws = env["LEGION_WORKSPACE"]
subprocess.run(["jj", "-R", ws, "split", "-m", "…", "<path>"], cwd=ws, env=env, check=True)
```

With that `env`, file edits and `jj split` commits behave exactly as from the pane: `JJ_CONFIG` is present, so every
commit still carries the `Omp-Session:` trailer and the role's bot author (verified on #953's four text commits, all
made this way). What the kernel cannot do is redeem a grant — `legion gh`, `jj git push`, and `legion handoff complete`
need the per-call `LEGION_GRANT` the bash hook injects — so queue those until the bridge returns.

## 9. While the daemon's API is down, every bash tool call fails before your command runs

Section 8's symptom has a specific cause worth knowing: the same tool-call hook that prepends the grant (section 1)
must mint it from the daemon at `LEGION_DAEMON_URL` (`http://127.0.0.1:13370` on this rig) before the bash command
starts. If the daemon is restarting — it did three times during LEGION-14's implement phase, unrelated to the branch
under work, and was fully down for seventeen minutes — the hook fails and the tool returns `Unable to connect. Is the
computer able to access the url?` for every bash call, whatever the command was. Nothing you type in the command
changes that.

What still works, and what to do:

- **The `eval` tool and file tools are unaffected.** Probe the port from `eval` (`socket.connect(("127.0.0.1", 13370))`)
  or start a supervised watcher through `hub` that polls the port and prints a marker when it opens, then `hub wait` on
  that marker. Do not spin in a foreground loop.
- **`legion handoff write` needs no daemon** (it writes `.legion/<phase>.json` under the workspace), and neither does a
  local `jj` commit. During the outage the implementer wrote its handoff through an `eval` subprocess carrying the
  pane's exact environment, read from `/proc/<omp-pid>/environ` of this session's own `omp` process (section 8's
  recipe), plus the `JJ_CONFIG` overlay the extension adds at session start
  (`<LEGION_STATE_DIR>/omp-attribution-<session-id>.toml`, which is what puts the `Omp-Session:` trailer on the
  commit). Confirm the trailer matches an earlier commit of yours before relying on it.
- **Anything that redeems a grant must wait**: pushing, `legion gh`, `legion handoff complete`. Grants minted before the
  restart are gone with the old process's memory.
- **Never restart, signal, or write to the daemon yourself.** It runs under a supervisor from
  `/home/ubuntu/legion-ws-RunDaemon` and comes back on its own; it runs `main`, not your branch, so its restarts are
  never evidence about your change.
