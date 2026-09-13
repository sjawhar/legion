import type { GitHubAppRole } from "../config";

export interface TokenLease {
  token: string;
  expiresAt: string;
  gitIdentity: { name: string; email: string };
}

export interface GitHubTokenSource {
  getToken(role: GitHubAppRole, owner: string): Promise<TokenLease>;
}

/** Leases GitHub App tokens for Legion's single configured repo. Every Legion issue is a Dispatch
 * key with no owner/repo of its own (D1/D2 of the T20 design) — the repo is `DaemonConfig.repo`,
 * injected once at construction, never derived from an issue key. */
export class GitHubService {
  private readonly owner: string;

  constructor(
    repo: `${string}/${string}`,
    private readonly tokenManager: GitHubTokenSource
  ) {
    const [owner] = repo.split("/") as [string, string];
    this.owner = owner;
  }

  async tokenForIssue(appRole: GitHubAppRole): Promise<TokenLease> {
    return this.tokenManager.getToken(appRole, this.owner);
  }
}
