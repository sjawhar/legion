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
  actionBar,
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

/** Whether the mark's span was in the page at about `second` seconds after it was written: the
 *  first sample taken at or after it, since the sampler loses a second now and then on a loaded
 *  two-core box and an exact label would make the row flake rather than measure. */
function presentAt(log: readonly string[], second: number): boolean {
  return (
    log
      .find((sample) => {
        const at = Number.parseInt(sample, 10);
        return Number.isFinite(at) && at >= second;
      })
      ?.endsWith(":present") ?? false
  );
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

/** A spec whose selected words read twice, as a phrase a reader picks out of a specification
 *  often does ("the lazy dog" here, "the operator" or "this field" in a real one). */
const REPEATED_SPEC = [
  "## Review",
  "",
  "The quick brown fox jumps over the lazy dog. The vet looks at the lazy dog again.",
  "",
].join("\n");
/** The words the rows below select: they read twice in REPEATED_SPEC's one paragraph. */
const REPEATED_QUOTE = "the lazy dog";

async function repeatedIssue() {
  await createProject({ key: "CORE", name: "Core" });
  return createIssue({ project: "CORE", spec: REPEATED_SPEC, title: "Review the spec" });
}

/** Selects the second reading of `quote` in the editor, as `selectEditorText` selects the first:
 *  the text nodes read as one text, and the selection change reaches ProseMirror's observer. */
async function selectSecondReading(page: Page, quote: string): Promise<void> {
  const editor = documentEditor(page);
  await editor.waitFor();
  await editor.focus();
  await editor.evaluate((root, quote) => {
    const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
    const runs: { node: Node; start: number; end: number }[] = [];
    let text = "";
    for (let node = walker.nextNode(); node !== null; node = walker.nextNode()) {
      if (node.parentElement?.isContentEditable !== true) continue;
      const start = text.length;
      text += node.textContent ?? "";
      runs.push({ end: text.length, node, start });
    }
    const index = text.indexOf(quote, text.indexOf(quote) + 1);
    const first = runs.find((run) => run.start <= index && index < run.end);
    const last = runs.find(
      (run) => run.start < index + quote.length && index + quote.length <= run.end
    );
    if (index < 0 || first === undefined || last === undefined) {
      throw new Error(`the editor holds no second reading of: ${quote}`);
    }
    const range = document.createRange();
    range.setStart(first.node, index - first.start);
    range.setEnd(last.node, index + quote.length - last.start);
    const selection = window.getSelection();
    selection?.removeAllRanges();
    selection?.addRange(range);
    document.dispatchEvent(new Event("selectionchange"));
  }, quote);
  await actionBar(page).waitFor({ state: "visible" });
}

/** Where each comment mark sits in its own paragraph, in characters from the paragraph's start:
 *  the quote alone cannot say which reading a mark covers. */
function markOffsets(page: Page): Promise<{ id: string; offset: number }[]> {
  return documentEditor(page)
    .locator('span[data-proof="comment"][data-id]')
    .evaluateAll((spans) =>
      spans.map((span) => {
        const paragraph = span.closest("p");
        let offset = 0;
        if (paragraph !== null) {
          const walker = document.createTreeWalker(paragraph, NodeFilter.SHOW_TEXT);
          for (let text = walker.nextNode(); text !== null; text = walker.nextNode()) {
            if (span.contains(text)) break;
            offset += (text.nodeValue ?? "").length;
          }
        }
        return { id: span.getAttribute("data-id") ?? "", offset };
      })
    );
}

/** The author-offline row's wait, with words that read twice in the document. The document's text
 *  never changes: the sweep takes the mark while the tab is away, and Send has to put the comment
 *  on the reading its author selected - the second one here, at character 62 of the paragraph -
 *  not refuse it as text that "was changed or deleted" (dispatch://INFRA-727 acceptance 2). */
test("a comment on words that read twice keeps the reading its author selected", async ({
  browser,
}) => {
  test.setTimeout(240_000);
  const issue = await repeatedIssue();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const transport = await documentTransport(page);
    await openSpec(page, issue.key, "quick brown fox");
    await expect(connectedDot(page)).toHaveText("connected");
    await selectSecondReading(page, REPEATED_QUOTE);
    await barAction(page, "Comment");
    const span = documentEditor(page).locator('span[data-proof="comment"][data-id]');
    await expect(span).toHaveCount(1);
    expect((await markOffsets(page)).map((mark) => mark.offset)).toEqual([62]);
    await composer(page).getByLabel("Comment").fill("Which dog is this?");

    transport.hold();
    await transport.sever();
    await page.waitForTimeout(PAST_THE_SWEEP_MS);
    await transport.release();
    await expect(connectedDot(page)).toHaveText("connected");

    await send(page);
    const comment = await theComment(issue.key, issue.primary_artifact_id);
    expect(comment.body).toBe("Which dog is this?");
    expect(comment.anchor?.quote).toBe(REPEATED_QUOTE);
    expect(comment.anchor?.orphaned).toBe(false);
    await expectMark(page, comment.anchor?.mark_id ?? "", REPEATED_QUOTE);
    expect(await markOffsets(page)).toEqual([{ id: comment.anchor?.mark_id ?? "", offset: 62 }]);
    await expect(composer(page)).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

/** The same refusal as the row above it, from an Ask composer: the words it is anchored to are
 *  gone, so the draft stays and the reader is told. What the reader is told has to be about the
 *  thing they are writing - an ask here, a suggestion from the Suggest composer - and never
 *  "this comment" (dispatch://INFRA-727 acceptance 2: the composer keeps the draft and says why). */
test("an ask whose selected text another writer deleted is not called a comment", async ({
  browser,
}) => {
  test.setTimeout(240_000);
  const issue = await reviewIssue();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await openSpec(page, issue.key, "quick brown fox");
    await selectEditorText(page, QUOTE);
    await barAction(page, "Ask");
    await expect(composer(page)).toContainText(QUOTE);
    await composer(page).getByLabel("Question").fill("Which dog?");
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

    await composer(page).locator('button[type="submit"]').click();
    await expect(composer(page)).toContainText("Your draft is kept");
    await expect(composer(page).getByLabel("Question")).toHaveValue("Which dog?");
    expect(await composer(page).textContent()).not.toContain("this comment");
  } finally {
    await alice.close();
  }
});

/** A spec whose one paragraph reads the selected words twice, with a word before each that tells
 *  the two apart, so a row can say which reading a mark covers. */
const TWO_READINGS_SPEC = ["## Review", "", "Alpha the lazy dog. Beta the lazy dog.", ""].join(
  "\n"
);

/** Where `REPEATED_QUOTE` reads in the paragraph that holds it, in characters from that
 *  paragraph's start, in the order it reads them. */
function readingOffsets(page: Page): Promise<number[]> {
  return documentEditor(page).evaluate((root, quote) => {
    const paragraph = Array.from(root.querySelectorAll("p")).find((node) =>
      (node.textContent ?? "").includes(quote)
    );
    const text = paragraph?.textContent ?? "";
    const found: number[] = [];
    for (let at = text.indexOf(quote); at !== -1; at = text.indexOf(quote, at + 1)) found.push(at);
    return found;
  }, REPEATED_QUOTE);
}

/** Another writer puts a reading of the same words in front of the reader's, inside the block the
 *  reader selected in, and takes the provisional mark with the same edit. The reader's own words
 *  are still there - they are the paragraph's third reading now, where they were its second - so
 *  Send either attaches the comment to them or tells the reader it cannot and keeps the draft.
 *  What it must never do is attach the comment to a reading the reader never selected, which no
 *  refusal tells them about (dispatch://INFRA-727 acceptance 2; the INFRA-512 architect's decision
 *  of 2026-10-30T09:49:42Z rests on it: "a record on the wrong passage is worse than a refusal"). */
test("a comment keeps its own reading when another writer adds an earlier one", async ({
  browser,
}) => {
  test.setTimeout(240_000);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: TWO_READINGS_SPEC,
    title: "Review the spec",
  });
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await openSpec(page, issue.key, "Alpha");
    await expect(connectedDot(page)).toHaveText("connected");
    await selectSecondReading(page, REPEATED_QUOTE);
    await barAction(page, "Comment");
    await expect(documentEditor(page).locator('span[data-proof="comment"][data-id]')).toHaveCount(
      1
    );
    // The reader's own reading, the second of the two the paragraph then holds.
    expect((await markOffsets(page)).map((mark) => mark.offset)).toEqual([
      (await readingOffsets(page))[1],
    ]);
    await composer(page).getByLabel("Comment").fill("Which dog is this?");

    await editArtifact(
      issue.primary_artifact_id,
      {
        ops: [
          {
            find: "Alpha the lazy dog. Beta the lazy dog.",
            op: "replace",
            with: "Zero the lazy dog. Alpha the lazy dog. Beta the lazy dog.",
          },
        ],
      },
      { actor: { id: "e2e-compose-wait", kind: "session" }, as: "agent" }
    );
    await expect(documentEditor(page)).toContainText("Zero the lazy dog");

    await send(page);
    // Either outcome is settled: the comment is on the page, or the composer says why not.
    await expect
      .poll(async () => {
        if ((await listComments(issue.key, issue.primary_artifact_id)).length > 0) return "sent";
        const shown = (await composer(page).textContent()) ?? "";
        return shown.includes("Your draft is kept") ? "refused" : "waiting";
      })
      .not.toBe("waiting");

    const rows = await listComments(issue.key, issue.primary_artifact_id);
    if (rows.length === 0) {
      await expect(composer(page).getByLabel("Comment")).toHaveValue("Which dog is this?");
      return;
    }
    // "Beta the lazy dog" is the paragraph's third reading now that "Zero the lazy dog" stands
    // in front of it.
    const readings = await readingOffsets(page);
    expect(readings).toHaveLength(3);
    expect(await markOffsets(page)).toEqual([
      { id: rows[0]?.anchor?.mark_id ?? "", offset: readings[2] },
    ]);
  } finally {
    await alice.close();
  }
});

