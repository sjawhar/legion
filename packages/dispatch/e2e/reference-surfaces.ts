import { DISPATCH_TEXT_REFERENCES } from "@legion/contracts";
import { expect, type Locator, type Page } from "@playwright/test";

import type { Issue } from "../web/src/api/types";
import {
  createAsk,
  createComment,
  createIssue,
  createIssueArtifact,
  createMessage,
  createProject,
  listIssueAsks,
} from "./api";
import { blockId, openedThreadCard, setSheet } from "./editor";
import { READING_PANE_WIDTH } from "./inbox-pane";
import { replyRowId } from "./inbox-threads";

/**
 * The one world every reference spec is walked across, and the surfaces it is read on.
 *
 * One fixture rather than four: the forms a reference is written in (a `dispatch://` reference, a
 * dashboard address, an issue key on its own, a `#` and a question block's id, and any of their
 * ids written short) all resolve against the same records, so the margin, the Conversation, the
 * Inbox's thread rows, the Answers page, the live editor and a past version are read against
 * one seeding.
 * `seedReferenceWorld` makes it; the helpers below are how each surface is reached.
 */

/** The agent session these specs write as, so a write is never the reader's own. */
export const referenceAgent = {
  actor: { id: "e2e-references-agent", kind: "session" as const },
  as: "agent" as const,
};

/** The paragraph of the host's spec every margin thread here is anchored to. It is also the
 *  spec block whose id a short block reference names. */
export const ANCHOR = "The margin anchor sits here.";

/** The keys the world's projects and issues take. This repository is public, so the fixture
 *  names no real project. */
export const HOST_KEY = "CORE-1";
export const TARGET_KEY = "CORE-2";
export const OTHER_KEY = "XCORE-1";
export const ACME_KEY = "ACME-12";

/** The question block ids the host's spec declares: every id the shared text table's `#id` rows
 *  name, so the one world answers every row of it (`@legion/contracts`'s
 *  DISPATCH_TEXT_REFERENCES, the table the Go reader and the dashboard's reader share). */
export const QUESTION_IDS: readonly string[] = [
  ...new Set(DISPATCH_TEXT_REFERENCES.flatMap((row) => Object.keys(row.questions ?? {}))),
];

/** What a question block of `scope`'s document asks. Each document asks its own words, so a
 *  block another document declares under the same id is that document's question and not this
 *  one's. */
export function questionText(id: string, scope: string): string {
  return `Which way does question ${id} of ${scope} go?`;
}

/** The `:::ask` block that declares question `id` of `scope`'s document. */
export function questionBlock(id: string, scope: string): string {
  return `:::ask{#${id} urgency="med" multiple="false"}\n${questionText(id, scope)}\n\n- Yes\n- No\n:::`;
}

/** A question block's ask once the server has settled it. */
export interface SettledQuestion {
  readonly id: string;
  readonly question: string;
}

/**
 * The asks the question blocks of the document `slug` names under `issueKey` have settled into,
 * by block id, once every id of `ids` has one: the server indexes a block into its ask row after
 * the version holding it is written, one block at a time.
 */
export async function settledQuestions(
  issueKey: string,
  slug: string,
  ids: readonly string[]
): Promise<Record<string, SettledQuestion>> {
  const read = async () => {
    const settled: Record<string, SettledQuestion> = {};
    for (const ask of await listIssueAsks(issueKey)) {
      if (ask.block_id && ask.block_artifact?.slug === slug) {
        settled[ask.block_id] = { id: ask.id, question: ask.question };
      }
    }
    return settled;
  };
  await expect
    .poll(async () => Object.keys(await read()).sort(), { timeout: 60_000 })
    .toEqual([...ids].sort());
  return read();
}

/** One reference and the title every surface reads it as. */
export interface Target {
  readonly address: string;
  readonly title: string;
}

/** The kinds of target a reference names in these specs. */
export const TARGET_KINDS = ["Issue", "Document", "Question", "Comment", "Message"] as const;
export type TargetKind = (typeof TARGET_KINDS)[number];

/** An item of the host a reference may write with a short id, and the reference that names it
 *  whole. */
export interface ShortenableItem {
  readonly id: string;
  readonly reference: string;
}

export interface ReferenceWorld {
  /** CORE-1, the host: its spec holds the anchor, filler that puts the questions below the
   *  fold, and one question block per `QUESTION_IDS`. The texts these specs write live on it. */
  readonly host: Issue;
  /** CORE-2, the issue references name, which owns the document, question, comment and message
   *  of `targets`. */
  readonly target: Issue;
  /** XCORE-1 and ACME-12, the two other keys a text writes bare. */
  readonly other: Issue;
  readonly acme: Issue;
  /** The host's spec's questions, by block id. */
  readonly questions: Readonly<Record<string, SettledQuestion>>;
  /** The host's own ask, comment, message and spec block, the four records whose ids a text may
   *  write short (`dispatch://CORE-1/ask/2a7f1c90`). */
  readonly items: Readonly<Record<"ask" | "comment" | "message" | "block", ShortenableItem>>;
  /** What each kind of target reads as, by its reference. */
  readonly targets: Readonly<Record<TargetKind, Target>>;
}

