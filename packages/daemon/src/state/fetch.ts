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

function recordValue(value: unknown): Record<string, unknown> | undefined {
  return typeof value === "object" && value !== null && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : undefined;
}

async function runGraphqlQuery(
  query: string,
  variables: Record<string, string>,
  runner: CommandRunner,
  runnerOptions: CommandRunnerOptions | undefined,
  maxAttempts: number,
  allowEmptyData: boolean = false
): Promise<Record<string, unknown>> {
  let lastError = new GitHubAPIError("All GraphQL query retry attempts failed");
  for (let attempt = 0; attempt < maxAttempts; attempt += 1) {
    if (attempt > 0) {
      await sleep(Math.min(2 ** (attempt - 1) * 1000, 10000));
    }
    const command = ["gh", "api", "graphql", "-f", `query=${query}`];
    for (const [name, value] of Object.entries(variables)) {
      command.push("-f", `${name}=${value}`);
    }
    const { stdout, stderr, exitCode } = await runner(command, runnerOptions);
    if (exitCode !== 0) {
      lastError = new GitHubAPIError(`GraphQL query failed: ${stderr}`);
      continue;
    }
    try {
      const response = recordValue(JSON.parse(stdout));
      const data = recordValue(response?.data);
      if (data) return data;
      if (allowEmptyData) return {};
      lastError = new GitHubAPIError("GitHub returned GraphQL data in an invalid shape");
    } catch (error) {
      lastError = new GitHubAPIError(`Failed to parse GraphQL response: ${error}`);
    }
  }
  throw lastError;
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

  const dataObj = await runGraphqlQuery(query, {}, runner, runnerOptions, maxAttempts, true);
  const result: Record<string, ReviewStateLiteral | null> = {};

  for (const [repoAlias] of repoAliasMap) {
    const repoData = recordValue(dataObj[repoAlias]);
    const prAliases = prAliasMap.get(repoAlias) ?? new Map();
    for (const [prAlias, [issueId]] of prAliases) {
      const rawPr = recordValue(repoData?.[prAlias]);
      const latestReviews = recordValue(rawPr?.latestReviews);
      const nodes = latestReviews?.nodes;
      const reviewState = Array.isArray(nodes) ? recordValue(nodes[0])?.state : undefined;
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
  updatedAt: string | null;
  latestCheckRunId: number | null;
}

export interface CiFetchFailure {
  owner: string;
  error: string;
}

export type CiFetchResult = CiAndMergeStatus | CiFetchFailure;

export function isCiFetchFailure(result: CiFetchResult): result is CiFetchFailure {
  return "error" in result;
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

function latestCheckRunId(nodes: readonly unknown[]): number | null {
  let latest: number | null = null;
  for (const node of nodes) {
    const databaseId = recordValue(node)?.databaseId;
    if (
      typeof databaseId === "number" &&
      Number.isSafeInteger(databaseId) &&
      databaseId > 0 &&
      (latest === null || databaseId > latest)
    ) {
      latest = databaseId;
    }
  }
  return latest;
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
  headSha: string,
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
    const query = `query($after: String!) { repository(owner: "${ref.owner}", name: "${ref.repo}") { object(oid: "${headSha}") { ... on Commit { statusCheckRollup { contexts(first: 100, after: $after) { pageInfo { hasNextPage endCursor } nodes { ... on CheckRun { name conclusion databaseId } ... on StatusContext { name: context statusConclusion: state } } } } } } } }`;
    const data = await runGraphqlQuery(
      query,
      { after: page.endCursor },
      runner,
      runnerOptions,
      maxAttempts
    );
    const rollup = recordValue(
      recordValue(recordValue(data.repository)?.object)?.statusCheckRollup
    );
    if (!rollup) {
      throw new GitHubAPIError("GitHub returned an invalid paginated check rollup");
    }
    page = contextsPage(rollup);
    nodes.push(...page.nodes);
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
): Promise<Record<string, CiFetchResult>> {
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

  const result: Record<string, CiFetchResult> = {};
  for (const batch of batches.values()) {
    try {
      Object.assign(
        result,
        await getCiStatusBatchWithOptions(
          batch.refs,
          runner,
          await runnerOptionsForOwner(batch.owner)
        )
      );
    } catch (error) {
      const message = error instanceof Error ? error.message : String(error);
      for (const issueId of Object.keys(batch.refs)) {
        result[issueId] = { owner: batch.owner, error: message };
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
        `${prAlias}: pullRequest(number: ${prNumber}) { state updatedAt mergeable commits(last: 1) { nodes { commit { oid statusCheckRollup { state contexts(first: 100) { pageInfo { hasNextPage endCursor } nodes { ... on CheckRun { name conclusion databaseId } ... on StatusContext { name: context statusConclusion: state } } } } } } } }`
      );
    }

    queryParts.push(
      `${repoAlias}: repository(owner: "${owner}", name: "${repo}") { ${prParts.join(" ")} }`
    );
    repoIdx++;
  }

  const query = `query { ${queryParts.join(" ")} }`;

  const dataObj = await runGraphqlQuery(query, {}, runner, runnerOptions, maxAttempts);
  const result: Record<string, CiAndMergeStatus> = {};

  for (const [repoAlias, [owner, repo]] of repoAliasMap) {
    const repoData = recordValue(dataObj[repoAlias]);
    const prAliases = prAliasMap.get(repoAlias) ?? new Map();
    for (const [prAlias, [issueId, prNumber]] of prAliases) {
      const rawPr = recordValue(repoData?.[prAlias]);
      if (!rawPr) {
        result[issueId] = {
          ciStatus: null,
          mergeableStatus: null,
          headSha: null,
          isOpen: false,
          updatedAt: null,
          latestCheckRunId: null,
        };
        continue;
      }

      const updatedAt = typeof rawPr.updatedAt === "string" ? rawPr.updatedAt : null;
      const commits = recordValue(rawPr.commits);
      const firstNode = Array.isArray(commits?.nodes) ? commits.nodes[0] : undefined;
      const commit = recordValue(recordValue(firstNode)?.commit);
      if (!commit) {
        result[issueId] = {
          ciStatus: null,
          mergeableStatus: null,
          headSha: null,
          isOpen: rawPr.state === "OPEN",
          updatedAt,
          latestCheckRunId: null,
        };
        continue;
      }

      const headSha = typeof commit.oid === "string" ? commit.oid : null;
      const rollup = recordValue(commit.statusCheckRollup);
      const rollupState = rollup?.state;
      const ciStatus = mapCiRollupState(
        typeof rollupState === "string" || rollupState === null ? rollupState : null
      );
      const initialContexts = contextsPage(rollup);
      let contextNodes = initialContexts.nodes;
      if (ciStatus === CiStatus.FAILING && initialContexts.hasNextPage) {
        if (!headSha) {
          throw new GitHubAPIError(
            `GitHub returned a paginated check rollup without an oid for ${issueId}`
          );
        }
        contextNodes = await fetchRemainingContextNodes(
          initialContexts,
          { owner, repo, number: prNumber },
          headSha,
          runner,
          runnerOptions,
          maxAttempts
        );
      }
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
        updatedAt,
        latestCheckRunId: latestCheckRunId(contextNodes),
      };
    }
  }

  return result;
}
