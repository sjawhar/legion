import { GitHubTracker } from "./github";
import type { BackendName, IssueTracker } from "./issue-tracker";
import { LinearTracker } from "./linear";

export function getBackend(name: BackendName): IssueTracker {
  switch (name) {
    case "linear":
      return new LinearTracker();
    case "github":
      return new GitHubTracker();
    default:
      throw new Error(`Unknown backend: ${name}`);
  }
}

export type { BackendName, IssueTracker } from "./issue-tracker";
