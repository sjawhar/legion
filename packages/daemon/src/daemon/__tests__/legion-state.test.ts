import { afterEach, describe, expect, it } from "bun:test";
import { chmod, mkdtemp, readdir, readFile, rm, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { controllerToken, formatIssueKey, roleToken } from "@legion/contracts";
import { loadState, newLegionState, pruneStalePrTombstones, saveState } from "../legion-state";

const issue = formatIssueKey("sjawhar", "legion", 42);
const initialState = { project: "omp", cap: 4 };
const prKey = "sjawhar/legion#7";

function stateWithTree() {
  const state = newLegionState(initialState.project, initialState.cap);
  state.issues[issue] = {
    key: issue,
    title: "Implement Legion state",
    state: "open",
    children: [],
    released: true,
    labels: ["human-approved"],
  };
  state.trees[issue] = {
    root: issue,
    generation: 3,
    locator: {
      tmuxSession: "legion-omp-project",
      tmuxWindowId: "@42",
      ompSessionFile: "/tmp/session.json",
    },
    status: "launch-failed",
    launchFailures: 3,
  };
  state.roles[roleToken(initialState.project, issue, "implementer")] = {
    issue,
    role: "implementer",
    sessionId: "ses_123",
    agentId: "agt_implementer",
  };
  state.roles[controllerToken(initialState.project)] = {
    role: "controller",
    sessionId: "ses_controller",
  };
  state.prs[prKey] = {
    key: issue,
    repo: "sjawhar/legion",
    number: 7,
    headSha: "abc123",
    verdict: "green",
    failing: [],
    failingStatuses: [],
    ciSettledAt: 1_724_457_600_000,
    ciCheckRuns: null,
    ciSettlementGeneration: null,
    ciSnapshot: null,
    ciReconciled: false,
    fixAttempts: 1,
    reviewDecision: "approved",
  };
  state.prByBranch["sjawhar/legion@legion-42"] = "sjawhar/legion#7";
  state.admission.active.push(issue);
  state.phases[issue] = { phase: "implement", sessionId: "ses_123" };
  state.controllerCapabilityHash = "f".repeat(64);
  return state;
}

function legacyV8State(pr: Record<string, unknown>) {
  const current = stateWithTree();
  const {
    verdict: _verdict,
    failing: _failing,
    failingStatuses: _failingStatuses,
    ciSettledAt: _ciSettledAt,
    ciCheckRuns: _ciCheckRuns,
    ciSettlementGeneration: _ciSettlementGeneration,
    ciSnapshot: _ciSnapshot,
    ciReconciled: _ciReconciled,
    ...legacyPr
  } = current.prs[prKey];
  const legacy = {
    ...current,
    version: 8,
    prs: { ...current.prs, [prKey]: { ...legacyPr, ...pr } },
  };
  // The v12 -> v13 migration drops a legacy worker's session identity when it has no
  // resumable locator; `legacy.roles` (spread from `current` above) keeps the original
  // full-identity object, but the post-migration expectation must reflect the cutover.
  const implementerToken = roleToken(initialState.project, issue, "implementer");
  current.roles = { ...current.roles, [implementerToken]: { issue, role: "implementer" } };
  return { current, legacy };
}

describe("legion state", () => {
  let tempDir: string | undefined;

  afterEach(async () => {
    if (tempDir) {
      await chmod(tempDir, 0o700);
      await rm(tempDir, { recursive: true, force: true });
      tempDir = undefined;
    }
  });

  it("initializes empty v18 state with a valid project and admission capacity", () => {
    expect(newLegionState(initialState.project, initialState.cap)).toEqual({
      version: 18,
      project: "omp",
      issues: {},
      trees: {},
      roles: {},
      spawnCapabilities: {},
      prs: {},
      prByBranch: {},
      prTombstones: {},
      admission: { cap: 4, active: [], queue: [] },
      workerAdmission: { queue: [] },
      phases: {},
      controllerPendingNotices: [],
    });
  });

  it("drops PR tombstones older than 30 days but keeps ones within the window", () => {
    const now = Date.parse("2026-09-07T00:00:00.000Z");
    const THIRTY_DAYS_MS = 30 * 24 * 60 * 60 * 1000;
    const state = newLegionState("omp", 4);
    state.prTombstones = {
      "acme/widgets#7": now - THIRTY_DAYS_MS - 1,
      "acme/widgets#8": now - THIRTY_DAYS_MS,
      "acme/widgets#9": now - 1_000,
    };

    pruneStalePrTombstones(state, now);

    expect(state.prTombstones).toEqual({
      "acme/widgets#8": now - THIRTY_DAYS_MS,
      "acme/widgets#9": now - 1_000,
    });
  });

  it("prunes stale PR tombstones on load", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-tombstone-"));
    const file = path.join(tempDir, "state.json");
    const state = newLegionState("omp", 4);
    const THIRTY_DAYS_MS = 30 * 24 * 60 * 60 * 1000;
    state.prTombstones = {
      "acme/widgets#7": Date.now() - THIRTY_DAYS_MS - 60_000,
      "acme/widgets#8": Date.now(),
    };

    await saveState(file, state);
    const loaded = await loadState(file, initialState);

    expect(loaded.prTombstones).toEqual({ "acme/widgets#8": state.prTombstones["acme/widgets#8"] });
  });

  it("persists and reloads state without losing tree, role, or PR data", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-v2-"));
    const file = path.join(tempDir, "state.json");
    const state = stateWithTree();

    await saveState(file, state);

    expect(await loadState(file, initialState)).toEqual(state);
  });

  it("persists and reloads an attempt set whose check names collide with Object.prototype", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-proto-"));
    const file = path.join(tempDir, "state.json");
    const state = stateWithTree();
    const pr = state.prs[prKey];
    if (!pr) throw new Error("fixture PR missing");
    pr.ciCheckRuns = [
      { name: "__proto__", id: 100 },
      { name: "constructor", id: 200 },
      { name: "toString", id: 300 },
    ];

    await saveState(file, state);

    expect((await loadState(file, initialState)).prs[prKey]?.ciCheckRuns).toEqual(pr.ciCheckRuns);
  });

  it("refuses a persisted attempt set that is unsorted or repeats a name", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-set-"));
    const file = path.join(tempDir, "state.json");
    const state = stateWithTree();
    const pr = state.prs[prKey];
    if (!pr) throw new Error("fixture PR missing");
    for (const runs of [
      [
        { name: "lint", id: 900 },
        { name: "build", id: 200 },
      ],
      [
        { name: "build", id: 200 },
        { name: "build", id: 100 },
      ],
    ]) {
      pr.ciCheckRuns = runs;
      await saveState(file, state);
      await expect(loadState(file, initialState)).rejects.toThrow(
        /sorted by name without duplicates/
      );
    }
  });

  it("keeps the original v8 state bytes in a rollback backup", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-v8-backup-"));
    const file = path.join(tempDir, "state.json");
    const { legacy } = legacyV8State({
      firstRedEmitted: true,
      settledRedEmitted: false,
      greenEmitted: false,
      lastEventAt: 1_724_457_600_000,
    });
    const original = JSON.stringify(legacy, null, 2);
    await writeFile(file, original, "utf8");

    await loadState(file, initialState);

    const backup = `${file}.v8.bak`;
    expect(await readFile(backup, "utf8")).toBe(original);

    const laterLegacy = {
      ...legacy,
      trees: {
        ...legacy.trees,
        [issue]: { ...legacy.trees[issue], launchFailures: 4 },
      },
    };
    await writeFile(file, JSON.stringify(laterLegacy, null, 2), "utf8");
    await loadState(file, initialState);

    expect(await readFile(backup, "utf8")).toBe(original);
  });

  it("migrates v5 name-only locators by clearing their unsafe identities", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-v5-"));
    const file = path.join(tempDir, "state.json");
    // A v5 file carries the same legacy CI flags a v8 file does.
    const { current, legacy: v8 } = legacyV8State({
      greenEmitted: true,
      lastEventAt: 1_724_457_600_000,
    });
    const legacy = {
      ...v8,
      version: 5,
      trees: {
        ...current.trees,
        [issue]: {
          ...current.trees[issue],
          locator: {
            tmuxSession: "legion-omp-project",
            tmuxWindow: "sjawhar-legion-42",
          },
        },
      },
    };
    await writeFile(file, JSON.stringify(legacy), "utf8");

    const migrated = await loadState(file, initialState);
    const expected = stateWithTree();
    delete expected.trees[issue].locator;
    const implementerToken = roleToken(initialState.project, issue, "implementer");
    expected.roles[implementerToken] = { issue, role: "implementer" };

    expect(migrated).toEqual(expected);
  });

  it("migrates v6 state by dropping attribution and locator pids", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-v6-"));
    const file = path.join(tempDir, "state.json");
    // A v6 file carries the same legacy CI flags a v8 file does.
    const { current, legacy: v8 } = legacyV8State({
      greenEmitted: true,
      lastEventAt: 1_724_457_600_000,
    });
    const legacy = {
      ...v8,
      version: 6,
      attribution: [{ sha: "abc123", sessionId: "ses_123", issue, phase: "implement" }],
      trees: {
        ...current.trees,
        [issue]: {
          ...current.trees[issue],
          locator: { ...current.trees[issue]?.locator, pid: 1234 },
        },
      },
    };
    await writeFile(file, JSON.stringify(legacy), "utf8");

    const migrated = await loadState(file, initialState);

    expect(migrated).toEqual(current);
  });
  it("migrates v7 state by removing adopted dispatch threads", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-v7-"));
    const file = path.join(tempDir, "state.json");
    const dispatchThread = formatIssueKey("sjawhar", "legion", 99);
    const retainedChild = formatIssueKey("sjawhar", "legion", 100);
    const legacy = stateWithTree();
    legacy.issues[issue].children = [dispatchThread, retainedChild];
    legacy.issues[dispatchThread] = {
      key: dispatchThread,
      title: "Dispatch question",
      state: "open",
      parent: issue,
      children: [],
      released: false,
      labels: [],
    };
    legacy.issues[retainedChild] = {
      key: retainedChild,
      title: "Retained child",
      state: "open",
      parent: issue,
      children: [],
      released: false,
      labels: [],
    };
    const v7State = {
      ...legacy,
      // A v7 file carries the same legacy CI flags a v8 file does.
      prs: legacyV8State({ greenEmitted: true, lastEventAt: 1_724_457_600_000 }).legacy.prs,
      version: 7,
      dispatchThreads: [
        {
          repo: "sjawhar/legion",
          thread: 99,
          role: "architect",
          issue,
          tree: issue,
        },
      ],
    };
    await writeFile(file, JSON.stringify(v7State), "utf8");

    const expected = stateWithTree();
    expected.issues[issue].children = [retainedChild];
    expected.issues[retainedChild] = legacy.issues[retainedChild];
    const implementerToken = roleToken(initialState.project, issue, "implementer");
    expected.roles[implementerToken] = { issue, role: "implementer" };

    expect(await loadState(file, initialState)).toEqual(expected);
  });

  it("migrates v8 legacy red flags with a later green emission to green", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-v8-"));
    const file = path.join(tempDir, "state.json");
    const { current, legacy } = legacyV8State({
      firstRedEmitted: true,
      settledRedEmitted: true,
      greenEmitted: true,
      lastEventAt: 1_724_457_600_000,
    });
    await writeFile(file, JSON.stringify(legacy), "utf8");

    expect(await loadState(file, initialState)).toEqual(current);
  });

  it("migrates v8 legacy red flags without a green emission to red", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-v8-"));
    const file = path.join(tempDir, "state.json");
    const { current, legacy } = legacyV8State({
      firstRedEmitted: true,
      settledRedEmitted: true,
      greenEmitted: false,
      lastEventAt: 1_724_457_600_000,
    });
    current.prs[prKey] = { ...current.prs[prKey], verdict: "red" };
    await writeFile(file, JSON.stringify(legacy), "utf8");

    expect(await loadState(file, initialState)).toEqual(current);
  });

  it("migrates v8 terminal failing checks to red over legacy green flags", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-v8-"));
    const file = path.join(tempDir, "state.json");
    const { current, legacy } = legacyV8State({
      checks: {
        lint: { status: "completed", conclusion: "failure" },
        unit: { status: "completed", conclusion: "success" },
      },
      firstRedEmitted: false,
      settledRedEmitted: false,
      greenEmitted: true,
      lastEventAt: 1_724_457_600_000,
    });
    current.prs[prKey] = { ...current.prs[prKey], verdict: "red" };
    await writeFile(file, JSON.stringify(legacy), "utf8");

    expect(await loadState(file, initialState)).toEqual(current);
  });

  it("migrates v8 terminal non-success checks to red", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-v8-"));
    const file = path.join(tempDir, "state.json");
    const { current, legacy } = legacyV8State({
      checks: {
        integration: { status: "completed", conclusion: "cancelled" },
        unit: { status: "completed", conclusion: "success" },
      },
      firstRedEmitted: false,
      settledRedEmitted: false,
      greenEmitted: true,
      lastEventAt: 1_724_457_600_000,
    });
    current.prs[prKey] = { ...current.prs[prKey], verdict: "red" };
    await writeFile(file, JSON.stringify(legacy), "utf8");

    expect(await loadState(file, initialState)).toEqual(current);
  });

  it("migrates v12 state to v13 (a version bump only, no field changes)", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-v12-"));
    const file = path.join(tempDir, "state.json");
    const current = stateWithTree();
    // A locator keeps the v13 -> v14 worker-identity migration (tested below) from touching
    // this claim, so the only observable change through the full chain is the version bump
    // this test names.
    const implementerToken = roleToken(initialState.project, issue, "implementer");
    current.roles = {
      ...current.roles,
      [implementerToken]: {
        ...current.roles[implementerToken],
        locator: {
          tmuxSession: "legion-omp-project",
          tmuxWindowId: "@42",
          tmuxPaneId: "%1",
          socketPath: "/tmp/implementer.sock",
        },
      },
    };
    const v12State = { ...current, version: 12 };
    await writeFile(file, JSON.stringify(v12State), "utf8");

    expect(await loadState(file, initialState)).toEqual(current);
  });

  it("migrates v14 state to v15 (a version bump only, no field changes)", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-v14-"));
    const file = path.join(tempDir, "state.json");
    const current = stateWithTree();
    const v14State = { ...current, version: 14 };
    await writeFile(file, JSON.stringify(v14State), "utf8");

    expect(await loadState(file, initialState)).toEqual(current);
  });

  it("migrates v12 state by dropping a legacy worker's session identity when it has no resumable locator", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-v12-"));
    const file = path.join(tempDir, "state.json");
    const current = stateWithTree();
    await writeFile(file, JSON.stringify({ ...current, version: 12 }), "utf8");

    const migrated = await loadState(file, initialState);

    const token = roleToken(initialState.project, issue, "implementer");
    expect(migrated).toEqual({
      ...current,
      roles: { ...current.roles, [token]: { issue, role: "implementer" } },
    });
  });

  it("keeps a v12 worker's session identity intact when a resumable locator is already present", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-v12-locator-"));
    const file = path.join(tempDir, "state.json");
    const current = stateWithTree();
    const token = roleToken(initialState.project, issue, "implementer");
    current.roles[token] = {
      issue,
      role: "implementer",
      sessionId: "ses_123",
      agentId: "agt_implementer",
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%1",
        socketPath: "/state/workers/implementer.sock",
        ompSessionFile: "/tmp/implementer-session.json",
      },
    };
    await writeFile(file, JSON.stringify({ ...current, version: 12 }), "utf8");

    expect(await loadState(file, initialState)).toEqual(current);
  });

  it("migrates v15 state by adding an empty running-worker queue", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-v15-"));
    const file = path.join(tempDir, "state.json");
    const current = stateWithTree();
    const { workerAdmission: _workerAdmission, ...withoutWorkerAdmission } = current;
    await writeFile(file, JSON.stringify({ ...withoutWorkerAdmission, version: 15 }), "utf8");

    expect(await loadState(file, initialState)).toEqual(current);
  });

  it("migrates v16 state to v18: drops the tree-scoped held-event plumbing (heldEvents, recoveryEvents) but converts controllerHeldEvents into controllerPendingNotices, preserving workerAdmission and phases", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-v16-"));
    const file = path.join(tempDir, "state.json");
    const current = stateWithTree();
    current.workerAdmission = { queue: [roleToken(initialState.project, issue, "implementer")] };
    current.phases[issue] = {
      phase: "implementer",
      sessionId: "ses_123",
      completed: { summary: "implemented the thing", at: "2026-09-01T00:00:00.000Z" },
    };
    // A controller-bound event has no other source of truth to recover it from, so migrating a
    // v16 `controllerHeldEvents` entry must convert it into `controllerPendingNotices` rather
    // than drop it like every other held-event field.
    current.controllerPendingNotices = [
      { payloadJson: '{"text":"@legion please investigate"}', eventId: "evt-controller" },
    ];
    const { controllerPendingNotices: _omitNotices, ...currentWithoutNotices } = current;
    const v16Input = {
      ...currentWithoutNotices,
      version: 16,
      controllerHeldEvents: [
        {
          role: "controller",
          payloadJson: '{"text":"@legion please investigate"}',
          heldAt: "2026-09-01T00:00:00.000Z",
          eventId: "evt-controller",
        },
      ],
      trees: {
        ...current.trees,
        [issue]: {
          ...current.trees[issue],
          heldEvents: [
            {
              role: "architect",
              payloadJson: "{}",
              heldAt: "2026-09-01T00:00:00.000Z",
              eventId: "evt-architect",
            },
          ],
          recoveryEvents: [
            {
              issue,
              role: "implementer",
              original: { topic: "s.implementer", payload: "{}", eventId: "evt-implementer" },
            },
          ],
        },
      },
    };
    await writeFile(file, JSON.stringify(v16Input), "utf8");

    const loaded = await loadState(file, initialState);

    expect(loaded).toEqual(current);
    // The held-event plumbing being dropped (or, for the controller, converted) is orthogonal
    // to a queued admission retry and a completed-but-unrouted phase, both of which pre-date
    // v16->v17 and must survive it intact.
    expect(loaded.workerAdmission).toEqual({
      queue: [roleToken(initialState.project, issue, "implementer")],
    });
    expect(loaded.phases[issue]).toEqual({
      phase: "implementer",
      sessionId: "ses_123",
      completed: { summary: "implemented the thing", at: "2026-09-01T00:00:00.000Z" },
    });
  });

  it("migrates v17 state to v18 by adding an empty controllerPendingNotices array when there is no carried-forward controllerHeldEvents entry to convert", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-v17-"));
    const file = path.join(tempDir, "state.json");
    const current = stateWithTree();
    const { controllerPendingNotices: _omit, ...currentWithoutNotices } = current;
    const v17Input = { ...currentWithoutNotices, version: 17 };
    await writeFile(file, JSON.stringify(v17Input), "utf8");

    const loaded = await loadState(file, initialState);

    expect(loaded).toEqual(current);
    expect(loaded.controllerPendingNotices).toEqual([]);
  });

  it("discards a malformed v16 controllerHeldEvents entry during migration instead of carrying garbage into controllerPendingNotices", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-v16-malformed-"));
    const file = path.join(tempDir, "state.json");
    const current = stateWithTree();
    const { controllerPendingNotices: _omit, ...currentWithoutNotices } = current;
    const v16Input = {
      ...currentWithoutNotices,
      version: 16,
      controllerHeldEvents: [
        // Missing `payloadJson` entirely -- not a shape this migration can convert.
        { role: "controller", heldAt: "2026-09-01T00:00:00.000Z", eventId: "evt-malformed" },
      ],
    };
    await writeFile(file, JSON.stringify(v16Input), "utf8");

    const loaded = await loadState(file, initialState);

    expect(loaded.controllerPendingNotices).toEqual([]);
  });

  it("rejects removed v6 fields on current-version state", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-current-"));
    const file = path.join(tempDir, "state.json");
    const current = stateWithTree();
    const malformedStates = [
      {
        ...current,
        attribution: [{ sha: "abc123", sessionId: "ses_123", issue, phase: "implement" }],
      },
      {
        ...current,
        trees: {
          ...current.trees,
          [issue]: {
            ...current.trees[issue],
            locator: { ...current.trees[issue]?.locator, pid: 1234 },
          },
        },
      },
    ];

    for (const malformed of malformedStates) {
      await writeFile(file, JSON.stringify(malformed), "utf8");
      await expect(loadState(file, initialState)).rejects.toThrow("Invalid Legion state");
    }
  });

  it("creates the supplied initial state when the file is absent", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-v4-"));
    const init = { project: "other", cap: 2 };

    expect(await loadState(path.join(tempDir, "missing.json"), init)).toEqual(
      newLegionState(init.project, init.cap)
    );
  });

  it("rejects invalid project configuration when initializing or loading state", async () => {
    expect(() => newLegionState("OMP.Project", 4)).toThrow("Invalid Legion project token");

    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-v2-"));
    const file = path.join(tempDir, "state.json");
    const state = stateWithTree();
    state.project = "omp-project";
    await saveState(file, state);

    await expect(loadState(file, initialState)).rejects.toThrow("Invalid Legion state");
  });

  it("keeps the previous state and leaves no temp file after a write error", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-v2-"));
    const file = path.join(tempDir, "state.json");
    const original = stateWithTree();
    const replacement = newLegionState("replacement", 1);
    await saveState(file, original);

    await chmod(tempDir, 0o500);
    try {
      await expect(saveState(file, replacement)).rejects.toThrow();
    } finally {
      await chmod(tempDir, 0o700);
    }

    expect(await loadState(file, initialState)).toEqual(original);
    expect((await readdir(tempDir)).filter((entry) => entry.includes(".tmp-"))).toEqual([]);
  });

  it("rejects prior v4 state rather than silently migrating spawn capabilities", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-v5-"));
    const file = path.join(tempDir, "state.json");
    await writeFile(file, JSON.stringify({ version: 4 }), "utf8");

    await expect(loadState(file, initialState)).rejects.toThrow(
      "Unsupported Legion state version: 4"
    );
  });
  it("rejects malformed current-version state instead of accepting a partial object", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-v5-"));
    const file = path.join(tempDir, "state.json");
    await writeFile(file, JSON.stringify({ version: 5 }), "utf8");

    await expect(loadState(file, initialState)).rejects.toThrow("Invalid Legion state");
  });
});
