import { randomUUID } from "node:crypto";
import { LegionDaemonApi, roleToken } from "@legion/contracts";
import type { WorkerRoleClaim } from "../../legion-state";
import { equalSecretHash, secretHash, spawnCapabilityKey } from "../auth";
import { type RouteContext, roleForSession, rootForIssue, treeContains } from "../context";
import { appRoleForLegionRole } from "../github";
import {
  HttpError,
  legionRole,
  requiredNumber,
  requiredString,
  validateContractResponse,
} from "../http";

/** Rejects the request unless the supplied spawn token was minted for exactly this tree/issue/role. */
function requireSpawnCapability(
  ctx: RouteContext,
  body: Record<string, unknown>,
  expected: { tree: string; issue: string; role: string },
  message: string
): void {
  const spawnToken = requiredString(body, "spawnToken");
  const spawn = ctx.deps.state.spawnCapabilities[spawnCapabilityKey(spawnToken)];
  if (
    !spawn ||
    spawn.tree !== expected.tree ||
    spawn.issue !== expected.issue ||
    spawn.role !== expected.role
  ) {
    throw new HttpError(403, message);
  }
}

export async function handleSpawnToken(
  ctx: RouteContext,
  body: Record<string, unknown>
): Promise<Response> {
  const { tree, issue } = ctx.requireTreeIssue(body);
  const role = legionRole(requiredString(body, "role"));
  ctx.auth.requireArchitectCapability(body, tree);
  const spawnToken = randomUUID();
  ctx.deps.state.spawnCapabilities[spawnCapabilityKey(spawnToken)] = {
    tree,
    issue,
    role,
  };
  await ctx.save();
  return Response.json(
    validateContractResponse(LegionDaemonApi.SpawnToken.response, {
      spawnToken,
    })
  );
}

export async function handlePhase(
  ctx: RouteContext,
  body: Record<string, unknown>
): Promise<Response> {
  const { tree, issue } = ctx.requireTreeIssue(body);
  const phase = requiredString(body, "phase");
  const sessionId = requiredString(body, "sessionId");
  const role = roleForSession(ctx.deps.state, issue, sessionId, phase);
  requireSpawnCapability(
    ctx,
    body,
    { tree, issue, role },
    "Worker session is not bound to a matching daemon-issued spawn token"
  );
  const secret = randomUUID();
  ctx.auth.setCapability(sessionId, {
    tree,
    issue,
    role,
    secretHash: secretHash(secret),
  });
  ctx.deps.state.phases[issue] = { phase: role, sessionId };
  const token = roleToken(ctx.deps.state.project, issue, role);
  const existing = ctx.deps.state.roles[token];
  ctx.deps.state.roles[token] = {
    issue,
    role,
    sessionId,
    ...(existing && "issue" in existing && existing.agentId ? { agentId: existing.agentId } : {}),
  };
  const lease = await ctx.github.tokenForIssue(issue, appRoleForLegionRole(role));
  await ctx.save();
  return Response.json(
    validateContractResponse(LegionDaemonApi.Phase.response, {
      secret,
      gitName: lease.gitIdentity.name,
      gitEmail: lease.gitIdentity.email,
    })
  );
}

export async function handleWorkerSession(
  ctx: RouteContext,
  body: Record<string, unknown>
): Promise<Response> {
  const sessionId = requiredString(body, "sessionId");
  const recoveryToken = requiredString(body, "recoveryToken");
  const boot = ctx.auth.getBootToken(recoveryToken);
  const spawn = ctx.deps.state.spawnCapabilities[spawnCapabilityKey(recoveryToken)];
  const rootClaim = boot
    ? ctx.deps.state.roles[roleToken(ctx.deps.state.project, boot.tree, "architect")]
    : undefined;
  const workerClaim = spawn
    ? Object.values(ctx.deps.state.roles).find(
        (candidate) =>
          "issue" in candidate &&
          candidate.sessionId === sessionId &&
          candidate.issue === spawn.issue &&
          candidate.role === spawn.role
      )
    : undefined;
  // Durable phase-worker recovery: `worker/started` hashes its boot token onto the claim, so a
  // restarted daemon (whose in-memory boot-token maps are gone) can still bind a reconnecting
  // worker to the same claim it already registered, exactly like the root's boot-token path.
  const durableClaim = Object.values(ctx.deps.state.roles).find(
    (candidate): candidate is WorkerRoleClaim =>
      "issue" in candidate &&
      candidate.bootTokenHash !== undefined &&
      candidate.sessionId === sessionId &&
      equalSecretHash(candidate.bootTokenHash, recoveryToken)
  );
  const durableTree = durableClaim ? rootForIssue(ctx.deps.state, durableClaim.issue) : undefined;
  const claim =
    boot?.sessionId === sessionId &&
    rootClaim &&
    "issue" in rootClaim &&
    rootClaim.issue === boot.tree &&
    rootClaim.sessionId === sessionId
      ? rootClaim
      : (workerClaim ?? durableClaim);
  const tree = boot?.sessionId === sessionId ? boot.tree : (spawn?.tree ?? durableTree);
  if (
    !claim ||
    !("issue" in claim) ||
    !tree ||
    !ctx.deps.state.trees[tree] ||
    !treeContains(ctx.deps.state, tree, claim.issue)
  ) {
    throw new HttpError(403, "Worker session is not bound to a daemon-issued recovery token");
  }
  const role = legionRole(claim.role);
  const secret = randomUUID();
  ctx.auth.setCapability(sessionId, {
    tree,
    issue: claim.issue,
    role,
    secretHash: secretHash(secret),
  });
  return Response.json(
    validateContractResponse(LegionDaemonApi.WorkerSession.response, {
      tree,
      issue: claim.issue,
      role,
      secret,
    })
  );
}

