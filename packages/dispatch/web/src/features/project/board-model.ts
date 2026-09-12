import type { IssueSummary } from "../../api/types";

export const issueStatuses = [
  "triage",
  "icebox",
  "backlog",
  "todo",
  "in_progress",
  "testing",
  "needs_review",
  "retro",
  "done",
] as const;

export type IssueStatus = (typeof issueStatuses)[number];

export function isHumanSettableStatus(status: IssueStatus): boolean {
  return status === "triage" || status === "icebox" || status === "backlog" || status === "todo";
}

export interface BoardColumn {
  readonly status: IssueStatus;
  readonly issues: IssueSummary[];
}

export function groupIssuesByStatus(issues: readonly IssueSummary[]): BoardColumn[] {
  return issueStatuses.map((status) => ({
    status,
    issues: issues.filter((issue) => issue.status === status),
  }));
}

export function rankInputForInsertion(
  issues: readonly IssueSummary[],
  insertionIndex: number
): { before?: string; after?: string } {
  const before = issues[insertionIndex];
  const after = issues[insertionIndex - 1];
  return {
    ...(after === undefined ? {} : { after: after.key }),
    ...(before === undefined ? {} : { before: before.key }),
  };
}
