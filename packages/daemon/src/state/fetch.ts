import {
  CiStatus,
  type CiStatusLiteral,
  type GitHubPRRef as GitHubPRRefType,
  MergeableStatus,
  type MergeableStatusLiteral,
  ReviewState,
  type ReviewStateLiteral,
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

export interface CommandRunnerOptions {
  readonly env?: NodeJS.ProcessEnv;
}

/**
 * Protocol for running external commands (dependency injection for testing).
 */
export type CommandRunner = (
  cmd: string[],
  options?: CommandRunnerOptions
) => Promise<CommandResult>;

export type OwnerCommandRunnerOptionsProvider = (owner: string) => Promise<CommandRunnerOptions>;

/**
 * Default command runner using Bun.spawn.
 */
export async function defaultRunner(
  cmd: string[],
  options?: CommandRunnerOptions
): Promise<CommandResult> {
  const proc = Bun.spawn(cmd, {
    env: options?.env,
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
// GitHub PR Draft Status Fetching
// =============================================================================

async function sleep(ms: number): Promise<void> {
  const delay = Promise.withResolvers<void>();
  setTimeout(delay.resolve, ms);
  await delay.promise;
}

// =============================================================================
// CI Status Mapping
// =============================================================================

/**
 * Map GitHub statusCheckRollup state to CiStatusLiteral.
 *
 * GitHub GraphQL statusCheckRollup.state values:
 * - SUCCESS → "passing"
 * - FAILURE, ERROR → "failing"
 * - PENDING, EXPECTED → "pending"
 * - null or unknown → null
 */
export function mapCiRollupState(state: string | null | undefined): CiStatusLiteral | null {
  if (state === null || state === undefined) {
    return null;
  }
  switch (state) {
    case "SUCCESS":
      return CiStatus.PASSING;
    case "FAILURE":
    case "ERROR":
      return CiStatus.FAILING;
    case "PENDING":
    case "EXPECTED":
      return CiStatus.PENDING;
    default:
      return null;
  }
}

/**
 * Map GitHub GraphQL MergeableState enum to MergeableStatusLiteral.
 *
 * GitHub GraphQL PullRequest.mergeable values:
 * - MERGEABLE -> "mergeable"
 * - CONFLICTING -> "conflicting"
 * - UNKNOWN -> "unknown" (GitHub hasn't computed yet)
 * - null or unrecognized -> null
 */
export function mapMergeableState(state: string | null | undefined): MergeableStatusLiteral | null {
  if (state === null || state === undefined) {
    return null;
  }
  switch (state) {
    case "MERGEABLE":
      return MergeableStatus.MERGEABLE;
    case "CONFLICTING":
      return MergeableStatus.CONFLICTING;
    case "UNKNOWN":
      return MergeableStatus.UNKNOWN;
    default:
      return null;
  }
}

/**
 * Fetch PR review state for multiple issues in a single GraphQL query.
 *
 * Queries `latestReviews(first:1)` on each PR to determine whether the most
 * recent review approved or requested changes. Replaces the previous draft-based
 * signaling — with separate GitHub Apps for impl and review roles, native
 * reviews are the canonical signal.
 *
 * Batches all PRs across all repositories into one API call by default. When
 * owner-scoped runner options are supplied, queries each owner separately.
 * Retries up to 3 times with exponential backoff on failure.
 *
 * @param prRefs - Dict mapping issue_id to GitHubPRRef
 * @param runner - Command runner for testing
 * @returns Dict mapping issue_id to review state (approved/changes_requested/null)
 * @throws GitHubAPIError if GraphQL query fails after retries
 */
export async function getPrReviewStateBatch(
  prRefs: Record<string, GitHubPRRefType>,
  runner: CommandRunner = defaultRunner,
  runnerOptionsForOwner?: OwnerCommandRunnerOptionsProvider
): Promise<Record<string, ReviewStateLiteral | null>> {
  if (!runnerOptionsForOwner) {
    return getPrReviewStateBatchWithOptions(prRefs, runner);
  }

  const batches = new Map<string, { owner: string; refs: Record<string, GitHubPRRefType> }>();
  for (const [issueId, ref] of Object.entries(prRefs)) {
    const ownerKey = ref.owner.toLowerCase();
    const batch = batches.get(ownerKey);
    if (batch) {
      batch.refs[issueId] = ref;
    } else {
      batches.set(ownerKey, { owner: ref.owner, refs: { [issueId]: ref } });
    }
  }

  const result: Record<string, ReviewStateLiteral | null> = {};
  for (const batch of batches.values()) {
    try {
      Object.assign(
        result,
        await getPrReviewStateBatchWithOptions(
          batch.refs,
          runner,
          await runnerOptionsForOwner(batch.owner),
          1
        )
      );
    } catch {
      for (const issueId of Object.keys(batch.refs)) {
        result[issueId] = null;
      }
    }
  }
  return result;
}

async function getPrReviewStateBatchWithOptions(
  prRefs: Record<string, GitHubPRRefType>,
  runner: CommandRunner,
  runnerOptions?: CommandRunnerOptions,
  maxAttempts: number = 3
): Promise<Record<string, ReviewStateLiteral | null>> {
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
      prParts.push(
        `${prAlias}: pullRequest(number: ${prNumber}) { latestReviews(first: 1) { nodes { state } } }`
      );
    }

    queryParts.push(
      `${repoAlias}: repository(owner: "${owner}", name: "${repo}") { ${prParts.join(" ")} }`
    );
    repoIdx++;
  }

  const query = `query { ${queryParts.join(" ")} }`;

  // Retry loop with exponential backoff (configurable attempts)
  let lastError: GitHubAPIError = new GitHubAPIError("All retry attempts failed");

  for (let attempt = 0; attempt < maxAttempts; attempt++) {
    if (attempt > 0) {
      // Exponential backoff: 1s, 2s (min 1s, max 10s)
      const waitMs = Math.min(2 ** (attempt - 1) * 1000, 10000);
      await sleep(waitMs);
    }

    const { stdout, stderr, exitCode } = await runner(
      ["gh", "api", "graphql", "-f", `query=${query}`],
      runnerOptions
    );

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

    const result: Record<string, ReviewStateLiteral | null> = {};

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
        const rawPr = repoData[prAlias] as
          | { latestReviews?: { nodes?: Array<{ state?: string }> } }
          | null
          | undefined;

        const reviewState = rawPr?.latestReviews?.nodes?.[0]?.state ?? null;
        if (reviewState === "APPROVED") {
          result[issueId] = ReviewState.APPROVED;
        } else if (reviewState === "CHANGES_REQUESTED") {
          result[issueId] = ReviewState.CHANGES_REQUESTED;
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
// GitHub CI Status Fetching
// =============================================================================

/**
 * Combined CI and mergeable status for a PR.
 */
interface CiAndMergeStatus {
  ciStatus: CiStatusLiteral | null;
  mergeableStatus: MergeableStatusLiteral | null;
}

/**
 * Fetch CI and mergeable status for multiple PRs in a single GraphQL query.
 *
 * Uses statusCheckRollup on the latest commit of each PR, plus the mergeable field.
 * Batches all PRs across all repositories into one API call by default. When
 * owner-scoped runner options are supplied, queries each owner separately.
 * Retries up to 3 times with exponential backoff on failure.
 *
 * @param prRefs - Dict mapping issue_id to GitHubPRRef
 * @param runner - Command runner for testing
 * @returns Dict mapping issue_id to CI and mergeable status
 * @throws GitHubAPIError if GraphQL query fails after retries
 */
export async function getCiStatusBatch(
  prRefs: Record<string, GitHubPRRefType>,
  runner: CommandRunner = defaultRunner,
  runnerOptionsForOwner?: OwnerCommandRunnerOptionsProvider
): Promise<Record<string, CiAndMergeStatus>> {
  if (!runnerOptionsForOwner) {
    return getCiStatusBatchWithOptions(prRefs, runner);
  }

  const batches = new Map<string, { owner: string; refs: Record<string, GitHubPRRefType> }>();
  for (const [issueId, ref] of Object.entries(prRefs)) {
    const ownerKey = ref.owner.toLowerCase();
    const batch = batches.get(ownerKey);
    if (batch) {
      batch.refs[issueId] = ref;
    } else {
      batches.set(ownerKey, { owner: ref.owner, refs: { [issueId]: ref } });
    }
  }

  const result: Record<string, CiAndMergeStatus> = {};
  for (const batch of batches.values()) {
    try {
      Object.assign(
        result,
        await getCiStatusBatchWithOptions(
          batch.refs,
          runner,
          await runnerOptionsForOwner(batch.owner),
          1
        )
      );
    } catch {
      for (const issueId of Object.keys(batch.refs)) {
        result[issueId] = { ciStatus: null, mergeableStatus: null };
      }
    }
  }
  return result;
}

async function getCiStatusBatchWithOptions(
  prRefs: Record<string, GitHubPRRefType>,
  runner: CommandRunner,
  runnerOptions?: CommandRunnerOptions,
  maxAttempts: number = 3
): Promise<Record<string, CiAndMergeStatus>> {
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
      prParts.push(
        `${prAlias}: pullRequest(number: ${prNumber}) { mergeable commits(last: 1) { nodes { commit { statusCheckRollup { state } } } } }`
      );
    }

    queryParts.push(
      `${repoAlias}: repository(owner: "${owner}", name: "${repo}") { ${prParts.join(" ")} }`
    );
    repoIdx++;
  }

  const query = `query { ${queryParts.join(" ")} }`;

  // Retry loop with exponential backoff (configurable attempts)
  let lastError: GitHubAPIError = new GitHubAPIError("All retry attempts failed");

  for (let attempt = 0; attempt < maxAttempts; attempt++) {
    if (attempt > 0) {
      const waitMs = Math.min(2 ** (attempt - 1) * 1000, 10000);
      await sleep(waitMs);
    }

    const { stdout, stderr, exitCode } = await runner(
      ["gh", "api", "graphql", "-f", `query=${query}`],
      runnerOptions
    );

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

    const result: Record<string, CiAndMergeStatus> = {};

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
        const rawPr = repoData[prAlias] as Record<string, unknown> | null | undefined;
        if (
          rawPr === null ||
          rawPr === undefined ||
          typeof rawPr !== "object" ||
          Array.isArray(rawPr)
        ) {
          result[issueId] = { ciStatus: null, mergeableStatus: null };
          continue;
        }

        // Navigate: pr.commits.nodes[0].commit.statusCheckRollup.state
        const commits = rawPr.commits as { nodes?: unknown[] } | null | undefined;
        const nodes = commits?.nodes;
        if (!Array.isArray(nodes) || nodes.length === 0) {
          result[issueId] = { ciStatus: null, mergeableStatus: null };
          continue;
        }

        const firstNode = nodes[0] as {
          commit?: { statusCheckRollup?: { state?: string | null } | null };
        } | null;
        const rollupState = firstNode?.commit?.statusCheckRollup?.state ?? null;
        result[issueId] = {
          ciStatus: mapCiRollupState(rollupState),
          mergeableStatus: mapMergeableState(
            typeof rawPr === "object" && rawPr !== null && "mergeable" in rawPr
              ? ((rawPr as { mergeable?: string | null }).mergeable ?? null)
              : null
          ),
        };
      }
    }

    return result;
  }

  throw lastError;
}
