---
title: "A real daemon boot on a fresh state dir beside a busy smoke rig: scratch NATS and ports, no pre-created directories, and what to do with the bug it finds"
category: testing
tags:
  - smoke-rig
  - real-boot
  - fresh-state
  - XDG_STATE_HOME
  - legions-registry
  - scope
  - adjacent-finding
  - nats
date: 2026-09-12
status: active
module: packages/daemon, scripts/smoke
related_issues:
  - "LEGION-21"
  - "sjawhar/legion#962"
  - "LEGION-52"
  - "sjawhar/legion#1018"
  - "LEGION-41"
  - "sjawhar/legion#1010"
  - "LEGION-94"
---

# A Real Daemon Boot on a Fresh State Dir Beside a Busy Smoke Rig

## Context

The LEGION-21 rebase round needed one real boot of the branch daemon to prove a seam (the
worker-stream listener binding `config.bind`) — enough to see both `listening on` lines and
stop. The shared smoke rig could not provide it: `scripts/smoke/up.sh` hardcodes the NATS
container name (`legion-smoke-nats`) and its ports, and a sibling tree's live rig (LEGION-16)
held both the container and port 19371, which is the default `worker_stream_port` for a daemon
on 19370. Bringing the shared rig up would have torn down a sibling's run. The scratch boot
below took under a minute, needed nothing shared, and found a real bug the rig structurally
cannot find.

## The recipe

Run under `secrets GH_AGENT_APP_PRIVATE_KEY_B64 GH_REVIEW_APP_PRIVATE_KEY_B64 --` (the rig's
`legion.yaml` resolves App keys through `private_key_command`), from a pane environment scrubbed
of the inherited `LEGION_*` family — `LEGION_OMP_PATH` included, or the probe at the launch hold
dies on the inherited hand-built OMP (`docs/solutions/daemon/config-env-keys-that-panes-also-carry.md`).
To boot the branch against a `private_key_secret` config with a fake `secrets` on PATH instead, see
`fake-cli-on-path-outputs-from-files-and-a-call-log.md` §4 (the fake must delegate `--value` to
the real binary; boot mints a real JWT before it listens).

1. **Config from the rig's own yaml, edited by `sed`:** `port` and `daemon_url` to a free pair
   (19380 → the daemon binds 19381 for the stream), `state_dir` to a scratch directory,
   `nats://127.0.0.1:<port>` to a scratch NATS port. Keep everything else (project, repos,
   `dispatch_project`, gates, App ids) so `--check-config` on the rig's yaml and the scratch boot
   exercise the same loader.
2. **Own NATS with JetStream and the stream the daemon expects:**
   `docker run -d --name <scratch-name> -p 127.0.0.1:14224:4222 nats:2.10 -js`, then create
   `ENVOY_NOTIFICATIONS` with subjects `notifications.>` (from `packages/daemon`, where bun
   resolves `nats` immediately: `jsm.streams.add({ name: "ENVOY_NOTIFICATIONS", subjects:
   ["notifications.>"] })`). Without it the daemon still boots and listens, but its two durable
   consumers log `stopped unexpectedly; restarting: NatsError: stream not found` in a loop.
