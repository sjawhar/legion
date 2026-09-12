# `panePid` target-row fix (LEGION-9) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `tmux.panePid(server, "%N")` returns pane `%N`'s own `pane_pid` instead of the pid of the first pane in `%N`'s window.

**Architecture:** One tmux call, `list-panes -t <target> -F "#{pane_id} #{pane_pid}"`, whose rows are selected by pane id when the target is a pane id and by position (first row) when it is a window id. Callers, `firstPaneId`, and `windowAlive` are untouched. Every test fixture that answers a `list-panes` liveness probe with a bare pid is cut over to `<pane_id> <pid>` rows.

**Tech Stack:** TypeScript on Bun, `bun:test`, tmux 3.7c on the daemon's private server `tmux -L legion-<project>`.

**Spec:** `dispatch://LEGION-9/spec` (root spec v2, architect). The design there is fixed by the approved gate — do not re-open it. `display-message -p` was rejected (spec § Rejected); `-a` + global filter was rejected; changing callers to always pass a pane id was rejected.

## Global Constraints

- Every tmux argv goes through `argv(server, …)` in `packages/daemon/src/daemon/tmux.ts:18` (`tmux -L <socket> …`). (spec § Requirements)
- `panePid(server, "@W")` keeps returning the window's first pane's pid. (spec § Requirements — `probe` at `processes.ts:1697–1701` backfills `tmuxPaneId` from `firstPaneId`, the first pane; pid and backfilled id must describe the same pane.)
- A pane-id target absent from the rows is `undefined`, never the first row. (spec § Errors)
- Callers unchanged: `processes.ts:1693–1694` (`probe`), `processes.ts:2878–2879` (`controllerAlive`), `worker-boot-watchdog.ts:177–178` (`probeAlive`). All already pass `locator.tmuxPaneId ?? locator.tmuxWindowId`.
- Tests pinning the old command shape are updated, never kept. (spec § Requirements)
- Biome: double quotes, semicolons, 100-char width. `import type` for type-only imports. Tests in co-located `__tests__/`, `bun:test`.
- Never commit `.omp/config.yml` (an empty workspace file the Legion extension provisioned; present in the working copy as `A .omp/config.yml`). Use `jj split` with explicit paths, never a bare `jj describe` + `jj new` that would sweep it in.
- One PR against `sjawhar/legion` from branch `legion/LEGION-9`; PR body carries `Dispatch: LEGION-9` and the READY-format `## Verification` section from `skills/legion-worker/SKILL.md`.

## Branch state (verified 2026-09-11)

