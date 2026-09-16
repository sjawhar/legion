---
title: "Worker-pane shell gotchas: the credential line the model imitated (fixed in LEGION-12), the env delivery that secretsd's bash tool dropped (LEGION_GRANT is missing on 1.17.1–1.17.2, fixed in LEGION-54), the credential's 60-second lifetime, env-dependent tests in the daemon suite, jj split's bookmark placement, the box's hanging git credential helper, a role topic with no Envoy holder, a bash-bridge outage, a daemon outage blocking every bash call, a pane OMP_SESSION_ID that is not yours, a phase completion refused with 409 after a respawn, a pane `legion` that is the deployed build, not your branch, a bash tool `jq` that is jaq, not the jq your script runs, a `(divergent)` change left behind by `jj squash` on the shared operation log, a workspace `.git` pointer that breaks `gh` from inside the workspace and a signing config that drops every signature (after the daemon moved to its own user), and a plan's `actionlint`/`yq`/`go` that are not on the pane PATH"
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
  - jq
  - jaq
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
  - "LEGION-52"
  - "LEGION-54"
  - "sjawhar/legion#992"
  - "LEGION-16"
  - "LEGION-14"
  - "sjawhar/legion#952"
  - "LEGION-29"
  - "sjawhar/legion#970"
  - "LEGION-13"
  - "sjawhar/legion#978"
  - "LEGION-37"
  - "LEGION-17"
  - "sjawhar/legion#956"
  - "LEGION-52"
  - "LEGION-34"
  - "sjawhar/legion#1003"
  - "LEGION-78"
  - "sjawhar/legion#1015"
  - "LEGION-40"
  - "sjawhar/legion#1011"
  - "LEGION-53"
  - "sjawhar/legion#1028"
  - "LEGION-84"
  - "sjawhar/legion#1080"
  - "LEGION-96"
  - "sjawhar/legion#1086"
  - "LEGION-99"
  - "sjawhar/legion#1089"
  - "LEGION-131"
  - "sjawhar/legion#1106"
  - "LEGION-164"
  - "LEGION-173"
symptoms:
  - "git: Unable to redeem LEGION_GRANT (403) on jj git push / legion gh / legion handoff complete, more often as a session goes on (every pi-envoy release through 1.16.0, before the one that carries LEGION-12)"
  - "several credential blocks at the top of one bash call's command text, with placeholder, repeated, or non-uuid ids after the first"
  - "the same 403 on the FIRST grant of a call, after a slow jj command ran ahead of the push"
  - "legion gh / jj git push / legion handoff complete: LEGION_GRANT is missing on every call (pi-envoy 1.17.1-1.17.2 under the secretsd plugin)"
  - "bun test from the repository root: hundreds of 'document is not defined' and ECONNREFUSED failures outside the changed package"
  - "Refusing to move bookmark backwards or sideways: legion/<KEY> after jj split"
  - "rig daemon's first jj git clone killed at the 30 s runner timeout; launchFailures 1; tree queued"
  - "legion handoff write: Handoff data field schemaVersion is not allowed"
  - "legion handoff complete: Unable to report phase completion (409): Phase for <KEY> is no longer owned by this worker"
  - "legion handoff complete: Unable to report phase completion (403): Invalid or expired grant"
  - "[handoff] Warning: phase recorded; no architect was live to receive the summary"
  - "envoy_publish: no holder for role legion-<project>-<KEY>-architect"
  - "bash tool: Unable to connect. Is the computer able to access the url?"
  - "Unable to connect. Is the computer able to access the url? on every bash tool call, whatever the command"
  - "The Legion PR footer names a session id three days older than the worker; printenv OMP_SESSION_ID disagrees with envoy_whoami"
  - "jj log shows (divergent) next to your commit after jj squash --into; a second visible commit shares its change id"
  - "Error: Change ID `xxxx` is divergent — Hint: Use change offset to select single revision"
  - "the pane's `legion gh` performed the write the branch's shim refuses, and skill:// served the template the branch fixed"
  - "packages/claude-envoy-bridge bun test: expected Bearer reply-token, received the pane's real Dispatch token (40 pass, 1 fail in a pane; 41 pass elsewhere)"
  - "the pane's `legion handoff write` accepted a payload the branch's schema refuses; the committed handoff was written by the deployed build"
  - "spawn_worker: POST /legion/v1/worker/spawn failed with 400: requestId: Invalid input: expected string, received undefined (a sub-architect pane on a pre-release plugin)"
  - "jq --version prints jaq 2.3.0 in the bash tool; a jq expression that passed there fails (or a failing one passes) when the script runs"
  - "Error: cannot use null as iterable (array or object) from jq on a missing key, in the bash tool only"
  - "legion gh -- pr create --repo … --head …: failed to run git: fatal: not a git repository: /home/ubuntu/.local/state/legion/…/.git/worktrees/<workspace>, from inside the workspace"
  - "error: command not found: actionlint / yq / go in a worker pane, for a tool the plan's verify steps name"
