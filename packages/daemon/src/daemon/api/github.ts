import type { LegionRole } from "@legion/contracts";
import type { CommandRunner } from "../../state/fetch";
import type { GitHubAppRole } from "../config";
import { buildRoleEnv } from "../github-apps";

export interface TokenLease {
  token: string;
  expiresAt: string;
  gitIdentity: { name: string; email: string };
}

export interface GitHubTokenSource {
  getToken(role: GitHubAppRole, owner: string): Promise<TokenLease>;
}

/** The controller merges under the implement App's identity, subject to the repository's own
 * branch protection; only the reviewer role uses the review App. */
export function appRoleForLegionRole(role: LegionRole | "controller"): GitHubAppRole {
  return role === "reviewer" ? "review" : "implement";
}

/** Fetches GitHub App tokens for Legion's single configured repo and runs `gh` under that
 * identity. Every Legion issue is a Dispatch key with no owner/repo of its own (D1/D2 of the T20
 * design) — the repo is `DaemonConfig.repo`, injected once at construction, never derived from
 * an issue key. */
export class GitHubService {
  private readonly owner: string;

  constructor(
    repo: `${string}/${string}`,
    private readonly tokenManager: GitHubTokenSource,
    private readonly runner: CommandRunner
  ) {
    const [owner] = repo.split("/") as [string, string];
    this.owner = owner;
  }

  async tokenForIssue(appRole: GitHubAppRole): Promise<TokenLease> {
    return this.tokenManager.getToken(appRole, this.owner);
  }

  async gh(command: string[], appRole: GitHubAppRole = "implement"): Promise<string> {
    const lease = await this.tokenForIssue(appRole);
    const result = await this.runner(command, {
      env: buildRoleEnv(lease.token, lease.gitIdentity, process.env),
    });
    if (result.exitCode !== 0) {
      throw new Error(`GitHub command failed: ${result.stderr || result.stdout}`);
    }
    return result.stdout;
  }
}
