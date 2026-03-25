import { describe, expect, it, mock } from "bun:test";
import type { GitHubAppsConfig } from "../config";
import {
  buildRoleEnv,
  exchangeToken,
  generateJwt,
  getGitIdentity,
  modeToRole,
  TokenManager,
} from "../github-apps";

function decodeBase64Url(input: string): Buffer {
  const padded = input + "=".repeat((4 - (input.length % 4)) % 4);
  const base64 = padded.replace(/-/g, "+").replace(/_/g, "/");
  return Buffer.from(base64, "base64");
}

function toPem(label: string, der: ArrayBuffer): string {
  const base64 = Buffer.from(new Uint8Array(der)).toString("base64");
  const chunks = base64.match(/.{1,64}/g) ?? [];
  return [`-----BEGIN ${label}-----`, ...chunks, `-----END ${label}-----`, ""].join("\n");
}

async function generateTestKeyPair() {
  const keyPair = await crypto.subtle.generateKey(
    {
      name: "RSASSA-PKCS1-v1_5",
      modulusLength: 2048,
      publicExponent: new Uint8Array([1, 0, 1]),
      hash: "SHA-256",
    },
    true,
    ["sign", "verify"]
  );
  const exportedPrivate = await crypto.subtle.exportKey("pkcs8", keyPair.privateKey);
  return {
    privatePem: toPem("PRIVATE KEY", exportedPrivate),
    publicKey: keyPair.publicKey,
  };
}

describe("modeToRole", () => {
  it("maps implement and merge to impl", () => {
    expect(modeToRole("implement")).toBe("impl");
    expect(modeToRole("merge")).toBe("impl");
  });

  it("maps review to review", () => {
    expect(modeToRole("review")).toBe("review");
  });

  it("maps test, architect, plan to review", () => {
    expect(modeToRole("test")).toBe("review");
    expect(modeToRole("architect")).toBe("review");
    expect(modeToRole("plan")).toBe("review");
  });

  it("throws for unknown mode", () => {
    expect(() => modeToRole("unknown")).toThrow("Unknown worker mode: unknown");
  });
});

describe("getGitIdentity", () => {
  it("returns bot-format name and email", () => {
    const identity = getGitIdentity("12345", "legion-impl");
    expect(identity).toEqual({
      name: "legion-impl[bot]",
      email: "12345+legion-impl[bot]@users.noreply.github.com",
    });
  });
});

describe("generateJwt", () => {
  it("creates RS256 JWT with valid signature and expected claims", async () => {
    const { privatePem, publicKey } = await generateTestKeyPair();
    const jwt = await generateJwt("12345", privatePem);

    const parts = jwt.split(".");
    expect(parts).toHaveLength(3);
    const headerPart = parts[0];
    const payloadPart = parts[1];
    const signaturePart = parts[2];

    const header = JSON.parse(decodeBase64Url(headerPart).toString("utf8")) as {
      alg: string;
      typ: string;
    };
    const payload = JSON.parse(decodeBase64Url(payloadPart).toString("utf8")) as {
      iss: string;
      iat: number;
      exp: number;
    };

    expect(header).toEqual({ alg: "RS256", typ: "JWT" });
    expect(payload.iss).toBe("12345");
    expect(payload.exp - payload.iat).toBe(660); // 60s drift + 600s = 660s window

    const signedInput = `${headerPart}.${payloadPart}`;
    const isValid = await crypto.subtle.verify(
      { name: "RSASSA-PKCS1-v1_5", hash: "SHA-256" },
      publicKey,
      new Uint8Array(decodeBase64Url(signaturePart)).buffer as ArrayBuffer,
      new TextEncoder().encode(signedInput)
    );
    expect(isValid).toBe(true);
  });
});

describe("exchangeToken", () => {
  it("posts to GitHub API and returns token payload", async () => {
    const fetchFn = mock(async (input: string | URL | Request, init?: RequestInit) => {
      expect(String(input)).toBe("https://api.github.com/app/installations/42/access_tokens");
      expect(init?.method).toBe("POST");
      const headers = init?.headers as Record<string, string>;
      expect(headers.Authorization).toBe("Bearer jwt-token");
      expect(headers.Accept).toBe("application/vnd.github+v3+json");
      expect(headers["User-Agent"]).toBe("legion-daemon");

      return new Response(
        JSON.stringify({ token: "ghs_abc", expires_at: "2099-01-01T00:00:00Z" }),
        { status: 201, headers: { "content-type": "application/json" } }
      );
    }) as unknown as typeof fetch;

    const result = await exchangeToken("jwt-token", "42", fetchFn);
    expect(result).toEqual({ token: "ghs_abc", expiresAt: "2099-01-01T00:00:00Z" });
  });

  it("throws descriptive error on non-2xx response", async () => {
    const fetchFn = mock(async () => {
      return new Response(JSON.stringify({ message: "forbidden" }), { status: 403 });
    }) as unknown as typeof fetch;

    await expect(exchangeToken("jwt", "42", fetchFn)).rejects.toThrow(
      "GitHub App token exchange failed (403)"
    );
  });
});

