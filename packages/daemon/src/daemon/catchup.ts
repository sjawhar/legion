import { type IssueKey, isLegionRole, type LegionRole } from "@legion/contracts";
import type { CommandRunner, CommandRunnerOptions } from "../state/fetch";
import { buildRoleEnv, modeToRole, type TokenManager } from "./github-apps";
import type { LegionState, PrState } from "./legion-state";
import type { LegionEventPayload } from "./reducers";

type JsonRecord = Record<string, unknown>;

type CiVerdict = "green" | "red" | "pending";
const WORKER_MODE: Record<LegionRole, string> = {
  architect: "architect",
  planner: "plan",
  implementer: "implement",
  tester: "test",
  reviewer: "review",
  merger: "merge",
};

export interface CatchupOverseerPayload extends LegionEventPayload {
  type: "catchup-overseer";
  gates: Record<IssueKey, { designAskId?: string; designApproved?: string }>;
  childCounts: Record<IssueKey, { total: number; open: number; closed: number }>;
  prVerdicts: Record<
    string,
    {
      issue: IssueKey;
      sha: string;
      ci: CiVerdict;
      review: "approved" | "changes_requested" | "pending";
      failing?: string[];
      fixAttempts: number;
    }
  >;
  /** Phases that finished with no live architect holder to deliver `phase-complete` to
   * (`state.phases[issue].completed`, set by `handlePhaseComplete`), replayed here so a
   * resurrected or reconnecting architect learns them instead of losing them. */
  phaseCompletions: Array<{ issue: IssueKey; role: LegionRole; summary: string; at: string }>;
}

export type CatchupUnhandled =
  | {
      kind: "comment" | "review-comment";
      id: number;
      occurredAt: string;
      author: string;
      body: string;
      url: string;
    }
  | {
      kind: "review";
      id: number;
      occurredAt: string;
      author: string;
      state: string;
      body: string;
      url: string;
    };

export interface CatchupWorkerPayload extends LegionEventPayload {
  type: "catchup-worker";
  unhandled: CatchupUnhandled[];
}

export interface WorkerCatchupDeps {
  runner: CommandRunner;
  tokenManager: Pick<TokenManager, "getToken">;
  repo: `${string}/${string}`;
}

interface Artifact {
  repo: `${string}/${string}`;
  number: number;
}

function asRecord(value: unknown): JsonRecord | undefined {
  return typeof value === "object" && value !== null && !Array.isArray(value)
    ? (value as JsonRecord)
    : undefined;
}

function stringValue(value: unknown): string | undefined {
  return typeof value === "string" ? value : undefined;
}

function numberValue(value: unknown): number | undefined {
  return typeof value === "number" && Number.isSafeInteger(value) ? value : undefined;
}

function stateTree(state: LegionState, tree: IssueKey): Set<IssueKey> {
  if (!state.issues[tree]) throw new Error(`Unknown Legion tree: ${tree}`);
  const issues = new Set<IssueKey>();
  const pending = [tree];
  while (pending.length > 0) {
    const issue = pending.pop();
    if (!issue || issues.has(issue)) continue;
    issues.add(issue);
    for (const child of state.issues[issue]?.children ?? []) pending.push(child);
  }
  return issues;
}

function ciVerdict(pr: PrState): CiVerdict {
  return pr.verdict ?? "pending";
}

