import { afterEach, describe, expect, it, vi } from "bun:test";
import * as crypto from "node:crypto";
import { chmod, mkdtemp, readdir, readFile, rm, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { controllerToken, roleToken } from "@legion/contracts";
import {
  describeAdmissionDrift,
  type LegionState,
  loadState,
  newLegionState,
  owningArchitect,
  pruneStalePrTombstones,
  repairAdmissionDrift,
  samePendingTask,
  saveState,
  type TreeState,
} from "../legion-state";

const issue = "LEGION-42";
const initialState = { project: "omp", cap: 4 };
const prKey = "sjawhar/legion#7";
/** The `Date.now()` a migration test pins so a backfilled time (v19/v20's `readyConfirmedAt`,
 * v30 -> v31's `queuedAt`) is predictable; `MIGRATED_AT_ISO` is what v31 stamps from it. */
const MIGRATED_AT = 1_726_000_000_000;
const MIGRATED_AT_ISO = new Date(MIGRATED_AT).toISOString();
const TEST_DELIVERY_ID = "00000000-0000-4000-8000-000000000001";

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
      runtime: "tmux",
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
function pendingDeliveryId(state: LegionState, token: string): string | undefined {
  const claim = state.roles[token];
  if (!claim || !("issue" in claim)) throw new Error(`worker claim ${token} disappeared`);
  return claim.pendingAssignment?.deliveryId;
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

  it("initializes empty v31 state with a valid project and admission capacity", () => {
    expect(newLegionState(initialState.project, initialState.cap)).toEqual({
      version: 33,
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
      spawnRequests: {},
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
          runtime: "tmux",
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
        runtime: "tmux",
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

  it("migrates a controller-held-events-free v17 state through v18, v19, v20, v21, v22, v23, v24, v25, v26, v27, v28, v29, v30, and v31", async () => {
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

    expect(migrated.version).toBe(33);
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

  it("converts a tree-less, issue-less v18 state to v19 (and onward to v31), preserving its controller notices", async () => {
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

    expect(migrated.version).toBe(33);
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
      runtime: "tmux" as const,
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
        // never confirmed under either the old or new meaning and must stay untouched. Its
        // pending assignment is written to the v19 file as the bare string that era persisted;
        // the v25 -> v26 migration classifies it into this object form and v30 -> v31 stamps
        // the migration instant as its queuedAt.
        issue,
        role: "tester",
        pendingAssignment: {
          kind: "assignment",
          task: "verify #41",
          queuedAt: new Date(1_726_000_000_000).toISOString(),
          deliveryId: TEST_DELIVERY_ID,
        },
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
    const v19State = JSON.parse(JSON.stringify({ ...current, version: 19 }));
    v19State.roles[unconfirmedToken].pendingAssignment = "verify #41";
    await writeFile(file, JSON.stringify(v19State), "utf8");

    const migrationTimestamp = 1_726_000_000_000;
    const dateNowSpy = vi.spyOn(Date, "now").mockReturnValue(migrationTimestamp);
    const randomUuidSpy = vi.spyOn(crypto, "randomUUID").mockReturnValue(TEST_DELIVERY_ID);
    try {
      const migrated = await loadState(file, initialState);

      expect(migrated.version).toBe(33);
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
      randomUuidSpy.mockRestore();
    }
  });

  it("migrates v20 state to v21 by backfilling readyConfirmedAt for active root trees with a locator, leaving queued/no-locator and already-confirmed trees untouched", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-v20-"));
    const file = path.join(tempDir, "state.json");
    const current = newLegionState(initialState.project, initialState.cap);
    const confirmedIssue = "LEGION-1";
    const noLocatorIssue = "LEGION-2";
    const queuedIssue = "LEGION-3";
    const alreadyConfirmedIssue = "LEGION-4";
    const locator = { runtime: "tmux" as const, tmuxSession: "legion-omp", tmuxWindowId: "@42" };
    current.trees = {
      [confirmedIssue]: {
        // active + a recorded locator, no readyConfirmedAt: the pre-v21 meaning of "a root
        // actually launched and is presumably running fine" this migration backfills.
        root: confirmedIssue,
        generation: 1,
        locator,
        status: "active",
        launchFailures: 0,
      },
      [noLocatorIssue]: {
        // active but no locator at all: nothing worth protecting from a fresh deadline, left
        // untouched.
        root: noLocatorIssue,
        generation: 1,
        status: "active",
        launchFailures: 0,
      },
      [queuedIssue]: {
        // not active: untouched regardless of any locator.
        root: queuedIssue,
        generation: 0,
        locator,
        status: "queued",
        launchFailures: 0,
      },
      [alreadyConfirmedIssue]: {
        // Already carries readyConfirmedAt (persisted by an already-patched daemon): never
        // overwritten by this migration's own timestamp.
        root: alreadyConfirmedIssue,
        generation: 1,
        locator,
        status: "active",
        launchFailures: 0,
        readyConfirmedAt: 1_700_000_000_000,
      },
    };
    await writeFile(file, JSON.stringify({ ...current, version: 20 }), "utf8");

    const migrationTimestamp = 1_726_000_000_000;
    const dateNowSpy = vi.spyOn(Date, "now").mockReturnValue(migrationTimestamp);
    try {
      const migrated = await loadState(file, initialState);

      expect(migrated.version).toBe(33);
      expect(migrated.trees[confirmedIssue]).toEqual({
        ...current.trees[confirmedIssue],
        readyConfirmedAt: migrationTimestamp,
      });
      expect(migrated.trees[noLocatorIssue]).toEqual(current.trees[noLocatorIssue]);
      expect(migrated.trees[queuedIssue]).toEqual(current.trees[queuedIssue]);
      expect(migrated.trees[alreadyConfirmedIssue]).toEqual(current.trees[alreadyConfirmedIssue]);
    } finally {
      dateNowSpy.mockRestore();
    }
  });

  it("migrates v22 state past v23 by dropping the approvalStatusPending map it carried", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-v22-"));
    const file = path.join(tempDir, "state.json");
    const current = stateWithTree();
    current.trees = {};
    current.issues = {};
    await writeFile(
      file,
      JSON.stringify({
        ...current,
        version: 22,
        approvalStatusPending: {
          "acme/widgets#7": { sha: "head-1", attempts: 3, lastError: "HTTP 403" },
        },
      }),
      "utf8"
    );

    expect(await loadState(file, initialState)).toEqual(current);
  });

  it('migrates v23 state to v24 by tagging every persisted locator with runtime: "tmux"', async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-v23-"));
    const file = path.join(tempDir, "state.json");
    const current = stateWithTree();
    const implementerToken = roleToken(initialState.project, issue, "implementer");
    current.roles[implementerToken] = {
      ...current.roles[implementerToken],
      issue,
      role: "implementer",
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp-project",
        tmuxWindowId: "@42",
        tmuxPaneId: "%1",
        socketPath: "/state/workers/implementer.sock",
      },
    };
    current.controllerLocator = {
      runtime: "tmux",
      tmuxSession: "legion-omp-project",
      tmuxWindowId: "@7",
      tmuxPaneId: "%9",
    };
    // The v23 file: the same state, every locator predating the discriminant.
    const v23State = JSON.parse(JSON.stringify({ ...current, version: 23 }));
    delete v23State.trees[issue].locator.runtime;
    delete v23State.roles[implementerToken].locator.runtime;
    delete v23State.controllerLocator.runtime;
    v23State.controllerLocator.socketPath = "/state/workers/controller.sock";
    const raw = JSON.stringify(v23State);
    await writeFile(file, raw, "utf8");

    const migrated = await loadState(file, initialState);

    expect(migrated.version).toBe(33);
    expect(migrated.trees[issue]?.locator?.runtime).toBe("tmux");
    expect(migrated.controllerLocator?.runtime).toBe("tmux");
    const claim = migrated.roles[implementerToken];
    expect(claim && "issue" in claim ? claim.locator?.runtime : undefined).toBe("tmux");
    expect(migrated).toEqual(current);
    expect(await readFile(`${file}.v23.bak`, "utf8")).toBe(raw);
  });

  it("migrates v25 state to v27, classifying each persisted pendingAssignment string by its payload on the way through v26", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-v25-"));
    const file = path.join(tempDir, "state.json");
    const current = newLegionState(initialState.project, initialState.cap);
    const implementerToken = roleToken(initialState.project, issue, "implementer");
    const testerToken = roleToken(initialState.project, issue, "tester");
    const reviewerToken = roleToken(initialState.project, issue, "reviewer");
    const catchupTask = JSON.stringify({ type: "catchup-worker", unhandled: [] });
    current.roles = {
      [implementerToken]: {
        issue,
        role: "implementer",
        generation: 1,
        pendingAssignment: {
          kind: "assignment",
          task: "implement #43",
          queuedAt: MIGRATED_AT_ISO,
          deliveryId: TEST_DELIVERY_ID,
        },
      },
      [testerToken]: {
        issue,
        role: "tester",
        sessionId: "ses_tester",
        generation: 2,
        pendingAssignment: {
          kind: "catchup",
          task: catchupTask,
          queuedAt: MIGRATED_AT_ISO,
          deliveryId: TEST_DELIVERY_ID,
        },
      },
      [reviewerToken]: {
        issue,
        role: "reviewer",
        sessionId: "ses_reviewer",
        readyConfirmedAt: 1_700_000_000_000,
      },
      [controllerToken(initialState.project)]: {
        role: "controller",
        sessionId: "ses_controller",
      },
    };
    // The v25 file, exactly as a daemon on main's LEGION-33 bump persists it: the same state,
    // every pending assignment a bare task string.
    const v25State = JSON.parse(JSON.stringify({ ...current, version: 25 }));
    v25State.roles[implementerToken].pendingAssignment = "implement #43";
    v25State.roles[testerToken].pendingAssignment = catchupTask;
    const raw = JSON.stringify(v25State);
    await writeFile(file, raw, "utf8");

    const dateNowSpy = vi.spyOn(Date, "now").mockReturnValue(MIGRATED_AT);
    const randomUuidSpy = vi.spyOn(crypto, "randomUUID").mockReturnValue(TEST_DELIVERY_ID);
    try {
      const migrated = await loadState(file, initialState);

      expect(migrated.version).toBe(33);
      expect(migrated.roles[implementerToken]).toEqual(current.roles[implementerToken]);
      expect(migrated.roles[testerToken]).toEqual(current.roles[testerToken]);
      expect(migrated.roles[reviewerToken]).toEqual(current.roles[reviewerToken]);
      expect(migrated.roles[controllerToken(initialState.project)]).toEqual(
        current.roles[controllerToken(initialState.project)]
      );
      expect(migrated).toEqual(current);
      expect(pendingDeliveryId(migrated, implementerToken)).toBe(TEST_DELIVERY_ID);
      expect(pendingDeliveryId(migrated, testerToken)).toBe(TEST_DELIVERY_ID);
      expect(await readFile(`${file}.v25.bak`, "utf8")).toBe(raw);
    } finally {
      dateNowSpy.mockRestore();
      randomUuidSpy.mockRestore();
    }
  });

  it("composes the LEGION-33 v24 -> v25 bump with the v25 -> v26 classification and the v26 -> v27 bump: a v24 file with bare-string pendingAssignments migrates through all three to v27 objects", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-v24-through-v26-"));
    const file = path.join(tempDir, "state.json");
    const current = newLegionState(initialState.project, initialState.cap);
    const implementerToken = roleToken(initialState.project, issue, "implementer");
    const testerToken = roleToken(initialState.project, issue, "tester");
    const catchupTask = JSON.stringify({ type: "catchup-worker", unhandled: [] });
    current.roles = {
      [implementerToken]: {
        issue,
        role: "implementer",
        generation: 1,
        pendingAssignment: {
          kind: "assignment",
          task: "implement #43",
          queuedAt: MIGRATED_AT_ISO,
          deliveryId: TEST_DELIVERY_ID,
        },
      },
      [testerToken]: {
        issue,
        role: "tester",
        sessionId: "ses_tester",
        generation: 2,
        pendingAssignment: {
          kind: "catchup",
          task: catchupTask,
          queuedAt: MIGRATED_AT_ISO,
          deliveryId: TEST_DELIVERY_ID,
        },
      },
    };
    // A file the pre-LEGION-33 daemon wrote: version 24, bare-string pending assignments.
    const v24State = JSON.parse(JSON.stringify({ ...current, version: 24 }));
    v24State.roles[implementerToken].pendingAssignment = "implement #43";
    v24State.roles[testerToken].pendingAssignment = catchupTask;
    const raw = JSON.stringify(v24State);
    await writeFile(file, raw, "utf8");

    const dateNowSpy = vi.spyOn(Date, "now").mockReturnValue(MIGRATED_AT);
    const randomUuidSpy = vi.spyOn(crypto, "randomUUID").mockReturnValue(TEST_DELIVERY_ID);
    try {
      const migrated = await loadState(file, initialState);

      expect(migrated.version).toBe(33);
      expect(migrated).toEqual(current);
      expect(pendingDeliveryId(migrated, implementerToken)).toBe(TEST_DELIVERY_ID);
      expect(pendingDeliveryId(migrated, testerToken)).toBe(TEST_DELIVERY_ID);
      expect(await readFile(`${file}.v24.bak`, "utf8")).toBe(raw);
    } finally {
      dateNowSpy.mockRestore();
      randomUuidSpy.mockRestore();
    }
  });

  it("classifies a v25 pendingAssignment that is not JSON, or JSON of another type, as an architect assignment", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-v25-freetext-"));
    const file = path.join(tempDir, "state.json");
    const current = newLegionState(initialState.project, initialState.cap);
    const plannerToken = roleToken(initialState.project, issue, "planner");
    const architectToken = roleToken(initialState.project, issue, "architect");
    const otherJson = JSON.stringify({ type: "review-round", round: 2 });
    current.roles = {
      [plannerToken]: {
        issue,
        role: "planner",
        pendingAssignment: {
          kind: "assignment",
          task: '{"not json',
          queuedAt: MIGRATED_AT_ISO,
          deliveryId: TEST_DELIVERY_ID,
        },
      },
      [architectToken]: {
        issue,
        role: "architect",
        pendingAssignment: {
          kind: "assignment",
          task: otherJson,
          queuedAt: MIGRATED_AT_ISO,
          deliveryId: TEST_DELIVERY_ID,
        },
      },
    };
    const v25State = JSON.parse(JSON.stringify({ ...current, version: 25 }));
    v25State.roles[plannerToken].pendingAssignment = '{"not json';
    v25State.roles[architectToken].pendingAssignment = otherJson;
    await writeFile(file, JSON.stringify(v25State), "utf8");

    const dateNowSpy = vi.spyOn(Date, "now").mockReturnValue(MIGRATED_AT);
    const randomUuidSpy = vi.spyOn(crypto, "randomUUID").mockReturnValue(TEST_DELIVERY_ID);
    try {
      const migrated = await loadState(file, initialState);
      expect(migrated).toEqual(current);
      expect(pendingDeliveryId(migrated, plannerToken)).toBe(TEST_DELIVERY_ID);
      expect(pendingDeliveryId(migrated, architectToken)).toBe(TEST_DELIVERY_ID);
    } finally {
      dateNowSpy.mockRestore();
      randomUuidSpy.mockRestore();
    }
  });

  it("rejects a v26 worker claim whose pendingAssignment is still a bare string", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-v26-string-assignment-"));
    const file = path.join(tempDir, "state.json");
    const testerToken = roleToken(initialState.project, issue, "tester");
    const raw = JSON.parse(JSON.stringify(stateWithTree()));
    raw.roles[testerToken] = { issue, role: "tester", pendingAssignment: "verify #41" };
    await writeFile(file, JSON.stringify(raw), "utf8");

    await expect(loadState(file, initialState)).rejects.toThrow(/Invalid Legion state/);
  });

  it("migrates v26 state to v27 as a pure version bump, keeping identity-less tmux locators and LEGION-37's {kind, task} pendingAssignments (stamped with queuedAt only at v31)", async () => {
    // The v26 file exactly as a daemon on main's LEGION-37 bump (#991) persists it: pending
    // assignments `{ kind, task }` objects without queuedAt, no pane identity on any locator.
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-v26-"));
    const file = path.join(tempDir, "state.json");
    const current = stateWithTree();
    current.controllerLocator = {
      runtime: "tmux",
      tmuxSession: "legion-omp-project",
      tmuxWindowId: "@7",
      tmuxPaneId: "%9",
    };
    const testerToken = roleToken(initialState.project, issue, "tester");
    const catchupTask = JSON.stringify({ type: "catchup-worker", unhandled: [] });
    current.roles[testerToken] = {
      issue,
      role: "tester",
      sessionId: "ses_tester",
      generation: 2,
      pendingAssignment: {
        kind: "catchup",
        task: catchupTask,
        queuedAt: MIGRATED_AT_ISO,
        deliveryId: TEST_DELIVERY_ID,
      },
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp-project",
        tmuxWindowId: "@42",
        tmuxPaneId: "%3",
        socketPath: "/state/workers/tester.sock",
      },
    };
    const v26State = JSON.parse(JSON.stringify({ ...current, version: 26 }));
    delete v26State.roles[testerToken].pendingAssignment.queuedAt;
    v26State.controllerLocator.socketPath = "/state/workers/controller.sock";
    const raw = JSON.stringify(v26State);
    await writeFile(file, raw, "utf8");

    const dateNowSpy = vi.spyOn(Date, "now").mockReturnValue(MIGRATED_AT);
    try {
      const migrated = await loadState(file, initialState);

      expect(migrated.version).toBe(33);
      // The v31 queuedAt backfill and durable-ledger migration preserve the #991 shape and its
      // identity-less locator, so the migrated state equals the current fixture.
      expect(migrated).toMatchObject(current);
      expect(await readFile(`${file}.v26.bak`, "utf8")).toBe(raw);
    } finally {
      dateNowSpy.mockRestore();
    }
  });

  it("migrates v30 state to v31 by stamping the migration time as queuedAt onto every pending task without one, leaving stamped, pending-less, and controller claims untouched", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-v30-"));
    const file = path.join(tempDir, "state.json");
    const testerToken = roleToken(initialState.project, issue, "tester");
    const plannerToken = roleToken(initialState.project, issue, "planner");
    const reviewerToken = roleToken(initialState.project, issue, "reviewer");
    const raw = JSON.parse(JSON.stringify(stateWithTree()));
    raw.version = 30;
    delete raw.spawnRequests;
    raw.roles[testerToken] = {
      issue,
      role: "tester",
      pendingAssignment: { kind: "assignment", task: "verify #41" },
    };
    raw.roles[plannerToken] = {
      issue,
      role: "planner",
      pendingAssignment: {
        kind: "catchup",
        task: '{"type":"catchup-worker"}',
        queuedAt: "2026-09-01T00:00:00.000Z",
      },
    };
    raw.roles[reviewerToken] = { issue, role: "reviewer" };
    const rawText = JSON.stringify(raw);
    await writeFile(file, rawText, "utf8");

    const migrationTimestamp = 1_726_000_000_000;
    const dateNowSpy = vi.spyOn(Date, "now").mockReturnValue(migrationTimestamp);
    try {
      const migrated = await loadState(file, initialState);

      expect(migrated.version).toBe(33);
      expect(migrated.roles[testerToken]).toMatchObject({
        issue,
        role: "tester",
        pendingAssignment: {
          kind: "assignment",
          task: "verify #41",
          queuedAt: new Date(migrationTimestamp).toISOString(),
        },
      });
      expect(migrated.roles[plannerToken]).toMatchObject(raw.roles[plannerToken]);
      expect(migrated.roles[reviewerToken]).toEqual(raw.roles[reviewerToken]);
      expect(migrated.roles[controllerToken(initialState.project)]).toEqual(
        raw.roles[controllerToken(initialState.project)]
      );
      expect(await readFile(`${file}.v30.bak`, "utf8")).toBe(rawText);
      expect(JSON.parse(await readFile(file, "utf8")).version).toBe(33);
    } finally {
      dateNowSpy.mockRestore();
    }
  });

  it("migrates v30 state to v31 with an empty durable spawn request ledger", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-v30-"));
    const file = path.join(tempDir, "state.json");
    const current = stateWithTree();
    const raw = JSON.parse(JSON.stringify(current));
    raw.version = 30;
    delete raw.spawnRequests;
    const rawText = JSON.stringify(raw);
    await writeFile(file, rawText, "utf8");

    const migrated = await loadState(file, initialState);

    expect(migrated).toEqual(current);
    expect(await readFile(`${file}.v30.bak`, "utf8")).toBe(rawText);
    expect(JSON.parse(await readFile(file, "utf8")).version).toBe(33);
  });

  it("migrates v31 pending assignments with delivery IDs while preserving existing IDs", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-v31-"));
    const file = path.join(tempDir, "state.json");
    const testerToken = roleToken(initialState.project, issue, "tester");
    const plannerToken = roleToken(initialState.project, issue, "planner");
    const raw = JSON.parse(JSON.stringify(stateWithTree()));
    raw.version = 31;
    raw.roles[testerToken] = {
      issue,
      role: "tester",
      pendingAssignment: {
        kind: "assignment",
        task: "verify #41",
        queuedAt: "2026-09-01T00:00:00.000Z",
      },
    };
    raw.roles[plannerToken] = {
      issue,
      role: "planner",
      pendingAssignment: {
        kind: "catchup",
        task: '{"type":"catchup-worker"}',
        queuedAt: "2026-09-01T00:00:00.000Z",
        deliveryId: "00000000-0000-4000-8000-000000000031",
      },
    };
    const rawText = JSON.stringify(raw);
    await writeFile(file, rawText, "utf8");

    const migrated = await loadState(file, initialState);

    expect(migrated.version).toBe(33);
    const tester = migrated.roles[testerToken];
    if (!tester || !("issue" in tester)) throw new Error("tester claim disappeared");
    expect(tester.pendingAssignment).toMatchObject({
      kind: "assignment",
      task: "verify #41",
      queuedAt: "2026-09-01T00:00:00.000Z",
    });
    expect(tester.pendingAssignment?.deliveryId).toMatch(
      /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/
    );
    const planner = migrated.roles[plannerToken];
    if (!planner || !("issue" in planner)) throw new Error("planner claim disappeared");
    expect(planner.pendingAssignment?.deliveryId).toBe("00000000-0000-4000-8000-000000000031");
    expect(JSON.parse(await readFile(file, "utf8")).version).toBe(33);
    expect(await readFile(`${file}.v31.bak`, "utf8")).toBe(rawText);
  });

  it("migrates v32 state to v33 by stripping the controller locator's socketPath and keeping its runtime tag, leaving every pending delivery ID alone", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-v32-"));
    const file = path.join(tempDir, "state.json");
    const current = stateWithTree();
    const workerToken = roleToken(initialState.project, issue, "implementer");
    current.controllerLocator = {
      runtime: "tmux",
      tmuxSession: "legion-omp-project",
      tmuxWindowId: "@7",
      tmuxPaneId: "%9",
      ompSessionFile: "/state/controller.jsonl",
    };
    current.roles[workerToken] = {
      ...current.roles[workerToken],
      issue,
      role: "implementer",
      pendingAssignment: {
        kind: "assignment",
        task: "implement controller",
        queuedAt: "2026-09-14T00:00:00.000Z",
        deliveryId: "00000000-0000-4000-8000-000000000032",
      },
    };
    const rawText = JSON.stringify({
      ...current,
      version: 32,
      controllerLocator: {
        ...current.controllerLocator,
        socketPath: "/state/workers/controller.sock",
      },
    });
    await writeFile(file, rawText, "utf8");

    const stripped: Array<{ tmuxSession?: string; tmuxPaneId?: string }> = [];
    const migrated = await loadState(file, {
      ...initialState,
      onHeadlessControllerStripped: (locator) => stripped.push(locator),
    });

    expect(migrated.version).toBe(33);
    expect(migrated).toEqual(current);
    expect(pendingDeliveryId(migrated, workerToken)).toBe("00000000-0000-4000-8000-000000000032");
    expect(await readFile(`${file}.v32.bak`, "utf8")).toBe(rawText);
    // The upgrade warning fires exactly once, naming the pane the operator must kill by hand.
    expect(stripped).toEqual([{ tmuxSession: "legion-omp-project", tmuxPaneId: "%9" }]);
    // Loading the migrated (v33, socketless) file again strips nothing and stays silent.
    await loadState(file, {
      ...initialState,
      onHeadlessControllerStripped: (locator) => stripped.push(locator),
    });
    expect(stripped).toHaveLength(1);
  });

  it("migrates v32 state without a controller locator to v33 unchanged", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-v32-no-controller-"));
    const file = path.join(tempDir, "state.json");
    const current = stateWithTree();
    delete current.controllerLocator;
    await writeFile(file, JSON.stringify({ ...current, version: 32 }), "utf8");

    const stripped: unknown[] = [];
    expect(
      await loadState(file, {
        ...initialState,
        onHeadlessControllerStripped: (locator) => stripped.push(locator),
      })
    ).toEqual(current);
    expect(stripped).toEqual([]);
  });

  it("loads a retained spawn result with a future response field", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-spawn-result-"));
    const file = path.join(tempDir, "state.json");
    const requestId = "4e0aca36-77b3-43bd-96cf-d58890ae64e4";
    const raw = JSON.parse(JSON.stringify(stateWithTree()));
    raw.spawnRequests[requestId] = {
      tree: issue,
      issue,
      role: "tester",
      task: "verify #41",
      result: {
        status: "queued",
        roleToken: roleToken(initialState.project, issue, "tester"),
        futureField: "preserved",
      },
      settledAt: MIGRATED_AT,
    };
    await writeFile(file, JSON.stringify(raw), "utf8");

    const loaded = await loadState(file, initialState);

    expect((loaded.spawnRequests[requestId]?.result as Record<string, unknown>).futureField).toBe(
      "preserved"
    );
  });

  it("rejects a current-version (v31) pending task without queuedAt", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-v31-unstamped-"));
    const file = path.join(tempDir, "state.json");
    const testerToken = roleToken(initialState.project, issue, "tester");
    const raw = JSON.parse(JSON.stringify(stateWithTree()));
    raw.roles[testerToken] = {
      issue,
      role: "tester",
      pendingAssignment: { kind: "assignment", task: "verify #41" },
    };
    await writeFile(file, JSON.stringify(raw), "utf8");

    await expect(loadState(file, initialState)).rejects.toThrow(/Invalid Legion state/);
  });

  it.each([
    ["the same kind and text", { kind: "assignment", task: "verify #41" }, true],
    ["the same kind, other text", { kind: "assignment", task: "verify #42" }, false],
    ["another kind, the same text", { kind: "catchup", task: "verify #41" }, false],
  ] as const)("samePendingTask is true only for the same kind and text: %s", (_label, next, same) => {
    const existing = {
      kind: "assignment",
      task: "verify #41",
      queuedAt: "2026-09-13T17:00:00.000Z",
      deliveryId: TEST_DELIVERY_ID,
    } as const;
    expect(samePendingTask(existing, next)).toBe(same);
    expect(samePendingTask(undefined, next)).toBeFalse();
  });

  it("migrates v28 state through v29, v30, and v31, keeping phase records without assignedAt and accepting a fresh assignment-shaped one (LEGION-72)", async () => {
    // A v28 file exactly as a daemon on main persists it: one live phase record and one completed
    // record, neither carrying `assignedAt`. The migration backfills nothing — a record from
    // before the field existed stays without it forever (the log prints `assignedAt unknown`).
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-v28-"));
    const file = path.join(tempDir, "state.json");
    const current = stateWithTree();
    const otherIssue = "LEGION-43";
    current.issues[otherIssue] = {
      key: otherIssue,
      title: "Sibling",
      children: [],
      status: "retro",
    };
    current.phases[issue] = { phase: "reviewer", sessionId: "ses_reviewer" };
    current.phases[otherIssue] = {
      phase: "tester",
      sessionId: "ses_tester",
      completed: { summary: "verified", at: "2026-09-13T05:36:58.000Z" },
    };
    const raw = JSON.stringify({ ...current, version: 28 });
    await writeFile(file, raw, "utf8");

    const migrated = await loadState(file, initialState);

    expect(migrated.version).toBe(33);
    expect(migrated).toEqual(current);
    expect(migrated.phases[issue]).toEqual({ phase: "reviewer", sessionId: "ses_reviewer" });
    expect(migrated.phases[otherIssue]).toEqual({
      phase: "tester",
      sessionId: "ses_tester",
      completed: { summary: "verified", at: "2026-09-13T05:36:58.000Z" },
    });
    expect(await readFile(`${file}.v28.bak`, "utf8")).toBe(raw);

    // A record the current daemon writes on an assignment carries `assignedAt` and round-trips.
    migrated.phases[issue] = {
      phase: "reviewer",
      sessionId: "ses_reviewer",
      assignedAt: "2026-09-13T05:20:00.000Z",
    };
    await saveState(file, migrated);
    const reloaded = await loadState(file, initialState);
    expect(reloaded.phases[issue]).toEqual({
      phase: "reviewer",
      sessionId: "ses_reviewer",
      assignedAt: "2026-09-13T05:20:00.000Z",
    });
  });

  it("migrates v29 state through v30 and v31, leaving every tree unchanged (LEGION-83, acceptance 5)", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-v29-"));
    const file = path.join(tempDir, "state.json");
    const current = stateWithTree();
    const rawState = JSON.parse(JSON.stringify(current));
    rawState.version = 29;
    delete rawState.spawnRequests;
    const raw = JSON.stringify(rawState);
    await writeFile(file, raw, "utf8");

    const migrated = await loadState(file, initialState);

    expect(migrated.version).toBe(33);
    expect(migrated.trees).toEqual(current.trees);
    expect(migrated).toEqual(current);
    expect(await readFile(`${file}.v29.bak`, "utf8")).toBe(raw);
    // Written once, at load: the next boot reads v31 and runs no migration.
    expect(JSON.parse(await readFile(file, "utf8")).version).toBe(33);
  });

  it("round-trips a tree's kept session file (resumeSessionFile) through save and load", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-tree-resume-"));
    const file = path.join(tempDir, "state.json");
    const current = stateWithTree();
    const tree = current.trees[issue];
    if (!tree) throw new Error("fixture tree missing");
    delete tree.locator;
    tree.status = "queued";
    tree.resumeSessionFile = "/state/trees/sjawhar-legion-42/.omp/session.json";
    await saveState(file, current);

    expect(await loadState(file, initialState)).toEqual(current);
  });

  it("round-trips a tmux locator's recorded process identity on a tree and worker, with a socketless controller", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-identity-"));
    const file = path.join(tempDir, "state.json");
    const current = stateWithTree();
    const tree = current.trees[issue];
    if (!tree?.locator || tree.locator.runtime !== "tmux") throw new Error("fixture: tmux tree");
    tree.locator = { ...tree.locator, panePid: 4242, paneStartTicks: 987654321 };
    const testerToken = roleToken(initialState.project, issue, "tester");
    current.roles[testerToken] = {
      issue,
      role: "tester",
      generation: 1,
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp-project",
        tmuxWindowId: "@42",
        tmuxPaneId: "%3",
        panePid: 4300,
        paneStartTicks: 0,
        socketPath: "/state/workers/tester.sock",
      },
    };
    current.controllerLocator = {
      runtime: "tmux",
      tmuxSession: "legion-omp-project",
      tmuxWindowId: "@7",
      tmuxPaneId: "%9",
      panePid: 5000,
      paneStartTicks: 123,
      ompSessionFile: "/state/controller.jsonl",
    };
    await saveState(file, current);
    expect(await loadState(file, initialState)).toEqual(current);

    const invalid = JSON.parse(await readFile(file, "utf8"));
    invalid.controllerLocator.panePid = 0;
    await writeFile(file, JSON.stringify(invalid), "utf8");
    await expect(loadState(file, initialState)).rejects.toThrow(/Invalid Legion state/);
  });

  it("round-trips a kubernetes locator and rejects one missing a required field", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-k8s-"));
    const file = path.join(tempDir, "state.json");
    const current = stateWithTree();
    const testerToken = roleToken(initialState.project, issue, "tester");
    current.roles[testerToken] = {
      issue,
      role: "tester",
      generation: 1,
      locator: {
        runtime: "kubernetes",
        namespace: "legion",
        podName: "legion-legion-42-tester-g1",
        podUid: "8f0c8d2e-4c1a-4e6b-9c7a-0d1e2f3a4b5c",
        pvcName: "legion-legion-42",
        roleToken: "legion-omp-legion-42-tester",
        ompSessionFile: "/legion/sessions/tester/session.jsonl",
      },
    };
    await saveState(file, current);
    expect(await loadState(file, initialState)).toEqual(current);

    const raw = JSON.parse(JSON.stringify(current));
    delete raw.roles[testerToken].locator.podUid;
    await writeFile(file, JSON.stringify(raw), "utf8");
    await expect(loadState(file, initialState)).rejects.toThrow(/Invalid Legion state/);

    // The stream claim token is required: without it the runtime could not find the pod's
    // registered stream, so a locator lacking it is refused naming the field.
    const withoutToken = JSON.parse(JSON.stringify(current));
    delete withoutToken.roles[testerToken].locator.roleToken;
    await writeFile(file, JSON.stringify(withoutToken), "utf8");
    await expect(loadState(file, initialState)).rejects.toThrow(/locator\.roleToken/);
  });

  it("rejects a tmux worker claim whose locator lacks its socket or pane id, naming the claim and the field, while a tree locator may lack both", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-worker-strict-"));
    const file = path.join(tempDir, "state.json");
    const current = stateWithTree();
    const testerToken = roleToken(initialState.project, issue, "tester");
    current.roles[testerToken] = {
      issue,
      role: "tester",
      generation: 1,
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%3",
        socketPath: "/state/workers/tester.sock",
      },
    };
    // `stateWithTree()`'s own tree locator has neither pane id nor socket: still valid.
    await saveState(file, current);
    expect(await loadState(file, initialState)).toEqual(current);

    for (const field of ["socketPath", "tmuxPaneId"]) {
      const raw = JSON.parse(JSON.stringify(current));
      delete raw.roles[testerToken].locator[field];
      await writeFile(file, JSON.stringify(raw), "utf8");
      await expect(loadState(file, initialState)).rejects.toThrow(
        `Invalid Legion state: worker claim ${issue}/tester has a tmux locator without ${field}`
      );
    }
  });

  it("migrates a v24 file through v25, v26, v27, v28, v29, and v30 to v31, leaving PR records untouched", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-v24-"));
    const file = path.join(tempDir, "state.json");
    const current = stateWithTree();
    const raw = JSON.stringify({ ...current, version: 24 });
    await writeFile(file, raw, "utf8");

    const migrated = await loadState(file, initialState);

    expect(migrated).toEqual(current);
    // A v24 file walks the whole chain: v24 -> v25 (#993) -> v26 (#991) -> v27 (pane identity)
    // -> v28 (design gate) -> v29 (assignedAt) -> v30 (resumeSessionFile) -> v31
    // (queuedAt and durable spawn requests).
    expect(migrated.version).toBe(33);
    expect(Object.keys(migrated.prs[prKey] ?? {})).toEqual(Object.keys(current.prs[prKey] ?? {}));
    expect(await readFile(`${file}.v24.bak`, "utf8")).toBe(raw);
  });

  it("round-trips a PR carrying headCounted, pendingPush, and blockedAttempts", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-push-fields-"));
    const file = path.join(tempDir, "state.json");
    const current = stateWithTree();
    const pr = current.prs[prKey];
    if (!pr) throw new Error("fixture PR missing");
    pr.headCounted = true;
    pr.pendingPush = { sha: "def456", handoffOnly: true };
    pr.blockedAttempts = 3;

    await saveState(file, current);

    expect(await loadState(file, initialState)).toEqual(current);
  });

  it("rejects headCounted: false", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-head-counted-false-"));
    const file = path.join(tempDir, "state.json");
    const state = JSON.parse(JSON.stringify(stateWithTree()));
    state.prs[prKey].headCounted = false;
    await writeFile(file, JSON.stringify(state), "utf8");

    await expect(loadState(file, initialState)).rejects.toThrow(/Invalid Legion state/);
  });

  it("rejects a current-version (v27) locator that carries no runtime discriminant", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-untagged-"));
    const file = path.join(tempDir, "state.json");
    const untagged = JSON.parse(JSON.stringify(stateWithTree()));
    delete untagged.trees[issue].locator.runtime;
    await writeFile(file, JSON.stringify(untagged), "utf8");

    await expect(loadState(file, initialState)).rejects.toThrow(/Invalid Legion state/);
  });

  describe("v27 -> v28 design gates", () => {
    const active = (root: string) => ({ root, generation: 1, status: "active", launchFailures: 0 });
    /** A v27 file (after LEGION-33's, LEGION-37's, and LEGION-27's steps; the gate records still `{designAskId, designApproved}`) with one gate of each kind the migration distinguishes: approved by a human on
     * an active tree (LEGION-1), registered but unanswered on an active tree (LEGION-2), never
     * registered (LEGION-3, no tree either), and satisfied by `gates.design: off` on a tree that
     * has since closed (LEGION-4). */
    function v27File() {
      const current = newLegionState(initialState.project, initialState.cap);
      return {
        ...current,
        version: 27,
        trees: {
          "LEGION-1": active("LEGION-1"),
          "LEGION-2": active("LEGION-2"),
          "LEGION-4": { ...active("LEGION-4"), status: "closed" },
        },
        gates: {
          "LEGION-1": { designAskId: "ask-1", designApproved: "ask-1" },
          "LEGION-2": { designAskId: "ask-2" },
          "LEGION-3": {},
          "LEGION-4": { designAskId: "ask-4", designApproved: "gate-off" },
        },
      };
    }

    it("resolves each kept gate's spec document from Dispatch and drops the rest with a log line naming the issue (acceptance 5)", async () => {
      tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-v27-"));
      const file = path.join(tempDir, "state.json");
      const source = v27File();
      await writeFile(file, JSON.stringify(source), "utf8");
      const resolved: string[] = [];
      const warnings: string[] = [];
      const warnSpy = vi.spyOn(console, "warn").mockImplementation((...args: unknown[]) => {
        warnings.push(args.map(String).join(" "));
      });
      try {
        const migrated = await loadState(file, {
          ...initialState,
          resolveSpecArtifact: async (issue) => {
            resolved.push(issue);
            if (issue === "LEGION-1") return { artifactId: "art-a", latestVersion: 7 };
            if (issue === "LEGION-2") return { artifactId: "art-b", latestVersion: 2 };
            throw new Error(`unexpected resolve for ${issue}`);
          },
        });

        expect(migrated.version).toBe(33);
        expect(migrated.gates).toEqual({
          "LEGION-1": { artifactId: "art-a", latestVersion: 7, approvedVersion: 7 },
          "LEGION-2": { artifactId: "art-b", latestVersion: 2 },
        });
        expect([...resolved].sort()).toEqual(["LEGION-1", "LEGION-2"]);
        expect(warnings.filter((line) => line.includes("LEGION-3"))).toHaveLength(1);
        expect(warnings.filter((line) => line.includes("LEGION-4"))).toHaveLength(1);
        expect(warnings.some((line) => line.includes("LEGION-1"))).toBe(false);
        expect(await readFile(`${file}.v27.bak`, "utf8")).toBe(JSON.stringify(source));
        // The migrated state is on disk at once: the next boot loads the current version and
        // never asks Dispatch again, even with no ordinary save in between.
        expect(JSON.parse(await readFile(file, "utf8")).version).toBe(33);
        const reloaded = await loadState(file, {
          ...initialState,
          resolveSpecArtifact: async (issue) => {
            throw new Error(`the migration ran again for ${issue}`);
          },
        });
        expect(reloaded.gates).toEqual(migrated.gates);
        expect(resolved).toHaveLength(2);
      } finally {
        warnSpy.mockRestore();
      }
    });

    it("refuses to load, naming the issue, when a kept gate's spec cannot be resolved", async () => {
      tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-v27-unresolved-"));
      const file = path.join(tempDir, "state.json");
      await writeFile(file, JSON.stringify(v27File()), "utf8");
      const warnSpy = vi.spyOn(console, "warn").mockImplementation(() => {});
      try {
        await expect(
          loadState(file, {
            ...initialState,
            resolveSpecArtifact: async () => {
              throw new Error("Dispatch unreachable");
            },
          })
        ).rejects.toThrow("Cannot migrate the design gate for LEGION-1: Dispatch unreachable");
        await expect(loadState(file, initialState)).rejects.toThrow(
          "Cannot migrate the design gate for LEGION-1"
        );
        // Nothing was written: the v27 file survives intact for a retry once Dispatch is back.
        expect(await readdir(tempDir)).toEqual(["state.json"]);
      } finally {
        warnSpy.mockRestore();
      }
    });

    it("needs no resolver for a v27 file without gates", async () => {
      tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-v27-empty-"));
      const file = path.join(tempDir, "state.json");
      const current = newLegionState(initialState.project, initialState.cap);
      await writeFile(file, JSON.stringify({ ...current, version: 27 }), "utf8");

      expect(await loadState(file, initialState)).toEqual(current);
    });

    it("migrates a v24 file end to end through 25 (#993), 26 (#991), 27 (#981), 28 (design gate), 29 (assignedAt), and 30 (resumeSessionFile) to v31, composing every step", async () => {
      tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-v24-through-v31-"));
      const file = path.join(tempDir, "state.json");
      // What a pre-LEGION-33 daemon wrote: a v24 file whose root tree carries a locator without
      // #981's process identity, whose architect claim carries a bare-string pending assignment
      // (#991 classifies it), and whose gate is a human-approved ask (this branch resolves it).
      const architectToken = roleToken(initialState.project, "LEGION-1", "architect");
      const source = {
        ...v27File(),
        version: 24,
        trees: {
          "LEGION-1": {
            ...active("LEGION-1"),
            locator: {
              runtime: "tmux",
              tmuxSession: "legion-omp",
              tmuxWindowId: "@1",
              tmuxPaneId: "%1",
              socketPath: "/state/workers/architect.sock",
            },
          },
        },
        roles: {
          [architectToken]: {
            issue: "LEGION-1",
            role: "architect",
            generation: 1,
            pendingAssignment: JSON.stringify({ type: "catchup-worker", unhandled: [] }),
          },
        },
        gates: { "LEGION-1": { designAskId: "ask-1", designApproved: "ask-1" } },
      };
      await writeFile(file, JSON.stringify(source), "utf8");

      const dateNowSpy = vi.spyOn(Date, "now").mockReturnValue(MIGRATED_AT);
      let migrated: LegionState;
      try {
        migrated = await loadState(file, {
          ...initialState,
          resolveSpecArtifact: async () => ({ artifactId: "art-a", latestVersion: 4 }),
        });
      } finally {
        dateNowSpy.mockRestore();
      }

      expect(migrated.version).toBe(33);
      // #991's step classified the bare string; #981's step left the identity-less locator as it
      // was (both identity fields optional); the design-gate step resolved the gate; v29 added
      // optional assignedAt; v30 added optional resumeSessionFile; and v31 stamped queuedAt and
      // added the empty durable request ledger.
      const claim = migrated.roles[architectToken];
      if (!claim || !("issue" in claim)) throw new Error("architect claim was not migrated");
      expect(claim.pendingAssignment).toMatchObject({
        kind: "catchup",
        task: JSON.stringify({ type: "catchup-worker", unhandled: [] }),
        queuedAt: MIGRATED_AT_ISO,
      });
      expect(migrated.trees["LEGION-1"]?.locator).toEqual({
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@1",
        tmuxPaneId: "%1",
        socketPath: "/state/workers/architect.sock",
      });
      expect(migrated.gates).toEqual({
        "LEGION-1": { artifactId: "art-a", latestVersion: 4, approvedVersion: 4 },
      });
      expect(await readFile(`${file}.v24.bak`, "utf8")).toBe(JSON.stringify(source));
      expect(JSON.parse(await readFile(file, "utf8")).version).toBe(33);
    });
  });

  it("rejects a current-version controller locator that carries a shim socket, naming the field", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-controller-socket-"));
    const file = path.join(tempDir, "state.json");
    const current = stateWithTree();
    current.controllerLocator = {
      runtime: "tmux",
      tmuxSession: "legion-omp",
      tmuxWindowId: "@7",
      tmuxPaneId: "%9",
    };
    await saveState(file, current);
    expect(await loadState(file, initialState)).toEqual(current);

    // The same locator with the socket a pre-v33 headless controller had: a v33 file must never
    // carry it (the migration strips it), so an unmigrated or hand-edited one fails at load.
    await writeFile(
      file,
      JSON.stringify({
        ...current,
        controllerLocator: {
          ...current.controllerLocator,
          socketPath: "/state/workers/controller.sock",
        },
      }),
      "utf8"
    );
    await expect(loadState(file, initialState)).rejects.toThrow(
      "Invalid Legion state: controller locator carries a shim socket; the controller pane has none"
    );
  });

  it("accepts an issue's Dispatch status and design-gate entry on current state", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-status-"));
    const file = path.join(tempDir, "state.json");
    const current = stateWithTree();
    current.issues[issue] = { ...current.issues[issue], status: "in_progress" };
    current.gates[issue] = { artifactId: "art-1", latestVersion: 3, approvedVersion: 3 };
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

  describe("owningArchitect", () => {
    const root = "LEGION-1";
    const child = "LEGION-2";
    const grandchild = "LEGION-3";
    const orphan = "LEGION-9";
    const architectClaim = (key: string) => roleToken(initialState.project, key, "architect");

    /** R (active tree) > C > G, plus X with no tree and no parent; no architect claims. */
    function decomposedState() {
      const state = newLegionState(initialState.project, initialState.cap);
      state.issues[root] = { key: root, title: "Root", status: "in_progress", children: [child] };
      state.issues[child] = {
        key: child,
        title: "Child",
        status: "in_progress",
        parent: root,
        children: [grandchild],
      };
      state.issues[grandchild] = {
        key: grandchild,
        title: "Grandchild",
        status: "todo",
        parent: child,
        children: [],
      };
      state.issues[orphan] = { key: orphan, title: "Orphan", status: "todo", children: [] };
      state.trees[root] = { root, generation: 1, status: "active", launchFailures: 0 };
      return state;
    }

    it("resolves a child with no architect claim to the tree root", () => {
      expect(owningArchitect(decomposedState(), child)).toBe(root);
    });

    it("resolves a child to itself once it holds an architect claim", () => {
      const state = decomposedState();
      state.roles[architectClaim(child)] = { issue: child, role: "architect" };
      expect(owningArchitect(state, child)).toBe(child);
    });

    it("resolves a grandchild to the nearest claimed ancestor", () => {
      const state = decomposedState();
      state.roles[architectClaim(child)] = { issue: child, role: "architect" };
      expect(owningArchitect(state, grandchild)).toBe(child);
    });

    it("resolves a grandchild with no claims on its chain to the tree root", () => {
      expect(owningArchitect(decomposedState(), grandchild)).toBe(root);
    });

    it("starts at the parent for a wake about the issue's own architect role", () => {
      const state = decomposedState();
      state.roles[architectClaim(child)] = { issue: child, role: "architect" };
      expect(owningArchitect(state, child, "architect")).toBe(root);
      expect(owningArchitect(state, grandchild, "architect")).toBe(child);
    });

    it("returns the root for the unreachable root architect wake", () => {
      expect(owningArchitect(decomposedState(), root, "architect")).toBe(root);
    });

    it("treats a launch-failed architect claim with no session or locator as the owner", () => {
      const state = decomposedState();
      state.roles[architectClaim(child)] = { issue: child, role: "architect", launchFailures: 3 };
      expect(owningArchitect(state, child)).toBe(child);
    });

    it("throws for an issue that belongs to no tree", () => {
      expect(() => owningArchitect(decomposedState(), orphan)).toThrow("No owning architect");
    });

    it("throws for a corrupt parent cycle with no owning tree", () => {
      const state = newLegionState(initialState.project, initialState.cap);
      state.issues["LEGION-7"] = {
        key: "LEGION-7",
        title: "A",
        status: "todo",
        parent: "LEGION-8",
        children: [],
      };
      state.issues["LEGION-8"] = {
        key: "LEGION-8",
        title: "B",
        status: "todo",
        parent: "LEGION-7",
        children: [],
      };
      expect(() => owningArchitect(state, "LEGION-7")).toThrow("No owning architect");
    });

    it("starts at the issue itself for a phase-worker role", () => {
      const state = decomposedState();
      state.roles[architectClaim(child)] = { issue: child, role: "architect" };
      expect(owningArchitect(state, child, "planner")).toBe(child);
    });

    it("stops at a child admitted as a legacy root of its own, like rootForIssue and treeFor", () => {
      const state = decomposedState();
      state.trees[child] = { root: child, generation: 1, status: "lingering", launchFailures: 0 };
      expect(owningArchitect(state, child)).toBe(child);
    });
  });
});

