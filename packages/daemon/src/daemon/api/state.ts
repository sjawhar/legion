import { type DaemonStateResponse, parseRoleToken } from "@legion/contracts";
import type {
  ControllerRoleClaim,
  LegionState,
  RoleClaim,
  WorkerRoleClaim,
  WorkspaceLost,
} from "../legion-state";
import {
  type ExternalControllerLocator,
  isExternalControllerLocator,
  type Locator,
} from "../runtime";

function isWorkerRoleClaim(claim: RoleClaim): claim is WorkerRoleClaim {
  return "issue" in claim;
}

/** The projected shape of one locator: its runtime discriminant and the runtime's own
 * addressing fields, never a `socketPath`; `ompSessionFile` only for a locator the daemon
 * resumes — a tree's or the controller's (`withSession`), never a worker claim's. Exhaustive
 * over `Locator["runtime"]` — this is the one place outside the runtimes themselves that reads
 * runtime-specific fields, purely to redact them. */
function redactLocator<WithSession extends boolean>(
  locator: Locator,
  withSession: WithSession
): WithSession extends true
  ? NonNullable<DaemonStateResponse["trees"][string]["locator"]>
  : NonNullable<DaemonStateResponse["roles"][string]["locator"]> {
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

/** The operator-launched controller's record, field by field per this file's rule. Nothing about
 * it is secret — a session id and a timestamp — and the strict contract is the second gate. */
function redactExternalController(
  locator: ExternalControllerLocator
): NonNullable<DaemonStateResponse["controllerLocator"]> {
  return {
    runtime: locator.runtime,
    external: locator.external,
    sessionId: locator.sessionId,
    registeredAt: locator.registeredAt,
  };
}

/** Projects the non-secret provenance of a fresh session caused by a missing tree volume. */
function redactWorkspaceLost(workspaceLost: WorkspaceLost | undefined) {
  if (!workspaceLost) return undefined;
  return {
    at: workspaceLost.at,
    generation: workspaceLost.generation,
    fromRef: workspaceLost.fromRef,
    ...(workspaceLost.previousSessionId === undefined
      ? {}
      : { previousSessionId: workspaceLost.previousSessionId }),
  };
}

function redactRole(claim: RoleClaim): DaemonStateResponse["roles"][string] {
  if (!isWorkerRoleClaim(claim)) {
    const controllerClaim: ControllerRoleClaim = claim;
    return { role: controllerClaim.role, sessionId: controllerClaim.sessionId };
  }
  const workspaceLost = redactWorkspaceLost(claim.workspaceLost);
  return {
    role: claim.role,
    issue: claim.issue,
    generation: claim.generation,
    sessionId: claim.sessionId,
    readyConfirmedAt: claim.readyConfirmedAt,
    launchFailures: claim.launchFailures,
    ...(workspaceLost === undefined ? {} : { workspaceLost }),
    locator: claim.locator && redactLocator(claim.locator, false),
  };
}

/** The running-worker queue in FIFO order. Its token is the durable identity; an absent or stale
 * claim only omits the pending-task fields, and the task text is never projected. */
function queuedWorkers(state: LegionState): DaemonStateResponse["workerAdmission"]["queue"] {
  const entries: DaemonStateResponse["workerAdmission"]["queue"] = [];
  for (const token of state.workerAdmission.queue) {
    const parsed = parseRoleToken(state.project, token);
    if (parsed === undefined || "controller" in parsed) continue;
    const claim = state.roles[token];
    const pending =
      claim !== undefined && isWorkerRoleClaim(claim) ? claim.pendingAssignment : undefined;
    entries.push({
      roleToken: token,
      issue: parsed.issue,
      role: parsed.role,
      ...(pending === undefined ? {} : { kind: pending.kind, queuedAt: pending.queuedAt }),
    });
  }
  return entries;
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
    const workspaceLost = redactWorkspaceLost(tree.workspaceLost);
    trees[key] = {
      status: tree.status,
      generation: tree.generation,
      launchFailures: tree.launchFailures,
      readyConfirmedAt: tree.readyConfirmedAt,
      ...(workspaceLost === undefined ? {} : { workspaceLost }),
      locator: tree.locator && redactLocator(tree.locator, true),
    };
  }

  const gates: DaemonStateResponse["gates"] = {};
  for (const [key, gate] of Object.entries(state.gates)) {
    gates[key] = {
      artifactId: gate.artifactId,
      latestVersion: gate.latestVersion,
      ...(gate.approvedVersion === undefined ? {} : { approvedVersion: gate.approvedVersion }),
    };
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
    controllerLocator:
      state.controllerLocator === undefined
        ? undefined
        : isExternalControllerLocator(state.controllerLocator)
          ? redactExternalController(state.controllerLocator)
          : redactLocator(state.controllerLocator, true),
    roles,
    controllerPendingNotices: state.controllerPendingNotices.length,
    pendingStatusWrites: Object.keys(state.pendingStatusWrites),
    workerAdmission: { queue: queuedWorkers(state) },
  };
}
