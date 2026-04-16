import { describe, expect, it, mock } from "bun:test";
import type { GitHubAppsConfig } from "../config";
import {
  buildRoleEnv,
  exchangeToken,
  generateJwt,
  getGitIdentity,
  modeToRole,
  setDraftStatus,
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
  it("maps implement and merge to implement", () => {
    expect(modeToRole("implement")).toBe("implement");
    expect(modeToRole("merge")).toBe("implement");
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
    const identity = getGitIdentity("12345", "legion-implement");
    expect(identity).toEqual({
      name: "legion-implement[bot]",
      email: "12345+legion-implement[bot]@users.noreply.github.com",
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

    expect(async () => {
      await exchangeToken("jwt", "42", fetchFn);
    }).toThrow("GitHub App token exchange failed (403)");
  });
});

describe("TokenManager", () => {
  const implementConfig: GitHubAppsConfig = {
    implement: {
      appId: "111",
      privateKey: "unused-in-shared-fixture",
      installations: {
        acme: "222",
      },
    },
  };

  it("reports configured roles", () => {
    const manager = new TokenManager(implementConfig);
    expect(manager.isConfigured("implement")).toBe(true);
    expect(manager.isConfigured("review")).toBe(false);
    expect(manager.getConfiguredRoles()).toEqual(["implement"]);
  });

  it("throws for unconfigured role", async () => {
    const manager = new TokenManager(implementConfig);
    expect(async () => {
      await manager.getToken("review", "acme");
    }).toThrow("role_not_configured: review");
  });

  it("throws when owner installation is not configured", async () => {
    const manager = new TokenManager(implementConfig);
    expect(async () => {
      await manager.getToken("implement", "missing-owner");
    }).toThrow("installation_not_configured: implement:missing-owner");
  });

  it("caches tokens per role and owner until the refresh window", async () => {
    const { privatePem } = await generateTestKeyPair();
    let fetchCalls = 0;

    const fetchFn = mock(async (input: string | URL | Request) => {
      fetchCalls++;
      const installationId = String(input).match(/installations\/(.+)\/access_tokens$/)?.[1];
      const expiresAt = new Date(Date.now() + 60 * 60 * 1000).toISOString();
      return new Response(
        JSON.stringify({ token: `ghs_${installationId}_${fetchCalls}`, expires_at: expiresAt }),
        { status: 201, headers: { "content-type": "application/json" } }
      );
    }) as unknown as typeof fetch;

    const manager = new TokenManager(
      {
        implement: {
          appId: "111",
          privateKey: privatePem,
          installations: {
            acme: "222",
            beta: "333",
          },
        },
      },
      { fetchFn }
    );

    const first = await manager.getToken("implement", "acme");
    expect(first.token).toBe("ghs_222_1");
    expect(first.gitIdentity.name).toBe("legion-implement[bot]");

    // Second call should return cached token
    const second = await manager.getToken("implement", "acme");
    expect(second.token).toBe("ghs_222_1");
    expect(fetchCalls).toBe(1);

    const differentOwner = await manager.getToken("implement", "beta");
    expect(differentOwner.token).toBe("ghs_333_2");
    expect(fetchCalls).toBe(2);
  });

  it("deduplicates concurrent requests per role and owner", async () => {
    const { privatePem } = await generateTestKeyPair();
    let fetchCalls = 0;

    const fetchFn = mock(async (input: string | URL | Request) => {
      fetchCalls++;
      await new Promise((resolve) => setTimeout(resolve, 50));
      const installationId = String(input).match(/installations\/(.+)\/access_tokens$/)?.[1];
      const expiresAt = new Date(Date.now() + 60 * 60 * 1000).toISOString();
      return new Response(
        JSON.stringify({ token: `ghs_${installationId}_${fetchCalls}`, expires_at: expiresAt }),
        { status: 201, headers: { "content-type": "application/json" } }
      );
    }) as unknown as typeof fetch;

    const manager = new TokenManager(
      {
        implement: {
          appId: "111",
          privateKey: privatePem,
          installations: {
            acme: "222",
            beta: "333",
          },
        },
      },
      { fetchFn }
    );

    const [r1, r2, otherOwner] = await Promise.all([
      manager.getToken("implement", "acme"),
      manager.getToken("implement", "acme"),
      manager.getToken("implement", "beta"),
    ]);

    expect(r1.token).toBe(r2.token);
    expect(r1.token).toStartWith("ghs_222_");
    expect(otherOwner.token).toStartWith("ghs_333_");
    expect(fetchCalls).toBe(2);
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
      name: "legion-implement[bot]",
      email: "111+legion-implement[bot]@users.noreply.github.com",
    };
    const result = buildRoleEnv("ghs_role_token", identity, baseEnv);

    // Scrubbed keys should be absent or replaced
    expect(result.HOME).toBe("/home/user"); // HOME preserved — GH_CONFIG_DIR handles credential isolation
    expect(result.GITHUB_TOKEN).toBeUndefined();
    expect(result.GH_HOST).toBeUndefined();

    // Role-specific values
    expect(result.GH_TOKEN).toBe("ghs_role_token");
    expect(result.GH_CONFIG_DIR).toBe("/dev/null");
    expect(result.GIT_AUTHOR_NAME).toBe("legion-implement[bot]");
    expect(result.GIT_AUTHOR_EMAIL).toBe("111+legion-implement[bot]@users.noreply.github.com");
    expect(result.GIT_COMMITTER_NAME).toBe("legion-implement[bot]");
    expect(result.GIT_COMMITTER_EMAIL).toBe("111+legion-implement[bot]@users.noreply.github.com");

    // Non-sensitive keys preserved
    expect(result.PATH).toBe("/usr/bin");
    expect(result.LEGION_ID).toBe("team-1");
  });

  it("scrubs LEGION_GITHUB_APP_* env vars containing inline app credentials", () => {
    const baseEnv: Record<string, string> = {
      PATH: "/usr/bin",
      LEGION_ID: "team-1",
      LEGION_GITHUB_APP_IMPLEMENT_ID: "12345",
      LEGION_GITHUB_APP_IMPLEMENT_PRIVATE_KEY: "-----BEGIN PRIVATE KEY-----...",
      LEGION_GITHUB_APP_IMPLEMENT_INSTALLATIONS_ACME: "777",
      LEGION_GITHUB_APP_REVIEW_ID: "333",
      LEGION_GITHUB_APP_REVIEW_PRIVATE_KEY: "-----BEGIN PRIVATE KEY-----...",
      LEGION_GITHUB_APP_REVIEW_INSTALLATIONS_ACME: "444",
    };

    const identity = {
      name: "legion-implement[bot]",
      email: "12345+legion-implement[bot]@users.noreply.github.com",
    };
    const result = buildRoleEnv("ghs_token", identity, baseEnv);

    // LEGION_GITHUB_APP_* vars should all be scrubbed
    expect(result.LEGION_GITHUB_APP_IMPLEMENT_ID).toBeUndefined();
    expect(result.LEGION_GITHUB_APP_IMPLEMENT_PRIVATE_KEY).toBeUndefined();
    expect(result.LEGION_GITHUB_APP_IMPLEMENT_INSTALLATIONS_ACME).toBeUndefined();
    expect(result.LEGION_GITHUB_APP_REVIEW_ID).toBeUndefined();
    expect(result.LEGION_GITHUB_APP_REVIEW_PRIVATE_KEY).toBeUndefined();
    expect(result.LEGION_GITHUB_APP_REVIEW_INSTALLATIONS_ACME).toBeUndefined();

    // Non-LEGION_GITHUB_APP vars preserved
    expect(result.PATH).toBe("/usr/bin");
    expect(result.LEGION_ID).toBe("team-1");
  });
});

