import { randomUUID } from "node:crypto";
import {
  type IssueKey,
  LegionDaemonApi,
  type LegionRole,
  roleToken,
  roleTopic,
} from "@legion/contracts";
import type { WorkerRoleClaim } from "../../legion-state";
import { equalSecretHash, secretHash, spawnCapabilityKey } from "../auth";
import { type RouteContext, roleForSession, rootForIssue, treeContains } from "../context";
import { appRoleForLegionRole } from "../github";
import {
  EnvoyPublishError,
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
    ...(existing && "issue" in existing ? existing : {}),
    issue,
    role,
    sessionId,
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
  await ctx.deps.processManager.registerRoleBacking(tree, issue, role, agentId, sessionId);
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

interface WorkerSession {
  tree: IssueKey;
  issue: IssueKey;
  role: LegionRole;
  sessionId: string;
}

/** Verifies the worker's own session capability and that its claimed role matches the request —
 * used by `worker/ready`, the one remaining route a phase worker still calls with its session
 * secret directly (`phase/complete` authenticates via a short-lived grant instead — see below).
 * `requestName` names the request in the 403 message. */
function requireWorkerSession(
  ctx: RouteContext,
  body: Record<string, unknown>,
  requestName: string
): WorkerSession {
  const { tree, issue } = ctx.requireTreeIssue(body);
  const role = legionRole(requiredString(body, "role"));
  const capability = ctx.auth.requireSessionCapability(body, tree, issue);
  if (capability.role !== role) {
    throw new HttpError(403, `Session role does not match ${requestName} request`);
  }
  const sessionId = requiredString(body, "sessionId");
  return { tree, issue, role, sessionId };
}

export async function handleWorkerReady(
  ctx: RouteContext,
  body: Record<string, unknown>
): Promise<Response> {
  const { issue, role, sessionId } = requireWorkerSession(ctx, body, "worker/ready");
  const generation = requiredNumber(body, "generation");
  await ctx.deps.processManager.workerReady(issue, role, sessionId, generation);
  return Response.json(validateContractResponse(LegionDaemonApi.WorkerReady.response, {}));
}

/**
 * A worker reports its phase done. Authenticates like `legion gh`/`legion credential`: a
 * short-lived grant (`LEGION_GRANT`) resolved via `ctx.auth.resolveGrant` — never a live session
 * secret in the request body. Verifies the claim for (issue, grant.role) still belongs to the
 * grant's session, and that the issue's active phase still belongs to this exact worker (a
 * retained worker resumed for a later reassignment keeps the same sessionId, so a duplicate/late
 * completion from a superseded phase is rejected on the phase check alone). The phase is captured
 * and cleared synchronously, before the publish `await`, so a second concurrent completion for
 * the same phase always finds it already gone and 409s instead of both publishing. Publishes to
 * the tree's architect next: a genuine delivery failure (anything but "no live holder") restores
 * the captured phase and returns 502, so the worker retries and — since neither the phase nor the
 * claim moved — the retry is exactly idempotent. A missing architect holder never drops the
 * completion either: state (the source of truth) records it as `phases[issue].completed` instead
 * of clearing the phase, so `overseerCatchup` replays it on the architect's next catch-up and
 * `routeActive` treats the issue as having no active phase until the next assignment (a fresh
 * `spawn_worker` write) replaces this record. Either way, if the save itself fails, the
 * in-memory phase is restored before rethrowing (500) so a retry redoes the whole attempt. The
 * worker's role claim is never touched: the same agent stays claimed and resumes for its next
 * assignment. A missing holder is not a worker-facing failure: the response is 202 instead of the
 * normal 200, so a caller can tell delivery was uncertain without treating it as an error.
 */
export async function handlePhaseComplete(
  ctx: RouteContext,
  body: Record<string, unknown>
): Promise<Response> {
  const grant = ctx.auth.resolveGrant(body);
  const summary = requiredString(body, "summary");
  const tree = rootForIssue(ctx.deps.state, grant.issue);
  if (!tree) throw new HttpError(404, `No Legion tree contains issue ${grant.issue}`);
  const token = roleToken(ctx.deps.state.project, grant.issue, grant.role);
  const claim = ctx.deps.state.roles[token];
  if (!claim || !("issue" in claim) || claim.sessionId !== grant.sessionId) {
    throw new HttpError(409, "Grant does not match the worker currently holding this role");
  }
  const phase = ctx.deps.state.phases[grant.issue];
  if (!phase || phase.phase !== grant.role || phase.sessionId !== grant.sessionId) {
    // The issue's active phase has already moved on to a later worker (a retained but superseded
    // worker reporting a duplicate/late completion must never clear a newer phase it no longer
    // owns). This is distinct from a stale grant: the worker's own claim is fine.
    throw new HttpError(409, `Phase for ${grant.issue} is no longer owned by this worker`);
  }

  // Capture and clear the phase synchronously, before the publish await below.
  delete ctx.deps.state.phases[grant.issue];

  const architectToken = roleToken(ctx.deps.state.project, tree, "architect");
  let noHolder = false;
  try {
    await ctx.deps.envoyPublish(
      roleTopic(architectToken),
      JSON.stringify({
        type: "phase-complete",
        issue: grant.issue,
        role: grant.role,
        summary,
      })
    );
  } catch (error) {
    if (!(error instanceof EnvoyPublishError) || error.status !== 404) {
      ctx.deps.state.phases[grant.issue] = phase;
      throw new HttpError(
        502,
        `Envoy publish to the tree's architect failed: ${error instanceof Error ? error.message : String(error)}`
      );
    }
    noHolder = true;
    console.error(
      `[legion] phase-complete for ${grant.issue} (${grant.role}) has no live architect holder at ${architectToken}; recording it for catch-up: ${summary}`
    );
  }

  if (noHolder) {
    ctx.deps.state.phases[grant.issue] = {
      phase: phase.phase,
      sessionId: phase.sessionId,
      completed: { summary, at: new Date(ctx.now()).toISOString() },
    };
  }
  try {
    await ctx.save();
  } catch (error) {
    ctx.deps.state.phases[grant.issue] = phase;
    throw error;
  }
  return Response.json(
    validateContractResponse(LegionDaemonApi.PhaseComplete.response, {}),
    noHolder ? { status: 202 } : undefined
  );
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
