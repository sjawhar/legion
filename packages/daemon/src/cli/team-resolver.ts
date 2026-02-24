import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { LinearTeamsResponseSchema } from "../daemon/schemas";

// UUID regex pattern
const UUID_PATTERN = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

interface TeamInfo {
  id: string;
  name: string;
}

interface TeamsCache {
  [key: string]: TeamInfo;
}

/**
 * Resolve a team reference to a stable ID.
 *
 * For GitHub backend, the team ref (e.g., "owner/project-number") is already the ID.
 * For Linear backend, resolves team keys (e.g., "LEG") to UUIDs via cache or API.
 *
 * @param teamRef - Team identifier: UUID, team key (Linear), or owner/project-number (GitHub)
 * @param options - Optional cache directory and backend
 * @returns The team ID
 * @throws Error if team cannot be resolved
 */
export async function resolveTeamId(
  teamRef: string,
  options?: string | { cacheDir?: string; backend?: string }
): Promise<string> {
  const cacheDir = typeof options === "string" ? options : options?.cacheDir;
  const backend = typeof options === "string" ? undefined : options?.backend;

  // GitHub backend: team ref is already the ID (owner/project-number)
  if (backend === "github") {
    return teamRef;
  }

  if (UUID_PATTERN.test(teamRef)) {
    return teamRef;
  }

  const resolvedCacheDir = cacheDir ?? path.join(os.homedir(), ".legion");
  const cacheFile = path.join(resolvedCacheDir, "teams.json");

  if (fs.existsSync(cacheFile)) {
    try {
      const teams: TeamsCache = JSON.parse(fs.readFileSync(cacheFile, "utf-8"));
      const keyUpper = teamRef.toUpperCase();
      if (keyUpper in teams) {
        const team = teams[keyUpper];
        console.log(`Using cached: ${teamRef} → ${team.name} (${team.id})`);
        return team.id;
      }
    } catch {}
  }

  const apiKey = process.env.LINEAR_API_TOKEN;
  if (apiKey) {
    return await lookupTeamViaApi(teamRef, apiKey, resolvedCacheDir);
  }

  throw new Error(
    `'${teamRef}' is not a UUID.\n` +
      `Run 'legion teams' to cache team mappings, or set LINEAR_API_TOKEN.`
  );
}

/**
 * Look up team via Linear GraphQL API.
 */
async function lookupTeamViaApi(
  teamRef: string,
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
    variables: { key: teamRef.toUpperCase() },
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

    const parsed = LinearTeamsResponseSchema.safeParse(await response.json());
    if (!parsed.success) {
      throw new Error(`Linear API returned invalid response: ${parsed.error.message}`);
    }

    if (parsed.data.errors && parsed.data.errors.length > 0) {
      throw new Error(parsed.data.errors[0]?.message ?? "Linear API returned GraphQL errors");
    }

    if (!parsed.data.data) {
      throw new Error("Linear API returned null data");
    }

    const teamsByKey: TeamsCache = {};
    for (const node of parsed.data.data.teams.nodes) {
      teamsByKey[node.key.toUpperCase()] = { id: node.id, name: node.name };
    }

    const cacheFile = path.join(cacheDir, "teams.json");
    let existingCache: TeamsCache = {};
    try {
      existingCache = JSON.parse(fs.readFileSync(cacheFile, "utf-8")) as TeamsCache;
    } catch {}

    const mergedCache: TeamsCache = { ...existingCache, ...teamsByKey };

    try {
      fs.mkdirSync(cacheDir, { recursive: true });
      fs.writeFileSync(cacheFile, JSON.stringify(mergedCache, null, 2));
    } catch (error) {
      console.warn(`Failed to write team cache to ${cacheFile}: ${String(error)}`);
    }

    const keyUpper = teamRef.toUpperCase();
    const team = parsed.data.data.teams.nodes.find((node) => node.key.toUpperCase() === keyUpper);
    if (!team) {
      const availableKeys = Object.keys(mergedCache).sort();
      const availableKeysMessage = availableKeys.length > 0 ? availableKeys.join(", ") : "(none)";
      throw new Error(
        `Team '${teamRef}' not found in Linear. Available team keys: ${availableKeysMessage}`
      );
    }

    console.log(`Resolved: ${teamRef} → ${team.name} (${team.id})`);
    return team.id;
  } catch (error) {
    throw new Error(
      `Failed to look up team '${teamRef}': ${error instanceof Error ? error.message : String(error)}`
    );
  }
}