- Workspace `@-` = `e47fb952` (`chore: release pi-envoy v1.1.0`). `main` = `f88acfc1`, 7 commits ahead; none of them touch `tmux.ts`, `processes.ts`, `worker-boot-watchdog.ts`, or their tests (`jj diff --from @- --to main --summary`). A rebase onto `main` is not required for this change; if one is wanted for CI freshness, `jj -R "$LEGION_WORKSPACE" rebase -d main` is conflict-free for this file set.
- Baseline (`packages/daemon`, after `bun install --frozen-lockfile` at the workspace root — the provisioned workspace had no `node_modules`; the planner ran the install, it is gitignored): `bun test` → 803 pass, 5 skip, 1 fail. The one failure is `legion start --check-config > validates github_apps.<role>.private_key_command without executing it` (`src/cli/__tests__/index.test.ts:156`): it does not isolate `process.env`, and every Legion pane carries `DISPATCH_URL` without `DISPATCH_TOKEN`, so `resolveDaemonConfig` refuses. Pre-existing, unrelated to this change, green in CI. **Run every gate on this rig as `env -u DISPATCH_URL -u DISPATCH_TOKEN_FILE bun test`** (verified: that file → 14 pass). Out of scope here; reported to the architect in the plan handoff as an observation.
- `ProcessManager > reconnectRoots re-arms the registration deadline for an active tree with a locator that never confirmed before a restart` (`processes.test.ts:5001`, a 20 s `flushEventLoopUntil` wait) failed once in three full-suite runs of the validated tree and passed in isolation and in the other two runs; its fixture is one of the Group B sites and behaves identically before/after. Treat a single failure as the known flake and re-run; two consecutive failures are a real finding.
- Defect reproduced live on this rig's private server (window `@1`: architect `%1`, planner `%3`):

  ```
  $ tmux -L legion-sjawharlegion list-panes -t %3 -F '#{pane_id} #{pane_pid}'
  %1 3715931
  %3 4141285
  $ tmux -L legion-sjawharlegion display-message -p -t %3 '#{pane_pid}'
  4141285
  $ tmux -L legion-sjawharlegion list-panes -t %999999 -F '#{pane_id} #{pane_pid}'
  can't find pane: %999999      (exit 1)
  ```

  Current `panePid(server, "%3")` returns `3715931` (the architect's pid).

## Why the fixture sweep is larger than the spec's "≈20 sites"

The spec counted fixtures whose predicate names `#{pane_pid}` textually. Two things widen that:

1. `Array.prototype.includes` is whole-element equality. After the change the format argv element is the single string `"#{pane_id} #{pane_pid}"`, so `command.includes("#{pane_pid}")` is **false** for the new probe — every such predicate must become `command.includes("#{pane_id} #{pane_pid}")` or the probe falls through to the fixture's default reply (usually `{ stdout: "", exitCode: 0 }` → `undefined` → "dead").
2. ~26 fixtures answer **any** `list-panes` with a bare `"12345\n"`. Under the new parser a bare-pid line has no pid column (`row[1]` is `undefined`), so both target kinds read `undefined` → "dead". Those must answer in row form too.

Acceptance 4's real gate is `bun test packages/daemon` green; the enumeration in Task 3 is the complete population (47 edits + one helper), built from `grep` over the whole file, not a sample.

## Validation of this plan (planner, scratch copy — nothing touched in the workspace)

Every code block below was applied verbatim to a throwaway copy of the workspace at `/tmp/legion9-plan-check/repo` (tar of the branch tree + installed `node_modules`; not a jj workspace) and run there:

- New `panePid` + `tmux.test.ts` + watchdog case + the three watchdog fixtures: `bun test src/daemon/__tests__/tmux.test.ts src/daemon/__tests__/worker-boot-watchdog.test.ts` → 11 pass.
- Old `panePid` + the new tests (the red phase): `tmux.test.ts` 2 fail / 2 pass, watchdog case fails — exact shapes recorded in Task 1 Step 3.
- Task 3's 47 edits applied by a script that asserted each listed line's current text before replacing it (no site missing, no site mistyped), then `bun test src/daemon/__tests__/processes.test.ts` → 190 pass, 0 fail; full `env -u DISPATCH_URL -u DISPATCH_TOKEN_FILE bun test` → 809 pass, 5 skip, 0 fail (twice); `bunx tsc --noEmit` clean; `bunx biome check src/` clean after `bunx biome format src/ --write` (it collapses three shortened ternaries — Task 3 Step 7).
- The resulting unified diff (655 lines, 4 files) is at `/tmp/legion9-plan-check/reference.diff` as a cross-check for the implementer. The plan text is authoritative; the diff is evidence that the plan is complete, not a substitute for following it.

---

### Task 1: Failing tests first — `tmux.test.ts` (new) and the watchdog sibling-OMP case

**Files:**
- Create: `packages/daemon/src/daemon/__tests__/tmux.test.ts`
- Modify: `packages/daemon/src/daemon/__tests__/worker-boot-watchdog.test.ts` (append a new `describe` after line 304)

**Interfaces:**
- Consumes: `panePid(server: TmuxServer, target: string): Promise<number | undefined>` and `TmuxServer { run: TmuxRun; socket: string }` from `../tmux` (both already exported).
- Consumes: `WorkerBootWatchdog`, `baseDeps`, `root`, `child`, `role`, `token`, `locator` already defined in `worker-boot-watchdog.test.ts:17–97`.

- [ ] **Step 1: Create `tmux.test.ts`** (acceptance 1–2)

```ts
// Unit tests for `panePid`'s row selection. `list-panes -t <target>` always lists the target's
// whole window (a pane id resolves to its window), so the pane-id column — not row position —
// must pick the row for a pane target; a window target keeps the first row (its first pane, the
// same pane `firstPaneId` backfills into a pane-id-less locator).
import { describe, expect, it } from "bun:test";
import { panePid, type TmuxServer } from "../tmux";

// One window, three panes: the architect's pane first, then two split-in workers.
const rows = "%1531 2363427\n%1533 3003090\n%1534 446716\n";

function server(
  reply: { stdout: string; exitCode: number },
  commands: string[][] = []
): TmuxServer {
  return {
    socket: "legion-omp",
    run: async (cmd) => {
      commands.push(cmd);
      return reply;
    },
  };
}

describe("panePid", () => {
  it("returns the target pane's own pid from its window's listing, over the private server", async () => {
    const commands: string[][] = [];
    expect(await panePid(server({ stdout: rows, exitCode: 0 }, commands), "%1533")).toBe(3003090);
    expect(commands).toEqual([
      ["tmux", "-L", "legion-omp", "list-panes", "-t", "%1533", "-F", "#{pane_id} #{pane_pid}"],
    ]);
  });

  it("returns the first pane's pid for a window id", async () => {
    expect(await panePid(server({ stdout: rows, exitCode: 0 }), "@1464")).toBe(2363427);
  });

  it("is undefined when tmux exits non-zero, whatever it printed", async () => {
    expect(await panePid(server({ stdout: rows, exitCode: 1 }), "%1533")).toBeUndefined();
  });

  it("is undefined for a pane id missing from the listing, never a sibling pane's pid", async () => {
    expect(await panePid(server({ stdout: rows, exitCode: 0 }), "%1535")).toBeUndefined();
  });
});
```

- [ ] **Step 2: Append the acceptance-3 case to `worker-boot-watchdog.test.ts`** (after the closing `});` at line 304)

```ts
describe("WorkerBootWatchdog pid probe", () => {
  it("probes the worker's own pane pid, not its window's first pane, so a sibling's live OMP never confirms a dead boot", async () => {
    const events: string[] = [];
    const watchdog = new WorkerBootWatchdog(
      baseDeps({
        // 0.01s → exactly one connect attempt per interval (see `maxAttemptsPerInterval`).
        workerBootTimeoutSeconds: () => 0.01,
        sleep: async () => {},
        yield: async () => {},
        tmux: {
          socket: "legion-omp",
          run: async (cmd) => {
            if (cmd.includes("list-panes")) {
              // The worker's whole window: the architect's pane first, then two split-in workers.
              return { stdout: "%1531 2363427\n%1533 3003090\n%1534 446716\n", exitCode: 0 };
            }
            return { stdout: "", exitCode: 0 };
          },
        },
        // Only the architect's pane still runs OMP; the watched worker's own process is gone.
        isOmpPane: async (pid) => {
          events.push(`isOmpPane:${pid}`);
          return pid === 2363427;
        },
        workerClient: async () => {
          events.push("workerClient");
          throw new Error("shim not listening");
        },
        retireUnconfirmedBoot: async () => {
          events.push("retire");
        },
      })
    );

    watchdog.arm(
      root,
      child,
      role,
      token,
      { ...locator, tmuxWindowId: "@1464", tmuxPaneId: "%1533" },
      1
    );
    for (let i = 0; i < 200 && !events.includes("retire"); i += 1) await Promise.resolve();

    // The interval's single connect attempt, then `probeAlive`: the pid probe asks about %1533's
    // own pid (not OMP), falls through to the socket probe (refused), and the boot is retired.
    // Before the fix the first row's 2363427 confirmed the boot and no socket probe ran.
    expect(events).toEqual(["workerClient", "isOmpPane:3003090", "workerClient", "retire"]);
  });
});
```

- [ ] **Step 3: Run both files; expect the new cases to fail against the current `panePid`**

Run (from `packages/daemon`): `bun test src/daemon/__tests__/tmux.test.ts src/daemon/__tests__/worker-boot-watchdog.test.ts`

Expected (verified in the scratch copy): `tmux.test.ts` — 2 fail, 2 pass. The pane-target and window-id cases both read `undefined` (the old parser takes the first whitespace token of the row-form stdout, `%1531`, as the pid → `NaN`), so the pane-target case never reaches its argv assertion; the exit ≠ 0 and missing-pane cases pass by accident. `worker-boot-watchdog.test.ts` — the new case fails with `events` = `["workerClient", "workerClient", "retire"]` (no `isOmpPane:` entry at all — the old parser reads no pid from row-form output, so the pid probe is skipped rather than answered wrongly); the 6 existing cases still pass. The pre-fix defect itself (`2363427` confirming the boot) is what the live check in Task 5 reproduces against `main`, since only real tmux output has a bare first token that parses as a pid.

### Task 2: Rewrite `panePid`, then fix the three watchdog fixtures

**Files:**
- Modify: `packages/daemon/src/daemon/tmux.ts:179–184`
- Modify: `packages/daemon/src/daemon/__tests__/worker-boot-watchdog.test.ts:210, 247, 282`

**Interfaces:**
- Produces: `panePid(server: TmuxServer, target: string): Promise<number | undefined>` — unchanged signature; new argv `["tmux", "-L", <socket>, "list-panes", "-t", <target>, "-F", "#{pane_id} #{pane_pid}"]`.

- [ ] **Step 1: Replace lines 179–184 of `tmux.ts`**

```ts
/** Reads the live pid of `target`'s pane — the pane itself for a pane id, or a window's first
 * pane for a window id — or `undefined` if it cannot be read. `list-panes -t` always lists the
 * target's whole window (a pane id resolves to its window; without `-a`/`-s` there is no
 * single-pane listing), so the pane-id column picks the row: a pane-id target absent from the
 * listing is gone, never approximated by a sibling's pid. */
export async function panePid(server: TmuxServer, target: string): Promise<number | undefined> {
  const panes = await server.run(
    argv(server, "list-panes", "-t", target, "-F", "#{pane_id} #{pane_pid}")
  );
  if (panes.exitCode !== 0) return undefined;
  const rows = panes.stdout
    .split(/\r?\n/)
    .map((line) => line.trim().split(/\s+/))
    .filter((row) => row[0] !== "");
  const row = /^%\d+$/.test(target) ? rows.find((r) => r[0] === target) : rows[0];
  const pid = Number(row?.[1]);
  return Number.isSafeInteger(pid) && pid > 0 ? pid : undefined;
}
```

Notes: `Number(undefined)` is `NaN`, so a missing row or a bare-pid row (no second column) falls to `undefined` through the existing pid check — no separate branch. Empty stdout yields `rows = []`.

- [ ] **Step 2: Run the two files again**

Run: `bun test src/daemon/__tests__/tmux.test.ts src/daemon/__tests__/worker-boot-watchdog.test.ts`

Expected (verified): `tmux.test.ts` 4 pass; the new watchdog case passes; of the three existing watchdog cases whose fixtures answer `"12345\n"` (no pane-id column, so the `%7` probe now reads `undefined`), **only line 266 fails** (`probeCount` expected 2, received 0 — `isOmpPane` is never consulted). Lines 194 and 232 still pass, but for the wrong reason: a dead pid probe also ends in `retireUnconfirmedBoot`, which is what they assert, so they no longer exercise the alive/re-arm path they describe. All three fixtures are cut over regardless (acceptance 4).

- [ ] **Step 3: Update the three watchdog fixtures** — lines 210, 247, 282 each read

```ts
if (cmd.includes("list-panes")) return { stdout: "12345\n", exitCode: 0 };
```

change each to (the module-level `locator` at line 24 is `tmuxPaneId: "%7"`):

```ts
if (cmd.includes("list-panes")) return { stdout: "%7 12345\n", exitCode: 0 };
```

- [ ] **Step 4: Run and confirm green**

Run: `bun test src/daemon/__tests__/tmux.test.ts src/daemon/__tests__/worker-boot-watchdog.test.ts`
Expected: 11 pass, 0 fail.

### Task 3: `processes.test.ts` fixture cutover — the complete population

**Files:**
- Modify: `packages/daemon/src/daemon/__tests__/processes.test.ts` (snapshot `#6A30`, 9881 lines; line numbers below are from that snapshot — edit bottom-up or re-`read` after each hunk)

**Interfaces:**
- Produces (test-local): `livePanes(command: string[], pid?: number): { stdout: string; exitCode: number }` — inserted directly after `liveRun` (after line 224).

- [ ] **Step 1: Run the file first to see the breakage shape**

Run: `bun test src/daemon/__tests__/processes.test.ts`
Expected: many failures, all of the form "dead where alive was expected" or a missing `toContainEqual` argv.

- [ ] **Step 2: Add the helper after `liveRun` (after line 224)**

```ts
/** Answers a `list-panes` liveness probe for a pane that is alive. `panePid`'s
 * `-F "#{pane_id} #{pane_pid}"` probe gets one `<pane_id> <pid>` row for the probed target — the
 * pane itself, or `%1` standing in for a window's first pane; any other `list-panes` format keeps
 * the bare `<pid>` line these fixtures have always answered with (so `firstPaneId` still reads no
 * pane id from it and `windowAlive` still reads exit 0). */
function livePanes(command: string[], pid = 12345): { stdout: string; exitCode: number } {
  if (!command.includes("#{pane_id} #{pane_pid}")) return { stdout: `${pid}\n`, exitCode: 0 };
  const target = command[command.indexOf("-t") + 1] ?? "";
  return { stdout: `${target.startsWith("%") ? target : "%1"} ${pid}\n`, exitCode: 0 };
}
```

- [ ] **Step 3: Group A — predicates that name `#{pane_pid}` (21 edits).** In each, change `command.includes("#{pane_pid}")` → `command.includes("#{pane_id} #{pane_pid}")` **and** change the reply as listed.

| lines | today | change to |
| :--- | :--- | :--- |
| 255–265 (`manager()` default runner) | predicate `(command.includes("#{pane_id}") \|\| command.includes("#{pane_pid}"))`; reply `stdout: command.includes("#{pane_id}") ? "%1\n" : "12345\n"` | predicate `(command.includes("#{pane_id}") \|\| command.includes("#{pane_id} #{pane_pid}"))`; reply `return command.includes("#{pane_id}") ? { stdout: "%1\n", exitCode: 0 } : livePanes(command);` (keep the `if (!launchedAnyWindow) return { stdout: "", exitCode: 1 };` line) |
| 1160–1166 | `return { stdout: "12345\n", exitCode: 0 };` | `return livePanes(command);` |
| 1404–1413 (argv pin) | `"-F", "#{pane_pid}",` | `"-F", "#{pane_id} #{pane_pid}",` |
| 2162–2164 | `windows > 0 ? { stdout: "12345\n", exitCode: 0 } : { stdout: "", exitCode: 1 }` | `windows > 0 ? livePanes(command) : { stdout: "", exitCode: 1 }` |
| 2399–2405 | `{ stdout: "4242\n", exitCode: 0 }` | `livePanes(command, 4242)` |
| 2468–2474 | `4242` | `livePanes(command, 4242)` |
| 2544–2550 | `4242` | `livePanes(command, 4242)` |
| 2591–2597 | `4242` | `livePanes(command, 4242)` |
| 2639–2645 | `4242` | `livePanes(command, 4242)` |
| 2676–2682 | `4242` | `livePanes(command, 4242)` |
| 2737–2743 | `4242` | `livePanes(command, 4242)` |
| 2785–2791 | `4242` | `livePanes(command, 4242)` |
| 2810–2816 | `{ stdout: "", exitCode: 1 }` | predicate only; reply unchanged |
| 2838–2844 | `4242` | `livePanes(command, 4242)` |
| 2883–2889 | `{ stdout: "", exitCode: 1 }` | predicate only; reply unchanged |
| 5313–5315 | `launched ? { stdout: "12345\n", exitCode: 0 } : { stdout: "", exitCode: 1 }` | `launched ? livePanes(command) : { stdout: "", exitCode: 1 }` |
| 5881–5887 | `4242` | `livePanes(command, 4242)` |
| 6624–6632 | predicate also has `command.includes("%7") &&`; reply `{ stdout: "22222\n", exitCode: 0 }` | keep the `%7` clause, update the format clause; reply `{ stdout: "%7 22222\n", exitCode: 0 }` |
| 8757–8764 | same shape as 6624 | same change: `{ stdout: "%7 22222\n", exitCode: 0 }` |
| 9777–9783 | `4242` | `livePanes(command, 4242)` |

- [ ] **Step 4: Group B — generic `list-panes` fixtures with no format check that reply with a bare pid (26 edits).** Predicates unchanged; only the alive reply changes.

| lines | today | change to |
| :--- | :--- | :--- |
| 219–221 (`liveRun`) | `return Promise.resolve({ stdout: "12345\n", exitCode: 0 });` | `return Promise.resolve(livePanes(command));` |
| 1385–1387 | `command.includes("%7")` → `{ stdout: "12345\n", exitCode: 0 }` | `{ stdout: "%7 12345\n", exitCode: 0 }` |
| 3683–3686 | `controllerSpawned ? { stdout: "12345\n", exitCode: 0 } : …` | `controllerSpawned ? livePanes(command) : …` |
| 3731–3733 | `controllerLive ? { stdout: "12345\n", exitCode: 0 } : …` | `controllerLive ? livePanes(command) : …` |
| 3774 | `if (command[3] === "list-panes") return { stdout: "12345\n", exitCode: 0 };` | `if (command[3] === "list-panes") return livePanes(command);` |
| 3831 | same | same |
| 3889 | same | same |
| 3935 | same | same |
| 3997–3999 | `panesAlive ? { stdout: "12345\n", exitCode: 0 } : …` | `panesAlive ? livePanes(command) : …` |
| 4056–4068 | line 4067 `return { stdout: "12345\n", exitCode: 0 };` | `return livePanes(command);` (keep the `listPanesCalls` bookkeeping above it) |
| 4114 | plain one-liner | `return livePanes(command);` |
| 4182–4184 | `paneAlive ? { stdout: "12345\n", exitCode: 0 } : …` | `paneAlive ? livePanes(command) : …` |
| 4240–4242 | same | same |
| 4312–4314 | same | same |
| 4374 | plain one-liner | `return livePanes(command);` |
| 4429–4435 | line 4434 `return { stdout: "12345\n", exitCode: 0 };` | `return livePanes(command);` (keep the `probeGate` await above it) |
| 4576–4578 | `paneAlive ? …` | `paneAlive ? livePanes(command) : …` |
| 4660–4662 | same | same |
| 4725–4727 | same | same |
| 4794–4796 | same | same |
| 4953–4956 | `killPaneSucceeded ? { stdout: "", exitCode: 1 } : { stdout: "12345\n", exitCode: 0 }` | `killPaneSucceeded ? { stdout: "", exitCode: 1 } : livePanes(command)` |
| 5031–5033 | `paneAlive ? …` | `paneAlive ? livePanes(command) : …` |
| 5133 | plain one-liner | `return livePanes(command);` |
| 5179 | plain one-liner | `return livePanes(command);` |
| 5219–5222 | `controllerSpawned ? { stdout: "12345\n", exitCode: 0 } : …` | `controllerSpawned ? livePanes(command) : …` |
| 9838–9840 | `command[5] === "%1"` → `{ stdout: "12345\n", exitCode: 0 }` | `{ stdout: "%1 12345\n", exitCode: 0 }` |

- [ ] **Step 5: Group C — leave these alone** (they answer exit 1 always, answer only the `-F "#{pane_id}"` probes of `firstPaneId`/`windowAlive`, or are `list-panes -a` for `listUnknownPanes`): 1153–1159, 1969–1973, 2159–2161, 3563–3565, 4496–4500, 4873, 5310–5312, 5737–5741, 5803–5809, 6207–6213, 6419–6423, 6519–6521, 6526–6530, 6640–6644, 6830–6834, 6897–6901, 8873–8878, 8957–8959, 9041–9044, 9121–9122, 9203–9204, 9333–9337, 9640–9650, 9708–9718, 9768–9776, 9835–9837. Also unchanged: the `new-window` pin at 576–585 (`"#{window_id} #{pane_id} #{pane_pid}"`) and every `new-window`/`split-window` reply such as `"@42 %1 12345\n"` / `"%2 12345\n"`.

- [ ] **Step 6: Sweep for leftovers with `grep` (the tool, not shell)**

- Pattern `"12345\\n"` over `processes.test.ts`: every remaining hit must be a `new-window`/`split-window` reply (`"@… %… 12345\n"`, `"%2 12345\n"`) or inside `livePanes` itself. A bare `stdout: "12345\n"` inside any `list-panes` branch is a missed site.
- Pattern `#\{pane_pid\}"` over `processes.test.ts` and `worker-boot-watchdog.test.ts`: every hit must be `"#{pane_id} #{pane_pid}"` or `"#{window_id} #{pane_id} #{pane_pid}"`. A lone `"#{pane_pid}"` is a missed predicate or the stale argv pin.

- [ ] **Step 7: Format, then run the file, then the whole daemon suite**

Run: `bunx biome format src/ --write` — it collapses the three ternaries that now fit on one line (sites 3683–3686, 4953–4956, 5219–5222 become e.g. `return controllerSpawned ? livePanes(command) : { stdout: "", exitCode: 1 };`). Nothing else should change; check with `jj -R "$LEGION_WORKSPACE" diff --stat`.
Run: `bun test src/daemon/__tests__/processes.test.ts` → expected 190 pass, 0 fail (193 collected, 3 skipped).
Run: `env -u DISPATCH_URL -u DISPATCH_TOKEN_FILE bun test` (in `packages/daemon`) → expected 809 pass, 5 skip, 0 fail. (Without the `env -u`, exactly one pre-existing failure appears — see "Branch state".)

### Task 4: Gates, commit, push, PR

**Files:** none new.

- [ ] **Step 1: Gates in `packages/daemon`**

```bash
cd -- "$LEGION_WORKSPACE/packages/daemon" && bunx biome check src/ && bunx tsc --noEmit && env -u DISPATCH_URL -u DISPATCH_TOKEN_FILE bun test
```

Expected: biome clean, tsc clean (tsconfig `include: ["src/**/*.ts"]` covers the tests), 809 pass / 5 skip / 0 fail. CI runs the same three commands without the pane environment, so the `env -u` is only needed on this rig.

- [ ] **Step 2: One implementation commit** (behavior + its regression locks + fixture cutover are one indivisible, bisectable change — row-form fixtures do not pass against the old parser and the old fixtures do not pass against the new one):

```bash
cd -- "$LEGION_WORKSPACE" && jj -R "$LEGION_WORKSPACE" split \
  -m 'fix(daemon): panePid reads the target pane'"'"'s own pid, not its window'"'"'s first pane

`list-panes -t <pane>` lists the pane'"'"'s whole window, so the first pid was the
architect'"'"'s for every split-in worker: a dead worker whose architect was alive
probed alive, and the boot watchdog skipped its socket probe. `panePid` now lists
`#{pane_id} #{pane_pid}` and picks the row whose pane id is the target (a window
id still takes the first row, the pane `firstPaneId` backfills). A pane id missing
from the listing is undefined, never a sibling'"'"'s pid.

