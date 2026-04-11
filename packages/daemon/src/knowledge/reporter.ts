import type { ClassifiedLearningAggregate } from "./types";

export interface ReportIndexMutation {
  action: string;
  detail?: string;
  key: string;
  learningPath: string;
}

export interface ReportStatusMutation {
  action: string;
  detail?: string;
  learningPath: string;
  status?: string;
}

export interface ConsolidationReportLike {
  apply: boolean;
  indexMutations: ReportIndexMutation[];
  learnings: Array<Pick<ClassifiedLearningAggregate, "disposition" | "notes" | "path">>;
  legionId: string;
  logPath: string;
  statusMutations: ReportStatusMutation[];
  warnings: string[];
  workspaceRoot: string;
}

function dispositionLabel(disposition: string): string {
  switch (disposition) {
    case "accepted":
    case "promote":
      return "PROMOTE";
    case "needs_review":
    case "review":
      return "REVIEW";
    case "archived":
    case "stale":
      return "STALE";
    case "rejected":
    case "keep":
      return "KEEP";
    default:
      return disposition.toUpperCase();
  }
}

export function formatConsolidationReportHuman(report: ConsolidationReportLike): string {
  const groups = new Map<string, ConsolidationReportLike["learnings"]>();

  for (const learning of report.learnings) {
    const label = dispositionLabel(learning.disposition);
    const existing = groups.get(label) ?? [];
    existing.push(learning);
    groups.set(label, existing);
  }

  const orderedLabels = ["PROMOTE", "REVIEW", "STALE", "KEEP"].filter((label) => groups.has(label));
  const lines = [
    `Knowledge consolidation report: ${report.legionId}`,
    "",
    "Metric | Count",
    "--- | ---",
    `Learnings | ${report.learnings.length}`,
    `Index mutations | ${report.indexMutations.length}`,
    `Status mutations | ${report.statusMutations.length}`,
    `Warnings | ${report.warnings.length}`,
  ];

  for (const label of orderedLabels) {
    const learningGroup = groups.get(label) ?? [];
    lines.push("", `${label} (${learningGroup.length})`);

    for (const learning of learningGroup.toSorted((left, right) =>
      left.path.localeCompare(right.path)
    )) {
      lines.push(`- ${learning.path}${learning.notes ? ` — ${learning.notes}` : ""}`);
    }
  }

  if (report.warnings.length > 0) {
    lines.push("", "Warnings");
    for (const warning of report.warnings) {
      lines.push(`- ${warning}`);
    }
  }

  return lines.join("\n");
}

export function formatConsolidationReportJson(report: ConsolidationReportLike): string {
  return JSON.stringify(report, null, 2);
}
