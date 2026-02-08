/**
 * State script CLI entry point.
 *
 * Reads Linear JSON from stdin, processes through fetchAllIssueData
 * and buildCollectedState, outputs JSON to stdout.
 *
 * Usage:
 *   echo '<linear-json>' | bun run src/state/cli.ts --team-id <uuid> --daemon-url <url>
 */

import { buildCollectedState } from "./decision";
import { type CommandRunner, fetchAllIssueData } from "./fetch";
import { CollectedState, type LinearIssueRaw } from "./types";

// =============================================================================
// Arg Parsing
// =============================================================================

export interface CliArgs {
  teamId: string;
  daemonUrl: string;
}

/**
 * Parse CLI arguments.
 *
 * @param args - Raw argument strings (without node/script path)
 * @returns Parsed arguments
 * @throws Error if required arguments are missing
 */
export function parseArgs(args: string[]): CliArgs {
  let teamId: string | null = null;
  let daemonUrl: string | null = null;

  for (let i = 0; i < args.length; i++) {
    if (args[i] === "--team-id" && i + 1 < args.length) {
      teamId = args[i + 1];
      i++;
    } else if (args[i] === "--daemon-url" && i + 1 < args.length) {
      daemonUrl = args[i + 1];
      i++;
    }
  }

  if (!teamId) {
    throw new Error("Missing required argument: --team-id <uuid>");
  }
  if (!daemonUrl) {
    throw new Error("Missing required argument: --daemon-url <url>");
  }

  return { teamId, daemonUrl };
}

// =============================================================================
// Pipeline
// =============================================================================

/**
 * Run the state collection pipeline.
 *
 * @param linearIssues - Raw Linear issues (parsed from stdin JSON)
 * @param teamId - Linear team UUID
 * @param daemonUrl - Daemon HTTP API URL
 * @param runner - Optional command runner for testing
 * @returns JSON string of CollectedState
 */
export async function runPipeline(
  linearIssues: LinearIssueRaw[],
  teamId: string,
  daemonUrl: string,
  runner?: CommandRunner
): Promise<string> {
  const issuesData = await fetchAllIssueData(linearIssues, daemonUrl, runner);
  const state = buildCollectedState(issuesData, teamId);
  return JSON.stringify(CollectedState.toDict(state));
}

// =============================================================================
// Main (stdin → stdout)
// =============================================================================

async function main(): Promise<void> {
  const args = parseArgs(Bun.argv.slice(2));

  // Read stdin
  const stdinText = await new Response(Bun.stdin.stream()).text();
  let linearIssues: LinearIssueRaw[];
  try {
    linearIssues = JSON.parse(stdinText);
  } catch (e) {
    process.stderr.write(`Failed to parse stdin JSON: ${e}\n`);
    process.exit(1);
  }

  const output = await runPipeline(linearIssues, args.teamId, args.daemonUrl);
  process.stdout.write(`${output}\n`);
}

// Only run main when executed directly
if (import.meta.main) {
  main().catch((e) => {
    process.stderr.write(`Error: ${e}\n`);
    process.exit(1);
  });
}
