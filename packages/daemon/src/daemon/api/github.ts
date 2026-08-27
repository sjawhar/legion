import type { IssueKey, LegionRole } from "@legion/contracts";
import { parseIssueKey } from "@legion/contracts";
import type { CommandRunner } from "../../state/fetch";
import type { GitHubAppRole } from "../config";
import { buildRoleEnv } from "../github-apps";

export type TokenLease = {
  token: string;
  expiresAt: string;
  gitIdentity: { name: string; email: string };
};

export interface GithubTokenSource {
  getToken(role: GitHubAppRole, owner: string): Promise<TokenLease>;
}

export function issueUrl(issue: IssueKey): string {
  const parsed = parseIssueKey(issue);
  if (!parsed) {
    throw new Error(`Invalid issue key in Legion state: ${issue}`);
  }
  return `repos/${parsed.owner}/${parsed.repo}/issues/${parsed.number}`;
}

export function appRoleForLegionRole(role: LegionRole): GitHubAppRole {
  return role === "reviewer" ? "review" : "implement";
}

/** Fetches GitHub App tokens for an issue's repo and runs `gh` under that identity. */
export class GithubService {
  constructor(
    private readonly tokenManager: GithubTokenSource,
    private readonly runner: CommandRunner
  ) {}

  async tokenForIssue(issue: IssueKey, appRole: GitHubAppRole): Promise<TokenLease> {
    const parsed = parseIssueKey(issue);
    if (!parsed) {
      throw new Error(`Invalid issue key in Legion state: ${issue}`);
    }
    return this.tokenManager.getToken(appRole, parsed.owner);
  }

  async gh(
    issue: IssueKey,
    command: string[],
    appRole: GitHubAppRole = "implement"
  ): Promise<string> {
    const lease = await this.tokenForIssue(issue, appRole);
    const result = await this.runner(command, {
      env: buildRoleEnv(lease.token, lease.gitIdentity, process.env),
    });
    if (result.exitCode !== 0) {
      throw new Error(`GitHub command failed: ${result.stderr || result.stdout}`);
    }
    return result.stdout;
  }
}
