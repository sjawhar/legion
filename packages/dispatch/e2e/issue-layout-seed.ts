import { eventItemId, pinnedItemMarker } from "../web/src/features/issue/pins";
import {
  answerAsk,
  createAsk,
  createComment,
  createIssue,
  createIssueArtifact,
  createMessage,
  createProject,
  editArtifact,
  getIssueEvents,
  putIssueState,
  replyToMessageBlock,
  uploadIssueFile,
} from "./api";
import { blockId } from "./editor";
import { seedFakeGithub } from "./fake-github-helpers";
import {
  asPlanner,
  awaitBlockAsk,
  awaitSeededCurrencies,
  BASELINE_REPO,
  PLANNER,
  PROJECT,
  seedIssueWithDocuments,
  seedSessions,
  WHOLE_REPO,
} from "./issue-layout-seed-support";
import {
  CITATION_BLOCK_ID,
  checkBlockId,
  citationRequirement,
  OPEN_QUESTION_BLOCK_ID,
  planTaskBlockId,
  requirementBlockId,
  requirementText,
  SPINE_CHECK_BLOCK_ID,
  SPINE_DECISION_HEADING,
  SPINE_REQUIREMENTS,
  SPINE_SPEC,
  SUMMARY_BLOCK_ID,
  specDocument,
  spineDecisionsDocument,
  spineVerificationDocument,
} from "./issue-layout-seed-text";
import { ONE_PIXEL_PNG } from "./sends";

/**
 * The whole issues `e2e/issue-layout.e2e.ts` reads, for the issue page dispatch://INFRA-1196
 * redesigns. A caller needs `resetDatabase()` and one of these calls in `beforeEach` and nothing
 * else: each seed makes its own project, component model, repository and sessions.
 *
 * Every write goes through `e2e/api.ts`, as a person for a human's write and under the agent
 * bearer for a session's, with two exceptions that are not Dispatch's own state: the component
 * model and the pull request come from the fake GitHub (`e2e/fake-github-helpers.ts`), and a
 * subscriber is a fake Envoy interest (`e2e/agents.ts`), because Dispatch stores none and its one
 * subscriber write is a DELETE (`packages/envoy/internal/dispatch/api/routes_table.go:364-365`).
 * `resetDatabase()` clears both fakes. Each seed returns the issue key with a name for every
 * block id its tests read, so no test derives an id from rendered text.
 *
 * ## What today's model cannot hold, and what is seeded instead
 *
 * - **The summary node.** `blocks.json` is at version 3 and declares five types, `callout`,
 *   `ask`, `requirement`, `plan_task` and `check`, and no summary node, no author of the three
 *   sentences and no `last_read_back_at`, which are dispatch://INFRA-1278
 *   (`packages/envoy/internal/dispatch/pmdoc/schema/blocks.json:1-118`, read 2026-11-05). The
 *   three sentences are seeded as a `callout` with the block id `summary`.
 * - **`from` on a requirement.** `requirement` carries no attributes at all
 *   (`blocks.json:70-75`), so there is no `from` edge to seed and *every* seeded requirement's
 *   `from` is absent; dispatch://INFRA-1098 item 21 adds it. For the disagreement `c-spine`
 *   names, `seedSpineCases` seeds a decision whose block links one requirement while the
 *   answered question a reader would read as that requirement's origin is another node.
 * - **`shaped` on a decision, and a decision's author, uncertainty level and reason.** The
 *   decisions read answers `kind: "agent" | "person"` and a boolean `unsure`
 *   (`packages/contracts/src/dispatch-api.ts:2473-2497`); `shaped`, `uncertainty`,
 *   `uncertainty_reason` and `decided_by` arrive with pull request 2797. A decision's link to a
 *   requirement is seeded as the indexed `dispatch://` mention of that block, and one is a
 *   person's when it mentions an ask (`packages/envoy/internal/dispatch/api/decisions.go:25-28`).
 * - **Depth below a document.** `document_blocks` holds no parent and no ordinal
 *   (`packages/envoy/internal/dispatch/store/migrations/0121_block_graph.up.sql:16-27`, read
 *   2026-11-05), so a tree below an issue reaches exactly two levels today: the issue, its
 *   documents, and each document's blocks. `seedFrontierDeep` puts its open question at that
 *   depth, **two** levels, not the four `c-tree` names: that seed waits on INFRA-1098 item 19.
 * - **The five result states on one issue.** A before-merge result reads `older` when its commit
 *   is not a stored pull-request head and the check's issue resolves to a repository Dispatch can
 *   read, and `unknown` only when it cannot read one
 *   (`packages/envoy/internal/dispatch/api/results_currency.go:65-79`, read 2026-11-05). So
 *   `seedWholeIssue` records its results while the fake GitHub serves the repository and the pull
 *   request and then seeds the fake with no repository: the head results stay `current`, the
 *   reworded check's goes `older`, and the one at a commit no pull request names reads `unknown`,
 *   the `Unmarked` of `NODE_STATE_WORDS.check`. `seedRequestBaseline` keeps the repository
 *   installed, because its three GitHub reads are part of the measured baseline, so only four of
 *   the five states appear there.
 * - **The reply that answers a reply.** `threadHeadOf` in
 *   `packages/envoy/internal/dispatch/api/comment_create.go` climbs `reply_to` to the thread head
 *   before storing, so which entry a reply answered survives only as `reply_to_block` and
 *   `in_reply_to_block` until dispatch://INFRA-1098 item 15 lands, which is what it names here.
 * - **A check's `serves` read back.** `GET /api/v1/issues/{key}/verification` is pull request
 *   2754 and not in this build, so `serves` is seeded on the block and read only as coverage.
 */

