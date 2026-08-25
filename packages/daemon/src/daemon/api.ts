import { createHash, randomUUID, timingSafeEqual } from "node:crypto";
import {
  controllerToken,
  formatIssueKey,
  type IssueKey,
  LEGION_ROLES,
  LegionDaemonApi,
  type LegionRole,
  parseIssueKey,
  roleToken,
  sanitizeToken,
} from "@legion/contracts";
import type { CommandRunner } from "../state/fetch";
import { defaultRunner } from "../state/fetch";
import type { GitHubAppRole } from "./config";
import { buildRoleEnv, type TokenManager } from "./github-apps";
import type { LegionState } from "./legion-state";

const GRANT_TTL_MS = 60_000;

type DispatchThreadResult = { thread: number; url: string };

type MergeGateSetting = "human" | "off";

type TokenLease = {
  token: string;
  expiresAt: string;
  gitIdentity: { name: string; email: string };
};

export interface LegionApiConfig {
  port: number;
  hostname?: string;
  gates: { design: "root-issues" | "off"; merge: MergeGateSetting };
  now?: () => number;
}

export interface LegionApiTokenManager {
  getToken(role: GitHubAppRole, owner: string): Promise<TokenLease>;
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

export type LegionApiFetch = (
  input: string | URL | Request,
  init?: RequestInit
) => Promise<Response>;

export interface LegionApiDeps {
  state: LegionState;
  saveState?: () => Promise<void>;
  runner?: CommandRunner;
  tokenManager: Pick<TokenManager, "getToken"> | LegionApiTokenManager;
  processManager: LegionApiProcessManager;
  envoyPublish(topic: string, payloadJson: string): Promise<void>;
  dispatch: { url: string; bearer: string; fetch?: LegionApiFetch };
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

interface SessionCapability {
  tree: IssueKey;
  issue: IssueKey;
  role: LegionRole;
  secretHash: Buffer;
}

interface Grant {
  issue: IssueKey;
  role: LegionRole;
  expiresAt: number;
}

class HttpError extends Error {
  constructor(
    readonly status: number,
    message: string
  ) {
    super(message);
  }
}

function asRecord(value: unknown): Record<string, unknown> {
  if (typeof value !== "object" || value === null || Array.isArray(value)) {
    throw new HttpError(400, "Expected a JSON object");
  }
  return value as Record<string, unknown>;
}

type ContractSchema = { parse(value: unknown): unknown };

function validateContractRequest(schema: ContractSchema, body: Record<string, unknown>): void {
  try {
    schema.parse(body);
  } catch {
    throw new HttpError(400, "Invalid Legion daemon API request");
  }
}

function validateContractResponse<T>(schema: ContractSchema, response: T): T {
  schema.parse(response);
  return response;
}

function requiredString(body: Record<string, unknown>, field: string): string {
  const value = body[field];
  if (typeof value !== "string" || value.length === 0) {
    throw new HttpError(400, `Expected non-empty string ${field}`);
  }
  return value;
}

function optionalStrings(body: Record<string, unknown>, field: string): string[] {
  const value = body[field];
  if (value === undefined) {
    return [];
  }
  if (
    !Array.isArray(value) ||
    value.some((entry) => typeof entry !== "string" || entry.length === 0)
  ) {
    throw new HttpError(400, `Expected string array ${field}`);
  }
  return value;
}

function requiredNumber(body: Record<string, unknown>, field: string): number {
  const value = body[field];
  if (typeof value !== "number" || !Number.isFinite(value) || !Number.isInteger(value)) {
    throw new HttpError(400, `Expected integer ${field}`);
  }
  return value;
}

function issueKey(body: Record<string, unknown>, field: string): IssueKey {
  const value = requiredString(body, field);
  const parsed = parseIssueKey(value);
  if (!parsed) {
    throw new HttpError(400, `Expected issue key ${field}`);
  }
  return formatIssueKey(parsed.owner, parsed.repo, parsed.number);
}

function legionRole(value: string): LegionRole {
  const role = LEGION_ROLES.find((candidate) => candidate === value);
  if (!role) {
    throw new HttpError(400, `Unknown Legion role ${value}`);
  }
  return role;
}

function roleForSession(
  state: LegionState,
  issue: IssueKey,
  sessionId: string,
  phase: string
): LegionRole {
  const claim = Object.values(state.roles).find(
    (candidate) =>
      "issue" in candidate && candidate.issue === issue && candidate.sessionId === sessionId
  );
  if (!claim) {
    throw new HttpError(403, "Session is not registered for this issue");
  }
  const role = legionRole(claim.role);
  const declaredPhaseRole = LEGION_ROLES.find((candidate) => candidate === phase);
  if (declaredPhaseRole && declaredPhaseRole !== role) {
    throw new HttpError(403, "Session role does not match phase");
  }
  return role;
}

function appRoleForLegionRole(role: LegionRole): GitHubAppRole {
  return role === "reviewer" ? "review" : "implement";
}

function secretHash(secret: string): Buffer {
  return createHash("sha256").update(secret).digest();
}
function spawnCapabilityKey(spawnToken: string): string {
  return secretHash(spawnToken).toString("hex");
}

function equalSecret(expected: Buffer, supplied: string): boolean {
  const actual = secretHash(supplied);
  return actual.length === expected.length && timingSafeEqual(actual, expected);
}

function equalSecretHash(expectedHash: string, supplied: string): boolean {
  if (!/^[a-f0-9]{64}$/.test(expectedHash)) {
    return false;
  }
  return timingSafeEqual(Buffer.from(expectedHash, "hex"), secretHash(supplied));
}

function issueUrl(issue: IssueKey): string {
  const parsed = parseIssueKey(issue);
  if (!parsed) {
    throw new Error(`Invalid issue key in Legion state: ${issue}`);
  }
  return `repos/${parsed.owner}/${parsed.repo}/issues/${parsed.number}`;
}

function treeContains(state: LegionState, tree: IssueKey, candidate: IssueKey): boolean {
  const pending = [tree];
  const visited = new Set<IssueKey>();
  while (pending.length > 0) {
    const current = pending.pop();
    if (!current || visited.has(current)) {
      continue;
    }
    if (current === candidate) {
      return true;
    }
    visited.add(current);
    pending.push(...(state.issues[current]?.children ?? []));
  }
  return false;
}

function matchingHeldEvent(state: LegionState, issue: IssueKey, role: string): boolean {
  const claim = state.roles[role];
  if (claim && "issue" in claim && claim.issue === issue) {
    return true;
  }
  return LEGION_ROLES.some(
    (candidateRole) => role === roleToken(state.project, issue, candidateRole)
  );
}

export function startLegionApi(config: LegionApiConfig, deps: LegionApiDeps): LegionApi {
  const runner = deps.runner ?? defaultRunner;
  const now = config.now ?? Date.now;
  const capabilities = new Map<string, SessionCapability>();
  const bootTokens = new Map<string, { tree: IssueKey; generation: number }>();
  const grants = new Map<string, Grant>();
  let controllerReady = false;
  const save = async (): Promise<void> => {
    await deps.saveState?.();
  };
  const requireController = async (body: Record<string, unknown>): Promise<void> => {
    const secret = body.secret;
    if (
      typeof secret !== "string" ||
      secret.length === 0 ||
      !deps.state.controllerCapabilityHash ||
      !equalSecretHash(deps.state.controllerCapabilityHash, secret)
    ) {
      throw new HttpError(403, "Invalid controller capability");
    }
  };

  const requireTree = (body: Record<string, unknown>): IssueKey => {
    const tree = issueKey(body, "tree");
    if (!deps.state.trees[tree] || !deps.state.issues[tree]) {
      throw new HttpError(404, "Unknown tree");
    }
    return tree;
  };

  const requireTreeIssue = (body: Record<string, unknown>): { tree: IssueKey; issue: IssueKey } => {
    const tree = requireTree(body);
    const issue = issueKey(body, "issue");
    if (!deps.state.issues[issue]) {
      throw new HttpError(404, "Unknown issue");
    }
    if (!treeContains(deps.state, tree, issue)) {
      throw new HttpError(403, "Issue is outside tree");
    }
    return { tree, issue };
  };

  const tokenForIssue = async (issue: IssueKey, appRole: GitHubAppRole): Promise<TokenLease> => {
    const parsed = parseIssueKey(issue);
    if (!parsed) {
      throw new Error(`Invalid issue key in Legion state: ${issue}`);
    }
    return deps.tokenManager.getToken(appRole, parsed.owner);
  };

  const gh = async (issue: IssueKey, command: string[], appRole: GitHubAppRole = "implement") => {
    const lease = await tokenForIssue(issue, appRole);
    const result = await runner(command, {
      env: buildRoleEnv(lease.token, lease.gitIdentity, process.env),
    });
    if (result.exitCode !== 0) {
      throw new Error(`GitHub command failed: ${result.stderr || result.stdout}`);
    }
    return result.stdout;
  };
  const dispatchThread = async (
    parent: string,
    subject: string,
    body: string,
    ask: unknown,
    urgency: unknown
  ): Promise<DispatchThreadResult> => {
    const response = await (deps.dispatch.fetch ?? fetch)(
      `${deps.dispatch.url.replace(/\/+$/, "")}/mcp`,
      {
        method: "POST",
        headers: {
          Accept: "application/json, text/event-stream",
          Authorization: `Bearer ${deps.dispatch.bearer}`,
          "Content-Type": "application/json",
        },
        body: JSON.stringify({
          jsonrpc: "2.0",
          id: randomUUID(),
          method: "tools/call",
          params: {
            name: "dispatch",
            arguments: {
              parent,
              subject,
              body,
              ...(ask === undefined ? {} : { ask }),
              ...(urgency === undefined ? {} : { urgency }),
            },
          },
        }),
      }
    );
    const responseBody = await response.text();
    if (!response.ok) {
      throw new Error(`Dispatch MCP request failed with ${response.status}: ${responseBody}`);
    }
    const payload: unknown = JSON.parse(responseBody);
    if (typeof payload !== "object" || payload === null || !("result" in payload)) {
      throw new Error("Dispatch MCP response is missing a result");
    }
    const result = payload.result;
    if (
      typeof result !== "object" ||
      result === null ||
      !("content" in result) ||
      !Array.isArray(result.content)
    ) {
      throw new Error("Dispatch MCP response has invalid content");
    }
    const text = result.content[0];
    if (
      typeof text !== "object" ||
      text === null ||
      !("text" in text) ||
      typeof text.text !== "string"
    ) {
      throw new Error("Dispatch MCP response is missing result text");
    }
    const dispatched: unknown = JSON.parse(text.text);
    if (
      typeof dispatched !== "object" ||
      dispatched === null ||
      !("thread" in dispatched) ||
      typeof dispatched.thread !== "number" ||
      !Number.isInteger(dispatched.thread) ||
      !("url" in dispatched) ||
      typeof dispatched.url !== "string"
    ) {
      throw new Error("Dispatch MCP result must contain an integer thread and URL");
    }
    return { thread: dispatched.thread, url: dispatched.url };
  };

  const appendFooter = (tree: IssueKey, issue: IssueKey, body: string): string => {
    const session =
      deps.state.roles[roleToken(deps.state.project, tree, "architect")]?.sessionId ?? "";
    return `${body}\n\n<!-- legion: ${JSON.stringify({ session, issue })} -->`;
  };

  const resolveGrant = (body: Record<string, unknown>): Grant => {
    const grantId = requiredString(body, "grantId");
    const grant = grants.get(grantId);
    if (!grant || grant.expiresAt <= now()) {
      throw new HttpError(403, "Invalid or expired grant");
    }
    return grant;
  };
  const requireSessionCapability = (
    body: Record<string, unknown>,
    tree: IssueKey,
    issue: IssueKey
  ): SessionCapability => {
    const sessionId = body.sessionId;
    const suppliedSecret = body.secret;
    if (
      typeof sessionId !== "string" ||
      sessionId.length === 0 ||
      typeof suppliedSecret !== "string" ||
      suppliedSecret.length === 0
    ) {
      throw new HttpError(403, "Invalid session secret");
    }
    const capability = capabilities.get(sessionId);
    if (
      !capability ||
      capability.tree !== tree ||
      capability.issue !== issue ||
      !equalSecret(capability.secretHash, suppliedSecret)
    ) {
      throw new HttpError(403, "Invalid session secret");
    }
    return capability;
  };

  const requireArchitectCapability = (
    body: Record<string, unknown>,
    tree: IssueKey
  ): SessionCapability => {
    const sessionId = body.sessionId;
    const issuedCapability =
      typeof sessionId === "string" ? capabilities.get(sessionId) : undefined;
    const capability = requireSessionCapability(body, tree, issuedCapability?.issue ?? tree);
    if (capability.role !== "architect") {
      throw new HttpError(403, "Only an architect may perform lifecycle writes");
    }
    return capability;
  };

  const handler = async (request: Request): Promise<Response> => {
    try {
      const url = new URL(request.url);
      const pathname = url.pathname;
      if (request.method === "GET" && pathname === "/legion/v1/state") {
        const state = structuredClone(deps.state);
        delete state.controllerCapabilityHash;
        const { spawnCapabilities: _spawnCapabilities, ...redacted } = state;
        return Response.json(validateContractResponse(LegionDaemonApi.State.response, redacted));
      }
      if (request.method !== "POST") {
        throw new HttpError(404, "Not found");
      }
      const body = asRecord(await request.json());

      if (pathname === "/legion/v1/process/started") {
        const tree = requireTree(body);
        const generation = requiredNumber(body, "generation");
        const treeState = deps.state.trees[tree];
        if (!treeState || treeState.generation !== generation) {
          throw new HttpError(409, "Stale process generation");
        }
        const bootToken = body.bootToken;
        if (typeof bootToken !== "string" || bootToken.length === 0) {
          throw new HttpError(403, "Invalid root boot token");
        }
        const boot = bootTokens.get(bootToken);
        if (!boot || boot.tree !== tree || boot.generation !== generation) {
          throw new HttpError(403, "Invalid root boot token");
        }
        bootTokens.delete(bootToken);
        const rootSessionId = requiredString(body, "rootSessionId");
        const agentId = requiredString(body, "agentId");
        const ompSessionFile = requiredString(body, "ompSessionFile");
        if (!treeState.locator) {
          throw new Error(`Tree ${tree} is missing its process locator`);
        }
        treeState.status = "active";
        treeState.locator = { ...treeState.locator, ompSessionFile };
        const roles: Record<LegionRole, string> = {
          architect: roleToken(deps.state.project, tree, "architect"),
          planner: roleToken(deps.state.project, tree, "planner"),
          implementer: roleToken(deps.state.project, tree, "implementer"),
          tester: roleToken(deps.state.project, tree, "tester"),
          reviewer: roleToken(deps.state.project, tree, "reviewer"),
          merger: roleToken(deps.state.project, tree, "merger"),
        };
        deps.state.roles[roles.architect] = {
          issue: tree,
          role: "architect",
          sessionId: rootSessionId,
          agentId,
        };
        const secret = randomUUID();
        capabilities.set(rootSessionId, {
          tree,
          issue: tree,
          role: "architect",
          secretHash: secretHash(secret),
        });
        await save();
        validateContractRequest(LegionDaemonApi.ProcessStarted.request, body);
        return Response.json(
          validateContractResponse(LegionDaemonApi.ProcessStarted.response, {
            roleTokens: roles,
            controlSubject: `legion.ctl.${sanitizeToken(tree)}.${generation}`,
            gates: config.gates,
            secret,
          })
        );
      }
      if (pathname === "/legion/v1/process/ready") {
        const tree = requireTree(body);
        requireArchitectCapability(body, tree);
        await deps.onTreeReady?.(tree);
        await deps.processManager.markTreeReady(tree);
        validateContractRequest(LegionDaemonApi.ProcessReady.request, body);
        return Response.json(validateContractResponse(LegionDaemonApi.ProcessReady.response, {}));
      }

      if (pathname === "/legion/v1/process/exit") {
        const tree = requireTree(body);
        requireArchitectCapability(body, tree);
        const generation = requiredNumber(body, "generation");
        const treeState = deps.state.trees[tree];
        if (!treeState || treeState.generation !== generation) {
          throw new HttpError(409, "Stale process generation");
        }
        if (deps.state.issues[tree]?.state === "closed") {
          await deps.processManager.closeTree(tree);
        } else {
          await deps.processManager.markProcessDead(tree);
        }
        await save();
        validateContractRequest(LegionDaemonApi.ProcessExit.request, body);
        return Response.json(validateContractResponse(LegionDaemonApi.ProcessExit.response, {}));
      }

      if (pathname === "/legion/v1/spawn-token") {
        const { tree, issue } = requireTreeIssue(body);
        const role = legionRole(requiredString(body, "role"));
        requireArchitectCapability(body, tree);
        const spawnToken = randomUUID();
        deps.state.spawnCapabilities[spawnCapabilityKey(spawnToken)] = {
          tree,
          issue,
          role,
        };
        await save();
        validateContractRequest(LegionDaemonApi.SpawnToken.request, body);
        return Response.json(
          validateContractResponse(LegionDaemonApi.SpawnToken.response, {
            spawnToken,
          })
        );
      }

      if (pathname === "/legion/v1/issues") {
        const tree = requireTree(body);
        requireArchitectCapability(body, tree);
        const title = requiredString(body, "title");
        const issueBody = requiredString(body, "body");
        const labels = optionalStrings(body, "labels");
        if (labels.some((label) => label !== "needs-approval")) {
          throw new HttpError(400, "Architect issue creation only accepts needs-approval");
        }
        const childLabels = [...new Set([...labels, "legion-child"])];
        const parsedTree = parseIssueKey(tree);
        if (!parsedTree) {
          throw new Error(`Invalid issue key in Legion state: ${tree}`);
        }
        const createCommand = [
          "gh",
          "api",
          `repos/${parsedTree.owner}/${parsedTree.repo}/issues`,
          "-f",
          `title=${title}`,
          "-f",
          `body=${issueBody}`,
          ...childLabels.flatMap((label) => ["-f", `labels[]=${label}`]),
        ];
        const created = asRecord(JSON.parse(await gh(tree, createCommand)));
        const number = created.number;
        const createdUrl = created.html_url;
        const childNodeId = created.node_id;
        if (
          typeof number !== "number" ||
          !Number.isInteger(number) ||
          typeof createdUrl !== "string" ||
          typeof childNodeId !== "string"
        ) {
          throw new Error("GitHub issue create returned invalid response");
        }
        const child = formatIssueKey(parsedTree.owner, parsedTree.repo, number);
        const parent = asRecord(JSON.parse(await gh(tree, ["gh", "api", issueUrl(tree)])));
        const parentNodeId = parent.node_id;
        if (typeof parentNodeId !== "string") {
          throw new Error("GitHub parent issue returned invalid response");
        }
        const mutation =
          "mutation($parentId: ID!, $childId: ID!) { addSubIssue(input: {issueId: $parentId, subIssueId: $childId}) { issue { id } } }";
        await gh(tree, [
          "gh",
          "api",
          "graphql",
          "-f",
          `query=${mutation}`,
          "-F",
          `parentId=${parentNodeId}`,
          "-F",
          `childId=${childNodeId}`,
        ]);
        deps.state.issues[child] = {
          key: child,
          title,
          parent: tree,
          state: "open",
          children: [],
          released: false,
          labels: childLabels,
        };
        deps.state.issues[tree]?.children.push(child);
        await save();
        validateContractRequest(LegionDaemonApi.IssueCreate.request, body);
        return Response.json(
          validateContractResponse(LegionDaemonApi.IssueCreate.response, {
            issue: child,
            url: createdUrl,
          })
        );
      }

      if (pathname === "/legion/v1/waves/release") {
        const tree = requireTree(body);
        requireArchitectCapability(body, tree);
        const children = optionalStrings(body, "children").map((child) =>
          issueKey({ child }, "child")
        );
        for (const child of children) {
          if (!treeContains(deps.state, tree, child)) {
            throw new HttpError(403, "Issue is outside tree");
          }
        }
        const treeState = deps.state.trees[tree];
        if (!treeState) {
          throw new HttpError(404, "Unknown tree");
        }
        for (const child of children) {
          const node = deps.state.issues[child];
          if (node) {
            node.released = true;
          }
        }
        const released = new Set(children);
        const retained = [];
        for (const held of treeState.heldEvents) {
          const matchingChild = children.find((child) =>
            matchingHeldEvent(deps.state, child, held.role)
          );
          if (!matchingChild || !released.has(matchingChild)) {
            retained.push(held);
            continue;
          }
          try {
            await deps.envoyPublish(`notifications.role.${held.role}`, held.payloadJson);
          } catch {
            retained.push(held);
          }
        }
        treeState.heldEvents = retained;
        await save();
        validateContractRequest(LegionDaemonApi.WaveRelease.request, body);
        return Response.json(
          validateContractResponse(LegionDaemonApi.WaveRelease.response, {
            released: children,
          })
        );
      }

      if (pathname === "/legion/v1/issues/comment") {
        const { tree, issue } = requireTreeIssue(body);
        requireArchitectCapability(body, tree);
        const commentBody = appendFooter(tree, issue, requiredString(body, "body"));
        const result = asRecord(
          JSON.parse(
            await gh(issue, [
              "gh",
              "api",
              `${issueUrl(issue)}/comments`,
              "-f",
              `body=${commentBody}`,
            ])
          )
        );
        const commentId = result.id;
        const commentUrl = result.html_url;
        if (typeof commentId !== "number" || typeof commentUrl !== "string") {
          throw new Error("GitHub comment create returned invalid response");
        }
        await save();
        validateContractRequest(LegionDaemonApi.Comment.request, body);
        return Response.json(
          validateContractResponse(LegionDaemonApi.Comment.response, {
            commentId,
            url: commentUrl,
          })
        );
      }

      if (pathname === "/legion/v1/issues/body") {
        const { tree, issue } = requireTreeIssue(body);
        requireArchitectCapability(body, tree);
        await gh(issue, [
          "gh",
          "api",
          "-X",
          "PATCH",
          issueUrl(issue),
          "-f",
          `body=${requiredString(body, "body")}`,
        ]);
        await save();
        validateContractRequest(LegionDaemonApi.PostBody.request, body);
        return Response.json(validateContractResponse(LegionDaemonApi.PostBody.response, {}));
      }

      if (pathname === "/legion/v1/issues/labels") {
        const { tree, issue } = requireTreeIssue(body);
        requireArchitectCapability(body, tree);
        const add = optionalStrings(body, "add");
        const remove = optionalStrings(body, "remove");
        if (add.some((label) => label !== "needs-approval") || remove.length > 0) {
          throw new HttpError(400, "Architect label changes only add needs-approval");
        }
        if (add.length > 0) {
          await gh(issue, [
            "gh",
            "api",
            "-X",
            "POST",
            `${issueUrl(issue)}/labels`,
            ...add.flatMap((label) => ["-f", `labels[]=${label}`]),
          ]);
        }
        const node = deps.state.issues[issue];
        if (!node) {
          throw new HttpError(404, "Unknown issue");
        }
        node.labels = [...new Set([...node.labels, ...add])];
        await save();
        validateContractRequest(LegionDaemonApi.Labels.request, body);
        return Response.json(
          validateContractResponse(LegionDaemonApi.Labels.response, {
            labels: node.labels,
          })
        );
      }

      if (pathname === "/legion/v1/issues/close") {
        const { tree, issue } = requireTreeIssue(body);
        requireArchitectCapability(body, tree);
        const comment = body.comment;
        if (comment !== undefined && typeof comment !== "string") {
          throw new HttpError(400, "Expected string comment");
        }
        let finalCommentRef: string | undefined;
        if (comment) {
          const result = asRecord(
            JSON.parse(
              await gh(issue, [
                "gh",
                "api",
                `${issueUrl(issue)}/comments`,
                "-f",
                `body=${appendFooter(tree, issue, comment)}`,
              ])
            )
          );
          const commentId = result.id;
          const commentUrl = result.html_url;
          if (typeof commentId !== "number" || typeof commentUrl !== "string") {
            throw new Error("GitHub comment create returned invalid response");
          }
          finalCommentRef = commentUrl;
        }
        await gh(issue, ["gh", "api", "-X", "PATCH", issueUrl(issue), "-f", "state=closed"]);
        const node = deps.state.issues[issue];
        if (node) {
          node.state = "closed";
          node.finalCommentRef = finalCommentRef;
        }
        deps.state.dispatchThreads = deps.state.dispatchThreads.filter(
          (thread) => thread.issue !== issue
        );
        if (issue === tree) deps.processManager.beginLinger(tree);
        validateContractRequest(LegionDaemonApi.IssueClose.request, body);
        await save();
        return Response.json(validateContractResponse(LegionDaemonApi.IssueClose.response, {}));
      }

      if (pathname === "/legion/v1/escalate") {
        const tree = requireTree(body);
        requireArchitectCapability(body, tree);
        const kind = requiredString(body, "kind");
        if (kind !== "re-file" && kind !== "capacity" && kind !== "cross-tree") {
          throw new HttpError(400, "Unknown escalation kind");
        }
        if (!("context" in body)) {
          throw new HttpError(400, "Expected context");
        }
        const controllerEvent = {
          type: "escalate",
          tree,
          kind,
          context: body.context,
        };
        validateContractRequest(LegionDaemonApi.Escalate.request, body);
        await deps.onControllerEvent(controllerEvent);
        return Response.json(validateContractResponse(LegionDaemonApi.Escalate.response, {}));
      }

      if (pathname === "/legion/v1/dispatch-threads") {
        const { tree, issue } = requireTreeIssue(body);
        const capability = requireSessionCapability(body, tree, issue);
        const role = legionRole(requiredString(body, "role"));
        if (role !== capability.role) {
          throw new HttpError(403, "Dispatch role does not match the authenticated session");
        }
        if (role !== "architect") {
          throw new HttpError(403, "Only an architect may open a Dispatch thread");
        }
        const parent = issueKey(body, "parent");
        if (!treeContains(deps.state, tree, parent)) {
          throw new HttpError(403, "Dispatch parent is outside the caller tree");
        }
        const subject = requiredString(body, "subject");
        const dispatchBody = requiredString(body, "body");
        const urgency = body.urgency;
        if (
          urgency !== undefined &&
          urgency !== "low" &&
          urgency !== "med" &&
          urgency !== "high" &&
          urgency !== "blocking"
        ) {
          throw new HttpError(400, "Invalid Dispatch urgency");
        }
        const dispatched = await dispatchThread(parent, subject, dispatchBody, body.ask, urgency);
        const parsedIssue = parseIssueKey(issue);
        if (!parsedIssue) {
          throw new Error(`Invalid issue key in Legion state: ${issue}`);
        }
        const repo = `${parsedIssue.owner}/${parsedIssue.repo}` as `${string}/${string}`;
        const existingMapping = deps.state.dispatchThreads.find(
          (entry) => entry.repo === repo && entry.thread === dispatched.thread
        );
        if (existingMapping && (existingMapping.tree !== tree || existingMapping.issue !== issue)) {
          throw new HttpError(403, "Dispatch thread is already owned by another issue");
        }
        const entry = { repo, thread: dispatched.thread, role, issue, tree };
        if (existingMapping) {
          const index = deps.state.dispatchThreads.indexOf(existingMapping);
          deps.state.dispatchThreads[index] = entry;
        } else {
          deps.state.dispatchThreads.push(entry);
        }
        await save();
        validateContractRequest(LegionDaemonApi.DispatchThread.request, body);
        return Response.json(
          validateContractResponse(LegionDaemonApi.DispatchThread.response, dispatched)
        );
      }

      if (pathname === "/legion/v1/phase") {
        const { tree, issue } = requireTreeIssue(body);
        const phase = requiredString(body, "phase");
        const sessionId = requiredString(body, "sessionId");
        const role = roleForSession(deps.state, issue, sessionId, phase);
        const spawnToken = requiredString(body, "spawnToken");
        const expectedSpawn = deps.state.spawnCapabilities[spawnCapabilityKey(spawnToken)];
        if (
          !expectedSpawn ||
          expectedSpawn.tree !== tree ||
          expectedSpawn.issue !== issue ||
          expectedSpawn.role !== role
        ) {
          throw new HttpError(
            403,
            "Worker session is not bound to a matching daemon-issued spawn token"
          );
        }
        const secret = randomUUID();
        capabilities.set(sessionId, {
          tree,
          issue,
          role,
          secretHash: secretHash(secret),
        });
        deps.state.phases[issue] = { phase, sessionId };
        const token = roleToken(deps.state.project, issue, role);
        const existing = deps.state.roles[token];
        deps.state.roles[token] = {
          issue,
          role,
          sessionId,
          ...(existing && "issue" in existing && existing.agentId
            ? { agentId: existing.agentId }
            : {}),
        };
        deps.state.attribution.push({ issue, phase, sessionId });
        const lease = await tokenForIssue(issue, appRoleForLegionRole(role));
        await save();
        validateContractRequest(LegionDaemonApi.Phase.request, body);
        return Response.json(
          validateContractResponse(LegionDaemonApi.Phase.response, {
            secret,
            gitName: lease.gitIdentity.name,
            gitEmail: lease.gitIdentity.email,
          })
        );
      }

      if (pathname === "/legion/v1/worker-session") {
        const sessionId = requiredString(body, "sessionId");
        const agentId = requiredString(body, "agentId");
        const claim = Object.values(deps.state.roles).find(
          (candidate) =>
            "issue" in candidate &&
            candidate.sessionId === sessionId &&
            candidate.agentId === agentId
        );
        if (!claim || !("issue" in claim)) {
          throw new HttpError(403, "Worker session is not registered for this agent");
        }
        const tree = Object.values(deps.state.trees).find((candidate) =>
          treeContains(deps.state, candidate.root, claim.issue)
        )?.root;
        if (!tree) {
          throw new HttpError(404, "Worker issue is not in an active tree");
        }
        const role = legionRole(claim.role);
        const secret = randomUUID();
        capabilities.set(sessionId, {
          tree,
          issue: claim.issue,
          role,
          secretHash: secretHash(secret),
        });
        validateContractRequest(LegionDaemonApi.WorkerSession.request, body);
        return Response.json(
          validateContractResponse(LegionDaemonApi.WorkerSession.response, {
            tree,
            issue: claim.issue,
            role,
            secret,
          })
        );
      }

      if (pathname === "/legion/v1/role-backing") {
        const { tree, issue } = requireTreeIssue(body);
        const role = legionRole(requiredString(body, "role"));
        const agentId = requiredString(body, "agentId");
        const sessionId = requiredString(body, "sessionId");
        const spawnToken = requiredString(body, "spawnToken");
        const expectedSpawn = deps.state.spawnCapabilities[spawnCapabilityKey(spawnToken)];
        if (
          !expectedSpawn ||
          expectedSpawn.tree !== tree ||
          expectedSpawn.issue !== issue ||
          expectedSpawn.role !== role
        ) {
          throw new HttpError(403, "Unknown or mismatched Legion spawn token");
        }
        deps.state.roles[roleToken(deps.state.project, issue, role)] = {
          issue,
          role,
          sessionId,
          agentId,
        };
        await deps.processManager.registerRoleBacking(tree, issue, role, agentId);
        validateContractRequest(LegionDaemonApi.RoleBacking.request, body);
        await save();
        return Response.json(validateContractResponse(LegionDaemonApi.RoleBacking.response, {}));
      }

      if (pathname === "/legion/v1/provisioning-credential") {
        const { tree, issue } = requireTreeIssue(body);
        requireArchitectCapability(body, tree);
        const lease = await tokenForIssue(issue, "implement");
        validateContractRequest(LegionDaemonApi.ProvisioningCredential.request, body);
        return Response.json(
          validateContractResponse(LegionDaemonApi.ProvisioningCredential.response, {
            token: lease.token,
          })
        );
      }

      if (pathname === "/legion/v1/grants") {
        const { tree, issue } = requireTreeIssue(body);
        const capability = requireSessionCapability(body, tree, issue);
        const grantId = randomUUID();
        const expiresAt = now() + GRANT_TTL_MS;
        grants.set(grantId, { issue, role: capability.role, expiresAt });
        validateContractRequest(LegionDaemonApi.Grant.request, body);
        return Response.json(
          validateContractResponse(LegionDaemonApi.Grant.response, {
            grantId,
            expiresAt: new Date(expiresAt).toISOString(),
          })
        );
      }

      if (pathname === "/legion/v1/git-credential") {
        const grant = resolveGrant(body);
        const lease = await tokenForIssue(grant.issue, appRoleForLegionRole(grant.role));
        return new Response(`username=x-access-token\npassword=${lease.token}`, {
          headers: { "content-type": "text/plain; charset=utf-8" },
        });
      }

      if (pathname === "/legion/v1/gh-token") {
        const grant = resolveGrant(body);
        const lease = await tokenForIssue(grant.issue, appRoleForLegionRole(grant.role));
        return Response.json({
          token: lease.token,
          appLogin: lease.gitIdentity.name.endsWith("[bot]")
            ? lease.gitIdentity.name.slice(0, -"[bot]".length)
            : lease.gitIdentity.name,
        });
      }

      // T24 controller startup contract: POST this with the session ID from envoy_whoami
      // immediately after boot so held events are redelivered before any work endpoint.
      if (pathname === "/legion/v1/controller/ready") {
        await requireController(body);
        const sessionId = requiredString(body, "sessionId");
        deps.state.roles[controllerToken(deps.state.project)] = {
          role: "controller",
          sessionId,
        };
        await save();
        if (!controllerReady) {
          controllerReady = true;
          await deps.onControllerReady();
        }
        validateContractRequest(LegionDaemonApi.ControllerReady.request, body);
        return Response.json(
          validateContractResponse(LegionDaemonApi.ControllerReady.response, {})
        );
      }

      if (pathname === "/legion/v1/gates/approve") {
        await requireController(body);
        const issue = issueKey(body, "issue");
        const node = deps.state.issues[issue];
        if (!node) {
          throw new HttpError(404, "Unknown issue");
        }
        await gh(issue, [
          "gh",
          "api",
          "-X",
          "POST",
          `${issueUrl(issue)}/labels`,
          "-f",
          "labels[]=human-approved",
        ]);
        await gh(issue, ["gh", "api", "-X", "DELETE", `${issueUrl(issue)}/labels/needs-approval`]);
        node.labels = [
          ...new Set([
            ...node.labels.filter((label) => label !== "needs-approval"),
            "human-approved",
          ]),
        ];
        validateContractRequest(LegionDaemonApi.GatesApprove.request, body);
        await save();
        return Response.json(validateContractResponse(LegionDaemonApi.GatesApprove.response, {}));
      }

      if (pathname === "/legion/v1/admission") {
        await requireController(body);
        const issue = issueKey(body, "issue");
        if (!deps.state.issues[issue]) {
          throw new HttpError(404, "Unknown issue");
        }
        const result = deps.processManager.admit(issue);
        await save();
        validateContractRequest(LegionDaemonApi.Admission.request, body);
        return Response.json(
          validateContractResponse(LegionDaemonApi.Admission.response, {
            result,
          })
        );
      }

      if (pathname === "/legion/v1/backlog") {
        await requireController(body);
        const issue = issueKey(body, "issue");
        const marker = requiredString(body, "marker");
        const node = deps.state.issues[issue];
        if (!node) {
          throw new HttpError(404, "Unknown issue");
        }
        await gh(issue, [
          "gh",
          "api",
          "-X",
          "POST",
          `${issueUrl(issue)}/labels`,
          "-f",
          "labels[]=legion-backlog",
        ]);
        node.labels = [...new Set([...node.labels, "legion-backlog"])];
        node.backlogMarker = marker;
        validateContractRequest(LegionDaemonApi.Backlog.request, body);
        await save();
        return Response.json(validateContractResponse(LegionDaemonApi.Backlog.response, {}));
      }

      throw new HttpError(404, "Not found");
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
      bootTokens.set(bootToken, { tree, generation });
      return bootToken;
    },
    mintControllerCapability: async () => {
      const secret = randomUUID();
      deps.state.controllerCapabilityHash = secretHash(secret).toString("hex");
      controllerReady = false;
      await save();
      return secret;
    },
    stop: () => server.stop(true),
  };
}
