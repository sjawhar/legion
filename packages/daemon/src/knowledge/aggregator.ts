import type { HandoffPhase } from "../handoff/types";
import {
  type CollectedIssueFeedback,
  type HelpfulIssueContext,
  KNOWLEDGE_NON_ARCHITECT_PHASES,
  type LearningAggregate,
} from "./types";

export interface HelpfulIssuePromotionContext extends HelpfulIssueContext {
  touchedPaths: string[];
}

export interface AggregatedLearningFeedback extends LearningAggregate {
  helpfulRatio: number;
  issues: HelpfulIssuePromotionContext[];
  issuesHelpful: number;
  issuesInjected: number;
  lastInjected: string;
  touchedPaths: string[];
}

interface HelpfulIssueState {
  firstSeenAt: string;
  helpfulPaths: Set<string>;
  phases: Set<HandoffPhase>;
  touchedPaths: Set<string>;
}

interface AggregateState {
  firstSeenAt: string;
  helpfulIssues: Map<string, HelpfulIssueState>;
  injectedIssues: Set<string>;
  issueOrder: string[];
  lastInjected: string;
  lastSeenAt: string;
  path: string;
  touchedPaths: Set<string>;
}

const NON_ARCHITECT_PHASES = new Set<HandoffPhase>(KNOWLEDGE_NON_ARCHITECT_PHASES);

function ensureAggregate(
  aggregates: Map<string, AggregateState>,
  learningPath: string,
  timestamp: string
): AggregateState {
  const existing = aggregates.get(learningPath);
  if (existing) {
    if (timestamp < existing.firstSeenAt) {
      existing.firstSeenAt = timestamp;
    }
    if (timestamp > existing.lastSeenAt) {
      existing.lastSeenAt = timestamp;
    }
    return existing;
  }

  const created: AggregateState = {
    firstSeenAt: timestamp,
    helpfulIssues: new Map<string, HelpfulIssueState>(),
    injectedIssues: new Set<string>(),
    issueOrder: [],
    lastInjected: timestamp,
    lastSeenAt: timestamp,
    path: learningPath,
    touchedPaths: new Set<string>(),
  };
  aggregates.set(learningPath, created);
  return created;
}

function ensureHelpfulIssue(
  aggregate: AggregateState,
  issueId: string,
  timestamp: string
): HelpfulIssueState {
  const existing = aggregate.helpfulIssues.get(issueId);
  if (existing) {
    if (timestamp < existing.firstSeenAt) {
      existing.firstSeenAt = timestamp;
    }
    return existing;
  }

  const created: HelpfulIssueState = {
    firstSeenAt: timestamp,
    helpfulPaths: new Set<string>(),
    phases: new Set<HandoffPhase>(),
    touchedPaths: new Set<string>(),
  };
  aggregate.helpfulIssues.set(issueId, created);
  aggregate.issueOrder.push(issueId);
  return created;
}

export function aggregateLearningFeedback(
  issues: CollectedIssueFeedback[]
): AggregatedLearningFeedback[] {
  const aggregates = new Map<string, AggregateState>();

  for (const issue of issues) {
    for (const record of issue.records) {
      for (const [phase, phaseFeedback] of Object.entries(record.phases) as Array<
        [HandoffPhase, (typeof record.phases)[HandoffPhase]]
      >) {
        if (!phaseFeedback) {
          continue;
        }

        for (const injectedPath of phaseFeedback.injected) {
          const aggregate = ensureAggregate(aggregates, injectedPath, record.timestamp);
          aggregate.injectedIssues.add(issue.issueId);
          if (record.timestamp > aggregate.lastInjected) {
            aggregate.lastInjected = record.timestamp;
          }
        }

        if (!NON_ARCHITECT_PHASES.has(phase)) {
          continue;
        }

        for (const helpfulPath of phaseFeedback.helpful) {
          const aggregate = ensureAggregate(aggregates, helpfulPath, record.timestamp);
          const helpfulIssue = ensureHelpfulIssue(aggregate, issue.issueId, record.timestamp);

          helpfulIssue.helpfulPaths.add(helpfulPath);
          helpfulIssue.phases.add(phase);

          for (const touchedPath of issue.touchedPaths) {
            helpfulIssue.touchedPaths.add(touchedPath);
            aggregate.touchedPaths.add(touchedPath);
          }
        }
      }
    }
  }

  return Array.from(aggregates.values())
    .sort((left, right) => left.path.localeCompare(right.path))
    .map((aggregate) => {
      const issuesHelpful = aggregate.helpfulIssues.size;
      const issuesInjected = aggregate.injectedIssues.size;
      const touchedPaths = Array.from(aggregate.touchedPaths).sort();

      return {
        firstSeenAt: aggregate.firstSeenAt,
        helpfulCount: issuesHelpful,
        helpfulRatio: issuesInjected === 0 ? 0 : issuesHelpful / issuesInjected,
        issues: aggregate.issueOrder
          .map((issueId) => {
            const issue = aggregate.helpfulIssues.get(issueId);
            if (!issue) {
              return null;
            }

            return {
              firstSeenAt: issue.firstSeenAt,
              helpfulPaths: Array.from(issue.helpfulPaths).sort(),
              issueId,
              phases: Array.from(issue.phases).sort(),
              touchedPaths: Array.from(issue.touchedPaths).sort(),
            };
          })
          .filter((issue): issue is HelpfulIssuePromotionContext => issue !== null),
        issuesHelpful,
        issuesInjected,
        lastInjected: aggregate.lastInjected,
        lastSeenAt: aggregate.lastSeenAt,
        path: aggregate.path,
        touchedCount: touchedPaths.length,
        touchedPaths,
      };
    });
}