/** How many filler paragraphs stand between the anchor and the first question block, so a link
 *  to a question opens a document whose question starts below the fold. */
const FILLER_PARAGRAPHS = 40;

/**
 * Seeds the world: the three projects, the host whose spec declares every question block, the
 * target with a document, an ask, a comment and a message, the cross-project issue, and the
 * host's own ask, comment, message and spec block whose ids a short reference names.
 *
 * The keys are deterministic — an issue's key is its project's next number — and the seeding
 * refuses any other, so a change in how keys are minted fails here rather than inside a row.
 */
export async function seedReferenceWorld(): Promise<ReferenceWorld> {
  await createProject({ key: "CORE", name: "Core" });
  await createProject({ key: "XCORE", name: "Cross core" });
  await createProject({ key: "ACME", name: "Acme" });
  const filler = Array.from(
    { length: FILLER_PARAGRAPHS },
    (_, index) => `Filler paragraph ${index + 1} keeps the questions below the fold.`
  );
  const spec = [ANCHOR, ...filler, ...QUESTION_IDS.map((id) => questionBlock(id, "spec")), ""].join(
    "\n\n"
  );
  const host = await createIssue({ project: "CORE", spec, title: "Reference host" });
  const target = await createIssue({ project: "CORE", title: "Warm the cache" });
  const other = await createIssue({ project: "XCORE", title: "Cross-project target" });
  // ACME-12 is the twelfth issue of ACME, so eleven stand before it.
  let acme = await createIssue({ project: "ACME", title: "Acme first" });
  for (let made = 2; made <= 12; made += 1) {
    acme = await createIssue({ project: "ACME", title: `Acme number ${made}` });
  }
  const made = { acme: acme.key, host: host.key, other: other.key, target: target.key };
  const want = { acme: ACME_KEY, host: HOST_KEY, other: OTHER_KEY, target: TARGET_KEY };
  for (const [name, key] of Object.entries(want)) {
    if (made[name as keyof typeof made] !== key) {
      throw new Error(`setup: the world made ${JSON.stringify(made)}, not ${JSON.stringify(want)}`);
    }
  }

  const document = await createIssueArtifact(
    target.key,
    { content: "# Notes\n\nThe cache notes.\n", name: "notes.md" },
    referenceAgent
  );
  expect(document.artifact.slug).toBe("notes-md");
  const targetAsk = await createAsk(
    target.key,
    { question: "Should the cache be warmed first?" },
    referenceAgent
  );
  const targetComment = await createComment(
    target.key,
    { body: "The warm cache halves start-up time." },
    referenceAgent
  );
  const targetMessage = await createMessage(
    target.key,
    { body: "The rollout finished at noon." },
    referenceAgent
  );

  // The host's own four, which a text writes short: an ask, a comment, a message and the spec
  // block the anchor is.
  const hostAsk = await createAsk(
    host.key,
    { question: "Which cache backs the rollout?" },
    referenceAgent
  );
  const hostComment = await createComment(
    host.key,
    { body: "The rollout waits on the cache." },
    referenceAgent
  );
  const hostMessage = await createMessage(
    host.key,
    { body: "The cache is warm now." },
    referenceAgent
  );
  const hostBlock = await blockId(host.primary_artifact_id, ANCHOR);

  const questions = await settledQuestions(host.key, "spec", QUESTION_IDS);
  for (const id of ["q2", "q3"]) {
    if (questions[id] === undefined) {
      throw new Error(`setup: the shared text table no longer declares the question ${id}`);
    }
  }

  return {
    acme,
    host,
    items: {
      ask: { id: hostAsk.id, reference: `dispatch://${host.key}/ask/${hostAsk.id}` },
      block: { id: hostBlock, reference: `dispatch://${host.key}/spec#b-${hostBlock}` },
      comment: {
        id: hostComment.id,
        reference: `dispatch://${host.key}/comment/${hostComment.id}`,
      },
      message: {
        id: hostMessage.id,
        reference: `dispatch://${host.key}/message/${hostMessage.id}`,
      },
    },
    other,
    questions,
    target,
    targets: {
      Comment: {
        address: `dispatch://${target.key}/comment/${targetComment.id}`,
        title: "The warm cache halves start-up time.",
      },
      Document: {
        address: `dispatch://${target.key}/artifact/notes-md`,
        title: "notes.md",
      },
      Issue: { address: `dispatch://${target.key}`, title: `${target.key} Warm the cache` },
      Message: {
        address: `dispatch://${target.key}/message/${targetMessage.id}`,
        title: "The rollout finished at noon.",
      },
      Question: {
        address: `dispatch://${target.key}/ask/${targetAsk.id}`,
        title: "Should the cache be warmed first?",
      },
    },
  };
}

/** What a rendered body links: its reference anchors and the placeholder a Dispatch picture
 *  links its artifact's page from. */
export const RENDERED_REFERENCE = "a[data-dispatch-ref], a[data-dispatch-picture]";
/** What an inert one-line preview shows for the same: a link-styled span carrying the same
 *  attributes, with no link inside it. */
