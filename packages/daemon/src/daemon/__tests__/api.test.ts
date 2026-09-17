import { afterEach, beforeEach, describe, expect, it, spyOn, vi } from "bun:test";
import { randomUUID } from "node:crypto";
import { mkdtemp, rm } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import {
  type ArtifactApproval,
  controllerToken,
  type IssueDetails,
  type IssueKey,
  roleToken,
  roleTopic,
} from "@legion/contracts";
import { type LegionApi, type LegionApiDeps, startLegionApi } from "../api";
import { secretHash, spawnCapabilityKey } from "../api/auth";
import { CONTROLLER_HAS_NO_REPOSITORY, EnvoyPublishError } from "../api/http";
import { type LegionState, loadState, newLegionState, saveState } from "../legion-state";
import { TreeClosingError } from "../processes";
import { routeActive } from "../reducers";
import { checkPr, fakeDispatchClient } from "./ci-fixtures";

const root = "WIDGETS-1" as IssueKey;
const child = "WIDGETS-2" as IssueKey;
const otherRoot = "OTHER-9" as IssueKey;
const foreign = "OTHER-10" as IssueKey;

/** The `GET /api/v1/issues/<key>` read `register_gate` makes when the gate would be closed: one
 * document on the issue carrying the given `approval`. Only the fields the route reads are
 * meaningful; the rest satisfy the contract's shape. */
