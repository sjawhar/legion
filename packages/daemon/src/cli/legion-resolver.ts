import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { resolveLegionPaths } from "../daemon/paths";
import { LinearTeamsResponseSchema } from "../daemon/schemas";

// UUID regex pattern
const UUID_PATTERN = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

interface LegionInfo {
  id: string;
  name: string;
}

interface LegionsCache {
  [key: string]: LegionInfo;
}

/**
 * Resolve a legion reference to a stable ID.
 *
 * For GitHub backend, the legion ref (e.g., "owner/project-number") is already the ID.
 * For Linear backend, resolves legion keys (e.g., "LEG") to UUIDs via cache or API.
 *
 * @param legionRef - Legion identifier: UUID, team key (Linear), or owner/project-number (GitHub)
 * @param options - Optional cache directory and backend
 * @returns The legion ID
 * @throws Error if legion cannot be resolved
 */
export async function resolveLegionId(
  legionRef: string,
  options?: string | { cacheDir?: string; backend?: string }
): Promise<string> {
  const cacheDir = typeof options === "string" ? options : options?.cacheDir;
  const backend = typeof options === "string" ? undefined : options?.backend;

  // GitHub backend: legion ref is already the ID (owner/project-number)
  if (backend === "github") {
    return legionRef;
  }

  if (UUID_PATTERN.test(legionRef)) {
    return legionRef;
  }

  const resolvedCacheDir = cacheDir ?? resolveLegionPaths(process.env, os.homedir()).stateDir;
  const cacheFile = path.join(resolvedCacheDir, "project-cache.json");

  if (fs.existsSync(cacheFile)) {
    try {
      const raw: unknown = JSON.parse(fs.readFileSync(cacheFile, "utf-8"));
      if (typeof raw !== "object" || raw === null || Array.isArray(raw)) {
        throw new Error("Invalid cache format");
      }
      const legions = raw as LegionsCache;
      const keyUpper = legionRef.toUpperCase();
      if (keyUpper in legions) {
        const legion = legions[keyUpper];
        console.log(`Using cached: ${legionRef} → ${legion.name} (${legion.id})`);
        return legion.id;
      }
    } catch {}
  }

  const apiKey = process.env.LINEAR_API_TOKEN;
  if (apiKey) {
    return await lookupLegionViaApi(legionRef, apiKey, resolvedCacheDir);
  }

  throw new Error(
    `'${legionRef}' is not a UUID.\n` +
      `Run 'legion teams' to cache legion mappings, or set LINEAR_API_TOKEN.`
  );
}

/**
 * Look up legion via Linear GraphQL API.
 */
async function lookupLegionViaApi(
  legionRef: string,
  apiKey: string,
  cacheDir: string
): Promise<string> {
  const query = `
    query GetTeamByKey($key: String!) {
      teams(filter: { key: { eq: $key } }) {
        nodes {
          id
          key
          name
        }
      }
    }
  `;

  const payload = JSON.stringify({
    query,
    variables: { key: legionRef.toUpperCase() },
  });

  try {
    const response = await fetch("https://api.linear.app/graphql", {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        Authorization: apiKey,
      },
      body: payload,
      signal: AbortSignal.timeout(15_000), // 15s for external API
    });

    if (!response.ok) {
      throw new Error(`Linear API returned ${response.status} ${response.statusText}`);
    }

    let jsonBody: unknown;
    try {
      jsonBody = await response.json();
    } catch {
      throw new Error(`Linear API returned non-JSON response (status ${response.status})`);
    }
    const parsed = LinearTeamsResponseSchema.safeParse(jsonBody);
    if (!parsed.success) {
      throw new Error(`Linear API returned invalid response: ${parsed.error.message}`);
    }

    if (parsed.data.errors && parsed.data.errors.length > 0) {
      throw new Error(parsed.data.errors[0]?.message ?? "Linear API returned GraphQL errors");
    }

    if (!parsed.data.data) {
      throw new Error("Linear API returned null data");
    }

    const legionsByKey: LegionsCache = {};
    for (const node of parsed.data.data.teams.nodes) {
      legionsByKey[node.key.toUpperCase()] = { id: node.id, name: node.name };
    }

    const cacheFile = path.join(cacheDir, "project-cache.json");
    let existingCache: LegionsCache = {};
    try {
      const raw: unknown = JSON.parse(fs.readFileSync(cacheFile, "utf-8"));
      if (typeof raw === "object" && raw !== null && !Array.isArray(raw)) {
        existingCache = raw as LegionsCache;
      }
    } catch {}

    const mergedCache: LegionsCache = { ...existingCache, ...legionsByKey };

    try {
      fs.mkdirSync(cacheDir, { recursive: true });
      fs.writeFileSync(cacheFile, JSON.stringify(mergedCache, null, 2));
    } catch (error) {
      console.warn(`Failed to write legion cache to ${cacheFile}: ${String(error)}`);
    }

    const keyUpper = legionRef.toUpperCase();
    const legion = parsed.data.data.teams.nodes.find((node) => node.key.toUpperCase() === keyUpper);
    if (!legion) {
      const availableKeys = Object.keys(mergedCache).sort();
      const availableKeysMessage = availableKeys.length > 0 ? availableKeys.join(", ") : "(none)";
      throw new Error(
        `Legion '${legionRef}' not found in Linear. Available legion keys: ${availableKeysMessage}`
      );
    }

    console.log(`Resolved: ${legionRef} → ${legion.name} (${legion.id})`);
    return legion.id;
  } catch (error) {
    throw new Error(
      `Failed to look up legion '${legionRef}': ${error instanceof Error ? error.message : String(error)}`
    );
  }
}
