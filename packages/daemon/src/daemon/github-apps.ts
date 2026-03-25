import type { GitHubAppsConfig } from "./config";

export type RoleName = "impl" | "review" | "ops";

const MODE_TO_ROLE: Record<string, RoleName> = {
  implement: "impl",
  merge: "impl",
  review: "review",
  test: "ops",
  architect: "ops",
  plan: "ops",
};

const APP_NAMES: Record<RoleName, string> = {
  impl: "legion-impl",
  review: "legion-review",
  ops: "legion-ops",
};

function toBase64Url(data: Uint8Array): string {
  return Buffer.from(data)
    .toString("base64")
    .replace(/\+/g, "-")
    .replace(/\//g, "_")
    .replace(/=+$/g, "");
}

function encodeDerLength(length: number): Uint8Array {
  if (length < 128) {
    return new Uint8Array([length]);
  }

  const bytes: number[] = [];
  let value = length;
  while (value > 0) {
    bytes.unshift(value & 0xff);
    value >>= 8;
  }

  return new Uint8Array([0x80 | bytes.length, ...bytes]);
}

function der(tag: number, value: Uint8Array): Uint8Array {
  const length = encodeDerLength(value.length);
  return new Uint8Array([tag, ...length, ...value]);
}

function concatBytes(...parts: Uint8Array[]): Uint8Array {
  const total = parts.reduce((sum, part) => sum + part.length, 0);
  const output = new Uint8Array(total);
  let offset = 0;
  for (const part of parts) {
    output.set(part, offset);
    offset += part.length;
  }
  return output;
}

function decodeBase64ToBytes(base64: string): Uint8Array {
  return new Uint8Array(Buffer.from(base64, "base64"));
}

function parsePem(pem: string): { derBytes: Uint8Array; type: string } {
  const trimmed = pem.trim();
  const match = trimmed.match(/-----BEGIN ([^-]+)-----([\s\S]*?)-----END \1-----/);
  if (!match) {
    throw new Error("Invalid PEM format for GitHub App private key");
  }

  const type = match[1]?.trim() ?? "";
  const body = (match[2] ?? "").replace(/\s+/g, "");
  if (!body) {
    throw new Error("GitHub App private key PEM body is empty");
  }

  return {
    derBytes: decodeBase64ToBytes(body),
    type,
  };
}

function wrapPkcs1InPkcs8(pkcs1Der: Uint8Array): Uint8Array {
  const version = der(0x02, new Uint8Array([0x00]));
  const rsaEncryptionOid = der(
    0x06,
    new Uint8Array([0x2a, 0x86, 0x48, 0x86, 0xf7, 0x0d, 0x01, 0x01, 0x01])
  );
  const nullParam = der(0x05, new Uint8Array());
  const algorithmIdentifier = der(0x30, concatBytes(rsaEncryptionOid, nullParam));
  const privateKey = der(0x04, pkcs1Der);

  return der(0x30, concatBytes(version, algorithmIdentifier, privateKey));
}

async function importPrivateKey(privateKeyPem: string): Promise<CryptoKey> {
  const { derBytes, type } = parsePem(privateKeyPem);
  const pkcs8Der =
    type === "RSA PRIVATE KEY" ? wrapPkcs1InPkcs8(derBytes) : new Uint8Array(derBytes);

  return crypto.subtle.importKey(
    "pkcs8",
    pkcs8Der.buffer as ArrayBuffer,
    {
      name: "RSASSA-PKCS1-v1_5",
      hash: "SHA-256",
    },
    false,
    ["sign"]
  );
}

export function modeToRole(mode: string): RoleName {
  const role = MODE_TO_ROLE[mode];
  if (!role) {
    throw new Error(`Unknown worker mode: ${mode}`);
  }
  return role;
}

export async function generateJwt(appId: string, privateKeyPem: string): Promise<string> {
  const nowSeconds = Math.floor(Date.now() / 1000);
  const header = { alg: "RS256", typ: "JWT" };
  const payload = {
    iss: appId,
    iat: nowSeconds - 60,
    exp: nowSeconds + 600,
  };

  const encodedHeader = toBase64Url(new TextEncoder().encode(JSON.stringify(header)));
  const encodedPayload = toBase64Url(new TextEncoder().encode(JSON.stringify(payload)));
  const signingInput = `${encodedHeader}.${encodedPayload}`;

  const privateKey = await importPrivateKey(privateKeyPem);
  const signature = await crypto.subtle.sign(
    {
      name: "RSASSA-PKCS1-v1_5",
      hash: "SHA-256",
    },
    privateKey,
    new TextEncoder().encode(signingInput)
  );

  const encodedSignature = toBase64Url(new Uint8Array(signature));
  return `${signingInput}.${encodedSignature}`;
}

export async function exchangeToken(
  jwt: string,
  installationId: string,
  fetchFn: typeof fetch = globalThis.fetch
): Promise<{ token: string; expiresAt: string }> {
  const response = await fetchFn(
    `https://api.github.com/app/installations/${installationId}/access_tokens`,
    {
      method: "POST",
      headers: {
        Authorization: `Bearer ${jwt}`,
        Accept: "application/vnd.github+v3+json",
        "User-Agent": "legion-daemon",
      },
    }
  );

  if (!response.ok) {
    const bodyText = await response.text();
    throw new Error(`Failed to exchange GitHub App token (status ${response.status}): ${bodyText}`);
  }

  const body = (await response.json()) as { token?: string; expires_at?: string };
  if (!body.token || !body.expires_at) {
    throw new Error("Failed to exchange GitHub App token: missing token or expires_at");
  }

  return {
    token: body.token,
    expiresAt: body.expires_at,
  };
}

export class TokenManager {
  private cache = new Map<RoleName, { token: string; expiresAt: string }>();

  constructor(
    private config: GitHubAppsConfig,
    private fetchFn: typeof fetch = globalThis.fetch,
    private readFileFn: (path: string) => Promise<string> = (p) => Bun.file(p).text()
  ) {}

  async getCredentials(role: RoleName): Promise<{ token: string; expiresAt: string } | null> {
    const roleConfig = this.config[role];
    if (!roleConfig) {
      return null;
    }

    const cached = this.cache.get(role);
    if (cached && Date.parse(cached.expiresAt) - Date.now() >= 5 * 60 * 1000) {
      return cached;
    }

    const privateKeyPem = await this.readFileFn(roleConfig.privateKeyPath);
    const jwt = await generateJwt(roleConfig.appId, privateKeyPem);
    const token = await exchangeToken(jwt, roleConfig.installationId, this.fetchFn);
    this.cache.set(role, token);
    return token;
  }

  isConfigured(role: RoleName): boolean {
    return this.config[role] !== undefined;
  }

  getGitIdentity(role: RoleName): { name: string; email: string } | null {
    const roleConfig = this.config[role];
    if (!roleConfig) {
      return null;
    }
    return getGitIdentity(role, roleConfig.appId);
  }
}

export function getGitIdentity(role: RoleName, appId: string): { name: string; email: string } {
  const appName = APP_NAMES[role];
  return {
    name: `${appName}[bot]`,
    email: `${appId}+${appName}[bot]@users.noreply.github.com`,
  };
}
