import { type IssueKey, roleToken } from "@legion/contracts";
import type { CommandRunner } from "../../state/fetch";
import type { LegionApiConfig, LegionApiDeps } from "../api";
import type { LegionState } from "../legion-state";
import type { CapabilityService } from "./auth";
import type { GitHubService } from "./github";
import { HttpError, issueKey } from "./http";

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

/** Walks `issue`'s parent chain to the tree root it belongs to, if any. */
export function rootForIssue(state: LegionState, issue: IssueKey): IssueKey | undefined {
  if (state.trees[issue]) return issue;
  const seen = new Set<IssueKey>();
  let current: IssueKey | undefined = issue;
  while (current && !seen.has(current)) {
    if (state.trees[current]) return current;
    seen.add(current);
    current = state.issues[current]?.parent;
  }
  return undefined;
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
  github: GitHubService;
  requireTree(body: Record<string, unknown>): IssueKey;
  requireTreeIssue(body: Record<string, unknown>): { tree: IssueKey; issue: IssueKey };
  appendFooter(tree: IssueKey, issue: IssueKey, body: string): string;
}