3. **Start exactly as `up.sh` does** (`up.sh`'s `start_process daemon env …` block):
   `ENVOY_NATS_URL LEGION_STATE_DIR XDG_DATA_HOME XDG_STATE_HOME` set, then
   `bun run packages/daemon/src/cli/index.ts start <project> --config <scratch yaml>` in the
   background with its output to a log.
4. **Evidence:** wait for `legion worker stream listening on` in the log; `ss -ltnp` filtered by
   port (the `bun run` pid is a wrapper — the sockets belong to its child, so filter by port, not
   pid); `curl /legion/v1/state`; `state.json` on disk; the private tmux server for the project
   answering `no server running` (a fresh state boots no panes); `SIGTERM` and confirm the ports
   and container are released.
5. **Make evidence steps non-fatal** under `set -euo pipefail`: a `grep` with no match or an
   `ls` of a directory that should not exist kills the script and takes the daemon with it. Wrap
   them (`{ cmd || true; } | head`), or the "failure" you see is your own script.

## What the rig never exercises: the first boot on a fresh `XDG_STATE_HOME`

`up.sh` pre-creates `xdg-state/legion` (line ~513) before starting the daemon. The scratch boot
did not, and `legion start` crashed **after** printing both listening lines:

```
ENOENT: no such file or directory, open '<XDG_STATE_HOME>/legion/legions.json.lock'
    at withRegistryLock (packages/daemon/src/daemon/legions-registry.ts:90)
```

`withRegistryLock` opened its lock file with `O_CREAT|O_EXCL` before `writeRegistry`'s `mkdir`
ever ran. Identical on `main`; every rig and the dogfood daemon already had the directory, so
nobody had booted a daemon into an empty state home in months. The fix is one line (`mkdir` at
the top of `withRegistryLock`; the redundant one in `writeRegistry` removed), with a test that
writes two registry entries concurrently under a nonexistent parent — it fails pre-fix with the
same ENOENT.

Rule: a rig that pre-creates directories, seeds files, or reuses containers is testing the
steady state. Once per change that touches boot, run the real command against an empty
`state_dir` and an empty `XDG_STATE_HOME` too. Both boots take the same minute; only one of them
finds first-boot bugs.

## What to do with a bug that is not yours

The ENOENT was outside the PR's declared file list. The disposition that worked, and that the
repository's no-deferral rule requires:

1. Reproduce it on `main`, not only on the branch, and record the exact command and error.
2. Confirm the branch did not touch the file (`jj diff --from main@origin -- <path>` empty).
3. Name the one-line fix.
4. Hand the decision to the architect in the handoff, with all three facts.

The architect ruled it rides in the PR as its own commit — because a PR hold was in force,
"its own PR" would have been an indefinite deferral — with its own reproduction (pre-fix boot
fails, post-fix boot creates `legions.json` under the fresh directory), unit test, and PR-body
line. Neither silent option is acceptable: fixing it unannounced widens a reviewed diff; leaving
it "for later" is the deferral the rules forbid.

## The same contention one day later, and what has changed since (LEGION-52)

LEGION-52's rounds (2026-09-13) hit every part of this note's setup, on a box with three live
rigs, and some of the workarounds are now history:

- **The fixed container name is fixed on `main`.** The implementer's first scratch boots ran
  beside another session's rig holding `legion-smoke-nats` and ports 14222/19020/19370; the tester
  ran `up.sh` as a copy with `nats_name=legion-smoke-nats-legion52` and its own
  `SMOKE_DIR`/`NATS_PORT`/`ENVOY_PORT`/`LEGION_DAEMON_PORT`. LEGION-41 (#1010) since derives the
  NATS container name and the listener machine id per rig, so a fresh checkout no longer needs
  the copy — but a branch forked before #1010 still does; check `scripts/smoke/up.sh` on your
  branch, not on `main`.
- **The shared Dispatch project makes the unfiltered `envoy`-mode bridge unsafe with siblings.**
  Every rig's root issue lives in `LEGSMOKE`, and the checked-in bridge relays *every* LEGSMOKE
  issue event into the rig's NATS. With three rigs live, LEGSMOKE-141/138/139/119 were `todo`
  roots of other rigs; the daemon would have admitted them as its own. The tester instead ran a
  per-root issue relay (LEGION-61's `scripts/smoke/issue-relay.ts`, replaying one root and its
  children from `SMOKE_ROOT_ISSUE`) as a supervised process after `RIG READY`, with
  `SMOKE_WEBHOOK_MODE=none` recorded — which also means `checkpoints.sh 1–5` print
  `SKIPPED-BLOCKED` (the script gates on the recorded mode) and the equivalent facts are read
  from `state.json` and Dispatch by hand. Until the bridge filters by root, a rig beside other
  rigs needs a relay of that shape; write down which one you used and why in the PR's E2E line.
- **A docs-only branch still runs the whole package workflow, flakes included.** A doc-comment
  change under `packages/contracts/src` matches `packages/contracts/**` in
  `.github/workflows/envoy-and-contracts.yaml`'s `dispatch` filter, so every push of the branch —
  including the `.legion/`-only handoff pushes each phase makes — ran the seven-minute Dispatch
  dashboard browser suite, which failed three consecutive attempts on one run with a different
  fixed-timeout scenario each time (`doc.e2e.ts:105`, `inbox.e2e.ts:236`, `approval.e2e.ts:24`;
  filed as LEGION-94 with the four failure records). The rule held: re-run the failed job
  (`legion gh -- run rerun <run-id> --failed`), never push a commit to refresh CI. Budget the
  round for it: five implementer rounds on a docs PR meant five full workflow runs.
- **The scratch runner scrubs the pane's own identity.** A worker pane carries `LEGION_*`,
  `DISPATCH_URL`/`DISPATCH_TOKEN_FILE`, `OMP_PROFILE`, and (on this box) a stale `LEGION_OMP_PATH`
  pointing at an rpcfix build whose natives fail to load under a fresh profile. The runner that
  worked `env -u`'d all of them and set `OMP_PROFILE`/`PI_PROFILE` to the scratch profile, so the
  daemon resolved the pinned OMP through `mise where` and read the scratch profile's plugin
  manifest — the one the negative and positive controls vary. Without the scrub, the first attempt
  refused on `dispatch_project is required` and the second probed the wrong OMP.

## Related

- `docs/solutions/testing/smoke-rig-fakes-and-live-run-notes.md`: what the full rig needs and
  what its fakes cover.
- `docs/solutions/testing/live-proof-over-real-daemon-state-snapshots.md`: proving a change over
  a copy of real state, the complementary discipline for non-boot changes.
