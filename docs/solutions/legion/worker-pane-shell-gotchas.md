---
title: "Worker-pane shell gotchas: the credential line the model imitated (fixed in LEGION-12) and the credential's 60-second lifetime, env-dependent tests in the daemon suite, jj split's bookmark placement, the box's hanging git credential helper, a role topic with no Envoy holder, a bash-bridge outage, a daemon outage blocking every bash call, and a pane OMP_SESSION_ID that is not yours"
category: legion
tags:
  - legion
  - worker
  - legion-grant
  - bash
  - jj
  - bun-test
  - rig
  - session-identity
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
  - "sjawhar/legion#974"
  - "LEGION-16"
  - "LEGION-14"
  - "sjawhar/legion#952"
  - "LEGION-29"
  - "sjawhar/legion#970"
  - "LEGION-13"
  - "sjawhar/legion#978"
symptoms:
  - "git: Unable to redeem LEGION_GRANT (403) on jj git push / legion gh / legion handoff complete, more often as a session goes on (every pi-envoy release through 1.12.0, before the one that carries LEGION-12)"
  - "several credential blocks at the top of one bash call's command text, with placeholder, repeated, or non-uuid ids after the first"
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
  - "The Legion PR footer names a session id three days older than the worker; printenv OMP_SESSION_ID disagrees with envoy_whoami"
---

# Worker-Pane Shell Gotchas

Things every phase worker on `sjawhar/legion` hits in a worker pane or on the smoke rig. Sections 1 and 3 are from
LEGION-9 (planner, implementer, tester, and reviewer each rediscovered the first one); 4–6 are from LEGION-22; 7–8 are
from LEGION-18; the 60-second grant lifetime in §1, the `packages/daemon` note in §2, and §9 are from LEGION-14, whose
four workers hit §1–§3 again; the `packages/pi-envoy` note in §2 and the independent confirmation of §1's cause are
from LEGION-29; the §2 environment-argument paragraph and §10 are from LEGION-13 (sjawhar/legion#978). None was part of
the scope of the issue whose workers hit it. Section 1's cause was found and fixed by LEGION-12 (pull request #974): its
workarounds are recorded only so a worker still running the old plugin recognizes them, and must not be used on the
fixed one. §2's check-config leak was fixed by LEGION-13. §7 is filed as LEGION-29; until it is fixed, that section is
the workaround.

## 1. Credential lines multiplying in a bash call: the model was copying its own transcript (fixed in LEGION-12)

**What was seen.** The Legion extension for Oh My Pi (`packages/pi-envoy/extensions/legion.ts`) attaches a one-time
credential to every bash call a phase worker makes: it mints a grant from the daemon and, until the release that
carries LEGION-12 (every `@sjawhar/pi-legion-envoy` version through 1.12.0), put it at the top of the command as shell
text — an `export` line setting `LEGION_GRANT`, three `unset` lines, the isolated `GH_CONFIG_DIR`, and a `PATH` with
the worker's `gh` shim first. Over a session the number of such blocks at the top of one call grew (1 → 10 in one
LEGION-9 implementer session; 2–4 per call on LEGION-29). Only the **first** block's grant ever redeemed; every later
one answered `Unable to redeem LEGION_GRANT (403)`. Because the last `export` wins in a shell, `jj git push` (through
the credential helper), `legion gh`, and `legion handoff complete` failed whenever a later block existed.

**What was actually happening.** The hook never multiplied. Oh My Pi writes a hook's revised tool input back into
the assistant message (`prepareToolCallDispatch` in the agent loop), so the credential block appeared in the
transcript as text the model itself had written. On later calls the model read its own transcript and copied the
block into new commands with made-up or stale ids — a literal `'...'` placeholder, a counting pattern like
`1f2e3d4c-5b6a-…`, or a verbatim copy of an earlier call's id. The hook's block was always first and always real;
the imitations followed it; the shell's last `export` won; `legion …` ran under an id the daemon had never minted.
The count grew as the context filled with more examples. Measured on the LEGION-12 rig with a stand-in daemon:
one hook invocation and one grant mint per bash call, every time, while the blocks in the command text climbed
1,2,1,2,2,2,3,3,4,… (see `packages/pi-envoy/scripts/grant-rig/`). LEGION-29's implementer and tester reached the same
conclusion independently: they probed each block's id against the daemon, every block after the first was one the
daemon had never issued, and the same command sent once through eval's `tool.bash` redeemed first time. The LEGION-16
controller transcript (an interactive terminal session, no `task` spawns) is a third case: its third bash call already
carried a `'...'` placeholder block in a single-line form the hook never wrote. Once the controller owns the merge
queue, this failure mode breaks `legion gh -- pr merge` intermittently, not only worker pushes and phase reports.

**How to tell a stale id from an unminted one.** A grant lives 60 seconds and redeems any number of times inside
that window (`GRANT_TTL_MS` in `packages/daemon/src/daemon/api.ts`; `resolveGrant` in `api/auth.ts` checks
existence and expiry, nothing else). So a 403 within seconds of the call means the id was never minted — imitation,
not expiry.

**The 60-second lifetime is a separate gotcha, and it stays live after the fix.** The hook mints the grant when the
bash call starts, before your command runs, and it expires 60 seconds later whichever way it is delivered. A slow
command ahead of the one that redeems it burns the lifetime: on LEGION-14, `jj bookmark set … && jj git push …` in one
call, with the `bookmark set` taking about forty seconds on a loaded box, made the push's first credential-helper call
return the same `Unable to redeem LEGION_GRANT (403)` with a perfectly good grant. (That particular push still landed
on a later helper call; do not count on it.) Put the command that redeems the grant — `legion gh`, `jj git push`,
`legion handoff complete`, `legion credential` — **first** in its bash call, or alone in one. The recipe in section 3
puts the bookmark move and the push in the same call; on a loaded box, split them.

