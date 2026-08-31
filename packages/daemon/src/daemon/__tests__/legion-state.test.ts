import { afterEach, describe, expect, it } from "bun:test";
import { chmod, mkdtemp, readdir, rm, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { controllerToken, formatIssueKey, roleToken } from "@legion/contracts";
import { loadState, newLegionState, saveState } from "../legion-state";

const issue = formatIssueKey("sjawhar", "legion", 42);
const initialState = { project: "omp", cap: 4 };

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
    heldEvents: [
      {
        role: "implementer",
        payloadJson: "{}",
        heldAt: "2026-08-24T00:00:00Z",
        eventId: "event-1",
      },
    ],
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
  state.prs["sjawhar/legion#7"] = {
    key: issue,
    repo: "sjawhar/legion",
    number: 7,
    headSha: "abc123",
    checks: { test: { status: "completed", conclusion: "success" } },
    firstRedEmitted: false,
    settledRedEmitted: false,
    greenEmitted: true,
    lastEventAt: 1_724_457_600_000,
    fixAttempts: 1,
    reviewDecision: "approved",
  };
  state.prByBranch["sjawhar/legion@legion-42"] = "sjawhar/legion#7";
  state.admission.active.push(issue);
  state.phases[issue] = { phase: "implement", sessionId: "ses_123" };
  state.controllerCapabilityHash = "f".repeat(64);
  return state;
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

  it("initializes empty v8 state with a valid project and admission capacity", () => {
    expect(newLegionState(initialState.project, initialState.cap)).toEqual({
      version: 8,
      project: "omp",
      issues: {},
      trees: {},
      roles: {},
      spawnCapabilities: {},
      prs: {},
      prByBranch: {},
      admission: { cap: 4, active: [], queue: [] },
      phases: {},
      controllerHeldEvents: [],
    });
  });

  it("persists and reloads state without losing tree, role, or PR data", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-v2-"));
    const file = path.join(tempDir, "state.json");
    const state = stateWithTree();

    await saveState(file, state);

    expect(await loadState(file, initialState)).toEqual(state);
  });

  it("migrates v5 name-only locators by clearing their unsafe identities", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-v5-"));
    const file = path.join(tempDir, "state.json");
    const current = stateWithTree();
    const legacy = {
      ...current,
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

    expect(migrated).toEqual(expected);
  });

  it("migrates v6 state by dropping attribution and locator pids", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-v6-"));
    const file = path.join(tempDir, "state.json");
    const current = stateWithTree();
    const legacy = {
      ...current,
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
  it("migrates v7 state by removing adopted dispatch threads and their held replies", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-v7-"));
    const file = path.join(tempDir, "state.json");
    const dispatchThread = formatIssueKey("sjawhar", "legion", 99);
    const retainedChild = formatIssueKey("sjawhar", "legion", 100);
    const retainedHeldEvent = {
      role: "implementer",
      payloadJson: '{"type":"work"}',
      heldAt: "2026-08-24T00:00:01Z",
      eventId: "event-work",
    };
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
    legacy.trees[issue].heldEvents = [
      retainedHeldEvent,
      {
        role: "architect",
        payloadJson: '{"type":"dispatch-reply","body":"answer"}',
        heldAt: "2026-08-24T00:00:02Z",
        eventId: "event-dispatch-reply",
      },
    ];
    const v7State = {
      ...legacy,
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
    expected.trees[issue].heldEvents = [retainedHeldEvent];

    expect(await loadState(file, initialState)).toEqual(expected);
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