export interface WholeIssueSeed {
  /** The answered question the decisions document names, which makes one decision a person's. */
  readonly answeredAskId: string;
  /** The checks of the Verification document, by the state each one is seeded in. */
  readonly checks: {
    readonly failed: string;
    readonly noNegativeControl: string;
    readonly noResult: string;
    readonly older: string;
    readonly passed: string;
    readonly unmarked: string;
  };
  readonly children: { readonly blocked: string; readonly done: string; readonly keys: string[] };
  readonly comments: {
    /** The comment anchored to the spec block `anchoredBlock`. */
    readonly anchored: string;
    readonly anchoredBlock: string;
    readonly replies: readonly string[];
    readonly threads: readonly string[];
  };
  /** The heading block of each decision, in document order; the last one is replaced. */
  readonly decisions: readonly string[];
  readonly decisionsArtifactId: string;
  readonly documents: {
    /** The slug of the document a spec block cites, and that block. */
    readonly cited: string;
    readonly citingBlock: string;
    readonly image: string;
    readonly uncited: string;
  };
  readonly key: string;
  readonly messages: {
    readonly reply: string;
    /** The reply that answers a block of `reply`, which is a reply itself. */
    readonly replyToReply: string;
    readonly root: string;
  };
  /** The ask the spec's `ask` block was indexed into, two levels below the issue. */
  readonly openQuestion: { readonly askId: string; readonly blockId: string };
  readonly parentKey: string;
  readonly pinnedEventId: number;
  readonly planArtifactId: string;
  readonly planTasks: readonly string[];
  readonly requirements: readonly string[];
  readonly specArtifactId: string;
  readonly summaryBlockId: string;
  readonly verificationArtifactId: string;
}

