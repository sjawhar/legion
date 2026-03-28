import { createPrivateKey } from "node:crypto";
import type { GitHubAppRole, GitHubAppsConfig } from "./config";

const MODE_TO_ROLE: Record<string, GitHubAppRole> = {
  implement: "impl",
  merge: "impl",
  review: "review",
  test: "review",
  architect: "review",
  plan: "review",
};

const ROLE_TO_APP_NAME: Record<GitHubAppRole, string> = {
  impl: "legion-impl",
  review: "legion-review",
};

/** Token refresh window — regenerate when within 5 minutes of expiry */
const REFRESH_WINDOW_MS = 5 * 60 * 1000;

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

function toBase64Url(buffer: ArrayBuffer | Uint8Array): string {
  const bytes = buffer instanceof Uint8Array ? buffer : new Uint8Array(buffer);
  return Buffer.from(bytes)
    .toString("base64")
    .replace(/\+/g, "-")
    .replace(/\//g, "_")
    .replace(/=+$/, "");
}

function toPkcs8Pem(privateKeyPem: string): string {
  if (privateKeyPem.includes("BEGIN PRIVATE KEY")) {
    return privateKeyPem;
  }
  // PKCS#1 → PKCS#8 via node:crypto
  return createPrivateKey(privateKeyPem).export({
    type: "pkcs8",
    format: "pem",
  }) as string;
}

function pemToDer(pem: string): ArrayBuffer {
  const base64 = pem
    .replace(/-----BEGIN [^-]+-----/, "")
    .replace(/-----END [^-]+-----/, "")
    .replace(/\s/g, "");
  const binary = Buffer.from(base64, "base64");
  return binary.buffer.slice(binary.byteOffset, binary.byteOffset + binary.byteLength);
}

export async function generateJwt(appId: string, privateKeyPem: string): Promise<string> {
  const pkcs8Pem = toPkcs8Pem(privateKeyPem);
  const der = pemToDer(pkcs8Pem);

  const key = await crypto.subtle.importKey(
    "pkcs8",
    der,
    { name: "RSASSA-PKCS1-v1_5", hash: "SHA-256" },
    false,
    ["sign"]
  );

  const now = Math.floor(Date.now() / 1000);
  const header = toBase64Url(
    new TextEncoder().encode(JSON.stringify({ alg: "RS256", typ: "JWT" }))
  );
  const payload = toBase64Url(
    new TextEncoder().encode(JSON.stringify({ iss: appId, iat: now - 60, exp: now + 600 }))
  );
  const signingInput = `${header}.${payload}`;
  const signature = await crypto.subtle.sign(
    "RSASSA-PKCS1-v1_5",
    key,
    new TextEncoder().encode(signingInput)
  );

  return `${signingInput}.${toBase64Url(signature)}`;
}

export async function exchangeToken(
  jwt: string,
  installationId: string,
  fetchFn: typeof fetch = globalThis.fetch
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

export class TokenManager {
  private readonly cache = new Map<GitHubAppRole, CachedToken>();
  private readonly pending = new Map<GitHubAppRole, Promise<CachedToken>>();
  private readonly keyCache = new Map<GitHubAppRole, string>();
  private readonly fetchFn: typeof fetch;
  private readonly readFile: (path: string) => Promise<string>;

  constructor(
    private readonly config: GitHubAppsConfig,
    opts?: {
      fetchFn?: typeof fetch;
      readFile?: (path: string) => Promise<string>;
    }
  ) {
    this.fetchFn = opts?.fetchFn ?? globalThis.fetch;
    this.readFile = opts?.readFile ?? ((p: string) => Bun.file(p).text());
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
    role: GitHubAppRole
  ): Promise<{ token: string; expiresAt: string; gitIdentity: { name: string; email: string } }> {
    const roleConfig = this.config[role];
    if (!roleConfig) {
      throw new Error(`role_not_configured: ${role}`);
    }

    // Check cache
    const cached = this.cache.get(role);
    if (cached && cached.expiresAt.getTime() - Date.now() > REFRESH_WINDOW_MS) {
      return {
        token: cached.token,
        expiresAt: cached.expiresAt.toISOString(),
        gitIdentity: cached.gitIdentity,
      };
    }

    // Deduplicate concurrent requests
    const existing = this.pending.get(role);
    if (existing) {
      const result = await existing;
      return {
        token: result.token,
        expiresAt: result.expiresAt.toISOString(),
        gitIdentity: result.gitIdentity,
      };
    }

    const promise = this.generateToken(role, roleConfig);
    this.pending.set(role, promise);

    try {
      const result = await promise;
      return {
        token: result.token,
        expiresAt: result.expiresAt.toISOString(),
        gitIdentity: result.gitIdentity,
      };
    } finally {
      this.pending.delete(role);
    }
  }

  private async generateToken(
    role: GitHubAppRole,
    roleConfig: { appId: string; privateKeyPath: string; installationId: string }
  ): Promise<CachedToken> {
    let privateKey = this.keyCache.get(role);
    if (!privateKey) {
      privateKey = await this.readFile(roleConfig.privateKeyPath);
      this.keyCache.set(role, privateKey);
    }
    const jwt = await generateJwt(roleConfig.appId, privateKey);
    const { token, expiresAt } = await exchangeToken(jwt, roleConfig.installationId, this.fetchFn);

    const appName = ROLE_TO_APP_NAME[role];
    const gitIdentity = getGitIdentity(roleConfig.appId, appName);
    const result: CachedToken = {
      token,
      expiresAt: new Date(expiresAt),
      gitIdentity,
    };

    this.cache.set(role, result);
    return result;
  }
}

/**
 * Environment variables to strip from role serves to prevent credential leakage.
 * Workers should only have access to their role's GH_TOKEN.
 */
const SCRUBBED_ENV_KEYS = ["GH_TOKEN", "GITHUB_TOKEN", "GH_HOST", "GH_CONFIG_DIR"];
const SCRUBBED_ENV_PREFIX = "LEGION_GITHUB_APP_";

export function buildRoleEnv(
  token: string,
  gitIdentity: { name: string; email: string },
  baseEnv: Record<string, string>
): Record<string, string> {
  const env: Record<string, string> = {};

  for (const [key, value] of Object.entries(baseEnv)) {
    if (!SCRUBBED_ENV_KEYS.includes(key) && !key.startsWith(SCRUBBED_ENV_PREFIX)) {
      env[key] = value;
    }
  }

  env.GH_TOKEN = token;
  env.GH_CONFIG_DIR = "/dev/null";
  env.GIT_AUTHOR_NAME = gitIdentity.name;
  env.GIT_AUTHOR_EMAIL = gitIdentity.email;
  env.GIT_COMMITTER_NAME = gitIdentity.name;
  env.GIT_COMMITTER_EMAIL = gitIdentity.email;

  return env;
}
