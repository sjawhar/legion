/** A person takes as long as they like to write a comment on a document (dispatch://INFRA-727).
 *
 *  Each row opens the margin composer from a selection, leaves it open while something that can
 *  take the provisional mark away happens, and then presses Send. Every row records, each second,
 *  whether the mark's span is still in the page, and prints that line with the row's result: it is
 *  what tells a sweep (the span goes at about 62 s, one `unrecordedMarkTTL` after the settlement
 *  that first saw it) from a reconnect (the span goes when the socket does).
 *
 *  Real server timings are used throughout - `Settle` 2 s, `unrecordedMarkTTL` 1 minute,
 *  `markWait` 1 s - so these rows exercise the default path a person meets. That is why each sets
 *  its own 240 s timeout, as `deep-links.e2e.ts:47` and `failed-room.e2e.ts:115` do. */
import { expect, type Page, test } from "@playwright/test";

import { createIssue, createProject, editArtifact, listComments } from "./api";
import {
  barAction,
  composer,
  connectedDot,
  documentEditor,
  documentTransport,
  expectMark,
  markSpan,
  openSpec,
  selectEditorText,
} from "./editor";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

const SPEC = [
  "## Review",
  "",
  "The quick brown fox jumps over the lazy dog.",
  "",
  "A second paragraph gives the composer something else to leave alone.",
  "",
].join("\n");
/** A passage that occurs once in SPEC, so an anchor on it is unambiguous. */
const QUOTE = "jumps over the lazy dog";
/** How long after the mark is written the unrecorded-mark sweep has had its chance: the
 *  settlement that records the mark's first-seen time runs 2 s after it arrives, and the sweep
 *  expires it one `unrecordedMarkTTL` (1 minute) after that. */
const PAST_THE_SWEEP_MS = 70_000;
/** The composer's message when the selected text is gone and nothing can be anchored to it. */
const GONE_MESSAGE =
  "The text you selected was changed or deleted, so this comment has nothing to attach to. " +
  "Your draft is kept — select the text again to attach it.";

test.beforeEach(async () => {
  await resetDatabase();
});

/** Samples whether `markId`'s span is in `page` once a second until `untilMs` after `writtenAt`,
 *  appending `<seconds>s:<present|gone>` to `log`. The row waits by sampling, so nothing else
 *  touches the page while it runs. */
async function watchMark(
  page: Page,
  markId: string,
  writtenAt: number,
  untilMs: number,
  log: string[]
): Promise<void> {
  while (Date.now() - writtenAt < untilMs) {
    const present = (await markSpan(page, markId).count()) > 0;
    const second = Math.round((Date.now() - writtenAt) / 1000);
    log.push(`${second}s:${present ? "present" : "gone"}`);
    await page.waitForTimeout(1_000);
  }
}

/** Whether the mark's span was in the page at about `second` seconds after it was written. */
function presentAt(log: readonly string[], second: number): boolean {
  return log.includes(`${second}s:present`);
}

/** Opens a document, selects QUOTE, presses Comment and types `draft`. Returns the provisional
 *  mark's id, its `data-by` attribute and the moment the span appeared. */
async function openComposer(
  page: Page,
  issueKey: string,
  draft: string
): Promise<{ by: string | null; markId: string; writtenAt: number }> {
  await openSpec(page, issueKey, "quick brown fox");
  await selectEditorText(page, QUOTE);
  await barAction(page, "Comment");
  const span = documentEditor(page).locator('span[data-proof="comment"][data-id]');
  await expect(span).toHaveCount(1);
  const writtenAt = Date.now();
  const markId = await span.getAttribute("data-id");
  const by = await span.getAttribute("data-by");
  if (markId === null) throw new Error("The provisional comment mark has no data-id.");
  await expect(composer(page)).toContainText(QUOTE);
  await composer(page).getByLabel("Comment").fill(draft);
  return { by, markId, writtenAt };
}

async function send(page: Page): Promise<void> {
  await composer(page).getByRole("button", { exact: true, name: "Send" }).click();
}

/** The one comment the document carries, once the server has it. */
async function theComment(issueKey: string, artifactId: string) {
  await expect.poll(() => listComments(issueKey, artifactId).then((rows) => rows.length)).toBe(1);
  const [comment] = await listComments(issueKey, artifactId);
  if (comment.anchor === null) throw new Error("The comment the composer sent has no anchor.");
  return comment;
}

async function reviewIssue() {
  await createProject({ key: "CORE", name: "Core" });
  return createIssue({ project: "CORE", spec: SPEC, title: "Review the spec" });
}

test("a comment written for longer than the unrecorded-mark TTL keeps its anchor", async ({
  browser,
}, testInfo) => {
  test.setTimeout(240_000);
  const issue = await reviewIssue();
  const alice = await asUser(browser, "alice");
  const log: string[] = [];
  try {
    const page = await alice.newPage();
    const { by, markId, writtenAt } = await openComposer(page, issue.key, "Why this one?");
    // Layer 1 of the fix matches the sweep's exemption on this attribute, so the row pins it.
    expect(by).toBe("alice");
    await watchMark(page, markId, writtenAt, PAST_THE_SWEEP_MS, log);
    await testInfo.attach("mark-span-presence", { body: log.join(" "), contentType: "text/plain" });
    console.log(`long compose: ${log.join(" ")}`);
    expect(presentAt(log, 10)).toBe(true);

    await send(page);
    const comment = await theComment(issue.key, issue.primary_artifact_id);
    expect(comment.body).toBe("Why this one?");
    expect(comment.anchor?.quote).toBe(QUOTE);
    expect(comment.anchor?.orphaned).toBe(false);
    await expectMark(page, comment.anchor?.mark_id ?? "", QUOTE);
    await expect(composer(page)).toHaveCount(0);
  } finally {
    console.log(`long compose (final): ${log.join(" ")}`);
    await alice.close();
  }
});