Dispatch: LEGION-9' \
  packages/daemon/src/daemon/tmux.ts \
  packages/daemon/src/daemon/__tests__/tmux.test.ts \
  packages/daemon/src/daemon/__tests__/worker-boot-watchdog.test.ts \
  packages/daemon/src/daemon/__tests__/processes.test.ts
```

(`.omp/config.yml` stays out of the commit because `split` takes explicit paths.) Then check ancestry: `jj -R "$LEGION_WORKSPACE" log -r 'ancestors(@, 5)'` — only LEGION-9 commits above `e47fb952`.

- [ ] **Step 3: Push and open the PR** (implementer only)

```bash
cd -- "$LEGION_WORKSPACE" && jj -R "$LEGION_WORKSPACE" bookmark set legion/LEGION-9 && \
  jj -R "$LEGION_WORKSPACE" git push --bookmark legion/LEGION-9
legion gh -- pr create --repo sjawhar/legion --base main --head legion/LEGION-9 \
  --title 'fix(daemon): panePid reads the target pane'"'"'s own pid, not its window'"'"'s first pane' \
  --body-file /tmp/legion9-pr-body.md
```

PR body: the defect (two sentences, the `%3` reproduction above), `Dispatch: LEGION-9`, and the `## Verification` section in the READY format from `skills/legion-worker/SKILL.md` (CI run id at head; Threads; Thermo; **E2E left for the tester** with the driver output from Task 5; Fast-follow `none`; Chain `not stacked`).

