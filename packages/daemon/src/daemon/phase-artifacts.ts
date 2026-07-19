import { type CommandResult, type CommandRunner, defaultRunner } from "../state/fetch";

// allow: SIZE_OK — batch construction and decoding share the exact alias map.
export type ArtifactResolution = "resolved" | "unresolvable";

export interface IssueRef {
  readonly issueId: string;
  readonly status: string;
  readonly source: { readonly owner: string; readonly repo: string; readonly number: number };
  readonly prRef: { readonly owner: string; readonly repo: string; readonly number: number } | null;
}

export interface PhaseArtifactResolutions {
  readonly hasNonDraftPr: ArtifactResolution;
  readonly headSha: ArtifactResolution;
  readonly testerCheckOnHead: ArtifactResolution;
  readonly architectCheckOnHead: ArtifactResolution;
  readonly nativeReviewOnHead: ArtifactResolution;
  readonly autoMergeEnabledOrMerged: ArtifactResolution;
  readonly merged: ArtifactResolution;
  readonly planHandoff: ArtifactResolution;
}

export interface PhaseArtifacts {
  readonly hasNonDraftPr: boolean;
  readonly headSha: string | null;
  readonly testerCheckOnHead: "success" | "failure" | null;
  readonly architectCheckOnHead: "success" | "failure" | null;
  readonly nativeReviewOnHead: "approved" | "changes_requested" | null;
  readonly autoMergeEnabledOrMerged: boolean;
  readonly merged: boolean;
  readonly planHandoff: "present" | "absent" | "unresolvable";
  readonly resolved: PhaseArtifactResolutions;
}

export interface FetchPhaseArtifactsDeps {
  readonly reviewerAppId: number;
  readonly reviewerAppLogin: string;
  readonly runner?: CommandRunner;
}

export interface PhaseArtifactError {
  readonly issueId: string;
  readonly message: string;
}

export interface PhaseArtifactBatch {
  readonly artifacts: Readonly<Record<string, PhaseArtifacts>>;
  readonly errors: readonly PhaseArtifactError[];
}

interface QueryAlias {
  readonly ref: IssueRef;
  readonly repoAlias: string;
  readonly prAlias: string;
}

interface QueryGroup {
  readonly owner: string;
  readonly repo: string;
  readonly refs: IssueRef[];
}

const UNRESOLVABLE: ArtifactResolution = "unresolvable";

export function unresolvablePhaseArtifacts(): PhaseArtifacts {
  return {
    hasNonDraftPr: false,
    headSha: null,
    testerCheckOnHead: null,
    architectCheckOnHead: null,
    nativeReviewOnHead: null,
    autoMergeEnabledOrMerged: false,
    merged: false,
    planHandoff: "unresolvable",
    resolved: {
      hasNonDraftPr: UNRESOLVABLE,
      headSha: UNRESOLVABLE,
      testerCheckOnHead: UNRESOLVABLE,
      architectCheckOnHead: UNRESOLVABLE,
      nativeReviewOnHead: UNRESOLVABLE,
      autoMergeEnabledOrMerged: UNRESOLVABLE,
      merged: UNRESOLVABLE,
      planHandoff: UNRESOLVABLE,
    },
  };
}