describe("repairAdmissionDrift", () => {
  const activeTree = (root: string): TreeState => ({
    root,
    generation: 1,
    status: "active",
    launchFailures: 0,
  });

  it("adds every active tree missing from admission.active, pulling it out of the queue if it sits there", () => {
    const state = newLegionState("omp", 4);
    state.trees["LEGION-1"] = activeTree("LEGION-1");
    state.trees["LEGION-2"] = activeTree("LEGION-2");
    state.admission.active = ["LEGION-1"];
    state.admission.queue = ["LEGION-2", "LEGION-3"];

    expect(repairAdmissionDrift(state)).toEqual({ added: ["LEGION-2"], removed: [] });
    expect(state.admission).toEqual({
      cap: 4,
      active: ["LEGION-1", "LEGION-2"],
      queue: ["LEGION-3"],
    });
  });

  it("removes an entry whose tree is missing, lingering, closed, launch-failed, or queued, and leaves a dead one alone (a resurrection in flight)", () => {
    const state = newLegionState("omp", 8);
    state.trees["LEGION-1"] = activeTree("LEGION-1");
    state.trees["LEGION-2"] = { ...activeTree("LEGION-2"), status: "lingering" };
    state.trees["LEGION-3"] = { ...activeTree("LEGION-3"), status: "closed" };
    state.trees["LEGION-4"] = { ...activeTree("LEGION-4"), status: "launch-failed" };
    state.trees["LEGION-5"] = { ...activeTree("LEGION-5"), status: "queued" };
    state.trees["LEGION-6"] = { ...activeTree("LEGION-6"), status: "dead" };
    state.admission.active = [
      "LEGION-1",
      "LEGION-2",
      "LEGION-3",
      "LEGION-4",
      "LEGION-5",
      "LEGION-6",
      "LEGION-7",
    ];

    expect(repairAdmissionDrift(state)).toEqual({
      added: [],
      removed: [
        { issue: "LEGION-2", reason: "tree is lingering" },
        { issue: "LEGION-3", reason: "tree is closed" },
        { issue: "LEGION-4", reason: "tree is launch-failed" },
        { issue: "LEGION-5", reason: "tree is queued" },
        { issue: "LEGION-7", reason: "no tree recorded" },
      ],
    });
    expect(state.admission.active).toEqual(["LEGION-1", "LEGION-6"]);
  });

  it("drops a duplicate admission.active entry, keeping the first", () => {
    const state = newLegionState("omp", 2);
    state.trees["LEGION-1"] = activeTree("LEGION-1");
    state.admission.active = ["LEGION-1", "LEGION-1"];

    expect(repairAdmissionDrift(state)).toEqual({
      added: [],
      removed: [{ issue: "LEGION-1", reason: "duplicate entry" }],
    });
    expect(state.admission.active).toEqual(["LEGION-1"]);
  });

  it("reports occupancy above the cap without changing it", () => {
    const state = newLegionState("omp", 1);
    state.trees["LEGION-1"] = activeTree("LEGION-1");
    state.trees["LEGION-2"] = activeTree("LEGION-2");
    state.admission.active = ["LEGION-1", "LEGION-2"];

    expect(repairAdmissionDrift(state)).toEqual({
      added: [],
      removed: [],
      overCap: { active: 2, cap: 1 },
    });
    expect(state.admission.active).toEqual(["LEGION-1", "LEGION-2"]);
  });

  it("changes nothing on a consistent state", () => {
    const state = newLegionState("omp", 2);
    state.trees["LEGION-1"] = activeTree("LEGION-1");
    state.trees["LEGION-2"] = { ...activeTree("LEGION-2"), status: "queued" };
    state.admission.active = ["LEGION-1"];
    state.admission.queue = ["LEGION-2"];
    const before = structuredClone(state);

    expect(repairAdmissionDrift(state)).toEqual({ added: [], removed: [] });
    expect(state).toEqual(before);
  });

  it("describes each change as one line, the over-cap as one more, and nothing for a clean repair", () => {
    const lines = describeAdmissionDrift(
      {
        added: ["LEGION-2"],
        removed: [{ issue: "LEGION-7", reason: "no tree recorded" }],
        overCap: { active: 3, cap: 2 },
      },
      "at boot"
    );
    expect(lines).toHaveLength(3);
    expect(lines[0]).toContain("added LEGION-2");
    expect(lines[0]).toContain("at boot");
    expect(lines[1]).toContain("removed LEGION-7");
    expect(lines[1]).toContain("no tree recorded");
    expect(lines[2]).toContain("over cap");
    expect(lines[2]).toContain("3");
    expect(lines[2]).toContain("cap of 2");
    expect(describeAdmissionDrift({ added: [], removed: [] }, "by resync")).toEqual([]);
  });
});