export async function handleRoleBacking(
  ctx: RouteContext,
  body: Record<string, unknown>
): Promise<Response> {
  const { tree, issue } = ctx.requireTreeIssue(body);
  const role = legionRole(requiredString(body, "role"));
  const agentId = requiredString(body, "agentId");
  const sessionId = requiredString(body, "sessionId");
  requireSpawnCapability(
    ctx,
    body,
    { tree, issue, role },
    "Unknown or mismatched Legion spawn token"
  );
  ctx.deps.state.roles[roleToken(ctx.deps.state.project, issue, role)] = {
    issue,
    role,
    sessionId,
    agentId,
  };
  await ctx.deps.processManager.registerRoleBacking(tree, issue, role, agentId);
  await ctx.save();
  return Response.json(validateContractResponse(LegionDaemonApi.RoleBacking.response, {}));
}

export async function handleWorkerStarted(
  ctx: RouteContext,
  body: Record<string, unknown>
): Promise<Response> {
  const { tree, issue } = ctx.requireTreeIssue(body);
  const role = legionRole(requiredString(body, "role"));
  const bootToken = requiredString(body, "bootToken");
  const sessionId = requiredString(body, "sessionId");
  const boot = ctx.auth.getWorkerBootToken(bootToken);
  if (
    !boot ||
    boot.tree !== tree ||
    boot.issue !== issue ||
    boot.role !== role ||
    (boot.sessionId !== undefined && boot.sessionId !== sessionId)
  ) {
    throw new HttpError(403, "Invalid worker boot token");
  }
  const token = roleToken(ctx.deps.state.project, issue, role);
  const claim = ctx.deps.state.roles[token];
  if (!claim || !("issue" in claim) || claim.generation !== boot.generation || !claim.locator) {
    throw new HttpError(409, "Stale worker generation");
  }
  if (boot.expectedSessionId !== undefined && boot.expectedSessionId !== sessionId) {
    throw new HttpError(409, "Worker respawn must resume the same agent session");
  }
  const agentId = requiredString(body, "agentId");
  const ompSessionFile = requiredString(body, "ompSessionFile");
  // Fallible work runs before any mutation: a transient tokenForIssue failure leaves the boot
  // token and claim untouched, so a retry with the same {bootToken, sessionId} starts clean.
  const lease = await ctx.github.tokenForIssue(issue, appRoleForLegionRole(role));
  const secret = randomUUID();

  // Build the new claim as a local draft rather than mutating the live one in place, so a save
  // failure can be rolled back by simply restoring the old reference — leaving the in-memory
  // claim exactly as durable as what was ever written to disk, and a retry with the same
  // {bootToken, sessionId} starting where the first attempt did.
  const priorClaim = claim;
  const priorPhase = ctx.deps.state.phases[issue];
  ctx.deps.state.roles[token] = {
    ...claim,
    sessionId,
    agentId,
    bootTokenHash: secretHash(bootToken).toString("hex"),
    locator: { ...claim.locator, ompSessionFile },
  };
  ctx.deps.state.phases[issue] = { phase: role, sessionId };
  try {
    await ctx.save();
  } catch (error) {
    ctx.deps.state.roles[token] = priorClaim;
    if (priorPhase === undefined) delete ctx.deps.state.phases[issue];
    else ctx.deps.state.phases[issue] = priorPhase;
    throw error;
  }

  // Only after the durable state is persisted do we consume the boot token and mint the session
  // capability: both are ephemeral (never part of `ctx.save()`'s payload), so the save-failure
  // rollback above needed no counterpart for them — they were never touched in that case.
  boot.sessionId = sessionId;
  ctx.auth.setCapability(sessionId, { tree, issue, role, secretHash: secretHash(secret) });
  return Response.json(
    validateContractResponse(LegionDaemonApi.WorkerStarted.response, {
      roleToken: token,
      secret,
      gitName: lease.gitIdentity.name,
      gitEmail: lease.gitIdentity.email,
    })
  );
}

export async function handleWorkerReady(
  ctx: RouteContext,
  body: Record<string, unknown>
): Promise<Response> {
  const { tree, issue } = ctx.requireTreeIssue(body);
  const role = legionRole(requiredString(body, "role"));
  const capability = ctx.auth.requireSessionCapability(body, tree, issue);
  if (capability.role !== role) {
    throw new HttpError(403, "Session role does not match worker/ready request");
  }
  const sessionId = requiredString(body, "sessionId");
  const generation = requiredNumber(body, "generation");
  await ctx.deps.processManager.workerReady(issue, role, sessionId, generation);
  return Response.json(validateContractResponse(LegionDaemonApi.WorkerReady.response, {}));
}

export async function handleSpawnWorker(
  ctx: RouteContext,
  body: Record<string, unknown>
): Promise<Response> {
  const { tree, issue } = ctx.requireTreeIssue(body);
  ctx.auth.requireArchitectCapability(body, tree);
  const role = legionRole(requiredString(body, "role"));
  if (role === "architect" && issue === tree) {
    throw new HttpError(
      400,
      "The root architect is not spawnable through spawn_worker; it is started by process/started"
    );
  }
  const task = requiredString(body, "task");
  const result = await ctx.deps.processManager.spawnWorker(tree, issue, role, task);
  return Response.json(validateContractResponse(LegionDaemonApi.SpawnWorker.response, result));
}
