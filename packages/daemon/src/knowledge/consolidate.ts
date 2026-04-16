import os from "node:os";
import path from "node:path";

import { resolveLegionPaths } from "../daemon/paths";
import { aggregateLearningFeedback } from "./aggregator";
import { collectLearningFeedback } from "./collector";
import { getLearningFeedbackLogPath } from "./feedback-logger";
import { setLearningStatus } from "./front-matter";
import { applyPromotions } from "./promoter";
import { classifyLearningAggregate } from "./rules";
import type { CollectedIssueFeedback } from "./types";

export interface ConsolidationIndexMutation {
  action: "upsert";
  key: string;
  learningPath: string;
}

export interface ConsolidationStatusMutation {
  action: "set";
  learningPath: string;
  reason: string;
  status: "needs-review";
}

type ClassifiedAggregate = ReturnType<typeof classifyLearningAggregate>;

export interface ConsolidationReport {
  aggregates: ClassifiedAggregate[];
  apply: boolean;
  generatedAt: string;
  indexMutations: ConsolidationIndexMutation[];
  issueCount: number;
  learnings: Array<{
    disposition: ClassifiedAggregate["disposition"];
    notes?: string;
    path: string;
  }>;
  legionId: string;
  logPath: string;
  recordCount: number;
  statusMutations: ConsolidationStatusMutation[];
  warnings: string[];
  workspaceRoot: string;
}

export async function consolidateKnowledge(options: {
  legionId: string;
  workspaceRoot: string;
  repoRoot?: string;
  apply: boolean;
  preCollectedIssues?: CollectedIssueFeedback[];
  env?: Record<string, string | undefined>;
  homeDir?: string;
  now?: Date;
}): Promise<ConsolidationReport> {
  const env = options.env ?? process.env;
  const homeDir = options.homeDir ?? os.homedir();
  const repoRoot = options.repoRoot ?? options.workspaceRoot;
  const docsRoot = path.join(repoRoot, "docs", "solutions");
  const indexDir = path.join(docsRoot, ".index");

  resolveLegionPaths(env, homeDir).forLegion(options.legionId);
  const logPath = getLearningFeedbackLogPath(options.legionId, env, homeDir);
  let issues: CollectedIssueFeedback[];
  const warnings: string[] = [];

  if (options.preCollectedIssues) {
    issues = options.preCollectedIssues;
  } else {
    const collected = await collectLearningFeedback({
      env,
      homeDir,
      legionId: options.legionId,
    });
    issues = collected.issues;
    warnings.push(...collected.warnings);
  }

  const aggregates = aggregateLearningFeedback(issues).map((aggregate) =>
    classifyLearningAggregate(aggregate, options.now)
  );
  const report: ConsolidationReport = {
    aggregates,
    apply: options.apply,
    generatedAt: (options.now ?? new Date()).toISOString(),
    indexMutations: [],
    issueCount: issues.length,
    learnings: aggregates.map((aggregate) => ({
      disposition: aggregate.disposition,
      notes: aggregate.notes,
      path: aggregate.path,
    })),
    legionId: options.legionId,
    logPath,
    recordCount: issues.reduce((count, issue) => count + issue.records.length, 0),
    statusMutations: [],
    warnings,
    workspaceRoot: options.workspaceRoot,
  };

  if (!options.apply) {
    return report;
  }

  const promotionResult = await applyPromotions(
    indexDir,
    docsRoot,
    aggregates
      .filter((aggregate) => aggregate.disposition === "accepted")
      .map((aggregate) => ({
        disposition: aggregate.disposition,
        path: aggregate.path,
        touchedPaths: aggregate.touchedPaths,
      })),
    options.legionId
  );
  report.indexMutations.push(...promotionResult.mutations);
  warnings.push(...promotionResult.warnings);

  for (const aggregate of aggregates) {
    const isReviewCandidate =
      aggregate.disposition === "needs_review" || aggregate.disposition === "archived";

    if (!isReviewCandidate) {
      continue;
    }

    try {
      const updated = await setLearningStatus(path.join(docsRoot, aggregate.path), "needs-review");
      if (!updated) {
        continue;
      }

      report.statusMutations.push({
        action: "set",
        learningPath: aggregate.path,
        reason: aggregate.notes ?? "Set learning status to needs-review.",
        status: "needs-review",
      });
    } catch (error) {
      warnings.push(
        `Failed to update learning status for ${aggregate.path}: ${error instanceof Error ? error.message : String(error)}`
      );
    }
  }

  return report;
}
