import fs from "node:fs";
import os from "node:os";
import path from "node:path";

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
 * Resolve a team reference to a UUID.
 *
 * @param teamRef - Either a UUID or a team key (e.g., "LEG")
 * @param cacheDir - Optional cache directory (defaults to ~/.legion)
 * @returns The team UUID
 * @throws Error if team cannot be resolved
 */
export async function resolveTeamId(teamRef: string, cacheDir?: string): Promise<string> {
  if (UUID_PATTERN.test(teamRef)) {
    return teamRef;
  }

  const resolvedCacheDir = cacheDir ?? path.join(os.homedir(), ".legion");
  const cacheFile = path.join(resolvedCacheDir, "teams.json");

  if (fs.existsSync(cacheFile)) {
    const teams: TeamsCache = JSON.parse(fs.readFileSync(cacheFile, "utf-8"));
    const keyUpper = teamRef.toUpperCase();
    if (keyUpper in teams) {
      const team = teams[keyUpper];
      console.log(`Using cached: ${teamRef} → ${team.name} (${team.id})`);
      return team.id;
    }
  }

  const apiKey = process.env.LINEAR_API_KEY;
  if (apiKey) {
    return await lookupTeamViaApi(teamRef, apiKey, resolvedCacheDir);
  }

  throw new Error(
    `'${teamRef}' is not a UUID.\n` +
      `Run 'legion teams' to cache team mappings, or set LINEAR_API_KEY.`
  );
}

/**
 * Look up team via Linear GraphQL API.
 */
async function lookupTeamViaApi(
  teamRef: string,
  apiKey: string,
  cacheDir?: string
): Promise<string> {
  const query = `{ teams { nodes { id key name } } }`;

  try {
    const response = await fetch("https://api.linear.app/graphql", {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        Authorization: apiKey,
      },
      body: JSON.stringify({ query }),
      signal: AbortSignal.timeout(15_000),
    });

    if (!response.ok) {
      throw new Error(`Linear API returned ${response.status} ${response.statusText}`);
    }

    const data = (await response.json()) as {
      data?: { teams?: { nodes: Array<{ id: string; key: string; name: string }> } };
    };
    const nodes = data?.data?.teams?.nodes;
    if (!nodes) {
      throw new Error("Unexpected Linear API response");
    }

    // Cache all teams for future lookups
    const resolvedCacheDir = cacheDir ?? path.join(os.homedir(), ".legion");
    fs.mkdirSync(resolvedCacheDir, { recursive: true });
    const cache: TeamsCache = {};
    for (const node of nodes) {
      cache[node.key] = { id: node.id, name: node.name };
    }
    fs.writeFileSync(path.join(resolvedCacheDir, "teams.json"), JSON.stringify(cache, null, 2));

    const keyUpper = teamRef.toUpperCase();
    const team = nodes.find((n) => n.key === keyUpper);
    if (!team) {
      throw new Error(
        `Team '${teamRef}' not found. Available: ${nodes.map((n) => n.key).join(", ")}`
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
