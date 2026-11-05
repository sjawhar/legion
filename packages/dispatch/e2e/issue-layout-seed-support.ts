// What every seed of `e2e/issue-layout-seed.ts` stands on: the project, the fake GitHub's
// component model and pull request, the fake Envoy's sessions, the polls that wait for the
// server's own indexing, and the one issue-with-documents builder the whole issue and the request
// baseline differ from each other only by size in. The gaps between what the page is specified to
// draw and what today's model holds are recorded in `issue-layout-seed.ts`'s doc comment.

import type { Actor, CheckBlock } from "../web/src/api/types";
import { setInterests, setLiveSessions } from "./agents";
import {
  answerAsk,
  claimIssue,
  createAsk,
  createComment,
  createHandoff,
  createIssue,
  createIssueArtifact,
  createProject,
  editArtifact,
  getIssue,
  getIssueResults,
  patchIssue,
  putArchitectureSource,
  putRepoProject,
  recordCheckResult,
  replyToCommentBlock,
  syncArchitectureSource,
} from "./api";
import { blockId } from "./editor";
import { seedFakeGithub } from "./fake-github-helpers";
import {
  checkBlockId,
  checkStateOf,
  checkText,
  decisionHeadings,
  decisionsDocument,
  OPEN_QUESTION_BLOCK_ID,
  planDocument,
  rewordedCheckText,
  type SeededCheckState,
  specDocument,
  verificationDocument,
} from "./issue-layout-seed-text";

export const PROJECT = "LAY";
const ARCHITECTURE_REPO = "legion/issue-layout-arch";
/** The repository the whole issue carries; the request baseline carries its own, so the two never
 *  share the App's cached answer about one repository. */
export const WHOLE_REPO = "acme/issue-layout";
export const BASELINE_REPO = "acme/issue-baseline";
/** The commit the seeded pull request's head is at, so a result recorded there reads `current`. */
export const HEAD_SHA = "a".repeat(40);
/** A commit no seeded pull request names, so a result recorded there is at no head. */
export const STRAY_SHA = "b".repeat(40);
const PULL_NUMBER = 11;

export const PLANNER = "issue-layout-planner";
export const REVIEWER = "issue-layout-reviewer";
export const TESTER = "issue-layout-tester";
export const plannerActor: Actor = { id: PLANNER, kind: "session" };
export const asPlanner = { actor: plannerActor, as: "agent" as const };

/** The component model the fake GitHub serves: two components, both attachable. */
const ARCHITECTURE_MODEL = {
  "panel.md": "---\ntitle: Issue panel\n---\nThe one right-hand panel.\n",
  "tree.md": "---\ntitle: Issue tree\n---\nThe ordered body of the page.\n",
};
const COMPONENT_IDS = ["panel", "tree"];

export async function pause(milliseconds: number): Promise<void> {
  const tick = Promise.withResolvers<void>();
  setTimeout(tick.resolve, milliseconds);
  await tick.promise;
}

/** How a check's newest result stands, by the rule `VerificationTab.tsx:31-43` reads it: `none`
 *  for a check nobody has run, and a result whose currency the server left out is `unknown` for
 *  the reason the page calls it unmarked - nothing judged it. */
function currencyOf(check: CheckBlock): string {
  const latest = check.results[0];
  return latest === undefined ? "none" : (latest.currency ?? "unknown");
}

/** The currency a check seeded in `state` must read. `unmarked` reads `unknown` only while the
 *  App is installed on no repository that covers the check's issue; with one installed, a result
 *  at a commit no pull request names reads `older` instead (results_currency.go:65-79). */
function wantedCurrency(state: SeededCheckState, repositoryInstalled: boolean): string {
  switch (state) {
    case "no-result":
      return "none";
    case "older":
      return "older";
    case "unmarked":
      return repositoryInstalled ? "older" : "unknown";
    default:
      return "current";
  }
}

/** Waits until every check of the issue reads the currency its seeded state implies, so a fixture
 *  that came out in four states fails here and not inside the test that reads the fifth. */
export async function awaitSeededCurrencies(
  issueKey: string,
  checks: number,
  repositoryInstalled: boolean
): Promise<void> {
  const wanted = new Map<string, string>();
  for (let index = 1; index <= checks; index += 1) {
    wanted.set(checkBlockId(index), wantedCurrency(checkStateOf(index), repositoryInstalled));
  }
  const deadline = Date.now() + 20_000;
  for (;;) {
    const read = await getIssueResults(issueKey);
    const seen = new Map(read.checks.map((check) => [check.block_id, currencyOf(check)]));
    if ([...wanted].every(([block, currency]) => seen.get(block) === currency)) return;
    if (Date.now() > deadline) {
      throw new Error(
        `${issueKey}'s checks read ${JSON.stringify([...seen])}, want ${JSON.stringify([...wanted])}`
      );
    }
    await pause(100);
  }
}

