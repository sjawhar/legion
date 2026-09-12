---
title: "A test helper that hardcodes what production derives: the tmux server named legion-omp, the LEGION_TMUX_LIVE-only failure that exposed it, and the vacuous assertion beside it"
category: testing
tags:
  - test-fixtures
  - tmux
  - LEGION_TMUX_LIVE
  - state.project
  - refactor
  - skipIf
  - vacuous-assertion
  - leaked-resources
date: 2026-09-12
status: active
module: packages/daemon
related_issues:
  - "LEGION-21"
  - "sjawhar/legion#962"
---

# A Test Helper That Hardcodes What Production Derives

## Context

LEGION-21 moved every tmux operation out of `ProcessManager` into a `TmuxRuntime` that the
daemon constructs and injects. Before the branch, `ProcessManager` built its tmux handle inline:
`{ run: deps.run, socket: \`legion-${deps.state.project}\` }`. The refactor turned that one
derivation into four construction sites — `index.ts` for production and one helper each in
`processes.test.ts`, `processes.thermo-ops.test.ts`, and `real-shutdown-e2e.test.ts` — and the
test helpers wrote `socket: "legion-omp", project: "omp"` as literals. The full daemon suite was
green locally (880 pass) and the tester failed the branch on acceptance 5 with one test:

```
ProcessManager > probes a real tmux pane as alive, detects its death, and resurrects it once
Expected: "dead"   Received: "alive"     processes.test.ts:5558
```

## Mechanism

The two live-tmux tests are gated `it.skipIf(process.env.LEGION_TMUX_LIVE !== "1")`. Each picks
`project = smoke<Date.now()>`, expects the runtime to spawn onto tmux server
`legion-<project>`, and drives that server itself (`kill-pane`, `list-windows`, `kill-session`).
With the helper's literal, the runtime spawned onto `legion-omp` instead: the test's `kill-pane`
hit a server that did not exist, the probe (correctly) stayed `alive`, and a real `legion-omp`
server with a `sleep 999` window was left running on the box (a third run then failed with
`duplicate session: legion-omp`).

The sibling test, "creates the first live root without a default bash window", passed for the
wrong reason: it asserted `expect(windows.stdout).not.toContain("bash")` over the listing of
`legion-<project>`, and `no server running on …` does not contain `bash`.

None of this was visible to a plain `bun test`: the cases skip. CI runs the daemon suite with
`LEGION_E2E=1 LEGION_TMUX_LIVE=1` (`.github/workflows/pr-and-main.yaml`), which is where the
tester reproduced it, 3/3.

The production site had the same shape of drift: `index.ts` first passed `config.project`,
while `ProcessManager` keys every role token, secret-file name, and prune decision by
`deps.state.project`. The two agree whenever the state file was created by this config, which is
why nothing broke on the rig, but the runtime's server name and secret-file names must come from
the same value the manager uses, or a future mismatch is silent.

## Fix (commit "the tmux runtime takes its server and secret-file project from state.project")

- `index.ts` constructs the runtime from `state.project`, with a comment saying why it is not
  `config.project`.
- Every test helper derives `socket`/`project` from the fixture's `state.project`, with the same
  comment; no helper names a server.
- The bash-window test first asserts `windows.exitCode === 0` and that the listing contains the
  spawned window (`legion-42`), then asserts no `bash` window — presence before absence.

## Rules

1. **When a refactor moves construction of a dependency out of a class, every test helper that
   mirrors the new construction site is a fresh place to diverge from production.** The old
   inline derivation was the only thing keeping the helpers honest. Derive fixture construction
   from the same source production reads (here `state.project`), never from a hand-picked
   literal, and say in a comment which production line it mirrors.
2. **Run the suite under the CI environment before handing off**, not just `bun test`:
   `cd packages/daemon && env -u DISPATCH_URL -u DISPATCH_TOKEN_FILE LEGION_E2E=1
   LEGION_TMUX_LIVE=1 bun test`. A `skipIf`-gated case that only CI runs is exactly the case a
   local green run cannot vouch for.
3. **Assert presence before absence in live-resource tests.** `not.toContain` over an error
   string passes vacuously; check the command succeeded and the expected item is listed first.
4. **Check for leaked servers before and after any live-tmux run.** List the sockets under
   `$TMUX_TMPDIR/tmux-$(id -u)/` and ask each whether it answers (`tmux -L <name> list-sessions`);
   a test that targets the wrong server leaves the right one alive. On this box the leaked
   `legion-omp` had to be killed by hand.
5. **Prefer `state.project` over `config.project` for anything keyed by the persisted project.**
   They are equal by construction today; the manager reads the persisted one, so the runtime must
   too.

## Related

- `docs/solutions/testing/argv-format-change-is-a-fixture-population-sweep.md`: the sibling
  failure mode — many fixtures answering in an old shape — and the population-sweep procedure.
- `docs/solutions/legion/worker-pane-shell-gotchas.md`: the `env -u DISPATCH_URL
  -u DISPATCH_TOKEN_FILE` prefix the daemon suite needs from a Legion pane.
- `docs/solutions/architecture-patterns/proving-a-pure-refactor-behind-an-interface.md`: the
  gates that did hold on this refactor, and why they did not catch this one (they inspect
  `processes.ts`, not the test helpers).
