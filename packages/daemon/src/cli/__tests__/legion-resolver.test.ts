import { afterEach, beforeEach, describe, expect, mock, test } from "bun:test";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { resolveLegionId } from "../legion-resolver";

describe("resolveLegionId", () => {
  const originalHome = process.env.HOME;
  const originalFetch = globalThis.fetch;
  let testHome: string;
  let testCacheDir: string;
  let testCacheFile: string;

  beforeEach(() => {
    testHome = path.join(os.tmpdir(), `legion-test-${Date.now()}-${Math.random()}`);
    testCacheDir = path.join(testHome, ".legion");
    testCacheFile = path.join(testCacheDir, "project-cache.json");

    fs.mkdirSync(testCacheDir, { recursive: true });
    process.env.HOME = testHome;
  });

  afterEach(() => {
    globalThis.fetch = originalFetch;
    if (fs.existsSync(testHome)) {
      fs.rmSync(testHome, { recursive: true, force: true });
    }
    process.env.HOME = originalHome;
    delete process.env.LINEAR_API_TOKEN;
    delete process.env.XDG_STATE_HOME;
  });

  test("returns UUID as-is when given valid UUID", async () => {
    const uuid = "12345678-1234-1234-1234-123456789abc";
    const result = await resolveLegionId(uuid, testCacheDir);
    expect(result).toBe(uuid);
  });

  test("uses XDG state dir for cache when cacheDir not provided", async () => {
    const xdgStateHome = path.join(testHome, "xdg-state-home");
    const cacheDir = path.join(xdgStateHome, "legion");
    const cacheFile = path.join(cacheDir, "project-cache.json");
    process.env.XDG_STATE_HOME = xdgStateHome;
    fs.mkdirSync(cacheDir, { recursive: true });
    fs.writeFileSync(
      cacheFile,
      JSON.stringify(
        {
          LEG: {
            id: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
            name: "Legion",
          },
        },
        null,
        2
      )
    );

    await expect(resolveLegionId("LEG")).resolves.toBe("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee");
  });

  test("returns UUID as-is when given valid UUID (uppercase)", async () => {
    const uuid = "12345678-1234-1234-1234-123456789ABC";
    const result = await resolveLegionId(uuid, testCacheDir);
    expect(result).toBe(uuid);
  });

  test("resolves legion key from cache file", async () => {
    const teams = {
      LEG: {
        id: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
        name: "Legion",
      },
      ENG: {
        id: "11111111-2222-3333-4444-555555555555",
        name: "Engineering",
      },
    };
    fs.writeFileSync(testCacheFile, JSON.stringify(teams, null, 2));

    const result = await resolveLegionId("LEG", testCacheDir);
    expect(result).toBe("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee");
  });

  test("resolves legion key case-insensitively from cache", async () => {
    const teams = {
      LEG: {
        id: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
        name: "Legion",
      },
    };
    fs.writeFileSync(testCacheFile, JSON.stringify(teams, null, 2));

    const result = await resolveLegionId("leg", testCacheDir);
    expect(result).toBe("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee");
  });

  test("throws error when legion key not in cache and no API key", async () => {
    const teams = {
      ENG: {
        id: "11111111-2222-3333-4444-555555555555",
        name: "Engineering",
      },
    };
    fs.writeFileSync(testCacheFile, JSON.stringify(teams, null, 2));

    await expect(resolveLegionId("LEG", testCacheDir)).rejects.toThrow("'LEG' is not a UUID");
  });

  test("throws error when no cache file and no API key", async () => {
    await expect(resolveLegionId("LEG", testCacheDir)).rejects.toThrow("'LEG' is not a UUID");
  });

  test("looks up legion via API when cache miss and API key present", async () => {
    process.env.LINEAR_API_TOKEN = "test-api-key";

    const mockFetch = mock(async (url: string, options?: RequestInit) => {
      expect(url).toBe("https://api.linear.app/graphql");
      expect(options?.method).toBe("POST");
      const body = JSON.parse(options?.body as string);
      expect(body.variables.key).toBe("LEG");
      expect(body.query).toContain("teams(filter: { key: { eq: $key } })");

      return new Response(
        JSON.stringify({
          data: {
            teams: {
              nodes: [{ id: "fetched-uuid-1234", key: "LEG", name: "Legion Team" }],
            },
          },
        }),
        { status: 200 }
      );
    });

    globalThis.fetch = mockFetch as unknown as typeof fetch;

    const result = await resolveLegionId("LEG", testCacheDir);
    expect(result).toBe("fetched-uuid-1234");
    expect(mockFetch).toHaveBeenCalledTimes(1);
  });

  test("throws error when API returns no legion", async () => {
    process.env.LINEAR_API_TOKEN = "test-api-key";

    globalThis.fetch = mock(async () => {
      return new Response(
        JSON.stringify({
          data: {
            teams: {
              nodes: [],
            },
          },
        }),
        { status: 200 }
      );
    }) as unknown as typeof fetch;

    await expect(resolveLegionId("NOTFOUND", testCacheDir)).rejects.toThrow(
      "Legion 'NOTFOUND' not found in Linear. Available legion keys: (none)"
    );
  });

  test("error message shows previously-cached keys", async () => {
    process.env.LINEAR_API_TOKEN = "test-api-key";

    fs.writeFileSync(
      testCacheFile,
      JSON.stringify(
        {
          ENG: { id: "eng-id", name: "Engineering" },
          DES: { id: "des-id", name: "Design" },
        },
        null,
        2
      )
    );

    globalThis.fetch = mock(async () => {
      return new Response(
        JSON.stringify({
          data: {
            teams: {
              nodes: [],
            },
          },
        }),
        { status: 200 }
      );
    }) as unknown as typeof fetch;

    await expect(resolveLegionId("NOTFOUND", testCacheDir)).rejects.toThrow(
      "Available legion keys: DES, ENG"
    );
  });

  test("throws error when GraphQL response includes errors array", async () => {
    process.env.LINEAR_API_TOKEN = "test-api-key";

    globalThis.fetch = mock(async () => {
      return new Response(
        JSON.stringify({
          errors: [{ message: "auth error" }],
          data: null,
        }),
        { status: 200 }
      );
    }) as unknown as typeof fetch;

    await expect(resolveLegionId("LEG", testCacheDir)).rejects.toThrow("auth error");
  });

  // NOTE: The real API query filters by a single key (teams(filter: { key: { eq: $key } })),
  // so it returns at most 1 legion. This test validates the merge/caching logic with multiple
  // legions as a theoretical edge case — if the API filter is ever broadened, this ensures
  // cache merging still works correctly.

  test("caches all returned legions after successful API lookup", async () => {
    process.env.LINEAR_API_TOKEN = "test-api-key";

    globalThis.fetch = mock(async () => {
      return new Response(
        JSON.stringify({
          data: {
            teams: {
              nodes: [
                { id: "fetched-uuid-1234", key: "LEG", name: "Legion Team" },
                { id: "fetched-uuid-5678", key: "ENG", name: "Engineering Team" },
              ],
            },
          },
        }),
        { status: 200 }
      );
    }) as unknown as typeof fetch;

    const result = await resolveLegionId("LEG", testCacheDir);
    expect(result).toBe("fetched-uuid-1234");

    const cached = JSON.parse(fs.readFileSync(testCacheFile, "utf-8"));
    expect(cached).toEqual({
      LEG: { id: "fetched-uuid-1234", name: "Legion Team" },
      ENG: { id: "fetched-uuid-5678", name: "Engineering Team" },
    });
  });

  test("merges new legions with existing cache", async () => {
    process.env.LINEAR_API_TOKEN = "test-api-key";

    fs.writeFileSync(
      testCacheFile,
      JSON.stringify(
        {
          ENG: { id: "eng-id", name: "Engineering" },
        },
        null,
        2
      )
    );

    globalThis.fetch = mock(async () => {
      return new Response(
        JSON.stringify({
          data: {
            teams: {
              nodes: [{ id: "leg-id", key: "LEG", name: "Legion" }],
            },
          },
        }),
        { status: 200 }
      );
    }) as unknown as typeof fetch;

    const result = await resolveLegionId("LEG", testCacheDir);
    expect(result).toBe("leg-id");

    const cached = JSON.parse(fs.readFileSync(testCacheFile, "utf-8"));
    expect(cached).toEqual({
      ENG: { id: "eng-id", name: "Engineering" },
      LEG: { id: "leg-id", name: "Legion" },
    });
  });

  test("cache write failure still resolves legion", async () => {
    process.env.LINEAR_API_TOKEN = "test-api-key";

    globalThis.fetch = mock(async () => {
      return new Response(
        JSON.stringify({
          data: {
            teams: {
              nodes: [{ id: "leg-id", key: "LEG", name: "Legion" }],
            },
          },
        }),
        { status: 200 }
      );
    }) as unknown as typeof fetch;

    const originalWriteFileSync = fs.writeFileSync;
    fs.writeFileSync = mock(() => {
      throw new Error("disk full");
    }) as typeof fs.writeFileSync;

    try {
      await expect(resolveLegionId("LEG", testCacheDir)).resolves.toBe("leg-id");
    } finally {
      fs.writeFileSync = originalWriteFileSync;
    }
  });

  test("corrupted cache file does not crash", async () => {
    process.env.LINEAR_API_TOKEN = "test-api-key";

    fs.writeFileSync(testCacheFile, "not valid json");

    globalThis.fetch = mock(async () => {
      return new Response(
        JSON.stringify({
          data: {
            teams: {
              nodes: [{ id: "leg-id", key: "LEG", name: "Legion" }],
            },
          },
        }),
        { status: 200 }
      );
    }) as unknown as typeof fetch;

    await expect(resolveLegionId("LEG", testCacheDir)).resolves.toBe("leg-id");
  });

  test("throws error when API response fails schema validation", async () => {
    process.env.LINEAR_API_TOKEN = "test-api-key";

    globalThis.fetch = mock(async () => {
      return new Response(
        JSON.stringify({
          data: {
            teams: {
              nodes: [{ id: "fetched-uuid-1234", key: "LEG" }],
            },
          },
        }),
        { status: 200 }
      );
    }) as unknown as typeof fetch;

    await expect(resolveLegionId("LEG", testCacheDir)).rejects.toThrow(
      /Failed to look up legion.*invalid response/s
    );
  });

  test("throws error when API request fails", async () => {
    process.env.LINEAR_API_TOKEN = "test-api-key";

    globalThis.fetch = mock(async () => {
      throw new Error("Network error");
    }) as unknown as typeof fetch;

    await expect(resolveLegionId("LEG", testCacheDir)).rejects.toThrow(
      "Failed to look up legion 'LEG'"
    );
  });

  test("prefers cache over API when legion key exists in cache", async () => {
    process.env.LINEAR_API_TOKEN = "test-api-key";

    const teams = {
      LEG: {
        id: "cached-uuid",
        name: "Cached Legion",
      },
    };
    fs.writeFileSync(testCacheFile, JSON.stringify(teams, null, 2));

    const mockFetch = mock(async () => {
      throw new Error("Should not call API when cache hit");
    });
    globalThis.fetch = mockFetch as unknown as typeof fetch;

    const result = await resolveLegionId("LEG", testCacheDir);
    expect(result).toBe("cached-uuid");
    expect(mockFetch).not.toHaveBeenCalled();
  });

  test("returns GitHub project ref as-is when backend is github", async () => {
    const result = await resolveLegionId("sjawhar/5", {
      cacheDir: testCacheDir,
      backend: "github",
    });
    expect(result).toBe("sjawhar/5");
  });

  test("skips Linear resolution for github backend even with API key", async () => {
    process.env.LINEAR_API_TOKEN = "test-api-key";
    const mockFetch = mock(async () => {
      throw new Error("Should not call Linear API for github backend");
    });
    globalThis.fetch = mockFetch as unknown as typeof fetch;

    const result = await resolveLegionId("my-org/42", {
      cacheDir: testCacheDir,
      backend: "github",
    });
    expect(result).toBe("my-org/42");
    expect(mockFetch).not.toHaveBeenCalled();
  });

  test("uses Linear resolution when backend is not specified (backward compat)", async () => {
    const uuid = "12345678-1234-1234-1234-123456789abc";
    const result = await resolveLegionId(uuid, { cacheDir: testCacheDir });
    expect(result).toBe(uuid);
  });

  test("throws error when GraphQL response omits data field entirely", async () => {
    process.env.LINEAR_API_TOKEN = "test-api-key";
    globalThis.fetch = mock(async () => {
      return new Response(JSON.stringify({ errors: [{ message: "Authentication required" }] }), {
        status: 200,
      });
    }) as unknown as typeof fetch;
    await expect(resolveLegionId("LEG", testCacheDir)).rejects.toThrow("Authentication required");
  });

  test("throws descriptive error when Linear returns non-JSON response", async () => {
    process.env.LINEAR_API_TOKEN = "test-api-key";

    globalThis.fetch = mock(async () => {
      return new Response("<html>502 Bad Gateway</html>", {
        status: 200,
        statusText: "OK",
      });
    }) as unknown as typeof fetch;

    await expect(resolveLegionId("LEG", testCacheDir)).rejects.toThrow(/non-JSON response/);
  });

  test("non-object cache file (JSON array) falls through to API", async () => {
    process.env.LINEAR_API_TOKEN = "test-api-key";

    fs.writeFileSync(testCacheFile, JSON.stringify([1, 2, 3]));

    globalThis.fetch = mock(async () => {
      return new Response(
        JSON.stringify({
          data: {
            teams: {
              nodes: [{ id: "leg-id", key: "LEG", name: "Legion" }],
            },
          },
        }),
        { status: 200 }
      );
    }) as unknown as typeof fetch;

    const result = await resolveLegionId("LEG", testCacheDir);
    expect(result).toBe("leg-id");
  });
});
