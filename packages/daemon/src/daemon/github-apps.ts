import type { GitHubAppRole, GitHubAppRoleConfig, GitHubAppsConfig } from "./config";
import { generateJwt } from "./github-app-crypto";

export { generateJwt } from "./github-app-crypto";
export { buildRoleEnv } from "./github-app-env";

const MODE_TO_ROLE: Record<string, GitHubAppRole> = {
  implement: "implement",
  merge: "implement",
  review: "review",
  test: "review",
  architect: "review",
  plan: "review",
};

const ROLE_TO_APP_NAME: Record<GitHubAppRole, string> = {
  implement: "legion-implement",
  review: "legion-review",
};

/** Token refresh window — regenerate when within 5 minutes of expiry */
const REFRESH_WINDOW_MS = 5 * 60 * 1000;
const GITHUB_APP_INSTALLATIONS_URL = "https://api.github.com/app/installations";
const INSTALLATIONS_PER_PAGE = 100;

export type GitHubFetch = (input: string | URL | Request, init?: RequestInit) => Promise<Response>;

export function modeToRole(mode: string): GitHubAppRole {
  const role = MODE_TO_ROLE[mode];
  if (!role) {
    throw new Error(`Unknown worker mode: ${mode}`);
  }
  return role;
}

export function getGitIdentity(appId: string, appName: string): { name: string; email: string } {
  return {
    name: `${appName}[bot]`,
    email: `${appId}+${appName}[bot]@users.noreply.github.com`,
  };
}

export async function exchangeToken(
  jwt: string,
  installationId: string,
  fetchFn: GitHubFetch = globalThis.fetch
): Promise<{ token: string; expiresAt: string }> {
  const url = `https://api.github.com/app/installations/${installationId}/access_tokens`;
  const res = await fetchFn(url, {
    method: "POST",
    headers: {
      Authorization: `Bearer ${jwt}`,
      Accept: "application/vnd.github+v3+json",
      "User-Agent": "legion-daemon",
    },
  });

  if (!res.ok) {
    let body = "";
    try {
      body = await res.text();
    } catch {
      // ignore
    }
    throw new Error(`GitHub App token exchange failed (${res.status}): ${body}`);
  }

  const data = (await res.json()) as { token: string; expires_at: string };
  return { token: data.token, expiresAt: data.expires_at };
}

interface CachedToken {
  token: string;
  expiresAt: Date;
  gitIdentity: { name: string; email: string };
}