function issueWithDocument(
  key: IssueKey,
  documentId: string,
  approval: ArtifactApproval | undefined
): IssueDetails {
  const actor = { kind: "user", id: "sjawhar" } as const;
  return {
    key,
    project: "WIDGETS",
    number: 1,
    title: key,
    status: "in_progress",
    rank: "a0",
    priority: null,
    labels: [],
    parent: null,
    assignee: "sjawhar",
    components: { mode: "inherit", ids: [], unknown: [], reason: null, inherited_from: null },
    external_links: [],
    route: null,
    created_by: actor,
    created_at: "2026-01-01T00:00:00.000Z",
    updated_at: "2026-01-01T00:00:00.000Z",
    closed_at: null,
    primary_artifact_id: documentId,
    last_seq: 1,
    artifacts: [
      {
        id: documentId,
        issue_key: key,
        project: "WIDGETS",
        slug: "spec",
        name: "spec.md",
        kind: "doc",
        primary: true,
        created_by: actor,
        created_at: "2026-01-01T00:00:00.000Z",
        versions: Array.from({ length: approval?.latest_version ?? 1 }, (_, index) => ({
          number: index + 1,
          named: false,
          summary: null,
          authors: [actor],
          created_at: "2026-01-01T00:00:00.000Z",
        })),
        ...(approval ? { approval } : {}),
      },
    ],
    open_asks: [],
    children: [],
  };
}

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
  let publications: Array<{ topic: string; payload: string }>;
  let tokenRoles: string[];
  let tokenCalls: Array<[string, string]>;
  let closedTrees: IssueKey[];
  let admissions: IssueKey[];
  let spawnedWorkers: Array<{ tree: IssueKey; issue: IssueKey; role: string; task: string }>;
  let recoveredRoles: string[];
  let workerReadyCalls: Array<{
    issue: IssueKey;
    role: string;
    sessionId: string;
    generation: number;
  }>;
  let treeReadyConnected: IssueKey[];
  let workerReadyConnected: boolean;
  let confirmRootReadyCalls: Array<{ tree: IssueKey; generation: number }>;
  let now: number;
  let controllerSecret: string;

  beforeEach(() => {
    publications = [];
    tokenRoles = [];
    tokenCalls = [];
    closedTrees = [];
    admissions = [];
    spawnedWorkers = [];
    recoveredRoles = [];
    workerReadyCalls = [];
    treeReadyConnected = [];
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
      locator: { runtime: "tmux", tmuxSession: "legion-omp", tmuxWindowId: "@1" },
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
    gates?: { design: "root-issues" | "off" };
    state?: LegionState;
    saveState?: () => Promise<void>;
    mintController?: boolean;
    operatorToken?: string;
    admissionResult?: "spawned" | "queued";
    admit?: (issue: IssueKey) => "spawned" | "queued";
    onTreeReady?: (tree: IssueKey) => Promise<void>;
    onControllerReady?: () => Promise<void>;
    stashControllerReadyImpl?: LegionApiDeps["processManager"]["stashControllerReady"];
    recordControllerReadyImpl?: LegionApiDeps["processManager"]["recordControllerReady"];
    getToken?: LegionApiDeps["tokenManager"]["getToken"];
    projects?: Readonly<Record<string, { repo: `${string}/${string}` }>>;
    spawnWorkerImpl?: LegionApiDeps["processManager"]["spawnWorker"];
    mutateLiveRoleClaimImpl?: LegionApiDeps["processManager"]["mutateLiveRoleClaim"];
    markTreeReadyImpl?: LegionApiDeps["processManager"]["markTreeReady"];
    workerReadyImpl?: LegionApiDeps["processManager"]["workerReady"];
    recoverRoleImpl?: LegionApiDeps["processManager"]["recoverRole"];
    dispatchClient?: LegionApiDeps["dispatchClient"];
  }) {
    const deps: LegionApiDeps = {
      state: options?.state ?? state,
      dispatchClient: options?.dispatchClient ?? fakeDispatchClient(),
      tokenManager: {
        getToken:
          options?.getToken ??
          (async (role, owner) => {
            tokenCalls.push([role, owner]);
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
        cancelBootWatchdog: () => {},
        stashControllerReady: options?.stashControllerReadyImpl ?? (() => false),
        recordControllerReady: options?.recordControllerReadyImpl ?? (() => false),
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
        recoverRole: async (role) => {
          recoveredRoles.push(role);
          await options?.recoverRoleImpl?.(role);
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
        projects: options?.projects ?? { WIDGETS: { repo: "acme/widgets" } },
        gates: options?.gates ?? { design: "root-issues" },
        now: () => now,
        operatorToken: options?.operatorToken,
      },
      deps
    );
    if (options?.mintController !== false) {
      controllerSecret = await api.mintControllerCapability();
    }
  }

  /** Mints a root boot token and registers the root architect (generation 3), returning the
   * capability the architect routes take. */
  async function registerRootArchitect(): Promise<{ sessionId: string; secret: string }> {
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
    publications.length = 0;
    return { sessionId: "ses_root", secret: started.body.secret };
  }

  async function mintArchitectGrant(issue: IssueKey): Promise<GrantResponse> {
    state.issues[issue] = {
      key: issue,
      title: issue,
      status: "in_progress",
      children: [],
    };
    state.trees[issue] = {
      root: issue,
      generation: 1,
      locator: { runtime: "tmux", tmuxSession: "legion-omp", tmuxWindowId: "@1" },
      status: "queued",
      launchFailures: 0,
    };
    const bootToken = await api?.mintBootToken(issue, 1);
    if (!bootToken) throw new Error("boot nonce was not minted");
    const started = await json<{ secret: string }>("/legion/v1/process/started", {
      tree: issue,
      generation: 1,
      rootSessionId: `ses_${issue}`,
      bootToken,
      agentId: `agent_${issue}`,
      ompSessionFile: `/tmp/${issue}.json`,
    });
    expect(started.response.status).toBe(200);
    const grant = await curlJson<GrantResponse>("/legion/v1/grants", {
      tree: issue,
      issue,
      sessionId: `ses_${issue}`,
      secret: started.body.secret,
    });
    expect(grant.status).toBe(200);
    return grant.body;
  }

  async function request(path: string, body?: unknown, headers?: Record<string, string>) {
    if (!api) throw new Error("API was not started");
    return fetch(`http://127.0.0.1:${api.server.port}${path}`, {
      method: body === undefined ? "GET" : "POST",
      headers: body === undefined ? headers : { "content-type": "application/json", ...headers },
      body: body === undefined ? undefined : JSON.stringify(body),
    });
  }

  async function json<T = unknown>(path: string, body: unknown, headers?: Record<string, string>) {
    const response = await request(path, body, headers);
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
  it("resolves a root boot token on the worker stream to the tree's architect token, stale once the tree's generation moves on, and still after /process/started consumed it", async () => {
    await start();
    const bootToken = await api?.mintBootToken(root, 3);
    if (!bootToken || !api) throw new Error("boot nonce was not minted");
    const architect = roleToken("omp", root, "architect");
    expect(api.resolveWorkerBootToken(bootToken)).toEqual({ token: architect, stale: false });
    // Consumed by /process/started -- the stream may still re-register the same pod after a
    // redial, so the listener's lookup never consumes the token.
    expect(
      (
        await json("/legion/v1/process/started", {
          tree: root,
          generation: 3,
          rootSessionId: "ses_root",
          agentId: "root-agent",
          ompSessionFile: "/tmp/root.json",
          bootToken,
        })
      ).response.status
    ).toBe(200);
    expect(api.resolveWorkerBootToken(bootToken)).toEqual({ token: architect, stale: false });
    const tree = state.trees[root];
    if (!tree) throw new Error("tree missing");
    tree.generation = 4;
    expect(api.resolveWorkerBootToken(bootToken)).toEqual({ token: architect, stale: true });
    expect(api.resolveWorkerBootToken("never-minted")).toBeUndefined();
  });
  it("resolves a root boot token from its persisted spawnCapabilities record after a restart, generation included, and rejects one whose record is not the tree architect's", async () => {
    // A restarted daemon: the state already carries the hash records, no in-memory mint.
    state.spawnCapabilities[spawnCapabilityKey("durable-root")] = {
      tree: root,
      issue: root,
      role: "architect",
      generation: 3,
    };
    state.spawnCapabilities[spawnCapabilityKey("durable-tester")] = {
      tree: root,
      issue: root,
      role: "tester",
    };
    await start();
    if (!api) throw new Error("api not started");
    const architect = roleToken("omp", root, "architect");
    expect(api.resolveWorkerBootToken("durable-root")).toEqual({ token: architect, stale: false });
    const tree = state.trees[root];
    if (!tree) throw new Error("tree missing");
    tree.generation = 4;
    expect(api.resolveWorkerBootToken("durable-root")).toEqual({ token: architect, stale: true });
    expect(api.resolveWorkerBootToken("durable-tester")).toBeUndefined();
  });
  it("mintBootToken persists the generation on the spawn capability", async () => {
    await start();
    const bootToken = await api?.mintBootToken(root, 3);
    if (!bootToken) throw new Error("boot nonce was not minted");
    expect(state.spawnCapabilities[spawnCapabilityKey(bootToken)]).toEqual({
      tree: root,
      issue: root,
      role: "architect",
      generation: 3,
    });
  });
  it("refuses a resurrected root whose session is not the recorded architect session with the worker route's 409, consuming nothing and leaving the recorded session and locator unchanged", async () => {
    await start();
    await registerRootArchitect();
    const architect = roleToken("omp", root, "architect");
    // The resurrection: `spawnTree` bumps the generation and mints with the session the previous
    // `/process/started` recorded (mirroring `launchWorker`'s `claim.sessionId`).
    const tree = state.trees[root];
    if (!tree) throw new Error("tree missing");
    tree.generation = 4;
    const bootToken = await api?.mintBootToken(root, 4, "ses_root");
    if (!bootToken) throw new Error("root boot token was not minted");
    // Under postgres a missing session row makes Oh My Pi start a fresh session with a new id at
    // the requested path — the fresh agent this refusal exists to keep off the old tree.
    const refused = await json<{ error: string }>("/legion/v1/process/started", {
      tree: root,
      generation: 4,
      rootSessionId: "ses_fresh",
      bootToken,
      agentId: "fresh-agent",
      ompSessionFile: "/tmp/fresh.json",
    });
    expect(refused.response.status).toBe(409);
    expect(refused.body).toEqual({ error: "Worker respawn must resume the same agent session" });
    expect(state.roles[architect]).toMatchObject({ sessionId: "ses_root", agentId: "root-agent" });
    expect(state.trees[root]?.locator).toMatchObject({ ompSessionFile: "/tmp/root.json" });
    // The refusal consumed nothing: the recorded session registers with the same token.
    const accepted = await json("/legion/v1/process/started", {
      tree: root,
      generation: 4,
      rootSessionId: "ses_root",
      bootToken,
      agentId: "root-agent",
      ompSessionFile: "/tmp/root.json",
    });
    expect(accepted.response.status).toBe(200);
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
      runtime: "tmux",
      tmuxSession: "legion-omp",
      tmuxWindowId: "@1",
      tmuxPaneId: "%1",
      socketPath: "/tmp/legion/architect.sock",
      ompSessionFile: "/tmp/root.jsonl",
    };
    state.controllerLocator = {
      runtime: "tmux",
      tmuxSession: "legion-omp",
      tmuxWindowId: "@0",
      tmuxPaneId: "%0",
      ompSessionFile: "/tmp/controller.jsonl",
    };
    state.gates[root] = {
      artifactId: "4e0aca36-77b3-43bd-96cf-d58890ae64e4",
      latestVersion: 3,
      approvedVersion: 3,
    };
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
        runtime: "tmux",
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
          runtime: "tmux",
          tmuxSession: "legion-omp",
          tmuxWindowId: "@1",
          tmuxPaneId: "%1",
          ompSessionFile: "/tmp/root.jsonl",
        },
      },
    });
    // The tree locator projection is exact: the runtime discriminant and session file come
    // through, the shim `socketPath` the fixture carries never does.
    expect((body.trees as Record<string, { locator: unknown }>)[root]?.locator).toEqual({
      runtime: "tmux",
      tmuxSession: "legion-omp",
      tmuxWindowId: "@1",
      tmuxPaneId: "%1",
      ompSessionFile: "/tmp/root.jsonl",
    });
    expect(body.admission).toEqual({ cap: 2, active: [], queue: [] });
    expect(body.gates).toEqual({
      [root]: {
        artifactId: "4e0aca36-77b3-43bd-96cf-d58890ae64e4",
        latestVersion: 3,
        approvedVersion: 3,
      },
    });
    expect(body.controllerLocator).toEqual({
      runtime: "tmux",
      tmuxSession: "legion-omp",
      tmuxWindowId: "@0",
      tmuxPaneId: "%0",
      ompSessionFile: "/tmp/controller.jsonl",
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
        locator: {
          runtime: "tmux",
          tmuxSession: "legion-omp",
          tmuxWindowId: "@2",
          tmuxPaneId: "%2",
        },
      },
    });
    expect(body.controllerPendingNotices).toBe(1);
    expect(body.pendingStatusWrites).toEqual([child]);
    expect(Object.keys(body)).toContain("workerAdmission");
    expect(body.workerAdmission).toEqual({ queue: [] });

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

  it("projects the operator-launched controller's external record unchanged on GET /legion/v1/state", async () => {
    state.controllerLocator = {
      runtime: "kubernetes",
      external: true,
      sessionId: "ses_op",
      registeredAt: 1234,
    };
    await start();
    const response = await request("/legion/v1/state");
    // A 200 proves `validateContractResponse` accepted the projection against the strict schema.
    expect(response.status).toBe(200);
    const body = (await response.json()) as Record<string, unknown>;
    expect(body.controllerLocator).toEqual({
      runtime: "kubernetes",
      external: true,
      sessionId: "ses_op",
      registeredAt: 1234,
    });
  });

  it("lists the worker admission queue in order with roleToken, issue, role, kind, and queuedAt, omitting kind and queuedAt for a stale entry and never the task text", async () => {
    const testerToken = roleToken(state.project, root, "tester");
    const plannerToken = roleToken(state.project, child, "planner");
    state.issues[child] = {
      key: child,
      title: "Child",
      parent: root,
      status: "todo",
      children: [],
    };
    state.roles[testerToken] = {
      issue: root,
      role: "tester",
      pendingAssignment: {
        kind: "assignment",
        task: "verify #41 with the secret phrase xyzzy",
        queuedAt: "2026-09-13T17:00:00.000Z",
        deliveryId: "00000000-0000-4000-8000-000000000001",
      },
    };
    // A stale head: the claim lost its pending task (the next promotion drain drops it).
    state.roles[plannerToken] = { issue: child, role: "planner" };
    state.workerAdmission.queue.push(testerToken, plannerToken);

    await start();
    const stateResponse = await request("/legion/v1/state");
    // A 200 proves `validateContractResponse` accepted the projection against the strict schema.
    expect(stateResponse.status).toBe(200);
    const text = await stateResponse.text();
    const body = JSON.parse(text) as Record<string, unknown>;

    expect(body.workerAdmission).toEqual({
      queue: [
        {
          roleToken: testerToken,
          issue: root,
          role: "tester",
          kind: "assignment",
          queuedAt: "2026-09-13T17:00:00.000Z",
        },
        { roleToken: plannerToken, issue: child, role: "planner" },
      ],
    });
    expect(text).not.toContain("xyzzy");
  });

  it("records the controller's OMP session file on /controller/ready and leaves it unset when omitted", async () => {
    state.controllerLocator = {
      runtime: "tmux",
      tmuxSession: "legion-omp",
      tmuxWindowId: "@0",
      tmuxPaneId: "%0",
    };
    let readyCalls = 0;
    await start({
      onControllerReady: async () => {
        readyCalls += 1;
      },
    });

    const withoutFile = await json("/legion/v1/controller/ready", {
      secret: controllerSecret,
      sessionId: "ses_controller",
    });
    expect(withoutFile.response.status).toBe(200);
    expect(state.controllerLocator).toEqual({
      runtime: "tmux",
      tmuxSession: "legion-omp",
      tmuxWindowId: "@0",
      tmuxPaneId: "%0",
    });
    expect(state.roles[controllerToken(state.project)]).toEqual({
      role: "controller",
      sessionId: "ses_controller",
    });
    expect(readyCalls).toBe(1);

    const withFile = await json("/legion/v1/controller/ready", {
      secret: controllerSecret,
      sessionId: "ses_controller",
      ompSessionFile: "/tmp/controller.jsonl",
    });
    expect(withFile.response.status).toBe(200);
    expect(state.controllerLocator).toEqual({
      runtime: "tmux",
      tmuxSession: "legion-omp",
      tmuxWindowId: "@0",
      tmuxPaneId: "%0",
      ompSessionFile: "/tmp/controller.jsonl",
    });
    expect(readyCalls).toBe(2);

    // An older plugin (or a takeover claim) that omits the field leaves the recorded file alone.
    const omittedAfterRecord = await json("/legion/v1/controller/ready", {
      secret: controllerSecret,
      sessionId: "ses_takeover",
    });
    expect(omittedAfterRecord.response.status).toBe(200);
    expect(state.controllerLocator?.ompSessionFile).toBe("/tmp/controller.jsonl");
    expect(state.roles[controllerToken(state.project)]).toEqual({
      role: "controller",
      sessionId: "ses_takeover",
    });

    const wrongSecret = await json("/legion/v1/controller/ready", {
      secret: "wrong",
      sessionId: "ses_controller",
      ompSessionFile: "/tmp/other.jsonl",
    });
    expect(wrongSecret.response.status).toBe(403);
    expect(wrongSecret.body).toEqual({ error: "Invalid controller capability" });
    expect(state.controllerLocator?.ompSessionFile).toBe("/tmp/controller.jsonl");
  });

  it("stashes an OMP session file from a first controller ready until the runtime locator exists", async () => {
    delete state.controllerLocator;
    const stashed: Array<{ sessionId: string; ompSessionFile: string }> = [];
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    try {
      await start({
        stashControllerReadyImpl: (sessionId, ompSessionFile) => {
          stashed.push({ sessionId, ompSessionFile });
          return true;
        },
      });
      const ready = await json("/legion/v1/controller/ready", {
        secret: controllerSecret,
        sessionId: "ses_controller",
        ompSessionFile: "/tmp/controller.jsonl",
      });
      expect(ready.response.status).toBe(200);
      expect(state.controllerLocator).toBeUndefined();
      expect(stashed).toEqual([
        { sessionId: "ses_controller", ompSessionFile: "/tmp/controller.jsonl" },
      ]);
      expect(warn).not.toHaveBeenCalled();
    } finally {
      warn.mockRestore();
    }
  });

  it("records the operator-launched controller's external record on /controller/ready when the runtime provides one, ignoring its OMP session file", async () => {
    let stashCalls = 0;
    await start({
      recordControllerReadyImpl: (sessionId) => {
        state.controllerLocator = {
          runtime: "kubernetes",
          external: true,
          sessionId,
          registeredAt: 5,
        };
        return true;
      },
      stashControllerReadyImpl: () => {
        stashCalls += 1;
        return true;
      },
    });
    const ready = await json("/legion/v1/controller/ready", {
      secret: controllerSecret,
      sessionId: "ses_operator",
      ompSessionFile: "/x.jsonl",
    });
    expect(ready.response.status).toBe(200);
    expect(state.roles[controllerToken(state.project)]).toEqual({
      role: "controller",
      sessionId: "ses_operator",
    });
    // Nothing resumes the operator's process, so its transcript is never recorded or stashed.
    expect(state.controllerLocator).toEqual({
      runtime: "kubernetes",
      external: true,
      sessionId: "ses_operator",
      registeredAt: 5,
    });
    expect(stashCalls).toBe(0);
  });

  describe("POST /legion/v1/controller/secret (legion controller start)", () => {
    const bearer = (token: string) => ({ authorization: `Bearer ${token}` });

    it("mints a controller capability for the operator token, and the previous secret stops working", async () => {
      await start({ operatorToken: "op-tok", mintController: false });
      const first = await json<{ secret: string }>(
        "/legion/v1/controller/secret",
        {},
        bearer("op-tok")
      );
      expect(first.response.status).toBe(200);
      expect(first.body.secret).toMatch(/^[0-9a-f-]{36}$/);
      const readyA = await json("/legion/v1/controller/ready", {
        secret: first.body.secret,
        sessionId: "ses_a",
      });
      expect(readyA.response.status).toBe(200);

      // A second `legion controller start`: a fresh secret, and the first session's stops working.
      const second = await json<{ secret: string }>(
        "/legion/v1/controller/secret",
        {},
        bearer("op-tok")
      );
      expect(second.response.status).toBe(200);
      expect(second.body.secret).not.toBe(first.body.secret);
      const staleReady = await json("/legion/v1/controller/ready", {
        secret: first.body.secret,
        sessionId: "ses_a",
      });
      expect(staleReady.response.status).toBe(403);
      const readyB = await json("/legion/v1/controller/ready", {
        secret: second.body.secret,
        sessionId: "ses_b",
      });
      expect(readyB.response.status).toBe(200);
      expect(state.roles[controllerToken(state.project)]).toEqual({
        role: "controller",
        sessionId: "ses_b",
      });
    });

    it("answers 403 and mints nothing for a wrong or missing token, logging one line each", async () => {
      await start({ operatorToken: "op-tok" });
      const hashBefore = state.controllerCapabilityHash;
      const errors = vi.spyOn(console, "error").mockImplementation(() => {});
      try {
        const wrong = await json("/legion/v1/controller/secret", {}, bearer("not-it"));
        expect(wrong.response.status).toBe(403);
        expect(wrong.body).toEqual({ error: "Invalid operator token" });
        const missing = await json("/legion/v1/controller/secret", {});
        expect(missing.response.status).toBe(403);
        expect(missing.body).toEqual({ error: "Invalid operator token" });
        expect(errors.mock.calls.map((call) => call.map(String).join(" "))).toEqual([
          "[legion] refused POST /legion/v1/controller/secret: wrong operator token",
          "[legion] refused POST /legion/v1/controller/secret: no bearer token",
        ]);
      } finally {
        errors.mockRestore();
      }
      expect(state.controllerCapabilityHash).toBe(hashBefore);
      // The controller the daemon minted for itself still works.
      const ready = await json("/legion/v1/controller/ready", {
        secret: controllerSecret,
        sessionId: "ses_controller",
      });
      expect(ready.response.status).toBe(200);
    });

    it("is disabled on a daemon without an operator token (tmux), naming operator_token_file", async () => {
      await start();
      const hashBefore = state.controllerCapabilityHash;
      const errors = vi.spyOn(console, "error").mockImplementation(() => {});
      try {
        const refused = await json("/legion/v1/controller/secret", {}, bearer("anything"));
        expect(refused.response.status).toBe(403);
        expect(refused.body).toEqual({
          error:
            "This daemon has no operator_token_file configured; the controller secret route is disabled",
        });
        expect(errors).toHaveBeenCalledTimes(1);
      } finally {
        errors.mockRestore();
      }
      expect(state.controllerCapabilityHash).toBe(hashBefore);
    });

    it("rejects a non-empty body: the token travels as a bearer header, never in the body", async () => {
      await start({ operatorToken: "op-tok" });
      const refused = await json(
        "/legion/v1/controller/secret",
        { token: "op-tok" },
        bearer("op-tok")
      );
      expect(refused.response.status).toBe(400);
    });
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
    otherTree.locator = { runtime: "tmux", tmuxSession: "legion-omp", tmuxWindowId: "@9" };

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
        runtime: "tmux",
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

  it("registers the design gate's spec document and version only for an architect's own tree, keeping a recorded approval of the same document", async () => {
    // Every registration below leaves the gate closed, so each reads the issue from Dispatch
    // once; the document is still awaiting its approval, so nothing is seeded.
    const reads: string[] = [];
    await start({
      dispatchClient: fakeDispatchClient({
        getIssue: async (key) => {
          reads.push(key);
          return issueWithDocument(key as IssueKey, "4e0aca36-77b3-43bd-96cf-d58890ae64e4", {
            state: "awaiting",
            latest_version: 3,
          });
        },
      }),
    });
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
    publications.length = 0;

    const outOfTree = await json("/legion/v1/gates/register", {
      tree: root,
      issue: foreign,
      artifactId: "0f0f0f0f-0000-4000-8000-00000000f00d",
      version: 1,
      ...architect,
    });
    expect(outOfTree.response.status).toBe(403);
    expect(state.gates[foreign]).toBeUndefined();

    const registered = await json("/legion/v1/gates/register", {
      tree: root,
      issue: child,
      artifactId: "4e0aca36-77b3-43bd-96cf-d58890ae64e4",
      version: 3,
      ...architect,
    });
    expect(registered.response.status).toBe(200);
    expect(state.gates[child]).toEqual({
      artifactId: "4e0aca36-77b3-43bd-96cf-d58890ae64e4",
      latestVersion: 3,
    });
    expect(reads).toEqual([child]);
    expect(publications).toEqual([]);

    // The same document at a later version keeps the approval already recorded (the gate is
    // now closed until that version is approved) and only raises latestVersion. Dispatch's own
    // latest_version (here still 3, the read is a fixture) never lowers what the architect sent.
    state.gates[child] = {
      artifactId: "4e0aca36-77b3-43bd-96cf-d58890ae64e4",
      latestVersion: 3,
      approvedVersion: 3,
    };
    const reRegistered = await json("/legion/v1/gates/register", {
      tree: root,
      issue: child,
      artifactId: "4e0aca36-77b3-43bd-96cf-d58890ae64e4",
      version: 5,
      ...architect,
    });
    expect(reRegistered.response.status).toBe(200);
    expect(state.gates[child]).toEqual({
      artifactId: "4e0aca36-77b3-43bd-96cf-d58890ae64e4",
      latestVersion: 5,
      approvedVersion: 3,
    });

    // A different document replaces the gate wholesale: an approval pins one document's version.
    // Dispatch must know that document on the issue, or the registration is refused naming it.
    const unknownDocument = await json<{ error: string }>("/legion/v1/gates/register", {
      tree: root,
      issue: child,
      artifactId: "9c1d3f5a-2b4e-4c6d-8e0f-1a2b3c4d5e6f",
      version: 1,
      ...architect,
    });
    expect(unknownDocument.response.status).toBe(404);
    expect(unknownDocument.body.error).toBe(
      `9c1d3f5a-2b4e-4c6d-8e0f-1a2b3c4d5e6f is not a document of ${child}`
    );
    expect(state.gates[child]).toEqual({
      artifactId: "4e0aca36-77b3-43bd-96cf-d58890ae64e4",
      latestVersion: 5,
      approvedVersion: 3,
    });
    expect(reads).toEqual([child, child, child]);
  });

  it("registers the document id lowercase however the architect typed it, so Dispatch's lowercase artifact events match the gate", async () => {
    await start({
      dispatchClient: fakeDispatchClient({
        getIssue: async (key) =>
          issueWithDocument(key as IssueKey, "4e0aca36-77b3-43bd-96cf-d58890ae64e4", {
            state: "awaiting",
            latest_version: 2,
          }),
      }),
    });
    const architect = await registerRootArchitect();
    const registered = await json("/legion/v1/gates/register", {
      tree: root,
      issue: root,
      artifactId: "4E0ACA36-77B3-43BD-96CF-D58890AE64E4",
      version: 2,
      ...architect,
    });
    expect(registered.response.status).toBe(200);
    expect(state.gates[root]).toEqual({
      artifactId: "4e0aca36-77b3-43bd-96cf-d58890ae64e4",
      latestVersion: 2,
    });
  });

  it("opens the gate at registration when Dispatch already shows a human approval of the current version, waking the architect once", async () => {
    await start({
      dispatchClient: fakeDispatchClient({
        getIssue: async (key) =>
          issueWithDocument(key as IssueKey, "4e0aca36-77b3-43bd-96cf-d58890ae64e4", {
            state: "approved",
            latest_version: 2,
            version: 2,
            by: { kind: "user", id: "sjawhar" },
          }),
      }),
    });
    const architect = await registerRootArchitect();
    // The human approved from the document header before the architect registered; the
    // architect still sends the version it requested approval of.
    const registered = await json("/legion/v1/gates/register", {
      tree: root,
      issue: root,
      artifactId: "4e0aca36-77b3-43bd-96cf-d58890ae64e4",
      version: 2,
      ...architect,
    });
    expect(registered.response.status).toBe(200);
    expect(state.gates[root]).toEqual({
      artifactId: "4e0aca36-77b3-43bd-96cf-d58890ae64e4",
      latestVersion: 2,
      approvedVersion: 2,
    });
    expect(publications).toEqual([
      {
        topic: roleTopic(roleToken(state.project, root, "architect")),
        payload: JSON.stringify({ type: "design-approved" }),
      },
    ]);

    // Re-registering an open gate reads nothing and wakes nobody twice.
    const again = await json("/legion/v1/gates/register", {
      tree: root,
      issue: root,
      artifactId: "4e0aca36-77b3-43bd-96cf-d58890ae64e4",
      version: 2,
      ...architect,
    });
    expect(again.response.status).toBe(200);
    expect(publications).toHaveLength(1);
  });

  it("records a stale approval at registration as closed without a wake, and raises latestVersion to Dispatch's current version", async () => {
    await start({
      dispatchClient: fakeDispatchClient({
        getIssue: async (key) =>
          issueWithDocument(key as IssueKey, "4e0aca36-77b3-43bd-96cf-d58890ae64e4", {
            state: "stale",
            latest_version: 3,
            version: 2,
            by: { kind: "user", id: "sjawhar" },
          }),
      }),
    });
    const architect = await registerRootArchitect();
    // The human approved v2, then the spec was edited to v3 before the architect registered.
    const registered = await json("/legion/v1/gates/register", {
      tree: root,
      issue: root,
      artifactId: "4e0aca36-77b3-43bd-96cf-d58890ae64e4",
      version: 2,
      ...architect,
    });
    expect(registered.response.status).toBe(200);
    expect(state.gates[root]).toEqual({
      artifactId: "4e0aca36-77b3-43bd-96cf-d58890ae64e4",
      latestVersion: 3,
      approvedVersion: 2,
    });
    expect(publications).toEqual([]);
  });

  it("fails a registration with 502 and records no gate when the Dispatch read fails, so the architect retries", async () => {
    await start({
      dispatchClient: fakeDispatchClient({
        getIssue: async () => {
          throw new Error("connect ECONNREFUSED 127.0.0.1:8766");
        },
      }),
    });
    const architect = await registerRootArchitect();
    const registered = await json<{ error: string }>("/legion/v1/gates/register", {
      tree: root,
      issue: root,
      artifactId: "4e0aca36-77b3-43bd-96cf-d58890ae64e4",
      version: 2,
      ...architect,
    });
    expect(registered.response.status).toBe(502);
    expect(registered.body.error).toBe(
      `Dispatch read of ${root} failed; retry register_gate: connect ECONNREFUSED 127.0.0.1:8766`
    );
    expect(state.gates[root]).toBeUndefined();
    expect(publications).toEqual([]);
  });

  it("rejects a gate registration that names an ask id or omits the version with a 400 naming the field", async () => {
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
    const architect = { sessionId: "ses_root", secret: started.body.secret };

    const askId = await json<{ error: string }>("/legion/v1/gates/register", {
      tree: root,
      issue: root,
      askId: "ask-1",
      ...architect,
    });
    expect(askId.response.status).toBe(400);
    expect(askId.body.error).toContain("askId");
    expect(askId.body.error).toContain("artifactId");

    const missingVersion = await json<{ error: string }>("/legion/v1/gates/register", {
      tree: root,
      issue: root,
      artifactId: "4e0aca36-77b3-43bd-96cf-d58890ae64e4",
      ...architect,
    });
    expect(missingVersion.response.status).toBe(400);
    expect(missingVersion.body.error).toContain("version");

    // The slug the architect typed into dispatch_request_approval, not the document id its
    // result carries: no approval event would ever name it, so the daemon refuses the gate.
    const slug = await json<{ error: string }>("/legion/v1/gates/register", {
      tree: root,
      issue: root,
      artifactId: "spec",
      version: 2,
      ...architect,
    });
    expect(slug.response.status).toBe(400);
    expect(slug.body.error).toContain("artifactId");
    expect(state.gates[root]).toBeUndefined();
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
      artifactId: "4e0aca36-77b3-43bd-96cf-d58890ae64e4",
      version: 2,
      ...architect,
    });
    expect(registered.response.status).toBe(200);
    expect(state.gates[root]).toEqual({
      artifactId: "4e0aca36-77b3-43bd-96cf-d58890ae64e4",
      latestVersion: 2,
      approvedVersion: 2,
    });
    expect(published).toEqual([
      {
        topic: roleTopic(roleToken(state.project, root, "architect")),
        payload: JSON.stringify({ type: "design-approved" }),
      },
    ]);

    // Re-registering the same document at its approved version is already open: wakes nobody twice.
    const again = await json("/legion/v1/gates/register", {
      tree: root,
      issue: root,
      artifactId: "4e0aca36-77b3-43bd-96cf-d58890ae64e4",
      version: 2,
      ...architect,
    });
    expect(again.response.status).toBe(200);
    expect(state.gates[root]).toEqual({
      artifactId: "4e0aca36-77b3-43bd-96cf-d58890ae64e4",
      latestVersion: 2,
      approvedVersion: 2,
    });
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
        runtime: "tmux",
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
        runtime: "tmux",
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
      gitName: "legion-review[bot]",
      gitEmail: "42+legion-review[bot]@users.noreply.github.com",
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
      token: "minted-review-acme",
      appLogin: "legion-review[bot]",
    });
    expect(tokenRoles).toEqual(["review", "review"]);

    const credential = await curl("/legion/v1/git-credential", {
      grantId: grant.body.grantId,
    });
    expect(credential.status).toBe(200);
    expect(credential.body).toBe("username=x-access-token\npassword=minted-review-acme");

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

    // Simulates what `ProcessManager.closeTree`/`retireWorkerLocator`/`removeTreeProcess`
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
  it("resolves the GitHub App owner from the issue's project on every credential route", async () => {
    await start({
      projects: {
        LEGION: { repo: "sjawhar/legion" },
        AGENTC: { repo: "trajectory-labs-pbc/agent-c" },
      },
    });
    const agentcGrant = await mintArchitectGrant("AGENTC-9");

    const token = await json("/legion/v1/gh-token", { grantId: agentcGrant.grantId });
    expect(token.response.status).toBe(200);
    expect(tokenCalls.at(-1)).toEqual(["review", "trajectory-labs-pbc"]);

    const credential = await curl("/legion/v1/git-credential", { grantId: agentcGrant.grantId });
    expect(credential.status).toBe(200);
    expect(tokenCalls.at(-1)).toEqual(["review", "trajectory-labs-pbc"]);

    const legionGrant = await mintArchitectGrant("LEGION-2");
    const legionToken = await json("/legion/v1/gh-token", { grantId: legionGrant.grantId });
    expect(legionToken.response.status).toBe(200);
    expect(tokenCalls.at(-1)).toEqual(["review", "sjawhar"]);
  });

  it("refuses a controller grant on both credential routes: the controller never touches GitHub", async () => {
    await start({ projects: { LEGION: { repo: "sjawhar/legion" } } });
    const grant = await curlJson<GrantResponse>("/legion/v1/grants", {
      sessionId: "ses_controller",
      secret: controllerSecret,
    });
    expect(grant.status).toBe(200);

    for (const route of ["/legion/v1/gh-token", "/legion/v1/git-credential"]) {
      const response = await json<{ error: string }>(route, { grantId: grant.body.grantId });
      expect(response.response.status).toBe(403);
      expect(response.body).toEqual({ error: CONTROLLER_HAS_NO_REPOSITORY });
    }
    expect(tokenCalls).toEqual([]);
  });

  it("rejects the retired merge field on /gh-token as a contract violation", async () => {
    await start();
    const grant = await mintArchitectGrant(root);
    const response = await json("/legion/v1/gh-token", { grantId: grant.grantId, merge: true });
    expect(response.response.status).toBe(400);
  });
  it("refuses a controller grant on phase/complete", async () => {
    await start();
    const controllerGrant = await curlJson<GrantResponse>("/legion/v1/grants", {
      sessionId: "ses_controller",
      secret: controllerSecret,
    });
    expect(controllerGrant.status).toBe(200);

    const completed = await json("/legion/v1/phase/complete", {
      grantId: controllerGrant.body.grantId,
      summary: "done",
    });
    expect(completed.response.status).toBe(403);
    expect(completed.body).toEqual({ error: "A controller grant cannot complete a phase" });
    expect(publications).toEqual([]);
  });
  it("revokes outstanding controller grants when the controller capability is rotated for a respawn, leaving phase-worker grants alone", async () => {
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
    const architectGrant = await curlJson<GrantResponse>("/legion/v1/grants", {
      tree: root,
      issue: root,
      sessionId: "ses_architect",
      secret: started.body.secret,
    });
    expect(architectGrant.status).toBe(200);
    const controllerGrant = await curlJson<GrantResponse>("/legion/v1/grants", {
      sessionId: "ses_controller",
      secret: controllerSecret,
    });
    expect(controllerGrant.status).toBe(200);

    // What `spawnController` does before opening a fresh pane.
    await api?.mintControllerCapability();

    const stale = await json("/legion/v1/gh-token", {
      grantId: controllerGrant.body.grantId,
      merge: true,
    });
    expect(stale.response.status).toBe(403);
    expect(tokenRoles).toEqual([]);
    // The architect's grant was minted by a different capability and outlives the rotation (an
    // architect acts as the review App — `appRoleForLegionRole`, LEGION-42).
    const survivor = await json("/legion/v1/gh-token", { grantId: architectGrant.body.grantId });
    expect(survivor.response.status).toBe(200);
    expect(tokenRoles).toEqual(["review"]);
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
        runtime: "tmux",
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
  it("accepts a first worker/started registration from a persisted boot-token hash after a restart", async () => {
    await start();
    const testerToken = roleToken(state.project, root, "tester");
    const workerBootToken = await api?.mintWorkerBootToken(root, root, "tester", 3);
    if (!workerBootToken) throw new Error("worker boot token was not minted");
    // Simulates the fresh-generation state `launchWorker` persists before its worker gets as far
    // as /worker/started. A restart at this point leaves only the boot-token hash to resolve.
    state.roles[testerToken] = {
      issue: root,
      role: "tester",
      generation: 3,
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@1",
        tmuxPaneId: "%1",
        socketPath: "/state/workers/tester.sock",
      },
      bootTokenHash: secretHash(workerBootToken).toString("hex"),
    };

    api?.stop();
    await start({ state });

    const body = {
      tree: root,
      issue: root,
      role: "tester",
      bootToken: workerBootToken,
      sessionId: "ses_tester",
      agentId: "agent-tester",
      ompSessionFile: "/tmp/tester.json",
    };
    const started = await json<{ secret: string }>("/legion/v1/worker/started", body);
    expect(started.response.status).toBe(200);
    expect(started.body.secret).toEqual(expect.any(String));
    expect(state.roles[testerToken]).toMatchObject({
      sessionId: "ses_tester",
      agentId: "agent-tester",
      locator: { ompSessionFile: "/tmp/tester.json" },
    });
    const beforeReplay = structuredClone(state.roles[testerToken]);

    // Recreate the persisted-hash route after the first registration has recorded its session.
    api?.stop();
    await start({ state });

    const replay = await json<{ secret: string }>("/legion/v1/worker/started", body);
    expect(replay.response.status).toBe(200);
    expect(replay.body.secret).toEqual(expect.any(String));
    expect(replay.body.secret).not.toBe(started.body.secret);
    expect(state.roles[testerToken]).toEqual(beforeReplay);
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
  it("registers a worker session from a valid worker boot token without touching the issue's active phase", async () => {
    await start();
    const token = roleToken(state.project, root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      locator: {
        runtime: "tmux",
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
    expect(started.body.gitName).toBe("legion-review[bot]");
    expect(state.roles[token]).toMatchObject({
      sessionId: "ses_tester",
      agentId: "agt_tester",
      locator: { ompSessionFile: "/tmp/tester.json" },
    });
    expect(state.phases[root]).toBeUndefined();
  });

  it("keeps the implementer as the active phase when a finished reviewer's relaunch registers, and keeps routing to the implementer", async () => {
    await start();
    const implementerToken = roleToken(state.project, root, "implementer");
    const reviewerToken = roleToken(state.project, root, "reviewer");
    // The reviewer finished at 14:29 and was retired; the implementer is doing the retro. The
    // daemon relaunched the reviewer (a lost connection, a restart) as generation 2 of the same
    // agent -- exactly LEGION-14's shape when the implementer's completion was refused.
    state.phases[root] = { phase: "implementer", sessionId: "ses_implementer" };
    state.roles[implementerToken] = {
      issue: root,
      role: "implementer",
      sessionId: "ses_implementer",
      generation: 1,
      readyConfirmedAt: now,
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%1",
        socketPath: "/state/workers/implementer.sock",
      },
    };
    state.roles[reviewerToken] = {
      issue: root,
      role: "reviewer",
      generation: 2,
      expectedSessionId: "ses_reviewer",
      resumeSessionFile: "/tmp/reviewer.json",
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%2",
        socketPath: "/state/workers/reviewer.sock",
        ompSessionFile: "/tmp/reviewer.json",
      },
    };
    const bootToken = await api?.mintWorkerBootToken(root, root, "reviewer", 2, "ses_reviewer");
    if (!bootToken) throw new Error("worker boot token was not minted");

    const started = await json<{ roleToken: string }>("/legion/v1/worker/started", {
      tree: root,
      issue: root,
      role: "reviewer",
      bootToken,
      sessionId: "ses_reviewer",
      agentId: "agt_reviewer",
      ompSessionFile: "/tmp/reviewer.json",
    });

    expect(started.response.status).toBe(200);
    expect(started.body.roleToken).toBe(reviewerToken);
    expect(state.roles[reviewerToken]).toMatchObject({ sessionId: "ses_reviewer", generation: 2 });
    expect(state.phases[root]).toEqual({ phase: "implementer", sessionId: "ses_implementer" });
    // The green-CI wake for the retro commit still reaches the implementer, not the relaunched
    // reviewer.
    const payload = { type: "ci-green" as const, sha: "retro-head" };
    expect(routeActive(state, root, payload)).toEqual([
      { kind: "publish", role: implementerToken, payload },
    ]);
  });

  it("accepts the implementer's completion after a finished reviewer relaunched", async () => {
    await start();
    const implementerToken = roleToken(state.project, root, "implementer");
    const reviewerToken = roleToken(state.project, root, "reviewer");
    state.roles[implementerToken] = {
      issue: root,
      role: "implementer",
      generation: 1,
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%1",
        socketPath: "/state/workers/implementer.sock",
      },
    };
    const implementerBoot = await api?.mintWorkerBootToken(root, root, "implementer", 1);
    if (!implementerBoot) throw new Error("worker boot token was not minted");
    const implementerStarted = await json<{ secret: string }>("/legion/v1/worker/started", {
      tree: root,
      issue: root,
      role: "implementer",
      bootToken: implementerBoot,
      sessionId: "ses_implementer",
      agentId: "agt_implementer",
      ompSessionFile: "/tmp/implementer.json",
    });
    expect(implementerStarted.response.status).toBe(200);
    // The architect's assignment reached the implementer (the one write of the phase).
    state.phases[root] = { phase: "implementer", sessionId: "ses_implementer" };

    state.roles[reviewerToken] = {
      issue: root,
      role: "reviewer",
      generation: 2,
      expectedSessionId: "ses_reviewer",
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%2",
        socketPath: "/state/workers/reviewer.sock",
        ompSessionFile: "/tmp/reviewer.json",
      },
    };
    const reviewerBoot = await api?.mintWorkerBootToken(root, root, "reviewer", 2, "ses_reviewer");
    if (!reviewerBoot) throw new Error("worker boot token was not minted");
    const reviewerStarted = await json("/legion/v1/worker/started", {
      tree: root,
      issue: root,
      role: "reviewer",
      bootToken: reviewerBoot,
      sessionId: "ses_reviewer",
      agentId: "agt_reviewer",
      ompSessionFile: "/tmp/reviewer.json",
    });
    expect(reviewerStarted.response.status).toBe(200);
    expect(state.phases[root]).toEqual({ phase: "implementer", sessionId: "ses_implementer" });

    const grantId = await mintGrant(root, "ses_implementer", implementerStarted.body.secret);
    const complete = await json("/legion/v1/phase/complete", {
      grantId,
      summary: "Retro recorded",
    });

    expect(complete.response.status).toBe(200);
    expect(state.phases[root]).toBeUndefined();
    expect(publications).toContainEqual({
      topic: roleTopic(roleToken(state.project, root, "architect")),
      payload: JSON.stringify({
        type: "phase-complete",
        issue: root,
        role: "implementer",
        summary: "Retro recorded",
      }),
    });
  });

  it("accepts the mid-turn reviewer's completion after the finished implementer's relaunch registered, keeping the assignment time on the record until then (LEGION-27)", async () => {
    // The completion half of the LEGION-27 sequence (processes.test.ts holds the recovery half):
    // the reviewer is the active phase, still inside the turn that runs `legion handoff complete`,
    // when the pane a pre-#991 daemon's own recovery relaunched for the finished implementer
    // registers through /worker/started. That registration must leave the reviewer's record —
    // role, session, and the time its assignment was delivered — exactly as written, so the
    // completion that follows is accepted, clears the record, and reaches the architect, and no
    // refusal is logged.
    await start();
    const errorSpy = spyOn(console, "error").mockImplementation(() => {});
    const infoSpy = spyOn(console, "info").mockImplementation(() => {});
    try {
      const reviewerToken = roleToken(state.project, root, "reviewer");
      const implementerToken = roleToken(state.project, root, "implementer");
      state.roles[reviewerToken] = {
        issue: root,
        role: "reviewer",
        generation: 1,
        locator: {
          runtime: "tmux",
          tmuxSession: "legion-omp",
          tmuxWindowId: "@42",
          tmuxPaneId: "%1",
          socketPath: "/state/workers/reviewer.sock",
        },
      };
      const reviewerBoot = await api?.mintWorkerBootToken(root, root, "reviewer", 1);
      if (!reviewerBoot) throw new Error("worker boot token was not minted");
      const reviewerStarted = await json<{ secret: string }>("/legion/v1/worker/started", {
        tree: root,
        issue: root,
        role: "reviewer",
        bootToken: reviewerBoot,
        sessionId: "ses_reviewer",
        agentId: "agt_reviewer",
        ompSessionFile: "/tmp/reviewer.json",
      });
      expect(reviewerStarted.response.status).toBe(200);
      // The architect's assignment reached the reviewer (the one write of the phase, stamped).
      const assigned = {
        phase: "reviewer",
        sessionId: "ses_reviewer",
        assignedAt: "2026-09-13T05:20:00.000Z",
      };
      state.phases[root] = { ...assigned };

      // The finished implementer, relaunched with --resume: a new generation, the same agent.
      state.roles[implementerToken] = {
        issue: root,
        role: "implementer",
        generation: 2,
        expectedSessionId: "ses_implementer",
        resumeSessionFile: "/tmp/implementer.json",
        locator: {
          runtime: "tmux",
          tmuxSession: "legion-omp",
          tmuxWindowId: "@42",
          tmuxPaneId: "%2",
          socketPath: "/state/workers/implementer.sock",
          ompSessionFile: "/tmp/implementer.json",
        },
      };
      const implementerBoot = await api?.mintWorkerBootToken(
        root,
        root,
        "implementer",
        2,
        "ses_implementer"
      );
      if (!implementerBoot) throw new Error("worker boot token was not minted");
      const implementerStarted = await json("/legion/v1/worker/started", {
        tree: root,
        issue: root,
        role: "implementer",
        bootToken: implementerBoot,
        sessionId: "ses_implementer",
        agentId: "agt_implementer",
        ompSessionFile: "/tmp/implementer.json",
      });
      expect(implementerStarted.response.status).toBe(200);
      expect(state.phases[root]).toEqual(assigned);

      const grantId = await mintGrant(root, "ses_reviewer", reviewerStarted.body.secret);
      const complete = await json("/legion/v1/phase/complete", {
        grantId,
        summary: "Round 3 reviewed",
      });

      expect(complete.response.status).toBe(200);
      expect(state.phases[root]).toBeUndefined();
      expect(publications).toContainEqual({
        topic: roleTopic(roleToken(state.project, root, "architect")),
        payload: JSON.stringify({
          type: "phase-complete",
          issue: root,
          role: "reviewer",
          summary: "Round 3 reviewed",
        }),
      });
      expect(refusalLines(errorSpy)).toEqual([]);
    } finally {
      infoSpy.mockRestore();
      errorSpy.mockRestore();
    }
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
        runtime: "tmux",
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

  it("rejects a worker boot token that has already been consumed, logging both sessions and changing nothing", async () => {
    await start();
    const token = roleToken(state.project, root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      locator: {
        runtime: "tmux",
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
    const before = structuredClone(state.roles[token]);
    const warnSpy = spyOn(console, "warn").mockImplementation(() => {});
    try {
      const replay = await json("/legion/v1/worker/started", {
        ...body,
        sessionId: "ses_tester_2",
      });
      expect(replay.response.status).toBe(403);
      expect((replay.body as { error: string }).error).toBe("Invalid worker boot token");
      const line = warnSpy.mock.calls
        .map((call) => String(call[0]))
        .find((entry) => entry.includes(token) && entry.includes("refused /worker/started"));
      expect(line).toContain("from session ses_tester_2");
      expect(line).toContain("already registered session ses_tester");
      expect(line).toContain("generation 1");
      // Registered but never reached /worker/ready: the line says so instead of a timestamp.
      expect(line).toContain("(not yet ready)");
    } finally {
      warnSpy.mockRestore();
    }
    expect(state.roles[token]).toEqual(before);
  });

  it("refuses a worker/started registration from a different session once the claim registered one at this generation — after a daemon restart, on the persisted boot-token hash — logging both sessions and changing nothing", async () => {
    await start();
    const token = roleToken(state.project, root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      locator: {
        runtime: "tmux",
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
    // What `workerReady` (processes.ts) persists once the shim connects and the assignment is
    // delivered, and what the architect's assignment delivery writes: the worker is confirmed
    // ready and is the issue's active phase — the incident's exact state.
    const registered = state.roles[token];
    if (!registered || !("issue" in registered)) throw new Error("claim was not registered");
    registered.readyConfirmedAt = now;
    state.phases[root] = { phase: "tester", sessionId: "ses_tester" };
    const before = structuredClone(state.roles[token]);

    // Restart: the in-memory mint record is gone; only the persisted `bootTokenHash` resolves the
    // token, so this is the path that rebound LEGION-39's tester.
    api?.stop();
    await start({ state });

    const warnSpy = spyOn(console, "warn").mockImplementation(() => {});
    try {
      const foreign = await json<{ error: string }>("/legion/v1/worker/started", {
        ...body,
        sessionId: "ses_intruder",
        agentId: "agt_intruder",
        ompSessionFile: "/tmp/intruder.json",
      });
      expect(foreign.response.status).toBe(403);
      expect(foreign.body.error).toBe("Invalid worker boot token");
      const line = warnSpy.mock.calls
        .map((call) => String(call[0]))
        .find((entry) => entry.includes(token) && entry.includes("refused /worker/started"));
      expect(line).toContain("from session ses_intruder");
      expect(line).toContain("already registered session ses_tester");
      expect(line).toContain("generation 1");
      expect(line).toContain(`ready confirmed at ${new Date(now).toISOString()}`);
    } finally {
      warnSpy.mockRestore();
    }
    // Nothing on the claim moved: session, agent, locator (incl. its ompSessionFile), hash, ready.
    expect(state.roles[token]).toEqual(before);

    // The worker that did the work still completes: it rebinds its capability through the durable
    // recovery route, mints a grant, and its completion is accepted — the incident's lost step.
    const recovered = await json<WorkerSessionResponse>("/legion/v1/worker-session", {
      sessionId: "ses_tester",
      recoveryToken: bootToken,
    });
    expect(recovered.response.status).toBe(200);
    const grantId = await mintGrant(root, "ses_tester", recovered.body.secret);
    const complete = await json("/legion/v1/phase/complete", {
      grantId,
      summary: "Verified the acceptance criteria",
    });
    expect(complete.response.status).toBe(200);
    expect(state.phases[root]).toBeUndefined();
  });

  it("accepts a same-session worker/started replay as idempotent, reissuing a secret", async () => {
    await start();
    const token = roleToken(state.project, root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      locator: {
        runtime: "tmux",
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
        runtime: "tmux",
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
        runtime: "tmux",
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
        runtime: "tmux",
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

  it("rolls back the claim mutation when the state save fails, leaving a clean retry", async () => {
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
        runtime: "tmux",
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
        runtime: "tmux",
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
    expect(state.phases[root]).toBeUndefined();
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
        runtime: "tmux",
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
        runtime: "tmux",
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
      pendingAssignment: {
        kind: "assignment",
        task: "verify #41",
        queuedAt: "2026-08-24T00:00:00.000Z",
        deliveryId: "00000000-0000-4000-8000-000000000001",
      },
      locator: {
        runtime: "tmux",
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
      pendingAssignment: {
        kind: "assignment",
        task: "verify #41",
        queuedAt: "2026-08-24T00:00:00.000Z",
        deliveryId: "00000000-0000-4000-8000-000000000001",
      },
      locator: {
        runtime: "tmux",
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
        runtime: "tmux",
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

    const spawn = await json<{ status: string; roleToken: string }>(
      "/legion/v1/worker/spawn",
      spawnBody({ secret: started.body.secret })
    );

    expect(spawn.response.status).toBe(200);
    expect(spawn.body).toEqual({
      status: "spawned",
      roleToken: roleToken(state.project, root, "planner"),
    });
    expect(spawnedWorkers).toEqual([{ tree: root, issue: root, role: "planner", task: "plan #1" }]);
  });

  async function architectSecret(): Promise<string> {
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
    return started.body.secret;
  }

  /** A `/legion/v1/worker/spawn` body: the architect capability plus the spawn fields, with a
   * fresh `requestId` per call unless the test pins one to exercise the daemon's dedupe. */
  function spawnBody(overrides: Record<string, unknown>): Record<string, unknown> {
    return {
      tree: root,
      issue: root,
      sessionId: "ses_root",
      role: "planner",
      task: "plan #1",
      requestId: randomUUID(),
      ...overrides,
    };
  }

  function recordingDispatchClient(): {
    client: LegionApiDeps["dispatchClient"];
    statusWrites: Array<{ issue: IssueKey; status: string }>;
  } {
    const statusWrites: Array<{ issue: IssueKey; status: string }> = [];
    return {
      statusWrites,
      client: fakeDispatchClient({
        setStatus: async (issue, status) => {
          statusWrites.push({ issue, status });
        },
      }),
    };
  }

  it("writes nothing when the architect spawns a worker for an issue a human moved back to todo", async () => {
    // A root gets `in_progress` from `spawnTree` and a child from its first sub-architect spawn, so
    // a phase worker spawned on an issue at `todo` is answering a human's move; the spawn must not
    // override it.
    const dispatch = recordingDispatchClient();
    await start({ dispatchClient: dispatch.client });
    state.issues[root].status = "todo";
    const secret = await architectSecret();

    const spawn = await json<{ status: string }>("/legion/v1/worker/spawn", spawnBody({ secret }));

    expect(spawn.response.status).toBe(200);
    expect(spawn.body.status).toBe("spawned");
    expect(dispatch.statusWrites).toEqual([]);
  });

  it("writes in_progress once when the architect spawns a released child's sub-architect", async () => {
    const dispatch = recordingDispatchClient();
    await start({ dispatchClient: dispatch.client });
    state.issues[root].children = [child];
    state.issues[child] = {
      key: child,
      title: "Child",
      parent: root,
      status: "todo",
      children: [],
    };
    const secret = await architectSecret();

    const spawn = await json<{ status: string; roleToken: string }>(
      "/legion/v1/worker/spawn",
      spawnBody({ issue: child, secret, role: "architect", task: "own this child" })
    );

    expect(spawn.response.status).toBe(200);
    expect(spawn.body).toEqual({
      status: "spawned",
      roleToken: roleToken(state.project, child, "architect"),
    });
    expect(spawnedWorkers).toEqual([
      { tree: root, issue: child, role: "architect", task: "own this child" },
    ]);
    expect(dispatch.statusWrites).toEqual([{ issue: child, status: "in_progress" }]);

    // Dispatch echoes the write back through the durable lane; the next spawn for the same live
    // sub-architect (a new task for it) writes nothing.
    state.issues[child].status = "in_progress";
    const again = await json<{ status: string }>(
      "/legion/v1/worker/spawn",
      spawnBody({ issue: child, secret, role: "architect", task: "re-scope the child" })
    );
    expect(again.response.status).toBe(200);
    expect(dispatch.statusWrites).toEqual([{ issue: child, status: "in_progress" }]);
  });

  it("writes in_progress when the architect spawns a corrective implementer while the PR's latest review is changes requested", async () => {
    const dispatch = recordingDispatchClient();
    await start({ dispatchClient: dispatch.client });
    state.issues[root].status = "retro";
    state.prs["acme/widgets#9"] = checkPr(root, { number: 9, reviewDecision: "changes_requested" });
    const secret = await architectSecret();

    const spawn = await json(
      "/legion/v1/worker/spawn",
      spawnBody({ secret, role: "implementer", task: "address the human's review" })
    );

    expect(spawn.response.status).toBe(200);
    expect(dispatch.statusWrites).toEqual([{ issue: root, status: "in_progress" }]);
  });

  it("writes nothing when the architect spawns the implementer at retro under an approved review", async () => {
    const dispatch = recordingDispatchClient();
    await start({ dispatchClient: dispatch.client });
    state.issues[root].status = "retro";
    state.prs["acme/widgets#9"] = checkPr(root, { number: 9, reviewDecision: "approved" });
    const secret = await architectSecret();

    const spawn = await json(
      "/legion/v1/worker/spawn",
      spawnBody({ secret, role: "implementer", task: "run retro" })
    );

    expect(spawn.response.status).toBe(200);
    expect(dispatch.statusWrites).toEqual([]);
  });

  it("writes nothing when the architect spawns the implementer at retro with no pull request recorded", async () => {
    const dispatch = recordingDispatchClient();
    await start({ dispatchClient: dispatch.client });
    state.issues[root].status = "retro";
    const secret = await architectSecret();

    const spawn = await json(
      "/legion/v1/worker/spawn",
      spawnBody({ secret, role: "implementer", task: "run retro" })
    );

    expect(spawn.response.status).toBe(200);
    expect(dispatch.statusWrites).toEqual([]);
  });

  it("refuses spawn_worker for a non-architect session capability", async () => {
    await start();
    const token = roleToken(state.project, root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      locator: {
        runtime: "tmux",
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

    const spawn = await json(
      "/legion/v1/worker/spawn",
      spawnBody({ sessionId: "ses_tester", secret: started.body.secret })
    );

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

    const spawn = await json(
      "/legion/v1/worker/spawn",
      spawnBody({ secret: started.body.secret, role: "architect", task: "reboot" })
    );

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

    const spawn = await json<{ error: string }>(
      "/legion/v1/worker/spawn",
      spawnBody({ secret: started.body.secret })
    );

    expect(spawn.response.status).toBe(409);
    expect(spawn.body.error).toContain(root);
    expect(spawnedWorkers).toEqual([]);
  });

  /** Polls `condition` across real macrotask ticks (`setImmediate`, never a wall-clock wait)
   * until it holds — the awaited chain is an HTTP request reaching a handler on the same event
   * loop — bounded so a broken expectation fails the test instead of hanging it. */
  async function waitUntil(condition: () => boolean, maxTicks = 20_000): Promise<void> {
    for (let tick = 0; tick < maxTicks; tick += 1) {
      if (condition()) return;
      const { promise, resolve } = Promise.withResolvers<void>();
      setImmediate(resolve);
      await promise;
    }
    throw new Error("condition did not hold within the wait bound");
  }

  /** Fires two spawn requests with one body and holds the caller until both have reached the
   * daemon: the first is inside `spawnWorkerImpl` (`started()` true) and the second has been
   * answered from the ledger (its `repeated` log line). Without the second wait a request still
   * on the wire when the gate opens arrives after the entry settled and runs its own spawn. */
  async function twoSpawnsInFlight<T>(body: Record<string, unknown>, started: () => boolean) {
    const infoSpy = spyOn(console, "info").mockImplementation(() => {});
    try {
      const a = json<T>("/legion/v1/worker/spawn", body);
      const b = json<T>("/legion/v1/worker/spawn", body);
      await waitUntil(
        () =>
          started() &&
          infoSpy.mock.calls.some(
            (call) =>
              String(call[0]).includes("spawn request") && String(call[0]).includes("repeated")
          )
      );
      return [a, b] as const;
    } finally {
      infoSpy.mockRestore();
    }
  }

  it("answers a repeated spawn requestId with the first result and runs nothing again — no second process-manager call, Dispatch write, or save", async () => {
    const dispatch = recordingDispatchClient();
    let saves = 0;
    await start({
      dispatchClient: dispatch.client,
      saveState: async () => {
        saves += 1;
      },
    });
    state.issues[root].status = "retro";
    state.prs["acme/widgets#9"] = checkPr(root, { number: 9, reviewDecision: "changes_requested" });
    const secret = await architectSecret();
    const body = spawnBody({ role: "implementer", task: "address the review", secret });

    const first = await json("/legion/v1/worker/spawn", body);
    const savesAfterFirst = saves;
    const publicationsAfterFirst = publications.length;
    const second = await json("/legion/v1/worker/spawn", body);

    expect(first.response.status).toBe(200);
    expect(second.response.status).toBe(200);
    expect(second.body).toEqual(first.body);
    expect(spawnedWorkers).toHaveLength(1);
    expect(dispatch.statusWrites).toEqual([{ issue: root, status: "in_progress" }]);
    expect(saves).toBe(savesAfterFirst);
    expect(publications).toHaveLength(publicationsAfterFirst);
  });

  it("returns an accepted spawn request's persisted result after a daemon restart without reprocessing it", async () => {
    const tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-spawn-request-"));
    const file = path.join(tempDir, "state.json");
    try {
      await start({ saveState: async () => saveState(file, state) });
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
      const body = spawnBody({ secret: started.body.secret });

      const first = await json<{ status: string; roleToken: string }>(
        "/legion/v1/worker/spawn",
        body
      );
      expect(first.response.status).toBe(200);
      expect(spawnedWorkers).toHaveLength(1);

      api?.stop();
      state = await loadState(file, { project: "omp", cap: 2 });
      await start({ state, mintController: false, saveState: async () => saveState(file, state) });

      const recovered = await json<WorkerSessionResponse>("/legion/v1/worker-session", {
        sessionId: "ses_root",
        recoveryToken: bootToken,
      });
      expect(recovered.response.status).toBe(200);
      const replay = await json<{ status: string; roleToken: string }>("/legion/v1/worker/spawn", {
        ...body,
        secret: recovered.body.secret,
      });
      expect(replay.response.status).toBe(200);
      expect(replay.body).toEqual(first.body);
      expect(spawnedWorkers).toHaveLength(1);
    } finally {
      await rm(tempDir, { recursive: true, force: true });
    }
  });

  it("a repeat that arrives while the original is still running waits for and shares its result", async () => {
    const gate = Promise.withResolvers<void>();
    let calls = 0;
    await start({
      spawnWorkerImpl: async (_tree, issue, role) => {
        calls += 1;
        await gate.promise;
        return { status: "queued", roleToken: roleToken(state.project, issue, role) };
      },
    });
    const secret = await architectSecret();
    const body = spawnBody({ secret });

    const [a, b] = await twoSpawnsInFlight(body, () => calls === 1);
    gate.resolve();
    const [ra, rb] = await Promise.all([a, b]);

    expect(ra.response.status).toBe(200);
    expect(rb.response.status).toBe(200);
    const expected = { status: "queued", roleToken: roleToken(state.project, root, "planner") };
    expect(ra.body).toEqual(expected);
    expect(rb.body).toEqual(expected);
    expect(calls).toBe(1);
  });

  it("every request sharing a requestId receives the same error, and the id can be used again once it has settled", async () => {
    const gate = Promise.withResolvers<void>();
    let calls = 0;
    let explode = true;
    await start({
      spawnWorkerImpl: async (_tree, issue, role) => {
        calls += 1;
        await gate.promise;
        if (explode) throw new Error("tmux exploded");
        return { status: "spawned", roleToken: roleToken(state.project, issue, role) };
      },
    });
    const secret = await architectSecret();
    const body = spawnBody({ secret });

    const [a, b] = await twoSpawnsInFlight<{ error: string }>(body, () => calls === 1);
    gate.resolve();
    const [ra, rb] = await Promise.all([a, b]);

    expect(ra.response.status).toBe(500);
    expect(rb.response.status).toBe(500);
    expect(ra.body.error).toBe("tmux exploded");
    expect(rb.body.error).toBe("tmux exploded");
    expect(calls).toBe(1);

    explode = false;
    const again = await json("/legion/v1/worker/spawn", body);
    expect(again.response.status).toBe(200);
    expect(calls).toBe(2);
  });

  it("refuses a repeated requestId whose body differs with 409 naming the id", async () => {
    await start();
    state.issues[root].children = [child];
    state.issues[child] = {
      key: child,
      title: "Child",
      parent: root,
      status: "todo",
      children: [],
    };
    const secret = await architectSecret();
    const requestId = randomUUID();
    const body = spawnBody({ secret, requestId });

    const first = await json("/legion/v1/worker/spawn", body);
    expect(first.response.status).toBe(200);

    const otherTask = await json<{ error: string }>("/legion/v1/worker/spawn", {
      ...body,
      task: "plan #2",
    });
    expect(otherTask.response.status).toBe(409);
    expect(otherTask.body.error).toContain(requestId);

    const otherIssue = await json<{ error: string }>("/legion/v1/worker/spawn", {
      ...body,
      issue: child,
    });
    expect(otherIssue.response.status).toBe(409);
    expect(otherIssue.body.error).toContain(requestId);

    expect(spawnedWorkers).toHaveLength(1);
  });

  it("rejects a spawn without a UUID requestId with 400 naming the field", async () => {
    await start();
    const secret = await architectSecret();
    const { requestId: _dropped, ...withoutRequestId } = spawnBody({ secret });

    const missing = await json<{ error: string }>("/legion/v1/worker/spawn", withoutRequestId);
    expect(missing.response.status).toBe(400);
    expect(missing.body.error).toContain("requestId");

    const malformed = await json<{ error: string }>(
      "/legion/v1/worker/spawn",
      spawnBody({ secret, requestId: "not-a-uuid" })
    );
    expect(malformed.response.status).toBe(400);
    expect(malformed.body.error).toContain("requestId");

    expect(spawnedWorkers).toEqual([]);
  });

  it("forgets a settled spawn request after ten minutes: the same id is processed again", async () => {
    await start();
    const secret = await architectSecret();
    const body = spawnBody({ secret });
    const control = spawnBody({ secret, role: "tester", task: "verify #1" });

    expect((await json("/legion/v1/worker/spawn", body)).response.status).toBe(200);
    expect(spawnedWorkers).toHaveLength(1);

    // Control: a repeat one millisecond short of the retention window is still deduped.
    expect((await json("/legion/v1/worker/spawn", control)).response.status).toBe(200);
    expect(spawnedWorkers).toHaveLength(2);
    now += 10 * 60_000 - 1;
    expect((await json("/legion/v1/worker/spawn", control)).response.status).toBe(200);
    expect(spawnedWorkers).toHaveLength(2);

    // The first request is now past the window; the same id runs the spawn again.
    now += 2;
    expect((await json("/legion/v1/worker/spawn", body)).response.status).toBe(200);
    expect(spawnedWorkers).toHaveLength(3);
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

  /** Every `[legion] phase/complete refused …` line a `console.error` spy saw, one string per
   * call (LEGION-72): the refusal log is the one place a 403/404/409 from that route says what
   * the grant named and what the daemon held. */
  function refusalLines(spy: { mock: { calls: unknown[][] } }): string[] {
    return spy.mock.calls
      .map((call) => call.map(String).join(" "))
      .filter((line) => line.startsWith("[legion] phase/complete refused "));
  }

  /** How a refusal line names a grant: the first twelve hex characters of its `secretHash`, the
   * hash claims keep for boot tokens — never the id, which is the credential itself. */
  function grantRef(grantId: string): string {
    return `grant ${secretHash(grantId).toString("hex").slice(0, 12)}`;
  }

  it("publishes phase-complete to the tree's architect, clears the phase, and keeps the worker's role claim", async () => {
    await start();
    const token = roleToken(state.project, root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      locator: {
        runtime: "tmux",
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

  /** A child under the root whose sub-architect holds a claim with a recorded pane. */
  function attachClaimedChild(): void {
    state.issues[root].children.push(child);
    state.issues[child] = {
      key: child,
      title: "Child",
      parent: root,
      status: "in_progress",
      children: [],
    };
    state.roles[roleToken(state.project, child, "architect")] = {
      issue: child,
      role: "architect",
      sessionId: "ses_sub",
      generation: 1,
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@43",
        tmuxPaneId: "%9",
        socketPath: "/state/workers/child-architect.sock",
      },
    };
  }

  /** Registers an implementer on the child through `/worker/started` and returns a grant for
   * its completion. */
  async function childImplementerGrant(): Promise<string> {
    const token = roleToken(state.project, child, "implementer");
    state.roles[token] = {
      issue: child,
      role: "implementer",
      generation: 1,
      locator: {
        runtime: "tmux",
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
    return mintGrant(child, "ses_implementer", started.body.secret);
  }

  it("publishes phase-complete for a child issue to its claimed sub-architect, not the root's architect", async () => {
    await start();
    attachClaimedChild();
    const grantId = await childImplementerGrant();

    const complete = await json("/legion/v1/phase/complete", {
      grantId,
      summary: "Implemented the change",
    });

    expect(complete.response.status).toBe(200);
    expect(publications).toContainEqual({
      topic: roleTopic(roleToken(state.project, child, "architect")),
      payload: JSON.stringify({
        type: "phase-complete",
        issue: child,
        role: "implementer",
        summary: "Implemented the change",
      }),
    });
    expect(
      publications.some((publication) =>
        publication.topic.includes(roleToken(state.project, root, "architect"))
      )
    ).toBeFalse();
    expect(state.phases[child]).toBeUndefined();
    // A delivered completion recovers no role.
    expect(recoveredRoles).toEqual([]);
  });

  it("publishes phase-complete for a child issue with no sub-architect claim to the root's architect", async () => {
    await start();
    state.issues[root].children.push(child);
    state.issues[child] = {
      key: child,
      title: "Child",
      parent: root,
      status: "in_progress",
      children: [],
    };
    const grantId = await childImplementerGrant();

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

  it("publishes a sub-architect's own phase-complete to the parent's architect, not its own topic", async () => {
    await start();
    attachClaimedChild();
    const bootToken = await api?.mintWorkerBootToken(root, child, "architect", 1);
    if (!bootToken) throw new Error("worker boot token was not minted");
    const started = await json<{ secret: string }>("/legion/v1/worker/started", {
      tree: root,
      issue: child,
      role: "architect",
      bootToken,
      sessionId: "ses_sub",
      agentId: "agt_sub",
      ompSessionFile: "/tmp/sub-architect.json",
    });
    expect(started.response.status).toBe(200);
    state.phases[child] = { phase: "architect", sessionId: "ses_sub" };
    const grantId = await mintGrant(child, "ses_sub", started.body.secret);

    const complete = await json("/legion/v1/phase/complete", {
      grantId,
      summary: "Child tree done",
    });

    expect(complete.response.status).toBe(200);
    expect(publications).toContainEqual({
      topic: roleTopic(roleToken(state.project, root, "architect")),
      payload: JSON.stringify({
        type: "phase-complete",
        issue: child,
        role: "architect",
        summary: "Child tree done",
      }),
    });
    expect(
      publications.some((publication) =>
        publication.topic.includes(roleToken(state.project, child, "architect"))
      )
    ).toBeFalse();
  });

  it("records a child's completion and resumes its owning sub-architect when that architect has no live holder", async () => {
    await start({
      envoyPublish: async (topic) => {
        throw new EnvoyPublishError(topic, 404);
      },
    });
    attachClaimedChild();
    const grantId = await childImplementerGrant();

    const complete = await json("/legion/v1/phase/complete", { grantId, summary: "smoke" });

    expect(complete.response.status).toBe(202);
    expect(state.phases[child]).toEqual({
      phase: "implementer",
      sessionId: "ses_implementer",
      completed: { summary: "smoke", at: new Date(now).toISOString() },
    });
    // The owning sub-architect is recovered so its snapshot replays the completion just recorded.
    expect(recoveredRoles).toEqual([roleToken(state.project, child, "architect")]);
  });

  it("logs and still answers 202 when resuming the owning sub-architect after a no-holder completion throws", async () => {
    await start({
      envoyPublish: async (topic) => {
        throw new EnvoyPublishError(topic, 404);
      },
      recoverRoleImpl: async () => {
        throw new Error("tree closing");
      },
    });
    attachClaimedChild();
    const grantId = await childImplementerGrant();
    // The resume is fire-and-forget, so the response never waits on it: await the one log line
    // its `.catch` writes rather than a guessed delay.
    const { promise: logged, resolve: markLogged } = Promise.withResolvers<void>();
    const errorSpy = spyOn(console, "error").mockImplementation((...args: unknown[]) => {
      if (args.map(String).join(" ").includes("recovering its owning architect")) markLogged();
    });
    try {
      const complete = await json("/legion/v1/phase/complete", { grantId, summary: "smoke" });
      await logged;

      expect(complete.response.status).toBe(202);
      expect(state.phases[child]).toEqual({
        phase: "implementer",
        sessionId: "ses_implementer",
        completed: { summary: "smoke", at: new Date(now).toISOString() },
      });
      const recoveryLines = errorSpy.mock.calls
        .map((call) => call.map(String).join(" "))
        .filter((line) => line.includes("recovering its owning architect"));
      expect(recoveryLines).toHaveLength(1);
      expect(recoveryLines[0]).toContain(roleToken(state.project, child, "architect"));
      expect(recoveryLines[0]).toContain("tree closing");
    } finally {
      errorSpy.mockRestore();
    }
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
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%1",
        socketPath: "/state/workers/reviewer.sock",
      },
    };
    state.prs["acme/widgets#9"] = checkPr(root, { number: 9, reviewDecision: "changes_requested" });
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
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%1",
        socketPath: "/state/workers/reviewer.sock",
      },
    };
    state.prs["acme/widgets#9"] = checkPr(root, { number: 9, reviewDecision: "approved" });
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

  it("advances the issue to testing when an implementer completes from in_progress", async () => {
    const statusWrites: Array<{ issue: IssueKey; status: string }> = [];
    await start({
      dispatchClient: fakeDispatchClient({
        setStatus: async (issue, status) => {
          statusWrites.push({ issue, status });
        },
      }),
    });
    state.issues[root].status = "in_progress";
    const token = roleToken(state.project, root, "implementer");
    state.roles[token] = {
      issue: root,
      role: "implementer",
      generation: 1,
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%1",
        socketPath: "/state/workers/implementer.sock",
      },
    };
    const bootToken = await api?.mintWorkerBootToken(root, root, "implementer", 1);
    if (!bootToken) throw new Error("worker boot token was not minted");
    const started = await json<{ secret: string }>("/legion/v1/worker/started", {
      tree: root,
      issue: root,
      role: "implementer",
      bootToken,
      sessionId: "ses_implementer",
      agentId: "agt_implementer",
      ompSessionFile: "/tmp/implementer.json",
    });
    expect(started.response.status).toBe(200);
    state.phases[root] = { phase: "implementer", sessionId: "ses_implementer" };
    const grantId = await mintGrant(root, "ses_implementer", started.body.secret);

    const complete = await json("/legion/v1/phase/complete", {
      grantId,
      summary: "Implemented the change",
    });

    expect(complete.response.status).toBe(200);
    expect(statusWrites).toEqual([{ issue: root, status: "testing" }]);
  });

  it("advances a child to testing on its implementer's completion when the sub-architect spawn's in_progress PATCH failed and no echo ever arrived", async () => {
    // `AGENTS.md`'s `/phase/complete` row promises this for a child: the `in_progress` write at
    // admission (the first sub-architect spawn) fails, resync has not retried it yet, Dispatch
    // still echoes `todo` -- and the implementer's completion still writes `testing`, because
    // `knownIssueStatus` reads the daemon's own pending write through the `statusAtRecord` fence.
    const statusWrites: Array<{ issue: IssueKey; status: string }> = [];
    let failNext = true;
    await start({
      dispatchClient: fakeDispatchClient({
        setStatus: async (issue, status) => {
          if (failNext) {
            failNext = false;
            throw new Error("Dispatch unavailable: 503");
          }
          statusWrites.push({ issue, status });
        },
      }),
    });
    state.issues[root].children = [child];
    state.issues[child] = {
      key: child,
      title: "Child",
      parent: root,
      status: "todo",
      children: [],
    };
    const secret = await architectSecret();
    const errorSpy = spyOn(console, "error").mockImplementation(() => {});
    try {
      const spawn = await json(
        "/legion/v1/worker/spawn",
        spawnBody({ issue: child, secret, role: "architect", task: "own this child" })
      );
      expect(spawn.response.status).toBe(200);
    } finally {
      errorSpy.mockRestore();
    }
    // The spawn succeeded; the failed PATCH is parked for resync against the status it saw.
    expect(statusWrites).toEqual([]);
    expect(state.pendingStatusWrites[child]).toEqual({
      status: "in_progress",
      statusAtRecord: "todo",
    });
    expect(state.issues[child].status).toBe("todo");

    state.roles[roleToken(state.project, child, "implementer")] = {
      issue: child,
      role: "implementer",
      generation: 1,
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%2",
        socketPath: "/state/workers/child-implementer.sock",
      },
    };
    const bootToken = await api?.mintWorkerBootToken(root, child, "implementer", 1);
    if (!bootToken) throw new Error("worker boot token was not minted");
    const started = await json<{ secret: string }>("/legion/v1/worker/started", {
      tree: root,
      issue: child,
      role: "implementer",
      bootToken,
      sessionId: "ses_child_implementer",
      agentId: "agt_child_implementer",
      ompSessionFile: "/tmp/child-implementer.json",
    });
    expect(started.response.status).toBe(200);
    state.phases[child] = { phase: "implementer", sessionId: "ses_child_implementer" };
    const grantId = await mintGrant(child, "ses_child_implementer", started.body.secret);

    const complete = await json("/legion/v1/phase/complete", {
      grantId,
      summary: "Implemented the child",
    });

    expect(complete.response.status).toBe(200);
    expect(statusWrites).toEqual([{ issue: child, status: "testing" }]);
  });

  it("writes no status when an implementer completes from retro (the .legion deletion push or retro itself)", async () => {
    const statusWrites: Array<{ issue: IssueKey; status: string }> = [];
    await start({
      dispatchClient: fakeDispatchClient({
        setStatus: async (issue, status) => {
          statusWrites.push({ issue, status });
        },
      }),
    });
    state.issues[root].status = "retro";
    const token = roleToken(state.project, root, "implementer");
    state.roles[token] = {
      issue: root,
      role: "implementer",
      generation: 1,
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%1",
        socketPath: "/state/workers/implementer.sock",
      },
    };
    const bootToken = await api?.mintWorkerBootToken(root, root, "implementer", 1);
    if (!bootToken) throw new Error("worker boot token was not minted");
    const started = await json<{ secret: string }>("/legion/v1/worker/started", {
      tree: root,
      issue: root,
      role: "implementer",
      bootToken,
      sessionId: "ses_implementer",
      agentId: "agt_implementer",
      ompSessionFile: "/tmp/implementer.json",
    });
    expect(started.response.status).toBe(200);
    state.phases[root] = { phase: "implementer", sessionId: "ses_implementer" };
    const grantId = await mintGrant(root, "ses_implementer", started.body.secret);

    const complete = await json("/legion/v1/phase/complete", {
      grantId,
      summary: "Pushed the .legion deletion",
    });

    expect(complete.response.status).toBe(200);
    expect(statusWrites).toEqual([]);
    expect(state.phases[root]).toBeUndefined();
  });

  it("advances the issue to testing when an implementer completes while the daemon's own in_progress write is still pending", async () => {
    const dispatch = recordingDispatchClient();
    await start({ dispatchClient: dispatch.client });
    // Dispatch has echoed nothing past `todo`, but admission's `in_progress` PATCH is recorded as
    // pending against that `todo`: the daemon's own view of the issue is `in_progress`.
    state.issues[root].status = "todo";
    state.pendingStatusWrites[root] = { status: "in_progress", statusAtRecord: "todo" };
    const token = roleToken(state.project, root, "implementer");
    state.roles[token] = {
      issue: root,
      role: "implementer",
      generation: 1,
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%1",
        socketPath: "/state/workers/implementer.sock",
      },
    };
    const bootToken = await api?.mintWorkerBootToken(root, root, "implementer", 1);
    if (!bootToken) throw new Error("worker boot token was not minted");
    const started = await json<{ secret: string }>("/legion/v1/worker/started", {
      tree: root,
      issue: root,
      role: "implementer",
      bootToken,
      sessionId: "ses_implementer",
      agentId: "agt_implementer",
      ompSessionFile: "/tmp/implementer.json",
    });
    expect(started.response.status).toBe(200);
    state.phases[root] = { phase: "implementer", sessionId: "ses_implementer" };
    const grantId = await mintGrant(root, "ses_implementer", started.body.secret);

    const complete = await json("/legion/v1/phase/complete", {
      grantId,
      summary: "Implemented the change",
    });

    expect(complete.response.status).toBe(200);
    expect(dispatch.statusWrites).toEqual([{ issue: root, status: "testing" }]);
  });

  it("writes no status when an implementer completes after a human's move superseded the daemon's pending in_progress write", async () => {
    const dispatch = recordingDispatchClient();
    await start({ dispatchClient: dispatch.client });
    // Admission's `in_progress` PATCH failed and was recorded against `todo`; before resync could
    // retry it, Dispatch echoed a human's move to `icebox`. That echo supersedes the pending write
    // (resync's own fence), so the daemon's view of the issue is the human's `icebox`.
    state.issues[root].status = "icebox";
    state.pendingStatusWrites[root] = { status: "in_progress", statusAtRecord: "todo" };
    const token = roleToken(state.project, root, "implementer");
    state.roles[token] = {
      issue: root,
      role: "implementer",
      generation: 1,
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%1",
        socketPath: "/state/workers/implementer.sock",
      },
    };
    const bootToken = await api?.mintWorkerBootToken(root, root, "implementer", 1);
    if (!bootToken) throw new Error("worker boot token was not minted");
    const started = await json<{ secret: string }>("/legion/v1/worker/started", {
      tree: root,
      issue: root,
      role: "implementer",
      bootToken,
      sessionId: "ses_implementer",
      agentId: "agt_implementer",
      ompSessionFile: "/tmp/implementer.json",
    });
    expect(started.response.status).toBe(200);
    state.phases[root] = { phase: "implementer", sessionId: "ses_implementer" };
    const grantId = await mintGrant(root, "ses_implementer", started.body.secret);

    const complete = await json("/legion/v1/phase/complete", {
      grantId,
      summary: "Implemented the change",
    });

    expect(complete.response.status).toBe(200);
    expect(dispatch.statusWrites).toEqual([]);
  });

  it("rejects a duplicate phase/complete once the architect has reassigned the issue to a later phase, logging one refusal line naming the grant's side and the record it refused against", async () => {
    await start();
    const token = roleToken(state.project, root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      locator: {
        runtime: "tmux",
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

    // The last statement before `try`: a setup failure above never leaves a swallowing spy behind.
    const errorSpy = spyOn(console, "error").mockImplementation(() => {});
    try {
      const first = await json("/legion/v1/phase/complete", body);
      expect(first.response.status).toBe(200);
      expect(state.phases[root]).toBeUndefined();
      expect(refusalLines(errorSpy)).toEqual([]);

      // The architect reassigns the same issue to a later phase (the delivery of that assignment
      // through spawn_worker writes exactly this record, stamped with its delivery time), and that
      // implementer has since finished with no architect live to receive it (the route's own 202
      // record, `completed` stamped on the same record). The tester's own claim never changes.
      // Grants are read-only, reusable-until-expiry tokens, so the same grantId is still valid.
      const reassigned = {
        phase: "implementer",
        sessionId: "ses_implementer",
        assignedAt: "2026-09-13T05:20:00.000Z",
        completed: { summary: "Implemented", at: "2026-09-13T05:36:58.000Z" },
      };
      state.phases[root] = { ...reassigned };

      const duplicate = await json<{ error: string }>("/legion/v1/phase/complete", body);

      expect(duplicate.response.status).toBe(409);
      expect(duplicate.body.error).toBe("Phase for WIDGETS-1 is no longer owned by this worker");
      expect(state.phases[root]).toEqual(reassigned);
      expect(
        publications.filter(
          (publication) =>
            publication.topic === roleTopic(roleToken(state.project, root, "architect"))
        )
      ).toHaveLength(1);
      // One line, both sides: what the grant named, whose claim the role holds, and the record
      // the completion was refused against — enough to tell this (an assignment moved the phase
      // on) from a grant problem without the body or a second read of state. The grant is named
      // by its hash prefix only: the id is the credential `legion gh` redeems for another 60 s.
      const lines = refusalLines(errorSpy);
      expect(lines).toHaveLength(1);
      const line = lines[0] ?? "";
      expect(line.startsWith("[legion] phase/complete refused 409 ")).toBe(true);
      expect(line).toContain("Phase for WIDGETS-1 is no longer owned by this worker");
      expect(line).toContain(grantRef(grantId));
      expect(line).not.toContain(grantId);
      expect(line).toContain("issue WIDGETS-1 role tester session ses_tester");
      expect(line).toContain(`claim ${token}: session ses_tester`);
      expect(line).toContain(
        "phases[WIDGETS-1]: role implementer session ses_implementer assignedAt 2026-09-13T05:20:00.000Z completed 2026-09-13T05:36:58.000Z"
      );
    } finally {
      errorSpy.mockRestore();
    }
  });

  it("rejects phase/complete with an expired grant, logging one refusal line that names the expiry and prints a pre-v29 record's assignedAt as unknown", async () => {
    await start();
    const token = roleToken(state.project, root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      locator: {
        runtime: "tmux",
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
    // A record persisted before v29: no assignedAt, never backfilled.
    state.phases[root] = { phase: "tester", sessionId: "ses_tester" };
    const mintedAt = now;
    const grantId = await mintGrant(root, "ses_tester", started.body.secret);

    const errorSpy = spyOn(console, "error").mockImplementation(() => {});
    try {
      now += 60_001;
      const complete = await json<{ error: string }>("/legion/v1/phase/complete", {
        grantId,
        summary: "smoke",
      });

      expect(complete.response.status).toBe(403);
      expect(complete.body.error).toBe("Invalid or expired grant");
      expect(state.phases[root]).toEqual({ phase: "tester", sessionId: "ses_tester" });
      expect(publications).toEqual([]);
      // The body cannot tell an expired grant from one this daemon never held; the log does, and
      // still names the claim and the record so an operator sees the completion was otherwise
      // sound (same worker, same phase) and only the grant's 60 s ran out.
      const lines = refusalLines(errorSpy);
      expect(lines).toHaveLength(1);
      const line = lines[0] ?? "";
      expect(line.startsWith("[legion] phase/complete refused 403 ")).toBe(true);
      expect(line).toContain("Invalid or expired grant");
      expect(line).toContain(grantRef(grantId));
      expect(line).not.toContain(grantId);
      expect(line).toContain("issue WIDGETS-1 role tester session ses_tester");
      expect(line).toContain(`expired ${new Date(mintedAt + 60_000).toISOString()}`);
      expect(line).toContain(`claim ${token}: session ses_tester`);
      expect(line).toContain(
        "phases[WIDGETS-1]: role tester session ses_tester assignedAt unknown"
      );
    } finally {
      errorSpy.mockRestore();
    }
  });

  it("rejects phase/complete with a grant this daemon never minted, logging one refusal line that names only the grant's hash prefix — never the caller-supplied bytes — and nothing else", async () => {
    await start();
    state.phases[root] = { phase: "tester", sessionId: "ses_tester" };
    // On this branch the id is whatever an unauthenticated caller sent — here a second, forged
    // refusal line embedded after a newline. The hash bounds it to twelve hex characters.
    const grantId =
      "00000000-0000-4000-8000-000000000000\n[legion] phase/complete refused 200 forged: grant deadbeef";

    const errorSpy = spyOn(console, "error").mockImplementation(() => {});
    try {
      const complete = await json<{ error: string }>("/legion/v1/phase/complete", {
        grantId,
        summary: "smoke",
      });

      expect(complete.response.status).toBe(403);
      expect(complete.body.error).toBe("Invalid or expired grant");
      expect(state.phases[root]).toEqual({ phase: "tester", sessionId: "ses_tester" });
      // A grant the daemon does not hold (never minted, revoked with its session, or minted before
      // this daemon started) names no issue, so there is no claim or record to look up.
      const lines = refusalLines(errorSpy);
      expect(lines).toHaveLength(1);
      const line = lines[0] ?? "";
      expect(line.startsWith("[legion] phase/complete refused 403 ")).toBe(true);
      expect(line).toContain("Invalid or expired grant");
      expect(line).toContain(`${grantRef(grantId)} unknown`);
      expect(line).toContain("nothing else to name");
      expect(line).not.toContain("\n");
      expect(line).not.toContain("00000000-0000-4000-8000-000000000000");
      expect(line).not.toContain("forged");
    } finally {
      errorSpy.mockRestore();
    }
  });

  it("rejects phase/complete with 404 when no tree contains the grant's issue any more, logging one refusal line with no claim and no record to name", async () => {
    await start();
    const token = roleToken(state.project, root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      locator: {
        runtime: "tmux",
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
    // The tree closed and was pruned between the mint and the redeem: its record, its role claims
    // and its phase are gone, while the in-memory grant (60 s) still resolves.
    delete state.trees[root];
    delete state.roles[token];
    delete state.phases[root];

    const errorSpy = spyOn(console, "error").mockImplementation(() => {});
    try {
      const complete = await json<{ error: string }>("/legion/v1/phase/complete", {
        grantId,
        summary: "smoke",
      });

      expect(complete.response.status).toBe(404);
      expect(complete.body.error).toBe("No Legion tree contains issue WIDGETS-1");
      expect(publications).toEqual([]);
      const lines = refusalLines(errorSpy);
      expect(lines).toHaveLength(1);
      const line = lines[0] ?? "";
      expect(line.startsWith("[legion] phase/complete refused 404 ")).toBe(true);
      expect(line).toContain("No Legion tree contains issue WIDGETS-1");
      expect(line).toContain(grantRef(grantId));
      expect(line).not.toContain(grantId);
      expect(line).toContain("issue WIDGETS-1 role tester session ses_tester");
      expect(line).toContain(`claim ${token}: none`);
      expect(line).toContain("phases[WIDGETS-1]: none");
    } finally {
      errorSpy.mockRestore();
    }
  });

  it("rejects phase/complete when the grant's session no longer matches the worker's claim, logging one refusal line that names both sessions", async () => {
    await start();
    const token = roleToken(state.project, root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      locator: {
        runtime: "tmux",
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
    state.phases[root] = {
      phase: "tester",
      sessionId: "ses_tester",
      assignedAt: "2026-09-13T05:20:00.000Z",
    };
    const grantId = await mintGrant(root, "ses_tester", started.body.secret);
    // A respawn between minting the grant and redeeming it moves the claim onto a new session;
    // the grant is still unexpired, but it no longer names the worker that currently owns the role.
    const claim = state.roles[token];
    if (!claim || !("issue" in claim)) throw new Error("worker claim disappeared");
    claim.sessionId = "ses_tester_respawned";

    const errorSpy = spyOn(console, "error").mockImplementation(() => {});
    try {
      const complete = await json<{ error: string }>("/legion/v1/phase/complete", {
        grantId,
        summary: "smoke",
      });

      expect(complete.response.status).toBe(409);
      expect(complete.body.error).toBe(
        "Grant does not match the worker currently holding this role"
      );
      expect(state.phases[root]).toEqual({
        phase: "tester",
        sessionId: "ses_tester",
        assignedAt: "2026-09-13T05:20:00.000Z",
      });
      expect(publications).toEqual([]);
      // The grant's session and the claim's current one side by side: the line says which
      // session redeemed and which one the role now belongs to.
      const lines = refusalLines(errorSpy);
      expect(lines).toHaveLength(1);
      const line = lines[0] ?? "";
      expect(line.startsWith("[legion] phase/complete refused 409 ")).toBe(true);
      expect(line).toContain("Grant does not match the worker currently holding this role");
      expect(line).toContain(grantRef(grantId));
      expect(line).not.toContain(grantId);
      expect(line).toContain("issue WIDGETS-1 role tester session ses_tester");
      expect(line).toContain(`claim ${token}: session ses_tester_respawned`);
      expect(line).toContain(
        "phases[WIDGETS-1]: role tester session ses_tester assignedAt 2026-09-13T05:20:00.000Z"
      );
    } finally {
      errorSpy.mockRestore();
    }
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
        runtime: "tmux",
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
    state.phases[root] = {
      phase: "tester",
      sessionId: "ses_tester",
      assignedAt: "2026-09-13T05:20:00.000Z",
    };
    const grantId = await mintGrant(root, "ses_tester", started.body.secret);

    const complete = await json("/legion/v1/phase/complete", { grantId, summary: "smoke" });

    expect(complete.response.status).toBe(202);
    // The completion is recorded, not dropped: state (the source of truth) keeps the phase with
    // a `completed` marker so `overseerCatchup` replays it and `routeActive` treats the issue as
    // having no active phase, instead of losing the report entirely.
    expect(state.phases[root]).toEqual({
      phase: "tester",
      sessionId: "ses_tester",
      assignedAt: "2026-09-13T05:20:00.000Z",
      completed: { summary: "smoke", at: new Date(now).toISOString() },
    });
    expect(state.roles[token]).toMatchObject({ issue: root, role: "tester", generation: 1 });
    // Recovery is delegated to the ProcessManager, which records that resync owns the root.
    expect(recoveredRoles).toEqual([roleToken(state.project, root, "architect")]);
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
        runtime: "tmux",
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
        runtime: "tmux",
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

    const errorSpy = spyOn(console, "error").mockImplementation(() => {});
    try {
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
      // The loser's refusal line is what tells this apart from a reassigned phase in the log: the
      // same 409 body, but the record side reads `none` — the winner had already cleared it.
      const lines = refusalLines(errorSpy);
      expect(lines).toHaveLength(1);
      const line = lines[0] ?? "";
      expect(line).toContain(`refused 409 Phase for ${root} is no longer owned by this worker`);
      expect(line).toContain(grantRef(grantId));
      expect(line).not.toContain(grantId);
      expect(line).toContain(`claim ${token}: session ses_tester`);
      expect(line).toContain(`phases[${root}]: none`);
    } finally {
      errorSpy.mockRestore();
    }
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
        runtime: "tmux",
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
    state.phases[root] = {
      phase: "tester",
      sessionId: "ses_tester",
      assignedAt: "2026-09-13T05:20:00.000Z",
    };
    const grantId = await mintGrant(root, "ses_tester", started.body.secret);
    const body = { grantId, summary: "Verified the acceptance criteria" };

    const failed = await json("/legion/v1/phase/complete", body);

    expect(failed.response.status).toBe(502);
    expect(state.phases[root]).toEqual({
      phase: "tester",
      sessionId: "ses_tester",
      assignedAt: "2026-09-13T05:20:00.000Z",
    });
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
        runtime: "tmux",
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
    state.phases[root] = {
      phase: "tester",
      sessionId: "ses_tester",
      assignedAt: "2026-09-13T05:20:00.000Z",
    };
    const grantId = await mintGrant(root, "ses_tester", started.body.secret);
    const body = { grantId, summary: "Verified the acceptance criteria" };

    failSave = true;
    const failed = await json("/legion/v1/phase/complete", body);

    expect(failed.response.status).toBe(500);
    expect(state.phases[root]).toEqual({
      phase: "tester",
      sessionId: "ses_tester",
      assignedAt: "2026-09-13T05:20:00.000Z",
    });

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
