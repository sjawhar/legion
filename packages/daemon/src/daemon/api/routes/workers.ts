import { randomUUID } from "node:crypto";
import { LegionDaemonApi, roleToken } from "@legion/contracts";
import { secretHash, spawnCapabilityKey } from "../auth";
import { type RouteContext, roleForSession, treeContains } from "../context";
import { appRoleForLegionRole } from "../github";
import { HttpError, legionRole, requiredString, validateContractResponse } from "../http";

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
  const spawnToken = requiredString(body, "spawnToken");
  const expectedSpawn = ctx.deps.state.spawnCapabilities[spawnCapabilityKey(spawnToken)];
  if (
    !expectedSpawn ||
    expectedSpawn.tree !== tree ||
    expectedSpawn.issue !== issue ||
    expectedSpawn.role !== role
  ) {
    throw new HttpError(403, "Worker session is not bound to a matching daemon-issued spawn token");
  }
  const secret = randomUUID();
  ctx.auth.setCapability(sessionId, {
    tree,
    issue,
    role,
    secretHash: secretHash(secret),
  });
  ctx.deps.state.phases[issue] = { phase, sessionId };
  const token = roleToken(ctx.deps.state.project, issue, role);
  const existing = ctx.deps.state.roles[token];
  ctx.deps.state.roles[token] = {
    issue,
    role,
    sessionId,
    ...(existing && "issue" in existing && existing.agentId ? { agentId: existing.agentId } : {}),
  };
  ctx.deps.state.attribution.push({ issue, phase, sessionId });
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
  const claim =
    boot?.sessionId === sessionId &&
    rootClaim &&
    "issue" in rootClaim &&
    rootClaim.issue === boot.tree &&
    rootClaim.sessionId === sessionId
      ? rootClaim
      : workerClaim;
  const tree = boot?.sessionId === sessionId ? boot.tree : spawn?.tree;
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
  const spawnToken = requiredString(body, "spawnToken");
  const expectedSpawn = ctx.deps.state.spawnCapabilities[spawnCapabilityKey(spawnToken)];
  if (
    !expectedSpawn ||
    expectedSpawn.tree !== tree ||
    expectedSpawn.issue !== issue ||
    expectedSpawn.role !== role
  ) {
    throw new HttpError(403, "Unknown or mismatched Legion spawn token");
  }
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