**The fix (pi-envoy 1.8.x — the post-merge task fills in the released version; LEGION-12, pull request #974).** The
grant now travels in the bash tool's per-command `env` (the `env` argument every Oh My Pi bash call accepts), together
with the cleared `GH_TOKEN`/`GITHUB_TOKEN`/`GH_HOST`, the isolated `GH_CONFIG_DIR`, and the shim-first `PATH`. The
command text is never touched, so nothing credential-shaped is written back into the transcript for the model to copy.
The hook's keys are spread last, so even a model that imitates a previous call's `env` object cannot displace the
grant minted for the current call. Oh My Pi applies that `env` to the one command only; nothing enters the persistent
shell.

**On a fixed plugin, do not use the old workarounds.** Three were recorded for the old plugin: the `export()` shell
function that kept only the first credential per call (LEGION-9, kept at `/tmp/legion9-grant-trap.sh` on the rig), the
`pickgrant` probe that tried every seen grant against the daemon (LEGION-22), and pinning the first block's id inline
as `LEGION_GRANT=<that-uuid> legion gh -- …` (LEGION-18). On the fixed plugin all three are obsolete: there is no
credential text left for them to read, and redefining `export` in the persistent shell only obscures later failures.
Recognize them by their names and delete them. On a pane still running the old plugin, the LEGION-18 form is the
simplest and needs no shell function: when a call 403s, read the *first* credential line the hook prepended to that
call and name that id explicitly on the next command — an explicit assignment on the command line outranks every
prepended `export`, and an unredeemed grant stays valid for its 60 seconds.

**One caveat.** A worker session resumed from a transcript recorded before the fix still shows the model its own
old credential blocks and may keep writing them for a while, and so may a model that re-runs an earlier command
verbatim from its transcript. Such a line is text the shell executes, so for that one call it overrides the grant the
hook put in `env`, and `legion …` still 403s — the fix removes the seed, it cannot rewrite what the model already
sees. Command text starts at `cd -- "$LEGION_WORKSPACE" && …`; never begin it with a credential block. The imitations
stop once the old examples age out of context; a fresh session never sees one.

**Verify a grant without side effects** (still true): `printf 'protocol=https\nhost=github.com\n' | legion credential get`
inside the same bash call — a `username=…` line is good, `403` is stale.

## 2. `bun test` in a pane: inject the env the code reads, and run it from `packages/daemon`

Every Legion pane carries `DISPATCH_URL` and `DISPATCH_TOKEN_FILE` without `DISPATCH_TOKEN`, plus the `LEGION_*` and
`ENVOY_*` families, so a test that reaches `process.env` through a helper with no env seam fails wherever the pane's
env differs from CI's. `legion start --check-config` takes its environment as an argument
(`cmdCheckConfig(project, configPath, env)` in `src/cli/index.ts`; the citty `start`/`restart` handlers are the only
callers that pass `process.env`), and its tests hand it `{ PATH, HOME }`. LEGION-13 (sjawhar/legion#978) introduced
that argument; before it, `loadStartConfig` hardcoded `process.env` and the test failed in every pane. Prefer that seam
whenever the code under test can take one; the pane's shape is then irrelevant to the suite.

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
  exits 0. The completion **is** recorded: the daemon cleared the active phase for routing, PATCHed the issue's Dispatch
  status (`legion state` showed `LEGION-18` at `testing` right after), and parked the summary for the architect's
  catch-up (`phases[<KEY>].completed`, the API's 202 path). Do not write a second handoff file. Do re-run the same
  `legion handoff complete` once the architect holds its role again — a repeat completion by the same worker is the
  designed recovery (200, published, record cleared); a repeat while the role is still unheld just 202s again. What
  the architect can and cannot do about it is in
  [phase-complete-stranded-on-no-holder](phase-complete-stranded-on-no-holder.md).
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

Section 8's symptom has a specific cause worth knowing: the same tool-call hook that attaches the grant to each bash call (section 1)
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

## 10. `$OMP_SESSION_ID` in your pane is not your session

The Legion PR footer (`<!-- legion: {"session":"<session-id>","phase":"<phase>"} -->`) needs this session's live id.
`printenv OMP_SESSION_ID` in a worker pane returns an id inherited from whatever OMP session started the daemon: the
daemon's private tmux server is forked from the daemon's own environment, every pane inherits it, and the daemon's
strip list (`PANE_SECRET_ENV_KEYS` in `packages/daemon/src/daemon/environment.ts`) covers secrets only. On LEGION-13
the pane's value decoded (UUIDv7, first 48 bits are epoch millis) to 2026-09-09T01:51Z, three days before the worker
was spawned; the implementer's two review-thread replies went out with that id in their footer and had to be edited.

Get the id from `envoy_whoami` (`session_id`) or from `legion state` → `roles["legion-<project>-<KEY>-<role>"].sessionId`;
the two agree, and both are the id the daemon registered at `/worker/started`. Never from the environment, and — per
[session-id-remint-stale-transcript-identity](../envoy/session-id-remint-stale-transcript-identity.md) — never from a
`whoami` result earlier in your own transcript either.
