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
  readonly cwd?: string;
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
    cwd: options?.cwd,
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
 * recent review approved or requested changes. Native reviews are the canonical
 * merge-gate signal for separate GitHub Apps on implementation and review roles.
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
    if (runner === defaultRunner) {
      throw new Error("Owner-scoped GitHub App runner options are required for GraphQL reads");
    }
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
 * Combined CI, mergeable, and failed-check status for a PR.
 */
export interface CiAndMergeStatus {
  ciStatus: CiStatusLiteral | null;
  mergeableStatus: MergeableStatusLiteral | null;
  failingChecks?: string[];
  headSha: string | null;
  isOpen: boolean;
}

const FAILING_CHECK_CONCLUSIONS: Record<string, true> = {
  ACTION_REQUIRED: true,
  CANCELLED: true,
  ERROR: true,
  FAILURE: true,
  STALE: true,
  STARTUP_FAILURE: true,
  TIMED_OUT: true,
};

function failingCheckNames(nodes: readonly unknown[]): string[] {
  const failing = new Set<string>();
  for (const node of nodes) {
    if (typeof node !== "object" || node === null || Array.isArray(node) || !("name" in node)) {
      continue;
    }
    const conclusion =
      "conclusion" in node
        ? node.conclusion
        : "statusConclusion" in node
          ? node.statusConclusion
          : undefined;
    if (
      typeof node.name === "string" &&
      typeof conclusion === "string" &&
      FAILING_CHECK_CONCLUSIONS[conclusion]
    ) {
      failing.add(node.name);
    }
  }
  return [...failing];
}

interface RollupContextsPage {
  nodes: unknown[];
  hasNextPage: boolean;
  endCursor: string | null;
}

function contextsPage(rollup: Record<string, unknown> | undefined): RollupContextsPage {
  const rawContexts = rollup?.contexts;
  if (typeof rawContexts !== "object" || rawContexts === null || Array.isArray(rawContexts)) {
    return { nodes: [], hasNextPage: false, endCursor: null };
  }
  const contexts = rawContexts as Record<string, unknown>;
  const rawPageInfo = contexts.pageInfo;
  const pageInfo: Record<string, unknown> | undefined =
    typeof rawPageInfo === "object" && rawPageInfo !== null && !Array.isArray(rawPageInfo)
      ? (rawPageInfo as Record<string, unknown>)
      : undefined;
  return {
    nodes: Array.isArray(contexts.nodes) ? contexts.nodes : [],
    hasNextPage: pageInfo?.hasNextPage === true,
    endCursor: typeof pageInfo?.endCursor === "string" ? pageInfo.endCursor : null,
  };
}

