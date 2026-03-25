import { describe, expect, it, mock } from "bun:test";
import type { GitHubAppsConfig } from "../config";
import {
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

describe("modeToRole", () => {
  it("maps all worker modes to expected roles", () => {
    expect(modeToRole("implement")).toBe("impl");
    expect(modeToRole("merge")).toBe("impl");
    expect(modeToRole("review")).toBe("review");
    expect(modeToRole("test")).toBe("ops");
    expect(modeToRole("architect")).toBe("ops");
    expect(modeToRole("plan")).toBe("ops");
  });

  it("throws for unknown mode", () => {
    expect(() => modeToRole("unknown")).toThrow("Unknown worker mode: unknown");
  });
});

describe("generateJwt", () => {
  it("creates RS256 JWT with valid signature and expected claims", async () => {
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
    const privatePem = toPem("PRIVATE KEY", exportedPrivate);
    const jwt = await generateJwt("12345", privatePem);

    const [headerPart, payloadPart, signaturePart] = jwt.split(".");
    expect(headerPart).toBeDefined();
    expect(payloadPart).toBeDefined();
    expect(signaturePart).toBeDefined();

    const header = JSON.parse(decodeBase64Url(headerPart ?? "").toString("utf8")) as {
      alg: string;
      typ: string;
    };
    const payload = JSON.parse(decodeBase64Url(payloadPart ?? "").toString("utf8")) as {
      iss: string;
      iat: number;
      exp: number;
    };

    expect(header).toEqual({ alg: "RS256", typ: "JWT" });
    expect(payload.iss).toBe("12345");
    expect(payload.exp - payload.iat).toBe(660);

    const signedInput = `${headerPart}.${payloadPart}`;
    const isValid = await crypto.subtle.verify(
      { name: "RSASSA-PKCS1-v1_5", hash: "SHA-256" },
      keyPair.publicKey,
      new Uint8Array(decodeBase64Url(signaturePart ?? "")).buffer as ArrayBuffer,
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
        {
          status: 201,
          headers: { "content-type": "application/json" },
        }
      );
    }) as unknown as typeof fetch;

    const result = await exchangeToken("jwt-token", "42", fetchFn);
    expect(result).toEqual({ token: "ghs_abc", expiresAt: "2099-01-01T00:00:00Z" });
  });

  it("throws descriptive error on non-2xx response", async () => {
    const fetchFn = mock(async () => {
      return new Response(JSON.stringify({ message: "forbidden" }), { status: 403 });
    }) as unknown as typeof fetch;

    await expect(exchangeToken("jwt-token", "42", fetchFn)).rejects.toThrow(
      "Failed to exchange GitHub App token"
    );
  });
});

describe("TokenManager", () => {
  it("returns null when role is not configured", async () => {
    const manager = new TokenManager({});
    await expect(manager.getCredentials("impl")).resolves.toBeNull();
  });

  it("caches token and reuses it before refresh window", async () => {
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
    const privatePem = toPem("PRIVATE KEY", exportedPrivate);
    const readFileFn = mock(async (filePath: string) => {
      expect(filePath).toBe("/tmp/impl.pem");
      return privatePem;
    });
    const fetchFn = mock(async () => {
      return new Response(
        JSON.stringify({ token: "cached-token", expires_at: "2099-01-01T00:00:00Z" }),
        {
          status: 201,
          headers: { "content-type": "application/json" },
        }
      );
    }) as unknown as typeof fetch;

    const config: GitHubAppsConfig = {
      impl: {
        appId: "123",
        privateKeyPath: "/tmp/impl.pem",
        installationId: "999",
      },
    };
    const manager = new TokenManager(config, fetchFn, readFileFn);

    const first = await manager.getCredentials("impl");
    const second = await manager.getCredentials("impl");

    expect(first).toEqual({ token: "cached-token", expiresAt: "2099-01-01T00:00:00Z" });
    expect(second).toEqual({ token: "cached-token", expiresAt: "2099-01-01T00:00:00Z" });
    expect(readFileFn).toHaveBeenCalledTimes(1);
    expect(fetchFn).toHaveBeenCalledTimes(1);
  });

  it("refreshes token when cached token is within 5 minutes of expiry", async () => {
    const originalDateNow = Date.now;
    Date.now = () => 1_700_000_000_000;

    try {
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
      const privatePem = toPem("PRIVATE KEY", exportedPrivate);
      const readFileFn = mock(async () => privatePem);

      let callCount = 0;
      const fetchFn = mock(async () => {
        callCount += 1;
        const soonExpiring = new Date(Date.now() + 4 * 60 * 1000).toISOString();
        const futureExpiring = new Date(Date.now() + 60 * 60 * 1000).toISOString();
        const response =
          callCount === 1
            ? { token: "token-1", expires_at: soonExpiring }
            : { token: "token-2", expires_at: futureExpiring };
        return new Response(JSON.stringify(response), {
          status: 201,
          headers: { "content-type": "application/json" },
        });
      }) as unknown as typeof fetch;

      const manager = new TokenManager(
        {
          impl: {
            appId: "app-1",
            privateKeyPath: "/tmp/impl.pem",
            installationId: "inst-1",
          },
        },
        fetchFn,
        readFileFn
      );

      const first = await manager.getCredentials("impl");
      const second = await manager.getCredentials("impl");

      expect(first?.token).toBe("token-1");
      expect(second?.token).toBe("token-2");
      expect(fetchFn).toHaveBeenCalledTimes(2);
    } finally {
      Date.now = originalDateNow;
    }
  });

  it("reports whether role has github app credentials", () => {
    const manager = new TokenManager({
      review: {
        appId: "321",
        privateKeyPath: "/tmp/review.pem",
        installationId: "654",
      },
    });

    expect(manager.isConfigured("review")).toBe(true);
    expect(manager.isConfigured("impl")).toBe(false);
    expect(manager.isConfigured("ops")).toBe(false);
  });

  it("returns git identity for configured role", () => {
    const manager = new TokenManager({
      impl: {
        appId: "42",
        privateKeyPath: "/tmp/impl.pem",
        installationId: "100",
      },
    });

    expect(manager.getGitIdentity("impl")).toEqual({
      name: "legion-impl[bot]",
      email: "42+legion-impl[bot]@users.noreply.github.com",
    });
  });

  it("returns null for unconfigured role", () => {
    const manager = new TokenManager({});
    expect(manager.getGitIdentity("impl")).toBeNull();
  });
});

describe("getGitIdentity", () => {
  it("returns identity for impl role", () => {
    expect(getGitIdentity("impl", "1001")).toEqual({
      name: "legion-impl[bot]",
      email: "1001+legion-impl[bot]@users.noreply.github.com",
    });
  });

  it("returns identity for review role", () => {
    expect(getGitIdentity("review", "1002")).toEqual({
      name: "legion-review[bot]",
      email: "1002+legion-review[bot]@users.noreply.github.com",
    });
  });

  it("returns identity for ops role", () => {
    expect(getGitIdentity("ops", "1003")).toEqual({
      name: "legion-ops[bot]",
      email: "1003+legion-ops[bot]@users.noreply.github.com",
    });
  });
});
