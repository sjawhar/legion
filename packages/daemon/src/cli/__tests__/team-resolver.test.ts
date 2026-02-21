import { afterEach, beforeEach, describe, expect, mock, test } from "bun:test";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { resolveTeamId } from "../team-resolver";

describe("resolveTeamId", () => {
  const originalHome = process.env.HOME;
  const originalFetch = globalThis.fetch;
  let testHome: string;
  let testCacheDir: string;
  let testCacheFile: string;

  beforeEach(() => {
    testHome = path.join(os.tmpdir(), `legion-test-${Date.now()}-${Math.random()}`);
    testCacheDir = path.join(testHome, ".legion");
    testCacheFile = path.join(testCacheDir, "teams.json");

    fs.mkdirSync(testCacheDir, { recursive: true });
    process.env.HOME = testHome;
  });

  afterEach(() => {
    globalThis.fetch = originalFetch;
    if (fs.existsSync(testHome)) {
      fs.rmSync(testHome, { recursive: true, force: true });
    }
    process.env.HOME = originalHome;
    delete process.env.LINEAR_API_KEY;
  });

  test("returns UUID as-is when given valid UUID", async () => {
    const uuid = "12345678-1234-1234-1234-123456789abc";
    const result = await resolveTeamId(uuid, testCacheDir);
    expect(result).toBe(uuid);
  });

  test("returns UUID as-is when given valid UUID (uppercase)", async () => {
    const uuid = "12345678-1234-1234-1234-123456789ABC";
    const result = await resolveTeamId(uuid, testCacheDir);
    expect(result).toBe(uuid);
  });

  test("resolves team key from cache file", async () => {
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

    const result = await resolveTeamId("LEG", testCacheDir);
    expect(result).toBe("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee");
  });

  test("resolves team key case-insensitively from cache", async () => {
    const teams = {
      LEG: {
        id: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
        name: "Legion",
      },
    };
    fs.writeFileSync(testCacheFile, JSON.stringify(teams, null, 2));

    const result = await resolveTeamId("leg", testCacheDir);
    expect(result).toBe("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee");
  });

  test("throws error when team key not in cache and no API key", async () => {
    const teams = {
      ENG: {
        id: "11111111-2222-3333-4444-555555555555",
        name: "Engineering",
      },
    };
    fs.writeFileSync(testCacheFile, JSON.stringify(teams, null, 2));

    await expect(resolveTeamId("LEG", testCacheDir)).rejects.toThrow("'LEG' is not a UUID");
  });

  test("throws error when no cache file and no API key", async () => {
    await expect(resolveTeamId("LEG", testCacheDir)).rejects.toThrow("'LEG' is not a UUID");
  });

  test("looks up team via API when cache miss and API key present", async () => {
    process.env.LINEAR_API_KEY = "test-api-key";

    const mockFetch = mock(async (url: string, options?: RequestInit) => {
      expect(url).toBe("https://api.linear.app/graphql");
      expect(options?.method).toBe("POST");

      return new Response(
        JSON.stringify({
          data: {
            teams: {
              nodes: [
                { id: "fetched-uuid-1234", key: "LEG", name: "Legion Team" },
                { id: "other-uuid", key: "ENG", name: "Engineering" },
              ],
            },
          },
        }),
        { status: 200 }
      );
    });

    globalThis.fetch = mockFetch as any;

    const result = await resolveTeamId("LEG", testCacheDir);
    expect(result).toBe("fetched-uuid-1234");
    expect(mockFetch).toHaveBeenCalledTimes(1);

    // Verify cache was written
    const cached = JSON.parse(fs.readFileSync(testCacheFile, "utf-8"));
    expect(cached.LEG.id).toBe("fetched-uuid-1234");
    expect(cached.ENG.id).toBe("other-uuid");
  });

  test("throws error when API returns no matching team", async () => {
    process.env.LINEAR_API_KEY = "test-api-key";

    globalThis.fetch = mock(async () => {
      return new Response(
        JSON.stringify({
          data: {
            teams: {
              nodes: [{ id: "some-uuid", key: "ENG", name: "Engineering" }],
            },
          },
        }),
        { status: 200 }
      );
    }) as any;

    await expect(resolveTeamId("NOTFOUND", testCacheDir)).rejects.toThrow(
      "Team 'NOTFOUND' not found. Available: ENG"
    );
  });

  test("throws error when API request fails", async () => {
    process.env.LINEAR_API_KEY = "test-api-key";

    globalThis.fetch = mock(async () => {
      throw new Error("Network error");
    }) as any;

    await expect(resolveTeamId("LEG", testCacheDir)).rejects.toThrow(
      "Failed to look up team 'LEG'"
    );
  });

  test("prefers cache over API when team key exists in cache", async () => {
    process.env.LINEAR_API_KEY = "test-api-key";

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
    globalThis.fetch = mockFetch as any;

    const result = await resolveTeamId("LEG", testCacheDir);
    expect(result).toBe("cached-uuid");
    expect(mockFetch).not.toHaveBeenCalled();
  });
});
