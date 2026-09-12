import { type IssueKey, LegionDaemonApi, roleTopic } from "@legion/contracts";
import { writeStatus } from "../../dispatch-client";
import {
  type DesignGate,
  designGateOpen,
  type IssueStatus,
  type LegionState,
} from "../../legion-state";
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
 * off`), to exactly the role the reducer's `artifact.approved` path would have chosen
 * (`routeActive`: the issue's active phase worker if any, else its tree's architect).
 * Best-effort: the state already carries the approval, so a resumed architect's catch-up shows
 * it; a 404 no-holder is silent, anything else is logged. Shared by the register route and the
 * boot fixup. */
export async function publishDesignApproved(
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

/** Registers the design gate's document: the architect requests approval of its root spec with
 * `dispatch_request_approval`, then records the returned document id (`artifactId`) and version
 * here so the `artifact.approved`/`artifact.changes_requested`/`artifact.version` reducers know
 * which document is the gate and which version is current. Re-registering the same document
 * keeps any approval already recorded and only raises `latestVersion`; registering a different
 * document replaces the gate wholesale, since an approval pins one document's version and cannot
 * carry to another.
 *
 * With `gates.design: off` the daemon satisfies the gate itself: a gate that is not open after
 * this write is approved at its `latestVersion` and the same `design-approved` wake a human
 * approval would produce is published, so an architect that registered anyway proceeds without
 * anyone clicking. The publish is best-effort: state is already approved, so a resumed
 * architect's catch-up shows it. */
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
  const artifactId = requiredString(body, "artifactId");
  const version = body.version;
  if (typeof version !== "number" || !Number.isSafeInteger(version) || version <= 0) {
    throw new HttpError(400, "Expected positive integer version");
  }
  const prior = ctx.deps.state.gates[issue];
  const gate: DesignGate =
    prior && prior.artifactId === artifactId
      ? {
          artifactId,
          latestVersion: Math.max(prior.latestVersion, version),
          ...(prior.approvedVersion === undefined
            ? {}
            : { approvedVersion: prior.approvedVersion }),
        }
      : { artifactId, latestVersion: version };
  const gateOffApproves = ctx.config.gates.design === "off" && !designGateOpen(gate);
  if (gateOffApproves) gate.approvedVersion = gate.latestVersion;
  ctx.deps.state.gates[issue] = gate;
  await ctx.save();
  if (gateOffApproves) {
    await publishDesignApproved(ctx.deps.state, issue, ctx.deps.envoyPublish);
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
