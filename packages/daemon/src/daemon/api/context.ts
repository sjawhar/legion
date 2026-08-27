import {
  type DispatchUrgency,
  type IssueKey,
  LEGION_ROLES,
  type LegionRole,
  roleToken,
} from "@legion/contracts";
import type { CommandRunner } from "../../state/fetch";
import type { LegionApiConfig, LegionApiDeps } from "../api";
import type { LegionState } from "../legion-state";
import type { CapabilityService, ControllerGate } from "./auth";
import type { DispatchThreadResult } from "./dispatch";
import type { GitHubService } from "./github";
import { HttpError, issueKey, legionRole } from "./http";

export function treeContains(state: LegionState, tree: IssueKey, candidate: IssueKey): boolean {
  const pending = [tree];
  const visited = new Set<IssueKey>();
  while (pending.length > 0) {
    const current = pending.pop();
    if (!current || visited.has(current)) {
      continue;
    }
    if (current === candidate) {
      return true;
    }
    visited.add(current);
    pending.push(...(state.issues[current]?.children ?? []));
  }
  return false;
}

export function matchingHeldEvent(state: LegionState, issue: IssueKey, role: string): boolean {
  const claim = state.roles[role];
  if (claim && "issue" in claim && claim.issue === issue) {
    return true;
  }
  return LEGION_ROLES.some(
    (candidateRole) => role === roleToken(state.project, issue, candidateRole)
  );
}

export function roleForSession(
  state: LegionState,
  issue: IssueKey,
  sessionId: string,
  phase: string
): LegionRole {
  const claim = Object.values(state.roles).find(
    (candidate) =>
      "issue" in candidate && candidate.issue === issue && candidate.sessionId === sessionId
  );
  if (!claim) {
    throw new HttpError(403, "Session is not registered for this issue");
  }
  const role = legionRole(claim.role);
  const declaredPhaseRole = LEGION_ROLES.find((candidate) => candidate === phase);
  if (declaredPhaseRole && declaredPhaseRole !== role) {
    throw new HttpError(403, "Session role does not match phase");
  }
  return role;
}

export function requireTree(state: LegionState, body: Record<string, unknown>): IssueKey {
  const tree = issueKey(body, "tree");
  if (!state.trees[tree] || !state.issues[tree]) {
    throw new HttpError(404, "Unknown tree");
  }
  return tree;
}

export function requireTreeIssue(
  state: LegionState,
  body: Record<string, unknown>
): { tree: IssueKey; issue: IssueKey } {
  const tree = requireTree(state, body);
  const issue = issueKey(body, "issue");
  if (!state.issues[issue]) {
    throw new HttpError(404, "Unknown issue");
  }
  if (!treeContains(state, tree, issue)) {
    throw new HttpError(403, "Issue is outside tree");
  }
  return { tree, issue };
}

export function appendFooter(
  state: LegionState,
  tree: IssueKey,
  issue: IssueKey,
  body: string
): string {
  const session = state.roles[roleToken(state.project, tree, "architect")]?.sessionId ?? "";
  return `${body}\n\n<!-- legion: ${JSON.stringify({ session, issue })} -->`;
}

/** The single object every Legion HTTP API route handler receives. */
export interface RouteContext {
  config: LegionApiConfig;
  deps: LegionApiDeps;
  now: () => number;
  save: () => Promise<void>;
  runner: CommandRunner;
  grantTtlMs: number;
  auth: CapabilityService;
  controllerGate: ControllerGate;
  github: GitHubService;
  dispatchThread: (
    parent: string,
    subject: string,
    body: string,
    ask: unknown,
    urgency: DispatchUrgency | undefined
  ) => Promise<DispatchThreadResult>;
  requireTree(body: Record<string, unknown>): IssueKey;
  requireTreeIssue(body: Record<string, unknown>): { tree: IssueKey; issue: IssueKey };
  appendFooter(tree: IssueKey, issue: IssueKey, body: string): string;
}
