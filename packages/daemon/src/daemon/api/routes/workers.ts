import { randomUUID } from "node:crypto";
import {
  type IssueKey,
  LegionDaemonApi,
  type LegionRole,
  roleToken,
  roleTopic,
} from "@legion/contracts";
import { writeStatus } from "../../dispatch-client";
import { appRoleForLegionRole } from "../../github-apps";
import {
  activePhaseLabel,
  type IssueStatus,
  isBystanderRole,
  type LegionState,
  type WorkerRoleClaim,
} from "../../legion-state";
import { sameProcess } from "../../runtime";
import { equalSecretHash, secretHash, spawnCapabilityKey } from "../auth";
import { type RouteContext, rootForIssue, treeContains } from "../context";
import {
  EnvoyPublishError,
  HttpError,
  legionRole,
  requiredNumber,
  requiredString,
  validateContractResponse,
} from "../http";

/** An issue's status as this daemon knows it. A lifecycle write of its own that has not landed on
 * Dispatch yet (`pendingStatusWrites`, recorded when the PATCH failed and retried by resync) wins
 * over the last status Dispatch echoed back through the durable lane — but only while that echoed
 * status is still the one the write was recorded against (`statusAtRecord`), resync's own
 * supersession fence (`retryPendingWrite` in `dispatch-client.ts`). So a Dispatch outage between
 * two daemon-owned transitions never makes the second one read a stale status, and a human's move
 * echoed after the failed write is never outranked by it. */
function knownIssueStatus(state: LegionState, issue: IssueKey): IssueStatus | undefined {
  const pending = state.pendingStatusWrites[issue];
  const echoed = state.issues[issue]?.status;
  return pending && echoed === pending.statusAtRecord ? pending.status : echoed;
}

/** Whether the `review` reducer has recorded changes requested on the issue's PR — from any
 * commit, while an approval is recorded only at the PR's current head. */
function changesRequested(state: LegionState, issue: IssueKey): boolean {
  return Object.values(state.prs).some(
    (pr) => pr.key === issue && pr.reviewDecision === "changes_requested"
  );
}

/** The status the daemon PATCHes off a phase's own completion, keyed by the role that just
 * finished. `planner`/`merger` completions never PATCH a status here: planning still reads as
 * `in_progress`, and a merge's `done` transition happens on `closeTree` instead. An implementer
 * completion advances `in_progress` → `testing` and nothing else: the same role also completes
 * the `.legion/` deletion push, a conflict-forced rebase, and retro, none of which is a test
 * round — from any other known status (or an issue whose status this daemon has not yet
 * observed) it writes nothing, so `retro` is never followed by `testing`. A reviewer completion
 * checks the review verdict already recorded on the issue's PR: changes requested returns the
 * issue to `in_progress` for a corrective implementer instead of advancing to `retro`. */
function phaseCompleteStatus(
  state: LegionState,
  issue: IssueKey,
  role: LegionRole
): IssueStatus | undefined {
  switch (role) {
    case "implementer":
      return knownIssueStatus(state, issue) === "in_progress" ? "testing" : undefined;
    case "tester":
      return "needs_review";
    case "reviewer":
      return changesRequested(state, issue) ? "in_progress" : "retro";
    default:
      return undefined;
  }
}

/** The status the daemon PATCHes when the architect spawns a worker: two cases, nothing else.
 *
 * (a) A `role: architect` spawn — a child's sub-architect; the root's own architect is refused 400
 * by `handleSpawnWorker` — while the known status is `todo` → `in_progress`. A child is never
 * admitted as a tree of its own (`reduceIssueUpdated` emits no `admit` for a `todo` under a live
 * ancestor tree), so this spawn is the child's admission and writes what `spawnTree` writes for a
 * root. A child a human moved back to `todo` gets `in_progress` again on its next sub-architect
 * spawn: for a child, that spawn *is* its admission. A second spawn for the same live
 * sub-architect writes nothing once Dispatch's echo lands (or, after a failed PATCH, at once,
 * through `knownIssueStatus`'s pending-write fence); one that lands between a successful PATCH
 * and its echo PATCHes `in_progress` again — an idempotent duplicate, the same window a root's
 * `spawnTree` write has.
 *
 * (b) An implementer spawned while the issue's PR carries `reviewDecision: "changes_requested"` (a
 * Legion reviewer's round or a human's review after approval) returns the issue to `in_progress`
 * from wherever the review left it, unless it is already there.
 *
 * No spawn starts a root. Every other spawn writes nothing — a `.legion/` deletion push or a retro
 * under an approved review never moves the status, and a phase worker spawned on an active issue a
 * human moved back to `todo` never overrides it — and the implementer's completion guard above
 * sees the status these writes put there. */
