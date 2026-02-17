import {
  createParsedIssue,
  GitHubPRRef,
  type GitHubPRRef as GitHubPRRefType,
  IssueStatus,
  type ParsedIssue,
} from "../types";
import type { IssueTracker } from "./issue-tracker";

export interface LinearStateDict {
  name: string;
}

export interface LinearLabelNode {
  name: string;
}

export interface LinearLabelsContainer {
  nodes: LinearLabelNode[];
}

export interface LinearIssue {
  identifier: string;
  state: LinearStateDict | null;
  labels: LinearLabelsContainer | null;
}

export interface LinearAttachment {
  url?: string;
}

export interface LinearIssueRaw {
  identifier?: string;
  status?: string;
  state?: LinearStateDict;
  labels?: string[] | LinearLabelsContainer;
  attachments?: LinearAttachment[] | { nodes: LinearAttachment[] };
}

export class LinearTracker implements IssueTracker {
  parseIssues(raw: unknown): ParsedIssue[] {
    if (!Array.isArray(raw)) {
      return [];
    }
    const parsed: ParsedIssue[] = [];

    for (const item of raw) {
      if (typeof item !== "object" || item === null) {
        continue;
      }
      const issue = item as LinearIssueRaw;
      const issueId = issue.identifier ?? "";
      if (!issueId) {
        continue;
      }

      let rawStatus: string = issue.status ?? "";
      if (!rawStatus) {
        const stateObj = issue.state;
        rawStatus = stateObj?.name ?? "";
      }
      const status = IssueStatus.normalize(rawStatus);

      const labelsRaw = issue.labels;
      let labels: string[] = [];

      if (labelsRaw !== null && labelsRaw !== undefined) {
        if (typeof labelsRaw === "object" && !Array.isArray(labelsRaw)) {
          const container = labelsRaw as LinearLabelsContainer;
          const nodes = container.nodes ?? [];
          if (Array.isArray(nodes)) {
            labels = nodes
              .filter(
                (node): node is { name: string } =>
                  typeof node === "object" &&
                  node !== null &&
                  typeof (node as { name?: unknown }).name === "string" &&
                  Boolean((node as { name: string }).name)
              )
              .map((node) => node.name);
          }
        } else if (Array.isArray(labelsRaw)) {
          labels = labelsRaw.filter((x): x is string => typeof x === "string" && x !== "");
        }
      }

      let prRef: GitHubPRRefType | null = null;
      const attachmentsRaw = issue.attachments;
      let attachments: LinearAttachment[] = [];
      if (Array.isArray(attachmentsRaw)) {
        attachments = attachmentsRaw;
      } else if (
        typeof attachmentsRaw === "object" &&
        attachmentsRaw !== null &&
        "nodes" in attachmentsRaw &&
        Array.isArray((attachmentsRaw as { nodes?: unknown }).nodes)
      ) {
        attachments = (attachmentsRaw as { nodes: LinearAttachment[] }).nodes;
      }
      for (const attachment of attachments) {
        if (typeof attachment === "object" && attachment !== null) {
          const url = attachment.url;
          if (typeof url === "string" && url.includes("github.com") && url.includes("/pull/")) {
            prRef = GitHubPRRef.fromUrl(url);
            if (prRef) {
              break;
            }
          }
        }
      }

      parsed.push(createParsedIssue(issueId, status, labels, prRef));
    }

    return parsed;
  }
}