/** The whole issue the Verification document of dispatch://INFRA-1196 describes, in one call. */
export async function seedWholeIssue(): Promise<WholeIssueSeed> {
  const seeded = await seedIssueWithDocuments({
    checks: 6,
    children: 7,
    commentReplies: 3,
    commentThreads: 2,
    decisions: 7,
    planTasks: 9,
    repository: WHOLE_REPO,
    requirements: 12,
    title: "The issue page draws one ordered body",
  });

  const cited = await createIssueArtifact(seeded.key, {
    content: "# Notes\n\nThe measurements behind the first requirement.\n",
    name: "notes.md",
  });
  const uncited = await createIssueArtifact(seeded.key, {
    content: "# Background\n\nNothing cites this document.\n",
    name: "background.md",
  });
  const image = await uploadIssueFile(seeded.key, {
    bytes: new Uint8Array(Buffer.from(ONE_PIXEL_PNG, "base64")),
    contentType: "image/png",
    name: "shot.png",
  });
  await editArtifact(seeded.specArtifactId, {
    ops: [
      {
        after: `block:${requirementBlockId(1)}`,
        markdown: citationRequirement(seeded.key, cited.artifact.slug),
        op: "insert",
      },
    ],
  });

  const anchored = await createComment(seeded.key, {
    anchor: { artifact: "spec", quote: requirementText(2) },
    body: "This requirement is the one the tree reads first.",
  });
  const root = await createMessage(
    seeded.key,
    { body: "The tree is folded to the frontier.\n\nThe fold budget counts no question." },
    asPlanner
  );
  const reply = await replyToMessageBlock(seeded.key, root.id, "b2", {
    body: "Which is why the deepest question is still on the first screen.",
  });
  const replyToReply = await replyToMessageBlock(seeded.key, reply.id, "b1", {
    body: "Read that against the row count with the question left out.",
  });

  const events = await getIssueEvents(seeded.key, { limit: 1 });
  const pinned = events[0];
  if (pinned === undefined) throw new Error(`${seeded.key} recorded no event to pin`);
  await putIssueState(seeded.key, {
    dismissed: [pinnedItemMarker(eventItemId(pinned))],
    pinned: true,
    seq: 1,
  });

  // With the App installed on nothing, the result at a commit no pull request names is the one
  // the page draws as Unmarked: see this module's doc comment on the five result states.
  await seedFakeGithub({});
  await awaitSeededCurrencies(seeded.key, 6, false);

  return {
    answeredAskId: seeded.answeredAskId,
    checks: {
      failed: checkBlockId(2),
      noNegativeControl: checkBlockId(6),
      noResult: checkBlockId(3),
      older: checkBlockId(4),
      passed: checkBlockId(1),
      unmarked: checkBlockId(5),
    },
    children: seeded.children,
    comments: {
      anchored: anchored.id,
      anchoredBlock: requirementBlockId(2),
      replies: seeded.commentReplies,
      threads: seeded.commentThreads,
    },
    decisions: seeded.decisions,
    decisionsArtifactId: seeded.decisionsArtifactId,
    documents: {
      cited: cited.artifact.slug,
      citingBlock: CITATION_BLOCK_ID,
      image: image.artifact.slug,
      uncited: uncited.artifact.slug,
    },
    key: seeded.key,
    messages: { reply: reply.id, replyToReply: replyToReply.id, root: root.id },
    openQuestion: { askId: seeded.openQuestionAskId, blockId: OPEN_QUESTION_BLOCK_ID },
    parentKey: seeded.parentKey,
    pinnedEventId: pinned.id,
    planArtifactId: seeded.planArtifactId,
    planTasks: Array.from({ length: 9 }, (_, index) => planTaskBlockId(index + 1)),
    requirements: Array.from({ length: 12 }, (_, index) => requirementBlockId(index + 1)),
    specArtifactId: seeded.specArtifactId,
    summaryBlockId: SUMMARY_BLOCK_ID,
    verificationArtifactId: seeded.verificationArtifactId,
  };
}

export interface FrontierSeed {
  readonly askId: string;
  /** The document block the open question is, or null when the ask hangs off the issue itself. */
  readonly askBlockId: string | null;
  /** How many levels below the issue the open question sits. */
  readonly depth: 1 | 2;
  /** A branch the page opens folded, with every node under it: a test reads the count the folded
   *  row draws against `blockIds.length`. */
  readonly foldedBranch: {
    readonly artifactId: string;
    readonly blockIds: readonly string[];
    readonly slug: string;
  };
  readonly key: string;
}

/** The folded branch both frontier seeds carry: one document of five requirement blocks. */
async function seedFoldedBranch(issueKey: string): Promise<FrontierSeed["foldedBranch"]> {
  const branch = await createIssueArtifact(issueKey, {
    content: specDocument({ openQuestion: false, requirements: 5, summary: false }),
    name: "branch.md",
  });
  return {
    artifactId: branch.artifact.id,
    blockIds: Array.from({ length: 5 }, (_, index) => requirementBlockId(index + 1)),
    slug: branch.artifact.slug,
  };
}

/** An issue whose open question sits one level below it: an ask on the issue itself, under no
 *  document. */
