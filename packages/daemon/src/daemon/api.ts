import { randomUUID } from "node:crypto";
import {
  type IssueKey,
  LegionDaemonApi,
  type LegionRole,
  roleToken,
  type SpawnWorkerResponse,
} from "@legion/contracts";
import { CapabilityService, secretHash, spawnCapabilityKey } from "./api/auth";
import { type RouteContext, requireTree, requireTreeIssue } from "./api/context";

import { GitHubService, type GitHubTokenSource } from "./api/github";
import {
  asRecord,
  type ContractSchema,
  HttpError,
  validateContractRequest,
  validateContractResponse,
} from "./api/http";
import { handleControllerReady } from "./api/routes/controller";
import {
  handleGhToken,
  handleGitCredential,
  handleGrants,
  handleProvisioningCredential,
} from "./api/routes/credentials";
import {
  handleEscalate,
  handleGatesRegister,
  handleIssueStatus,
  handleWaveRelease,
} from "./api/routes/issues";
import { handleProcessExit, handleProcessReady, handleProcessStarted } from "./api/routes/process";
import {
  handlePhaseComplete,
  handleSpawnWorker,
  handleWorkerReady,
  handleWorkerSession,
  handleWorkerStarted,
} from "./api/routes/workers";
import { SpawnRequestLedger } from "./api/spawn-requests";
import { buildLegionStateResponse } from "./api/state";
import type { DispatchClient } from "./dispatch-client";
import type { LegionState } from "./legion-state";
import { StopFailed, TreeClosingError } from "./processes";

const GRANT_TTL_MS = 60_000;

export interface LegionApiConfig {
  port: number;
  hostname?: string;
  repo: `${string}/${string}`;
  gates: { design: "root-issues" | "off" };
  now?: () => number;
}

export interface LegionApiProcessManager {
  admit(issue: IssueKey): "spawned" | "queued";
  releaseSlot(issue: IssueKey): void;
  spawnWorker(
    tree: IssueKey,
    issue: IssueKey,
    role: LegionRole,
    task: string
  ): Promise<SpawnWorkerResponse>;
  workerReady(
    issue: IssueKey,
    role: LegionRole,
    sessionId: string,
    generation: number
  ): void | Promise<void>;
  /** Recovers a no-holder role with its state-derived catch-up. */
  recoverRole(token: string): Promise<void>;
  rejectIfTreeGone(tree: IssueKey, issue: IssueKey): void;
  mutateLiveRoleClaim<T>(
    tree: IssueKey,
    issue: IssueKey,
    token: string,
    fn: () => Promise<T>
  ): Promise<T>;
  markProcessDead(tree: IssueKey): void | Promise<void>;
  reportRootExit(tree: IssueKey): void | Promise<void>;
  closeTree(tree: IssueKey, options?: { stopRoot?: boolean }): void | Promise<void>;
  markTreeReady(tree: IssueKey): void | Promise<void>;
  confirmRootReady(tree: IssueKey, generation: number): void;
  markControllerReady(): void | Promise<void>;
  cancelBootWatchdog(token: string, generation?: number): void;
  beginLinger(tree: IssueKey): void;
}

export interface LegionApiDeps {
  state: LegionState;
  saveState?: () => Promise<void>;
  tokenManager: GitHubTokenSource;
  dispatchClient: DispatchClient;
  processManager: LegionApiProcessManager;
  envoyPublish(topic: string, payloadJson: string): Promise<void>;
  onTreeReady?(tree: IssueKey): Promise<void>;
  onControllerReady(): Promise<void>;
  onControllerEvent(payload: { type: string }): Promise<void>;
}

/** What the worker stream listener needs from a hello's boot token: the claim token to register
 * the stream under, and whether the token's generation is no longer the current one. A phase
 * worker's or sub-architect's token resolves through `CapabilityService.resolveWorkerClaim`; a
 * tree root's through `resolveRootBootToken` to `roleToken(project, tree, "architect")`. */