/** The id of the open ask the server indexed from a document's `ask` block. The document service
 *  settles the document after the write has answered, so the ask exists a moment later. */
export async function awaitBlockAsk(issueKey: string, block: string): Promise<string> {
  const deadline = Date.now() + 20_000;
  for (;;) {
    const ask = (await getIssue(issueKey)).open_asks.find((row) => row.block_id === block);
    if (ask !== undefined) return ask.id;
    if (Date.now() > deadline) {
      throw new Error(`${issueKey}'s ${block} ask was not indexed within 20 s`);
    }
    await pause(100);
  }
}

/** The imported component model, the repository and the pull request the results are measured
 *  against, whose title attributes it to `issueKey`. */
async function seedProjectSurroundings(issueKey: string, repository: string): Promise<void> {
  await seedFakeGithub({
    [ARCHITECTURE_REPO]: { contents: "read", files: ARCHITECTURE_MODEL, installation_id: 311 },
    [repository]: {
      commit_pulls: { [HEAD_SHA]: [PULL_NUMBER] },
      contents: "read",
      installation_id: 312,
      pull_requests: "read",
      pulls: {
        [PULL_NUMBER]: { head_sha: HEAD_SHA, state: "open", title: `${issueKey} the redesign` },
      },
    },
  });
  await putArchitectureSource(PROJECT, { branch: "main", repo: ARCHITECTURE_REPO });
  const source = await syncArchitectureSource(PROJECT, ARCHITECTURE_REPO);
  if (source.last_error !== null) {
    throw new Error(`the component model did not import: ${source.last_error}`);
  }
  await putRepoProject(repository, { default: false, project: PROJECT });
}

/** The sessions a seeded issue routes to, is claimed by and is subscribed by. A subscriber is a
 *  fake Envoy interest, because Dispatch stores none. */
export async function seedSessions(
  issueKey: string,
  subscribers: readonly string[]
): Promise<void> {
  await setLiveSessions([
    { session_id: PLANNER, title: "Planner (issue layout)" },
    { session_id: REVIEWER, title: "Reviewer (issue layout)" },
    { session_id: TESTER, title: "Tester (issue layout)" },
  ]);
  await setInterests(
    subscribers.map((session) => ({
      session_id: session,
      topics: [`notifications.dispatch.issue.${issueKey}.>`],
    }))
  );
}

/** The heading block of each decision, in document order: the block id
 *  `GET /api/v1/issues/{key}/decisions` answers each decision under. */
async function decisionBlockIds(artifactId: string, count: number): Promise<string[]> {
  const ids: string[] = [];
  for (const heading of decisionHeadings(count)) {
    ids.push(await blockId(artifactId, heading));
  }
  return ids;
}

/** One result per check, in the state `checkStateOf` names: a pass and a fail at the pull
 *  request's head, nothing at all for the check nobody ran, a pass whose check is then reworded,
 *  a pass at a commit no pull request names, and a pass recording no negative control. */
async function recordSeededResults(
  issueKey: string,
  checks: number,
  verificationArtifactId: string
): Promise<void> {
  for (let index = 1; index <= checks; index += 1) {
    const state = checkStateOf(index);
    if (state === "no-result") continue;
    await recordCheckResult(issueKey, {
      check: checkBlockId(index),
      commit: state === "unmarked" ? STRAY_SHA : HEAD_SHA,
      failed: state === "failed" ? 2 : 0,
      negative_control: state === "no-control" ? "" : "Removed the row's state and the run failed.",
      observed: state === "failed" ? "2 failed" : "1 passed",
      role: "tester",
      run: "bun run e2e -- e2e/issue-layout.e2e.ts",
      surface: "browser",
      verdict: state === "failed" ? "fail" : "pass",
    });
    if (state === "older") {
      await editArtifact(verificationArtifactId, {
        ops: [{ find: checkText(index), op: "replace", with: rewordedCheckText(index) }],
      });
    }
  }
}

export interface IssueShape {
  readonly checks: number;
  readonly children: number;
  readonly commentReplies: number;
  readonly commentThreads: number;
  readonly decisions: number;
  readonly planTasks: number;
  readonly repository: string;
  readonly requirements: number;
  readonly title: string;
}

export interface SeededSkeleton {
  readonly answeredAskId: string;
  readonly checks: readonly string[];
  readonly children: { readonly blocked: string; readonly done: string; readonly keys: string[] };
  readonly commentReplies: string[];
  readonly commentThreads: string[];
  readonly decisions: readonly string[];
  readonly decisionsArtifactId: string;
  readonly key: string;
  readonly openQuestionAskId: string;
  readonly parentKey: string;
  readonly planArtifactId: string;
  readonly specArtifactId: string;
  readonly verificationArtifactId: string;
}

