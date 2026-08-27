import { randomUUID } from "node:crypto";
import { type IssueKey, LegionDaemonApi, type LegionRole } from "@legion/contracts";
import type { CommandRunner } from "../state/fetch";
import { defaultRunner } from "../state/fetch";
import { CapabilityService, ControllerGate, secretHash, spawnCapabilityKey } from "./api/auth";
import { appendFooter, type RouteContext, requireTree, requireTreeIssue } from "./api/context";
import { type DispatchDeps, dispatchThread } from "./api/dispatch";
import { GitHubService, type GitHubTokenSource } from "./api/github";
import {
  asRecord,
  type ContractSchema,
  HttpError,
  validateContractRequest,
  validateContractResponse,
} from "./api/http";
import {
  handleAdmission,
  handleBacklog,
  handleControllerReady,
  handleGatesApprove,
} from "./api/routes/controller";
import {
  handleGhToken,
  handleGitCredential,
  handleGrants,
  handleProvisioningCredential,
} from "./api/routes/credentials";
import { handleDispatchThreads } from "./api/routes/dispatch";
import {
  handleEscalate,
  handleIssueBody,
  handleIssueClose,
  handleIssueComment,
  handleIssueCreate,
  handleIssueLabels,
  handleWaveRelease,
} from "./api/routes/issues";
import { handleMergeGate } from "./api/routes/merge-gate";
import { handleProcessExit, handleProcessReady, handleProcessStarted } from "./api/routes/process";
import {
  handlePhase,
  handleRoleBacking,
  handleSpawnToken,
  handleWorkerSession,
} from "./api/routes/workers";
import type { LegionState } from "./legion-state";

const GRANT_TTL_MS = 60_000;

type MergeGateSetting = "human" | "off";

export interface LegionApiConfig {
  port: number;
  hostname?: string;
  gates: { design: "root-issues" | "off"; merge: MergeGateSetting };
  appLogins?: string[];
  now?: () => number;
}

export interface LegionApiProcessManager {
  admit(issue: IssueKey): "spawned" | "queued";
  releaseSlot(issue: IssueKey): void;
  registerRoleBacking(
    tree: IssueKey,
    issue: IssueKey,
    role: LegionRole,
    agentId: string
  ): void | Promise<void>;
  markProcessDead(tree: IssueKey): void | Promise<void>;
  closeTree(tree: IssueKey): void | Promise<void>;
  markTreeReady(tree: IssueKey): void | Promise<void>;
  beginLinger(tree: IssueKey): void;
}

export type { DispatchFetch as LegionApiFetch } from "./api/dispatch";

export interface LegionApiDeps {
  state: LegionState;
  saveState?: () => Promise<void>;
  runner?: CommandRunner;
  tokenManager: GitHubTokenSource;
  processManager: LegionApiProcessManager;
  envoyPublish(topic: string, payloadJson: string): Promise<void>;
  dispatch: DispatchDeps;
  onTreeReady?(tree: IssueKey): Promise<void>;
  onControllerReady(): Promise<void>;
  onControllerEvent(payload: { type: string }): Promise<void>;
}

export interface LegionApi {
  server: Bun.Server<undefined>;
  mintControllerCapability(): Promise<string>;
  mintBootToken(tree: IssueKey, generation: number): Promise<string>;
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
  "/legion/v1/merge-gate": { request: LegionDaemonApi.MergeGate.request, handler: handleMergeGate },
  "/legion/v1/issues": { request: LegionDaemonApi.IssueCreate.request, handler: handleIssueCreate },
  "/legion/v1/waves/release": {
    request: LegionDaemonApi.WaveRelease.request,
    handler: handleWaveRelease,
  },
  "/legion/v1/issues/comment": {
    request: LegionDaemonApi.Comment.request,
    handler: handleIssueComment,
  },
  "/legion/v1/issues/body": { request: LegionDaemonApi.PostBody.request, handler: handleIssueBody },
  "/legion/v1/issues/labels": {
    request: LegionDaemonApi.Labels.request,
    handler: handleIssueLabels,
  },
  "/legion/v1/issues/close": {
    request: LegionDaemonApi.IssueClose.request,
    handler: handleIssueClose,
  },
  "/legion/v1/escalate": { request: LegionDaemonApi.Escalate.request, handler: handleEscalate },
  "/legion/v1/dispatch-threads": {
    request: LegionDaemonApi.DispatchThread.request,
    handler: handleDispatchThreads,
  },
  "/legion/v1/spawn-token": {
    request: LegionDaemonApi.SpawnToken.request,
    handler: handleSpawnToken,
  },
  "/legion/v1/phase": { request: LegionDaemonApi.Phase.request, handler: handlePhase },
  "/legion/v1/worker-session": {
    request: LegionDaemonApi.WorkerSession.request,
    handler: handleWorkerSession,
  },
  "/legion/v1/role-backing": {
    request: LegionDaemonApi.RoleBacking.request,
    handler: handleRoleBacking,
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
  "/legion/v1/gates/approve": {
    request: LegionDaemonApi.GatesApprove.request,
    handler: handleGatesApprove,
  },
  "/legion/v1/admission": { request: LegionDaemonApi.Admission.request, handler: handleAdmission },
  "/legion/v1/backlog": { request: LegionDaemonApi.Backlog.request, handler: handleBacklog },
};

export function startLegionApi(config: LegionApiConfig, deps: LegionApiDeps): LegionApi {
  if (config.gates.merge === "human" && (!config.appLogins || config.appLogins.length === 0)) {
    throw new Error("gates.merge=human requires at least one configured GitHub App login");
  }

  const runner = deps.runner ?? defaultRunner;
  const now = config.now ?? Date.now;
  const save = async (): Promise<void> => {
    await deps.saveState?.();
  };
  const auth = new CapabilityService(now);
  const controllerGate = new ControllerGate();
  const github = new GitHubService(deps.tokenManager, runner);

  const ctx: RouteContext = {
    config,
    deps,
    now,
    save,
    runner,
    grantTtlMs: GRANT_TTL_MS,
    auth,
    controllerGate,
    github,
    dispatchThread: (parent, subject, body, ask, urgency) =>
      dispatchThread(deps.dispatch, parent, subject, body, ask, urgency),
    requireTree: (body) => requireTree(deps.state, body),
    requireTreeIssue: (body) => requireTreeIssue(deps.state, body),
    appendFooter: (tree, issue, body) => appendFooter(deps.state, tree, issue, body),
  };

  const handler = async (request: Request): Promise<Response> => {
    try {
      const url = new URL(request.url);
      const pathname = url.pathname;
      if (request.method === "GET" && pathname === "/legion/v1/state") {
        return Response.json(
          validateContractResponse(LegionDaemonApi.State.response, { project: deps.state.project })
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
      };
      await save();
      return bootToken;
    },
    mintControllerCapability: async () => {
      const secret = randomUUID();
      deps.state.controllerCapabilityHash = secretHash(secret).toString("hex");
      controllerGate.reset();
      await save();
      return secret;
    },
    stop: () => server.stop(true),
  };
}