---

# Worker-Pane Shell Gotchas

Things every phase worker on `sjawhar/legion` hits in a worker pane or on the smoke rig. Sections 1 and 3 are from
LEGION-9 (planner, implementer, tester, and reviewer each rediscovered the first one); 4–6 are from LEGION-22; 7–8 are
from LEGION-18; the 60-second grant lifetime in §1, the `packages/daemon` note in §2, and §9 are from LEGION-14, whose
four workers hit §1–§3 again; the `packages/pi-envoy` note in §2 and the independent confirmation of §1's cause are
from LEGION-29; the §2 environment-argument paragraph and §10 are from LEGION-13 (sjawhar/legion#978); §11 is from
LEGION-12's own retro and is filed as LEGION-37; the slow-push paragraph in §1 is from LEGION-17 (sjawhar/legion#956),
whose implementer, on a pane still running the pre-LEGION-12 plugin, hit §1, §3, §8, §9, and §11 across five rounds
and confirmed each as written (the architect filed the §1 harness fix as LEGION-52); §13 and the §12 correction are
from LEGION-34 (sjawhar/legion#1003), whose implementer hit §1 again on a pane running the fixed plugin — by copying
the block into its own command text — and whose new CLI subcommand could only be exercised live from the workspace;
§14 is from LEGION-40 (sjawhar/legion#1011), whose plan, harness comments, and shipped header comment all named the
wrong `jq`; §16 is from LEGION-84 (sjawhar/legion#1080), and its list of which `gh` calls must leave the workspace is
from LEGION-96 (sjawhar/legion#1086), whose implementer hit it on its first `pr create` the same day; §17 is from
LEGION-99 (sjawhar/legion#1089), whose plan's verify steps named three tools the pane lacks.
None was part of
the scope of the issue whose workers hit it. Section 1's cause was found by LEGION-12 (pull request #974) and its
delivery fixed for good by LEGION-54 (pi-envoy 1.20.1): the workarounds are recorded only so a worker still running
the 1.17.0 plugin recognizes them, and must not be used on the fixed one. §2's check-config leak was fixed by
LEGION-13. §7 is filed as LEGION-29; until it is fixed, that section is
the workaround.

## 1. Credential lines multiplying in a bash call: the model was copying its own transcript (fixed in LEGION-12)

**What was seen.** The Legion extension for Oh My Pi (`packages/pi-envoy/extensions/legion.ts`) attaches a one-time
credential to every bash call a phase worker makes: it mints a grant from the daemon and, until the release that
carries LEGION-12 (every `@sjawhar/pi-legion-envoy` version through 1.16.0), put it at the top of the command as shell
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
puts the bookmark move and the push in the same call; on a loaded box, split them. The push *itself* can outlive
the grant too: on LEGION-17 a `jj git push` alone, first in its call, took 102 seconds while the box's one-minute
load average was 889 on 32 cores, and every credential-helper call inside it 403'd once the 60 seconds were up
(the same push took 22 seconds and landed at a load average of 187, and under 3 seconds at 110). Nothing in the
command is at fault; wait for the load to fall and re-run the single push. Do not add a retry loop around it — a
loop that redeems a fresh grant per attempt only works because each attempt is a new bash call, and that is the
model re-issuing the command, not a script.

**The fix (pi-envoy 1.20.1; LEGION-54, after LEGION-12's pull request #974).** Before each of your bash commands
runs, the hook mints the grant and writes it to the file `$LEGION_GRANT_FILE` names — a 0600 file under
`<state_dir>/secrets/`, written to a temp name and renamed into place — and `legion credential`, `legion gh`, and
`legion handoff complete` read that file first (`LEGION_GRANT` is only a manual fallback when the pointer is unset).
The command text is never touched, so nothing credential-shaped is written back into the transcript for the model to
copy; nor is the bash tool's `env` argument: LEGION-12's release (1.17.1) delivered the grant there, and it failed on
the first real worker because the `secretsd` plugin replaces the bash tool with Oh My Pi's legacy `{command, timeout}`
shim, which drops `env` — every `legion …` command answered `LEGION_GRANT is missing` until the profile was pinned
back to 1.17.0 (LEGION-52). A bash tool's fields belong to whichever plugin installed it; the file does not. The
static settings — the cleared `GH_TOKEN`/`GITHUB_TOKEN`/`GH_HOST`, the isolated `GH_CONFIG_DIR`, the shim-first
`PATH` — are on the pane's environment from the daemon at spawn, for the pane's life. One sentence of history: on
1.17.1 (env delivery) nothing set in one bash call survived into the next; on file delivery the persistent shell
behaves as a shell does.

**On a fixed plugin, do not use the old workarounds.** Three were recorded for the old plugin: the `export()` shell
function that kept only the first credential per call (LEGION-9, kept at `/tmp/legion9-grant-trap.sh` on the rig), the
`pickgrant` probe that tried every seen grant against the daemon (LEGION-22), and pinning the first block's id inline
as `LEGION_GRANT=<that-uuid> legion gh -- …` (LEGION-18). On the fixed plugin all three are obsolete: there is no
credential text left for them to read, and redefining `export` in the persistent shell only obscures later failures.
Recognize them by their names and delete them. The grant file itself is never to be read, printed, or copied by hand:
`legion` reads it, and a model that echoes it into a command has put a credential into its own transcript. On a pane
still running the 1.17.0 plugin, the LEGION-18 form is the simplest and needs no shell function: when a call 403s,
read the *first* credential line the hook prepended to that call and name that id explicitly on the next command —
an explicit assignment on the command line outranks every prepended `export`, and an unredeemed grant stays valid
for its 60 seconds.

**One caveat.** A worker session resumed from a transcript recorded under the 1.17.0 plugin still shows the model its
own old credential blocks and may keep writing them for a while, and so may a model that re-runs an earlier command
verbatim from its transcript. With `LEGION_GRANT_FILE` set on the pane, `legion` ignores that shell variable, so the
line is harmless text — but it is still a credential-shaped line in the transcript. Command text starts at
`cd -- "$LEGION_WORKSPACE" && …`; never begin it with a credential block. The imitations stop once the old examples
age out of context; a fresh session never sees one.

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
name and leaving the file-pointer alive disables nothing. The third package to carry the same leak is
`packages/claude-envoy-bridge`: `tests/envoy-channel-server.test.ts` expects its fixture's `Bearer reply-token` on the
reply it posts to Dispatch and, in a pane, receives the real token from `DISPATCH_TOKEN_FILE` instead (LEGION-131's
implementer, rebuilding the bridge bundles: `40 pass, 1 fail`; `env -u DISPATCH_URL -u DISPATCH_TOKEN_FILE -u
DISPATCH_TOKEN bun test` → `41 pass`). Filed as LEGION-173; until it lands, run that package's suite with the three
variables unset and say so in the proof.

Run it from `packages/daemon`, which is the `working-directory` of the `test` job in
`.github/workflows/pr-and-main.yaml` (that job also sets `LEGION_E2E=1` and `LEGION_TMUX_LIVE=1`). There is no root
test script, and `bun test` from the repository root is not a CI entry point: it picks up every package, and the
per-package `bunfig.toml` preloads do not apply from the root, so `packages/dispatch/web` fails by the hundreds with
`document is not defined`, and the `pi-envoy` and `claude-envoy-bridge` suites fail with `ECONNREFUSED` for want of a
live NATS broker. LEGION-14's tester spent a diagnosis cycle on 262 such failures, none in the changed package. When
an assignment says "run the root suite", run the daemon package's suite and say which invocation you used.

## 3. `jj split` leaves the bookmark on the empty working copy

Workers commit with `jj split -m '…' <explicit paths>` (the worker skill's completion gate). After a split the issue
bookmark sits on the **remaining** half — the new, undescribed working copy — not on the commit you just described.
`jj bookmark set legion/<KEY>` then refuses (`Refusing to move bookmark backwards or sideways`). Before every push:

```bash
jj -R "$LEGION_WORKSPACE" bookmark set legion/<KEY> -r @- --allow-backwards
jj -R "$LEGION_WORKSPACE" git push --bookmark legion/<KEY>
```

The remote still moves **forward** — jj 0.45's push summary reads `Changes to push to origin:` followed by
`bookmark: legion/<KEY> [move forward from <old> to <new>]`; `--allow-backwards` only
concerns the local pointer stepping from the empty child to its described parent. Check `jj diff -r @- --stat` first:
the described commit must hold exactly the paths you named.

Related: a planner's, tester's, reviewer's, or architect's local commit in the shared workspace rides along on the
implementer's next push — those roles act as the review App, which has no `contents` permission, and their `jj git
push` is refused with `remote: Repository not found.` (not the REST API's `Resource not accessible by integration`;
`../legion/one-role-keyed-table-decides-which-github-app-acts.md`). Verify with `jj log` that the commit is an
ancestor before building on it. On LEGION-131 (#1106) the tester's `test: record handoff` and the reviewer's
`review: record handoff` sat unpushed above the implementer's reviewed head `9f55687a` until the implementer's
`.legion/` deletion push carried all three; the architect's instruction named both commits and said "do not rewrite
or drop them", and the deletion head `239aaa28` was `9f55687a` + those two + the deletion. One consequence for the
merger's and reviewer's tree check: `jj diff --from 9f55687a --to 239aaa28 --summary` lists only `D
.legion/implement.json` and `D .legion/plan.json`, because `test.json` and `review.json` were added *and* removed
above the compared head and net out — the same diff with `'~.legion'` appended is empty, which is the fact the
"approved head plus the deletion alone" rule wants.

## 4. The box's global git credential helper can make a test daemon clone time out

`~/.gitconfig` can set `credential.helper = !gh auth git-credential`. When `gh` blocks on the
D-Bus secret service, a test daemon that falls back to global git configuration can time out while
cloning a workspace and leave an incomplete directory. Legion's configured credential path remains
the supported path; do not use a global-configuration fallback for current test fixtures.

Diagnose the helper independently with `timeout 8 git credential fill <<<$'protocol=https\nhost=github.com'`:
a hang, rather than a prompt, is the signature.

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
need the grant the bash hook writes to `$LEGION_GRANT_FILE` before each command — so queue those until the bridge
returns.

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
`printenv OMP_SESSION_ID` in a worker pane returned an id inherited from whatever OMP session started the daemon: the
daemon's private tmux server is forked from the daemon's own environment and every pane inherits it. Until LEGION-74
the daemon's strip list covered secrets only; the allow-list (`PANE_ENV_ALLOW_LIST` in
`packages/daemon/src/daemon/environment.ts`) now excludes `OMP_SESSION_ID` and the boot-time server scrub removes it
from an older server, but a pane opened before that first restart keeps what it inherited. On LEGION-13 the pane's
value decoded (UUIDv7, first 48 bits are epoch millis) to
2026-09-09T01:51Z, three days before the worker was spawned; the implementer's two review-thread replies went out with
that id in their footer and had to be edited.

Get the id from `envoy_whoami` (`session_id`) or from `legion state` → `roles["legion-<project>-<KEY>-<role>"].sessionId`;
the two agree, and both are the id the daemon registered at `/worker/started`. Never from the environment, and — per
[session-id-remint-stale-transcript-identity](../envoy/session-id-remint-stale-transcript-identity.md) — never from a
`whoami` result earlier in your own transcript either.

## 11. `legion handoff complete` answers 409 "no longer owned by this worker" after a respawn (LEGION-37)

On LEGION-12 the implementer was revived three times for later rounds (opening the pull request, a rebase, the
review cleanup). Each round ended with `legion handoff complete`; the last one answered
`Unable to report phase completion (409): {"error":"Phase for LEGION-12 is no longer owned by this worker"}` although
the same worker, the same session, and a freshly minted credential had just pushed the branch and edited the pull
request without trouble. This is not the credential problem of section 1 (that is a 403, and a 403 within seconds of
the call means the id was never minted); it is the daemon's phase-ownership check in `handlePhaseComplete`
(`packages/daemon/src/daemon/api/routes/workers.ts`). The daemon records one active phase per issue
(`state.phases[<KEY>]`, with the role and the session id it was assigned to). The check passes only while that record
still names your role and your session; it answers 409 when the record has moved on — the architect assigned a later
phase, or the daemon respawned or re-assigned around you — even though your role claim itself is intact. LEGION-37
tracks why a worker revived for a follow-up round ends up on the wrong side of that check.

What to do: do not retry, and do not write a second handoff file (nothing about the branch is wrong). Report the
completion to the architect yourself with `envoy_publish` to `notifications.role.legion-<project>-<KEY>-architect`,
carrying exactly what the summary would have said — the head, what changed, the test counts, the CI run ids — and
name the 409 in it so the architect knows the daemon holds no record of the completion. The architect can act on the
message directly; a `phase-complete` event will not arrive. Contrast section 7's 202 (`no architect was live`), where
the daemon does keep the record and a repeat of the same command later delivers it: after a 409 a repeat only 409s
again.

**The same 409 in a multi-round tree, with no respawn involved (LEGION-20).** On #975 the implementer's later pushes
(a review-fix round, a fix for a tester finding, the `.legion/` deletion) each ended in this 409 while the tester's
or reviewer's phase was the active one on the issue — the architect had already handed the next phase on, and the
implementer's completion had nowhere to land. Only the very first round's `handoff complete` returned 200. It cost
nothing: every round's report went over the architect topic with the head sha, the CI run ids, and one line per
item, and the architect acted on those. Treat the topic publish as the completion channel on any round after the
first and the 409 as the expected daemon answer — do not wait for it to say 200 before publishing.

## 12. Three facts a multi-round tree meets that are not defects of the branch (from LEGION-54)

**The "exact released version" a doc states is a moving target while the tree is open.** LEGION-54's docs named the
pi-envoy release it would ship as; `main` released 1.17.3, 1.18.0, and 1.18.1 during its five rounds, and the number
was edited three times (1.17.3 → 1.17.4 → 1.18.2), one of them a tester FAIL, and the change then shipped as 1.20.1 anyway (two more releases landed before the merge queue reached it), corrected by this fast-follow. The workable rule: compute the number at
commit time from the latest tag with the release workflow's own script —
`.github/scripts/release-bump.sh <prev> pi-legion-envoy-v<prev>..<head> -- packages/pi-envoy/ packages/envoy-client/ packages/contracts/ packages/workspace/ skills/`
(paths from `.github/workflows/release.yaml`) — the merger recomputes it at READY, and a drift that appears after
approval is a one-line fast-follow, not a test FAIL. Note the merge queue's squash body is its READY packet, not the
branch's commit subjects, so only the PR title's conventional-commit type classifies the bump.

**"Diff against `main`" is not the branch's diff once `main` has moved.** A fresh-eyes review of this branch reported
that it "reverted" LEGION-11's `reconnectWorkers`-after-`api` fix and "deleted" its regression test. It had diffed the
branch head against `main@origin`, which by then carried LEGION-11 on top of the branch's base; the branch's own delta
on those files was seven lines. The PR's diff is against the merge base — `jj diff --from <the commit the branch was
rebased onto> --to <head>`, or `gh pr view --json files` — and a per-commit `jj diff -r <c> --stat <paths>` over
`main@origin..<branch>` settles what the branch touched. [rebasing-a-branch-across-a-refactor-of-its-own-call-sites](rebasing-a-branch-across-a-refactor-of-its-own-call-sites.md)
has the same rule for the symmetric case; check `jj log -r '::main@origin ~ ::<branch>'` before reading any
"main has X, the branch does not" finding as a revert.

**Already recorded, so read these rather than re-deriving:** the 409 after a daemon restart and the architect's
re-derived status write are §11 above and
[external-red-and-phase-ownership](../daemon/external-red-and-phase-ownership.md) §2 (LEGION-37); the review App's
inability to push or resolve threads, and the implementer resolving the threads the reviewer accepted with
`legion threads resolve --pr <n> --repo <owner>/<repo>` (LEGION-34, sjawhar/legion#1003 — until that release is
deployed the pane's `legion` has no `threads` subcommand, see §13; a worker on LEGION-54 was told to run it before it
existed on any branch), is §3 of that same note; the tester completion's status write is
verdict-blind (`phaseCompleteStatus` in `api/routes/workers.ts` returns `needs_review` for a tester whatever it
found), so a FAIL is carried by the tester's comment and the architect's own `set_status in_progress` seconds later —
a sibling architect reading Dispatch status alone will see `needs_review` flash by. The daemon-provisioned
`.omp/config.yml` that every path-scoped commit once had to leave out is gone since LEGION-58 (provisioning no longer
writes it; OMP needed nothing from it — absent and empty parse the same, `{}`) for every workspace provisioned after
the daemon that carries it was deployed; a tree provisioned before that keeps its copy until it closes, and its workers keep the path-scoped habit.

## 13. The pane's `legion` is the deployed daemon's build: a new CLI subcommand is exercised live from the workspace (from LEGION-34)

`legion` on a worker pane's PATH is `<state_dir>/bin/legion`, a launcher that re-execs the **running daemon's** own
checkout (`legionCliLauncherScript` in `packages/daemon/src/daemon/environment.ts`), and the `gh` beside it is a shim
that execs `legion gh --`. Neither knows anything on your branch. A CLI subcommand added by the PR under test does not
exist there until the release lands on `main` and the operator restarts the daemon — on LEGION-34 `legion threads
resolve` answered `Unknown command` from the pane for the whole tree. Every live run is the branch's own entry point,
from the workspace, with the same grant delivery the pane gives any command:

```bash
cd -- "$LEGION_WORKSPACE" && bun packages/daemon/src/cli/index.ts threads resolve --pr <n> --repo sjawhar/legion
```

(`bun install --frozen-lockfile` once in the workspace; `bun packages/daemon/src/cli/index.ts --help` lists the
branch's commands.) That is a real production-like proof, not a stand-in: the process reads `LEGION_GRANT_FILE` /
`LEGION_GRANT` from the pane, redeems it at the real daemon's `/legion/v1/gh-token`, and acts on real GitHub as the
App of your role. Name the `bun …` form in the PR body's `E2E` line and in every role's instructions for the tree,
and record in the handoff that the deployed `legion` form is exercised only after the merge (LEGION-34's deployment
note: the first review round on any later issue runs the plain `legion threads resolve` and quotes it on that PR).

Two things that bit the same runs. First, §1's copied credential block: a command whose text carries an imitated
`export LEGION_GRANT='…'` line ahead of the real one answers `Unable to redeem LEGION_GRANT (403)` on every `legion …`
call — and a `jj git push` through the credential helper fails as `could not read Username for
'https://github.com'`, which looks like a broken helper and is not. Running the command from the eval kernel's
`tool.bash` bridge, with the shell in a script file (`bash /tmp/<issue>-step.sh`), keeps the model's own transcript
out of the command text; the bridge attaches the grant exactly as the bash tool does. Second, a body-only
or title-only PR edit re-runs only the `PR Title` workflow. The Tests workflow does not subscribe to `edited`.
Retargeting a pull request to a new base does not re-run Tests; after a retarget, rebase onto the new base and
push — the new head runs Tests against the new merge result — and cite that run in the PR body. The `CI:` line
cites the distinct Tests and PR Title run ids at the head
([text-only-skill-pr-mechanics](text-only-skill-pr-mechanics.md), §6).

**Two corrections from LEGION-78 (sjawhar/legion#1015), where the branch made `legion gh` refuse a command the
deployed build still forwarded.** First, the stale build is not always harmless. §13's example is a subcommand that
answers `Unknown command`; when the change is a *refusal*, the pane's old build does the thing instead of refusing it.
The tester ran `legion gh -- issue comment 1015 --repo sjawhar/legion --body smoke` once through the pane's `legion`
and it posted a real comment on the PR (id 5652558869, deleted with `pr comment --delete-last --yes` through the
branch entry). Prove a refusal only through `bun packages/daemon/src/cli/index.ts gh -- …` from the workspace, and
run the positive control (a forwarded `pr view`) through the same entry so the `E2E` line names one code path.
Second, "the operator restarts the daemon" is one release too many for a CLI-only change: the launcher re-execs
`bun <daemon checkout>/packages/daemon/src/cli/index.ts` on every call, so a change that touches only
`packages/daemon/src/cli/` is live in every pane the moment the daemon checkout carries the merge — no restart. What
needs updating is that checkout (on this box `/home/ubuntu/legion-ws-RunDaemon`, at `main`); the architect raises it
after the merge. A change to the daemon process itself still needs the restart.

**The same skew on a schema refusal (LEGION-131, #1106).** The branch tightened the handoff schema in
`@legion/contracts`: the pane's `legion` (execs the daemon checkout at `main`) accepted a test handoff with
`implementerProof.verdict: "rejected"` and no `failures`, and a proof whose field was whitespace, while the branch's
`bun packages/daemon/src/cli/index.ts handoff write` refused both, naming `failures` and `proof.0.<field>` — the
tester recorded the live contrast in its handoff, the clearest form of a rule proving itself. Two consequences.
Every phase's *own* handoff on a schema-tightening issue is written through the branch CLI
(`bun packages/daemon/src/cli/index.ts handoff write --phase <p> --workspace "$LEGION_WORKSPACE" --data …`), not
the pane's `legion`, or the PR's own ledger is the one artifact the new rule never validated at write time (the
committed `test.json` was written by the deployed build and only *read* back clean under the branch schema). And the
deployed refusal arrives only when the operator advances the daemon checkout — record that as the pending production
check, exactly as the paragraph above says.

**The same indirection holds for skills, through a different surface.** A pane's `skill://legion-<name>` is not the
branch's `skills/legion-<name>/SKILL.md` and not the daemon checkout's either: OMP loads it from the installed
`@sjawhar/pi-legion-envoy` plugin release (`~/.omp/plugins/node_modules/@sjawhar/pi-legion-envoy/dist/skills/`,
the `skills` entry of its `package.json` `omp` manifest). During LEGION-78's own retro, `skill://legion-retro`
served the pre-change template (`legion gh -- issue comment <issue-number>`, the exact hazard the branch fixed)
from plugin 1.21.0 while `$LEGION_WORKSPACE/skills/legion-retro/SKILL.md` carried the `dispatch_message` call. So a
worker on an issue that changes a skill reads the branch file, not `skill://`, for the rest of that tree, and the
architect's task for each later phase says which template to follow. A skill fix reaches other trees only after the
plugin release built from the merged commit is installed — a release step, unlike the CLI's checkout advance above.

## 14. The bash tool's `jq` is jaq, a shell builtin; every script you write runs mise's jq (from LEGION-40)

In the bash tool's own shell, `type jq` answers `jq is a shell builtin` and `jq --version` prints `jaq 2.3.0`
(`type -a jq` lists the mise install and shim behind it). That builtin exists nowhere else: a child `bash` — `bash -c`,
the retired rig's startup invocation, every `*.test.sh` harness, every script a pane's daemon or an operator runs — resolves
`jq` through `PATH` to `/home/ubuntu/.mise/installs/jq/1.8.2/jq` (`jq-1.8.2`; `/usr/bin/jq` is `jq-1.7`). So an
expression checked interactively in the tool shell was checked against jaq, and the script's behaviour is jq's.
LEGION-40's plan recorded "`jq` on this box's PATH is jaq 2.3.0", the harness and the shipped `dispatch-config.sh`
header repeated it, and the reviewer's thermo pass caught it as false for the context the resolver actually runs in.

The two engines agree on most of `jq`, and the difference bites exactly where a missing config key is being tolerated.
Verified on this box with `jq -r '<expr>' envoy.json`, `envoy.json` as named, exit code and output:

| `envoy.json` | `.dispatch.token // empty` | `.dispatch.token? // empty` | `(.dispatch.token)? // empty` |
| :--- | :--- | :--- | :--- |
| `{"dispatch":{"token":"t"}}` | `t` on all three | `t` | `t` |
| `{"dispatch":{}}`, `{}` | jq 1.7/1.8.2: empty, exit 0; **jaq: exit 5** (`cannot use null as iterable`) on `{}` | empty, exit 0 on all three | empty, exit 0 |
| `{"dispatch":null}` | jq: empty, exit 0; **jaq: exit 5** | empty, exit 0 | empty, exit 0 |
| `{"dispatch":"str"}` | **exit 5 on all three** (jq: `Cannot index string with string "token"`) | empty, exit 0 on all three | empty, exit 0 |
| `[1,2]` (top level not an object) | exit 5 on all three | **exit 5 on all three** (`?` binds to the last index only) | empty, exit 0 |

What follows for a script that reads an optional key: the trailing `?` is load-bearing under real jq too — a
non-object parent raises without it — and jaq additionally raises on a null or absent parent; `// empty` is what
turns jq's `null` (which `-r` would print as the literal `null`, the production launcher's bug) into nothing. Only
`(<path>)?` also survives a top level that is not an object, which is why the reviewer's fast-follow on
`dispatch-config.sh` asks for that form. Two rules:

- Verify a `jq` expression with the `jq` the script will run: `bash -c 'jq -r "…" file'`, never bare `jq` in the tool
  shell. When the verdict matters, run the matrix above on `bash -c 'jq'`, `/usr/bin/jq`, and the tool shell, and
  record which engine each result came from.
- Do not write "the `jq` on PATH is jaq" into a script comment or a plan premise. `type -P jq` from a child bash is the
  fact to quote; the tool shell's builtin is a fact about your session, not about the box.

## 15. `jj squash --into` on the shared operation log can leave a `(divergent)` twin of your commit (from LEGION-53)

Every issue workspace on this box shares one repository operation log (`.jj/repo` → the shared clone), and other
trees' workers are committing into it at the same time. On LEGION-53 two `jj squash --from @ --into <commit>`
calls (folding biome format fixes into their owning commits) each printed `Concurrent modification detected,
resolving automatically.`, and the later `jj split` of the handoff file came back marked `(divergent)`: the working
copy's change id now named two visible commits — the described handoff commit the bookmark pointed at, and
`ebd6cba55383`, an undescribed sibling on the same parent holding the pre-squash working-copy content. Nothing was
lost and the push was correct (`jj git push --bookmark` pushes the commit the bookmark names), but the twin stays
visible in every workspace's `jj log`, and any revset that names the change id by prefix fails with
``Change ID `tzzoplly` is divergent``.

What to do: before every push, `jj -R "$LEGION_WORKSPACE" log -r 'change_id(<your change>)'` and read
`(divergent)` as a warning, not an error. Point the bookmark at the commit you mean by **commit id**, push, and
report the stray commit id to the architect in the handoff's `deviations` — a worker never runs `jj abandon`
(skill rule; the shared log is why), so it is not yours to clean up, and it must not be squashed into your branch
either. A `jj op restore` would make it worse for every other tree (LEGION-45). The cause is the shared log's
automatic reconciliation of two operations that both rewrote the same change; the way to make it rarer is to keep
`squash`/`split` sequences short and to run each as its own bash call rather than chained with `&&` behind slow
commands. This is not the two-editors-in-one-working-copy divergence of
[one-jj-actor-per-shared-workspace](../delegation/one-jj-actor-per-shared-workspace.md), whose recovery is to squash
the stale copy into `@`: here the twin holds content the branch already carries, and squashing it in would re-add the
lines the earlier squash removed.

## 16. After the daemon moved to its own user, `gh` inside an issue workspace fails with `not a git repository`, and worker commits are unsigned (from LEGION-84)

On 2026-09-13 the operator moved the daemon and every pane to the user `legion` (`HOME=/home/legion`,
state dir `/home/legion/.local/state/legion/…`). Two consequences a worker meets on its first command:

- **`gh` run from inside a pre-existing workspace fails**: `failed to run git: fatal: not a git repository:
  /home/ubuntu/.local/state/legion/…/.git/worktrees/legion-84`. The workspace's colocated `.git` is a *file*
  whose `gitdir:` pointer still names the old absolute path; jj is unaffected (its `.jj/repo` pointer is
  relative) but git — and therefore `gh`, which resolves the repository from cwd — is not. Run `legion gh` from
  `/tmp` (or any non-repository directory) with `--repo <owner>/<repo>` and, for `pr create`, `--head <branch>
  --base main`. `jj git push` still works: it goes through jj. Which calls have to move (verified from a
  LEGION-96 workspace on 2026-09-14): `pr create --repo … --head … --base main` fails even with every
  repository flag given — it reads the branch's push state from cwd — and so does any call that omits
  `--repo` (`pr view <n>`); `pr view <n> --repo …`, `run list --repo …`, `run view <id> --repo …`, and
  `api …` succeed from inside the workspace, so reads that name the repository can stay where you are. Do not
  "fix" the pointer: the `.git` file is the shared clone's worktree entry for this workspace, and a worker
  rewriting it changes what `git` sees in that workspace for every later role on the issue.
- **Nothing signs.** `/home/legion/.config/jj/config.toml` sets `signing.behavior = "drop"` and the user holds no
  signing key, so every worker commit is unsigned and GitHub shows it Unverified. `jj sign` would fail on a missing
  backend; do not run it, do not write a signing config, and say "commits are unsigned (the worker user's
  `signing.behavior = "drop"`)" in the PR body. Author and committer are still the role's bot from the pane's
  `JJ_USER`/`JJ_EMAIL` (LEGION-44); a commit born before the move under another committer can be re-stamped with
  `jj metaedit --update-author-timestamp <change>`, which rewrites the committer under the current identity.

Both are box facts, not defects of your branch; a plan step that says "confirm a fresh commit signs before the
first push" is satisfied by recording why it cannot.

## 17. `actionlint`, `yq`, and `go` are not on the pane PATH; run them from mise's Go without touching the profile (from LEGION-99)

A worker pane's PATH is the daemon's allow-listed environment plus `mise env` for the tools the *daemon* needs
(`bun`, `jj`, `gh`, `tmux`, `jq`). A plan whose verify steps say `actionlint <workflow>`, `yq '.on | keys'
<workflow>`, or `go test ./...` meets `command not found` for all three — including `go`, although
`mise ls` shows Go installed (`go 1.26.8` on this box); it is installed, not activated. The deployment
instructions forbid `bun add`/`mise install` against the live profile, so the answer is never to install anything:

```bash
mise x go@1.26.8 -- go test ./internal/webhook/ -count=1              # the pinned Go, activated for one command
mise x go@1.26.8 -- go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.7 <workflow>   # module cache only
js-yaml <workflow> | jq -c '.on | keys_unsorted, .on.merge_group.types, (.jobs | to_entries | map({job: .key, if: .value["if"]}))'
```

`mise x <tool>@<version> -- <cmd>` activates the installed tool for that one process (check the version against
`packages/envoy/go.mod`'s `go` line and the workflow's `setup-go`); `go run <module>@<version>` fetches into the Go
module cache under the worker user's home and leaves the mise profile untouched, so it is not a profile mutation. Pin
the actionlint version in the command and quote it in the proof, since it is the tool whose verdict the negative
control (`merge_grup` → `unknown Webhook event`) depends on. `js-yaml` is on the box's Node install (`/usr/bin`) and,
piped into `jq`, gives the same structural read a `yq` step asks for — say "run as `js-yaml | jq`" in the handoff's
`deviations` rather than reporting the `yq` step as skipped. Note `jq` here is the bash tool's jaq (§14); for a
`keys`/`to_entries`/`map` read the two engines agree.

## 18. A sub-architect pane whose plugin predates the daemon is refused every `spawn_worker`; the parent architect spawns its workers until the pane is relaunched (from LEGION-131; filed as LEGION-164)

A root or sub-architect pane loads the installed `@sjawhar/pi-legion-envoy` once, when it opens, and can live for
days. After the operator installs a plugin release and restarts the daemon on a contract the old plugin does not
speak, every `spawn_worker` from such a pane is refused: `POST /legion/v1/worker/spawn failed with 400: requestId:
Invalid input: expected string, received undefined` (the installed plugin sends `requestId`; the loaded one does
not). The daemon's boot gate checks the *installed* manifest, not what a live pane loaded. LEGION-131's sub-architect
(pane `%35`, opened 2026-09-14 before that day's install) reported exactly that at 23:20:48Z when it tried to spawn
the child's implementer; the daemon then relaunched the sub-architect three times in twenty seconds (`worker-started`
at 23:22:02, 23:22:07, 23:22:22Z), each relaunch replaying the same request to the parent's topic (six copies by
23:22:13Z) — the tree's first three spawns went to relaunching the pane that could not spawn. The parent (LEGION-53's)
architect spawned the child's implementer itself at 23:20:50Z (`spawned`), and the child's phases then ran under the
parent's spawns while the sub-architect stayed the addressee for reports.

What to do while LEGION-164 (the daemon retiring and relaunching stale root/controller panes from their saved sessions
at boot) is open:

- **Sub-architect:** on the first `requestId: Invalid input` 400, stop spawning — each retry re-queues the same
  prompt and feeds the relaunch loop — and send the parent architect the exact 400, the pane id, and the spawn you
  need (issue, role, task) with `envoy_publish`; ask the operator, through the architect, to kill the pane so the
  daemon resumes it (`--resume`) on the installed plugin.
- **Parent architect:** spawn the child's workers directly (`spawn_worker` with the child's issue key); the daemon
  accepts it from any architect whose tree owns the issue. Say in each worker's assignment that the sub-architect's
  topic is still the report address, and expect duplicate catch-up copies of the child's messages while the relaunch
  loop runs.
- **Worker on such a tree:** nothing changes for you except who spawned you; report to the topic your system prompt
  names. If a relaunched sub-architect later re-sends an old request, answer it once from your committed handoff.
