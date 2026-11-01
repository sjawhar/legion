> **Suspended 2026-09-13.** Sami: "Please shutdown the goddamn legion smoke. It's pointless and it has led to destructive actions twice now." The rig this entry describes no longer exists; keep the learning, not the procedure.

---
title: "Smoke rig from a worker pane: historical environment, boot-probe, actor, and event-isolation findings"
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
module: retired smoke rig
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

# Smoke Rig from a Worker Pane: Historical Findings

The implementer's historical production-like proof ran the smoke rig from inside a Legion worker
pane. The rig is retired; this record keeps the environment, boot-probe, actor, and event-isolation
findings that cost LEGION-79 an hour, not its launch procedure.

## 1. Historical resource-ownership failure

LEGION-79's branch predated LEGION-41, so its rig startup used the fixed Docker name
`legion-smoke-nats`. A sibling rig already owned that container on another port; startup refused
with `NATS container legion-smoke-nats is not mapped to configured port 14222`. LEGION-41 later
derived the container and listener names from the project identity. The durable rule is that an
isolated fixture owns every port, container, server, and state directory through recorded fixture
state; it does not borrow a shared default or overwrite another fixture. The commands that overlaid
and ran the retired scripts are suspended.

## 2. A boot probe, not a version command, proves native compatibility

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

Rule (LEGION-92's recommendation): a supported fixture controls the OMP build on its exact child
process and never relies on a pane-inherited `LEGION_OMP_PATH`. A shell-level `unset` did not reach
a later child in the historical run; a per-command environment did. The full inherited-variable
analysis is in [config-env-keys-that-panes-also-carry](../daemon/config-env-keys-that-panes-also-carry.md).
## 3. Historical seeded-state restart evidence

The retired setup rewrote `legion.yaml` on every invocation, so an `admission_cap: 1` edit for a
waiting-issue scenario did not survive a re-run. Its manual daemon restart also needed both App
private keys because the generated `private_key_command` resolved them from the daemon environment.
Without them, LEGION-79 observed a dead port for 300 seconds, an unchanged log, and a stale daemon
pid; under `secrets`, the process listened in three seconds.

The durable fixture rule is to own the exact configuration and persisted state it supplies, verify
that its process received the dependencies needed for first boot, and clean up only its own process
and state. The old restart commands are suspended.
## 4. Dispatch bearer writes need a session actor

`POST /api/v1/issues` and `PATCH /api/v1/issues/{key}` with only a bearer token now answer
`400 {"code":"ACTOR_KIND","error":"bearer callers require actor.kind session"}`. The retired
rig's `ensure_root_issue` sent this shape on every write:

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
