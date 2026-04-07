/**
 * Pipeline view formatting for `legion status`.
 *
 * Groups issues by phase and shows worker state, blocked items, and idle issues.
 * Supports both human-readable text and machine-readable JSON output.
 */

import type { IssueStatusLiteral } from "../state/types";

/**
 * Subset of IssueState fields needed for pipeline display.
 * Avoids importing the full IssueState type to keep the CLI layer decoupled.
 */
export interface PipelineIssue {
  title?: string;
  status: string;
  labels: string[];
  hasLiveWorker: boolean;
  workerMode: string | null;
  workerStatus: string | null;
  suggestedAction: string;
  isBlocked: boolean;
}

/**
 * Pipeline state: issues keyed by issue ID.
 */
export interface PipelineState {
  issues: Record<string, PipelineIssue>;
}

/**
 * Phase display order matching the issue lifecycle.
 */
const PHASE_ORDER: IssueStatusLiteral[] = [
  "Triage",
  "Icebox",
  "Backlog",
  "Todo",
  "In Progress",
  "Testing",
  "Needs Review",
  "Retro",
  "Done",
];

/**
 * Determine the display status indicator for an issue.
 *
 * Priority:
 *  1. BLOCKED (isBlocked === true)
 *  2. NEEDS INPUT (labels include user-input-needed)
 *  3. Worker mode:status (hasLiveWorker === true)
 *  4. IDLE (no live worker and non-skip suggested action)
 *  5. (empty) — nothing actionable
 */
export function getStatusIndicator(issue: PipelineIssue): string {
  if (issue.isBlocked) {
    return "BLOCKED";
  }
  if (issue.labels.includes("user-input-needed")) {
    return "NEEDS INPUT";
  }
  if (issue.hasLiveWorker && issue.workerMode && issue.workerStatus) {
    return `${issue.workerMode}:${issue.workerStatus}`;
  }
  if (!issue.hasLiveWorker && issue.suggestedAction !== "skip") {
    return "IDLE";
  }
  return "";
}

/**
 * Truncate a string to maxLen, appending "…" if truncated.
 */
function truncate(text: string, maxLen: number): string {
  if (text.length <= maxLen) {
    return text;
  }
  return `${text.slice(0, maxLen - 1)}…`;
}

/**
 * JSON output shape for --json flag.
 */
export interface PipelineJsonOutput {
  phases: Array<{
    name: string;
    issues: Array<{
      id: string;
      title: string;
      status: string;
    }>;
  }>;
  summary: {
    total: number;
    active: number;
    blocked: number;
    needsInput: number;
    idle: number;
  };
}

/**
 * Build structured JSON output for the pipeline view.
 */
export function buildPipelineJson(state: PipelineState): PipelineJsonOutput {
  // Group issues by phase
  const grouped = new Map<string, Array<{ id: string; title: string; status: string }>>();
  for (const phase of PHASE_ORDER) {
    grouped.set(phase, []);
  }

  let total = 0;
  let active = 0;
  let blocked = 0;
  let needsInput = 0;
  let idle = 0;

  for (const [issueId, issue] of Object.entries(state.issues)) {
    total++;
    const indicator = getStatusIndicator(issue);

    if (issue.isBlocked) blocked++;
    if (issue.labels.includes("user-input-needed")) needsInput++;
    if (issue.hasLiveWorker) active++;
    if (!issue.hasLiveWorker && issue.suggestedAction !== "skip") idle++;

    const phase = issue.status;
    const bucket = grouped.get(phase);
    if (bucket) {
      bucket.push({ id: issueId, title: issue.title ?? "", status: indicator });
    } else {
      // Unknown phase — create a bucket
      grouped.set(phase, [{ id: issueId, title: issue.title ?? "", status: indicator }]);
    }
  }

  const phases: PipelineJsonOutput["phases"] = [];
  for (const [name, issues] of grouped) {
    if (issues.length > 0) {
      phases.push({ name, issues });
    }
  }

  return {
    phases,
    summary: { total, active, blocked, needsInput, idle },
  };
}

/**
 * Format the pipeline view as human-readable text.
 */
export function formatPipelineView(state: PipelineState): string {
  const lines: string[] = [];

  // Group issues by phase
  const grouped = new Map<string, Array<[string, PipelineIssue]>>();
  for (const phase of PHASE_ORDER) {
    grouped.set(phase, []);
  }

  let total = 0;
  let active = 0;
  let blocked = 0;
  let needsInput = 0;
  let idle = 0;

  for (const [issueId, issue] of Object.entries(state.issues)) {
    total++;

    if (issue.isBlocked) blocked++;
    if (issue.labels.includes("user-input-needed")) needsInput++;
    if (issue.hasLiveWorker) active++;
    if (!issue.hasLiveWorker && issue.suggestedAction !== "skip") idle++;

    const phase = issue.status;
    const bucket = grouped.get(phase);
    if (bucket) {
      bucket.push([issueId, issue]);
    } else {
      grouped.set(phase, [[issueId, issue]]);
    }
  }

  // Render phases
  lines.push("\nPipeline");
  lines.push("-".repeat(40));

  for (const [phase, issues] of grouped) {
    if (issues.length === 0) continue;

    lines.push(`\n${phase} (${issues.length})`);
    for (const [issueId, issue] of issues) {
      const indicator = getStatusIndicator(issue);
      const title = truncate(issue.title ?? "", 50);
      const statusPart = indicator ? ` [${indicator}]` : "";
      lines.push(`  ${issueId} ${title}${statusPart}`);
    }
  }

  // Summary line
  lines.push("");
  lines.push(
    `Total: ${total} issues | Active: ${active} workers | Blocked: ${blocked} | Needs Input: ${needsInput} | Idle: ${idle}`
  );

  return lines.join("\n");
}
