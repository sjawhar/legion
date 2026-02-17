/**
 * Async data fetching for Legion state collection.
 *
 * All I/O operations are async and can be composed with Promise.all.
 * Uses:
 * - fetch() for daemon HTTP API (worker detection)
 * - Bun.spawn for GitHub CLI (PR draft status)
 * - Retry logic with exponential backoff for GitHub API calls
 *
 * Ported from Python: src/legion/state/fetch.py
 */

import { LinearTracker } from "./backends/linear";
import type {
  FetchedIssueData,
  GitHubPRRef as GitHubPRRefType,
  LinearIssueRaw,
  ParsedIssue,
} from "./types";

// =============================================================================
// Types for Dependency Injection
// =============================================================================

/**
 * Result of running an external command.
 */
export interface CommandResult {
  stdout: string;
  stderr: string;
  exitCode: number;
}

/**
 * Protocol for running external commands (dependency injection for testing).
 */
export type CommandRunner = (cmd: string[]) => Promise<CommandResult>;

/**
 * Default command runner using Bun.spawn.
 */
export async function defaultRunner(cmd: string[]): Promise<CommandResult> {
  const proc = Bun.spawn(cmd, {
    stdout: "pipe",
    stderr: "pipe",
  });

  const killTimeout = setTimeout(() => {
    try {
      proc.kill();
    } catch {
      // Process may have already exited
    }
  }, 30_000); // 30s for gh api graphql
  const [stdout, stderr] = await Promise.all([
    new Response(proc.stdout).text(),
    new Response(proc.stderr).text(),
  ]);

  const exitCode = await proc.exited;
  clearTimeout(killTimeout);
  return { stdout, stderr, exitCode };
}

// =============================================================================
// GitHub API Error
// =============================================================================

/**
 * Raised when GitHub API calls fail after retries.
 */
export class GitHubAPIError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "GitHubAPIError";
  }
}

// =============================================================================
// HTTP Worker Detection
// =============================================================================

interface DaemonWorker {
  id: string;
  status?: string;
}

/**
 * Get live workers as {issue_id: {mode, status}} from daemon HTTP API.
 *
 * Workers are reported by the daemon's /workers endpoint.
 * Worker ID format: "ISSUE-ID-mode" (e.g., "ENG-21-implement").
 *
 * @param daemonUrl - Base URL of the daemon HTTP API
 * @returns Dict mapping issue_id (uppercase) to mode/status
 */
export async function getLiveWorkers(
  daemonUrl: string
): Promise<Record<string, { mode: string; status: string }>> {
  try {
    const response = await fetch(`${daemonUrl}/workers`, {
      signal: AbortSignal.timeout(5_000), // 5s — local daemon should respond fast
    });
    if (!response.ok) {
      return {};
    }

    const workers = (await response.json()) as DaemonWorker[];
    const result: Record<string, { mode: string; status: string }> = {};

    for (const worker of workers) {
      if (worker.status && worker.status !== "running" && worker.status !== "starting") {
        continue;
      }

      // Parse worker.id format: "ISSUE-ID-mode"
      // The mode is always the last segment after the last hyphen
      const lastDash = worker.id.lastIndexOf("-");
      if (lastDash <= 0) continue;

      const issueId = worker.id.substring(0, lastDash).toUpperCase();
      const mode = worker.id.substring(lastDash + 1);
      result[issueId] = { mode, status: worker.status ?? "running" };
    }

    return result;
  } catch {
    // Network error, daemon not running, etc.
    return {};
  }
}

// =============================================================================
// GitHub PR Draft Status Fetching
// =============================================================================

/**
 * Sleep for a given number of milliseconds.
 */
function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

/**
 * Fetch PR draft status for multiple issues in a single GraphQL query.
 *
 * Batches all PRs across all repositories into one API call.
 * Retries up to 3 times with exponential backoff on failure.
 *
 * @param prRefs - Dict mapping issue_id to GitHubPRRef
 * @param runner - Command runner for testing
 * @returns Dict mapping issue_id to draft status (true/false/null)
 * @throws GitHubAPIError if GraphQL query fails after retries
 */
