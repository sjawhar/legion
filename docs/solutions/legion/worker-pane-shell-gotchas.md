---
title: "Worker-pane shell gotchas: stacked LEGION_GRANT exports, the pane's DISPATCH_URL in the daemon test suite, jj split's bookmark placement, and the box's hanging git credential helper"
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
symptoms:
  - "git: Unable to redeem LEGION_GRANT (403) on jj git push / legion gh / legion handoff complete"
  - "legion start --check-config > validates github_apps.<role>.private_key_command fails only inside a Legion pane"
  - "Refusing to move bookmark backwards or sideways: legion/<KEY> after jj split"
  - "rig daemon's first jj git clone killed at the 30 s runner timeout; launchFailures 1; tree queued"
  - "legion handoff write: Handoff data field schemaVersion is not allowed"
---

# Worker-Pane Shell Gotchas

Things every phase worker on `sjawhar/legion` hits in a worker pane or on the smoke rig. Sections 1–3 are from
LEGION-9 (planner, implementer, tester, and reviewer each rediscovered the first one); 4–6 and the §1 alternative are
from LEGION-22. None is part of any issue's scope; §1 is re-filed to the controller as a rig bug. Until it is fixed,
this is the workaround.

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

Two observations for whoever fixes the hook: the count is per session, not per tool; and in this shell (bash
5.2.37), inside a function, `builtin export "$@"` with an expanded `NAME=value` word returned 0 without binding the
variable — a plain assignment followed by `builtin export NAME` did. Cause not investigated.

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

## 2. The pane's `DISPATCH_URL` fails one pre-existing CLI test (fixed in #967)

Every Legion pane carries `DISPATCH_URL` (and `DISPATCH_TOKEN_FILE`) but not `DISPATCH_TOKEN`.
`src/cli/__tests__/index.test.ts` › `legion start --check-config > validates github_apps.<role>.private_key_command
without executing it` read `process.env` rather than an isolated env, so `resolveDaemonConfig` refused
(`dispatch_url is set but DISPATCH_TOKEN is not`) and the test failed in every pane while staying green in CI.
sjawhar/legion#967 (`wxzknkyk`) made that `describe` scrub `LEGION_*`/`DISPATCH_*`/`ENVOY_*` around its cases, so
`bun test packages/daemon` runs clean from a pane on branches that include it. On older branches the workaround is
still `env -u DISPATCH_URL -u DISPATCH_TOKEN_FILE bun test`, and say so in the handoff. The general rule stands: a CLI
test that reaches `process.env` through a helper with no env seam will fail wherever the pane's env differs from CI's.

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
