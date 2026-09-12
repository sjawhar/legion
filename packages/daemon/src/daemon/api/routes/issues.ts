import { type IssueKey, LegionDaemonApi, roleTopic } from "@legion/contracts";
import { type DispatchClient, DispatchHttpError, writeStatus } from "../../dispatch-client";
import type { IssueStatus, LegionState } from "../../legion-state";
import { type EnvelopeJson, routeActive } from "../../reducers";
import { type RouteContext, treeContains } from "../context";
import {
  EnvoyPublishError,
  HttpError,
  issueKey,
  optionalStrings,
  requiredString,
  validateContractResponse,
} from "../http";

/** The `designApproved` marker a gate carries when the deployment's design gate is `off`: the
 * daemon, not a human, satisfied it. Distinguishable from an ask id in state dumps and tests. */
export const GATE_OFF_APPROVAL = "gate-off";

/** The resolution a gate-off ask carries on Dispatch: what the dashboard shows in place of an
 * answer, written for the human who would otherwise have been asked. */
export const GATE_OFF_ASK_REASON =
  "The design gate is off for this deployment (gates.design: off): the Legion daemon approved it " +
  "when the architect registered it, so no human answer is needed. Closed by the daemon so it does " +
  "not wait in anyone's inbox.";

/** `routeActive` takes the triggering envelope only to keep one signature with the reducers; the
 * gate-off wake has no Dispatch event behind it. */
const GATE_OFF_ENVELOPE: EnvelopeJson = { event_id: "gate-off", issued_at: 0 };

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

/** Publishes the `design-approved` wake for a gate the daemon satisfied itself (`gates.design:
 * off`), to exactly the role the reducer's `ask.answered` path would have chosen (`routeActive`:
 * the issue's active phase worker if any, else its tree's architect). Best-effort: the state
 * already carries the approval, so a resumed architect's catch-up shows it; a 404 no-holder is
 * silent, anything else is logged. */
async function publishDesignApproved(
  state: LegionState,
  issue: IssueKey,
  envoyPublish: (topic: string, payloadJson: string) => Promise<void>
): Promise<void> {
  for (const effect of routeActive(state, issue, { type: "design-approved" }, GATE_OFF_ENVELOPE)) {
    if (effect.kind !== "publish") continue;
    try {
      await envoyPublish(roleTopic(effect.role), JSON.stringify(effect.payload));
    } catch (error) {
      if (!(error instanceof EnvoyPublishError) || error.status !== 404) {
        console.error(
          `[legion] design gate is off but the design-approved wake for ${issue} failed to publish; the architect's catch-up carries the approval: ${error instanceof Error ? error.message : String(error)}`
        );
      }
    }
  }
}

/** Closes the architect's design-gate ask on Dispatch once the daemon has satisfied the gate, so a
 * question nobody needs to answer never sits in a human's inbox. Best-effort like the wake: state
 * already carries the approval. A 409 means someone closed it first (answered by hand, or a
 * previous attempt landed) and is silent; anything else is logged, never retried. */
async function resolveGateOffAsk(
  dispatchClient: Pick<DispatchClient, "resolveAsk">,
  issue: IssueKey,
  askId: string
): Promise<void> {
  try {
    await dispatchClient.resolveAsk(askId, GATE_OFF_ASK_REASON);
  } catch (error) {
    if (error instanceof DispatchHttpError && error.status === 409) return;
    console.error(
      `[legion] design gate is off but the design-gate ask ${askId} for ${issue} could not be closed on Dispatch; it stays open as a record: ${error instanceof Error ? error.message : String(error)}`
    );
  }
}

/** Everything the daemon does after marking a gate `designApproved: GATE_OFF_APPROVAL` and saving:
 * wake the architect as a human answer would, and close the ask it opened. Shared by the register
 * route and the boot fixup so the two paths cannot drift. */
export async function satisfyGateOff(
  state: LegionState,
  issue: IssueKey,
  askId: string,
  deps: {
    envoyPublish: (topic: string, payloadJson: string) => Promise<void>;
    dispatchClient: Pick<DispatchClient, "resolveAsk">;
  }
): Promise<void> {
  await publishDesignApproved(state, issue, deps.envoyPublish);
  await resolveGateOffAsk(deps.dispatchClient, issue, askId);
}

/** Registers the design gate's ask id: the architect opens `dispatch_ask` on its root issue, then
 * records the resulting ask id here so `ask.answered` (`reducers.ts`'s `reduceAskAnswered`) knows
 * which answer approves the gate.
 *
 * With `gates.design: off` the daemon satisfies the gate itself: the register records
 * `designApproved: GATE_OFF_APPROVAL`, publishes the same `design-approved` wake a human answer
 * would, and closes the ask on Dispatch (`resolved`, `GATE_OFF_ASK_REASON`) so the architect
 * proceeds without anyone clicking and nobody is left a question to answer. Both side effects are
 * best-effort: state is already approved, so a resumed architect's catch-up shows it, and a
 * still-open ask answered later is a no-op (`reduceAskAnswered` skips an approved gate). */
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
  const gateOff = ctx.config.gates.design === "off";
  const prior = ctx.deps.state.gates[issue];
  ctx.deps.state.gates[issue] = {
    ...prior,
    designAskId: askId,
    ...(gateOff && prior?.designApproved === undefined
      ? { designApproved: GATE_OFF_APPROVAL }
      : {}),
  };
  await ctx.save();
  if (gateOff && prior?.designApproved === undefined) {
    await satisfyGateOff(ctx.deps.state, issue, askId, ctx.deps);
  }
  return Response.json(validateContractResponse(LegionDaemonApi.GatesRegister.response, {}));
}

export async function handleWaveRelease(
  ctx: RouteContext,
  body: Record<string, unknown>
): Promise<Response> {
  const tree = ctx.requireTree(body);
  ctx.auth.requireArchitectCapability(body, tree);
  const issues = optionalStrings(body, "issues").map((issue) => issueKey({ issue }, "issue"));
  for (const issue of issues) {
    if (!treeContains(ctx.deps.state, tree, issue)) {
      throw new HttpError(403, "Issue is outside tree");
    }
  }
  if (!ctx.deps.state.trees[tree]) {
    throw new HttpError(404, "Unknown tree");
  }
  // The PATCH itself is all this route does: the resulting `issue.updated` event (Dispatch echoes
  // every status write back through the daemon's own durable consumer) is what actually admits
  // each child, exactly like a human moving a child to `todo` in the dashboard would.
  for (const issue of issues) {
    await writeStatus(ctx.deps.state, ctx.deps.dispatchClient, issue, "todo");
  }
  await ctx.save();
  return Response.json(
    validateContractResponse(LegionDaemonApi.WaveRelease.response, {
      released: issues,
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