export async function getPrDraftStatusBatch(
  prRefs: Record<string, GitHubPRRefType>,
  runner: CommandRunner = defaultRunner
): Promise<Record<string, boolean | null>> {
  if (Object.keys(prRefs).length === 0) {
    return {};
  }

  // Group by repository for query structure
  const byRepo = new Map<string, Array<[string, number]>>();
  for (const [issueId, ref] of Object.entries(prRefs)) {
    const key = `${ref.owner}/${ref.repo}`;
    if (!byRepo.has(key)) {
      byRepo.set(key, []);
    }
    byRepo.get(key)?.push([issueId, ref.number]);
  }

  // Build single GraphQL query for all repos and PRs
  // Maps: repoAlias -> [owner, repo], prAlias -> [issueId, prNumber]
  const repoAliasMap = new Map<string, [string, string]>();
  const prAliasMap = new Map<string, Map<string, [string, number]>>();

  const queryParts: string[] = [];
  let repoIdx = 0;
  for (const [repoKey, issuePrs] of byRepo) {
    const [owner, repo] = repoKey.split("/");
    const repoAlias = `repo${repoIdx}`;
    repoAliasMap.set(repoAlias, [owner, repo]);
    prAliasMap.set(repoAlias, new Map());

    const prParts: string[] = [];
    for (let prIdx = 0; prIdx < issuePrs.length; prIdx++) {
      const [issueId, prNumber] = issuePrs[prIdx];
      const prAlias = `pr${prIdx}`;
      prAliasMap.get(repoAlias)?.set(prAlias, [issueId, prNumber]);
      prParts.push(`${prAlias}: pullRequest(number: ${prNumber}) { isDraft }`);
    }

    queryParts.push(
      `${repoAlias}: repository(owner: "${owner}", name: "${repo}") { ${prParts.join(" ")} }`
    );
    repoIdx++;
  }

  const query = `query { ${queryParts.join(" ")} }`;

  // Retry loop with exponential backoff (3 attempts)
  const maxAttempts = 3;
  let lastError: GitHubAPIError = new GitHubAPIError("All retry attempts failed");

  for (let attempt = 0; attempt < maxAttempts; attempt++) {
    if (attempt > 0) {
      // Exponential backoff: 1s, 2s (min 1s, max 10s)
      const waitMs = Math.min(2 ** (attempt - 1) * 1000, 10000);
      await sleep(waitMs);
    }

    const { stdout, stderr, exitCode } = await runner([
      "gh",
      "api",
      "graphql",
      "-f",
      `query=${query}`,
    ]);

    if (exitCode !== 0) {
      lastError = new GitHubAPIError(`GraphQL query failed: ${stderr}`);
      continue;
    }

    let response: { data?: unknown };
    try {
      response = JSON.parse(stdout);
    } catch (e) {
      lastError = new GitHubAPIError(`Failed to parse GraphQL response: ${e}`);
      continue;
    }

    // Success - parse response
    const rawData = response.data;
    const dataObj: Record<string, unknown> =
      rawData !== null &&
      rawData !== undefined &&
      typeof rawData === "object" &&
      !Array.isArray(rawData)
        ? (rawData as Record<string, unknown>)
        : {};

    const result: Record<string, boolean | null> = {};

    for (const [repoAlias, [_owner, _repo]] of repoAliasMap) {
      const rawRepo = dataObj[repoAlias];
      const repoData: Record<string, unknown> =
        rawRepo !== null &&
        rawRepo !== undefined &&
        typeof rawRepo === "object" &&
        !Array.isArray(rawRepo)
          ? (rawRepo as Record<string, unknown>)
          : {};

      const prAliases = prAliasMap.get(repoAlias) ?? new Map();
      for (const [prAlias, [issueId]] of prAliases) {
        const rawPr = repoData[prAlias];
        const prData: { isDraft?: boolean } | null =
          rawPr !== null &&
          rawPr !== undefined &&
          typeof rawPr === "object" &&
          !Array.isArray(rawPr)
            ? (rawPr as { isDraft?: boolean })
            : null;

        if (prData !== null && "isDraft" in prData) {
          // Convert null to false for safety (null isDraft means not draft)
          result[issueId] = Boolean(prData.isDraft);
        } else {
          result[issueId] = null;
        }
      }
    }

    return result;
  }

  throw lastError;
}

// =============================================================================
// Issue Parsing
// =============================================================================

// =============================================================================
// Main Data Fetching
// =============================================================================

export async function enrichParsedIssues(
  parsedIssues: ParsedIssue[],
  daemonUrl: string,
  runner: CommandRunner = defaultRunner
): Promise<FetchedIssueData[]> {
  const prRefsForStatus: Record<string, GitHubPRRefType> = {};
  for (const p of parsedIssues) {
    if (p.needsPrStatus && p.prRef !== null) {
      prRefsForStatus[p.issueId] = p.prRef;
    }
  }

  let liveWorkers: Record<string, { mode: string; status: string }> = {};
  let prDraftMap: Record<string, boolean | null> = {};

  await Promise.all([
    (async () => {
      liveWorkers = await getLiveWorkers(daemonUrl);
    })(),
    (async () => {
      if (Object.keys(prRefsForStatus).length === 0) {
        return;
      }
      try {
        prDraftMap = await getPrDraftStatusBatch(prRefsForStatus, runner);
      } catch {
        for (const issueId of Object.keys(prRefsForStatus)) {
          prDraftMap[issueId] = null;
        }
      }
    })(),
  ]);

  return parsedIssues.map((issue) => {
    const workerInfo = liveWorkers[issue.issueId.toUpperCase()] ?? null;
    return {
      issueId: issue.issueId,
      status: issue.status,
      labels: issue.labels,
      hasPr: issue.hasPr,
      prIsDraft: prDraftMap[issue.issueId] ?? null,
      hasLiveWorker: workerInfo !== null,
      workerMode: workerInfo?.mode ?? null,
      workerStatus: workerInfo?.status ?? null,
      hasUserFeedback: issue.hasUserFeedback,
      hasUserInputNeeded: issue.hasUserInputNeeded,
      hasNeedsApproval: issue.hasNeedsApproval,
      hasHumanApproved: issue.hasHumanApproved,
      source: issue.source,
    };
  });
}

/**
 * Fetch all data for issues in parallel.
 *
 * All I/O operations run concurrently:
 * - Daemon HTTP API (for live workers)
 * - GitHub PR draft status (fetched via gh api graphql)
 *
 * @param linearIssues - Raw issue dicts from Linear API (legacy — use enrichParsedIssues for new code)
 * @param daemonUrl - Base URL of daemon HTTP API
 * @param runner - Command runner for testing
 * @returns List of fully fetched issue data
 */
export async function fetchAllIssueData(
  linearIssues: LinearIssueRaw[],
  daemonUrl: string,
  runner: CommandRunner = defaultRunner
): Promise<FetchedIssueData[]> {
  const parsedIssues = new LinearTracker().parseIssues(linearIssues);
  return enrichParsedIssues(parsedIssues, daemonUrl, runner);
}