function spawnStatus(
  state: LegionState,
  issue: IssueKey,
  role: LegionRole
): IssueStatus | undefined {
  const known = knownIssueStatus(state, issue);
  if (role === "architect") return known === "todo" ? "in_progress" : undefined;
  return role === "implementer" && known !== "in_progress" && changesRequested(state, issue)
    ? "in_progress"
    : undefined;
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

/** Registers a booted worker's session: records its capability, locator (with the OMP session
 * file), and claim, and mints its session secret. Never writes the issue's active phase
 * (`state.phases[issue]`): the one place a new active phase is written is the delivery of an
 * architect assignment (`promptExistingWorker` with `kind: "assignment"` -- a task prompted into
 * a live worker, or delivered from the claim's `pendingAssignment` at `/worker/ready`;
 * `/phase/complete` only deletes, restores, or marks the record completed), so a relaunch of a
 * worker whose phase already finished registers as a bystander and the newer phase keeps its
 * completion route and its wakes. */
export async function handleWorkerStarted(
  ctx: RouteContext,
  body: Record<string, unknown>
): Promise<Response> {
  const { tree, issue } = ctx.requireTreeIssue(body);
  const role = legionRole(requiredString(body, "role"));
  const bootToken = requiredString(body, "bootToken");
  const sessionId = requiredString(body, "sessionId");
  const token = roleToken(ctx.deps.state.project, issue, role);
  const claim = ctx.deps.state.roles[token];
  if (!claim || !("issue" in claim) || !claim.locator) {
    throw new HttpError(409, "Stale worker generation");
  }
  // Captured now, re-checked against the current claim after the slow lease await below: the
  // boot watchdog (or a reconnect probe) races this exact confirmation and may retire this
  // generation's process while the GitHub request is in flight.
  const capturedGeneration = claim.generation;
  const capturedLocator = { ...claim.locator };
  const resolved = ctx.auth.resolveWorkerClaim(ctx.deps.state, bootToken);
  if (!resolved || resolved.token !== token || (resolved.boot && resolved.boot.tree !== tree)) {
    throw new HttpError(403, "Invalid worker boot token");
  }
  const boot = resolved.boot;
  if (boot) {
    if (boot.sessionId !== undefined && boot.sessionId !== sessionId) {
      throw new HttpError(403, "Invalid worker boot token");
    }
    if (claim.generation !== boot.generation) {
      throw new HttpError(409, "Stale worker generation");
    }
    if (boot.expectedSessionId !== undefined && boot.expectedSessionId !== sessionId) {
      throw new HttpError(409, "Worker respawn must resume the same agent session");
    }
  } else if (claim.expectedSessionId !== undefined && claim.expectedSessionId !== sessionId) {
    // The in-memory boot-token map is gone (the daemon restarted between this launch and
    // /worker/started): `resolveWorkerClaim` matched the hash `launchWorker` persisted onto the
    // claim at mint, and the same-agent check reads the durable counterpart of the in-memory
    // path's `expectedSessionId`.
    throw new HttpError(409, "Worker respawn must resume the same agent session");
  }
  const agentId = requiredString(body, "agentId");
  const ompSessionFile = requiredString(body, "ompSessionFile");
  // Fallible AND potentially slow work runs before any mutation, and — critically — outside the
  // per-token lock below: a hung or merely slow GitHub call must never hold that lock, since
  // `closeTree`'s own stop-then-delete for this exact token needs the SAME lock just to START
  // its stop timeout (`stopProcessSerialized` -> `workerAdmission.mutateClaim`). Holding it across
  // this network round trip would let a stuck `tokenForIssue` block a graceful shutdown from ever
  // timing out for this worker. A transient failure here leaves the boot token and claim
  // untouched, so a retry with the same {bootToken, sessionId} starts clean. The lock is
  // (re-)acquired below, after this await, to re-validate against whatever changed while it was
  // outstanding and commit atomically with that re-validation.
  const lease = await ctx.github.tokenForIssue(appRoleForLegionRole(role));
  const secret = randomUUID();

  // Everything from here on runs inside this token's own critical section (rejecting with
  // TreeClosingError, mapped to 409 by the caller's error handling, both before entering it and
  // again immediately inside): a `closeTree` racing this exact request must never have this
  // handler resurrect a claim it already deleted, or persist a locator for a tree it already
  // reported closed. The lock now spans only synchronous state plus one disk-persist await, never
  // the GitHub network round trip above.
  return ctx.deps.processManager.mutateLiveRoleClaim(tree, issue, token, async () => {
    // Re-read: the boot watchdog or a reconnect probe may have retired this exact boot (or a
    // newer launch may have replaced it) while the GitHub lease was in flight above — never
    // write over whatever now occupies this token.
    const current = ctx.deps.state.roles[token];
    if (
      !current ||
      !("issue" in current) ||
      !current.locator ||
      current.generation !== capturedGeneration ||
      !sameProcess(current.locator, capturedLocator)
    ) {
      throw new HttpError(409, "Stale worker generation");
    }
    // Build the new claim as a local draft rather than mutating the live one in place, so a
    // save failure can be rolled back by simply restoring the old reference — leaving the
    // in-memory claim exactly as durable as what was ever written to disk, and a retry with the
    // same {bootToken, sessionId} starting where the first attempt did.
    const priorClaim = current;
    const nextClaim: WorkerRoleClaim = {
      ...current,
      sessionId,
      agentId,
      bootTokenHash: secretHash(bootToken).toString("hex"),
      locator: { ...current.locator, ompSessionFile },
    };
    // launchFailures is deliberately untouched here: a mere registration is not a recovery
    // signal for launch accounting -- only a durably confirmed `/worker/ready` (`workerReady` in
    // processes.ts) resets it, so a worker that keeps registering but never reaches ready
    // confirmation still escalates to `worker-died` at the threshold instead of resetting every
    // generation.
    ctx.deps.state.roles[token] = nextClaim;
    try {
      await ctx.save();
    } catch (error) {
      ctx.deps.state.roles[token] = priorClaim;
      throw error;
    }
    // A fresh spawn or a resume carrying the architect's task is the normal path: its
    // assignment makes it the active phase at ready. A bystander role (`isBystanderRole`: a
    // phase worker that is not the active phase; an architect never is one) registering with
    // anything else -- a finished worker relaunched by the daemon's own recovery, or a
    // reconnect -- gets one line saying so.
    if (
      isBystanderRole(ctx.deps.state, issue, role) &&
      nextClaim.pendingAssignment?.kind !== "assignment"
    ) {
      console.info(
        `[legion] ${token} registered session ${sessionId} while ${issue}'s active phase is ${activePhaseLabel(ctx.deps.state, issue)}; the phase changes only when an architect assignment is delivered`
      );
    }

    // Only after the durable state is persisted do we consume the boot token and mint the
    // session capability: both are ephemeral (never part of `ctx.save()`'s payload), so the
    // save-failure rollback above needed no counterpart for them — they were never touched in
    // that case. `boot` is absent on the persisted-hash fallback path (the in-memory map never
    // had this token to begin with after a restart), so there is nothing to consume there.
    if (boot) boot.sessionId = sessionId;
    ctx.auth.setCapability(sessionId, { tree, issue, role, secretHash: secretHash(secret) });
    return Response.json(
      validateContractResponse(LegionDaemonApi.WorkerStarted.response, {
        roleToken: token,
        secret,
        gitName: lease.gitIdentity.name,
        gitEmail: lease.gitIdentity.email,
      })
    );
  });
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
  // Same deadlock shape as /process/ready (see handleProcessReady): the calling worker's own
  // bootstrap cannot answer a negotiate_protocol request until this response returns, so the
  // shim connect and the pending-prompt delivery it enables must happen after we respond, not
  // before. A connect/prompt failure leaves pendingAssignment queued exactly as it is today —
  // `ProcessManager.clientFor` never caches a failed connect, so the next touch (a probe, a
  // resume, another spawnWorker) retries it.
  Promise.resolve(ctx.deps.processManager.workerReady(issue, role, sessionId, generation)).catch(
    (error) => {
      console.error(
        `[legion] failed to deliver worker/ready assignment for ${issue}/${role}:`,
        error
      );
    }
  );
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

  const nextStatus = phaseCompleteStatus(ctx.deps.state, grant.issue, grant.role);
  if (nextStatus) {
    await writeStatus(ctx.deps.state, ctx.deps.dispatchClient, grant.issue, nextStatus);
  }

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
  // Written after the process manager accepted the spawn (a refused one moves nothing) and before
  // the result is returned, whether it was spawned, resumed, or queued: the worker's phase has
  // started from the architect's point of view either way.
  const nextStatus = spawnStatus(ctx.deps.state, issue, role);
  if (nextStatus) {
    await writeStatus(ctx.deps.state, ctx.deps.dispatchClient, issue, nextStatus);
    await ctx.save();
  }
  return Response.json(validateContractResponse(LegionDaemonApi.SpawnWorker.response, result));
}
