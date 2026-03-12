/**
 * State script CLI entry point.
 *
 * Legacy CLI entry point. Reads issue JSON from stdin, processes through
 * fetchAllIssueData and buildCollectedState, outputs JSON to stdout.
 * Superseded by POST /state/collect on the daemon.
 *
 * Usage:
 *   echo '<issue-json>' | bun run packages/daemon/src/state/cli.ts --team-id <id> --daemon-url <url>
 */

import { buildCollectedState } from "./decision";
import { type CommandRunner, fetchAllIssueData } from "./fetch";
import { CollectedState, type LinearIssueRaw } from "./types";

// =============================================================================
// Arg Parsing
// =============================================================================

export interface CliArgs {
  legionId: string;
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
  let legionId: string | null = null;
  let daemonUrl: string | null = null;

  for (let i = 0; i < args.length; i++) {
    if (args[i] === "--team-id" && i + 1 < args.length) {
      legionId = args[i + 1];
      i++;
    } else if (args[i] === "--daemon-url" && i + 1 < args.length) {
      daemonUrl = args[i + 1];
      i++;
    }
  }

  if (!legionId) {
    throw new Error("Missing required argument: --team-id <uuid>");
  }
  if (!daemonUrl) {
    throw new Error("Missing required argument: --daemon-url <url>");
  }

  return { legionId, daemonUrl };
}

// =============================================================================
// Pipeline
// =============================================================================

/**
 * Run the state collection pipeline.
 *
 * @param linearIssues - Raw issues (parsed from stdin JSON)
 * @param legionId - Team/project identifier
 * @param daemonUrl - Daemon HTTP API URL
 * @param runner - Optional command runner for testing
 * @returns JSON string of CollectedState
 */
export async function runPipeline(
  linearIssues: LinearIssueRaw[],
  legionId: string,
  daemonUrl: string,
  runner?: CommandRunner
): Promise<string> {
  const issuesData = await fetchAllIssueData(linearIssues, daemonUrl, runner);
  const state = buildCollectedState(issuesData, legionId);
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

  const output = await runPipeline(linearIssues, args.legionId, args.daemonUrl);
  process.stdout.write(`${output}\n`);
}

// Only run main when executed directly
if (import.meta.main) {
  main().catch((e) => {
    process.stderr.write(`Error: ${e}\n`);
    process.exit(1);
  });
}