export function noPrPhaseArtifacts(): PhaseArtifacts {
  const artifacts = unresolvablePhaseArtifacts();
  return {
    ...artifacts,
    resolved: {
      ...artifacts.resolved,
      hasNonDraftPr: "resolved",
      merged: "resolved",
    },
  };
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function recordAt(value: unknown, key: string): Record<string, unknown> | null {
  return isRecord(value) && isRecord(value[key]) ? value[key] : null;
}

function recordsAt(value: unknown, key: string): Record<string, unknown>[] | null {
  const container = recordAt(value, key);
  return container && Array.isArray(container.nodes) ? container.nodes.filter(isRecord) : null;
}

function stringAt(value: unknown, key: string): string | null {
  return isRecord(value) && typeof value[key] === "string" ? value[key] : null;
}

function booleanAt(value: unknown, key: string): boolean | null {
  return isRecord(value) && typeof value[key] === "boolean" ? value[key] : null;
}

function numberAt(value: unknown, key: string): number | null {
  return isRecord(value) && typeof value[key] === "number" ? value[key] : null;
}

function graphQlErrorForAlias(response: unknown, alias: QueryAlias): string | null {
  const errors = isRecord(response) && Array.isArray(response.errors) ? response.errors : [];
  for (const error of errors) {
    const path = isRecord(error) && Array.isArray(error.path) ? error.path : null;
    if (path?.[0] === alias.repoAlias && path[1] === alias.prAlias) {
      return stringAt(error, "message");
    }
  }
  return null;
}

function queryFor(refs: readonly IssueRef[]): {
  readonly query: string;
  readonly aliases: QueryAlias[];
} {
  const groups = new Map<string, QueryGroup>();
  for (const ref of refs) {
    if (!ref.prRef) continue;
    const key = `${ref.prRef.owner}/${ref.prRef.repo}`;
    const group = groups.get(key);
    if (group) {
      group.refs.push(ref);
    } else {
      groups.set(key, { owner: ref.prRef.owner, repo: ref.prRef.repo, refs: [ref] });
    }
  }

  const aliases: QueryAlias[] = [];
  const repositories = [...groups.values()].map((group, repoIndex) => {
    const pullRequests = group.refs.map((ref, prIndex) => {
      const prAlias = `pr${prIndex}`;
      aliases.push({ ref, repoAlias: `repo${repoIndex}`, prAlias });
      return `${prAlias}: pullRequest(number: ${ref.prRef?.number}) { isDraft headRefOid merged autoMergeRequest { enabledAt } latestReviews(first: 100) { nodes { state commit { oid } author { login } } } commits(last: 1) { nodes { commit { oid statusCheckRollup { contexts(first: 100) { nodes { ... on CheckRun { name conclusion app { databaseId } } } } } } } } }`;
    });
    return `repo${repoIndex}: repository(owner: ${JSON.stringify(group.owner)}, name: ${JSON.stringify(group.repo)}) { ${pullRequests.join(" ")} }`;
  });
  return { query: `query { ${repositories.join(" ")} }`, aliases };
}

function checkConclusion(
  pullRequest: Record<string, unknown>,
  headSha: string | null,
  name: "tester" | "architect",
  reviewerAppId: number
): { readonly value: "success" | "failure" | null; readonly resolved: ArtifactResolution } {
  if (!headSha) return { value: null, resolved: UNRESOLVABLE };
  const commits = recordsAt(pullRequest, "commits");
  const commit = commits?.[0] ? recordAt(commits[0], "commit") : null;
  if (!commit || stringAt(commit, "oid") !== headSha)
    return { value: null, resolved: UNRESOLVABLE };
  const contexts = recordsAt(recordAt(commit, "statusCheckRollup"), "contexts");
  if (!contexts) return { value: null, resolved: UNRESOLVABLE };
  const conclusion = stringAt(
    contexts.find(
      (context) =>
        stringAt(context, "name") === name &&
        numberAt(recordAt(recordAt(context, "checkSuite"), "app"), "databaseId") === reviewerAppId
    ),
    "conclusion"
  );
  if (conclusion === "SUCCESS") return { value: "success", resolved: "resolved" };
  if (conclusion === "FAILURE") return { value: "failure", resolved: "resolved" };
  return { value: null, resolved: "resolved" };
}

function reviewOnHead(
  pullRequest: Record<string, unknown>,
  headSha: string | null,
  reviewerAppLogin: string
): { readonly value: PhaseArtifacts["nativeReviewOnHead"]; readonly resolved: ArtifactResolution } {
  if (!headSha) return { value: null, resolved: UNRESOLVABLE };
  const reviews = recordsAt(pullRequest, "latestReviews");
  if (!reviews) return { value: null, resolved: UNRESOLVABLE };

  let approved = false;
  let hasStaleReview = false;
  for (const review of reviews) {
    if (stringAt(recordAt(review, "commit"), "oid") !== headSha) {
      hasStaleReview = true;
      continue;
    }
    const state = stringAt(review, "state");
    if (state !== "APPROVED" && state !== "CHANGES_REQUESTED") continue;

    // Bot.databaseId identifies the bot user, not the CheckRun publisher App ID. Pin the
    // reviewer's exact Actor.login alongside reviewerAppId so native reviews are attributable.
    const authorLogin = stringAt(recordAt(review, "author"), "login");
    if (!authorLogin) return { value: null, resolved: UNRESOLVABLE };
    if (authorLogin !== reviewerAppLogin) continue;
    if (state === "CHANGES_REQUESTED") return { value: "changes_requested", resolved: "resolved" };
    approved = true;
  }
  return approved
    ? { value: "approved", resolved: "resolved" }
    : { value: null, resolved: hasStaleReview ? UNRESOLVABLE : "resolved" };
}

function artifactsFor(
  pullRequest: Record<string, unknown>,
  deps: FetchPhaseArtifactsDeps
): PhaseArtifacts {
  const isDraft = booleanAt(pullRequest, "isDraft");
  const headSha = stringAt(pullRequest, "headRefOid");
  const merged = booleanAt(pullRequest, "merged");
  const tester = checkConclusion(pullRequest, headSha, "tester", deps.reviewerAppId);
  const architect = checkConclusion(pullRequest, headSha, "architect", deps.reviewerAppId);
  const review = reviewOnHead(pullRequest, headSha, deps.reviewerAppLogin);
  const autoMergeRequest = recordAt(pullRequest, "autoMergeRequest");
  const autoMergeResolved = Object.hasOwn(pullRequest, "autoMergeRequest");
  return {
    hasNonDraftPr: isDraft === false,
    headSha,
    testerCheckOnHead: tester.value,
    architectCheckOnHead: architect.value,
    nativeReviewOnHead: review.value,
    autoMergeEnabledOrMerged: merged === true || autoMergeRequest !== null,
    merged: merged === true,
    planHandoff: "unresolvable",
    resolved: {
      hasNonDraftPr: isDraft === null ? UNRESOLVABLE : "resolved",
      headSha: headSha ? "resolved" : UNRESOLVABLE,
      testerCheckOnHead: tester.resolved,
      architectCheckOnHead: architect.resolved,
      nativeReviewOnHead: review.resolved,
      autoMergeEnabledOrMerged: autoMergeResolved ? "resolved" : UNRESOLVABLE,
      merged: merged === null ? UNRESOLVABLE : "resolved",
      planHandoff: UNRESOLVABLE,
    },
  };
}

function failedBatch(refs: readonly IssueRef[], message: string): PhaseArtifactBatch {
  const artifacts: Record<string, PhaseArtifacts> = {};
  const errors: PhaseArtifactError[] = [];
  for (const ref of refs) {
    if (!ref.prRef) {
      artifacts[ref.issueId] = noPrPhaseArtifacts();
      continue;
    }
    artifacts[ref.issueId] = unresolvablePhaseArtifacts();
    errors.push({ issueId: ref.issueId, message });
  }
  return { artifacts, errors };
}

export async function fetchPhaseArtifactsBatch(
  refs: readonly IssueRef[],
  deps: FetchPhaseArtifactsDeps
): Promise<PhaseArtifactBatch> {
  const { query, aliases } = queryFor(refs);
  if (aliases.length === 0) return failedBatch(refs, "");

  let result: CommandResult;
  try {
    result = await (deps.runner ?? defaultRunner)(["gh", "api", "graphql", "-f", `query=${query}`]);
  } catch (error) {
    return failedBatch(
      refs,
      error instanceof Error ? error.message : "GitHub phase-artifact query failed"
    );
  }
  let response: unknown;
  try {
    response = JSON.parse(result.stdout);
  } catch (error) {
    if (result.exitCode !== 0)
      return failedBatch(refs, `GitHub phase-artifact query failed: ${result.stderr}`);
    return failedBatch(
      refs,
      error instanceof Error
        ? `GitHub phase-artifact query returned invalid JSON: ${error.message}`
        : "GitHub phase-artifact query returned invalid JSON"
    );
  }
  const data = recordAt(response, "data");
  if (result.exitCode !== 0 && !data)
    return failedBatch(refs, `GitHub phase-artifact query failed: ${result.stderr}`);
  if (!data) return failedBatch(refs, "GitHub phase-artifact query returned no data");

  const artifacts: Record<string, PhaseArtifacts> = {};
  const errors: PhaseArtifactError[] = [];
  for (const ref of refs) {
    if (!ref.prRef) artifacts[ref.issueId] = noPrPhaseArtifacts();
  }
  for (const alias of aliases) {
    const pullRequest = recordAt(recordAt(data, alias.repoAlias), alias.prAlias);
    if (!pullRequest) {
      artifacts[alias.ref.issueId] = unresolvablePhaseArtifacts();
      errors.push({
        issueId: alias.ref.issueId,
        message:
          result.exitCode !== 0
            ? (graphQlErrorForAlias(response, alias) ??
              "GitHub phase-artifact query omitted PR data")
            : "GitHub phase-artifact query omitted PR data",
      });
      continue;
    }
    artifacts[alias.ref.issueId] = artifactsFor(pullRequest, deps);
  }
  return { artifacts, errors };
}

export async function fetchPhaseArtifacts(
  ref: IssueRef,
  deps: FetchPhaseArtifactsDeps
): Promise<PhaseArtifacts> {
  const { artifacts } = await fetchPhaseArtifactsBatch([ref], deps);
  return artifacts[ref.issueId] ?? unresolvablePhaseArtifacts();
}
