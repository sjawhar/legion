import type { ActionType, IssueSource, IssueStateDict } from "../state/types";

interface ActionableIssue {
  issueId: string;
  action: ActionType;
  status: string;
  source: IssueSource | null;
}

interface BlockedIssue {
  issueId: string;
  reason: string;
  source: IssueSource | null;
}

interface CategorizedIssues {
  actionable: ActionableIssue[];
  blocked: BlockedIssue[];
  summary: Record<string, number>;
}

function categorizeIssues(issues: Record<string, IssueStateDict>): CategorizedIssues {
  const actionable: ActionableIssue[] = [];
  const blocked: BlockedIssue[] = [];
  const summary: Record<string, number> = {};

  for (const [issueId, issue] of Object.entries(issues)) {
    // Check blocking conditions FIRST — these take precedence over suggestedAction
    // because the state machine may assign actionable actions (e.g., remove_worker_active_and_redispatch)
    // to issues that should appear as blocked in the poll summary.
    if (issue.labels.includes("user-input-needed")) {
      blocked.push({ issueId, reason: "user-input-needed", source: issue.source });
      continue;
    }

    if (issue.labels.includes("worker-active") && !issue.hasLiveWorker) {
      blocked.push({ issueId, reason: "worker-active (stale)", source: issue.source });
      continue;
    }

    if (issue.isBlocked) {
      const blockerList = issue.blockedByIds?.join(", ") ?? "";
      const reason = blockerList ? `blocked by ${blockerList}` : "blocked";
      blocked.push({ issueId, reason, source: issue.source });
      continue;
    }

    // Non-blocked items: actionable if not skip, otherwise summary
    if (issue.suggestedAction !== "skip") {
      actionable.push({
        issueId,
        action: issue.suggestedAction,
        status: issue.status,
        source: issue.source,
      });
      continue;
    }

    // Non-actionable, non-blocked — count in summary
    summary[issue.status] = (summary[issue.status] ?? 0) + 1;
  }

  return { actionable, blocked, summary };
}

function issueDisplayId(issueId: string, source: IssueSource | null): string {
  if (source?.number) {
    return `#${source.number}`;
  }
  return issueId;
}

/**
 * Format the /state/materialized response into a compact, controller-friendly summary.
 *
 * Output sections (empty sections are omitted):
 * - ACTIONABLE: issues grouped by suggestedAction
 * - BLOCKED: skip issues with blocking labels
 * - SUMMARY: counts of remaining skip issues by status
 */
export function formatPollOutput(
  issues: Record<string, IssueStateDict>,
  titles: Record<string, string>
): string {
  const { actionable, blocked, summary } = categorizeIssues(issues);
  const lines: string[] = [];

  // ACTIONABLE section
  if (actionable.length > 0) {
    lines.push(`ACTIONABLE (${actionable.length}):`);

    // Group by action and sort for stable output
    const byAction = new Map<string, ActionableIssue[]>();
    for (const item of actionable) {
      const group = byAction.get(item.action) ?? [];
      group.push(item);
      byAction.set(item.action, group);
    }

    for (const [action, items] of [...byAction.entries()].sort((a, b) =>
      a[0].localeCompare(b[0])
    )) {
      lines.push(`  ${action}:`);
      for (const item of items) {
        const id = issueDisplayId(item.issueId, item.source);
        const title = titles[item.issueId];
        const titlePart = title ? `  "${title}"` : "";
        lines.push(`    ${id}  ${item.status}${titlePart}`);
      }
    }
  }

  // BLOCKED section
  if (blocked.length > 0) {
    if (lines.length > 0) lines.push("");
    lines.push(`BLOCKED (${blocked.length}):`);
    for (const item of blocked) {
      const id = issueDisplayId(item.issueId, item.source);
      const title = titles[item.issueId];
      const titlePart = title ? `  "${title}"` : "";
      lines.push(`  ${id}  ${item.reason}${titlePart}`);
    }
  }

  // SUMMARY section
  const summaryEntries = Object.entries(summary).sort((a, b) => a[0].localeCompare(b[0]));
  if (summaryEntries.length > 0) {
    if (lines.length > 0) lines.push("");
    const parts = summaryEntries.map(([status, count]) => `${status}: ${count}`);
    lines.push("SUMMARY:");
    lines.push(`  ${parts.join(" | ")}`);
  }

  return lines.join("\n");
}
