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

/** Fetches GitHub App tokens for Legion's single configured repo and runs `gh` under that
 * identity. Every Legion issue is a Dispatch key with no owner/repo of its own (D1/D2 of the T20
 * design) — the repo is `DaemonConfig.repo`, injected once at construction, never derived from
 * an issue key. `baseEnv` is the daemon's `paneEnv`: the `gh` child gets it plus the minted
 * token and identity, never the daemon's own `process.env`. */
export class GitHubService {
  private readonly owner: string;

  constructor(
    repo: `${string}/${string}`,
    private readonly tokenManager: GitHubTokenSource,
    private readonly runner: CommandRunner,
    private readonly baseEnv: NodeJS.ProcessEnv
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
      env: buildRoleEnv(lease.token, lease.gitIdentity, this.baseEnv),
    });
    if (result.exitCode !== 0) {
      throw new Error(`GitHub command failed: ${result.stderr || result.stdout}`);
    }
    return result.stdout;
  }
}