/** A spec whose two paragraphs each read the same words, with different text around them. */
const TWO_PARAGRAPH_SPEC = [
  "## Review",
  "",
  "Alpha the lazy dog sits here.",
  "",
  "Beta the lazy dog sits there.",
  "",
].join("\n");

/** Another writer takes the reader's own words out of the paragraph they selected in, while the
 *  same words still read once in another paragraph, surrounded by different text. That other
 *  paragraph is a different passage, so Send must not quietly put the comment there: it says it
 *  cannot be attached, keeps the draft, and does not claim the words were changed or deleted,
 *  since they are plainly still on the page (the INFRA-512 architect's amended decision on
 *  sjawhar/legion#2548: words and surroundings both have to match). */
test("a comment whose own paragraph lost the words is not moved to another paragraph", async ({
  browser,
}) => {
  test.setTimeout(240_000);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: TWO_PARAGRAPH_SPEC,
    title: "Review the spec",
  });
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await openSpec(page, issue.key, "Alpha the lazy dog");
    await expect(connectedDot(page)).toHaveText("connected");
    await selectEditorText(page, REPEATED_QUOTE);
    await barAction(page, "Comment");
    await expect(documentEditor(page).locator('span[data-proof="comment"][data-id]')).toHaveCount(
      1
    );
    await composer(page).getByLabel("Comment").fill("Which dog is this?");

    await editArtifact(
      issue.primary_artifact_id,
      {
        ops: [
          { find: "Alpha the lazy dog sits here.", op: "replace", with: "Nothing of the sort." },
        ],
      },
      { actor: { id: "e2e-compose-wait", kind: "session" }, as: "agent" }
    );
    await expect(documentEditor(page)).toContainText("Nothing of the sort.");
    await expect(documentEditor(page)).toContainText("Beta the lazy dog sits there.");

    await send(page);
    await expect(composer(page)).toContainText("Your draft is kept");
    await expect(composer(page).getByLabel("Comment")).toHaveValue("Which dog is this?");
    expect(await composer(page).textContent()).not.toContain("changed or deleted");
    expect(await listComments(issue.key, issue.primary_artifact_id)).toHaveLength(0);
    expect(await markOffsets(page)).toEqual([]);
  } finally {
    await alice.close();
  }
});

