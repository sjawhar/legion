import { createParsedIssue, IssueStatus, type ParsedIssue } from "../types";
import type { IssueTracker } from "./issue-tracker";

interface GitHubProjectItem {
  id?: string;
  content?: {
    number?: number;
    repository?: string;
    url?: string;
    type?: string;
  };
  status?: string | null;
  labels?: string[] | null;
}

export class GitHubTracker implements IssueTracker {
  parseIssues(raw: unknown): ParsedIssue[] {
    if (!Array.isArray(raw)) {
      return [];
    }

    const parsed: ParsedIssue[] = [];

    for (const item of raw as GitHubProjectItem[]) {
      const content = item.content;
      if (!content || content.type !== "Issue") {
        continue;
      }

      const number = content.number;
      const repo = content.repository;
      if (typeof number !== "number" || typeof repo !== "string") {
        continue;
      }

      const issueId = `${repo.toUpperCase()}-${number}`;
      const status = IssueStatus.normalize(item.status ?? null);

      let labels: string[] = [];
      if (Array.isArray(item.labels)) {
        labels = item.labels.filter(
          (label): label is string => typeof label === "string" && label !== ""
        );
      }

      parsed.push(createParsedIssue(issueId, status, labels, null));
    }

    return parsed;
  }

  async resolveTeamId(_ref: string): Promise<string> {
    throw new Error("Not yet implemented");
  }
}
