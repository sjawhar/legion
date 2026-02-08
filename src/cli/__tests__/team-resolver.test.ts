import { describe, test, expect, beforeEach, afterEach, mock } from "bun:test";
import { resolveTeamId } from "../team-resolver";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

describe("resolveTeamId", () => {
  const testCacheDir = path.join(os.tmpdir(), "legion-test-cache");
  const testCacheFile = path.join(testCacheDir, "teams.json");
  const originalHome = process.env.HOME;

  beforeEach(() => {
    // Create test cache directory
    if (!fs.existsSync(testCacheDir)) {
      fs.mkdirSync(testCacheDir, { recursive: true });
    }
    // Override HOME to use test cache
    process.env.HOME = os.tmpdir();
  });

  afterEach(() => {
    // Clean up test cache
    if (fs.existsSync(testCacheFile)) {
      fs.unlinkSync(testCacheFile);
    }
    if (fs.existsSync(testCacheDir)) {
      fs.rmdirSync(testCacheDir);
    }
    // Restore HOME
    process.env.HOME = originalHome;
    delete process.env.LINEAR_API_KEY;
  });

  test("returns UUID as-is when given valid UUID", async () => {
    const uuid = "12345678-1234-1234-1234-123456789abc";
    const result = await resolveTeamId(uuid);
    expect(result).toBe(uuid);
  });

  test("returns UUID as-is when given valid UUID (uppercase)", async () => {
    const uuid = "12345678-1234-1234-1234-123456789ABC";
    const result = await resolveTeamId(uuid);
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

    const result = await resolveTeamId("LEG");
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

    const result = await resolveTeamId("leg");
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

    await expect(resolveTeamId("LEG")).rejects.toThrow(
      "'LEG' is not a UUID"
    );
  });

  test("throws error when no cache file and no API key", async () => {
    await expect(resolveTeamId("LEG")).rejects.toThrow(
      "'LEG' is not a UUID"
    );
  });

  test("looks up team via API when cache miss and API key present", async () => {
    process.env.LINEAR_API_KEY = "test-api-key";

    // Mock fetch
    const mockFetch = mock(async (url: string, options?: RequestInit) => {
      expect(url).toBe("https://api.linear.app/graphql");
      expect(options?.method).toBe("POST");
      const body = JSON.parse(options?.body as string);
      expect(body.variables.key).toBe("LEG");

      return new Response(
        JSON.stringify({
          data: {
            team: {
              id: "fetched-uuid-1234",
              name: "Legion Team",
            },
          },
        }),
        { status: 200 }
      );
    });

    globalThis.fetch = mockFetch as any;

    const result = await resolveTeamId("LEG");
    expect(result).toBe("fetched-uuid-1234");
    expect(mockFetch).toHaveBeenCalledTimes(1);
  });

  test("throws error when API returns no team", async () => {
    process.env.LINEAR_API_KEY = "test-api-key";

    globalThis.fetch = mock(async () => {
      return new Response(
        JSON.stringify({
          data: {},
        }),
        { status: 200 }
      );
    }) as any;

    await expect(resolveTeamId("NOTFOUND")).rejects.toThrow(
      "Team 'NOTFOUND' not found in Linear"
    );
  });

  test("throws error when API request fails", async () => {
    process.env.LINEAR_API_KEY = "test-api-key";

    globalThis.fetch = mock(async () => {
      throw new Error("Network error");
    }) as any;

    await expect(resolveTeamId("LEG")).rejects.toThrow(
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

    // Mock fetch should not be called
    const mockFetch = mock(async () => {
      throw new Error("Should not call API when cache hit");
    });
    globalThis.fetch = mockFetch as any;

    const result = await resolveTeamId("LEG");
    expect(result).toBe("cached-uuid");
    expect(mockFetch).not.toHaveBeenCalled();
  });
});
