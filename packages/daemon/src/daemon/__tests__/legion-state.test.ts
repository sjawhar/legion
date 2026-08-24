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
      tmuxWindow: "sjawhar-legion-42",
      ompSessionFile: "/tmp/session.json",
      pid: 1234,
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
  state.dispatchThreads.push({
    repo: "sjawhar/legion",
    thread: 99,
    role: "architect",
    issue,
    tree: issue,
  });
  state.attribution.push({ sha: "abc123", sessionId: "ses_123", issue, phase: "implement" });
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

  it("initializes empty v4 state with a valid project and admission capacity", () => {
    expect(newLegionState(initialState.project, initialState.cap)).toEqual({
      version: 4,
      project: "omp",
      issues: {},
      trees: {},
      roles: {},
      prs: {},
      prByBranch: {},
      admission: { cap: 4, active: [], queue: [] },
      dispatchThreads: [],
      attribution: [],
      phases: {},
    });
  });

  it("persists and reloads state without losing tree, role, PR, or attribution data", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-v2-"));
    const file = path.join(tempDir, "state.json");
    const state = stateWithTree();

    await saveState(file, state);

    expect(await loadState(file, initialState)).toEqual(state);
  });

  it("creates the supplied initial v4 state when the file is absent", async () => {
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

  it("rejects prior v3 state rather than silently migrating launch-failure state", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-v4-"));
    const file = path.join(tempDir, "state.json");
    await writeFile(file, JSON.stringify({ version: 3 }), "utf8");

    await expect(loadState(file, initialState)).rejects.toThrow(
      "Unsupported Legion state version: 3"
    );
  });
  it("rejects malformed current-version state instead of accepting a partial object", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-v4-"));
    const file = path.join(tempDir, "state.json");
    await writeFile(file, JSON.stringify({ version: 4 }), "utf8");

    await expect(loadState(file, initialState)).rejects.toThrow("Invalid Legion state");
  });
});