/** Another writer deletes the sentence the reader's own words were in, leaving the block reading
 *  those words once - in the other sentence, which the reader never selected. Their passage is
 *  gone, so Send must say so and keep the draft: the one reading left is somebody else's, and a
 *  comment put there is a record on a passage no refusal ever tells the reader about
 *  (dispatch://INFRA-727 acceptance 2; the rule the INFRA-512 architect's decisions rest on). */
test("a comment is not moved to the block's other reading when its own words are deleted", async ({
  browser,
}) => {
  test.setTimeout(240_000);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: TWO_READINGS_SPEC,
    title: "Review the spec",
  });
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await openSpec(page, issue.key, "Alpha");
    await expect(connectedDot(page)).toHaveText("connected");
    await selectSecondReading(page, REPEATED_QUOTE);
    await barAction(page, "Comment");
    await expect(documentEditor(page).locator('span[data-proof="comment"][data-id]')).toHaveCount(
      1
    );
    await composer(page).getByLabel("Comment").fill("Which dog is this?");

    await editArtifact(
      issue.primary_artifact_id,
      {
        ops: [
          {
            find: "Alpha the lazy dog. Beta the lazy dog.",
            op: "replace",
            with: "Alpha the lazy dog.",
          },
        ],
      },
      { actor: { id: "e2e-compose-wait", kind: "session" }, as: "agent" }
    );
    await expect(documentEditor(page)).not.toContainText("Beta");

    await send(page);
    await expect(composer(page)).toContainText("Your draft is kept");
    await expect(composer(page).getByLabel("Comment")).toHaveValue("Which dog is this?");
    expect(await listComments(issue.key, issue.primary_artifact_id)).toHaveLength(0);
    // Nothing is left on "Alpha the lazy dog", the reading the reader never selected.
    expect(await markOffsets(page)).toEqual([]);
  } finally {
    await alice.close();
  }
});

