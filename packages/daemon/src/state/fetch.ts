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

import {
  createParsedIssue,
  type FetchedIssueData,
  GitHubPRRef,
  type GitHubPRRef as GitHubPRRefType,
  IssueStatus,
  type LinearIssueRaw,
  type LinearLabelsContainer,
  type ParsedIssue,
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

  const [stdout, stderr] = await Promise.all([
    new Response(proc.stdout).text(),
    new Response(proc.stderr).text(),
  ]);

  const exitCode = await proc.exited;
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
 * Get live workers as {issue_id: mode} from daemon HTTP API.
 *
 * Workers are reported by the daemon's /workers endpoint.
 * Worker ID format: "ISSUE-ID-mode" (e.g., "ENG-21-implement").
 *
 * @param daemonUrl - Base URL of the daemon HTTP API
 * @returns Dict mapping issue_id (uppercase) to mode
 */
export async function getLiveWorkers(daemonUrl: string): Promise<Record<string, string>> {
  try {
    const response = await fetch(`${daemonUrl}/workers`);
    if (!response.ok) {
      return {};
    }

    const workers = (await response.json()) as DaemonWorker[];
    const result: Record<string, string> = {};

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
      result[issueId] = mode;
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
  let lastError: GitHubAPIError | null = null;

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

      for (const [prAlias, [issueId]] of prAliasMap.get(repoAlias)!) {
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

  throw lastError!;
}

// =============================================================================
// Issue Parsing
// =============================================================================

/**
 * Parse Linear API response into structured data.
 *
 * Handles both MCP format (labels as string[]) and GraphQL format (labels.nodes).
 *
 * @param linearIssues - Raw issue dicts from Linear API
 * @returns List of parsed issues with normalized data
 */
export function parseLinearIssues(linearIssues: LinearIssueRaw[]): ParsedIssue[] {
  const parsed: ParsedIssue[] = [];

  for (const issue of linearIssues) {
    const issueId = issue.identifier ?? "";
    if (!issueId) {
      continue;
    }

    // Extract and normalize status
    // Linear MCP returns "status" as string, but raw API might return "state.name"
    let rawStatus: string = issue.status ?? "";
    if (!rawStatus) {
      const stateObj = issue.state;
      rawStatus = stateObj?.name ?? "";
    }
    const status = IssueStatus.normalize(rawStatus);

    // Extract labels
    // Linear MCP returns "labels" as list of strings, raw API might return "labels.nodes"
    const labelsRaw = issue.labels;
    let labels: string[] = [];

    if (labelsRaw !== null && labelsRaw !== undefined) {
      if (typeof labelsRaw === "object" && !Array.isArray(labelsRaw)) {
        // Raw API format: {"nodes": [{"name": "label1"}, ...]}
        const container = labelsRaw as LinearLabelsContainer;
        const nodes = container.nodes ?? [];
        if (Array.isArray(nodes)) {
          labels = nodes
            .filter(
              (node): node is { name: string } =>
                typeof node === "object" &&
                node !== null &&
                typeof (node as { name?: unknown }).name === "string" &&
                Boolean((node as { name: string }).name)
            )
            .map((node) => node.name);
        }
      } else if (Array.isArray(labelsRaw)) {
        // MCP format: ["label1", "label2"] - filter out empty/non-string values
        labels = labelsRaw.filter((x): x is string => typeof x === "string" && x !== "");
      }
    }

    // Extract PR reference from attachments
    let prRef: GitHubPRRefType | null = null;
    let attachments = issue.attachments ?? [];
    if (!Array.isArray(attachments)) {
      attachments = [];
    }
    for (const attachment of attachments) {
      if (typeof attachment === "object" && attachment !== null) {
        const url = attachment.url ?? "";
        if (url.includes("github.com") && url.includes("/pull/")) {
          prRef = GitHubPRRef.fromUrl(url);
          if (prRef) {
            break;
          }
        }
      }
    }

    parsed.push(createParsedIssue(issueId, status, labels, prRef));
  }

  return parsed;
}

// =============================================================================
// Main Data Fetching
// =============================================================================

/**
 * Fetch all data for issues in parallel.
 *
 * All I/O operations run concurrently:
 * - Daemon HTTP API (for live workers)
 * - GitHub PR draft status (fetched via gh api graphql)
 *
 * @param linearIssues - Raw issue dicts from Linear API
 * @param daemonUrl - Base URL of daemon HTTP API
 * @param runner - Command runner for testing
 * @returns List of fully fetched issue data
 */
export async function fetchAllIssueData(
  linearIssues: LinearIssueRaw[],
  daemonUrl: string,
  runner: CommandRunner = defaultRunner
): Promise<FetchedIssueData[]> {
  // Phase 1: Parse issues (sync, fast)
  const parsedIssues = parseLinearIssues(linearIssues);

  // Identify PRs that need draft status lookup
  const prRefsForStatus: Record<string, GitHubPRRefType> = {};
  for (const p of parsedIssues) {
    if (p.needsPrStatus && p.prRef !== null) {
      prRefsForStatus[p.issueId] = p.prRef;
    }
  }

  // Phase 2: Fetch everything in parallel
  let liveWorkers: Record<string, string> = {};
  let prDraftMap: Record<string, boolean | null> = {};

  const fetchWorkers = async () => {
    liveWorkers = await getLiveWorkers(daemonUrl);
  };

  const fetchPrDraftStatusSafe = async () => {
    if (Object.keys(prRefsForStatus).length === 0) {
      return;
    }
    try {
      prDraftMap = await getPrDraftStatusBatch(prRefsForStatus, runner);
    } catch {
      // GitHub API failed - set all PRs to null (couldn't check)
      for (const issueId of Object.keys(prRefsForStatus)) {
        prDraftMap[issueId] = null;
      }
    }
  };

  await Promise.all([fetchWorkers(), fetchPrDraftStatusSafe()]);

  // Phase 3: Build results
  const results: FetchedIssueData[] = [];

  for (const issue of parsedIssues) {
    const hasLiveWorker = issue.issueId.toUpperCase() in liveWorkers;
    const prIsDraft: boolean | null = prDraftMap[issue.issueId] ?? null;

    results.push({
      issueId: issue.issueId,
      status: issue.status,
      labels: issue.labels,
      hasPr: issue.hasPr,
      prIsDraft,
      hasLiveWorker,
      hasUserFeedback: issue.hasUserFeedback,
      hasUserInputNeeded: issue.hasUserInputNeeded,
      hasNeedsApproval: issue.hasNeedsApproval,
      hasHumanApproved: issue.hasHumanApproved,
    });
  }

  return results;
}