export async function seedFrontierShallow(): Promise<FrontierSeed> {
  await createProject({ key: PROJECT, name: "Issue layout" });
  const issue = await createIssue({
    force: true,
    project: PROJECT,
    spec: specDocument({ openQuestion: false, requirements: 3, summary: true }),
    title: "The frontier is one level down",
  });
  await seedSessions(issue.key, [PLANNER]);
  const ask = await createAsk(
    issue.key,
    { question: "Does the fold open to the frontier on a phone too?", urgency: "high" },
    asPlanner
  );
  return {
    askBlockId: null,
    askId: ask.id,
    depth: 1,
    foldedBranch: await seedFoldedBranch(issue.key),
    key: issue.key,
  };
}

/** An issue whose open question sits as deep as today's model reaches: a block of a document,
 *  which is two levels below the issue and not the four `c-tree` names (see the doc comment). */
export async function seedFrontierDeep(): Promise<FrontierSeed> {
  await createProject({ key: PROJECT, name: "Issue layout" });
  const issue = await createIssue({
    force: true,
    project: PROJECT,
    spec: specDocument({ openQuestion: true, requirements: 3, summary: true }),
    title: "The frontier is as deep as the model reaches",
  });
  return {
    askBlockId: OPEN_QUESTION_BLOCK_ID,
    askId: await awaitBlockAsk(issue.key, OPEN_QUESTION_BLOCK_ID),
    depth: 2,
    foldedBranch: await seedFoldedBranch(issue.key),
    key: issue.key,
  };
}

export interface SpineSeed {
  /** The answered question a reader would read as `shapedElsewhere`'s origin. No `from` edge
   *  names it, because the model holds none; see the doc comment. */
  readonly answeredAskId: string;
  /** The check whose `serves` names both `servedTwiceA` and `servedTwiceB`. */
  readonly checkServingTwo: string;
  /** The heading block of the decision that links `shapedElsewhere`. */
  readonly decisionBlockId: string;
  readonly decisionsArtifactId: string;
  readonly key: string;
  readonly requirements: typeof SPINE_REQUIREMENTS;
  readonly specArtifactId: string;
  readonly verificationArtifactId: string;
}

/** The spine's three cases: a requirement nothing explains, a requirement a decision links while
 *  the answered question names another node, and a check that serves two requirements. */
export async function seedSpineCases(): Promise<SpineSeed> {
  await createProject({ key: PROJECT, name: "Issue layout" });
  const issue = await createIssue({
    force: true,
    project: PROJECT,
    spec: SPINE_SPEC,
    title: "The spine places every node",
  });
  await seedSessions(issue.key, [PLANNER]);
  const answered = await createAsk(
    issue.key,
    { question: "Which node does a requirement with no parent sit under?" },
    asPlanner
  );
  await answerAsk(answered.id, {
    expected_edited_at: null,
    selected: [],
    text: "The issue itself.",
  });
  const decisions = await createIssueArtifact(issue.key, {
    content: spineDecisionsDocument(issue.key, answered.id),
    name: "decisions.md",
  });
  const verification = await createIssueArtifact(issue.key, {
    content: spineVerificationDocument(issue.key),
    name: "verification.md",
  });
  return {
    answeredAskId: answered.id,
    checkServingTwo: SPINE_CHECK_BLOCK_ID,
    decisionBlockId: await blockId(decisions.artifact.id, SPINE_DECISION_HEADING),
    decisionsArtifactId: decisions.artifact.id,
    key: issue.key,
    requirements: SPINE_REQUIREMENTS,
    specArtifactId: issue.primary_artifact_id ?? "",
    verificationArtifactId: verification.artifact.id,
  };
}

export interface PlainIssueSeed {
  readonly key: string;
  readonly requirements: readonly string[];
  readonly specArtifactId: string;
  /** The block id of the three sentences, null on the spec that carries none. */
  readonly summaryBlockId: string | null;
}

/** One issue with a three-requirement spec and no children, with or without its summary node. */
async function seedPlainIssue(summary: boolean, title: string): Promise<PlainIssueSeed> {
  await createProject({ key: PROJECT, name: "Issue layout" });
  const issue = await createIssue({
    force: true,
    project: PROJECT,
    spec: specDocument({ openQuestion: false, requirements: 3, summary }),
    title,
  });
  return {
    key: issue.key,
    requirements: Array.from({ length: 3 }, (_, index) => requirementBlockId(index + 1)),
    specArtifactId: issue.primary_artifact_id ?? "",
    summaryBlockId: summary ? SUMMARY_BLOCK_ID : null,
  };
}

