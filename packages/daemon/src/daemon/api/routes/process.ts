import { randomUUID } from "node:crypto";
import { LegionDaemonApi, type LegionRole, roleToken, sanitizeToken } from "@legion/contracts";
import { secretHash } from "../auth";
import type { RouteContext } from "../context";
import { HttpError, requiredNumber, requiredString, validateContractResponse } from "../http";

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
  const rootSessionId = requiredString(body, "rootSessionId");
  boot.sessionId = rootSessionId;
  const agentId = requiredString(body, "agentId");
  const ompSessionFile = requiredString(body, "ompSessionFile");
  if (!treeState.locator) {
    throw new Error(`Tree ${tree} is missing its process locator`);
  }
  treeState.status = "active";
  treeState.locator = { ...treeState.locator, ompSessionFile };
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
  await ctx.deps.onTreeReady?.(tree);
  await ctx.deps.processManager.markTreeReady(tree);
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
  if (ctx.deps.state.issues[tree]?.state === "closed") {
    await ctx.deps.processManager.closeTree(tree);
  } else {
    await ctx.deps.processManager.markProcessDead(tree);
  }
  await ctx.save();
  return Response.json(validateContractResponse(LegionDaemonApi.ProcessExit.response, {}));
}
