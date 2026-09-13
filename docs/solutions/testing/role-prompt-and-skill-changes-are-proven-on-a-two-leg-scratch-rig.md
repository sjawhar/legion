---
title: "A role-prompt or skills change is proven on a scratch daemon in two legs: the released plugin plus main first, then the branch — and the argv you record is not the prompt the model saw"
category: testing
tags:
  - scratch-daemon
  - proof-rig
  - role-prompts
  - skills
  - negative-control
  - append-system-prompt
  - LEGION_OMP_PATH
  - argv
  - OMP_PROFILE
date: 2026-09-13
status: active
module: pi-envoy
applies_when:
  - A pull request changes `packages/pi-envoy/roles/*.md`, `skills/*/SKILL.md`, or anything else a pane receives through `--append-system-prompt` or resolves through `skill://`
  - An acceptance criterion says "a daemon-spawned <role> pane receives a prompt stating …" or "a worker reads `skill://…` and quotes …"
  - The implementer's own proof is required before the phase completes (LEGION-53's rule), and a unit grep over the source files is not it
related_issues:
  - "LEGION-53"
  - "sjawhar/legion#1028"
  - "LEGION-54"
  - "sjawhar/legion#975"
---

# A role-prompt or skills change is proven on a scratch daemon in two legs

LEGION-53 changed seven role prompts and three skills and had to prove, before merge, that a
daemon-spawned pane receives the new sentences and that a real worker resolves `skill://legion-worker`
to the new text. The unit suite cannot: the role prompt is `"$(cat <roles/x.md>)"`-expanded by the
pane's own shell, and `skill://` resolves from the installed `@sjawhar/pi-legion-envoy` package's
`dist/skills`, not from the checkout. This note is the rig shape that proved it, the one shortcut that
made it cheap, and the defect in the implementer's own proof that the tester caught. Build on
[scratch-daemon-rig-proves-what-unit-tests-cannot](scratch-daemon-rig-proves-what-unit-tests-cannot.md)
(isolation, teardown) and
[proof-rig-loads-the-production-plugin-tree](proof-rig-loads-the-production-plugin-tree.md)
(the production plugin tree with one plugin swapped); what follows adds only what those did not cover.

## 1. Two legs, negative control first, same rig

Run the **same five spawns twice**. Leg A: the profile holds the *released* plugin
(`RIG_LEGION_BUILD=<installed version>`, 1.23.0 on LEGION-53) and the daemon runs from a read-only
export of `main@origin` (`git archive <sha> | tar -x -C /tmp/<rig>/main-src && bun install --frozen-lockfile`
there — never `/home/ubuntu/legion-ws-RunDaemon`). Leg B: the profile holds the branch build and the
daemon runs from `$LEGION_WORKSPACE`. Every grep that must be ≥ 1 on leg B must be 0 on leg A, and the
*old* sentence the branch replaces must be present on leg A (`leave the E2E section for the tester` 1,
`E2E (implementer)` 0). Without leg A a passing grep is attributable to the rig, the plugin tree, or the
checkout on disk rather than to the branch; with it, the only variable between the legs is the branch.
Do leg A first: once leg B's profile exists you will not want to rebuild it.

## 2. What `setup.sh`'s branch mode does not swap: `dist/skills` and the manifest

`RIG_LEGION_BUILD=branch` copies `dist/envoy.js` and `dist/legion.js` over the installed package and
nothing else. `skill://` resolves from `package.json`'s `omp.skills` → `dist/skills`, which `prepack.sh`
builds as `cp -r ../../skills dist/skills` at publish time, so the rig profile still serves the
*released* skills until you do the same by hand:

```sh
PKG="$HOME/.omp/profiles/<rig>/plugins/node_modules/@sjawhar/pi-legion-envoy"
rm -rf "$PKG/dist/skills" && cp -r "$LEGION_WORKSPACE/skills" "$PKG/dist/skills"
cp "$LEGION_WORKSPACE/packages/pi-envoy/package.json" "$PKG/package.json"
jq '.omp.extensions = ["dist/envoy.js","dist/legion.js"]' "$PKG/package.json" > "$PKG/p.tmp" && mv "$PKG/p.tmp" "$PKG/package.json"
grep -c '<new phrase>' "$PKG/dist/skills/legion-worker/SKILL.md"   # ≥ 1 before any pane launches
```