/** An issue with no children at all, which draws no children section. */
export async function seedNoChildren(): Promise<PlainIssueSeed> {
  return await seedPlainIssue(true, "This issue has no children");
}

/** An issue whose spec carries no summary node, so the head of the page says so. */
export async function seedNoSummary(): Promise<PlainIssueSeed> {
  return await seedPlainIssue(false, "This spec carries no summary node");
}

export interface BaselineSeed {
  readonly checks: readonly string[];
  readonly children: { readonly blocked: string; readonly done: string; readonly keys: string[] };
  readonly comments: { readonly replies: readonly string[]; readonly threads: readonly string[] };
  readonly decisions: readonly string[];
  readonly key: string;
  /** 1 for the baseline itself, 2 for the issue carrying twice its nodes. */
  readonly nodeMultiplier: 1 | 2;
  readonly openQuestionAskId: string;
  readonly planArtifactId: string;
  readonly planTasks: readonly string[];
  /** The slugs of the two documents the spec cites, which are the baseline's two references. */
  readonly references: readonly string[];
  readonly requirements: readonly string[];
  readonly specArtifactId: string;
  readonly verificationArtifactId: string;
}

/** The content `dispatch://INFRA-1196/artifact/request-baseline-md` describes, at `multiplier`
 *  times its nodes: three labels, a repository with a pull request, two components, a parent, a
 *  claim, a message route, three subscribers, two references, seven children (one blocked, one
 *  done), twelve requirements, six checks with results in several states, nine plan tasks, two
 *  decision records, one open ask, six comment threads with eight replies, two handoffs. Its 37
 *  events are what those writes record, not a number seeded on its own, and the properties are
 *  not multiplied: the multiplier doubles the nodes, which the request count is read against. */
async function seedBaseline(multiplier: 1 | 2): Promise<BaselineSeed> {
  const requirements = 12 * multiplier;
  const planTasks = 9 * multiplier;
  const seeded = await seedIssueWithDocuments({
    checks: 6 * multiplier,
    children: 7 * multiplier,
    commentReplies: 8 * multiplier,
    commentThreads: 6 * multiplier,
    decisions: 2 * multiplier,
    planTasks,
    repository: BASELINE_REPO,
    requirements,
    title:
      multiplier === 1
        ? "The first load stays inside its request budget"
        : "The same content with twice the nodes",
  });
  const references: string[] = [];
  for (const name of ["notes.md", "measurements.md"]) {
    const cited = await createIssueArtifact(seeded.key, {
      content: `# ${name}\n\nCited from the spec.\n`,
      name,
    });
    await editArtifact(seeded.specArtifactId, {
      ops: [
        {
          after: `block:${requirementBlockId(1)}`,
          markdown: `See dispatch://${seeded.key}/artifact/${cited.artifact.slug}.`,
          op: "insert",
        },
      ],
    });
    references.push(cited.artifact.slug);
  }
  await awaitSeededCurrencies(seeded.key, 6 * multiplier, true);
  return {
    checks: seeded.checks,
    children: seeded.children,
    comments: { replies: seeded.commentReplies, threads: seeded.commentThreads },
    decisions: seeded.decisions,
    key: seeded.key,
    nodeMultiplier: multiplier,
    openQuestionAskId: seeded.openQuestionAskId,
    planArtifactId: seeded.planArtifactId,
    planTasks: Array.from({ length: planTasks }, (_, index) => planTaskBlockId(index + 1)),
    references,
    requirements: Array.from({ length: requirements }, (_, index) => requirementBlockId(index + 1)),
    specArtifactId: seeded.specArtifactId,
    verificationArtifactId: seeded.verificationArtifactId,
  };
}

/** The issue the request baseline was measured on, seeded from that artifact's own description. */
export async function seedRequestBaseline(): Promise<BaselineSeed> {
  return await seedBaseline(1);
}

/** The same content with twice its nodes, which proves the page makes no request per node. */
export async function seedTwiceTheNodes(): Promise<BaselineSeed> {
  return await seedBaseline(2);
}
