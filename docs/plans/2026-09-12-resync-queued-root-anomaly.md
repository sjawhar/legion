# Resync stops reporting queued roots as `zero-owner-tree` (LEGION-14) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `runResync`'s `reportRootAnomalies` no longer reports a root whose tree is `queued` (waiting in `admission.queue`) as `zero-owner-tree`, and reports a `launch-failed` root exactly once (as `launch-failed`), so the controller is never woken for a root the daemon already owns.

**Architecture:** One gate changes, in `packages/daemon/src/daemon/resync.ts` `reportRootAnomalies`: the non-triage branch skips a root whose `trees[key].status` is `active`, `queued`, or `launch-failed`, reading tree status directly (the loop already skips every child at line 250, so a root's own tree entry is the whole ownership question). `hasActiveTree` — whose only caller is that gate — is deleted rather than widened. `admission.queue`/`admission.active` membership is not consulted, so a queue entry with no `trees[key]` stays reported. The `launch-failed` loop (lines 286–297), `hasTree`, the triage branch, and the controller are untouched.

**Tech Stack:** TypeScript on Bun, `bun:test`, jj. Live proof: a throwaway bun driver over read-only `/tmp` copies of this rig's real `state.json` files, with a mock `DispatchClient`/`applyEffects`.

**Spec:** `dispatch://LEGION-14/spec` (v2; design gate approved on ask `b4af41f5`). The design is fixed — do not re-open it. Rejected there: filtering in the controller; widening `hasActiveTree`; gating on `admission.queue`/`admission.active` membership; a worker restarting the live daemon.

## Global Constraints

- Only `packages/daemon/src/daemon/resync.ts` and `packages/daemon/src/daemon/__tests__/resync.test.ts` change. (spec § Design)
- The gate reads `deps.state.trees[node.key]?.status` — never `admission.queue`/`admission.active`. A `todo` root in `admission.queue` with no `trees[key]` is still `zero-owner-tree`. (spec § Errors, § Rejected)
- `hasActiveTree` is not widened. It has exactly one caller (`resync.ts:278`, verified by grep over `packages/daemon/src`), and `packages/daemon/tsconfig.json` sets `noUnusedLocals: true`, so once the gate stops calling it, it MUST be deleted or `tsc` fails. Deleting it does not change any other caller — there is none. (spec § Design says it "keeps its current meaning for callers that need a live architect"; the verified fact is that no such caller exists. Recorded in the plan handoff for the architect.)
- The `launch-failed` loop at `resync.ts:286–297` and every controller-side file are untouched. (spec § Design)
- The `zero-owner-tree` `detail` string stays `Dispatch status "<status>" has no active Legion tree` — still accurate for every root that now reaches it (no tree, or `lingering`/`dead`/`closed`), and unchanged wording keeps the two characterization tests (Task 1 tests 2a/2b) green before and after the fix, so the only pre-fix failures are the two that prove the bug.
- Biome: double quotes, semicolons, ES5 trailing commas, 100-char width. `import type` for type-only imports. Tests co-located in `__tests__/`, `bun:test`, flat `it(...)` blocks inside the existing `describe("runResync")`.
- Never commit `.omp/config.yml` (an empty file the Legion extension provisioned; shows as `A .omp/config.yml` in `jj status`; `.omp/` is not gitignored). Commit with explicit paths (`jj commit -m … <paths>` / `jj split … <paths>`) and point the bookmark at the described commit (`-r @-`), never at the bare working copy — `jj git push` refuses an undescribed commit.
- Workers never restart, signal, or write to the daemon at `/home/ubuntu/legion-ws-RunDaemon`, and never write anything under `$LEGION_STATE_DIR` (`/home/ubuntu/.local/state/legion/sjawhar-legion`). Live-proof inputs are `/tmp` copies. (spec § Testing "1 (live)", § Rejected)
- One PR against `sjawhar/legion` from branch `legion/LEGION-14`; PR body carries `Dispatch: LEGION-14` and the READY-format `## Verification` section from `skills/legion-worker/SKILL.md`.

## Branch and rig state (verified 2026-09-12 04:20Z)

- Workspace `@-` = `ktqlulks` / `7db2cd8357ca` (`chore: release cli v1.1.2 [skip ci]`), the branch base. The planner's handoff commit will sit on top of it; the fix commit goes on top of that. `resync.ts` is identical at every one of those revisions until the fix.
- `bun install --frozen-lockfile` at the workspace root was run by the planner (the provisioned workspace had no `node_modules`; it is gitignored, `jj status` unchanged).
- Baseline, `cd packages/daemon && env -u DISPATCH_URL -u DISPATCH_TOKEN_FILE bun test src/daemon/__tests__/resync.test.ts` → **32 pass, 0 fail** (279 s wall). The `env -u` is needed on this rig for the same reason LEGION-9's plan records: every Legion pane carries `DISPATCH_URL` without `DISPATCH_TOKEN`, which makes `src/cli/__tests__/index.test.ts:156` (`--check-config`) refuse in the full suite. Pre-existing, unrelated, green in CI.
- The box is saturated (load average 285 on 32 cores at plan time; `tmux: server` and several `python3` at 80–90 % CPU). Expect: one `bun test` of `resync.test.ts` ≈ 5 min; each live-proof driver run 15–60 s wall at <1 s CPU; each `jj` command 20–45 s. Set tool timeouts ≥ 600 s for the test file and ≥ 180 s per driver run.
- Existing tests in `resync.test.ts` cover only `untriaged-open` (line 149–153). There is no existing test for `zero-owner-tree` or `launch-failed`, so no assertion pins the old gate.
- `reportRootAnomalies` iterates `Object.values(deps.state.issues)` and `continue`s on `node.parent` at line 250, so every node that reaches line 278 is a root. `hasActiveTree`'s parent-chain walk therefore never advanced past the first lookup for this caller; the direct `deps.state.trees[node.key]?.status` read is the same computation without the dead loop.
- The bug reproduced on this rig's real state (planner, pre-fix code, read-only `/tmp` copies; see Task 2 for the driver):
  - `$LEGION_STATE_DIR/state.json.v22.bak` — written by `loadState`'s migration at **2026-09-11 23:04:12Z**, inside the controller's observed wake window (23:14, 23:41, 23:51, 00:01Z). `admission: {cap 1, active [LEGION-9], queue [LEGION-10, LEGION-11]}`; both queued roots `status: "todo"`, `trees[key].status: "queued"`, `generation 0`, `launchFailures 0`; `pendingStatusWrites: {}`; trees `LEGION-6 lingering` (issue `done`), `LEGION-9 active`. Pre-fix `runResync({force: true})` → anomalies `[zero-owner-tree LEGION-10, zero-owner-tree LEGION-11]`, one `probe` effect (LEGION-9).
  - `$LEGION_STATE_DIR/state.json` at 04:14Z — `cap 12` saturated, `queue [LEGION-22, LEGION-23]`, but both queued entries have `parent: "LEGION-19"` (children, skipped at line 250). Pre-fix → **zero** anomalies, 12 `probe` effects. So the current snapshot is a no-regression control, not the proof; the v22 backup is the proof input. The tester re-copies `state.json` at run time and reports what the queue held then.
- Every expected value below was **observed, not predicted**: the planner ran the four Task 1 tests from a `/tmp` copy against the workspace's pre-fix `resync.ts` (2 fail / 2 pass, exactly the failures Task 1 Step 2 lists), then applied precisely the Task 1 Step 3 edits to a `/tmp` copy of `resync.ts` and re-ran them (4 pass), and ran the Task 2 driver against that fixed copy: `state.json.v22.bak` `2 → 0` anomalies, `state.json` `0 → 0`, the dead-`LEGION-10` negative control → exactly `zero-owner-tree:LEGION-10`, `probe` counts unchanged (1 and 12), and `$LEGION_STATE_DIR` still holding exactly its three files. The workspace's tracked files were not touched; `/tmp/legion14-live/` (driver, before/fixed copies, snapshots, reports) is left in place for the implementer and tester.

---

### Task 1: Regression tests and the gate fix

**Files:**
- Modify: `packages/daemon/src/daemon/resync.ts:62-71` (delete `hasActiveTree`), `:241-245` (docstring), `:278-284` (the gate)
- Test: `packages/daemon/src/daemon/__tests__/resync.test.ts` — insert after line 183 (the closing `});` of the `options.force bypasses the interval throttle…` test), before line 185 (`it("reconciles an unsettled red PR with failing check names"`)

**Interfaces:**
- Consumes: `newLegionState(project, cap)`, `runResync(deps, options?)`, the file's `resyncDeps(state)` helper (line 12) and `issue` constant (`"LEGION-42"`, line 10); `IssueKey` is already imported (line 2). No new imports.
- Produces: nothing exported changes. `ResyncAnomaly`, `LegionEventPayload`, `RunResyncDeps`, `runResync` keep their signatures.

- [ ] **Step 1: Add the four tests**

Insert verbatim after line 183 of `resync.test.ts`:

```ts
  it("does not report a queued root as zero-owner-tree while the admission cap is saturated", async () => {
    const state = newLegionState("omp", 1);
    const activeRoot: IssueKey = "LEGION-10";
    const queuedRoots: IssueKey[] = ["LEGION-11", "LEGION-12"];
    state.issues[activeRoot] = {
      key: activeRoot,
      title: "Admitted root",
      status: "in_progress",
      children: [],
    };
    state.trees[activeRoot] = {
      root: activeRoot,
      generation: 1,
      status: "active",
      launchFailures: 0,
    };
    for (const key of queuedRoots) {
      state.issues[key] = { key, title: `Queued root ${key}`, status: "todo", children: [] };
      state.trees[key] = { root: key, generation: 0, status: "queued", launchFailures: 0 };
    }
    state.admission.active = [activeRoot];
    state.admission.queue = [...queuedRoots];

    const result = await runResync(resyncDeps(state));

    expect(result.anomalies).toEqual([]);
  });

  it("reports a todo root whose tree is dead as zero-owner-tree", async () => {
    const state = newLegionState("omp", 1);
    state.issues[issue] = { key: issue, title: "Dead root", status: "todo", children: [] };
    state.trees[issue] = { root: issue, generation: 2, status: "dead", launchFailures: 0 };

    const result = await runResync(resyncDeps(state));

    expect(result.anomalies).toEqual([
      {
        kind: "zero-owner-tree",
        issue,
        detail: 'Dispatch status "todo" has no active Legion tree',
      },
    ]);
  });

  it("reports a todo root queued for admission without a tree entry as zero-owner-tree", async () => {
    const state = newLegionState("omp", 1);
    state.issues[issue] = {
      key: issue,
      title: "Queue entry without a tree",
      status: "todo",
      children: [],
    };
    state.admission.queue = [issue];

    const result = await runResync(resyncDeps(state));

    expect(result.anomalies).toEqual([
      {
        kind: "zero-owner-tree",
        issue,
        detail: 'Dispatch status "todo" has no active Legion tree',
      },
    ]);
  });

  it("reports a todo root whose tree launch failed once, as launch-failed only", async () => {
    const state = newLegionState("omp", 1);
    state.issues[issue] = {
      key: issue,
      title: "Launch-failed root",
      status: "todo",
      children: [],
    };
    state.trees[issue] = {
      root: issue,
      generation: 3,
      status: "launch-failed",
      launchFailures: 3,
    };

    const result = await runResync(resyncDeps(state));

    expect(result.anomalies).toEqual([
      { kind: "launch-failed", issue, detail: "tree launch failed 3 times" },
    ]);
  });
```

Why these four: test 1 is spec § Testing row 1 (one `active` + two `queued` roots, cap saturated). Tests 2a (`dead` tree) and 2b (queue entry, no tree) are spec § Acceptance 2 and § Errors row 2 — 2b is also the guard that the fix reads tree status, not queue membership. Test 3 is spec § Testing row 3. The `active` root has no `locator`/`readyConfirmedAt`, so `probeActiveRoots` emits nothing and `resyncDeps`' no-op `applyEffects` is fine.

- [ ] **Step 2: Run the new tests and confirm exactly two fail, for the right reason**

```bash
cd -- "$LEGION_WORKSPACE/packages/daemon" && env -u DISPATCH_URL -u DISPATCH_TOKEN_FILE \
  bun test src/daemon/__tests__/resync.test.ts -t "zero-owner-tree|launch-failed"
```

Expected: 4 tests matched; **2 fail, 2 pass**.
- FAIL `does not report a queued root…` — received `[{kind: "zero-owner-tree", issue: "LEGION-11", …}, {kind: "zero-owner-tree", issue: "LEGION-12", …}]`, expected `[]`.
- FAIL `…launch failed once, as launch-failed only` — received two anomalies (`zero-owner-tree` then `launch-failed`), expected one.
- PASS `…tree is dead…` and `…without a tree entry…` (existing behavior, kept by the fix).

Any other outcome means the test code or its placement is wrong — fix that before touching `resync.ts`.

- [ ] **Step 3: Apply the fix to `resync.ts`**

(a) Delete `hasActiveTree` entirely — lines 62–71:

```ts
function hasActiveTree(state: LegionState, issue: IssueKey): boolean {
  const seen = new Set<IssueKey>();
  let current: IssueKey | undefined = issue;
  while (current && !seen.has(current)) {
    if (state.trees[current]?.status === "active") return true;
    seen.add(current);
    current = state.issues[current]?.parent;
  }
  return false;
}
```

Leave `hasTree` (lines 51–60) exactly as is — the triage branch still calls it.

(b) Replace the docstring at lines 241–245 with:

```ts
/** Root-issue consistency anomalies a healed drift scan cannot itself explain: an issue Dispatch
 * still considers alive but whose tree/admission bookkeeping has fallen out of step with its own
 * status. A root whose tree is `queued` is waiting for an admission slot the daemon itself owns
 * and is not an anomaly; a `launch-failed` one is reported exactly once, by the launch-failed
 * loop below, never also as `zero-owner-tree`. Never self-heals `zero-owner-tree` (an architect
 * or human must decide whether to re-admit); `untriaged-open` self-heals by re-emitting the
 * `issue.created` controller wake the daemon apparently lost. */
```

(c) Replace the gate at lines 278–284:

```ts
    if (!hasActiveTree(deps.state, node.key)) {
      anomalies.push({
        kind: "zero-owner-tree",
        issue: node.key,
        detail: `Dispatch status "${node.status}" has no active Legion tree`,
      });
    }
```

with:

```ts
    // Every node reaching here is a root (children were skipped above), so its own tree entry
    // is the whole ownership question. `queued` waits for an admission slot the daemon owns;
    // `launch-failed` is reported by the loop below. Anything else -- no tree, or one that is
    // lingering, dead, or closed -- leaves a still-open issue with nobody responsible for it.
    const treeStatus = deps.state.trees[node.key]?.status;
    if (treeStatus === "active" || treeStatus === "queued" || treeStatus === "launch-failed") {
      continue;
    }
    anomalies.push({
      kind: "zero-owner-tree",
      issue: node.key,
      detail: `Dispatch status "${node.status}" has no active Legion tree`,
    });
```

Nothing else in the file changes. `IssueKey` and `LegionState` imports stay (both still used elsewhere in the file).

- [ ] **Step 4: Run the four tests, then the whole file**

```bash
cd -- "$LEGION_WORKSPACE/packages/daemon" && env -u DISPATCH_URL -u DISPATCH_TOKEN_FILE \
  bun test src/daemon/__tests__/resync.test.ts -t "zero-owner-tree|launch-failed"
```
Expected: 4 pass, 0 fail.

```bash
cd -- "$LEGION_WORKSPACE/packages/daemon" && env -u DISPATCH_URL -u DISPATCH_TOKEN_FILE \
  bun test src/daemon/__tests__/resync.test.ts
```
Expected: **36 pass, 0 fail** (baseline 32 + 4). Budget ≈ 5 min on this rig.

- [ ] **Step 5: Type-check and lint the daemon package**

```bash
cd -- "$LEGION_WORKSPACE/packages/daemon" && bunx tsc --noEmit
cd -- "$LEGION_WORKSPACE/packages/daemon" && bunx biome check src/daemon/resync.ts src/daemon/__tests__/resync.test.ts
```
Expected: `tsc` exits 0 (this is what proves `hasActiveTree` had no other caller — `noUnusedLocals` would otherwise have failed on it before deletion, and any missed caller would fail now). Biome exits 0; if it reports formatting only, run `bunx biome format --write` on those two files and re-check. Do not touch any other file.

Then the daemon package's full suite (the tester runs the root suite):
```bash
cd -- "$LEGION_WORKSPACE/packages/daemon" && env -u DISPATCH_URL -u DISPATCH_TOKEN_FILE bun test
```
Expected: 0 fail apart from the two known items LEGION-9's plan records — `--check-config` (only without the `env -u`) and the single-run flake of `ProcessManager > reconnectRoots re-arms the registration deadline…` (`processes.test.ts`, re-run once; two consecutive failures is a real finding).

- [ ] **Step 6: Commit with explicit paths, advance the bookmark to the described commit, push, open the PR**

```bash
cd -- "$LEGION_WORKSPACE" && jj -R "$LEGION_WORKSPACE" commit \
  -m $'fix(daemon): a queued root is not a zero-owner-tree resync anomaly\n\nreportRootAnomalies gated its non-triage branch on hasActiveTree (tree status\n"active" only), so every root waiting in admission.queue (tree "queued") was\nreported as zero-owner-tree each resync cycle, waking the controller for nothing,\nand a launch-failed root was reported twice (here and by the launch-failed loop).\nThe gate now skips a root whose own tree is active, queued, or launch-failed,\nreading tree status directly -- never admission.queue membership, so a queue entry\nwith no tree is still reported. hasActiveTree had no other caller and is removed.\n\nDispatch: LEGION-14' \
  packages/daemon/src/daemon/resync.ts packages/daemon/src/daemon/__tests__/resync.test.ts
cd -- "$LEGION_WORKSPACE" && jj -R "$LEGION_WORKSPACE" log -r 'ancestors(@, 4)'
```
Expected log: `@` (empty description, carries only `.omp/config.yml`) → the fix commit → `plan: record handoff` → `ktqlulks`. Nothing from another issue.

```bash
cd -- "$LEGION_WORKSPACE" && jj -R "$LEGION_WORKSPACE" bookmark set legion/LEGION-14 -r @- && \
  jj -R "$LEGION_WORKSPACE" git push --bookmark legion/LEGION-14
```

Open the PR with `legion gh -- pr create --repo sjawhar/legion --base main --head legion/LEGION-14 --title "fix(daemon): a queued root is not a zero-owner-tree resync anomaly" --body-file <file>` whose body contains `Dispatch: LEGION-14` and the READY-format `## Verification` section (CI line filled once `pr-checks-result` finishes; `E2E` left for the tester; `Thermo` for the reviewer; `Fast-follow: none`; `Chain: not stacked`).

- [ ] **Step 7: Write `.legion/implement.json`, commit it with `jj split`, push**

Fields the tester and architect need: `fixChangeId` (the `jj` change id of the fix commit), `fixCommitSha`, `prNumber`, `prUrl`, `filesChanged` (the two paths), `testsAdded` (the four `it` titles), `hasActiveTreeRemoved: true`, `deviationsFromPlan` (empty array if none), `testerNotes` (point at Task 2 here; note the `env -u` and the rig's timings). Then the skill's handoff commit:

```bash
cd -- "$LEGION_WORKSPACE" && jj -R "$LEGION_WORKSPACE" split -m "implement: record handoff" .legion/implement.json && \
  jj -R "$LEGION_WORKSPACE" bookmark set legion/LEGION-14 -r @- && \
  jj -R "$LEGION_WORKSPACE" git push --bookmark legion/LEGION-14
```

---

### Task 2: Live proof over this rig's real daemon state (tester)

Spec § Testing "1 (live)". Runs the fixed `runResync` and its parent over read-only `/tmp` copies of two real state files, with every side effect mocked, and captures both anomaly lists. Nothing here talks to the live daemon, Dispatch, GitHub, or tmux, and nothing is written under `$LEGION_STATE_DIR`.

**Files:**
- Create (throwaway, never committed): `/tmp/legion14-live/run-resync.ts`, `/tmp/legion14-live/resync-before.ts`, `/tmp/legion14-live/state-*.json`, `/tmp/legion14-live/{before,after}-*.json`
- Read-only inputs: `$LEGION_STATE_DIR/state.json.v22.bak` (the incident-window snapshot), `$LEGION_STATE_DIR/state.json` (current)

**Interfaces:**
- Consumes: `runResync(deps, {force: true})` from the pre-fix module (`jj file show -r "${FIX}-"`) and from the workspace file; `loadState(file, init)` from `legion-state.ts`; `RunResyncDeps`.
- Produces: four JSON reports plus one negative-control report, quoted in `.legion/test.json` and the PR body's `E2E` section.

- [ ] **Step 1: Stage the inputs and the pre-fix module (never point anything at `$LEGION_STATE_DIR` directly)**

`loadState` writes `<input>.v22.bak` beside a pre-current-version input — that is why the v22 backup MUST be copied to `/tmp` first; loading it in place would drop `state.json.v22.bak.v22.bak` into the live daemon's directory. The driver refuses any input outside `/tmp/`.

```bash
mkdir -p /tmp/legion14-live
cp "$LEGION_STATE_DIR/state.json.v22.bak" /tmp/legion14-live/state-2026-09-11T2304Z-v22.json
cp "$LEGION_STATE_DIR/state.json"         /tmp/legion14-live/state-current.json
ls -la "$LEGION_STATE_DIR"/state.json*     # record: exactly state.json, state.json.v21.bak, state.json.v22.bak
FIX=<fixChangeId from .legion/implement.json>
cd -- "$LEGION_WORKSPACE" && jj -R "$LEGION_WORKSPACE" diff -r "$FIX" --summary
#   expected: exactly M packages/daemon/src/daemon/resync.ts and M packages/daemon/src/daemon/__tests__/resync.test.ts
cd -- "$LEGION_WORKSPACE" && jj -R "$LEGION_WORKSPACE" file show -r "${FIX}-" packages/daemon/src/daemon/resync.ts \
  | sed -E "s#from \"\.\./#from \"$LEGION_WORKSPACE/packages/daemon/src/#; s#from \"\./#from \"$LEGION_WORKSPACE/packages/daemon/src/daemon/#" \
  > /tmp/legion14-live/resync-before.ts
```

The `sed` rewrites the pre-fix file's relative imports (`../state/fetch`, `./config`, `./dispatch-client`, `./legion-state`, `./reducers`) to absolute workspace paths so it runs from `/tmp`; the `@legion/contracts` import is `import type` and is erased by bun. The "after" module is the workspace file itself — no copy, no sed.

- [ ] **Step 2: Write the driver**

`/tmp/legion14-live/run-resync.ts`, verbatim (the planner ran exactly this against the pre-fix code; replace nothing — the absolute paths are this workspace's):

```ts
// LEGION-14 live proof driver (throwaway; lives in /tmp, never committed).
//
// Runs `runResync` from an arbitrary resync module over a read-only copy of a real daemon state
// file, with every side effect mocked, and prints the anomaly list. Nothing here talks to the
// live daemon, Dispatch, GitHub, or tmux.
//
// Usage (run with cwd = $LEGION_WORKSPACE/packages/daemon -- bun's startup from a non-project
// directory stalls ~30 s on module resolution):
//   bun /tmp/legion14-live/run-resync.ts <resync-module.ts> <state-copy.json> <out.json>
//
// <state-copy.json> MUST be a copy under /tmp: `loadState` writes `<file>.v<N>.bak` beside a
// pre-current-version input, so pointing it at <state_dir>/state.json.v22.bak would drop a stray
// file into the live daemon's state directory.
import path from "node:path";
import type { IssueDetails } from "/home/ubuntu/.local/state/legion/sjawhar-legion/workspaces/sjawhar/legion/legion-14/packages/contracts/src/dispatch-api";
import { loadState } from "/home/ubuntu/.local/state/legion/sjawhar-legion/workspaces/sjawhar/legion/legion-14/packages/daemon/src/daemon/legion-state";
import type { Effect, EnvelopeJson } from "/home/ubuntu/.local/state/legion/sjawhar-legion/workspaces/sjawhar/legion/legion-14/packages/daemon/src/daemon/reducers";
import type { RunResyncDeps } from "/home/ubuntu/.local/state/legion/sjawhar-legion/workspaces/sjawhar/legion/legion-14/packages/daemon/src/daemon/resync";

const [resyncModuleArg, stateCopyArg, outArg] = process.argv.slice(2);
if (!resyncModuleArg || !stateCopyArg || !outArg) {
  throw new Error("usage: bun run-resync.ts <resync-module.ts> <state-copy.json> <out.json>");
}
const resyncModulePath = path.resolve(resyncModuleArg);
const stateCopyPath = path.resolve(stateCopyArg);
const outPath = path.resolve(outArg);
if (!stateCopyPath.startsWith("/tmp/")) {
  throw new Error(`refusing to load ${stateCopyPath}: state input must be a copy under /tmp`);
}

// Dynamic import is required here: the module under test (pre-fix copy vs. workspace file) is
// selected at runtime from argv so one driver proves both sides.
const { runResync } = (await import(resyncModulePath)) as {
  runResync: (deps: RunResyncDeps, options?: { force?: boolean }) => Promise<{
    anomalies: Array<{ kind: string; issue: string; detail: string }>;
    healed: number;
  }>;
};

// `init` is consulted only when the file is missing (ENOENT); the copy exists, so it is inert.
const state = await loadState(stateCopyPath, { project: "sjawharlegion", cap: 1 });

const applied: Array<{ effects: Effect[]; envelope: EnvelopeJson }> = [];
const deps: RunResyncDeps = {
  state,
  config: { resyncIntervalMs: 0, dispatchProject: "LEGION", maxFixAttempts: 3 },
  dispatchClient: {
    // No drift to heal: the anomaly report is the only thing under test.
    listIssues: async () => [],
    // A pending status write is resolved in memory as "already landed" (retryPendingWrite
    // deletes it when remote.status === pending.status) so no PATCH is ever attempted.
    getIssue: async (key) => {
      const pending = state.pendingStatusWrites[key];
      if (!pending) throw new Error(`live proof: unexpected getIssue(${key})`);
      return { key, status: pending.status } as unknown as IssueDetails;
    },
    setStatus: async (key, status) => {
      throw new Error(`live proof: unexpected setStatus(${key}, ${status})`);
    },
  },
  saveState: async () => {},
  fetchCiStatusBatch: async () => ({}),
  applyEffects: async (effects, envelope) => {
    applied.push({ effects, envelope });
  },
  now: () => Date.now(),
};

const result = await runResync(deps, { force: true });

const roots = Object.values(state.issues).filter((node) => !node.parent);
const queuedRoots = state.admission.queue.filter((key) => !state.issues[key]?.parent);
// Written to a file, not stdout: `runResync` itself console.logs a `[legion] resync probed …`
// line, which would otherwise interleave with the JSON.
const report = {
  module: resyncModulePath,
  input: stateCopyPath,
  admission: state.admission,
  queuedRoots: queuedRoots.map((key) => ({
    key,
    dispatchStatus: state.issues[key]?.status,
    treeStatus: state.trees[key]?.status,
  })),
  queuedChildren: state.admission.queue.filter((key) => state.issues[key]?.parent),
  rootCount: roots.length,
  treeStatuses: Object.fromEntries(
    Object.entries(state.trees).map(([key, tree]) => [key, tree.status])
  ),
  pendingStatusWrites: Object.keys(state.pendingStatusWrites),
  anomalies: result.anomalies,
  healed: result.healed,
  effectKinds: applied.flatMap((entry) => entry.effects.map((effect) => effect.kind)),
};
await Bun.write(outPath, `${JSON.stringify(report, null, 2)}\n`);
console.log(`wrote ${outPath}: ${result.anomalies.length} anomalies`);
// Exit explicitly so no stray handle held by an imported daemon module can keep the process
// alive after the report is on disk.
process.exit(0);
```

What is mocked and why: `listIssues → []` (no drift heal; the anomaly report is the subject); `getIssue` answers a pending write with its own intended status so `retryPendingWrite` deletes it in memory and never PATCHes (both snapshots had `pendingStatusWrites: {}` at plan time, so this path is idle); `setStatus` throws (any call would be a bug in the proof); `fetchCiStatusBatch → {}` (every PR skipped as "no status"); `applyEffects` records — the `probe` effects for active trees are captured, never executed; `saveState` no-op.

- [ ] **Step 3: Run before/after on both snapshots (cwd MUST be `packages/daemon`)**

```bash
cd -- "$LEGION_WORKSPACE/packages/daemon"
D=/tmp/legion14-live; AFTER="$LEGION_WORKSPACE/packages/daemon/src/daemon/resync.ts"
bun $D/run-resync.ts $D/resync-before.ts $D/state-2026-09-11T2304Z-v22.json $D/before-v22.json
bun $D/run-resync.ts "$AFTER"            $D/state-2026-09-11T2304Z-v22.json $D/after-v22.json
bun $D/run-resync.ts $D/resync-before.ts $D/state-current.json               $D/before-current.json
bun $D/run-resync.ts "$AFTER"            $D/state-current.json               $D/after-current.json
jq -c '{input, queuedRoots: [.queuedRoots[].key], queuedChildren, anomalies: [.anomalies[] | "\(.kind):\(.issue)"], probes: (.effectKinds | length), pendingStatusWrites}' \
  $D/before-v22.json $D/after-v22.json $D/before-current.json $D/after-current.json
ls -la "$LEGION_STATE_DIR"/state.json*     # MUST still be exactly the three files from Step 1
```

Expected (each run 15–60 s wall on this rig; each prints one `[legion] resync probed N active roots` line then `wrote …`):

| report | queuedRoots | anomalies | probes |
| :--- | :--- | :--- | :--- |
| `before-v22.json` | `LEGION-10`, `LEGION-11` | `zero-owner-tree:LEGION-10`, `zero-owner-tree:LEGION-11` | 1 |
| `after-v22.json` | `LEGION-10`, `LEGION-11` | **`[]`** | 1 |
| `before-current.json` | whatever the queue held at copy time (at plan time: none — `queuedChildren: [LEGION-22, LEGION-23]`) | at plan time `[]` | number of located, ready-confirmed active trees (12 at plan time) |
| `after-current.json` | same as before-current | `before-current.anomalies` minus every `zero-owner-tree:<key>` whose `<key>` is in `queuedRoots` — nothing else may differ | same as before-current |

Acceptance: `after-v22.anomalies == []` with `before-v22.anomalies` listing both queued roots proves the fix on real incident-window state; `probes` unchanged before/after proves `probeActiveRoots` is untouched; the current-snapshot pair is the no-regression control — if its queue holds no roots at run time, say so explicitly in `test.json` rather than presenting `[] → []` as the proof.

- [ ] **Step 4: Negative control — the fix does not over-suppress on real-shaped data**

Take the incident snapshot copy and flip one queued root's tree to `dead` (the only edit), then run the fixed module:

```bash
cd -- "$LEGION_WORKSPACE/packages/daemon"; D=/tmp/legion14-live
jq '.trees["LEGION-10"].status = "dead"' $D/state-2026-09-11T2304Z-v22.json > $D/state-v22-dead-LEGION-10.json
bun $D/run-resync.ts "$LEGION_WORKSPACE/packages/daemon/src/daemon/resync.ts" $D/state-v22-dead-LEGION-10.json $D/after-v22-dead.json
jq -c '[.anomalies[] | "\(.kind):\(.issue)"]' $D/after-v22-dead.json
```
Expected: `["zero-owner-tree:LEGION-10"]` — LEGION-10 (dead tree, still `todo`, still in `admission.queue`) is reported; LEGION-11 (queued) is not. This is spec § Acceptance 2 and § Errors on real data, and it shows queue membership does not rescue a dead tree.

Second negative control, for the safety rail: `bun $D/run-resync.ts "$AFTER" "$LEGION_STATE_DIR/state.json" $D/x.json` → exits 1 with `refusing to load …: state input must be a copy under /tmp`, and `ls "$LEGION_STATE_DIR"/state.json*` is unchanged.

- [ ] **Step 5: Run the repo gates the implementer did not**

```bash
cd -- "$LEGION_WORKSPACE" && env -u DISPATCH_URL -u DISPATCH_TOKEN_FILE bun test
cd -- "$LEGION_WORKSPACE/packages/daemon" && bunx tsc --noEmit && bunx biome check src/
```
Known items only (see Task 1 Step 5). Confirm `pr-checks-result` succeeded at the PR head via `legion gh -- run list --repo sjawhar/legion --branch legion/LEGION-14 --limit 3`.

- [ ] **Step 6: Record the evidence**

Fill the PR body's `E2E` section (one line per report: input snapshot's timestamp, `queuedRoots`, before → after anomaly lists, head sha; plus the negative control), write `.legion/test.json` (`verdict`, `headSha`, the five report summaries, the `ls "$LEGION_STATE_DIR"/state.json*` before/after listing, and the composition of the current queue at copy time), then the skill's handoff commit and push:

```bash
cd -- "$LEGION_WORKSPACE" && jj -R "$LEGION_WORKSPACE" split -m "test: record handoff" .legion/test.json && \
  jj -R "$LEGION_WORKSPACE" bookmark set legion/LEGION-14 -r @- && \
  jj -R "$LEGION_WORKSPACE" git push --bookmark legion/LEGION-14
```

Leave `/tmp/legion14-live/` in place for the reviewer to re-run.

---

## Self-review against the spec

- § Acceptance 1 → Task 1 test 1; Task 2 `before-v22`/`after-v22`.
- § Acceptance 2 → Task 1 tests 2a (`dead`) and 2b (no tree, in queue); Task 2 Step 4 negative control. `closed` is not separately tested: it takes the same `else` path as `dead` (any status other than the three skipped ones), and a same-path parameter row would not defend anything 2a does not.
- § Acceptance 3 → Task 1 test 3.
- § Design: gate reads `trees[key].status ∈ {active, queued, launch-failed}`; `admission.*` not consulted; `hasActiveTree` not widened (deleted — see Global Constraints for why, and the handoff for the architect); launch-failed loop and controller untouched.
- § Errors rows 1–2 → tests 2a/2b (row 1 is 2a's shape with no tree and no queue entry — covered by 2b's assertion minus the queue; the anomaly is produced by the same `else` path).
- § Testing rows 1–3 → Task 1 tests 1, 2a, 3. Row "1 (live)" → Task 2, with the incident-window `state.json.v22.bak` as the input that actually exhibits the bug, because the current `state.json` queue held only children at plan time.
- § Rejected: no controller change; no `hasActiveTree` widening; no `admission.queue` gating; no daemon restart — the driver never touches the daemon, and every `$LEGION_STATE_DIR` read is a copy.
