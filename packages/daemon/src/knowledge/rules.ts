import type { AggregatedLearningFeedback } from "./aggregator";
import type { ClassifiedLearningAggregate } from "./types";

const NINETY_DAYS_MS = 90 * 24 * 60 * 60 * 1000;

export type ClassifiedAggregatedLearningFeedback = AggregatedLearningFeedback &
  ClassifiedLearningAggregate;

export function classifyLearningAggregate(
  aggregate: AggregatedLearningFeedback,
  now: Date = new Date()
): ClassifiedAggregatedLearningFeedback {
  const lastInjectedAt = Date.parse(aggregate.lastInjected);
  const isStale =
    Number.isFinite(lastInjectedAt) && now.getTime() - lastInjectedAt > NINETY_DAYS_MS;

  if (aggregate.helpfulRatio >= 0.7 && aggregate.issuesHelpful >= 3) {
    return {
      ...aggregate,
      disposition: "accepted",
      notes: "Promote this learning because it is repeatedly helpful across issues.",
    };
  }

  if (isStale && aggregate.issuesHelpful === 0) {
    return {
      ...aggregate,
      disposition: "archived",
      notes: "Archive this learning because it has gone stale without helpful confirmations.",
    };
  }

  if (aggregate.issuesInjected >= 3 && aggregate.helpfulRatio < 0.3) {
    return {
      ...aggregate,
      disposition: "needs_review",
      notes: "Review this learning because it is frequently injected but rarely helpful.",
    };
  }

  return {
    ...aggregate,
    disposition: "rejected",
    notes: "Keep gathering evidence before changing this learning entry.",
  };
}
