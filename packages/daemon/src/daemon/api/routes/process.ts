import { randomUUID } from "node:crypto";
import { LegionDaemonApi, type LegionRole, roleToken, sanitizeToken } from "@legion/contracts";
import { secretHash } from "../auth";
import type { RouteContext } from "../context";
import {
  HttpError,
  requiredNumber,
  requiredString,
  SAME_AGENT_REFUSAL,
  validateContractResponse,
} from "../http";

export async function handleProcessStarted(
  ctx: RouteContext,
  body: Record<string, unknown>
): Promise<Response> {
  const tree = ctx.requireTree(body);
  const generation = requiredNumber(body, "generation");
  const treeState = ctx.deps.state.trees[tree];
  if (!treeState || treeState.generation !== generation) {
    throw new HttpError(409, "Stale process generation");
  }
  const bootToken = body.bootToken;
  if (typeof bootToken !== "string" || bootToken.length === 0) {
    throw new HttpError(403, "Invalid root boot token");
  }
  const boot = ctx.auth.getBootToken(bootToken);
  if (!boot || boot.tree !== tree || boot.generation !== generation || boot.sessionId) {
    throw new HttpError(403, "Invalid root boot token");
  }
  // A lingering or closed tree's root may not register (LEGION-105): refused before the token is
  // consumed or anything is written, so the refusal changes nothing and the same token registers
  // once the tree is active again. The extension exits on this 409 (`exitOnRegistrationRefusal`).
  if (treeState.status !== "active") {
    throw new HttpError(
      409,
      `Legion tree ${tree} is ${treeState.status}; a root registers only for an active tree`
    );
  }
  const rootSessionId = requiredString(body, "rootSessionId");
  // The same-agent rule `/worker/started` applies through `WorkerBootToken.expectedSessionId`: a
  // resurrection minted its token with the recorded architect session, and a different one is a
  // fresh agent (under postgres, Oh My Pi resuming a missing row) that must not take the tree.
  // Checked before the token is consumed or anything written, so the refusal changes nothing.
  if (boot.expectedSessionId !== undefined && boot.expectedSessionId !== rootSessionId) {
    throw new HttpError(409, SAME_AGENT_REFUSAL);
  }
  boot.sessionId = rootSessionId;
  const agentId = requiredString(body, "agentId");
  const ompSessionFile = requiredString(body, "ompSessionFile");
  const pluginVersion = requiredString(body, "pluginVersion");
  if (!treeState.locator) {
    throw new Error(`Tree ${tree} is missing its process locator`);
  }
  // A close racing this exact boot must never have this handler resurrect a tree it already
  // reported closed, or register an architect claim `closeTreeLocked`'s own cleanup has already
  // passed over (a teardown in flight on a tree whose status has not flipped yet). No lock to
  // acquire here (unlike `/worker/started`): every check below through the write is
  // synchronous, so there is no awaited gap this check could go stale across.
  ctx.deps.processManager.rejectIfTreeGone(tree, tree);
  treeState.locator = { ...treeState.locator, ompSessionFile, pluginVersion };
  const roles: Record<LegionRole, string> = {
    architect: roleToken(ctx.deps.state.project, tree, "architect"),
    planner: roleToken(ctx.deps.state.project, tree, "planner"),
    implementer: roleToken(ctx.deps.state.project, tree, "implementer"),
    tester: roleToken(ctx.deps.state.project, tree, "tester"),
    reviewer: roleToken(ctx.deps.state.project, tree, "reviewer"),
    merger: roleToken(ctx.deps.state.project, tree, "merger"),
  };
  ctx.deps.state.roles[roles.architect] = {
    issue: tree,
    role: "architect",
    sessionId: rootSessionId,
    agentId,
  };
  const secret = randomUUID();
  ctx.auth.setCapability(rootSessionId, {
    tree,
    issue: tree,
    role: "architect",
    secretHash: secretHash(secret),
  });
  await ctx.save();
  return Response.json(
    validateContractResponse(LegionDaemonApi.ProcessStarted.response, {
      roleTokens: roles,
      controlSubject: `legion.ctl.${sanitizeToken(tree)}.${generation}`,
      gates: ctx.config.gates,
      secret,
    })
  );
}

