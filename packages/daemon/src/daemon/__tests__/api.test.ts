import { afterEach, beforeEach, describe, expect, it } from "bun:test";
import { mkdtemp, rm } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { controllerToken, type IssueKey, roleToken, roleTopic } from "@legion/contracts";
import type { CommandRunner } from "../../state/fetch";
import { type LegionApi, type LegionApiDeps, startLegionApi } from "../api";
import { secretHash, spawnCapabilityKey } from "../api/auth";
import { EnvoyPublishError } from "../api/http";
import { type LegionState, loadState, newLegionState, saveState } from "../legion-state";
import { TreeClosingError } from "../processes";
import { fakeDispatchClient } from "./ci-fixtures";

const root = "WIDGETS-1" as IssueKey;
const child = "WIDGETS-2" as IssueKey;
const otherRoot = "OTHER-9" as IssueKey;
const foreign = "OTHER-10" as IssueKey;

interface GrantResponse {
  grantId: string;
  expiresAt: string;
}
interface WorkerSessionResponse {
  tree: string;
  issue: string;
  role: string;
  secret: string;
}

describe("Legion HTTP API", () => {
  let api: LegionApi | undefined;
  let state: LegionState;
  let commands: string[][];
  let publications: Array<{ topic: string; payload: string }>;
  let tokenRoles: string[];
  let releaseSlots: IssueKey[];
  let closedTrees: IssueKey[];
  let admissions: IssueKey[];
  let spawnedWorkers: Array<{ tree: IssueKey; issue: IssueKey; role: string; task: string }>;
  let workerReadyCalls: Array<{
    issue: IssueKey;
    role: string;
    sessionId: string;
    generation: number;
  }>;
  let treeReadyConnected: IssueKey[];
  let controllerConnected: boolean;
  let workerReadyConnected: boolean;
  let confirmRootReadyCalls: Array<{ tree: IssueKey; generation: number }>;
  let now: number;
  let controllerSecret: string;

  beforeEach(() => {
    commands = [];
    publications = [];
    tokenRoles = [];
    releaseSlots = [];
    closedTrees = [];
    admissions = [];
    spawnedWorkers = [];
    workerReadyCalls = [];
    treeReadyConnected = [];
    controllerConnected = false;
    workerReadyConnected = false;
    confirmRootReadyCalls = [];
    now = 1_700_000_000_000;
    state = newLegionState("omp", 2);
    state.issues[root] = {
      key: root,
      title: "Root",
      status: "in_progress",
      children: [],
    };
    state.trees[root] = {
      root,
      generation: 3,
      locator: { tmuxSession: "legion-omp", tmuxWindowId: "@1" },
      status: "queued",
      launchFailures: 0,
    };
    state.issues[otherRoot] = {
      key: otherRoot,
      title: "Other root",
      status: "in_progress",
      children: [foreign],
    };
    state.issues[foreign] = {
      key: foreign,
      title: "Foreign child",
      parent: otherRoot,
      status: "in_progress",
      children: [],
    };
    state.trees[otherRoot] = {
      root: otherRoot,
      generation: 1,
      status: "active",
      launchFailures: 0,
    };
  });

  afterEach(() => api?.stop());

  async function start(options?: {
    runner?: CommandRunner;
    gates?: { design: "root-issues" | "off" };
    state?: LegionState;
    saveState?: () => Promise<void>;
    mintController?: boolean;
    admissionResult?: "spawned" | "queued";
    admit?: (issue: IssueKey) => "spawned" | "queued";
    onTreeReady?: (tree: IssueKey) => Promise<void>;
    onControllerReady?: () => Promise<void>;
    getToken?: LegionApiDeps["tokenManager"]["getToken"];
    envoyPublish?: LegionApiDeps["envoyPublish"];
    spawnWorkerImpl?: LegionApiDeps["processManager"]["spawnWorker"];
    mutateLiveRoleClaimImpl?: LegionApiDeps["processManager"]["mutateLiveRoleClaim"];
    markTreeReadyImpl?: LegionApiDeps["processManager"]["markTreeReady"];
    markControllerReadyImpl?: LegionApiDeps["processManager"]["markControllerReady"];
    workerReadyImpl?: LegionApiDeps["processManager"]["workerReady"];
    dispatchClient?: LegionApiDeps["dispatchClient"];
  }) {
    const runner =
      options?.runner ??
      ((async (command) => {
        commands.push(command);
        return { stdout: "", stderr: "", exitCode: 0 };
      }) satisfies CommandRunner);

    const deps: LegionApiDeps = {
      state: options?.state ?? state,
      runner,
      dispatchClient: options?.dispatchClient ?? fakeDispatchClient(),
      tokenManager: {
        getToken:
          options?.getToken ??
          (async (role, owner) => {
            tokenRoles.push(role);
            return {
              token: `minted-${role}-${owner}`,
              expiresAt: "2099-01-01T00:00:00.000Z",
              gitIdentity: {
                name: role === "review" ? "legion-review[bot]" : "legion-implement[bot]",
                email: `42+legion-${role}[bot]@users.noreply.github.com`,
              },
            };
          }),
      },
      processManager: {
        admit: (issue) => {
          admissions.push(issue);
          return options?.admit?.(issue) ?? options?.admissionResult ?? "spawned";
        },
        releaseSlot: (issue) => {
          releaseSlots.push(issue);
        },
        markTreeReady: options?.markTreeReadyImpl ?? (() => {}),
        confirmRootReady: (tree, generation) => {
          confirmRootReadyCalls.push({ tree, generation });
        },
        markControllerReady: options?.markControllerReadyImpl ?? (() => {}),
        cancelBootWatchdog: () => {},
        spawnWorker:
          options?.spawnWorkerImpl ??
          (async (tree, issue, role, task) => {
            spawnedWorkers.push({ tree, issue, role, task });
            return { status: "spawned", roleToken: roleToken(state.project, issue, role) };
          }),
        workerReady: (issue, role, sessionId, generation) => {
          workerReadyCalls.push({ issue, role, sessionId, generation });
          return options?.workerReadyImpl?.(issue, role, sessionId, generation);
        },
        rejectIfTreeGone: () => {},
        mutateLiveRoleClaim:
          options?.mutateLiveRoleClaimImpl ?? (async (_tree, _issue, _token, fn) => fn()),
        beginLinger: (tree) => {
          const treeState = state.trees[tree];
          if (treeState) treeState.status = "lingering";
        },
        markProcessDead: () => {},
        reportRootExit: (tree) => {
          releaseSlots.push(tree);
          closedTrees.push(tree);
          const treeState = state.trees[tree];
          if (treeState) treeState.status = "closed";
        },
        closeTree: (tree) => {
          releaseSlots.push(tree);
          closedTrees.push(tree);
          const treeState = state.trees[tree];
          if (treeState) treeState.status = "closed";
        },
      },
      envoyPublish:
        options?.envoyPublish ??
        (async (topic, payload) => {
          publications.push({ topic, payload });
        }),
      onControllerReady: options?.onControllerReady ?? (async () => {}),
      onControllerEvent: async (payload) => {
        publications.push({
          topic: `notifications.role.${controllerToken(state.project)}`,
          payload: JSON.stringify(payload),
        });
      },
      saveState: options?.saveState ?? (async () => {}),
      onTreeReady: options?.onTreeReady,
    };
    api = startLegionApi(
      {
        port: 0,
        hostname: "127.0.0.1",
        repo: "acme/widgets",
        gates: options?.gates ?? { design: "root-issues" },
        now: () => now,
      },
      deps
    );
    if (options?.mintController !== false) {
      controllerSecret = await api.mintControllerCapability();
    }
  }

  async function request(path: string, body?: unknown) {
    if (!api) throw new Error("API was not started");
    return fetch(`http://127.0.0.1:${api.server.port}${path}`, {
      method: body === undefined ? "GET" : "POST",
      headers: body === undefined ? undefined : { "content-type": "application/json" },
      body: body === undefined ? undefined : JSON.stringify(body),
    });
  }

  async function json<T = unknown>(path: string, body: unknown) {
    const response = await request(path, body);
    const responseBody = (await response.json()) as T;
    return { response, body: responseBody };
  }
  async function curl(path: string, body: unknown): Promise<{ status: number; body: string }> {
    if (!api) throw new Error("API was not started");
    const proc = Bun.spawn(
      [
        "curl",
        "--silent",
        "--show-error",
        "--write-out",
        "\n%{http_code}",
        "--request",
        "POST",
        "--header",
        "content-type: application/json",
        "--data",
        JSON.stringify(body),
        `http://127.0.0.1:${api.server.port}${path}`,
      ],
      { stdout: "pipe", stderr: "pipe" }
    );
    const [stdout, stderr, exitCode] = await Promise.all([
      new Response(proc.stdout).text(),
      new Response(proc.stderr).text(),
      proc.exited,
    ]);
    if (exitCode !== 0) throw new Error(`curl failed: ${stderr}`);
    const statusOffset = stdout.lastIndexOf("\n");
    return {
      status: Number(stdout.slice(statusOffset + 1)),
      body: stdout.slice(0, statusOffset),
    };
  }

  async function curlJson<T>(path: string, body: unknown): Promise<{ status: number; body: T }> {
    const response = await curl(path, body);
    const responseBody = JSON.parse(response.body) as T;
    return { status: response.status, body: responseBody };
  }

  it("drains each held event exactly once when a child wave releases", async () => {
    const statusWrites: Array<{ issue: IssueKey; status: string }> = [];
    await start({
      dispatchClient: fakeDispatchClient({
        setStatus: async (issue, status) => {
          statusWrites.push({ issue, status });
        },
      }),
    });
    const bootToken = await api?.mintBootToken(root, 3);
    if (!bootToken) throw new Error("boot nonce was not minted");

    const started = await json("/legion/v1/process/started", {
      tree: root,
      generation: 3,
      rootSessionId: "ses_root",
      bootToken,
      agentId: "root-agent",
      ompSessionFile: "/tmp/root.json",
    });
    expect(started.response.status).toBe(200);
    expect(started.body).toEqual({
      roleTokens: {
        architect: "legion-omp-widgets-1-architect",
        planner: "legion-omp-widgets-1-planner",
        implementer: "legion-omp-widgets-1-implementer",
        tester: "legion-omp-widgets-1-tester",
        reviewer: "legion-omp-widgets-1-reviewer",
        merger: "legion-omp-widgets-1-merger",
      },
      controlSubject: "legion.ctl.widgets-1.3",
      gates: { design: "root-issues" },
      secret: expect.any(String),
    });
    if (
      typeof started.body !== "object" ||
      started.body === null ||
      !("secret" in started.body) ||
      typeof started.body.secret !== "string"
    ) {
      throw new Error("root start response is missing its architect capability");
    }
    const architect = { sessionId: "ses_root", secret: started.body.secret };
    expect(state.trees[root]).toMatchObject({
      status: "active",
      locator: { ompSessionFile: "/tmp/root.json" },
    });

    // The daemon never creates a Dispatch issue itself (Dispatch is the sole issue lifecycle
    // source); the architect's child issue arrives as an `issue.created` Dispatch event applied
    // by the reducer elsewhere, so setup here mutates state directly the same way that applied
    // event would have.
    const rootBeforeChild = state.issues[root];
    if (!rootBeforeChild) throw new Error("Root issue is missing from test state");
    rootBeforeChild.children.push(child);
    state.issues[child] = {
      key: child,
      title: "Child",
      parent: root,
      status: "todo",
      children: [],
    };

    const released = await json("/legion/v1/waves/release", {
      tree: root,
      issues: [child],
      ...architect,
    });
    expect(released.body).toEqual({ released: [child] });
    expect(statusWrites).toEqual([{ issue: child, status: "todo" }]);
    expect(publications).toEqual([]);
    const releasedAgain = await json("/legion/v1/waves/release", {
      tree: root,
      issues: [child],
      ...architect,
    });
    expect(releasedAgain.body).toEqual({ released: [child] });
    expect(statusWrites).toEqual([
      { issue: child, status: "todo" },
      { issue: child, status: "todo" },
    ]);

    const rootIssue = state.issues[root];
    if (!rootIssue) throw new Error("Root issue is missing from test state");
    rootIssue.status = "done";
    const unauthenticatedExit = await json("/legion/v1/process/exit", {
      tree: root,
      generation: 3,
    });
    expect(unauthenticatedExit.response.status).toBe(400);
    expect(releaseSlots).toEqual([]);
    const exited = await json("/legion/v1/process/exit", {
      tree: root,
      generation: 3,
      ...architect,
    });
    expect(exited.response.status).toBe(200);
    expect(releaseSlots).toEqual([root]);
    expect(state.trees[root]?.status).toBe("closed");
    expect(closedTrees).toEqual([root]);
  });
  it("requires a daemon-minted single-use boot nonce before root registration", async () => {
    await start();
    const input = {
      tree: root,
      generation: 3,
      rootSessionId: "ses_root",
      agentId: "root-agent",
      ompSessionFile: "/tmp/root.json",
    };

    expect((await json("/legion/v1/process/started", input)).response.status).toBe(400);
    const bootToken = await api?.mintBootToken(root, 3);
    if (!bootToken) throw new Error("boot nonce was not minted");
    expect(
      (
        await json("/legion/v1/process/started", {
          ...input,
          bootToken: "wrong-boot-token",
        })
      ).response.status
    ).toBe(403);
    expect(
      (
        await json("/legion/v1/process/started", {
          ...input,
          bootToken,
        })
      ).response.status
    ).toBe(200);
    expect(
      (
        await json("/legion/v1/process/started", {
          ...input,
          bootToken,
        })
      ).response.status
    ).toBe(403);
  });
  it("emits root catch-up only after the architect has confirmed readiness", async () => {
    const treeReady: IssueKey[] = [];
    await start({
      onTreeReady: async (tree) => {
        treeReady.push(tree);
      },
    });
    const bootToken = await api?.mintBootToken(root, 3);
    if (!bootToken) throw new Error("boot nonce was not minted");
    const started = await json<{ secret: string }>("/legion/v1/process/started", {
      tree: root,
      generation: 3,
      rootSessionId: "ses_root",
      bootToken,
      agentId: "root-agent",
      ompSessionFile: "/tmp/root.json",
    });

    expect(treeReady).toEqual([]);
    expect(
      (
        await json("/legion/v1/process/ready", {
          tree: root,
          sessionId: "ses_root",
          secret: started.body.secret,
          generation: 3,
        })
      ).response.status
    ).toBe(200);
    expect(treeReady).toEqual([root]);
    expect(confirmRootReadyCalls).toEqual([{ tree: root, generation: 3 }]);
  });

  it("rejects a stale generation at /process/ready with 409, never confirming or cancelling a newer deadline", async () => {
    await start();
    const bootToken = await api?.mintBootToken(root, 3);
    if (!bootToken) throw new Error("boot nonce was not minted");
    const started = await json<{ secret: string }>("/legion/v1/process/started", {
      tree: root,
      generation: 3,
      rootSessionId: "ses_root",
      bootToken,
      agentId: "root-agent",
      ompSessionFile: "/tmp/root.json",
    });
    expect(started.response.status).toBe(200);

    // A park-then-re-admit started a newer generation for this tree while this exact session's
    // capability (minted against generation 3) is still valid -- its own stale ready must never
    // confirm or cancel whatever deadline the newer generation has armed.
    const treeState = state.trees[root];
    if (!treeState) throw new Error("test root tree is missing");
    treeState.generation = 4;

    const stale = await json("/legion/v1/process/ready", {
      tree: root,
      sessionId: "ses_root",
      secret: started.body.secret,
      generation: 3,
    });
    expect(stale.response.status).toBe(409);
    expect(confirmRootReadyCalls).toEqual([]);
  });

  it("persists the ready confirmation before responding to /process/ready", async () => {
    let saves = 0;
    await start({
      saveState: async () => {
        saves += 1;
      },
    });
    const bootToken = await api?.mintBootToken(root, 3);
    if (!bootToken) throw new Error("boot nonce was not minted");
    const started = await json<{ secret: string }>("/legion/v1/process/started", {
      tree: root,
      generation: 3,
      rootSessionId: "ses_root",
      bootToken,
      agentId: "root-agent",
      ompSessionFile: "/tmp/root.json",
    });
    expect(started.response.status).toBe(200);
    const savesAfterStarted = saves;

    const ready = await json("/legion/v1/process/ready", {
      tree: root,
      sessionId: "ses_root",
      secret: started.body.secret,
      generation: 3,
    });
    expect(ready.response.status).toBe(200);
    expect(saves).toBeGreaterThan(savesAfterStarted);
  });

  it("responds to process/ready before the architect's own shim connects, delivering the connect afterward", async () => {
    const shimGate = Promise.withResolvers<void>();
    let connected: Promise<void> | undefined;
    await start({
      markTreeReadyImpl: (tree) => {
        connected = (async () => {
          await shimGate.promise;
          treeReadyConnected.push(tree);
        })();
        return connected;
      },
    });
    const bootToken = await api?.mintBootToken(root, 3);
    if (!bootToken) throw new Error("boot nonce was not minted");
    const started = await json<{ secret: string }>("/legion/v1/process/started", {
      tree: root,
      generation: 3,
      rootSessionId: "ses_root",
      bootToken,
      agentId: "root-agent",
      ompSessionFile: "/tmp/root.json",
    });

    // The response returns even though the fake shim connect below is still gated shut -- the
    // architect's own bootstrap (the caller of this exact request) must never be blocked on its
    // own shim answering, or every root spawn would deadlock under load.
    const ready = await json("/legion/v1/process/ready", {
      tree: root,
      sessionId: "ses_root",
      secret: started.body.secret,
      generation: 3,
    });
    expect(ready.response.status).toBe(200);
    expect(treeReadyConnected).toEqual([]);

    shimGate.resolve();
    await connected;
    expect(treeReadyConnected).toEqual([root]);
  });

  it("escalates, mints provisioning credentials, and redacts secrets from state", async () => {
    await start();
    const bootToken = await api?.mintBootToken(root, 3);
    if (!bootToken) throw new Error("boot nonce was not minted");
    const started = await json<{ secret: string }>("/legion/v1/process/started", {
      tree: root,
      generation: 3,
      rootSessionId: "ses_root",
      bootToken,
      agentId: "root-agent",
      ompSessionFile: "/tmp/root.json",
    });
    expect(started.response.status).toBe(200);
    const architect = { sessionId: "ses_root", secret: started.body.secret };
    const rootIssue = state.issues[root];
    if (!rootIssue) throw new Error("Root issue is missing from test state");
    rootIssue.children.push(child);
    state.issues[child] = {
      key: child,
      title: "Child",
      parent: root,
      status: "in_progress",
      children: [],
    };

    expect(
      (
        await json("/legion/v1/escalate", {
          tree: root,
          kind: "capacity",
          context: { blocked: true },
          ...architect,
        })
      ).response.status
    ).toBe(200);
    expect(publications.at(-1)).toEqual({
      topic: "notifications.role.legion-omp-controller",
      payload: JSON.stringify({
        type: "escalate",
        tree: root,
        kind: "capacity",
        context: { blocked: true },
      }),
    });

    const provisioningCredential = await json<{ token: string }>(
      "/legion/v1/provisioning-credential",
      {
        tree: root,
        issue: child,
        ...architect,
      }
    );
    expect(provisioningCredential.response.status).toBe(200);
    expect(provisioningCredential.body).toEqual({ token: "minted-implement-acme" });

    const unauthenticatedReady = await json("/legion/v1/controller/ready", {});
    expect(unauthenticatedReady.response.status).toBe(400);
    const missingSessionReady = await json("/legion/v1/controller/ready", {
      secret: controllerSecret,
    });
    expect(missingSessionReady.response.status).toBe(400);

    expect(
      (
        await json("/legion/v1/controller/ready", {
          secret: controllerSecret,
          sessionId: "ses_controller",
        })
      ).response.status
    ).toBe(200);
    expect(state.roles[controllerToken(state.project)]).toEqual({
      role: "controller",
      sessionId: "ses_controller",
    });
    expect(
      (
        await json("/legion/v1/controller/ready", {
          secret: controllerSecret,
          sessionId: "ses_controller",
        })
      ).response.status
    ).toBe(200);

    const stateResponse = await request("/legion/v1/state");
    const stateJson = await stateResponse.text();
    expect(stateJson).not.toContain("minted-");
    expect(stateJson).not.toContain("controllerCapabilityHash");
  });

  it("projects redacted durable state for GET /legion/v1/state: issues, trees, admission, and roles present, every secret/hash/token/grant key absent", async () => {
    state.issues[root].lastAppliedSeq = 7;
    state.trees[root].readyConfirmedAt = now;
    state.trees[root].locator = {
      tmuxSession: "legion-omp",
      tmuxWindowId: "@1",
      tmuxPaneId: "%1",
      socketPath: "/tmp/legion/architect.sock",
      ompSessionFile: "/tmp/root.jsonl",
    };
    state.controllerLocator = {
      tmuxSession: "legion-omp",
      tmuxWindowId: "@0",
      tmuxPaneId: "%0",
      socketPath: "/tmp/legion/controller.sock",
    };
    state.gates[root] = { designAskId: "ask-1", designApproved: "ask-1" };
    state.pendingStatusWrites[child] = { status: "in_progress", statusAtRecord: "todo" };
    state.controllerPendingNotices.push({ payloadJson: '{"type":"escalate"}', eventId: "evt-1" });
    state.roles[controllerToken(state.project)] = {
      role: "controller",
      sessionId: "ses_controller",
    };
    state.roles[roleToken(state.project, root, "implementer")] = {
      issue: root,
      role: "implementer",
      sessionId: "ses_implementer",
      generation: 2,
      readyConfirmedAt: now,
      launchFailures: 1,
      bootTokenHash: secretHash("boot-secret").toString("hex"),
      resumeSessionFile: "/tmp/resume.jsonl",
      expectedSessionId: "ses_implementer",
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@2",
        tmuxPaneId: "%2",
        socketPath: "/tmp/legion/implementer.sock",
        ompSessionFile: "/tmp/implementer.jsonl",
      },
    };
    state.spawnCapabilities[spawnCapabilityKey("boot-secret")] = {
      tree: root,
      issue: root,
      role: "architect",
    };
    state.controllerCapabilityHash = secretHash("controller-secret").toString("hex");

    await start();
    const stateResponse = await request("/legion/v1/state");
    expect(stateResponse.status).toBe(200);
    const body = (await stateResponse.json()) as Record<string, unknown>;

    expect(body.issues).toMatchObject({
      [root]: {
        key: root,
        title: "Root",
        status: "in_progress",
        children: [],
        lastAppliedSeq: 7,
      },
    });
    expect(body.trees).toMatchObject({
      [root]: {
        status: "queued",
        generation: 3,
        launchFailures: 0,
        readyConfirmedAt: now,
        locator: {
          tmuxSession: "legion-omp",
          tmuxWindowId: "@1",
          tmuxPaneId: "%1",
          ompSessionFile: "/tmp/root.jsonl",
        },
      },
    });
    expect(body.admission).toEqual({ cap: 2, active: [], queue: [] });
    expect(body.gates).toEqual({ [root]: { designAskId: "ask-1", designApproved: "ask-1" } });
    expect(body.controllerLocator).toEqual({
      tmuxSession: "legion-omp",
      tmuxWindowId: "@0",
      tmuxPaneId: "%0",
    });
    expect(body.roles).toMatchObject({
      [controllerToken(state.project)]: { role: "controller", sessionId: "ses_controller" },
      [roleToken(state.project, root, "implementer")]: {
        role: "implementer",
        issue: root,
        generation: 2,
        sessionId: "ses_implementer",
        readyConfirmedAt: now,
        launchFailures: 1,
        locator: { tmuxSession: "legion-omp", tmuxWindowId: "@2", tmuxPaneId: "%2" },
      },
    });
    expect(body.controllerPendingNotices).toBe(1);
    expect(body.pendingStatusWrites).toEqual([child]);

    // Every locator in the response is a plain tmux/window triple: no `socketPath` survived the
    // projection anywhere (tree, controller, or role locators).
    expect(JSON.stringify(body)).not.toContain("socketPath");

    const leakedKeys: string[] = [];
    const SECRET_KEY_PATTERN = /secret|hash|token|grant/i;
    const walk = (value: unknown): void => {
      if (Array.isArray(value)) {
        for (const entry of value) walk(entry);
        return;
      }
      if (value === null || typeof value !== "object") return;
      for (const [key, nested] of Object.entries(value)) {
        if (SECRET_KEY_PATTERN.test(key)) leakedKeys.push(key);
        walk(nested);
      }
    };
    walk(body);
    expect(leakedKeys).toEqual([]);
  });

  it("responds to controller/ready before its own shim connects, delivering the connect afterward", async () => {
    const shimGate = Promise.withResolvers<void>();
    let connected: Promise<void> | undefined;
    await start({
      markControllerReadyImpl: () => {
        connected = (async () => {
          await shimGate.promise;
          controllerConnected = true;
        })();
        return connected;
      },
    });

    // Same shape as /process/ready: the response returns before the (best-effort)
    // markControllerReady connect below settles, so a slow/failing shim connect can never add
    // RPC-timeout latency to the controller's own ready call.
    const ready = await json("/legion/v1/controller/ready", {
      secret: controllerSecret,
      sessionId: "ses_controller",
    });
    expect(ready.response.status).toBe(200);
    expect(controllerConnected).toBe(false);

    shimGate.resolve();
    await connected;
    expect(controllerConnected).toBe(true);
  });

  it("retries controller startup redelivery after a failed ready callback", async () => {
    let readyCalls = 0;
    await start({
      onControllerReady: async () => {
        readyCalls += 1;
        if (readyCalls === 1) throw new Error("redelivery failed");
      },
    });

    const first = await json("/legion/v1/controller/ready", {
      secret: controllerSecret,
      sessionId: "ses_controller",
    });
    expect(first.response.status).toBe(500);
    expect(first.body).toEqual({ error: "redelivery failed" });

    const second = await json("/legion/v1/controller/ready", {
      secret: controllerSecret,
      sessionId: "ses_controller",
    });
    expect(second.response.status).toBe(200);
    expect(readyCalls).toBe(2);
  });

  it("requires the owning architect capability for every architect lifecycle write", async () => {
    await start();
    const rootIssue = state.issues[root];
    const otherTree = state.trees[otherRoot];
    if (!rootIssue || !otherTree) throw new Error("test state is missing a root issue");
    rootIssue.children.push(child);
    state.issues[child] = {
      key: child,
      title: "Child",
      parent: root,
      status: "in_progress",
      children: [],
    };
    otherTree.locator = { tmuxSession: "legion-omp", tmuxWindowId: "@9" };

    const rootBootToken = await api?.mintBootToken(root, 3);
    const otherBootToken = await api?.mintBootToken(otherRoot, 1);
    if (!rootBootToken || !otherBootToken) throw new Error("root boot nonces were not minted");
    const rootStarted = await json<{ secret: string }>("/legion/v1/process/started", {
      tree: root,
      generation: 3,
      rootSessionId: "ses_root_architect",
      bootToken: rootBootToken,
      agentId: "root-agent",
      ompSessionFile: "/tmp/root.json",
    });
    const otherStarted = await json<{ secret: string }>("/legion/v1/process/started", {
      tree: otherRoot,
      generation: 1,
      rootSessionId: "ses_other_architect",
      bootToken: otherBootToken,
      agentId: "other-root-agent",
      ompSessionFile: "/tmp/other.json",
    });
    expect(rootStarted.response.status).toBe(200);
    expect(otherStarted.response.status).toBe(200);

    const testerToken = roleToken(state.project, root, "tester");
    state.roles[testerToken] = {
      issue: root,
      role: "tester",
      generation: 1,
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%1",
        socketPath: "/state/workers/tester.sock",
      },
    };
    const workerBootToken = await api?.mintWorkerBootToken(root, root, "tester", 1);
    if (!workerBootToken) throw new Error("worker boot token was not minted");
    const workerPhase = await json<{ secret: string }>("/legion/v1/worker/started", {
      tree: root,
      issue: root,
      role: "tester",
      bootToken: workerBootToken,
      sessionId: "ses_tester",
      agentId: "agent-tester",
      ompSessionFile: "/tmp/tester.json",
    });
    expect(workerPhase.response.status).toBe(200);
    expect(state.roles[testerToken]).toMatchObject({
      issue: root,
      role: "tester",
      sessionId: "ses_tester",
      agentId: "agent-tester",
    });

    const lifecycleWrites: Array<{
      path: string;
      body: Record<string, unknown>;
    }> = [
      {
        path: "/legion/v1/waves/release",
        body: { tree: root, issues: [child] },
      },
      {
        path: "/legion/v1/issues/status",
        body: { tree: root, issue: child, status: "in_progress" },
      },
      {
        path: "/legion/v1/escalate",
        body: { tree: root, kind: "capacity", context: { blocked: true } },
      },
    ];

    for (const write of lifecycleWrites) {
      expect((await json(write.path, write.body)).response.status).toBe(400);
      expect(
        (
          await json(write.path, {
            ...write.body,
            sessionId: "ses_root_architect",
            secret: "wrong-secret",
          })
        ).response.status
      ).toBe(403);
      expect(
        (
          await json(write.path, {
            ...write.body,
            sessionId: "ses_tester",
            secret: workerPhase.body.secret,
          })
        ).response.status
      ).toBe(403);
      expect(
        (
          await json(write.path, {
            ...write.body,
            sessionId: "ses_other_architect",
            secret: otherStarted.body.secret,
          })
        ).response.status
      ).toBe(403);
      expect(
        (
          await json(write.path, {
            ...write.body,
            sessionId: "ses_root_architect",
            secret: rootStarted.body.secret,
          })
        ).response.status
      ).toBe(200);
    }
  });
  it("enforces the issues/status authz matrix between controller triage and architect lifecycle writes", async () => {
    await start();
    const rootIssue = state.issues[root];
    if (!rootIssue) throw new Error("Root issue is missing from test state");
    rootIssue.children.push(child);
    state.issues[child] = {
      key: child,
      title: "Child",
      parent: root,
      status: "todo",
      children: [],
    };
    const bootToken = await api?.mintBootToken(root, 3);
    if (!bootToken) throw new Error("root boot token was not minted");
    const started = await json<{ secret: string }>("/legion/v1/process/started", {
      tree: root,
      generation: 3,
      rootSessionId: "ses_root",
      bootToken,
      agentId: "root-agent",
      ompSessionFile: "/tmp/root.json",
    });
    expect(started.response.status).toBe(200);
    const architect = { sessionId: "ses_root", secret: started.body.secret };

    // Controller capability: triage statuses only, on any issue in the project.
    for (const status of ["todo", "backlog", "icebox"] as const) {
      expect(
        (
          await json("/legion/v1/issues/status", {
            issue: root,
            secret: controllerSecret,
            status,
          })
        ).response.status
      ).toBe(200);
    }
    expect(
      (
        await json("/legion/v1/issues/status", {
          issue: root,
          secret: controllerSecret,
          status: "done",
        })
      ).response.status
    ).toBe(403);
    expect(
      (
        await json("/legion/v1/issues/status", {
          issue: root,
          secret: "wrong-controller-secret",
          status: "todo",
        })
      ).response.status
    ).toBe(403);

    // Architect capability: any lifecycle status, but only within its own tree.
    expect(
      (
        await json("/legion/v1/issues/status", {
          tree: root,
          issue: child,
          status: "done",
          ...architect,
        })
      ).response.status
    ).toBe(200);
    expect(
      (
        await json("/legion/v1/issues/status", {
          tree: root,
          issue: foreign,
          status: "done",
          ...architect,
        })
      ).response.status
    ).toBe(403);

    // Malformed: tree and sessionId must be presented together, never just one of the two.
    expect(
      (
        await json("/legion/v1/issues/status", {
          tree: root,
          issue: child,
          status: "done",
          secret: architect.secret,
        })
      ).response.status
    ).toBe(400);
    expect(
      (
        await json("/legion/v1/issues/status", {
          sessionId: architect.sessionId,
          issue: child,
          status: "done",
          secret: architect.secret,
        })
      ).response.status
    ).toBe(400);
  });

  it("registers a design-gate ask id only for an architect's own tree, keeping any recorded approval", async () => {
    await start();
    const rootIssue = state.issues[root];
    if (!rootIssue) throw new Error("Root issue is missing from test state");
    rootIssue.children.push(child);
    state.issues[child] = {
      key: child,
      title: "Child",
      parent: root,
      status: "in_progress",
      children: [],
    };
    const bootToken = await api?.mintBootToken(root, 3);
    if (!bootToken) throw new Error("root boot token was not minted");
    const started = await json<{ secret: string }>("/legion/v1/process/started", {
      tree: root,
      generation: 3,
      rootSessionId: "ses_root",
      bootToken,
      agentId: "root-agent",
      ompSessionFile: "/tmp/root.json",
    });
    expect(started.response.status).toBe(200);
    const architect = { sessionId: "ses_root", secret: started.body.secret };

    const outOfTree = await json("/legion/v1/gates/register", {
      tree: root,
      issue: foreign,
      askId: "ask-out-of-tree",
      ...architect,
    });
    expect(outOfTree.response.status).toBe(403);
    expect(state.gates[foreign]).toBeUndefined();

    const registered = await json("/legion/v1/gates/register", {
      tree: root,
      issue: child,
      askId: "ask-1",
      ...architect,
    });
    expect(registered.response.status).toBe(200);
    expect(state.gates[child]).toEqual({ designAskId: "ask-1" });

    state.gates[child] = { designAskId: "ask-1", designApproved: "ask-1" };
    const reRegistered = await json("/legion/v1/gates/register", {
      tree: root,
      issue: child,
      askId: "ask-2",
      ...architect,
    });
    expect(reRegistered.response.status).toBe(200);
    expect(state.gates[child]).toEqual({ designAskId: "ask-2", designApproved: "ask-1" });
  });

  it("with gates.design off, registering a gate approves it at once and wakes the architect", async () => {
    const published: Array<{ topic: string; payload: string }> = [];
    await start({
      gates: { design: "off" },
      envoyPublish: async (topic, payload) => {
        published.push({ topic, payload });
      },
    });
    const bootToken = await api?.mintBootToken(root, 3);
    if (!bootToken) throw new Error("root boot token was not minted");
    const started = await json<{ secret: string }>("/legion/v1/process/started", {
      tree: root,
      generation: 3,
      rootSessionId: "ses_root",
      bootToken,
      agentId: "root-agent",
      ompSessionFile: "/tmp/root.json",
    });
    expect(started.response.status).toBe(200);
    const architect = { sessionId: "ses_root", secret: started.body.secret };
    published.length = 0;

    const registered = await json("/legion/v1/gates/register", {
      tree: root,
      issue: root,
      askId: "ask-1",
      ...architect,
    });
    expect(registered.response.status).toBe(200);
    expect(state.gates[root]).toEqual({ designAskId: "ask-1", designApproved: "gate-off" });
    expect(published).toEqual([
      {
        topic: roleTopic(roleToken(state.project, root, "architect")),
        payload: JSON.stringify({ type: "design-approved" }),
      },
    ]);

    // Re-registering an already-approved gate records the new ask id and wakes nobody twice.
    const again = await json("/legion/v1/gates/register", {
      tree: root,
      issue: root,
      askId: "ask-2",
      ...architect,
    });
    expect(again.response.status).toBe(200);
    expect(state.gates[root]).toEqual({ designAskId: "ask-2", designApproved: "gate-off" });
    expect(published).toHaveLength(1);
  });

  it("persists a minted controller capability before controller spawn can proceed", async () => {
    const tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-controller-capability-"));
    const file = path.join(tempDir, "state.json");
    try {
      await start({ saveState: async () => saveState(file, state) });
      const mintedSecret = controllerSecret;
      api?.stop();

      const reloaded = await loadState(file, { project: "omp", cap: 2 });
      await start({ state: reloaded, mintController: false });
      expect(
        (
          await json("/legion/v1/issues/status", {
            issue: root,
            secret: mintedSecret,
            status: "todo",
          })
        ).response.status
      ).toBe(200);
    } finally {
      await rm(tempDir, { recursive: true, force: true });
    }
  });

  it("rejects a worker/started boot token minted for a different role", async () => {
    await start();
    const testerToken = roleToken(state.project, root, "tester");
    state.roles[testerToken] = {
      issue: root,
      role: "tester",
      generation: 1,
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%1",
        socketPath: "/state/workers/tester.sock",
      },
    };
    const reviewerBootToken = await api?.mintWorkerBootToken(root, root, "reviewer", 1);
    if (!reviewerBootToken) throw new Error("worker boot token was not minted");

    const mismatched = await json("/legion/v1/worker/started", {
      tree: root,
      issue: root,
      role: "tester",
      bootToken: reviewerBootToken,
      sessionId: "ses_tester",
      agentId: "agent-tester",
      ompSessionFile: "/tmp/tester.json",
    });

    expect(mismatched.response.status).toBe(403);
  });

  it("reissues a daemon-registered worker session capability and keeps grants short-lived", async () => {
    await start();
    const bootToken = await api?.mintBootToken(root, 3);
    if (!bootToken) throw new Error("boot nonce was not minted");
    const started = await json<{ secret: string }>("/legion/v1/process/started", {
      tree: root,
      generation: 3,
      rootSessionId: "ses_architect",
      bootToken,
      agentId: "root-agent",
      ompSessionFile: "/tmp/root.json",
    });
    expect(started.response.status).toBe(200);
    const testerToken = roleToken(state.project, root, "tester");
    state.roles[testerToken] = {
      issue: root,
      role: "tester",
      generation: 1,
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%1",
        socketPath: "/state/workers/tester.sock",
      },
    };
    const workerBootToken = await api?.mintWorkerBootToken(root, root, "tester", 1);
    if (!workerBootToken) throw new Error("worker boot token was not minted");
    // Mirrors what `mintBootToken`/`mintWorkerBootToken` write in production (the daemon mints
    // this internally; no public route mints one on a client's behalf) -- a worker-role
    // recovery token recorded directly against `spawnCapabilities`, recoverable by
    // `/legion/v1/worker-session` below.
    const spawnToken = "test-tester-recovery-token";
    state.spawnCapabilities[spawnCapabilityKey(spawnToken)] = {
      tree: root,
      issue: root,
      role: "tester",
    };
    const phase = await curlJson<{
      roleToken: string;
      secret: string;
      gitName: string;
      gitEmail: string;
    }>("/legion/v1/worker/started", {
      tree: root,
      issue: root,
      role: "tester",
      bootToken: workerBootToken,
      sessionId: "ses_tester",
      agentId: "agent-tester",
      ompSessionFile: "/tmp/tester.json",
    });
    expect(phase.status).toBe(200);
    expect(phase.body).toEqual({
      roleToken: testerToken,
      secret: expect.any(String),
      gitName: "legion-implement[bot]",
      gitEmail: "42+legion-implement[bot]@users.noreply.github.com",
    });

    const recovered = await curlJson<WorkerSessionResponse>("/legion/v1/worker-session", {
      sessionId: "ses_tester",
      recoveryToken: spawnToken,
    });
    expect(recovered.status).toBe(200);
    expect(recovered.body).toEqual({
      tree: root,
      issue: root,
      role: "tester",
      secret: expect.any(String),
    });
    expect(
      (
        await json("/legion/v1/worker-session", {
          sessionId: "ses_tester",
          recoveryToken: "not-a-daemon-issued-token",
        })
      ).response.status
    ).toBe(403);

    for (const secret of [undefined, "wrong-secret"]) {
      const denied = await json("/legion/v1/grants", {
        tree: root,
        issue: root,
        sessionId: "ses_tester",
        ...(secret === undefined ? {} : { secret }),
      });
      expect(denied.response.status).toBe(secret === undefined ? 400 : 403);
    }

    const grant = await curlJson<GrantResponse>("/legion/v1/grants", {
      tree: root,
      issue: root,
      sessionId: "ses_tester",
      secret: recovered.body.secret,
    });
    expect(grant.status).toBe(200);
    expect(grant.body).toEqual({
      grantId: expect.any(String),
      expiresAt: new Date(now + 60_000).toISOString(),
    });

    const token = await json("/legion/v1/gh-token", {
      grantId: grant.body.grantId,
    });
    expect(token.body).toEqual({
      token: "minted-implement-acme",
      appLogin: "legion-implement[bot]",
    });
    expect(tokenRoles).toEqual(["implement", "implement"]);

    const credential = await curl("/legion/v1/git-credential", {
      grantId: grant.body.grantId,
    });
    expect(credential.status).toBe(200);
    expect(credential.body).toBe("username=x-access-token\npassword=minted-implement-acme");

    now += 60_001;
    const expired = await json("/legion/v1/gh-token", {
      grantId: grant.body.grantId,
    });
    expect(expired.response.status).toBe(403);
  });
  it("revokes an already-minted grant the moment its minting session's capability is revoked, even before it would otherwise expire", async () => {
    await start();
    const bootToken = await api?.mintBootToken(root, 3);
    if (!bootToken) throw new Error("boot nonce was not minted");
    const started = await json<{ secret: string }>("/legion/v1/process/started", {
      tree: root,
      generation: 3,
      rootSessionId: "ses_architect",
      bootToken,
      agentId: "root-agent",
      ompSessionFile: "/tmp/root.json",
    });
    expect(started.response.status).toBe(200);

    const grant = await curlJson<GrantResponse>("/legion/v1/grants", {
      tree: root,
      issue: root,
      sessionId: "ses_architect",
      secret: started.body.secret,
    });
    expect(grant.status).toBe(200);

    // Simulates what `ProcessManager.closeTree`/`retireWorkerLocator`/`removeTreeWindow`
    // actually call the instant a session's process is observed dead or torn down (see
    // `revokeRoleClaim`) - `deleteCapability` must not merely stop future grant mints, it must
    // also invalidate every grant this session already minted, since `resolveGrant` only ever
    // checks expiry, never the capability that minted it.
    api?.revokeSessionCapability("ses_architect");

    const token = await json("/legion/v1/gh-token", {
      grantId: grant.body.grantId,
    });
    expect(token.response.status).toBe(403);
  });
  it("rejects gh-token/git-credential with 403 when the minting session's capability is revoked while the GitHub lease is in flight", async () => {
    const reachedLease = Promise.withResolvers<void>();
    const leaseGate = Promise.withResolvers<void>();
    await start({
      getToken: async (role, owner) => {
        reachedLease.resolve();
        await leaseGate.promise;
        return {
          token: `minted-${role}-${owner}`,
          expiresAt: "2099-01-01T00:00:00.000Z",
          gitIdentity: {
            name: "legion-implement[bot]",
            email: "42+legion-implement[bot]@users.noreply.github.com",
          },
        };
      },
    });
    const bootToken = await api?.mintBootToken(root, 3);
    if (!bootToken) throw new Error("boot nonce was not minted");
    const started = await json<{ secret: string }>("/legion/v1/process/started", {
      tree: root,
      generation: 3,
      rootSessionId: "ses_architect",
      bootToken,
      agentId: "root-agent",
      ompSessionFile: "/tmp/root.json",
    });
    expect(started.response.status).toBe(200);

    const grant = await curlJson<GrantResponse>("/legion/v1/grants", {
      tree: root,
      issue: root,
      sessionId: "ses_architect",
      secret: started.body.secret,
    });
    expect(grant.status).toBe(200);

    const tokenRequest = json("/legion/v1/gh-token", { grantId: grant.body.grantId });
    await reachedLease.promise;

    // Revoked while the GitHub lease call above is still in flight -- the grant was valid when
    // this request started, but must not be honored once its minting session's capability (and
    // every grant it minted) is gone.
    api?.revokeSessionCapability("ses_architect");
    leaseGate.resolve();

    const token = await tokenRequest;
    expect(token.response.status).toBe(403);

    // Same race, same outcome, for the other grant-authenticated route.
    const reachedLease2 = Promise.withResolvers<void>();
    const leaseGate2 = Promise.withResolvers<void>();
    api?.stop();
    await start({
      getToken: async (role, owner) => {
        reachedLease2.resolve();
        await leaseGate2.promise;
        return {
          token: `minted-${role}-${owner}`,
          expiresAt: "2099-01-01T00:00:00.000Z",
          gitIdentity: {
            name: "legion-implement[bot]",
            email: "42+legion-implement[bot]@users.noreply.github.com",
          },
        };
      },
    });
    const bootToken2 = await api?.mintBootToken(root, 3);
    if (!bootToken2) throw new Error("boot nonce was not minted");
    const started2 = await json<{ secret: string }>("/legion/v1/process/started", {
      tree: root,
      generation: 3,
      rootSessionId: "ses_architect",
      bootToken: bootToken2,
      agentId: "root-agent",
      ompSessionFile: "/tmp/root.json",
    });
    expect(started2.response.status).toBe(200);
    const grant2 = await curlJson<GrantResponse>("/legion/v1/grants", {
      tree: root,
      issue: root,
      sessionId: "ses_architect",
      secret: started2.body.secret,
    });
    expect(grant2.status).toBe(200);

    const credentialRequest = json("/legion/v1/git-credential", { grantId: grant2.body.grantId });
    await reachedLease2.promise;
    api?.revokeSessionCapability("ses_architect");
    leaseGate2.resolve();

    const credential = await credentialRequest;
    expect(credential.response.status).toBe(403);
  });
  it("resolves worker/started against a persisted boot-token hash after a restart, rejecting a session that does not match the resumed agent", async () => {
    await start();
    const testerToken = roleToken(state.project, root, "tester");
    const workerBootToken = await api?.mintWorkerBootToken(root, root, "tester", 3, "ses_original");
    if (!workerBootToken) throw new Error("worker boot token was not minted");
    // Simulates what a real `launchWorker` call already persists onto the claim at mint time
    // (see `launchWorker`'s doc comment): the boot token's hash and the session this respawn is
    // expected to resume, durable independent of the in-memory boot-token map.
    state.roles[testerToken] = {
      issue: root,
      role: "tester",
      generation: 3,
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@1",
        tmuxPaneId: "%1",
        socketPath: "/state/workers/tester.sock",
      },
      bootTokenHash: secretHash(workerBootToken).toString("hex"),
      expectedSessionId: "ses_original",
    };

    // Restart: a fresh daemon/API instance means a fresh in-memory boot-token map, so
    // `/worker/started` must fall back to the persisted hash on the claim.
    api?.stop();
    await start({ state });

    const mismatched = await json("/legion/v1/worker/started", {
      tree: root,
      issue: root,
      role: "tester",
      bootToken: workerBootToken,
      sessionId: "ses_different",
      agentId: "agent-tester",
      ompSessionFile: "/tmp/tester.json",
    });
    expect(mismatched.response.status).toBe(409);
    expect((mismatched.body as { error?: string }).error).toBe(
      "Worker respawn must resume the same agent session"
    );
    // Rejected before any mutation: the claim is untouched, so a follow-up attempt with the
    // correct session still works against the same persisted hash.
    const untouchedClaim = state.roles[testerToken];
    expect(
      untouchedClaim && "issue" in untouchedClaim ? untouchedClaim.sessionId : "present"
    ).toBeUndefined();

    const resumed = await json<{ secret: string }>("/legion/v1/worker/started", {
      tree: root,
      issue: root,
      role: "tester",
      bootToken: workerBootToken,
      sessionId: "ses_original",
      agentId: "agent-tester",
      ompSessionFile: "/tmp/tester.json",
    });
    expect(resumed.response.status).toBe(200);
    expect(state.roles[testerToken]).toMatchObject({ sessionId: "ses_original" });
  });
  it("restores a root architect capability from durable transcript backing after a daemon restart", async () => {
    await start();
    const bootToken = await api?.mintBootToken(root, 3);
    if (!bootToken) throw new Error("boot nonce was not minted");
    const started = await json<{ secret: string }>("/legion/v1/process/started", {
      tree: root,
      generation: 3,
      rootSessionId: "ses_root",
      agentId: "root-transcript",
      bootToken,
      ompSessionFile: "/tmp/root-transcript.jsonl",
    });
    expect(started.response.status).toBe(200);

    api?.stop();
    await start({ state });
    const stale = await json("/legion/v1/process/ready", {
      tree: root,
      sessionId: "ses_root",
      secret: started.body.secret,
      generation: 3,
    });
    expect(stale.response.status).toBe(403);
    const recovered = await json<WorkerSessionResponse>("/legion/v1/worker-session", {
      sessionId: "ses_root",
      recoveryToken: bootToken,
    });
    expect(recovered.response.status).toBe(200);
    expect(recovered.body).toEqual({
      tree: root,
      issue: root,
      role: "architect",
      secret: expect.any(String),
    });
    expect(
      (
        await json("/legion/v1/process/ready", {
          tree: root,
          sessionId: "ses_root",
          secret: recovered.body.secret,
          generation: 3,
        })
      ).response.status
    ).toBe(200);
  });
  it("registers a worker session and phase from a valid worker boot token", async () => {
    await start();
    const token = roleToken(state.project, root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%1",
        socketPath: "/state/workers/tester.sock",
      },
    };
    const bootToken = await api?.mintWorkerBootToken(root, root, "tester", 1);
    if (!bootToken) throw new Error("worker boot token was not minted");

    const started = await json<{
      roleToken: string;
      secret: string;
      gitName: string;
      gitEmail: string;
    }>("/legion/v1/worker/started", {
      tree: root,
      issue: root,
      role: "tester",
      bootToken,
      sessionId: "ses_tester",
      agentId: "agt_tester",
      ompSessionFile: "/tmp/tester.json",
    });

    expect(started.response.status).toBe(200);
    expect(started.body.roleToken).toBe(token);
    expect(started.body.gitName).toBe("legion-implement[bot]");
    expect(state.roles[token]).toMatchObject({
      sessionId: "ses_tester",
      agentId: "agt_tester",
      locator: { ompSessionFile: "/tmp/tester.json" },
    });
    expect(state.phases[root]).toEqual({ phase: "tester", sessionId: "ses_tester" });
  });

  it("rejects worker/started with a fresh 409 when the claim's generation changes while its GitHub lease is in flight", async () => {
    const reachedLease = Promise.withResolvers<void>();
    const leaseGate = Promise.withResolvers<void>();
    await start({
      getToken: async (role, owner) => {
        reachedLease.resolve();
        await leaseGate.promise;
        return {
          token: `minted-${role}-${owner}`,
          expiresAt: "2099-01-01T00:00:00.000Z",
          gitIdentity: {
            name: "legion-implement[bot]",
            email: "42+legion-implement[bot]@users.noreply.github.com",
          },
        };
      },
    });
    const token = roleToken(state.project, root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%1",
        socketPath: "/state/workers/tester.sock",
      },
    };
    const bootToken = await api?.mintWorkerBootToken(root, root, "tester", 1);
    if (!bootToken) throw new Error("worker boot token was not minted");
    const body = {
      tree: root,
      issue: root,
      role: "tester",
      bootToken,
      sessionId: "ses_tester",
      agentId: "agt_tester",
      ompSessionFile: "/tmp/tester.json",
    };

    const requestPromise = request("/legion/v1/worker/started", body);

    // Waits for the handler to have actually reached (and blocked inside) the GitHub lease call
    // -- confirming its own two earlier identity checks (the fast-fail before any lock, and the
    // re-check just inside the per-token lock) already ran and passed against generation 1 --
    // before mutating anything. Without this synchronization, mutating state immediately after
    // firing the request risks the mutation landing before either earlier check even runs,
    // which would prove nothing about the *post-lease* re-check specifically.
    await reachedLease.promise;

    // A concurrent respawn bumps this exact claim onto a new generation while the request above
    // is blocked awaiting its GitHub lease, inside `mutateLiveRoleClaim`'s own per-token critical
    // section. The re-validation after that lease await must see THIS identity, not the
    // generation-1 view the two earlier checks already passed.
    const staleClaim = state.roles[token];
    if (!staleClaim || !("issue" in staleClaim)) throw new Error("claim missing before lease");
    staleClaim.generation = 2;

    leaseGate.resolve();
    const response = await requestPromise;
    const responseBody = (await response.json()) as { error: string };

    expect(response.status).toBe(409);
    expect(responseBody.error).toBe("Stale worker generation");
    // Never wrote its stale (generation-1) view over the fresher generation-2 claim.
    expect(state.roles[token]).toMatchObject({ generation: 2 });
    expect(state.phases[root]).toBeUndefined();
  });

  it("rejects a worker boot token that has already been consumed", async () => {
    await start();
    const token = roleToken(state.project, root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%1",
        socketPath: "/state/workers/tester.sock",
      },
    };
    const bootToken = await api?.mintWorkerBootToken(root, root, "tester", 1);
    if (!bootToken) throw new Error("worker boot token was not minted");
    const body = {
      tree: root,
      issue: root,
      role: "tester",
      bootToken,
      sessionId: "ses_tester",
      agentId: "agt_tester",
      ompSessionFile: "/tmp/tester.json",
    };

    expect((await json("/legion/v1/worker/started", body)).response.status).toBe(200);
    const replay = await json("/legion/v1/worker/started", { ...body, sessionId: "ses_tester_2" });
    expect(replay.response.status).toBe(403);
  });

  it("accepts a same-session worker/started replay as idempotent, reissuing a secret", async () => {
    await start();
    const token = roleToken(state.project, root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%1",
        socketPath: "/state/workers/tester.sock",
      },
    };
    const bootToken = await api?.mintWorkerBootToken(root, root, "tester", 1);
    if (!bootToken) throw new Error("worker boot token was not minted");
    const body = {
      tree: root,
      issue: root,
      role: "tester",
      bootToken,
      sessionId: "ses_tester",
      agentId: "agt_tester",
      ompSessionFile: "/tmp/tester.json",
    };

    const first = await json<{ secret: string; roleToken: string }>(
      "/legion/v1/worker/started",
      body
    );
    expect(first.response.status).toBe(200);
    const retry = await json<{ secret: string; roleToken: string }>(
      "/legion/v1/worker/started",
      body
    );
    expect(retry.response.status).toBe(200);
    expect(retry.body.roleToken).toBe(token);
    expect(state.roles[token]).toMatchObject({ sessionId: "ses_tester", agentId: "agt_tester" });
  });

  it("preserves a claim's accumulated launchFailures across worker/started registration, since only a durable ready confirmation resets it", async () => {
    await start();
    const token = roleToken(state.project, root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      launchFailures: 2,
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%1",
        socketPath: "/state/workers/tester.sock",
      },
    };
    const bootToken = await api?.mintWorkerBootToken(root, root, "tester", 1);
    if (!bootToken) throw new Error("worker boot token was not minted");

    const started = await json<{ secret: string }>("/legion/v1/worker/started", {
      tree: root,
      issue: root,
      role: "tester",
      bootToken,
      sessionId: "ses_tester",
      agentId: "agt_tester",
      ompSessionFile: "/tmp/tester.json",
    });

    expect(started.response.status).toBe(200);
    expect(state.roles[token]).toMatchObject({ launchFailures: 2, sessionId: "ses_tester" });
  });

  it("retries cleanly after a transient tokenForIssue failure, leaving exactly one claim", async () => {
    let calls = 0;
    await start({
      getToken: async (role, owner) => {
        calls += 1;
        if (calls === 1) throw new Error("GitHub token service unavailable");
        return {
          token: `minted-${role}-${owner}`,
          expiresAt: "2099-01-01T00:00:00.000Z",
          gitIdentity: {
            name: "legion-implement[bot]",
            email: "42+legion-implement[bot]@users.noreply.github.com",
          },
        };
      },
    });
    const token = roleToken(state.project, root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%1",
        socketPath: "/state/workers/tester.sock",
      },
    };
    const bootToken = await api?.mintWorkerBootToken(root, root, "tester", 1);
    if (!bootToken) throw new Error("worker boot token was not minted");
    const body = {
      tree: root,
      issue: root,
      role: "tester",
      bootToken,
      sessionId: "ses_tester",
      agentId: "agt_tester",
      ompSessionFile: "/tmp/tester.json",
    };

    const first = await request("/legion/v1/worker/started", body);
    expect(first.status).toBe(500);
    expect(state.roles[token]).toEqual({
      issue: root,
      role: "tester",
      generation: 1,
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%1",
        socketPath: "/state/workers/tester.sock",
      },
    });

    const retry = await json<{ secret: string; roleToken: string }>(
      "/legion/v1/worker/started",
      body
    );
    expect(retry.response.status).toBe(200);
    expect(retry.body.secret).toBeString();
    expect(retry.body.roleToken).toBe(token);
    expect(Object.keys(state.roles)).toEqual([token]);
    expect(state.roles[token]).toMatchObject({ sessionId: "ses_tester", agentId: "agt_tester" });
  });

  it("rolls back the claim and phase mutations when the state save fails, leaving a clean retry", async () => {
    let saveCalls = 0;
    await start({
      mintController: false,
      saveState: async () => {
        saveCalls += 1;
        if (saveCalls === 1) throw new Error("disk full");
      },
    });
    const token = roleToken(state.project, root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%1",
        socketPath: "/state/workers/tester.sock",
      },
    };
    const bootToken = await api?.mintWorkerBootToken(root, root, "tester", 1);
    if (!bootToken) throw new Error("worker boot token was not minted");
    const body = {
      tree: root,
      issue: root,
      role: "tester",
      bootToken,
      sessionId: "ses_tester",
      agentId: "agt_tester",
      ompSessionFile: "/tmp/tester.json",
    };

    const first = await request("/legion/v1/worker/started", body);
    expect(first.status).toBe(500);
    expect(state.roles[token]).toEqual({
      issue: root,
      role: "tester",
      generation: 1,
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%1",
        socketPath: "/state/workers/tester.sock",
      },
    });
    expect(state.phases[root]).toBeUndefined();

    const retry = await json<{ secret: string; roleToken: string }>(
      "/legion/v1/worker/started",
      body
    );
    expect(retry.response.status).toBe(200);
    expect(state.roles[token]).toMatchObject({ sessionId: "ses_tester", agentId: "agt_tester" });
    expect(state.phases[root]).toEqual({ phase: "tester", sessionId: "ses_tester" });
  });

  it("rejects a worker/started sessionId that differs from the boot token's expected respawn session", async () => {
    await start();
    const token = roleToken(state.project, root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      sessionId: "ses_original",
      generation: 1,
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%1",
        socketPath: "/state/workers/tester.sock",
      },
    };
    const bootToken = await api?.mintWorkerBootToken(root, root, "tester", 1, "ses_original");
    if (!bootToken) throw new Error("worker boot token was not minted");

    const mismatched = await json("/legion/v1/worker/started", {
      tree: root,
      issue: root,
      role: "tester",
      bootToken,
      sessionId: "ses_different",
      agentId: "agt_tester",
      ompSessionFile: "/tmp/tester.json",
    });
    expect(mismatched.response.status).toBe(409);

    const resumed = await json<{ secret: string }>("/legion/v1/worker/started", {
      tree: root,
      issue: root,
      role: "tester",
      bootToken,
      sessionId: "ses_original",
      agentId: "agt_tester",
      ompSessionFile: "/tmp/tester.json",
    });
    expect(resumed.response.status).toBe(200);
  });

  it("recovers a phase worker's session capability from its persisted boot token after a daemon restart", async () => {
    await start();
    const token = roleToken(state.project, root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%1",
        socketPath: "/state/workers/tester.sock",
      },
    };
    const bootToken = await api?.mintWorkerBootToken(root, root, "tester", 1);
    if (!bootToken) throw new Error("worker boot token was not minted");
    const started = await json<{ secret: string }>("/legion/v1/worker/started", {
      tree: root,
      issue: root,
      role: "tester",
      bootToken,
      sessionId: "ses_tester",
      agentId: "agt_tester",
      ompSessionFile: "/tmp/tester.json",
    });
    expect(started.response.status).toBe(200);

    api?.stop();
    await start({ state });
    const stale = await json("/legion/v1/worker/ready", {
      tree: root,
      issue: root,
      role: "tester",
      sessionId: "ses_tester",
      generation: 1,
      secret: started.body.secret,
    });
    expect(stale.response.status).toBe(403);

    const recovered = await json<WorkerSessionResponse>("/legion/v1/worker-session", {
      sessionId: "ses_tester",
      recoveryToken: bootToken,
    });
    expect(recovered.response.status).toBe(200);
    expect(recovered.body).toEqual({
      tree: root,
      issue: root,
      role: "tester",
      secret: expect.any(String),
    });
    expect(
      (
        await json("/legion/v1/worker/ready", {
          tree: root,
          issue: root,
          role: "tester",
          sessionId: "ses_tester",
          generation: 1,
          secret: recovered.body.secret,
        })
      ).response.status
    ).toBe(200);

    const wrongToken = await json("/legion/v1/worker-session", {
      sessionId: "ses_tester",
      recoveryToken: "not-a-daemon-issued-token",
    });
    expect(wrongToken.response.status).toBe(403);
  });

  it("delivers a worker's pending assignment through the process manager on worker/ready", async () => {
    await start();
    const token = roleToken(state.project, root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      pendingAssignment: "verify #41",
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%1",
        socketPath: "/state/workers/tester.sock",
      },
    };
    const bootToken = await api?.mintWorkerBootToken(root, root, "tester", 1);
    if (!bootToken) throw new Error("worker boot token was not minted");
    const started = await json<{ secret: string }>("/legion/v1/worker/started", {
      tree: root,
      issue: root,
      role: "tester",
      bootToken,
      sessionId: "ses_tester",
      agentId: "agt_tester",
      ompSessionFile: "/tmp/tester.json",
    });

    const ready = await json("/legion/v1/worker/ready", {
      tree: root,
      issue: root,
      role: "tester",
      sessionId: "ses_tester",
      generation: 1,
      secret: started.body.secret,
    });

    expect(ready.response.status).toBe(200);
    expect(workerReadyCalls).toEqual([
      { issue: root, role: "tester", sessionId: "ses_tester", generation: 1 },
    ]);
  });

  it("responds to worker/ready before the calling worker's own shim connects, delivering the prompt afterward", async () => {
    const shimGate = Promise.withResolvers<void>();
    let connected: Promise<void> | undefined;
    await start({
      workerReadyImpl: () => {
        connected = (async () => {
          await shimGate.promise;
          workerReadyConnected = true;
        })();
        return connected;
      },
    });
    const token = roleToken(state.project, root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      pendingAssignment: "verify #41",
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%1",
        socketPath: "/state/workers/tester.sock",
      },
    };
    const bootToken = await api?.mintWorkerBootToken(root, root, "tester", 1);
    if (!bootToken) throw new Error("worker boot token was not minted");
    const started = await json<{ secret: string }>("/legion/v1/worker/started", {
      tree: root,
      issue: root,
      role: "tester",
      bootToken,
      sessionId: "ses_tester",
      agentId: "agt_tester",
      ompSessionFile: "/tmp/tester.json",
    });

    // Same shape as /process/ready: the calling worker's own bootstrap is blocked on this
    // exact HTTP response, so the daemon must never await connecting to (and prompting) that
    // same worker's own shim socket before responding -- the same deadlock, one level
    // down from the root architect.
    const ready = await json("/legion/v1/worker/ready", {
      tree: root,
      issue: root,
      role: "tester",
      sessionId: "ses_tester",
      generation: 1,
      secret: started.body.secret,
    });
    expect(ready.response.status).toBe(200);
    expect(workerReadyConnected).toBe(false);

    shimGate.resolve();
    await connected;
    expect(workerReadyConnected).toBe(true);
  });

  it("rejects worker/ready when the session's role does not match the requested role", async () => {
    await start();
    const token = roleToken(state.project, root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%1",
        socketPath: "/state/workers/tester.sock",
      },
    };
    const bootToken = await api?.mintWorkerBootToken(root, root, "tester", 1);
    if (!bootToken) throw new Error("worker boot token was not minted");
    const started = await json<{ secret: string }>("/legion/v1/worker/started", {
      tree: root,
      issue: root,
      role: "tester",
      bootToken,
      sessionId: "ses_tester",
      agentId: "agt_tester",
      ompSessionFile: "/tmp/tester.json",
    });

    const ready = await json("/legion/v1/worker/ready", {
      tree: root,
      issue: root,
      role: "reviewer",
      sessionId: "ses_tester",
      generation: 1,
      secret: started.body.secret,
    });

    expect(ready.response.status).toBe(403);
    expect(workerReadyCalls).toEqual([]);
  });

  it("spawns a worker through the architect capability and forwards to the process manager", async () => {
    await start();
    const bootToken = await api?.mintBootToken(root, 3);
    if (!bootToken) throw new Error("root boot token was not minted");
    const started = await json<{ secret: string }>("/legion/v1/process/started", {
      tree: root,
      generation: 3,
      rootSessionId: "ses_root",
      bootToken,
      agentId: "root-agent",
      ompSessionFile: "/tmp/root.json",
    });
    expect(started.response.status).toBe(200);

    const spawn = await json<{ status: string; roleToken: string }>("/legion/v1/worker/spawn", {
      tree: root,
      issue: root,
      sessionId: "ses_root",
      secret: started.body.secret,
      role: "planner",
      task: "plan #1",
    });

    expect(spawn.response.status).toBe(200);
    expect(spawn.body).toEqual({
      status: "spawned",
      roleToken: roleToken(state.project, root, "planner"),
    });
    expect(spawnedWorkers).toEqual([{ tree: root, issue: root, role: "planner", task: "plan #1" }]);
  });

  it("refuses spawn_worker for a non-architect session capability", async () => {
    await start();
    const token = roleToken(state.project, root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%1",
        socketPath: "/state/workers/tester.sock",
      },
    };
    const bootToken = await api?.mintWorkerBootToken(root, root, "tester", 1);
    if (!bootToken) throw new Error("worker boot token was not minted");
    const started = await json<{ secret: string }>("/legion/v1/worker/started", {
      tree: root,
      issue: root,
      role: "tester",
      bootToken,
      sessionId: "ses_tester",
      agentId: "agt_tester",
      ompSessionFile: "/tmp/tester.json",
    });

    const spawn = await json("/legion/v1/worker/spawn", {
      tree: root,
      issue: root,
      sessionId: "ses_tester",
      secret: started.body.secret,
      role: "planner",
      task: "plan #1",
    });

    expect(spawn.response.status).toBe(403);
    expect(spawnedWorkers).toEqual([]);
  });

  it("rejects spawn_worker for the root issue's own architect role", async () => {
    await start();
    const bootToken = await api?.mintBootToken(root, 3);
    if (!bootToken) throw new Error("root boot token was not minted");
    const started = await json<{ secret: string }>("/legion/v1/process/started", {
      tree: root,
      generation: 3,
      rootSessionId: "ses_root",
      bootToken,
      agentId: "root-agent",
      ompSessionFile: "/tmp/root.json",
    });
    expect(started.response.status).toBe(200);

    const spawn = await json("/legion/v1/worker/spawn", {
      tree: root,
      issue: root,
      sessionId: "ses_root",
      secret: started.body.secret,
      role: "architect",
      task: "reboot",
    });

    expect(spawn.response.status).toBe(400);
    expect(spawnedWorkers).toEqual([]);
  });

  it("surfaces a closing tree's spawn_worker rejection as HTTP 409", async () => {
    await start({
      spawnWorkerImpl: async (tree) => {
        throw new TreeClosingError(tree);
      },
    });
    const bootToken = await api?.mintBootToken(root, 3);
    if (!bootToken) throw new Error("root boot token was not minted");
    const started = await json<{ secret: string }>("/legion/v1/process/started", {
      tree: root,
      generation: 3,
      rootSessionId: "ses_root",
      bootToken,
      agentId: "root-agent",
      ompSessionFile: "/tmp/root.json",
    });
    expect(started.response.status).toBe(200);

    const spawn = await json<{ error: string }>("/legion/v1/worker/spawn", {
      tree: root,
      issue: root,
      sessionId: "ses_root",
      secret: started.body.secret,
      role: "planner",
      task: "plan #1",
    });

    expect(spawn.response.status).toBe(409);
    expect(spawn.body.error).toContain(root);
    expect(spawnedWorkers).toEqual([]);
  });

  async function mintGrant(issue: IssueKey, sessionId: string, secret: string): Promise<string> {
    const grant = await json<{ grantId: string }>("/legion/v1/grants", {
      tree: root,
      issue,
      sessionId,
      secret,
    });
    if (grant.response.status !== 200) {
      throw new Error(`grant was not minted: ${grant.response.status}`);
    }
    return grant.body.grantId;
  }

  it("publishes phase-complete to the tree's architect, clears the phase, and keeps the worker's role claim", async () => {
    await start();
    const token = roleToken(state.project, root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%1",
        socketPath: "/state/workers/tester.sock",
      },
    };
    const bootToken = await api?.mintWorkerBootToken(root, root, "tester", 1);
    if (!bootToken) throw new Error("worker boot token was not minted");
    const started = await json<{ secret: string }>("/legion/v1/worker/started", {
      tree: root,
      issue: root,
      role: "tester",
      bootToken,
      sessionId: "ses_tester",
      agentId: "agt_tester",
      ompSessionFile: "/tmp/tester.json",
    });
    expect(started.response.status).toBe(200);
    state.phases[root] = { phase: "tester", sessionId: "ses_tester" };
    const grantId = await mintGrant(root, "ses_tester", started.body.secret);

    const complete = await json("/legion/v1/phase/complete", {
      grantId,
      summary: "Verified the acceptance criteria",
    });

    expect(complete.response.status).toBe(200);
    expect(publications).toContainEqual({
      topic: roleTopic(roleToken(state.project, root, "architect")),
      payload: JSON.stringify({
        type: "phase-complete",
        issue: root,
        role: "tester",
        summary: "Verified the acceptance criteria",
      }),
    });
    expect(state.phases[root]).toBeUndefined();
    expect(state.roles[token]).toMatchObject({ issue: root, role: "tester", generation: 1 });
  });

  it("publishes phase-complete for a child issue to the ROOT's architect, not the child's own", async () => {
    await start();
    state.issues[root].children.push(child);
    state.issues[child] = {
      key: child,
      title: "Child",
      parent: root,
      status: "in_progress",
      children: [],
    };
    const token = roleToken(state.project, child, "implementer");
    state.roles[token] = {
      issue: child,
      role: "implementer",
      generation: 1,
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@43",
        tmuxPaneId: "%2",
        socketPath: "/state/workers/implementer.sock",
      },
    };
    const bootToken = await api?.mintWorkerBootToken(root, child, "implementer", 1);
    if (!bootToken) throw new Error("worker boot token was not minted");
    const started = await json<{ secret: string }>("/legion/v1/worker/started", {
      tree: root,
      issue: child,
      role: "implementer",
      bootToken,
      sessionId: "ses_implementer",
      agentId: "agt_implementer",
      ompSessionFile: "/tmp/implementer.json",
    });
    expect(started.response.status).toBe(200);
    state.phases[child] = { phase: "implementer", sessionId: "ses_implementer" };
    const grantId = await mintGrant(child, "ses_implementer", started.body.secret);

    const complete = await json("/legion/v1/phase/complete", {
      grantId,
      summary: "Implemented the change",
    });

    expect(complete.response.status).toBe(200);
    expect(publications).toContainEqual({
      topic: roleTopic(roleToken(state.project, root, "architect")),
      payload: JSON.stringify({
        type: "phase-complete",
        issue: child,
        role: "implementer",
        summary: "Implemented the change",
      }),
    });
    expect(
      publications.some((publication) =>
        publication.topic.includes(roleToken(state.project, child, "architect"))
      )
    ).toBeFalse();
    expect(state.phases[child]).toBeUndefined();
  });

  it("returns the issue to in_progress instead of retro when a reviewer completes with changes requested", async () => {
    const statusWrites: Array<{ issue: IssueKey; status: string }> = [];
    await start({
      dispatchClient: fakeDispatchClient({
        setStatus: async (issue, status) => {
          statusWrites.push({ issue, status });
        },
      }),
    });
    const token = roleToken(state.project, root, "reviewer");
    state.roles[token] = {
      issue: root,
      role: "reviewer",
      generation: 1,
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%1",
        socketPath: "/state/workers/reviewer.sock",
      },
    };
    state.prs["acme/widgets#9"] = {
      key: root,
      repo: "acme/widgets",
      number: 9,
      headSha: "head-sha",
      verdict: "green",
      failing: [],
      failingStatuses: [],
      ciSettledAt: 0,
      ciCheckRuns: null,
      ciSettlementGeneration: null,
      ciSnapshot: null,
      ciReconciled: false,
      fixAttempts: 0,
      reviewDecision: "changes_requested",
    };
    const bootToken = await api?.mintWorkerBootToken(root, root, "reviewer", 1);
    if (!bootToken) throw new Error("worker boot token was not minted");
    const started = await json<{ secret: string }>("/legion/v1/worker/started", {
      tree: root,
      issue: root,
      role: "reviewer",
      bootToken,
      sessionId: "ses_reviewer",
      agentId: "agt_reviewer",
      ompSessionFile: "/tmp/reviewer.json",
    });
    expect(started.response.status).toBe(200);
    state.phases[root] = { phase: "reviewer", sessionId: "ses_reviewer" };
    const grantId = await mintGrant(root, "ses_reviewer", started.body.secret);

    const complete = await json("/legion/v1/phase/complete", {
      grantId,
      summary: "Requested changes",
    });

    expect(complete.response.status).toBe(200);
    expect(statusWrites).toEqual([{ issue: root, status: "in_progress" }]);
  });

  it("advances the issue to retro when a reviewer completes with an approved review", async () => {
    const statusWrites: Array<{ issue: IssueKey; status: string }> = [];
    await start({
      dispatchClient: fakeDispatchClient({
        setStatus: async (issue, status) => {
          statusWrites.push({ issue, status });
        },
      }),
    });
    const token = roleToken(state.project, root, "reviewer");
    state.roles[token] = {
      issue: root,
      role: "reviewer",
      generation: 1,
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%1",
        socketPath: "/state/workers/reviewer.sock",
      },
    };
    state.prs["acme/widgets#9"] = {
      key: root,
      repo: "acme/widgets",
      number: 9,
      headSha: "head-sha",
      verdict: "green",
      failing: [],
      failingStatuses: [],
      ciSettledAt: 0,
      ciCheckRuns: null,
      ciSettlementGeneration: null,
      ciSnapshot: null,
      ciReconciled: false,
      fixAttempts: 0,
      reviewDecision: "approved",
    };
    const bootToken = await api?.mintWorkerBootToken(root, root, "reviewer", 1);
    if (!bootToken) throw new Error("worker boot token was not minted");
    const started = await json<{ secret: string }>("/legion/v1/worker/started", {
      tree: root,
      issue: root,
      role: "reviewer",
      bootToken,
      sessionId: "ses_reviewer",
      agentId: "agt_reviewer",
      ompSessionFile: "/tmp/reviewer.json",
    });
    expect(started.response.status).toBe(200);
    state.phases[root] = { phase: "reviewer", sessionId: "ses_reviewer" };
    const grantId = await mintGrant(root, "ses_reviewer", started.body.secret);

    const complete = await json("/legion/v1/phase/complete", {
      grantId,
      summary: "Approved",
    });

    expect(complete.response.status).toBe(200);
    expect(statusWrites).toEqual([{ issue: root, status: "retro" }]);
  });

  it("rejects a duplicate phase/complete once the architect has reassigned the issue to a later phase", async () => {
    await start();
    const token = roleToken(state.project, root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%1",
        socketPath: "/state/workers/tester.sock",
      },
    };
    const bootToken = await api?.mintWorkerBootToken(root, root, "tester", 1);
    if (!bootToken) throw new Error("worker boot token was not minted");
    const started = await json<{ secret: string }>("/legion/v1/worker/started", {
      tree: root,
      issue: root,
      role: "tester",
      bootToken,
      sessionId: "ses_tester",
      agentId: "agt_tester",
      ompSessionFile: "/tmp/tester.json",
    });
    expect(started.response.status).toBe(200);
    state.phases[root] = { phase: "tester", sessionId: "ses_tester" };
    const grantId = await mintGrant(root, "ses_tester", started.body.secret);
    const body = { grantId, summary: "Verified the acceptance criteria" };

    const first = await json("/legion/v1/phase/complete", body);
    expect(first.response.status).toBe(200);
    expect(state.phases[root]).toBeUndefined();

    // The architect reassigns the same issue to a later phase (a fresh worker/started call for a
    // different role would set exactly this); the tester's own claim never changes. Grants are
    // read-only, reusable-until-expiry tokens, so the same grantId is still valid here.
    state.phases[root] = { phase: "implementer", sessionId: "ses_implementer" };

    const duplicate = await json<{ error: string }>("/legion/v1/phase/complete", body);

    expect(duplicate.response.status).toBe(409);
    expect(duplicate.body.error).toBe("Phase for WIDGETS-1 is no longer owned by this worker");
    expect(state.phases[root]).toEqual({ phase: "implementer", sessionId: "ses_implementer" });
    expect(
      publications.filter(
        (publication) =>
          publication.topic === roleTopic(roleToken(state.project, root, "architect"))
      )
    ).toHaveLength(1);
  });

  it("rejects phase/complete with an expired grant", async () => {
    await start();
    const token = roleToken(state.project, root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%1",
        socketPath: "/state/workers/tester.sock",
      },
    };
    const bootToken = await api?.mintWorkerBootToken(root, root, "tester", 1);
    if (!bootToken) throw new Error("worker boot token was not minted");
    const started = await json<{ secret: string }>("/legion/v1/worker/started", {
      tree: root,
      issue: root,
      role: "tester",
      bootToken,
      sessionId: "ses_tester",
      agentId: "agt_tester",
      ompSessionFile: "/tmp/tester.json",
    });
    expect(started.response.status).toBe(200);
    state.phases[root] = { phase: "tester", sessionId: "ses_tester" };
    const grantId = await mintGrant(root, "ses_tester", started.body.secret);

    now += 60_001;
    const complete = await json<{ error: string }>("/legion/v1/phase/complete", {
      grantId,
      summary: "smoke",
    });

    expect(complete.response.status).toBe(403);
    expect(complete.body.error).toBe("Invalid or expired grant");
    expect(state.phases[root]).toEqual({ phase: "tester", sessionId: "ses_tester" });
    expect(publications).toEqual([]);
  });

  it("rejects phase/complete when the grant's session no longer matches the worker's claim", async () => {
    await start();
    const token = roleToken(state.project, root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%1",
        socketPath: "/state/workers/tester.sock",
      },
    };
    const bootToken = await api?.mintWorkerBootToken(root, root, "tester", 1);
    if (!bootToken) throw new Error("worker boot token was not minted");
    const started = await json<{ secret: string }>("/legion/v1/worker/started", {
      tree: root,
      issue: root,
      role: "tester",
      bootToken,
      sessionId: "ses_tester",
      agentId: "agt_tester",
      ompSessionFile: "/tmp/tester.json",
    });
    expect(started.response.status).toBe(200);
    state.phases[root] = { phase: "tester", sessionId: "ses_tester" };
    const grantId = await mintGrant(root, "ses_tester", started.body.secret);
    // A respawn between minting the grant and redeeming it moves the claim onto a new session;
    // the grant is still unexpired, but it no longer names the worker that currently owns the role.
    const claim = state.roles[token];
    if (!claim || !("issue" in claim)) throw new Error("worker claim disappeared");
    claim.sessionId = "ses_tester_respawned";

    const complete = await json<{ error: string }>("/legion/v1/phase/complete", {
      grantId,
      summary: "smoke",
    });

    expect(complete.response.status).toBe(409);
    expect(complete.body.error).toBe("Grant does not match the worker currently holding this role");
    expect(state.phases[root]).toEqual({ phase: "tester", sessionId: "ses_tester" });
    expect(publications).toEqual([]);
  });

  it("marks the phase completed and returns 202 when the architect has no live holder, without dropping the completion", async () => {
    await start({
      envoyPublish: async (topic) => {
        throw new EnvoyPublishError(topic, 404);
      },
    });
    const token = roleToken(state.project, root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%1",
        socketPath: "/state/workers/tester.sock",
      },
    };
    const bootToken = await api?.mintWorkerBootToken(root, root, "tester", 1);
    if (!bootToken) throw new Error("worker boot token was not minted");
    const started = await json<{ secret: string }>("/legion/v1/worker/started", {
      tree: root,
      issue: root,
      role: "tester",
      bootToken,
      sessionId: "ses_tester",
      agentId: "agt_tester",
      ompSessionFile: "/tmp/tester.json",
    });
    expect(started.response.status).toBe(200);
    state.phases[root] = { phase: "tester", sessionId: "ses_tester" };
    const grantId = await mintGrant(root, "ses_tester", started.body.secret);

    const complete = await json("/legion/v1/phase/complete", { grantId, summary: "smoke" });

    expect(complete.response.status).toBe(202);
    // The completion is recorded, not dropped: state (the source of truth) keeps the phase with
    // a `completed` marker so `overseerCatchup` replays it and `routeActive` treats the issue as
    // having no active phase, instead of losing the report entirely.
    expect(state.phases[root]).toEqual({
      phase: "tester",
      sessionId: "ses_tester",
      completed: { summary: "smoke", at: new Date(now).toISOString() },
    });
    expect(state.roles[token]).toMatchObject({ issue: root, role: "tester", generation: 1 });
  });

  it("delivers an already-completed phase's report once the architect reappears, on a repeat completion call", async () => {
    let holderLive = false;
    await start({
      envoyPublish: async (topic, payload) => {
        if (!holderLive) throw new EnvoyPublishError(topic, 404);
        publications.push({ topic, payload });
      },
    });
    const token = roleToken(state.project, root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%1",
        socketPath: "/state/workers/tester.sock",
      },
    };
    const bootToken = await api?.mintWorkerBootToken(root, root, "tester", 1);
    if (!bootToken) throw new Error("worker boot token was not minted");
    const started = await json<{ secret: string }>("/legion/v1/worker/started", {
      tree: root,
      issue: root,
      role: "tester",
      bootToken,
      sessionId: "ses_tester",
      agentId: "agt_tester",
      ompSessionFile: "/tmp/tester.json",
    });
    expect(started.response.status).toBe(200);
    state.phases[root] = { phase: "tester", sessionId: "ses_tester" };

    const first = await json("/legion/v1/phase/complete", {
      grantId: await mintGrant(root, "ses_tester", started.body.secret),
      summary: "smoke",
    });
    expect(first.response.status).toBe(202);
    expect(state.phases[root]?.completed).toBeDefined();

    // The architect reappears; the same worker (its role claim was never touched) reports again.
    // The claim/phase-ownership checks still pass because completing a phase never clears the
    // claim or reassigns the phase to anyone else — only a fresh assignment would.
    holderLive = true;
    const retry = await json("/legion/v1/phase/complete", {
      grantId: await mintGrant(root, "ses_tester", started.body.secret),
      summary: "smoke",
    });

    expect(retry.response.status).toBe(200);
    expect(state.phases[root]).toBeUndefined();
    expect(publications).toEqual([
      {
        topic: roleTopic(roleToken(state.project, root, "architect")),
        payload: JSON.stringify({
          type: "phase-complete",
          issue: root,
          role: "tester",
          summary: "smoke",
        }),
      },
    ]);
  });

  it("serializes two concurrent completions for the same phase: one succeeds, the other 409s, and only one publish happens", async () => {
    await start({
      envoyPublish: async (topic, payload) => {
        // A deliberate delay keeps both concurrent requests' handlers in flight at once, so the
        // test actually exercises the capture-before-publish ordering rather than two requests
        // that happen to run fully sequentially.
        await new Promise((resolve) => setTimeout(resolve, 20));
        publications.push({ topic, payload });
      },
    });
    const token = roleToken(state.project, root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%1",
        socketPath: "/state/workers/tester.sock",
      },
    };
    const bootToken = await api?.mintWorkerBootToken(root, root, "tester", 1);
    if (!bootToken) throw new Error("worker boot token was not minted");
    const started = await json<{ secret: string }>("/legion/v1/worker/started", {
      tree: root,
      issue: root,
      role: "tester",
      bootToken,
      sessionId: "ses_tester",
      agentId: "agt_tester",
      ompSessionFile: "/tmp/tester.json",
    });
    expect(started.response.status).toBe(200);
    state.phases[root] = { phase: "tester", sessionId: "ses_tester" };
    const grantId = await mintGrant(root, "ses_tester", started.body.secret);
    const body = { grantId, summary: "Verified the acceptance criteria" };

    const [first, second] = await Promise.all([
      json<{ error: string }>("/legion/v1/phase/complete", body),
      json<{ error: string }>("/legion/v1/phase/complete", body),
    ]);

    const statuses = [first.response.status, second.response.status].sort();
    expect(statuses).toEqual([200, 409]);
    const rejected = first.response.status === 409 ? first : second;
    expect(rejected.body.error).toBe(`Phase for ${root} is no longer owned by this worker`);
    expect(publications).toHaveLength(1);
    expect(state.phases[root]).toBeUndefined();
  });

  it("rejects phase/complete with 502 and leaves the phase intact when Envoy publish fails for a reason other than no-holder, then succeeds idempotently on retry", async () => {
    let failPublish = true;
    await start({
      envoyPublish: async (topic, payload) => {
        if (failPublish) throw new EnvoyPublishError(topic, 500);
        publications.push({ topic, payload });
      },
    });
    const token = roleToken(state.project, root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%1",
        socketPath: "/state/workers/tester.sock",
      },
    };
    const bootToken = await api?.mintWorkerBootToken(root, root, "tester", 1);
    if (!bootToken) throw new Error("worker boot token was not minted");
    const started = await json<{ secret: string }>("/legion/v1/worker/started", {
      tree: root,
      issue: root,
      role: "tester",
      bootToken,
      sessionId: "ses_tester",
      agentId: "agt_tester",
      ompSessionFile: "/tmp/tester.json",
    });
    expect(started.response.status).toBe(200);
    state.phases[root] = { phase: "tester", sessionId: "ses_tester" };
    const grantId = await mintGrant(root, "ses_tester", started.body.secret);
    const body = { grantId, summary: "Verified the acceptance criteria" };

    const failed = await json("/legion/v1/phase/complete", body);

    expect(failed.response.status).toBe(502);
    expect(state.phases[root]).toEqual({ phase: "tester", sessionId: "ses_tester" });
    expect(publications).toEqual([]);

    failPublish = false;
    const retried = await json("/legion/v1/phase/complete", body);

    expect(retried.response.status).toBe(200);
    expect(state.phases[root]).toBeUndefined();
    expect(publications).toEqual([
      {
        topic: roleTopic(roleToken(state.project, root, "architect")),
        payload: JSON.stringify({
          type: "phase-complete",
          issue: root,
          role: "tester",
          summary: "Verified the acceptance criteria",
        }),
      },
    ]);
  });

  it("restores the phase and returns 500 when saving fails after the in-memory delete, then succeeds on retry with a duplicate publish", async () => {
    let failSave = false;
    await start({
      saveState: async () => {
        if (failSave) throw new Error("disk full");
      },
    });
    const token = roleToken(state.project, root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%1",
        socketPath: "/state/workers/tester.sock",
      },
    };
    const bootToken = await api?.mintWorkerBootToken(root, root, "tester", 1);
    if (!bootToken) throw new Error("worker boot token was not minted");
    const started = await json<{ secret: string }>("/legion/v1/worker/started", {
      tree: root,
      issue: root,
      role: "tester",
      bootToken,
      sessionId: "ses_tester",
      agentId: "agt_tester",
      ompSessionFile: "/tmp/tester.json",
    });
    expect(started.response.status).toBe(200);
    state.phases[root] = { phase: "tester", sessionId: "ses_tester" };
    const grantId = await mintGrant(root, "ses_tester", started.body.secret);
    const body = { grantId, summary: "Verified the acceptance criteria" };

    failSave = true;
    const failed = await json("/legion/v1/phase/complete", body);

    expect(failed.response.status).toBe(500);
    expect(state.phases[root]).toEqual({ phase: "tester", sessionId: "ses_tester" });

    failSave = false;
    const retried = await json("/legion/v1/phase/complete", body);

    expect(retried.response.status).toBe(200);
    expect(state.phases[root]).toBeUndefined();
    expect(publications).toEqual([
      {
        topic: roleTopic(roleToken(state.project, root, "architect")),
        payload: JSON.stringify({
          type: "phase-complete",
          issue: root,
          role: "tester",
          summary: "Verified the acceptance criteria",
        }),
      },
      {
        topic: roleTopic(roleToken(state.project, root, "architect")),
        payload: JSON.stringify({
          type: "phase-complete",
          issue: root,
          role: "tester",
          summary: "Verified the acceptance criteria",
        }),
      },
    ]);
  });
});
