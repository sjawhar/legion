import type { DaemonStateResponse } from "@legion/contracts";
import type { ControllerRoleClaim, LegionState, RoleClaim, WorkerRoleClaim } from "../legion-state";
import type { Locator } from "../runtime";

function isWorkerRoleClaim(claim: RoleClaim): claim is WorkerRoleClaim {
  return "issue" in claim;
}

/** The projected shape of one locator: its runtime discriminant and the runtime's own
 * addressing fields, never a `socketPath`; `ompSessionFile` only for a tree locator
 * (`withSession`). Exhaustive over `Locator["runtime"]` — this is the one place outside the
 * runtimes themselves that reads runtime-specific fields, purely to redact them. */
function redactLocator<WithSession extends boolean>(
  locator: Locator,
  withSession: WithSession
): WithSession extends true
  ? NonNullable<DaemonStateResponse["trees"][string]["locator"]>
  : NonNullable<DaemonStateResponse["controllerLocator"]> {
  const session = withSession ? { ompSessionFile: locator.ompSessionFile } : {};
  switch (locator.runtime) {
    case "tmux":
      return {
        runtime: locator.runtime,
        tmuxSession: locator.tmuxSession,
        tmuxWindowId: locator.tmuxWindowId,
        tmuxPaneId: locator.tmuxPaneId,
        ...session,
      };
    case "kubernetes":
      return {
        runtime: locator.runtime,
        namespace: locator.namespace,
        podName: locator.podName,
        podUid: locator.podUid,
        pvcName: locator.pvcName,
        ...session,
      };
  }
}

function redactRole(claim: RoleClaim): DaemonStateResponse["roles"][string] {
  if (!isWorkerRoleClaim(claim)) {
    const controllerClaim: ControllerRoleClaim = claim;
    return { role: controllerClaim.role, sessionId: controllerClaim.sessionId };
  }
  return {
    role: claim.role,
    issue: claim.issue,
    generation: claim.generation,
    sessionId: claim.sessionId,
    readyConfirmedAt: claim.readyConfirmedAt,
    launchFailures: claim.launchFailures,
    locator: claim.locator && redactLocator(claim.locator, false),
  };
}

/** Builds the redacted `GET /legion/v1/state` projection: an explicit field-by-field allowlist
 * (never a spread of `LegionState`, and never a blocklist of "known-bad" fields) so a new secret,
 * hash, or capability field added to `LegionState` in the future does not silently reach this
 * response by default — it must be deliberately added here. `LegionDaemonApi.State.response` is
 * `strictObject`-shaped at every level as a second, independent gate: `validateContractResponse`
 * throws instead of serving the response if this function ever forwards a field the schema does
 * not name. */
export function buildLegionStateResponse(state: LegionState): DaemonStateResponse {
  const issues: DaemonStateResponse["issues"] = {};
  for (const [key, issue] of Object.entries(state.issues)) {
    issues[key] = {
      key: issue.key,
      title: issue.title,
      status: issue.status,
      children: issue.children,
      parent: issue.parent,
      lastAppliedSeq: issue.lastAppliedSeq,
    };
  }

  const trees: DaemonStateResponse["trees"] = {};
  for (const [key, tree] of Object.entries(state.trees)) {
    trees[key] = {
      status: tree.status,
      generation: tree.generation,
      launchFailures: tree.launchFailures,
      readyConfirmedAt: tree.readyConfirmedAt,
      locator: tree.locator && redactLocator(tree.locator, true),
    };
  }

  const gates: DaemonStateResponse["gates"] = {};
  for (const [key, gate] of Object.entries(state.gates)) {
    gates[key] = { designAskId: gate.designAskId, designApproved: gate.designApproved };
  }

  const roles: DaemonStateResponse["roles"] = {};
  for (const [token, claim] of Object.entries(state.roles)) {
    roles[token] = redactRole(claim);
  }

  return {
    project: state.project,
    version: state.version,
    issues,
    trees,
    admission: {
      cap: state.admission.cap,
      active: [...state.admission.active],
      queue: [...state.admission.queue],
    },
    gates,
    controllerLocator: state.controllerLocator && redactLocator(state.controllerLocator, false),
    roles,
    controllerPendingNotices: state.controllerPendingNotices.length,
    pendingStatusWrites: Object.keys(state.pendingStatusWrites),
  };
}