test("a comment written across a socket reconnect keeps its anchor", async ({
  browser,
}, testInfo) => {
  test.setTimeout(240_000);
  const issue = await reviewIssue();
  const alice = await asUser(browser, "alice");
  const log: string[] = [];
  try {
    const page = await alice.newPage();
    const transport = await documentTransport(page);
    const { markId, writtenAt } = await openComposer(page, issue.key, "Reconnected mid-sentence.");
    await watchMark(page, markId, writtenAt, 5_000, log);
    log.push("severed");
    await transport.sever();
    await expect(connectedDot(page)).toHaveText("connected");
    log.push("reconnected");
    await watchMark(page, markId, writtenAt, 15_000, log);
    await testInfo.attach("mark-span-presence", { body: log.join(" "), contentType: "text/plain" });
    console.log(`reconnect: ${log.join(" ")}`);

    await send(page);
    const comment = await theComment(issue.key, issue.primary_artifact_id);
    expect(comment.body).toBe("Reconnected mid-sentence.");
    expect(comment.anchor?.quote).toBe(QUOTE);
    expect(comment.anchor?.orphaned).toBe(false);
    await expectMark(page, comment.anchor?.mark_id ?? "", QUOTE);
    await expect(composer(page)).toHaveCount(0);
  } finally {
    console.log(`reconnect (final): ${log.join(" ")}`);
    await alice.close();
  }
});

test("a comment written while its author is offline past the TTL keeps its anchor", async ({
  browser,
}, testInfo) => {
  test.setTimeout(240_000);
  const issue = await reviewIssue();
  const alice = await asUser(browser, "alice");
  const log: string[] = [];
  try {
    const page = await alice.newPage();
    const transport = await documentTransport(page);
    const { markId, writtenAt } = await openComposer(page, issue.key, "Back from a long lunch.");
    // The author's tab goes away: its socket closes and the reconnect is held, so the server sees
    // no connection of hers at all while the sweep runs.
    transport.hold();
    await transport.sever();
    log.push("offline");
    await watchMark(page, markId, writtenAt, PAST_THE_SWEEP_MS, log);
    await transport.release();
    await expect(connectedDot(page)).toHaveText("connected");
    log.push("online");
    await watchMark(page, markId, writtenAt, PAST_THE_SWEEP_MS + 5_000, log);
    await testInfo.attach("mark-span-presence", { body: log.join(" "), contentType: "text/plain" });
    console.log(`offline: ${log.join(" ")}`);

    await send(page);
    const comment = await theComment(issue.key, issue.primary_artifact_id);
    expect(comment.body).toBe("Back from a long lunch.");
    expect(comment.anchor?.quote).toBe(QUOTE);
    expect(comment.anchor?.orphaned).toBe(false);
    await expectMark(page, comment.anchor?.mark_id ?? "", QUOTE);
    await expect(composer(page)).toHaveCount(0);
  } finally {
    console.log(`offline (final): ${log.join(" ")}`);
    await alice.close();
  }
});

/** The re-anchor path on its own, with no sweep and no clock in it: another writer rewrites the
 *  sentence the composer is anchored in, which takes the provisional mark away and leaves the
 *  selected words in the document. Send must attach the comment to those words. */
test("a comment whose mark another writer's edit removed re-anchors to the same text", async ({
  browser,
}) => {
  test.setTimeout(240_000);
  const issue = await reviewIssue();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const { markId } = await openComposer(page, issue.key, "Still worth asking.");
    await editArtifact(
      issue.primary_artifact_id,
      {
        ops: [
          {
            find: "The quick brown fox jumps over the lazy dog.",
            op: "replace",
            with: "The swift red fox jumps over the lazy dog.",
          },
        ],
      },
      { actor: { id: "e2e-compose-wait", kind: "session" }, as: "agent" }
    );
    await expect(documentEditor(page)).toContainText("swift red fox");
    // The edit took the mark and left the words: that is the state Send has to recover from.
    await expect(markSpan(page, markId)).toHaveCount(0);
    await expect(documentEditor(page)).toContainText(QUOTE);

    await send(page);
    const comment = await theComment(issue.key, issue.primary_artifact_id);
    expect(comment.body).toBe("Still worth asking.");
    expect(comment.anchor?.quote).toBe(QUOTE);
    expect(comment.anchor?.orphaned).toBe(false);
    await expectMark(page, comment.anchor?.mark_id ?? "", QUOTE);
    await expect(composer(page)).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

test("a comment whose selected text another writer deleted says so and keeps its draft", async ({
  browser,
}) => {
  test.setTimeout(240_000);
  const issue = await reviewIssue();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await openComposer(page, issue.key, "Please revise.");
    await editArtifact(
      issue.primary_artifact_id,
      {
        ops: [
          {
            find: "The quick brown fox jumps over the lazy dog.",
            op: "replace",
            with: "Nothing of the sort.",
          },
        ],
      },
      { actor: { id: "e2e-compose-wait", kind: "session" }, as: "agent" }
    );
    await expect(documentEditor(page)).toContainText("Nothing of the sort.");
    await expect(documentEditor(page)).not.toContainText(QUOTE);

    await send(page);
    await expect(composer(page)).toContainText(GONE_MESSAGE);
    await expect(composer(page).getByLabel("Comment")).toHaveValue("Please revise.");
    expect(await listComments(issue.key, issue.primary_artifact_id)).toHaveLength(0);
  } finally {
    await alice.close();
  }
});