export async function handleProcessReady(
  ctx: RouteContext,
  body: Record<string, unknown>
): Promise<Response> {
  const tree = ctx.requireTree(body);
  ctx.auth.requireArchitectCapability(body, tree);
  const generation = requiredNumber(body, "generation");
  const treeState = ctx.deps.state.trees[tree];
  if (!treeState || treeState.generation !== generation) {
    throw new HttpError(409, "Stale process generation");
  }
  // A lingering or closed tree's root confirms nothing (LEGION-105): no `readyConfirmedAt`, no
  // overseer catch-up, no shim connect. The extension exits on a boot-time 409; a
  // heartbeat-regain 409 is logged there and the linger sweep retires the process.
  if (treeState.status !== "active") {
    throw new HttpError(
      409,
      `Legion tree ${tree} is ${treeState.status}; only an active tree's root confirms ready`
    );
  }
  ctx.deps.processManager.confirmRootReady(tree, generation);
  await ctx.save();
  await ctx.deps.onTreeReady?.(tree);
  // The root architect's own bootstrap is itself blocked awaiting this HTTP response — it
  // cannot start its RPC dispatcher, and therefore cannot answer a `negotiate_protocol`
  // request, until this call returns. Awaiting the shim connect here deadlocks forever under
  // load (the daemon negotiates with an RPC loop that can never answer before this response
  // returns). Record readiness and respond immediately instead; the connect happens in the
  // background. `markTreeReady` retries one bounded backoff cycle itself (LEGION-39) and never
  // rejects for a connect failure — exhaustion logs and stops, nothing is retired — so it settles
  // only when that cycle ends and is never awaited here; this `.catch` guards only what escapes
  // (`requireTree`'s throw for an unknown tree).
  Promise.resolve(ctx.deps.processManager.markTreeReady(tree)).catch((error) => {
    console.error(`[legion] failed to connect architect shim socket on ready for ${tree}:`, error);
  });
  return Response.json(validateContractResponse(LegionDaemonApi.ProcessReady.response, {}));
}

export async function handleProcessExit(
  ctx: RouteContext,
  body: Record<string, unknown>
): Promise<Response> {
  const tree = ctx.requireTree(body);
  ctx.auth.requireArchitectCapability(body, tree);
  const generation = requiredNumber(body, "generation");
  const treeState = ctx.deps.state.trees[tree];
  if (!treeState || treeState.generation !== generation) {
    throw new HttpError(409, "Stale process generation");
  }
  const finished =
    treeState.status === "lingering" ||
    treeState.status === "closed" ||
    ctx.deps.state.issues[tree]?.status === "done";
  if (finished) {
    // Never await/join a teardown here: the caller of this route IS the tree's own root
    // process, currently blocked on this very HTTP response inside its `session_shutdown`
    // hook. If a teardown for this tree is already in flight (the common case — the linger
    // retire or the sweep's `closeTree` gracefully asked this exact root to exit, which is why
    // this request exists at all), awaiting it here would deadlock: that teardown cannot finish
    // until the root exits, and the root cannot finish exiting until this response returns. See
    // `reportRootExit`'s doc comment. A lingering tree's root — stopped by the linger retire, or
    // exiting on its own — records its exit here whatever the issue's status (a `backlog` or
    // `icebox` linger included) and never enters the dead path: `markProcessDead` would mark a
    // finished tree `dead` and hand it to resurrection (LEGION-105).
    await ctx.deps.processManager.reportRootExit(tree);
  } else {
    await ctx.deps.processManager.markProcessDead(tree);
  }
  await ctx.save();
  return Response.json(validateContractResponse(LegionDaemonApi.ProcessExit.response, {}));
}
