import type { GitHubAppRole, GitHubAppsConfig } from "./config";
import { buildRoleEnv, modeToRole, type TokenManager } from "./github-apps";
import { isPortFree } from "./ports";
import { createAdapter } from "./runtime";
import type { RuntimeAdapter } from "./runtime/types";

/**
 * Manages per-role serve instances for credential isolation.
 *
 * When GitHub Apps are configured, each role (impl, review) gets its own
 * `opencode serve` process with role-specific GH_TOKEN and scrubbed environment.
 * Workers are routed to the appropriate serve based on their mode.
 *
 * When GitHub Apps are NOT configured, all workers share the controller's serve
 * (backward compatible single-serve behavior).
 */

export interface RoleServeEntry {
  role: GitHubAppRole;
  adapter: RuntimeAdapter;
  port: number;
}

export interface RoleServeManagerOptions {
  githubApps: GitHubAppsConfig;
  tokenManager: TokenManager;
  runtime: "opencode" | "claude-code";
  basePort: number;
  shortId: string;
  /** Fallback adapter when role serves are not available */
  fallbackAdapter: RuntimeAdapter;
}

export class RoleServeManager {
  private readonly serves = new Map<GitHubAppRole, RoleServeEntry>();
  private readonly tokenManager: TokenManager;
  private readonly fallbackAdapter: RuntimeAdapter;
  private readonly runtime: "opencode" | "claude-code";
  private readonly shortId: string;

  constructor(private readonly opts: RoleServeManagerOptions) {
    this.tokenManager = opts.tokenManager;
    this.fallbackAdapter = opts.fallbackAdapter;
    this.runtime = opts.runtime;
    this.shortId = opts.shortId;
  }

  /**
   * Start per-role serve instances. Allocates sequential ports starting from basePort.
   * Only starts serves for configured roles.
   */
  async start(
    controllerEnv: Record<string, string>,
    workspace: string,
    logDir?: string
  ): Promise<void> {
    const roles = this.tokenManager.getConfiguredRoles();
    let nextPort = this.opts.basePort;

    for (const role of roles) {
      while (!(await isPortFree(nextPort))) {
        nextPort++;
      }

      const { token, gitIdentity } = await this.tokenManager.getToken(role);
      const roleEnv = buildRoleEnv(token, gitIdentity, controllerEnv);

      const adapter = createAdapter(this.runtime, {
        port: nextPort,
        shortId: `${this.shortId}-${role}`,
      });

      await adapter.start({
        env: roleEnv,
        workspace,
        logDir: logDir ? `${logDir}/${role}` : undefined,
      });

      this.serves.set(role, { role, adapter, port: nextPort });
      nextPort++;
    }
  }

  /**
   * Get the adapter for a worker mode. Routes mode → role → adapter.
   * Falls back to the shared adapter if the role's serve isn't available.
   */
  getAdapterForMode(mode: string): RuntimeAdapter {
    const role = modeToRole(mode);
    const entry = this.serves.get(role);
    return entry?.adapter ?? this.fallbackAdapter;
  }

  /**
   * Get the adapter for a specific role.
   * Falls back to the shared adapter if the role's serve isn't available.
   */
  getAdapterForRole(role: GitHubAppRole): RuntimeAdapter {
    const entry = this.serves.get(role);
    return entry?.adapter ?? this.fallbackAdapter;
  }

  /** Check health of all role serves. Returns roles that are unhealthy. */
  async checkHealth(): Promise<GitHubAppRole[]> {
    const unhealthy: GitHubAppRole[] = [];
    for (const [role, entry] of this.serves) {
      const healthy = await entry.adapter.healthy();
      if (!healthy) {
        unhealthy.push(role);
      }
    }
    return unhealthy;
  }

  /**
   * Restart a role's serve with fresh token.
   * Used by health loop when a serve becomes unhealthy or token nears expiry.
   */
  async restartRole(
    role: GitHubAppRole,
    controllerEnv: Record<string, string>,
    workspace: string,
    logDir?: string
  ): Promise<void> {
    const entry = this.serves.get(role);
    if (!entry) {
      return;
    }

    try {
      await entry.adapter.stop();
    } catch {
      // Best effort — serve may already be dead
    }

    const { token, gitIdentity } = await this.tokenManager.getToken(role);
    const roleEnv = buildRoleEnv(token, gitIdentity, controllerEnv);

    await entry.adapter.start({
      env: roleEnv,
      workspace,
      logDir: logDir ? `${logDir}/${role}` : undefined,
    });
  }

  /** Stop all role serves */
  async stop(): Promise<void> {
    for (const entry of this.serves.values()) {
      try {
        await entry.adapter.stop();
      } catch {
        // Best effort
      }
    }
    this.serves.clear();
  }

  /** Get all active role serve entries */
  getEntries(): RoleServeEntry[] {
    return Array.from(this.serves.values());
  }

  /** Check if any role serves are running */
  hasRoleServes(): boolean {
    return this.serves.size > 0;
  }
}