async function fetchRemainingContextNodes(
  initialPage: RollupContextsPage,
  ref: GitHubPRRefType,
  runner: CommandRunner,
  runnerOptions: CommandRunnerOptions | undefined,
  maxAttempts: number
): Promise<unknown[]> {
  const nodes = [...initialPage.nodes];
  let page = initialPage;
  while (page.hasNextPage) {
    if (!page.endCursor) {
      throw new GitHubAPIError("GitHub returned a paginated check rollup without an end cursor");
    }
    const query = `query($after: String!) { repository(owner: "${ref.owner}", name: "${ref.repo}") { pullRequest(number: ${ref.number}) { commits(last: 1) { nodes { commit { statusCheckRollup { contexts(first: 100, after: $after) { pageInfo { hasNextPage endCursor } nodes { ... on CheckRun { name conclusion } ... on StatusContext { name: context statusConclusion: state } } } } } } } } }`;
    let nextPage: RollupContextsPage | undefined;
    let lastError: GitHubAPIError = new GitHubAPIError("All context page retry attempts failed");
    for (let attempt = 0; attempt < maxAttempts; attempt += 1) {
      if (attempt > 0) {
        await sleep(Math.min(2 ** (attempt - 1) * 1000, 10000));
      }
      const { stdout, stderr, exitCode } = await runner(
        ["gh", "api", "graphql", "-f", `query=${query}`, "-f", `after=${page.endCursor}`],
        runnerOptions
      );
      if (exitCode !== 0) {
        lastError = new GitHubAPIError(`GraphQL check contexts query failed: ${stderr}`);
        continue;
      }
      try {
        const response = JSON.parse(stdout) as { data?: unknown };
        const data: Record<string, unknown> | undefined =
          typeof response.data === "object" &&
          response.data !== null &&
          !Array.isArray(response.data)
            ? (response.data as Record<string, unknown>)
            : undefined;
        const repository: Record<string, unknown> | undefined =
          data &&
          typeof data.repository === "object" &&
          data.repository !== null &&
          !Array.isArray(data.repository)
            ? (data.repository as Record<string, unknown>)
            : undefined;
        const pullRequest: Record<string, unknown> | undefined =
          repository &&
          typeof repository.pullRequest === "object" &&
          repository.pullRequest !== null &&
          !Array.isArray(repository.pullRequest)
            ? (repository.pullRequest as Record<string, unknown>)
            : undefined;
        const commits: Record<string, unknown> | undefined =
          pullRequest &&
          typeof pullRequest.commits === "object" &&
          pullRequest.commits !== null &&
          !Array.isArray(pullRequest.commits)
            ? (pullRequest.commits as Record<string, unknown>)
            : undefined;
        const firstCommit =
          commits && Array.isArray(commits.nodes) && commits.nodes.length > 0
            ? commits.nodes[0]
            : undefined;
        const commit =
          typeof firstCommit === "object" &&
          firstCommit !== null &&
          !Array.isArray(firstCommit) &&
          "commit" in firstCommit &&
          typeof firstCommit.commit === "object" &&
          firstCommit.commit !== null &&
          !Array.isArray(firstCommit.commit)
            ? firstCommit.commit
            : undefined;
        const rollup =
          commit &&
          "statusCheckRollup" in commit &&
          typeof commit.statusCheckRollup === "object" &&
          commit.statusCheckRollup !== null &&
          !Array.isArray(commit.statusCheckRollup)
            ? commit.statusCheckRollup
            : undefined;
        if (!rollup) {
          lastError = new GitHubAPIError("GitHub returned an invalid paginated check rollup");
          continue;
        }
        nextPage = contextsPage(rollup);
        break;
      } catch (error) {
        lastError = new GitHubAPIError(`Failed to parse paginated check rollup: ${error}`);
      }
    }
    if (!nextPage) throw lastError;
    nodes.push(...nextPage.nodes);
    page = nextPage;
  }
  return nodes;
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
    if (runner === defaultRunner) {
      throw new Error("Owner-scoped GitHub App runner options are required for GraphQL reads");
    }
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
        result[issueId] = { ciStatus: null, mergeableStatus: null, headSha: null, isOpen: false };
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
        `${prAlias}: pullRequest(number: ${prNumber}) { state mergeable commits(last: 1) { nodes { commit { oid statusCheckRollup { state contexts(first: 100) { pageInfo { hasNextPage endCursor } nodes { ... on CheckRun { name conclusion } ... on StatusContext { name: context statusConclusion: state } } } } } } } }`
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

    for (const [repoAlias, [owner, repo]] of repoAliasMap) {
      const rawRepo = dataObj[repoAlias];
      const repoData: Record<string, unknown> =
        rawRepo !== null &&
        rawRepo !== undefined &&
        typeof rawRepo === "object" &&
        !Array.isArray(rawRepo)
          ? (rawRepo as Record<string, unknown>)
          : {};

      const prAliases = prAliasMap.get(repoAlias) ?? new Map();
      for (const [prAlias, [issueId, prNumber]] of prAliases) {
        const rawPr = repoData[prAlias] as Record<string, unknown> | null | undefined;
        if (
          rawPr === null ||
          rawPr === undefined ||
          typeof rawPr !== "object" ||
          Array.isArray(rawPr)
        ) {
          result[issueId] = {
            ciStatus: null,
            mergeableStatus: null,
            headSha: null,
            isOpen: false,
          };
          continue;
        }

        // Navigate: pr.commits.nodes[0].commit.statusCheckRollup.state
        const commits = rawPr.commits;
        if (
          typeof commits !== "object" ||
          commits === null ||
          Array.isArray(commits) ||
          !("nodes" in commits) ||
          !Array.isArray(commits.nodes) ||
          commits.nodes.length === 0
        ) {
          result[issueId] = {
            ciStatus: null,
            mergeableStatus: null,
            headSha: null,
            isOpen: rawPr.state === "OPEN",
          };
          continue;
        }

        const firstNode = commits.nodes[0];
        const commit =
          typeof firstNode === "object" &&
          firstNode !== null &&
          !Array.isArray(firstNode) &&
          "commit" in firstNode &&
          typeof firstNode.commit === "object" &&
          firstNode.commit !== null &&
          !Array.isArray(firstNode.commit)
            ? firstNode.commit
            : undefined;
        const headSha = typeof commit?.oid === "string" ? commit.oid : null;
        const rollup =
          commit &&
          "statusCheckRollup" in commit &&
          typeof commit.statusCheckRollup === "object" &&
          commit.statusCheckRollup !== null &&
          !Array.isArray(commit.statusCheckRollup)
            ? commit.statusCheckRollup
            : undefined;
        const rollupState = rollup && "state" in rollup ? rollup.state : null;
        const ciStatus = mapCiRollupState(
          typeof rollupState === "string" || rollupState === null ? rollupState : null
        );
        const initialContexts = contextsPage(rollup);
        const contextNodes = initialContexts.hasNextPage
          ? await fetchRemainingContextNodes(
              initialContexts,
              { owner, repo, number: prNumber },
              runner,
              runnerOptions,
              maxAttempts
            )
          : initialContexts.nodes;
        const mergeable = rawPr.mergeable;
        result[issueId] = {
          ciStatus,
          mergeableStatus: mapMergeableState(
            typeof mergeable === "string" || mergeable === null ? mergeable : null
          ),
          ...(ciStatus === CiStatus.FAILING
            ? { failingChecks: failingCheckNames(contextNodes) }
            : {}),
          headSha,
          isOpen: rawPr.state === "OPEN",
        };
      }
    }

    return result;
  }

  throw lastError;
}