function installationCacheKey(owner: string): string {
  return owner.toLowerCase();
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function readInstallation(value: unknown): { owner: string; id: string } {
  if (!isRecord(value)) {
    throw new Error("GitHub App installation discovery returned an invalid installation");
  }
  const account = value.account;
  if (!isRecord(account)) {
    throw new Error(
      "GitHub App installation discovery returned an installation without an account"
    );
  }
  const login = account.login;
  const id = value.id;
  if (
    typeof login !== "string" ||
    login.length === 0 ||
    (typeof id !== "string" && (typeof id !== "number" || !Number.isFinite(id)))
  ) {
    throw new Error("GitHub App installation discovery returned an invalid installation account");
  }
  return { owner: login, id: String(id) };
}

export class TokenManager {
  private readonly cache = new Map<string, CachedToken>();
  private readonly pending = new Map<string, Promise<CachedToken>>();
  private readonly installationCache = new Map<GitHubAppRole, Map<string, string>>();
  private readonly missingInstallationCache = new Map<GitHubAppRole, Set<string>>();
  private readonly fetchFn: GitHubFetch;

  constructor(
    private readonly config: GitHubAppsConfig,
    opts?: {
      fetchFn?: GitHubFetch;
    }
  ) {
    this.fetchFn = opts?.fetchFn ?? globalThis.fetch;
  }

  isConfigured(role: GitHubAppRole): boolean {
    return this.config[role] !== undefined;
  }

  getConfiguredRoles(): GitHubAppRole[] {
    return (Object.keys(this.config) as GitHubAppRole[]).filter(
      (role) => this.config[role] !== undefined
    );
  }

  async getToken(
    role: GitHubAppRole,
    owner: string
  ): Promise<{ token: string; expiresAt: string; gitIdentity: { name: string; email: string } }> {
    const roleConfig = this.config[role];
    if (!roleConfig) {
      throw new Error(`role_not_configured: ${role}`);
    }

    const installationId = await this.resolveInstallationId(role, roleConfig, owner);
    if (!installationId) {
      throw new Error(`github_app_not_installed: ${role} not installed on ${owner}`);
    }

    const cacheKey = `${role}:${installationCacheKey(owner)}`;

    // Check cache
    const cached = this.cache.get(cacheKey);
    if (cached && cached.expiresAt.getTime() - Date.now() > REFRESH_WINDOW_MS) {
      return {
        token: cached.token,
        expiresAt: cached.expiresAt.toISOString(),
        gitIdentity: cached.gitIdentity,
      };
    }

    // Deduplicate concurrent requests
    const existing = this.pending.get(cacheKey);
    if (existing) {
      const result = await existing;
      return {
        token: result.token,
        expiresAt: result.expiresAt.toISOString(),
        gitIdentity: result.gitIdentity,
      };
    }

    const promise = this.generateToken(role, roleConfig, installationId, cacheKey);
    this.pending.set(cacheKey, promise);

    try {
      const result = await promise;
      return {
        token: result.token,
        expiresAt: result.expiresAt.toISOString(),
        gitIdentity: result.gitIdentity,
      };
    } finally {
      this.pending.delete(cacheKey);
    }
  }

  private async generateToken(
    role: GitHubAppRole,
    roleConfig: GitHubAppRoleConfig,
    installationId: string,
    cacheKey: string
  ): Promise<CachedToken> {
    const jwt = await generateJwt(roleConfig.appId, roleConfig.privateKey);
    const { token, expiresAt } = await exchangeToken(jwt, installationId, this.fetchFn);

    const appName = ROLE_TO_APP_NAME[role];
    const gitIdentity = getGitIdentity(roleConfig.appId, appName);
    const result: CachedToken = {
      token,
      expiresAt: new Date(expiresAt),
      gitIdentity,
    };

    this.cache.set(cacheKey, result);
    return result;
  }

  private async resolveInstallationId(
    role: GitHubAppRole,
    roleConfig: GitHubAppRoleConfig,
    owner: string
  ): Promise<string | undefined> {
    const ownerKey = installationCacheKey(owner);
    const configuredId = Object.entries(roleConfig.installations ?? {}).find(
      ([configuredOwner]) => installationCacheKey(configuredOwner) === ownerKey
    )?.[1];
    if (configuredId) {
      return configuredId;
    }

    const cache = this.installationCache.get(role) ?? new Map<string, string>();
    this.installationCache.set(role, cache);
    const cachedId = cache.get(ownerKey);
    if (cachedId) {
      return cachedId;
    }

    const missingOwners = this.missingInstallationCache.get(role) ?? new Set<string>();
    this.missingInstallationCache.set(role, missingOwners);
    if (missingOwners.has(ownerKey)) {
      return undefined;
    }

    await this.discoverInstallations(roleConfig, cache);
    const discoveredId = cache.get(ownerKey);
    if (discoveredId) {
      return discoveredId;
    }

    await this.discoverInstallations(roleConfig, cache);
    const refreshedId = cache.get(ownerKey);
    if (!refreshedId) {
      missingOwners.add(ownerKey);
    }
    return refreshedId;
  }

  private async discoverInstallations(
    roleConfig: GitHubAppRoleConfig,
    cache: Map<string, string>
  ): Promise<void> {
    const jwt = await generateJwt(roleConfig.appId, roleConfig.privateKey);
    for (let page = 1; ; page += 1) {
      const url = new URL(GITHUB_APP_INSTALLATIONS_URL);
      url.searchParams.set("per_page", String(INSTALLATIONS_PER_PAGE));
      url.searchParams.set("page", String(page));
      const response = await this.fetchFn(url, {
        headers: {
          Authorization: `Bearer ${jwt}`,
          Accept: "application/vnd.github+json",
          "User-Agent": "legion-daemon",
        },
      });
      if (!response.ok) {
        throw new Error(`GitHub App installation discovery failed (${response.status})`);
      }
      const payload: unknown = await response.json();
      if (!Array.isArray(payload)) {
        throw new Error("GitHub App installation discovery returned an invalid response");
      }
      for (const installation of payload) {
        const resolved = readInstallation(installation);
        cache.set(installationCacheKey(resolved.owner), resolved.id);
      }
      if (payload.length < INSTALLATIONS_PER_PAGE) {
        return;
      }
    }
  }
}
