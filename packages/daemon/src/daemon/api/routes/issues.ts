import { LegionDaemonApi } from "@legion/contracts";
import { writeStatus } from "../../dispatch-client";
import type { IssueStatus } from "../../legion-state";
import { type RouteContext, treeContains } from "../context";
import {
  HttpError,
  issueKey,
  optionalStrings,
  requiredString,
  validateContractResponse,
} from "../http";

/**
 * Sets an issue's Dispatch status. Two mutually exclusive credentials, distinguished by which is
 * actually presented in the body (never by whether `tree` happens to be set alone):
 * - Controller capability (bare `secret`): may move `todo`/`backlog`/`icebox` on any issue in the
 *   project — the triage decision.
 * - Architect capability (`tree`+`sessionId` alongside `secret`): may set any status on an issue
 *   within its own tree.
 * A body presenting exactly one of `tree`/`sessionId` without the other is malformed and
 * rejected before either capability check runs.
 */
export async function handleIssueStatus(
  ctx: RouteContext,
  body: Record<string, unknown>
): Promise<Response> {
  const tree = body.tree;
  const sessionId = body.sessionId;
  const hasTree = tree !== undefined;
  const hasSessionId = sessionId !== undefined;
  if (hasTree !== hasSessionId) {
    throw new HttpError(400, "issues/status requires both tree and sessionId, or neither");
  }
  const issue = issueKey(body, "issue");
  const status = requiredString(body, "status");

  if (hasTree) {
    const architectTree = ctx.requireTree(body);
    ctx.auth.requireArchitectCapability(body, architectTree);
    if (!treeContains(ctx.deps.state, architectTree, issue)) {
      throw new HttpError(403, "Issue is outside tree");
    }
  } else {
    await ctx.auth.requireController(ctx.deps.state, body);
    if (status !== "todo" && status !== "backlog" && status !== "icebox") {
      throw new HttpError(403, "Controller capability may only set todo, backlog, or icebox");
    }
    if (!ctx.deps.state.issues[issue]) {
      throw new HttpError(404, "Unknown issue");
    }
  }
  await writeStatus(ctx.deps.state, ctx.deps.dispatchClient, issue, status as IssueStatus);
  await ctx.save();
  return Response.json(validateContractResponse(LegionDaemonApi.IssueStatus.response, {}));
}

/** Registers the design gate's ask id: the architect opens `dispatch_ask` on its root issue, then
 * records the resulting ask id here so `ask.answered` (`reducers.ts`'s `reduceAskAnswered`) knows
 * which answer approves the gate. */
export async function handleGatesRegister(
  ctx: RouteContext,
  body: Record<string, unknown>
): Promise<Response> {
  const tree = ctx.requireTree(body);
  ctx.auth.requireArchitectCapability(body, tree);
  const issue = issueKey(body, "issue");
  if (!treeContains(ctx.deps.state, tree, issue)) {
    throw new HttpError(403, "Issue is outside tree");
  }
  const askId = requiredString(body, "askId");
  ctx.deps.state.gates[issue] = { ...ctx.deps.state.gates[issue], designAskId: askId };
  await ctx.save();
  return Response.json(validateContractResponse(LegionDaemonApi.GatesRegister.response, {}));
}

export async function handleWaveRelease(
  ctx: RouteContext,
  body: Record<string, unknown>
): Promise<Response> {
  const tree = ctx.requireTree(body);
  ctx.auth.requireArchitectCapability(body, tree);
  const children = optionalStrings(body, "children").map((child) => issueKey({ child }, "child"));
  for (const child of children) {
    if (!treeContains(ctx.deps.state, tree, child)) {
      throw new HttpError(403, "Issue is outside tree");
    }
  }
  if (!ctx.deps.state.trees[tree]) {
    throw new HttpError(404, "Unknown tree");
  }
  // The PATCH itself is all this route does: the resulting `issue.updated` event (Dispatch echoes
  // every status write back through the daemon's own durable consumer) is what actually admits
  // each child, exactly like a human moving a child to `todo` in the dashboard would.
  for (const child of children) {
    await writeStatus(ctx.deps.state, ctx.deps.dispatchClient, child, "todo");
  }
  await ctx.save();
  return Response.json(
    validateContractResponse(LegionDaemonApi.WaveRelease.response, {
      released: children,
    })
  );
}

export async function handleEscalate(
  ctx: RouteContext,
  body: Record<string, unknown>
): Promise<Response> {
  const tree = ctx.requireTree(body);
  ctx.auth.requireArchitectCapability(body, tree);
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
  await ctx.deps.onControllerEvent(controllerEvent);
  return Response.json(validateContractResponse(LegionDaemonApi.Escalate.response, {}));
}
