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
of the inherited `LEGION_*` family (`docs/solutions/daemon/config-env-keys-that-panes-also-carry.md`).

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

## Related

- `docs/solutions/testing/smoke-rig-fakes-and-live-run-notes.md`: what the full rig needs and
  what its fakes cover.
- `docs/solutions/testing/live-proof-over-real-daemon-state-snapshots.md`: proving a change over
  a copy of real state, the complementary discipline for non-boot changes.