- [ ] **Step 4: Implementer handoff** — `legion handoff write --phase implement --data '{…}'` with `commit`, `headSha`, `prNumber`, `prUrl`, `deviationsFromPlan` (expected: none; if a fixture site's line drifted, list the site as edited), `fixtureSitesEdited` (count: 47 + helper), and `testerNotes` pointing at Task 5. Then `jj split -m "implement: record handoff" .legion/implement.json`, bookmark set, push, `legion handoff complete`.

### Task 5: Tester — live check on this rig (acceptance 5) and the E2E section

**Not committed.** Location: `/tmp/legion9-live/` (outside the workspace, so jj never snapshots it). Run against `main`'s `panePid` first (reproduces the defect), then the branch's. Run from your own pane shell: it inherits `TMUX_TMPDIR=/home/ubuntu/.tmux/sockets`, where the daemon's private server socket lives; a shell without it (a Python/JS kernel subprocess, for instance) talks to no server and every tmux call fails — the script throws on that rather than printing defect-lookalike rows.

The planner dry-ran this exact script (state as of planning: one worker pane, `%3`): against `main`'s `panePid` → `FAIL … panePid=3715931 display-message=4141285 window-first-row="%1 3715931"`, `1 MISMATCH`, exit 1; against the plan's new `panePid` → `OK … panePid=4141285 display-message=4141285`, `negative control %999999 → undefined`, `ALL MATCH`, exit 0.

- [ ] **Step 1: Extract `main`'s `tmux.ts`** (its only import is `import type`, erased by Bun, so the file runs standalone):

```bash
mkdir -p /tmp/legion9-live && cd -- "$LEGION_WORKSPACE" && \
  jj -R "$LEGION_WORKSPACE" file show -r main packages/daemon/src/daemon/tmux.ts > /tmp/legion9-live/tmux-main.ts
```

- [ ] **Step 2: Write `/tmp/legion9-live/probe.ts`**

```ts
// Throwaway LEGION-9 live check — never committed.
// Usage: bun /tmp/legion9-live/probe.ts <path to tmux.ts> [state.json]
import { readFileSync } from "node:fs";

const stateDir = process.env.LEGION_STATE_DIR ?? "/home/ubuntu/.local/state/legion/sjawhar-legion";
const [tmuxModulePath, statePath = `${stateDir}/state.json`] = process.argv.slice(2);
if (!tmuxModulePath) throw new Error("usage: bun probe.ts <tmux.ts path> [state.json]");
const { panePid } = await import(tmuxModulePath);
const socket = "legion-sjawharlegion";

async function run(cmd: string[]): Promise<{ stdout: string; stderr: string; exitCode: number }> {
  const proc = Bun.spawn(cmd, { stdout: "pipe", stderr: "pipe" });
  const [stdout, stderr, exitCode] = await Promise.all([
    new Response(proc.stdout).text(),
    new Response(proc.stderr).text(),
    proc.exited,
  ]);
  return { stdout, stderr, exitCode };
}
const server = { socket, run };
const tmux = (...args: string[]) => run(["tmux", "-L", socket, ...args]);

const state = JSON.parse(readFileSync(statePath, "utf8"));
const panes: Array<{ role: string; windowId: string; paneId: string }> = [];
for (const [role, claim] of Object.entries<any>(state.roles ?? {})) {
  const paneId = claim?.locator?.tmuxPaneId;
  if (typeof paneId === "string") panes.push({ role, windowId: claim.locator.tmuxWindowId, paneId });
}
if (panes.length === 0) throw new Error(`no roles[*].locator.tmuxPaneId in ${statePath}`);

let mismatches = 0;
for (const { role, windowId, paneId } of panes) {
  const got = await panePid(server, paneId);
  const expected = Number((await tmux("display-message", "-p", "-t", paneId, "#{pane_pid}")).stdout.trim());
  if (!Number.isSafeInteger(expected) || expected <= 0) {
    throw new Error(`display-message gave no pid for ${paneId}: is TMUX_TMPDIR the daemon's (/home/ubuntu/.tmux/sockets)?`);
  }
  const firstRow = (await tmux("list-panes", "-t", windowId, "-F", "#{pane_id} #{pane_pid}")).stdout.split("\n")[0];
  const ok = got === expected;
  if (!ok) mismatches += 1;
  console.log(
    `${ok ? "OK  " : "FAIL"} ${role} ${windowId}/${paneId}: panePid=${got} display-message=${expected} window-first-row="${firstRow}"`
  );
}
// Negative control: a pane id that does not exist must be undefined, never some other pane's pid.
const missing = await panePid(server, "%999999");
console.log(`negative control %999999 → ${missing} (expected undefined)`);
if (missing !== undefined) mismatches += 1;
console.log(mismatches === 0 ? "ALL MATCH" : `${mismatches} MISMATCH`);
process.exit(mismatches === 0 ? 0 : 1);
```

- [ ] **Step 3: Run against `main` — expect the defect**

```bash
bun /tmp/legion9-live/probe.ts /tmp/legion9-live/tmux-main.ts
```

Expected: `FAIL` for every worker pane that is not the first pane of its window (its `panePid` equals the `window-first-row` pid, not `display-message`), exit 1. At the time of planning `roles[*]` held one pane locator (`legion-sjawharlegion-legion-9-planner` → `@1/%3`; window `@1`'s first pane is the architect `%1`); by test time the implementer's and tester's own panes are also in `@1`. A pane that *is* first in its window (a child issue's first worker opens its own window) trivially matches under both builds — note which rows those are; at least one non-first row must be present for the run to be evidence.

- [ ] **Step 4: Run against the branch — expect all match**

```bash
bun /tmp/legion9-live/probe.ts "$LEGION_WORKSPACE/packages/daemon/src/daemon/tmux.ts"
```

Expected: `OK` on every row, `negative control %999999 → undefined`, `ALL MATCH`, exit 0.

- [ ] **Step 5: Record** both outputs verbatim in `.legion/test.json` (`liveCheck: { main: <output>, branch: <output>, headSha }`) and in the PR body's `E2E` line: surface = the daemon's liveness probe against the live private tmux server `tmux -L legion-sjawharlegion`; command = the two `bun /tmp/legion9-live/probe.ts …` invocations; negative control = `%999999 → undefined`. Also run the tmux 3.7c one-liner reproduction from "Branch state" above at test time for the record.

### Task 6: Gates for reviewer/merger (acceptance 6)

- `bunx biome check src/`, `bunx tsc --noEmit`, `env -u DISPATCH_URL -u DISPATCH_TOKEN_FILE bun test` — all green in `packages/daemon` at the reviewed head (the `env -u` only on this rig; see "Branch state").
- Exactly one PR against `sjawhar/legion` from `legion/LEGION-9`, body containing `Dispatch: LEGION-9`.
- Reviewer verifies: callers unchanged (`processes.ts:1693–1701`, `2878–2895`; `worker-boot-watchdog.ts:177–179`), `firstPaneId`/`windowAlive` unchanged, argv still built through `argv(server, …)`, no `display-message` anywhere in `tmux.ts`.

## Self-review against the spec

- Acceptance 1, 2 → Task 1 Step 1 / Task 2 (`tmux.test.ts`, exact argv asserted, `@1464` → first row, exit ≠ 0 → undefined, missing pane row → undefined).
- Acceptance 3 → Task 1 Step 2 (`%1533`, three rows, `isOmpPane` true only for `2363427`, socket probe invoked, boot not confirmed) + Task 2 Step 3 (three fixtures at 210/247/282).
- Acceptance 4 → Task 3 (argv pin 1404–1413; 47 fixture edits enumerated; leftover sweep) — superset of the spec's "≈20".
- Acceptance 5 → Task 5 (driver in `/tmp/legion9-live`, `main` then branch, outputs pasted into the test handoff and PR E2E line).
- Acceptance 6 → Task 4 / Task 6.
- § Errors: exit ≠ 0, pane target absent, non-positive/non-safe pid — all in the Task 2 implementation; the first two are unit-tested, the third is the pre-existing check.
- § Rejected: nothing here uses `display-message` (the tester's driver uses it only as the independent oracle to compare against, outside the daemon), `-a`, or caller changes.
