import { afterEach, describe, expect, it, vi } from "bun:test";
import { chmod, mkdtemp, readdir, readFile, rm, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { controllerToken, roleToken } from "@legion/contracts";
import { loadState, newLegionState, pruneStalePrTombstones, saveState } from "../legion-state";

const issue = "LEGION-42";
const initialState = { project: "omp", cap: 4 };
const prKey = "sjawhar/legion#7";

function stateWithTree() {
  const state = newLegionState(initialState.project, initialState.cap);
  state.issues[issue] = {
    key: issue,
    title: "Implement Legion state",
    children: [],
    status: "in_progress",
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
  // The v18 -> v19 cutover refuses to migrate a state with any active tree or issue, so every
  // fixture that runs the full migration chain (all callers of this helper) must start empty.
  // The CI-verdict/backup behavior under test lives entirely in `prs`.
  current.trees = {};
  current.issues = {};
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

  it("initializes empty v20 state with a valid project and admission capacity", () => {
    expect(newLegionState(initialState.project, initialState.cap)).toEqual({
      version: 20,
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
      gates: {},
      pendingStatusWrites: {},
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
      prs: {
        ...legacy.prs,
        [prKey]: { ...legacy.prs[prKey], fixAttempts: 4 },
      },
    };
    await writeFile(file, JSON.stringify(laterLegacy, null, 2), "utf8");
    await loadState(file, initialState);

    expect(await readFile(backup, "utf8")).toBe(original);
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

  it("migrates v12 state to v13 (a version bump only, no field changes) and on to v20's readyConfirmedAt backfill", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-v12-"));
    const file = path.join(tempDir, "state.json");
    const current = stateWithTree();
    // The v17 -> v18 cutover refuses to migrate a state with any active tree or issue; this
    // test's subject (the v12 -> v13 version bump and locator-preserving role identity) lives
    // entirely in `roles`.
    current.trees = {};
    current.issues = {};
    // A locator keeps the v13 -> v14 worker-identity migration from touching this claim; with
    // `sessionId` also present, the same claim now also matches the v19 -> v20 backfill's own
    // pre-upgrade "confirmed" criteria, so `readyConfirmedAt` is the one other observable
    // change through the full chain this test names.
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

    const migrationTimestamp = 1_726_000_000_000;
    const dateNowSpy = vi.spyOn(Date, "now").mockReturnValue(migrationTimestamp);
    try {
      expect(await loadState(file, initialState)).toEqual({
        ...current,
        roles: {
          ...current.roles,
          [implementerToken]: {
            ...current.roles[implementerToken],
            readyConfirmedAt: migrationTimestamp,
          },
        },
      });
    } finally {
      dateNowSpy.mockRestore();
    }
  });

  it("migrates v14 state to v15 (a version bump only, no field changes)", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-v14-"));
    const file = path.join(tempDir, "state.json");
    const current = stateWithTree();
    current.trees = {};
    current.issues = {};
    const v14State = { ...current, version: 14 };
    await writeFile(file, JSON.stringify(v14State), "utf8");

    expect(await loadState(file, initialState)).toEqual(current);
  });

  it("migrates v12 state by dropping a legacy worker's session identity when it has no resumable locator", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-v12-"));
    const file = path.join(tempDir, "state.json");
    const current = stateWithTree();
    current.trees = {};
    current.issues = {};
    await writeFile(file, JSON.stringify({ ...current, version: 12 }), "utf8");

    const migrated = await loadState(file, initialState);

    const token = roleToken(initialState.project, issue, "implementer");
    expect(migrated).toEqual({
      ...current,
      roles: { ...current.roles, [token]: { issue, role: "implementer" } },
    });
  });

  it("keeps a v12 worker's session identity intact when a resumable locator is already present, backfilling readyConfirmedAt for it as an already-confirmed pre-v20 claim", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-v12-locator-"));
    const file = path.join(tempDir, "state.json");
    const current = stateWithTree();
    current.trees = {};
    current.issues = {};
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

    const migrationTimestamp = 1_726_000_000_000;
    const dateNowSpy = vi.spyOn(Date, "now").mockReturnValue(migrationTimestamp);
    try {
      expect(await loadState(file, initialState)).toEqual({
        ...current,
        roles: {
          ...current.roles,
          [token]: { ...current.roles[token], readyConfirmedAt: migrationTimestamp },
        },
      });
    } finally {
      dateNowSpy.mockRestore();
    }
  });

  it("migrates v15 state by adding an empty running-worker queue", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-v15-"));
    const file = path.join(tempDir, "state.json");
    const current = stateWithTree();
    current.trees = {};
    current.issues = {};
    const { workerAdmission: _workerAdmission, ...withoutWorkerAdmission } = current;
    await writeFile(file, JSON.stringify({ ...withoutWorkerAdmission, version: 15 }), "utf8");

    expect(await loadState(file, initialState)).toEqual(current);
  });

  it("migrates v16 controller notices through the Dispatch lifecycle", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-v16-"));
    const file = path.join(tempDir, "state.json");
    const current = newLegionState(initialState.project, initialState.cap);
    current.workerAdmission = { queue: [roleToken(initialState.project, issue, "implementer")] };
    current.phases[issue] = {
      phase: "implementer",
      sessionId: "ses_123",
      completed: { summary: "implemented the thing", at: "2026-09-01T00:00:00.000Z" },
    };
    const notice = {
      payloadJson: '{"text":"@legion please investigate"}',
      eventId: "evt-controller",
    };
    current.controllerPendingNotices = [notice];
    const {
      controllerPendingNotices: _notices,
      gates: _gates,
      pendingStatusWrites: _pendingStatusWrites,
      ...v16Fields
    } = current;
    await writeFile(
      file,
      JSON.stringify({
        ...v16Fields,
        version: 16,
        controllerHeldEvents: [
          {
            role: "controller",
            payloadJson: notice.payloadJson,
            heldAt: "2026-09-01T00:00:00.000Z",
            eventId: notice.eventId,
          },
        ],
      }),
      "utf8"
    );

    expect(await loadState(file, initialState)).toEqual(current);
  });

  it("migrates a controller-held-events-free v17 state through v18, v19, and v20", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-v17-chain-"));
    const file = path.join(tempDir, "state.json");
    const current = newLegionState(initialState.project, initialState.cap);
    const {
      controllerPendingNotices: _notices,
      gates: _gates,
      pendingStatusWrites: _pendingStatusWrites,
      ...v17State
    } = current;
    await writeFile(file, JSON.stringify({ ...v17State, version: 17 }), "utf8");

    const migrated = await loadState(file, initialState);

    expect(migrated.version).toBe(20);
    expect(migrated.controllerPendingNotices).toEqual([]);
    expect(migrated.gates).toEqual({});
  });

  it("drops malformed v16 controller notices instead of carrying them into v19", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-v16-malformed-"));
    const file = path.join(tempDir, "state.json");
    const current = newLegionState(initialState.project, initialState.cap);
    const {
      controllerPendingNotices: _notices,
      gates: _gates,
      pendingStatusWrites: _pendingStatusWrites,
      ...v16Fields
    } = current;
    await writeFile(
      file,
      JSON.stringify({
        ...v16Fields,
        version: 16,
        controllerHeldEvents: [{ role: "controller", eventId: "evt-malformed" }],
      }),
      "utf8"
    );
    const errorLog = vi.spyOn(console, "error").mockImplementation(() => {});

    try {
      expect(await loadState(file, initialState)).toEqual(current);
      expect(errorLog).toHaveBeenCalledWith(expect.stringContaining("evt-malformed"));
    } finally {
      errorLog.mockRestore();
    }
  });

  it("merges and dedupes migrated controller notices by event id", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-v16-dedupe-"));
    const file = path.join(tempDir, "state.json");
    const current = newLegionState(initialState.project, initialState.cap);
    const existing = { payloadJson: '{"text":"already queued"}', eventId: "evt-existing" };
    const duplicate = { payloadJson: '{"text":"first redelivery"}', eventId: "evt-shared" };
    const {
      controllerPendingNotices: _notices,
      gates: _gates,
      pendingStatusWrites: _pendingStatusWrites,
      ...v16Fields
    } = current;
    current.controllerPendingNotices = [existing, duplicate];
    await writeFile(
      file,
      JSON.stringify({
        ...v16Fields,
        version: 16,
        controllerPendingNotices: [existing],
        controllerHeldEvents: [
          {
            role: "controller",
            payloadJson: duplicate.payloadJson,
            heldAt: "2026-09-01T00:00:00.000Z",
            eventId: duplicate.eventId,
          },
          {
            role: "controller",
            payloadJson: '{"text":"duplicate redelivery"}',
            heldAt: "2026-09-01T00:05:00.000Z",
            eventId: duplicate.eventId,
          },
        ],
      }),
      "utf8"
    );

    expect(await loadState(file, initialState)).toEqual(current);
  });

  it("refuses a v18 state with active trees or issues during Dispatch migration", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-v18-active-"));
    const file = path.join(tempDir, "state.json");

    for (const activeState of [
      { version: 18, trees: { "LEGION-42": {} }, issues: {} },
      { version: 18, trees: {}, issues: { "LEGION-42": {} } },
    ]) {
      await writeFile(file, JSON.stringify(activeState), "utf8");
      await expect(loadState(file, initialState)).rejects.toThrow(
        "Cannot migrate a Legion state with active trees to the Dispatch lifecycle"
      );
    }
  });

  it("converts a tree-less, issue-less v18 state to v19 (and onward to v20), preserving its controller notices", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-v18-gates-"));
    const file = path.join(tempDir, "state.json");
    const notice = {
      payloadJson: '{"text":"@legion please investigate"}',
      eventId: "evt-controller",
    };
    const v18State = {
      version: 18,
      project: initialState.project,
      issues: {},
      trees: {},
      roles: {},
      spawnCapabilities: {},
      prs: {},
      prByBranch: {},
      prTombstones: {},
      admission: { cap: initialState.cap, active: [], queue: [] },
      workerAdmission: { queue: [] },
      phases: {},
      controllerPendingNotices: [notice],
      // A leftover pre-Dispatch design-gate ledger entry: the guard above must not refuse this
      // conversion just because `gates` is nonempty (it inspects only `trees`/`issues`).
      gates: { "sjawhar/legion#42": { designAskId: "ask-1" } },
    };
    await writeFile(file, JSON.stringify(v18State), "utf8");

    const migrated = await loadState(file, initialState);

    expect(migrated.version).toBe(20);
    expect(migrated.controllerPendingNotices).toEqual([notice]);
    expect(migrated.gates).toEqual({});
  });

  it("migrates v19 state to v20 by backfilling readyConfirmedAt for confirmed worker claims, leaving unconfirmed, already-confirmed, and controller claims untouched", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-v19-"));
    const file = path.join(tempDir, "state.json");
    const current = newLegionState(initialState.project, initialState.cap);
    const confirmedToken = roleToken(initialState.project, issue, "implementer");
    const unconfirmedToken = roleToken(initialState.project, issue, "tester");
    const alreadyConfirmedToken = roleToken(initialState.project, issue, "reviewer");
    const locator = {
      tmuxSession: "legion-omp",
      tmuxWindowId: "@42",
      tmuxPaneId: "%1",
      socketPath: "/state/workers/implementer.sock",
    };
    current.roles = {
      [confirmedToken]: {
        // sessionId + locator, no readyConfirmedAt: the pre-v20 meaning of "confirmed" this
        // migration backfills.
        issue,
        role: "implementer",
        sessionId: "ses_implementer",
        locator,
      },
      [unconfirmedToken]: {
        // No sessionId at all: /worker/started never registered this generation, so it was
        // never confirmed under either the old or new meaning and must stay untouched.
        issue,
        role: "tester",
        pendingAssignment: "verify #41",
      },
      [alreadyConfirmedToken]: {
        // Already carries readyConfirmedAt (persisted by an already-patched daemon): never
        // overwritten by this migration's own timestamp.
        issue,
        role: "reviewer",
        sessionId: "ses_reviewer",
        locator,
        readyConfirmedAt: 1_700_000_000_000,
      },
      [controllerToken(initialState.project)]: {
        role: "controller",
        sessionId: "ses_controller",
      },
    };
    await writeFile(file, JSON.stringify({ ...current, version: 19 }), "utf8");

    const migrationTimestamp = 1_726_000_000_000;
    const dateNowSpy = vi.spyOn(Date, "now").mockReturnValue(migrationTimestamp);
    try {
      const migrated = await loadState(file, initialState);

      expect(migrated.version).toBe(20);
      expect(migrated.roles[confirmedToken]).toEqual({
        ...current.roles[confirmedToken],
        readyConfirmedAt: migrationTimestamp,
      });
      expect(migrated.roles[unconfirmedToken]).toEqual(current.roles[unconfirmedToken]);
      expect(migrated.roles[alreadyConfirmedToken]).toEqual(current.roles[alreadyConfirmedToken]);
      expect(migrated.roles[controllerToken(initialState.project)]).toEqual(
        current.roles[controllerToken(initialState.project)]
      );
    } finally {
      dateNowSpy.mockRestore();
    }
  });

  it("accepts an issue's Dispatch status and design-gate entry on current state", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-status-"));
    const file = path.join(tempDir, "state.json");
    const current = stateWithTree();
    current.issues[issue] = { ...current.issues[issue], status: "in_progress" };
    current.gates[issue] = { designAskId: "ask-1", designApproved: "ask-1" };
    await saveState(file, current);

    expect(await loadState(file, initialState)).toEqual(current);
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