/** A spec whose two paragraphs a reader can select across. */
const ACROSS_BLOCKS_SPEC = [
  "## Review",
  "",
  "The quick brown fox jumps.",
  "",
  "Over the lazy dog it goes.",
  "",
].join("\n");

/** Selects from `from` in the first paragraph to `to` in the second, as a drag across the break
 *  does: one DOM range over the two paragraphs' text nodes, and the selectionchange ProseMirror's
 *  observer reads. The editor reads that selection as one quote with the break in it, which no
 *  single block's text holds. */
async function selectAcrossBlocks(page: Page, from: string, to: string): Promise<void> {
  const editor = documentEditor(page);
  await editor.waitFor();
  await editor.focus();
  await editor.evaluate(
    (root, { from: start, to: end }) => {
      const paragraphs = Array.from(root.querySelectorAll("p"));
      const first = paragraphs.find((node) => (node.textContent ?? "").includes(start));
      const last = paragraphs.find((node) => (node.textContent ?? "").includes(end));
      const head = first?.firstChild;
      const tail = last?.firstChild;
      if (head === null || head === undefined || tail === null || tail === undefined) {
        throw new Error(`the editor holds no paragraph pair for: ${start} / ${end}`);
      }
      const range = document.createRange();
      range.setStart(head, (head.textContent ?? "").indexOf(start));
      range.setEnd(tail, (tail.textContent ?? "").indexOf(end) + end.length);
      const selection = window.getSelection();
      selection?.removeAllRanges();
      selection?.addRange(range);
      document.dispatchEvent(new Event("selectionchange"));
    },
    { from, to }
  );
  await actionBar(page).waitFor({ state: "visible" });
}

