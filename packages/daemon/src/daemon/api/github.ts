import type { IssueKey } from "@legion/contracts";
import { type GitHubAppRole, ownerForIssue, type ProjectConfig } from "../config";

export interface TokenLease {
  token: string;
  expiresAt: string;
  gitIdentity: { name: string; email: string };
}

export interface GitHubTokenSource {
  getToken(role: GitHubAppRole, owner: string): Promise<TokenLease>;
}

/** Leases GitHub App tokens for the repository selected by an issue's Dispatch project. */
export class GitHubService {
  constructor(
    private readonly projects: Readonly<Record<string, ProjectConfig>>,
    private readonly tokenManager: GitHubTokenSource
  ) {}

  /** The App installation token for the owner of `issue`'s repository. */
  async tokenForIssue(appRole: GitHubAppRole, issue: IssueKey): Promise<TokenLease> {
    return this.tokenManager.getToken(appRole, ownerForIssue({ projects: this.projects }, issue));
  }
}