describe("setDraftStatus", () => {
  it("marks a PR as ready using markPullRequestReadyForReview", async () => {
    const fetchFn = mock(async (_input: string | URL | Request, init?: RequestInit) => {
      const body = JSON.parse(init?.body as string) as { query: string; variables: { id: string } };
      expect(body.query).toContain("markPullRequestReadyForReview");
      expect(body.variables.id).toBe("PR_abc123");
      const headers = init?.headers as Record<string, string>;
      expect(headers.Authorization).toBe("Bearer ghs_test");

      return new Response(
        JSON.stringify({
          data: {
            markPullRequestReadyForReview: {
              pullRequest: { id: "PR_abc123", isDraft: false },
            },
          },
        }),
        { status: 200, headers: { "content-type": "application/json" } }
      );
    }) as unknown as typeof fetch;

    const result = await setDraftStatus("ghs_test", "PR_abc123", true, fetchFn);
    expect(result).toEqual({ id: "PR_abc123", isDraft: false });
  });

  it("converts a PR to draft using convertPullRequestToDraft", async () => {
    const fetchFn = mock(async (_input: string | URL | Request, init?: RequestInit) => {
      const body = JSON.parse(init?.body as string) as { query: string; variables: { id: string } };
      expect(body.query).toContain("convertPullRequestToDraft");
      expect(body.variables.id).toBe("PR_xyz789");

      return new Response(
        JSON.stringify({
          data: {
            convertPullRequestToDraft: {
              pullRequest: { id: "PR_xyz789", isDraft: true },
            },
          },
        }),
        { status: 200, headers: { "content-type": "application/json" } }
      );
    }) as unknown as typeof fetch;

    const result = await setDraftStatus("ghs_test", "PR_xyz789", false, fetchFn);
    expect(result).toEqual({ id: "PR_xyz789", isDraft: true });
  });

  it("throws on HTTP error", async () => {
    const fetchFn = mock(async () => {
      return new Response("Internal Server Error", { status: 500 });
    }) as unknown as typeof fetch;

    expect(async () => {
      await setDraftStatus("ghs_test", "PR_abc", true, fetchFn);
    }).toThrow("GitHub GraphQL request failed (500)");
  });

  it("throws on GraphQL errors", async () => {
    const fetchFn = mock(async () => {
      return new Response(
        JSON.stringify({
          data: { markPullRequestReadyForReview: null },
          errors: [{ message: "Resource not accessible by integration", type: "FORBIDDEN" }],
        }),
        { status: 200, headers: { "content-type": "application/json" } }
      );
    }) as unknown as typeof fetch;

    expect(async () => {
      await setDraftStatus("ghs_test", "PR_abc", true, fetchFn);
    }).toThrow("GraphQL error: Resource not accessible by integration");
  });
});