export async function overseerCatchup(s: LegionState, tree: IssueKey): Promise<LegionEventPayload> {
  const issues = stateTree(s, tree);
  const gates = {} as CatchupOverseerPayload["gates"];
  const childCounts = {} as CatchupOverseerPayload["childCounts"];
  for (const issue of [...issues].sort()) {
    const node = s.issues[issue];
    gates[issue] = s.gates[issue] ?? {};
    let open = 0;
    let closed = 0;
    for (const child of node.children) {
      if (s.issues[child]?.status === "done") closed += 1;
      else if (s.issues[child]) open += 1;
    }
    childCounts[issue] = { total: node.children.length, open, closed };
  }

  const prVerdicts: CatchupOverseerPayload["prVerdicts"] = {};
  for (const [prKey, pr] of Object.entries(s.prs).sort(([first], [second]) =>
    first.localeCompare(second)
  )) {
    if (!issues.has(pr.key)) continue;
    prVerdicts[prKey] = {
      issue: pr.key,
      sha: pr.headSha,
      ci: ciVerdict(pr),
      ...(pr.verdict === "red" ? { failing: [...pr.failing, ...pr.failingStatuses] } : {}),
      review: pr.reviewDecision ?? "pending",
      fixAttempts: pr.fixAttempts,
    };
  }

  const phaseCompletions: CatchupOverseerPayload["phaseCompletions"] = [];
  for (const issue of [...issues].sort()) {
    const phase = s.phases[issue];
    if (!phase?.completed) continue;
    if (!isLegionRole(phase.phase)) {
      throw new Error(`state.phases[${issue}] has an unrecognized phase: ${phase.phase}`);
    }
    phaseCompletions.push({
      issue,
      role: phase.phase,
      summary: phase.completed.summary,
      at: phase.completed.at,
    });
  }

  return { type: "catchup-overseer", gates, childCounts, prVerdicts, phaseCompletions };
}

function artifactsFor(state: LegionState, issue: IssueKey): Artifact[] {
  return Object.values(state.prs)
    .filter((pr) => pr.key === issue)
    .sort((first, second) => first.number - second.number)
    .map((pr) => ({ repo: pr.repo, number: pr.number }));
}

async function runJsonArray(
  runner: CommandRunner,
  command: string[],
  options: CommandRunnerOptions
): Promise<JsonRecord[]> {
  const result = await runner(command, options);
  if (result.exitCode !== 0) {
    throw new Error(`GitHub catch-up query failed: ${result.stderr}`);
  }
  const parsed: unknown = JSON.parse(result.stdout);
  if (!Array.isArray(parsed)) {
    throw new Error("GitHub catch-up query returned an invalid timeline array");
  }
  const entries = (parsed.every(Array.isArray) ? parsed.flat() : parsed) as unknown[];
  if (entries.some((entry) => !asRecord(entry))) {
    throw new Error("GitHub catch-up query returned an invalid timeline array");
  }
  return entries as JsonRecord[];
}

function timestamp(value: unknown): number | undefined {
  const date = stringValue(value);
  if (!date) return undefined;
  const milliseconds = Date.parse(date);
  return Number.isNaN(milliseconds) ? undefined : milliseconds;
}

function authorLogin(entry: JsonRecord): string | undefined {
  return stringValue(asRecord(entry.user)?.login);
}

function isWorkerActivity(
  login: string | undefined,
  email: string | undefined,
  appLogin: string,
  appEmail: string
): boolean {
  return login === appLogin || email === appEmail;
}

function isBot(login: string | undefined): boolean {
  return login?.endsWith("[bot]") ?? false;
}

function cursorFor(
  commits: JsonRecord[],
  comments: JsonRecord[],
  appLogin: string,
  appEmail: string
): number | undefined {
  let cursor: number | undefined;
  for (const commit of commits) {
    const commitData = asRecord(commit.commit);
    const author = asRecord(commitData?.author);
    const committer = asRecord(commitData?.committer);
    for (const identity of [author, committer]) {
      if (!isWorkerActivity(undefined, stringValue(identity?.email), appLogin, appEmail)) continue;
      const activityAt = timestamp(identity?.date);
      if (activityAt !== undefined && (cursor === undefined || activityAt > cursor))
        cursor = activityAt;
    }
  }
  for (const comment of comments) {
    if (!isWorkerActivity(authorLogin(comment), undefined, appLogin, appEmail)) continue;
    const activityAt = timestamp(comment.created_at);
    if (activityAt !== undefined && (cursor === undefined || activityAt > cursor))
      cursor = activityAt;
  }
  return cursor;
}