export const INERT_REFERENCE = "[data-dispatch-ref], [data-dispatch-picture]";
/** The rendered body of a turn or a card, which is never one of the one-line previews beside
 *  it (a reference card's two lines of its target, a reply's quote of its parent). */
export const RENDERED_BODY = ".dispatch-markdown:not([data-markdown-preview])";

/** The reference each element in scope matching `selector` names, in the order they are drawn:
 *  a Dispatch picture's `data-dispatch-picture`, any other's `data-dispatch-ref`. */
export function referenceTargetsIn(scope: Locator, selector: string): Promise<string[]> {
  return scope
    .locator(selector)
    .evaluateAll((elements) =>
      elements.map(
        (element) =>
          element.getAttribute("data-dispatch-picture") ??
          element.getAttribute("data-dispatch-ref") ??
          ""
      )
    );
}

/** The `data-dispatch-href` of every title a document editor draws in scope, in the order it
 *  draws them (`@legion/proof-editor`'s reference decorations). */
export function editorTitlesIn(scope: Locator): Promise<string[]> {
  return scope
    .locator("a.dispatch-reference")
    .evaluateAll((elements) =>
      elements.map((element) => element.getAttribute("data-dispatch-href") ?? "")
    );
}

/** A margin thread's collapsed card: the toggle holding its one-line preview. On the phone
 *  layout the margin is a bottom sheet, which this opens first. */
export async function collapsedMarginThread(
  page: Page,
  project: string,
  rootId: string
): Promise<Locator> {
  await setSheet(page, project, true);
  const toggle = page
    .getByTestId("margin-sheet")
    .locator(`[data-margin-item="${rootId}"]`)
    .locator('button[aria-expanded="false"]');
  await expect(toggle).toBeVisible();
  return toggle;
}

/** Opens a margin thread from its collapsed card, as a reader's click does, and answers the
 *  opened card: beside the document, or in the Thread dialog on the phone layout. The click
 *  lands on the card's anchor quote, since its preview keeps its links live and the thread's
 *  root must be anchored. The opened card holds no collapsed preview, so what it renders is the
 *  comment itself. */
export async function openMarginThread(
  page: Page,
  project: string,
  rootId: string
): Promise<Locator> {
  await (await collapsedMarginThread(page, project, rootId)).locator("blockquote").click();
  const card = openedThreadCard(page, project, rootId);
  await expect(card).toBeVisible();
  await expect(card.locator('button[aria-expanded="false"]')).toHaveCount(0);
  return card;
}

/** The phone layout opens a margin thread in the Thread dialog over the sheet; Back closes it. */
export async function closeMarginThread(page: Page, project: string): Promise<void> {
  if (project !== "iphone") {
    return;
  }
  const thread = page.getByRole("dialog", { name: "Thread" });
  await thread.getByRole("button", { name: "Back" }).click();
  await expect(thread).toHaveCount(0);
}

/**
 * The inert one-line preview a reader still meets: an answered ask's question on the Answers
 * page (`web/src/features/answers/AnswersPage.tsx`, a `MarkdownPreview` with no `links` prop,
 * which is what makes it inert).
 *
 * The Replies page's own row carried this before INFRA-450, and that page is gone: `/replies`
 * lands on the Inbox, and the Inbox draws a thread row's thread whole with `MarkdownBody` and
 * live links (`features/inbox/ThreadBody.tsx`), so no one-line preview of a reply is left to
 * read. A preview is one control's text, so a reference in it reads as link-styled text with no
 * link inside it.
 */
export function answeredQuestionPreview(page: Page, askId: string): Locator {
  return page.locator(`[data-answer-row="${askId}"]`).locator("[data-markdown-preview]").first();
}

/** Opens the Inbox narrowed to its Reply rows, below the reading column's width so every row is
 *  the card that draws its own thread rather than a title alone (`e2e/inbox-pane.ts`'s
 *  `READING_PANE_WIDTH`). `/replies` lands on this same address. */
export async function openInboxReplies(page: Page): Promise<void> {
  await page.setViewportSize({ height: 900, width: READING_PANE_WIDTH - 256 });
  await page.goto("/?type=reply");
}

/** One turn of the thread an Inbox Reply row draws: the row is `[data-inbox-row="reply:<thread
 *  id>"]` (`features/inbox/list-row.ts`, `replyRowId`) and each turn in it carries the comment's
 *  or message's own id, as `refresh-freshness.e2e.ts` reads the same row. The turn's body is
 *  rendered rather than previewed, so a reference there is a live link. */
export function inboxReplyTurn(page: Page, threadId: string, turnId: string): Locator {
  return page
    .locator(`[data-inbox-row="${replyRowId(threadId)}"]`)
    .locator(`[data-inbox-thread-turn="${turnId}"]`);
}

/** A Conversation turn: the comment, message or ask the id names. */
export function conversationTurn(
  page: Page,
  kind: "comment" | "message" | "ask",
  id: string
): Locator {
  return page.locator(`[data-turn="${kind}:${id}"]`);
}
