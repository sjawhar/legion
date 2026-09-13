import { type IssueDetails, type IssueKey, LegionDaemonApi, roleTopic } from "@legion/contracts";
import { writeStatus } from "../../dispatch-client";
import {
  type DesignGate,
  designGateOpen,
  type IssueStatus,
  type LegionState,
} from "../../legion-state";
import { type Effect, type EnvelopeJson, routeActive } from "../../reducers";
import { type RouteContext, treeContains } from "../context";
import {
  EnvoyPublishError,
  HttpError,
  issueKey,
  optionalStrings,
  requiredNumber,
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

/** Publishes the `publish` effects of a wake the daemon itself originated (no Dispatch event
 * behind it: the design gate's self-approval, the boot repair's `child-adopted`) to exactly the
 * roles the reducers would have chosen. Best-effort: the state already carries what the wake
 * announces, so a resumed architect's catch-up shows it. A 404 no-holder is silent; anything else
 * is logged with the line `describeFailure` builds from the error's message. Any other effect kind
 * is ignored -- `routeActive` yields a `controller` wake only for a closed tree, which no caller
 * targets. */
export async function publishWakeEffects(
  effects: Effect[],
  envoyPublish: (topic: string, payloadJson: string) => Promise<void>,
  describeFailure: (message: string) => string
): Promise<void> {
  for (const effect of effects) {
    if (effect.kind !== "publish") continue;
    try {
      await envoyPublish(roleTopic(effect.role), JSON.stringify(effect.payload));
    } catch (error) {
      if (!(error instanceof EnvoyPublishError) || error.status !== 404) {
        console.error(describeFailure(error instanceof Error ? error.message : String(error)));
      }
    }
  }
}

/** Publishes the `design-approved` wake for a gate the daemon opened without an `artifact.approved`
 * event of its own to reduce — the `gates.design: off` self-approval, and a registration that
 * found the document already approved on Dispatch (`seedGateFromDispatch`) — to exactly the role
 * the reducer's `artifact.approved` path would have chosen (`routeActive`: the issue's active
 * phase worker if any, else its tree's architect). Shared by the register route and the boot
 * fixup; `publishWakeEffects` carries the best-effort/404-silent rule. */
export async function publishDesignApproved(
  state: LegionState,
  issue: IssueKey,
  envoyPublish: (topic: string, payloadJson: string) => Promise<void>
): Promise<void> {
  await publishWakeEffects(
    routeActive(state, issue, { type: "design-approved" }, GATE_OFF_ENVELOPE),
    envoyPublish,
    (message) =>
      `[legion] the design-approved wake for ${issue} failed to publish; the architect's catch-up carries the approval: ${message}`
  );
}

/** A registration that would leave the gate closed asks Dispatch whether a human already approved
 * the document. The daemon ignores an `artifact.approved` for a document no gate names yet, and
 * Dispatch never re-emits it (`dispatch_request_approval` on an approved document answers
 * "already approved" and opens nothing), so an approval that landed before this call — from the
 * document header while the architect was still writing, or from the Inbox between the request
 * and this call — would otherwise park the architect forever. One `getIssue` read (the same read
 * `specArtifactResolver` uses for the migration) seeds the gate from the document's `approval`:
 * `approved` at the latest version records that version and opens the gate; `stale` records the
 * older approved version, so the gate stays closed until the current version is approved; every
 * other state records no approval. Dispatch's `latest_version` raises `latestVersion` in every
 * case — a version the architect did not see is still the current one. Humans still write every
 * approval; this only reads what one already wrote. A failed read is a 502 recording nothing, so
 * the architect's `register_gate` retries rather than registering a gate that silently cannot
 * open; a document the issue does not carry is a 404 naming both. */
async function seedGateFromDispatch(
  ctx: RouteContext,
  issue: IssueKey,
  gate: DesignGate
): Promise<void> {
  let details: IssueDetails;
  try {
    details = await ctx.deps.dispatchClient.getIssue(issue);
  } catch (error) {
    throw new HttpError(
      502,
      `Dispatch read of ${issue} failed; retry register_gate: ${error instanceof Error ? error.message : String(error)}`
    );
  }
  const document = details.artifacts.find((artifact) => artifact.id === gate.artifactId);
  if (!document) {
    throw new HttpError(404, `${gate.artifactId} is not a document of ${issue}`);
  }
  const approval = document.approval;
  if (!approval) return;
  gate.latestVersion = Math.max(gate.latestVersion, approval.latest_version);
  if (
    approval.version !== undefined &&
    (approval.state === "stale" ||
      (approval.state === "approved" && approval.version === approval.latest_version))
  ) {
    gate.approvedVersion = approval.version;
  }
}

/** Registers the design gate's document: the architect requests approval of its root spec with
 * `dispatch_request_approval`, then records the returned document id (`artifactId`, normalized
 * to lowercase — Dispatch emits its UUIDs lowercase and the reducers match them with `===`) and
 * version here so the `artifact.approved`/`artifact.changes_requested`/`artifact.version`
 * reducers know which document is the gate and which version is current. Re-registering the
 * same document keeps any approval already recorded and only raises `latestVersion`; registering
 * a different document replaces the gate wholesale, since an approval pins one document's version
 * and cannot carry to another.
 *
 * A gate that would be closed after this write is opened without waiting for an event in two
 * cases, each publishing the same `design-approved` wake a human approval would produce (the
 * publish is best-effort: state is already approved, so a resumed architect's catch-up shows it):
 * with `gates.design: off` the daemon approves the gate at its `latestVersion` itself, so an
 * architect that registered anyway proceeds without anyone clicking; otherwise the daemon reads
 * the document's approval from Dispatch (`seedGateFromDispatch`) and opens the gate when a human
 * already approved the current version. The contract (`LegionDaemonApi.GatesRegister.request`,
 * validated before this handler) already rejects a non-positive or non-integer `version`. */
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
  const artifactId = requiredString(body, "artifactId").toLowerCase();
  const version = requiredNumber(body, "version");
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
  let opened = false;
  if (!designGateOpen(gate)) {
    if (ctx.config.gates.design === "off") {
      gate.approvedVersion = gate.latestVersion;
    } else {
      await seedGateFromDispatch(ctx, issue, gate);
    }
    opened = designGateOpen(gate);
  }
  ctx.deps.state.gates[issue] = gate;
  await ctx.save();
  if (opened) {
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
  // The PATCH itself is all this route does. The resulting `issue.updated` event (Dispatch echoes
  // every status write back through the daemon's own durable consumer) admits nothing for a child
  // under this live tree (`reduceIssueUpdated` -> `admitOnTodo`): it wakes this architect through
  // the parent's own `child.status` event, and the architect's `spawn_worker` for the child's
  // sub-architect is what starts the child (writing its `in_progress`, `spawnStatus`). Exactly
  // like a human moving a child to `todo` in the dashboard would.
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