function unhandledComments(
  comments: JsonRecord[],
  cursor: number | undefined,
  appLogin: string,
  kind: "comment" | "review-comment"
): CatchupUnhandled[] {
  const result: CatchupUnhandled[] = [];
  for (const comment of comments) {
    const author = authorLogin(comment);
    const occurredAt = stringValue(comment.created_at);
    const occurredAtMs = timestamp(occurredAt);
    const id = numberValue(comment.id);
    const body = stringValue(comment.body);
    const url = stringValue(comment.html_url);
    if (
      !author ||
      !occurredAt ||
      occurredAtMs === undefined ||
      id === undefined ||
      body === undefined ||
      !url ||
      isBot(author) ||
      author === appLogin ||
      (cursor !== undefined && occurredAtMs <= cursor)
    ) {
      continue;
    }
    result.push({ kind, id, occurredAt, author, body, url });
  }
  return result;
}

function unhandledReviews(
  reviews: JsonRecord[],
  cursor: number | undefined,
  appLogin: string
): CatchupUnhandled[] {
  const result: CatchupUnhandled[] = [];
  for (const review of reviews) {
    const author = authorLogin(review);
    const occurredAt = stringValue(review.submitted_at);
    const occurredAtMs = timestamp(occurredAt);
    const id = numberValue(review.id);
    const state = stringValue(review.state);
    const body = stringValue(review.body) ?? "";
    const url = stringValue(review.html_url);
    if (
      !author ||
      !occurredAt ||
      occurredAtMs === undefined ||
      id === undefined ||
      !state ||
      !url ||
      isBot(author) ||
      author === appLogin ||
      (cursor !== undefined && occurredAtMs <= cursor)
    ) {
      continue;
    }
    result.push({
      kind: "review",
      id,
      occurredAt,
      author,
      state: state.toLowerCase(),
      body,
      url,
    });
  }
  return result;
}

export async function workerCatchup(
  s: LegionState,
  issue: IssueKey,
  role: LegionRole,
  deps: WorkerCatchupDeps
): Promise<LegionEventPayload> {
  const [owner] = deps.repo.split("/") as [string, string];
  const credential = await deps.tokenManager.getToken(modeToRole(WORKER_MODE[role]), owner);
  const options: CommandRunnerOptions = {
    env: buildRoleEnv(credential.token, credential.gitIdentity, process.env),
  };
  const unhandled: CatchupUnhandled[] = [];
  for (const artifact of artifactsFor(s, issue)) {
    const commits = await runJsonArray(
      deps.runner,
      [
        "gh",
        "api",
        "--paginate",
        "--slurp",
        `repos/${artifact.repo}/pulls/${artifact.number}/commits`,
      ],
      options
    );
    const comments = await runJsonArray(
      deps.runner,
      [
        "gh",
        "api",
        "--paginate",
        "--slurp",
        `repos/${artifact.repo}/issues/${artifact.number}/comments`,
      ],
      options
    );
    const reviewComments = await runJsonArray(
      deps.runner,
      [
        "gh",
        "api",
        "--paginate",
        "--slurp",
        `repos/${artifact.repo}/pulls/${artifact.number}/comments`,
      ],
      options
    );
    const cursor = cursorFor(
      commits,
      [...comments, ...reviewComments],
      credential.gitIdentity.name,
      credential.gitIdentity.email
    );
    unhandled.push(
      ...unhandledComments(comments, cursor, credential.gitIdentity.name, "comment"),
      ...unhandledComments(reviewComments, cursor, credential.gitIdentity.name, "review-comment")
    );
    const reviews = await runJsonArray(
      deps.runner,
      [
        "gh",
        "api",
        "--paginate",
        "--slurp",
        `repos/${artifact.repo}/pulls/${artifact.number}/reviews`,
      ],
      options
    );
    unhandled.push(...unhandledReviews(reviews, cursor, credential.gitIdentity.name));
  }
  unhandled.sort((first, second) => {
    const firstAt = "occurredAt" in first ? first.occurredAt : "";
    const secondAt = "occurredAt" in second ? second.occurredAt : "";
    return firstAt.localeCompare(secondAt);
  });
  return { type: "catchup-worker", unhandled };
}