/** A selection that runs across a paragraph break belongs to no one block, so the composer keeps
 *  no block or surroundings for it and a re-anchor has only the words to go by. They still have
 *  to be found: the editor reads the document as one text, break and all, and attaches where
 *  those words read once. Here the sweep takes the mark while the author is away and the
 *  document's own text never changes. */
test("a comment on words that run across two paragraphs keeps its anchor", async ({ browser }) => {
  test.setTimeout(240_000);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: ACROSS_BLOCKS_SPEC,
    title: "Review the spec",
  });
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const transport = await documentTransport(page);
    await openSpec(page, issue.key, "quick brown fox");
    await selectAcrossBlocks(page, "jumps.", "Over the lazy dog");
    await barAction(page, "Comment");
    const span = documentEditor(page).locator('span[data-proof="comment"][data-id]');
    await expect(span).toHaveCount(2);
    await composer(page).getByLabel("Comment").fill("Which sentence is this?");

    transport.hold();
    await transport.sever();
    await page.waitForTimeout(PAST_THE_SWEEP_MS);
    await transport.release();
    await expect(connectedDot(page)).toHaveText("connected");

    await send(page);
    const comment = await theComment(issue.key, issue.primary_artifact_id);
    expect(comment.body).toBe("Which sentence is this?");
    expect(comment.anchor?.orphaned).toBe(false);
    // One mark over both halves of the selection, which is how a mark across a break renders.
    await expect(markSpan(page, comment.anchor?.mark_id ?? "")).toHaveCount(2);
    await expect(composer(page)).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

/** A second pair of paragraphs reading the same words as the first. */
const ACROSS_BLOCKS_TWICE_SPEC = [
  "## Review",
  "",
  "The fox jumps.",
  "",
  "Over the lazy dog it goes.",
  "",
  "A hound also jumps.",
  "",
  "Over the lazy dog it trots.",
  "",
].join("\n");

/** A selection across a paragraph break keeps no block and no surroundings, so where the same
 *  words read across another pair of paragraphs the editor cannot say which pair is the
 *  reader's: refusing is right. Telling them their words are "no longer where you selected it"
 *  is not - they are exactly where they were, and only the words around them changed. Each
 *  refusal says what is true of the case it is reached in (the INFRA-512 architect's rule, this
 *  review's finding 4); the sentence for words that read in more than one place already exists. */
test("a cross-block refusal does not claim the words moved while they are where they were", async ({
  browser,
}) => {
  test.setTimeout(240_000);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: ACROSS_BLOCKS_TWICE_SPEC,
    title: "Review the spec",
  });
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await openSpec(page, issue.key, "The fox jumps");
    await expect(connectedDot(page)).toHaveText("connected");
    await selectAcrossBlocks(page, "jumps.", "Over the lazy dog");
    await barAction(page, "Comment");
    await expect(documentEditor(page).locator('span[data-proof="comment"][data-id]')).toHaveCount(
      2
    );
    await composer(page).getByLabel("Comment").fill("Which pair is this?");

    // The reader's own words stay; the words around them change, which takes the mark.
    await editArtifact(
      issue.primary_artifact_id,
      {
        ops: [
          { find: "The fox jumps.", op: "replace", with: "The swift fox jumps." },
          {
            find: "Over the lazy dog it goes.",
            op: "replace",
            with: "Over the lazy dog it bounds.",
          },
        ],
      },
      { actor: { id: "e2e-compose-wait", kind: "session" }, as: "agent" }
    );
    await expect(documentEditor(page)).toContainText("The swift fox jumps.");

    await send(page);
    await expect(composer(page)).toContainText("Your draft is kept");
    await expect(composer(page).getByLabel("Comment")).toHaveValue("Which pair is this?");
    expect(await listComments(issue.key, issue.primary_artifact_id)).toHaveLength(0);
    // Their words are still where they selected them: the refusal must not say otherwise.
    expect(await composer(page).textContent()).not.toContain("no longer where you selected it");
  } finally {
    await alice.close();
  }
});