export interface ResolvedStreamClaim {
  token: string;
  stale: boolean;
}

export interface LegionApi {
  server: Bun.Server<undefined>;
  mintControllerCapability(): Promise<string>;
  mintBootToken(tree: IssueKey, generation: number): Promise<string>;
  mintWorkerBootToken(
    tree: IssueKey,
    issue: IssueKey,
    role: LegionRole,
    generation: number,
    expectedSessionId?: string
  ): Promise<string>;
  /** The listener side of the boot-token handshake: resolves a `legion worker-shim --connect`
   * hello's token to the claim token the stream registers under — a phase worker's or
   * sub-architect's claim (the same lookup `/worker/started` performs), or a tree root's
   * architect token — and whether its generation is stale; `undefined` for a token nothing
   * minted. */
  resolveWorkerBootToken(bootToken: string): ResolvedStreamClaim | undefined;
  revokeSessionCapability(sessionId: string): void;
  stop(): void;
}

interface RouteEntry {
  readonly request: ContractSchema;
  readonly handler: (ctx: RouteContext, body: Record<string, unknown>) => Promise<Response>;
}

// Every POST route carries its request contract, so a route cannot be added
// without schema validation running before its handler.
const ROUTES: Record<string, RouteEntry> = {
  "/legion/v1/process/started": {
    request: LegionDaemonApi.ProcessStarted.request,
    handler: handleProcessStarted,
  },
  "/legion/v1/process/ready": {
    request: LegionDaemonApi.ProcessReady.request,
    handler: handleProcessReady,
  },
  "/legion/v1/process/exit": {
    request: LegionDaemonApi.ProcessExit.request,
    handler: handleProcessExit,
  },
  "/legion/v1/waves/release": {
    request: LegionDaemonApi.WaveRelease.request,
    handler: handleWaveRelease,
  },
  "/legion/v1/escalate": { request: LegionDaemonApi.Escalate.request, handler: handleEscalate },
  "/legion/v1/worker/started": {
    request: LegionDaemonApi.WorkerStarted.request,
    handler: handleWorkerStarted,
  },
  "/legion/v1/worker/ready": {
    request: LegionDaemonApi.WorkerReady.request,
    handler: handleWorkerReady,
  },
  "/legion/v1/phase/complete": {
    request: LegionDaemonApi.PhaseComplete.request,
    handler: handlePhaseComplete,
  },
  "/legion/v1/worker/spawn": {
    request: LegionDaemonApi.SpawnWorker.request,
    handler: handleSpawnWorker,
  },
  "/legion/v1/worker-session": {
    request: LegionDaemonApi.WorkerSession.request,
    handler: handleWorkerSession,
  },
  "/legion/v1/provisioning-credential": {
    request: LegionDaemonApi.ProvisioningCredential.request,
    handler: handleProvisioningCredential,
  },
  "/legion/v1/grants": { request: LegionDaemonApi.Grant.request, handler: handleGrants },
  "/legion/v1/git-credential": {
    request: LegionDaemonApi.GitHubToken.request,
    handler: handleGitCredential,
  },
  "/legion/v1/gh-token": { request: LegionDaemonApi.GitHubToken.request, handler: handleGhToken },
  "/legion/v1/controller/ready": {
    request: LegionDaemonApi.ControllerReady.request,
    handler: handleControllerReady,
  },
  "/legion/v1/issues/status": {
    request: LegionDaemonApi.IssueStatus.request,
    handler: handleIssueStatus,
  },
  "/legion/v1/gates/register": {
    request: LegionDaemonApi.GatesRegister.request,
    handler: handleGatesRegister,
  },
};

