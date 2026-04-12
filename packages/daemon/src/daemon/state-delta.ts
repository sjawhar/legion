import type { IssueStateDict } from "../state/types";

const TRACKED_FIELDS = [
  "suggestedAction",
  "status",
  "labels",
  "hasLiveWorker",
  "ciStatus",
  "prIsDraft",
  "hasPr",
  "isBlocked",
] as const;

type TrackedField = (typeof TRACKED_FIELDS)[number];

export type IssueDelta = {
  issueId: string;
  state: IssueStateDict;
};

export type ChangedIssueDelta = {
  issueId: string;
  state: IssueStateDict;
  changedFields: string[];
};

export type StateDelta = {
  type: "state_delta";
  timestamp: number;
  changes: {
    new: IssueDelta[];
    removed: string[];
    changed: ChangedIssueDelta[];
    summary: string;
  };
};

export function computeStateDelta(
  previous: Record<string, IssueStateDict>,
  current: Record<string, IssueStateDict>
): StateDelta | null {
  const newIssues = Object.keys(current)
    .filter((issueId) => !(issueId in previous) && current[issueId].source !== null)
    .sort()
    .map((issueId) => ({
      issueId,
      state: current[issueId],
    }));

  const removed = Object.keys(previous)
    .filter((issueId) => !(issueId in current) && previous[issueId].source !== null)
    .sort();

  const changed = Object.keys(current)
    .filter((issueId) => issueId in previous)
    .sort()
    .reduce<ChangedIssueDelta[]>((deltas, issueId) => {
      const changedFields = TRACKED_FIELDS.filter((field) => {
        return !trackedFieldEquals(previous[issueId], current[issueId], field);
      });

      if (changedFields.length > 0) {
        deltas.push({
          issueId,
          state: current[issueId],
          changedFields: [...changedFields],
        });
      }

      return deltas;
    }, []);

  if (newIssues.length === 0 && removed.length === 0 && changed.length === 0) {
    return null;
  }

  return {
    type: "state_delta",
    timestamp: Date.now(),
    changes: {
      new: newIssues,
      removed,
      changed,
      summary: `${newIssues.length} new, ${removed.length} removed, ${changed.length} changed`,
    },
  };
}

function trackedFieldEquals(
  previous: IssueStateDict,
  current: IssueStateDict,
  field: TrackedField
): boolean {
  if (field === "labels") {
    return normalizeLabels(previous.labels) === normalizeLabels(current.labels);
  }

  return previous[field] === current[field];
}

function normalizeLabels(labels: string[]): string {
  return [...new Set(labels)].sort().join("\u0000");
}
