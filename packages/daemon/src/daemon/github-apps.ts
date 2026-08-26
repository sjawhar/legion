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

/** Token refresh window — regenerate when within 5 minutes of expiry */
const REFRESH_WINDOW_MS = 5 * 60 * 1000;
const MISSING_INSTALLATION_TTL_MS = 30_000;

const GITHUB_APP_INSTALLATIONS_URL = "https://api.github.com/app/installations";
const INSTALLATIONS_PER_PAGE = 100;
const GITHUB_APP_URL = "https://api.github.com/app";
const GITHUB_USERS_URL = "https://api.github.com/users";

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

async function fetchGitIdentity(
  jwt: string,
  fetchFn: GitHubFetch
): Promise<{ name: string; email: string }> {
  const appResponse = await fetchFn(GITHUB_APP_URL, {
    headers: {
      Authorization: `Bearer ${jwt}`,
      Accept: "application/vnd.github+json",
      "User-Agent": "legion-daemon",
    },
  });
  if (!appResponse.ok) {
    throw new Error(`GitHub App identity lookup failed (${appResponse.status})`);
  }
  const app: unknown = await appResponse.json();
  if (!isRecord(app) || typeof app.slug !== "string" || app.slug.length === 0) {
    throw new Error("GitHub App identity lookup returned an invalid app slug");
  }

  const botLogin = `${app.slug}[bot]`;
  const botResponse = await fetchFn(`${GITHUB_USERS_URL}/${encodeURIComponent(botLogin)}`, {
    headers: {
      Accept: "application/vnd.github+json",
      "User-Agent": "legion-daemon",
    },
  });
  if (!botResponse.ok) {
    throw new Error(`GitHub App bot identity lookup failed (${botResponse.status})`);
  }
  const bot: unknown = await botResponse.json();
  if (
    !isRecord(bot) ||
    (typeof bot.id !== "string" && (typeof bot.id !== "number" || !Number.isFinite(bot.id)))
  ) {
    throw new Error("GitHub App bot identity lookup returned an invalid bot user id");
  }
  return getGitIdentity(String(bot.id), app.slug);
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
  private readonly missingInstallationCache = new Map<GitHubAppRole, Map<string, number>>();
  private readonly fetchFn: GitHubFetch;
  private readonly now: () => number;
  private readonly gitIdentityCache = new Map<string, { name: string; email: string }>();
  private readonly pendingGitIdentities = new Map<
    string,
    Promise<{ name: string; email: string }>
  >();

  constructor(
    private readonly config: GitHubAppsConfig,
    opts?: {
      fetchFn?: GitHubFetch;
      now?: () => number;
    }
  ) {
    this.fetchFn = opts?.fetchFn ?? globalThis.fetch;
    this.now = opts?.now ?? Date.now;
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
  ): Promise<{
    token: string;
    expiresAt: string;
    gitIdentity: { name: string; email: string };
  }> {
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
    _role: GitHubAppRole,
    roleConfig: GitHubAppRoleConfig,
    installationId: string,
    cacheKey: string
  ): Promise<CachedToken> {
    const jwt = await generateJwt(roleConfig.appId, roleConfig.privateKey);
    const [{ token, expiresAt }, gitIdentity] = await Promise.all([
      exchangeToken(jwt, installationId, this.fetchFn),
      this.gitIdentity(roleConfig.appId, jwt),
    ]);
    const result: CachedToken = {
      token,
      expiresAt: new Date(expiresAt),
      gitIdentity,
    };

    this.cache.set(cacheKey, result);
    return result;
  }

  private async gitIdentity(appId: string, jwt: string): Promise<{ name: string; email: string }> {
    const cached = this.gitIdentityCache.get(appId);
    if (cached) return cached;

    const pending = this.pendingGitIdentities.get(appId);
    if (pending) return await pending;

    const lookup = fetchGitIdentity(jwt, this.fetchFn);
    this.pendingGitIdentities.set(appId, lookup);
    try {
      const identity = await lookup;
      this.gitIdentityCache.set(appId, identity);
      return identity;
    } finally {
      this.pendingGitIdentities.delete(appId);
    }
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

    const missingOwners = this.missingInstallationCache.get(role) ?? new Map<string, number>();
    this.missingInstallationCache.set(role, missingOwners);
    const missingUntil = missingOwners.get(ownerKey);
    if (missingUntil !== undefined && missingUntil > this.now()) {
      return undefined;
    }
    missingOwners.delete(ownerKey);

    await this.discoverInstallations(roleConfig, cache);
    const discoveredId = cache.get(ownerKey);
    if (discoveredId) {
      missingOwners.delete(ownerKey);
      return discoveredId;
    }

    await this.discoverInstallations(roleConfig, cache);
    const refreshedId = cache.get(ownerKey);
    if (!refreshedId) {
      missingOwners.set(ownerKey, this.now() + MISSING_INSTALLATION_TTL_MS);
    } else {
      missingOwners.delete(ownerKey);
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