describe("TokenManager", () => {
  const implConfig: GitHubAppsConfig = {
    impl: {
      appId: "111",
      privateKeyPath: "/tmp/impl.pem",
      installationId: "222",
    },
  };

  it("reports configured roles", () => {
    const manager = new TokenManager(implConfig);
    expect(manager.isConfigured("impl")).toBe(true);
    expect(manager.isConfigured("review")).toBe(false);
    expect(manager.getConfiguredRoles()).toEqual(["impl"]);
  });

  it("throws for unconfigured role", async () => {
    const manager = new TokenManager(implConfig);
    await expect(manager.getToken("review")).rejects.toThrow("role_not_configured: review");
  });

  it("generates and caches tokens", async () => {
    const { privatePem } = await generateTestKeyPair();
    let fetchCalls = 0;

    const fetchFn = mock(async () => {
      fetchCalls++;
      const expiresAt = new Date(Date.now() + 60 * 60 * 1000).toISOString();
      return new Response(
        JSON.stringify({ token: `ghs_token_${fetchCalls}`, expires_at: expiresAt }),
        { status: 201, headers: { "content-type": "application/json" } }
      );
    }) as unknown as typeof fetch;

    const readFile = mock(async () => privatePem);

    const manager = new TokenManager(implConfig, { fetchFn, readFile });

    const first = await manager.getToken("impl");
    expect(first.token).toBe("ghs_token_1");
    expect(first.gitIdentity.name).toBe("legion-impl[bot]");

    // Second call should return cached token
    const second = await manager.getToken("impl");
    expect(second.token).toBe("ghs_token_1");
    expect(fetchCalls).toBe(1);
  });

  it("deduplicates concurrent requests", async () => {
    const { privatePem } = await generateTestKeyPair();
    let fetchCalls = 0;

    const fetchFn = mock(async () => {
      fetchCalls++;
      await new Promise((resolve) => setTimeout(resolve, 50));
      const expiresAt = new Date(Date.now() + 60 * 60 * 1000).toISOString();
      return new Response(
        JSON.stringify({ token: `ghs_token_${fetchCalls}`, expires_at: expiresAt }),
        { status: 201, headers: { "content-type": "application/json" } }
      );
    }) as unknown as typeof fetch;

    const readFile = mock(async () => privatePem);
    const manager = new TokenManager(implConfig, { fetchFn, readFile });

    // Fire two concurrent requests
    const [r1, r2] = await Promise.all([manager.getToken("impl"), manager.getToken("impl")]);

    expect(r1.token).toBe(r2.token);
    expect(fetchCalls).toBe(1);
  });
});

describe("buildRoleEnv", () => {
  it("scrubs sensitive env vars and adds role-specific ones", () => {
    const baseEnv: Record<string, string> = {
      PATH: "/usr/bin",
      HOME: "/home/user",
      GH_TOKEN: "personal-token",
      GITHUB_TOKEN: "another-token",
      GH_HOST: "github.com",
      GH_CONFIG_DIR: "/home/user/.config/gh",
      LEGION_ID: "team-1",
    };

    const identity = {
      name: "legion-impl[bot]",
      email: "111+legion-impl[bot]@users.noreply.github.com",
    };
    const result = buildRoleEnv("ghs_role_token", identity, baseEnv);

    // Scrubbed keys should be absent or replaced
    expect(result.HOME).toBe("/home/user"); // HOME preserved — GH_CONFIG_DIR handles credential isolation
    expect(result.GITHUB_TOKEN).toBeUndefined();
    expect(result.GH_HOST).toBeUndefined();

    // Role-specific values
    expect(result.GH_TOKEN).toBe("ghs_role_token");
    expect(result.GH_CONFIG_DIR).toBe("/dev/null");
    expect(result.GIT_AUTHOR_NAME).toBe("legion-impl[bot]");
    expect(result.GIT_AUTHOR_EMAIL).toBe("111+legion-impl[bot]@users.noreply.github.com");
    expect(result.GIT_COMMITTER_NAME).toBe("legion-impl[bot]");
    expect(result.GIT_COMMITTER_EMAIL).toBe("111+legion-impl[bot]@users.noreply.github.com");

    // Non-sensitive keys preserved
    expect(result.PATH).toBe("/usr/bin");
    expect(result.LEGION_ID).toBe("team-1");
  });

  it("scrubs LEGION_GITHUB_APP_* env vars containing private key paths", () => {
    const baseEnv: Record<string, string> = {
      PATH: "/usr/bin",
      LEGION_ID: "team-1",
      LEGION_GITHUB_APP_IMPL_ID: "12345",
      LEGION_GITHUB_APP_IMPL_PRIVATE_KEY_PATH: "/etc/legion/keys/impl.pem",
      LEGION_GITHUB_APP_IMPL_INSTALLATION_ID: "777",
      LEGION_GITHUB_APP_REVIEW_ID: "333",
      LEGION_GITHUB_APP_REVIEW_PRIVATE_KEY_PATH: "/etc/legion/keys/review.pem",
      LEGION_GITHUB_APP_REVIEW_INSTALLATION_ID: "444",
    };

    const identity = {
      name: "legion-impl[bot]",
      email: "12345+legion-impl[bot]@users.noreply.github.com",
    };
    const result = buildRoleEnv("ghs_token", identity, baseEnv);

    // LEGION_GITHUB_APP_* vars should all be scrubbed
    expect(result.LEGION_GITHUB_APP_IMPL_ID).toBeUndefined();
    expect(result.LEGION_GITHUB_APP_IMPL_PRIVATE_KEY_PATH).toBeUndefined();
    expect(result.LEGION_GITHUB_APP_IMPL_INSTALLATION_ID).toBeUndefined();
    expect(result.LEGION_GITHUB_APP_REVIEW_ID).toBeUndefined();
    expect(result.LEGION_GITHUB_APP_REVIEW_PRIVATE_KEY_PATH).toBeUndefined();
    expect(result.LEGION_GITHUB_APP_REVIEW_INSTALLATION_ID).toBeUndefined();

    // Non-LEGION_GITHUB_APP vars preserved
    expect(result.PATH).toBe("/usr/bin");
    expect(result.LEGION_ID).toBe("team-1");
  });
});