The manifest copy matters for a second reason: the daemon's boot gate compares the installed
`legion.daemonApiVersion` with its own `LEGION_DAEMON_API_VERSION`, and it reads that manifest through
OMP's ambient plugin root — **the daemon's own `OMP_PROFILE`**. So the rig daemon is started with
`OMP_PROFILE=<rig> PI_PROFILE=<rig>` (and this pane's `PI_CODING_AGENT_DIR` unset), and the manifest
must say what the branch's `packages/pi-envoy/package.json` says (`main` was already at contract 2 while
LEGION-53's branch was at 1; a mismatch is a boot refusal naming both numbers, not a silent pass).

## 3. Admission without Dispatch: seed one queued tree, log the status writes

A tree becomes spawnable only through admission, and the rig has no Envoy listener relaying Dispatch
events. Two small files replace all of that:

- `seed-state.ts` writes `newLegionState("<slug>", 2)` with one issue at `todo`, one tree
  `{status: "queued", generation: 0, launchFailures: 0}`, and that key on `admission.queue`; boot's
  `reconcileAdmission()` promotes it and `spawnTree` opens the root pane and writes its boot-token file.
  It worked first time on both legs; LEGION-30's Dispatch-relay route was not needed.
- `dispatch-sink.ts`: `Bun.serve` on the loopback port `dispatch_url` names, logging
  `<time> <method> <path> <body>` per request and answering `{issues: []}` for `/issues` and `{}`
  otherwise (`createDispatchClient` dereferences `dispatch_url` at boot, so it cannot be omitted). The
  log is also the proof of absence: exactly one `PATCH … in_progress` at admission on each leg.
- The NATS container needs the `ENVOY_NOTIFICATIONS` stream created by hand
  ([fresh-state-dir-boot-beside-a-busy-rig](fresh-state-dir-boot-beside-a-busy-rig.md) §2) or the two
  durable consumers loop on `stream not found`; the daemon still boots and spawns without it.

The inert root: `LEGION_OMP_PATH` points at a wrapper that records its argv (§4) and then, when
`LEGION_ROLE=architect` or `LEGION_CONTROLLER` is set, `exec sleep 86400`; for every other role it
execs the real OMP. `isOmpPane` only needs "omp" in the pane's root cmdline, which the
`legion worker-shim … omp` argv supplies, so the inert root probes alive and never registers. The
driver then owns the tree: `POST /process/started` with the boot token read from
`<state_dir>/secrets/legion-<slug>-<key>-architect` and the tree's current `generation` from
`state.json` (a mismatch is 409), keep the returned `secret`, and `POST /worker/spawn` once per role.
`worker_boot_timeout_seconds: 900` keeps the unregistered root's deadline out of the run;
`worker_idle_retire_seconds: 0` keeps every worker pane up for inspection.

## 4. The argv you record is not the prompt the model saw

The wrapper writes one file per pane — the env line and every argv element after
`<<<ARG>>>` markers — which is the same instrument `real-deployment-instructions-e2e.test.ts` and
`cli/__tests__/fixtures/argv-recorder-omp.ts` use. `grep -c '<sentence>' argv/<role>-*.txt` then proves
what the pane's shell handed OMP after `"$(cat …)"` expansion. It does **not** prove what the model
received. OMP's `--append-system-prompt` is last-wins: several flags hand the model only the final
one. LEGION-53's branch leg ran the *branch's* daemon at a head cut before `#975` (LEGION-20) joined
the role prompt, addressing line, and deployment instructions into one flag, so every leg-B pane got
**two** flags — role prompt first, `Legion addressing: …` last — and the model in those panes saw the
addressing line alone. The greps passed anyway, because they searched the whole file. The leg-A
control, whose daemon came from `main`, had one flag; nothing in the proof compared the two shapes.
The tester caught it by asking each real worker to quote the sentence *from its own system prompt*
(planner and merger correctly answered `NONE` for phrases their prompts lack) and by re-driving the rig
at the rebased head, where the count is one.

Three rules, in the order that would have caught it cheapest:

1. **Assert the argv shape, not only its content:** `grep -c '^--append-system-prompt$' argv/<role>-*.txt`
   is `1`, and the role prompt is the *last* such flag's value. A count above one is a finding about
   the daemon under test, whatever the greps say.
2. **Rebase before the rig, not after — and if the rig ran first, diff, never restate.** The rig proves
   the branch's daemon as a whole. A branch cut before a `main` fix to `processes.ts` (or anything on
   the spawn path) proves a daemon nobody will deploy; a proof taken there names a head that is not an
   ancestor of the reviewed one, and the reviewer will say so. Check
   `jj log -r 'main@origin ~ ::@' -- packages/daemon/src/daemon/processes.ts` before starting the
   daemon. If the evidence already exists at a pre-rebase head, the claim that carries it forward is a
   diff of the recorded files between the two heads, quoted — LEGION-53's handoff said "the seven role
   prompts are byte-identical at 66e54dd7" from memory, and
   `jj diff --from 4ef79ebb --to 66e54dd7 --summary packages/pi-envoy/roles/` showed three of them
   modified by the rebase; the tester re-drove those three.
3. **The model-visible proof is the worker quoting its system prompt, and `skill://` reads are a
   different proof.** Give the tester pane two tasks: quote the sentence containing `<phrase>` from your
   system prompt (expect `NONE` for the roles that do not carry it), and read `skill://<name>` and quote
   the sentence containing `<phrase>`. Read the answers from the pane's session transcript under
   `~/.omp/profiles/<rig>/agent/sessions/…`, not from the argv file.

## 5. The branch CLI from inside a pane

The rig's daemon writes `<state_dir>/bin/legion` re-execing *its own* checkout, so a
`legion handoff write …` the tester pane runs is the branch's CLI resolved the way a production pane
resolves it (`which legion` → `/tmp/<rig>/daemon/bin/legion`). That is the second observation for a
CLI criterion — the first being `bun packages/daemon/src/cli/index.ts …` from the workspace
([worker-pane-shell-gotchas](../legion/worker-pane-shell-gotchas.md) §13) — and on leg A the same
command exits 0, which is the before half of the before/after pair.

## Checklist before reporting the proof

- Leg A ran first, on the released plugin and `main`'s CLI, and every new-phrase grep is 0 there.
- `dist/skills` and the manifest were swapped by hand and the new phrase grepped ≥ 1 in the package
  *before* the first pane launched.
- One `--append-system-prompt` per pane, role prompt last; the branch head the daemon ran from is an
  ancestor of the head the reviewer will read.
- Each role's own sentence quoted from its system prompt by the real worker; `skill://` quotes taken
  separately; both read from transcripts.
- The Dispatch sink log quoted in full; `launchFailures` unset for every role; the live server's pane
  set diffed before and after teardown.