export function startLegionApi(config: LegionApiConfig, deps: LegionApiDeps): LegionApi {
  const now = config.now ?? Date.now;
  const save = async (): Promise<void> => {
    await deps.saveState?.();
  };
  const auth = new CapabilityService(now);
  const github = new GitHubService(config.repo, deps.tokenManager);
  const spawnRequests = new SpawnRequestLedger(deps.state, now, save);

  const ctx: RouteContext = {
    config,
    deps,
    now,
    save,
    grantTtlMs: GRANT_TTL_MS,
    auth,
    github,
    spawnRequests,
    requireTree: (body) => requireTree(deps.state, body),
    requireTreeIssue: (body) => requireTreeIssue(deps.state, body),
  };

  const handler = async (request: Request): Promise<Response> => {
    try {
      const url = new URL(request.url);
      const pathname = url.pathname;
      if (request.method === "GET" && pathname === "/legion/v1/state") {
        return Response.json(
          validateContractResponse(
            LegionDaemonApi.State.response,
            buildLegionStateResponse(deps.state)
          )
        );
      }
      if (request.method !== "POST") {
        throw new HttpError(404, "Not found");
      }
      const body = asRecord(await request.json());
      const route = ROUTES[pathname];
      if (!route) {
        throw new HttpError(404, "Not found");
      }
      validateContractRequest(route.request, body);
      return await route.handler(ctx, body);
    } catch (error) {
      if (error instanceof HttpError) {
        return Response.json({ error: error.message }, { status: error.status });
      }
      if (error instanceof TreeClosingError) {
        return Response.json({ error: error.message }, { status: 409 });
      }
      if (error instanceof StopFailed) {
        return Response.json({ error: error.message }, { status: 502 });
      }
      return Response.json(
        {
          error: error instanceof Error ? error.message : "Internal server error",
        },
        { status: 500 }
      );
    }
  };

  const server = Bun.serve({
    hostname: config.hostname ?? "127.0.0.1",
    port: config.port,
    // Never close a connection whose request a handler is still working on. Bun documents a
    // ten-second default; on Bun 1.3.14 the default does not fire for a pending handler
    // (LEGION-102's reproduction), so this guards a later runtime, not the incident.
    // Loopback-only, and every client is one of this daemon's own panes with its own deadline;
    // 255 would not cover a fresh worker's workspace provisioning (slow_command_timeout_seconds,
    // default 300).
    idleTimeout: 0,
    fetch: handler,
  });
  return {
    server,
    mintBootToken: async (tree, generation) => {
      const treeState = deps.state.trees[tree];
      if (!treeState || treeState.generation !== generation) {
        throw new Error(`Cannot mint boot token for stale tree generation ${tree}`);
      }
      const bootToken = randomUUID();
      auth.registerBootToken(bootToken, tree, generation);
      deps.state.spawnCapabilities[spawnCapabilityKey(bootToken)] = {
        tree,
        issue: tree,
        role: "architect",
        generation,
      };
      await save();
      return bootToken;
    },
    mintWorkerBootToken: async (tree, issue, role, generation, expectedSessionId) => {
      const bootToken = randomUUID();
      auth.registerWorkerBootToken(bootToken, { tree, issue, role, generation, expectedSessionId });
      return bootToken;
    },
    resolveWorkerBootToken: (bootToken) => {
      const worker = auth.resolveWorkerClaim(deps.state, bootToken);
      if (worker) {
        return {
          token: worker.token,
          stale: worker.boot !== undefined && worker.boot.generation !== worker.claim.generation,
        };
      }
      const root = auth.resolveRootBootToken(deps.state, bootToken);
      const tree = root ? deps.state.trees[root.tree] : undefined;
      if (!root || !tree) return undefined;
      return {
        token: roleToken(deps.state.project, root.tree, "architect"),
        stale: root.generation !== undefined && root.generation !== tree.generation,
      };
    },
    mintControllerCapability: async () => {
      const secret = randomUUID();
      deps.state.controllerCapabilityHash = secretHash(secret).toString("hex");
      await save();
      return secret;
    },
    revokeSessionCapability: (sessionId) => auth.deleteCapability(sessionId),
    stop: () => server.stop(true),
  };
}
