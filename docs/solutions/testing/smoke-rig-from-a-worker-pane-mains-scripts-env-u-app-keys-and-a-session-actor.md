---
title: "Smoke rig from a worker pane: run main's rig scripts, launch under env -u LEGION_OMP_PATH, restart a seeded daemon under the App keys, send Dispatch a session actor, and expect sibling-rig noise"
category: testing
tags:
  - smoke-rig
  - worker-pane
  - LEGION_OMP_PATH
  - natives
  - dispatch-api
  - actor
  - seeded-state
  - secrets
  - shared-project
date: 2026-09-13
status: active
module: scripts/smoke
related_issues:
  - "LEGION-79"
  - "sjawhar/legion#1037"
  - "LEGION-92"
  - "LEGION-41"
symptoms:
  - "up.sh: NATS container legion-smoke-nats is not mapped to configured port 14222"
  - "GREEN OMP build: …/omp-18.1.15-sami.9bff2014-rpcfix, then the rig daemon dies at the probe: Failed to load pi_natives native addon … does not expose the … version sentinel __piNativesV18_1_15"
  - "A hand-started rig daemon never binds its port and writes nothing to daemon.log for minutes"
  - "curl POST /api/v1/issues → 400 {\"code\":\"ACTOR_KIND\",\"error\":\"bearer callers require actor.kind session\"}"
  - "Issues you never created appear in your rig's admission queue"
---

# Smoke rig from a worker pane

The implementer's own production-like proof (deployment instructions, Sami 2026-09-13: "the
agent that developed it is responsible for testing … before it gets to production") runs the
smoke rig from inside a Legion worker pane. Five things about that pane and the shared
`LEGSMOKE` project cost LEGION-79 an hour; each has a one-line fix.

## 1. Run `main`'s `up.sh`/`down.sh` when your branch did not change them

The rig scripts are infrastructure, not the product under test. LEGION-79's branch forked
before LEGION-41 landed, so its `scripts/smoke/up.sh` still hard-coded the Docker name
`legion-smoke-nats` — which a sibling rig owned on another port — and refused:
`NATS container legion-smoke-nats is not mapped to configured port 14222`. `main`'s scripts
derive every per-rig name from `SMOKE_PROJECT` (`nats_container_name`, `listener_machine_id`).

Overlay `main`'s copies as an uncommitted working-copy edit, run, and restore them **before your
next `jj split`** — a path-scoped split that names `scripts/smoke/*` would otherwise carry the
overlay into a commit:

```sh
cd -- "$LEGION_WORKSPACE"
jj -R "$LEGION_WORKSPACE" file show -r main@origin scripts/smoke/up.sh   > scripts/smoke/up.sh
jj -R "$LEGION_WORKSPACE" file show -r main@origin scripts/smoke/down.sh > scripts/smoke/down.sh
# … run the rig …
jj -R "$LEGION_WORKSPACE" restore scripts/smoke/up.sh scripts/smoke/down.sh
```

Give the rig its own identity and ports so it collides with nobody:
`SMOKE_PROJECT=sjawhar/<issue number>` (the tmux server, NATS container and listener machine id
derive from it), `SMOKE_DIR=/tmp/legion-smoke-<issue>`, and `NATS_PORT`/`ENVOY_PORT`/
`LEGION_DAEMON_PORT` you checked with `ss -H -ltn "sport = :<port>"` — the daemon also binds
`LEGION_DAEMON_PORT + 1` for the worker stream. Tear down with the same exported variables.

## 2. Launch under `env -u LEGION_OMP_PATH`; the failure it prevents is a shared natives cache

Every pane inherits the production daemon's `LEGION_OMP_PATH`, and `up.sh` treats a set
`LEGION_OMP_PATH` as the operator's explicit override of the pinned OMP
(`resolve_omp_path`). LEGION-79's rig printed `GREEN OMP build: …/omp-18.1.15-sami.9bff2014-rpcfix`,
reached `RIG READY`, and its daemon died three times at the boot probe with
`Failed to load pi_natives native addon … does not expose the @oh-my-pi/pi-natives@18.1.15
version sentinel __piNativesV18_1_15 … reinstall to re-sync`.

