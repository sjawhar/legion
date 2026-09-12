---
title: "Worker-pane shell gotchas: stacked LEGION_GRANT exports, the pane's DISPATCH_URL in the daemon test suite, and jj split's bookmark placement"
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
  - "LEGION-29"
  - "sjawhar/legion#970"
symptoms:
  - "git: Unable to redeem LEGION_GRANT (403) on jj git push / legion gh / legion handoff complete"
  - "legion start --check-config > validates github_apps.<role>.private_key_command fails only inside a Legion pane"
  - "Refusing to move bookmark backwards or sideways: legion/<KEY> after jj split"
---

# Worker-Pane Shell Gotchas

Three things every phase worker on `sjawhar/legion` hit during LEGION-9 (planner, implementer, tester, and reviewer
each rediscovered the first one). None is part of any issue's scope; the first is re-filed to the controller as a rig bug.
Until it is fixed, this is the workaround.

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

**LEGION-29 evidence points the other way on attribution.** Its implementer and tester both saw the same
`Unable to redeem LEGION_GRANT (403)` with 2–4 `export LEGION_GRANT='…'` blocks per call, and the tester's
`notesForReviewer` traced the duplicates to the *model's own command text* — the assistant re-emitting the
previous call's injected prelude verbatim — not to the extension: the same command issued once through eval's
`tool.bash` redeemed first time. Whether LEGION-9's 1 → 10 growth had the same cause is not established by either
record. Either way the workaround is the same, and simpler than the trap above when you can see your own
command: never replay a prelude line, and if a call already carries several, pin the *first* block's uuid inline
on the command that needs it (`LEGION_GRANT=<first-uuid> legion gh -- …`) — grants live 60 s and are not single-use,
so the first one is still valid for the whole call.

## 2. The pane's `DISPATCH_*` and `LEGION_*` leak into test suites that read `process.env`

Every Legion pane carries `DISPATCH_URL` and `DISPATCH_TOKEN_FILE` (but not `DISPATCH_TOKEN`) plus its own
`LEGION_TREE`/`LEGION_ROLE`/`LEGION_ISSUE`/`LEGION_GENERATION`/`LEGION_BOOT_TOKEN_FILE`. Any suite whose fixtures
read `process.env` instead of an isolated map fails inside a pane while staying green in CI.

- `packages/daemon`: `src/cli/__tests__/index.test.ts` › `legion start --check-config > validates
  github_apps.<role>.private_key_command without executing it` — `resolveDaemonConfig` refuses on the inherited
  `DISPATCH_URL`. Still open: run `env -u DISPATCH_URL -u DISPATCH_TOKEN_FILE bun test` and say so in the handoff.
- `packages/pi-envoy` (fixed on `legion/LEGION-29`, #970): `envoy.test.ts` cleared `DISPATCH_TOKEN` but not
  `DISPATCH_TOKEN_FILE`, which `resolveDispatchConfig` reads *ahead* of the token (see
  [secret-file-pointer-precedence](../integration-patterns/secret-file-pointer-precedence.md)), so three
  Dispatch-tool tests registered real tools against a stub zod; `legion.test.ts` inherited the pane's tree/role/issue
  markers, so nine tests died on `Legion session has both controller and tree launch markers`. Both suites now
  clear the whole variable family in `beforeEach` (the existing `environmentKeys` list in `legion.test.ts`;
  `DISPATCH_TOKEN_FILE` added beside `DISPATCH_URL`/`DISPATCH_TOKEN` in `envoy.test.ts`) and restore it in
  `afterEach`. Rule: clear the variable *family the resolver consumes*, in its precedence order — clearing the
  familiar name and leaving the file-pointer alive disables nothing.

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
