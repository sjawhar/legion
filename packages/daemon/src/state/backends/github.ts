import {
  createParsedIssue,
  GitHubPRRef,
  type GitHubPRRef as GitHubPRRefType,
  type IssueSource,
  IssueStatus,
  type ParsedIssue,
} from "../types";
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
  "linked pull requests"?: string[] | null;
}

function extractItems(raw: unknown): unknown[] {
  if (Array.isArray(raw)) {
    return raw;
  }
  if (typeof raw === "object" && raw !== null && "items" in raw) {
    const wrapped = raw as { items?: unknown };
    if (Array.isArray(wrapped.items)) {
      return wrapped.items;
    }
  }
  return [];
}

function parseOwnerRepo(repository: string): { owner: string; repo: string } | null {
  const parts = repository.split("/");
  if (parts.length !== 2 || !parts[0] || !parts[1]) {
    return null;
  }
  return { owner: parts[0], repo: parts[1] };
}

function buildIssueId(owner: string, repo: string, number: number): string {
  const safeOwner = owner.toLowerCase().replace(/[^a-z0-9-]/g, "-");
  const safeRepo = repo.toLowerCase().replace(/[^a-z0-9-]/g, "-");
  return `${safeOwner}-${safeRepo}-${number}`;
}

export class GitHubTracker implements IssueTracker {
  parseIssues(raw: unknown): ParsedIssue[] {
    const items = extractItems(raw);
    const parsed: ParsedIssue[] = [];

    for (const item of items) {
      if (typeof item !== "object" || item === null) {
        continue;
      }
      const typedItem = item as GitHubProjectItem;
      const content = typedItem.content;
      if (!content || content.type !== "Issue") {
        continue;
      }

      const number = content.number;
      const repository = content.repository;
      if (typeof number !== "number" || typeof repository !== "string") {
        continue;
      }

      const ownerRepo = parseOwnerRepo(repository);
      if (!ownerRepo) {
        continue;
      }

      const issueId = buildIssueId(ownerRepo.owner, ownerRepo.repo, number);
      const status = IssueStatus.normalize(typedItem.status ?? null);

      let labels: string[] = [];
      if (Array.isArray(typedItem.labels)) {
        labels = typedItem.labels.filter(
          (label): label is string => typeof label === "string" && label !== ""
        );
      }

      let prRef: GitHubPRRefType | null = null;
      const linkedPRs = typedItem["linked pull requests"];
      if (Array.isArray(linkedPRs)) {
        for (const prUrl of linkedPRs) {
          if (typeof prUrl === "string") {
            prRef = GitHubPRRef.fromUrl(prUrl);
            if (prRef) break;
          }
        }
      }

      const source: IssueSource = {
        owner: ownerRepo.owner,
        repo: ownerRepo.repo,
        number,
        url: typeof content.url === "string" ? content.url : "",
      };

      parsed.push(createParsedIssue(issueId, status, labels, prRef, source));
    }

    return parsed;
  }

  async resolveTeamId(_ref: string): Promise<string> {
    throw new Error("Not yet implemented");
  }
}