/** One whole issue: a parent, every property the identity block draws, its children with one
 *  blocked and one done, its spec, Verification, plan and decisions documents, one result per
 *  check, its comment threads and two handoffs. */
export async function seedIssueWithDocuments(shape: IssueShape): Promise<SeededSkeleton> {
  await createProject({ key: PROJECT, name: "Issue layout" });
  // `force` past the possible-duplicate guard: these titles are deliberately close to each other.
  const parent = await createIssue({
    force: true,
    project: PROJECT,
    title: "The issue page is redesigned",
  });
  const issue = await createIssue({
    force: true,
    parent: parent.key,
    project: PROJECT,
    spec: specDocument({ openQuestion: true, requirements: shape.requirements, summary: true }),
    title: shape.title,
  });
  const specArtifactId = issue.primary_artifact_id ?? "";
  await seedProjectSurroundings(issue.key, shape.repository);
  await seedSessions(issue.key, [PLANNER, REVIEWER, TESTER]);
  await patchIssue(issue.key, {
    assignee: "alice",
    components: { ids: COMPONENT_IDS, mode: "explicit" },
    external_links: [
      { kind: "github_pr", url: `https://github.com/${shape.repository}/pull/${PULL_NUMBER}` },
    ],
    labels: ["dispatch", "layout", "redesign"],
    priority: 1,
    repo: shape.repository,
    route: `session:${PLANNER}`,
    status: "in_progress",
  });
  await claimIssue(issue.key, asPlanner);

  const children: string[] = [];
  for (let index = 1; index <= shape.children; index += 1) {
    const child = await createIssue({
      force: true,
      parent: issue.key,
      project: PROJECT,
      title: `Child ${index} of the redesign`,
    });
    children.push(child.key);
  }
  const blocked = children[1] ?? "";
  const done = children[children.length - 1] ?? "";
  await patchIssue(blocked, { blocked_by: [children[0] ?? ""] });
  await patchIssue(done, { status: "done" });

  const verification = await createIssueArtifact(issue.key, {
    content: verificationDocument({
      checks: shape.checks,
      issueKey: issue.key,
      requirements: shape.requirements,
    }),
    name: "verification.md",
  });
  const plan = await createIssueArtifact(issue.key, {
    content: planDocument({
      issueKey: issue.key,
      planTasks: shape.planTasks,
      requirements: shape.requirements,
    }),
    name: "plan.md",
  });
  const answered = await createAsk(
    issue.key,
    { question: "Which addresses must the new page still answer?", urgency: "high" },
    asPlanner
  );
  await answerAsk(answered.id, { expected_edited_at: null, selected: [], text: "All of them." });
  const decisions = await createIssueArtifact(issue.key, {
    content: decisionsDocument({
      askRef: `dispatch://${issue.key}/ask/${answered.id}`,
      count: shape.decisions,
      issueKey: issue.key,
    }),
    name: "decisions.md",
  });
  await recordSeededResults(issue.key, shape.checks, verification.artifact.id);

  const commentThreads: string[] = [];
  for (let index = 1; index <= shape.commentThreads; index += 1) {
    const thread = await createComment(issue.key, {
      body: `Thread ${index} opens here.\n\nIts second paragraph is a block of its own.`,
    });
    commentThreads.push(thread.id);
  }
  const commentReplies: string[] = [];
  for (let index = 0; index < shape.commentReplies; index += 1) {
    const thread = commentThreads[index % commentThreads.length] ?? "";
    const reply = await replyToCommentBlock(issue.key, thread, "b2", {
      body: `Reply ${index + 1} answers the second paragraph of its thread.`,
    });
    commentReplies.push(reply.id);
  }

  await createHandoff(issue.key, "plan", { phase: "plan", schemaVersion: 1 }, plannerActor);
  await createHandoff(
    issue.key,
    "implement",
    { phase: "implement", schemaVersion: 1, summary: "The page on today's data." },
    plannerActor
  );

  return {
    answeredAskId: answered.id,
    checks: Array.from({ length: shape.checks }, (_, index) => checkBlockId(index + 1)),
    children: { blocked, done, keys: children },
    commentReplies,
    commentThreads,
    decisions: await decisionBlockIds(decisions.artifact.id, shape.decisions),
    decisionsArtifactId: decisions.artifact.id,
    key: issue.key,
    openQuestionAskId: await awaitBlockAsk(issue.key, OPEN_QUESTION_BLOCK_ID),
    parentKey: parent.key,
    planArtifactId: plan.artifact.id,
    specArtifactId,
    verificationArtifactId: verification.artifact.id,
  };
}