The corrected cause (LEGION-92, operator, 15:02Z): the binary is not broken. OMP keys its
native-module cache by version string only — `~/.omp/natives/18.1.15/` — and three different
builds that all call themselves 18.1.15 (the rpcfix binary and two mise builds,
`18.1.15-sami.20260910-172022` and `-001552`) share that one directory; whichever extracted
last wins (the file on disk carried `__piNativesV18_0_10`) and the others refuse to load.
Production panes run `18.1.18-sami.20260912-203541` with its own intact directory and were
never affected; the rig met the problem only through the inherited variable. `omp --version`
still prints `omp/18.1.15` because printing the version loads no natives — a boot probe is the
only test.

Rule (LEGION-92's recommendation): a smoke rig runs the pinned build from `legion.yaml`, never a
pane-inherited `LEGION_OMP_PATH`. Put the scrub on the launching command itself:

```sh
env -u LEGION_OMP_PATH secrets ENVOY_GITHUB_WEBHOOK_SECRET GH_AGENT_APP_PRIVATE_KEY_B64 GH_REVIEW_APP_PRIVATE_KEY_B64 -- \
  bash -c 'GITHUB_WEBHOOK_SECRET="$ENVOY_GITHUB_WEBHOOK_SECRET" exec bash scripts/smoke/up.sh'
```

`env -u` on that command is unconditional. A shell-level `unset LEGION_OMP_PATH` in the pane's
tool shell was observed once on LEGION-79 not to reach the following `up.sh` run (the rig still
printed the rpcfix path); whatever re-exported it, the per-command `env -u` form does not depend
on shell state. The full inherited-variable scrub list is in
[config-env-keys-that-panes-also-carry](../daemon/config-env-keys-that-panes-also-carry.md).

## 3. A seeded-state restart is the manual daemon command, under the App keys

`up.sh` rewrites `legion.yaml` on every run (`write_daemon_config`), so an edit such as
`admission_cap: 1` for a "genuinely waiting issue" scenario does not survive a re-run of
`up.sh`. Restart the daemon by hand with `up.sh`'s own env block and command — and give it the
two App private keys, because the generated `legion.yaml`'s `private_key_command` reads
`GH_AGENT_APP_PRIVATE_KEY_B64`/`GH_REVIEW_APP_PRIVATE_KEY_B64` from the daemon's environment.
Without them the daemon exits before its first log line: on LEGION-79 that was 300 s of a dead
port with **nothing** appended to `daemon.log` and a stale `daemon.pid`. Under `secrets` it
listens in three seconds.

```sh
kill -TERM "$(cat "$SMOKE_DIR/daemon.pid")"; until ! curl -fsS "http://127.0.0.1:$PORT/legion/v1/state" >/dev/null 2>&1; do sleep 1; done
cp "$SMOKE_DIR/daemon/state.json" "$SMOKE_DIR/daemon/state.json.bak"
jq '<your seed>' "$SMOKE_DIR/daemon/state.json.bak" > "$SMOKE_DIR/daemon/state.json"
sed -i 's/^admission_cap: .*/admission_cap: 1/' "$SMOKE_DIR/legion.yaml"
wc -c < "$SMOKE_DIR/daemon.log" > "$SMOKE_DIR/daemon.log.offset"       # so you can quote only this boot
rm -f "$SMOKE_DIR/daemon.pid" "$SMOKE_DIR/daemon.start"
env -u LEGION_OMP_PATH secrets GH_AGENT_APP_PRIVATE_KEY_B64 GH_REVIEW_APP_PRIVATE_KEY_B64 -- bash -c '
  setsid env ENVOY_NATS_URL=nats://127.0.0.1:'"$NATS_PORT"' ENVOY_URL=http://127.0.0.1:'"$ENVOY_PORT"' \
    LEGION_DAEMON_PORT='"$PORT"' DISPATCH_URL="$URL" DISPATCH_TOKEN="$TOKEN" \
    LEGION_STATE_DIR='"$SMOKE_DIR"'/daemon XDG_DATA_HOME='"$SMOKE_DIR"'/xdg-data XDG_STATE_HOME='"$SMOKE_DIR"'/xdg-state \
    bun run packages/daemon/src/cli/index.ts start '"$SMOKE_PROJECT"' --config '"$SMOKE_DIR"'/legion.yaml >>'"$SMOKE_DIR"'/daemon.log 2>&1 &
  pid=$!; echo $pid > '"$SMOKE_DIR"'/daemon.pid; awk "{print \$22}" /proc/$pid/stat > '"$SMOKE_DIR"'/daemon.start'
```

Writing `daemon.pid` **and** `daemon.start` (the `/proc/<pid>/stat` start ticks) the way
`start_process` does is what lets `down.sh` recognise and stop the daemon you started. Kill
only your own rig's pid, and wait for the port to drain before seeding — the old process holds
the state file until it exits. Restore `admission_cap` in `legion.yaml` before `down.sh`.

## 4. Dispatch bearer writes need a session actor

`POST /api/v1/issues` and `PATCH /api/v1/issues/{key}` with only a bearer token now answer
`400 {"code":"ACTOR_KIND","error":"bearer callers require actor.kind session"}`. Copy the shape
`ensure_root_issue` in `scripts/smoke/up.sh` sends, on every write:

```sh
ACTOR="$(jq -nc --arg id "legion-<issue>-implementer-smoke" \
  '{kind: "session", id: $id, origin: {session_title: "LEGION-<issue> implementer smoke"}}')"
curl -fsS -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -X POST "$URL/api/v1/issues" \
  -d "$(jq -nc --argjson actor "$ACTOR" '{project: "LEGSMOKE", title: "…", force: true, actor: $actor}')"
curl -fsS -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -X PATCH "$URL/api/v1/issues/$KEY" \
  -d "$(jq -nc --argjson actor "$ACTOR" '{status: "todo", actor: $actor}')"
```

`force: true` skips the possible-duplicate 409 for a disposable rig issue. A plan written from
an older curl (the bare `-d '{"project":…,"title":…}'`) will 400; fix the payload, not the
plan's intent.

## 5. The bridge relays every `LEGSMOKE` event: expect sibling-rig noise

In `envoy` mode the bridge forwards `notifications.dispatch.issue.>` for the whole shared
project, so your rig daemon ingests — and, with a free slot, admits — issues other rigs release
to `todo`, and their controllers triage yours within seconds. On LEGION-79, `LEGSMOKE-157/158`
appeared in my `active`/`queue` from a sibling, and a sibling controller wrote `backlog` on my
negative-control issue seven seconds after I closed it.

- Read `legion state` **by key** (`jq --arg k "$KEY" '{queue: .admission.queue, tree: .trees[$k],
  status: .issues[$k].status}'`), never by queue length or "the queue is empty".
- For a "genuinely waiting" issue, lower the cap and check `tree.status == "queued"` before
  acting; a free slot spawns instead of queueing.
- Byte-compare only the slice your change owns (`jq .admission` before/after) for the negative
  control, and name the sibling events you saw in the PR body's `E2E` line so the tester does not
  read them as failures.

## Related

- [config-env-keys-that-panes-also-carry](../daemon/config-env-keys-that-panes-also-carry.md) —
  the pane-inherited variable family and the full `env -u` list.
- [smoke-rig-modes-gate-checkpoints-and-the-pin-has-one-home](../legion/smoke-rig-modes-gate-checkpoints-and-the-pin-has-one-home.md)
  — `envoy` vs `forward` vs `none`, and why the pin lives in one place.
- [teardown-keys-on-the-rig-directory-and-tests-ownership-exactly](../legion/teardown-keys-on-the-rig-directory-and-tests-ownership-exactly.md)
  — why `down.sh` must own exactly what `up.sh` recorded.
- [live-proof-over-real-daemon-state-snapshots](live-proof-over-real-daemon-state-snapshots.md) —
  quoting `GET /legion/v1/state` before and after as the E2E evidence.
