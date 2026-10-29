import { type Browser, expect, type Locator, type Page, test } from "@playwright/test";

import { createComment, createIssue, createProject, getIssueArtifactText } from "./api";
import { documentEditor, openSpec } from "./editor";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

// The server's reader (the Go reader behind every document) and the browser's engine (the one
// that draws the editor, every comment and every past version) parted over the shapes below, so a
// document and a comment of the same text drew differently, and two of them drew nothing at all.
// Each row writes its shape through the surfaces a person reads - the issue's spec editor, its
// past version view and the Conversation comment - and, where the stored bytes or the write's own
// answer is the point, through the document's markdown and the API's refusal
// (sjawhar/legion#2507). Text is read with `evaluateAll`, not `toHaveText`, which normalises the
// whitespace several of these rows turn on.

const agentSession = {
  actor: { kind: "session" as const, id: "e2e-document-and-comment-read-alike" },
  as: "agent" as const,
};

/** A footnote definition's element: `dt` holds its label and `dd` its body. */
const footnoteDefinition = 'dl[data-type="footnote_definition"]';
/** The inline HTML node, the only node the Proof schema holds a tag in. */
const inlineHtml = 'span[data-type="html"]';

test.beforeEach(async () => {
  await resetDatabase();
});

/** The project, an issue whose spec is `spec` (when given) and one comment of `body` (when
 *  given), written by an agent as a person's collaborator writes them. */
async function seed({
  body,
  spec,
  title,
}: {
  body?: string;
  spec?: string;
  title: string;
}): Promise<{ commentId: string; key: string }> {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", spec, title }, agentSession);
  const commentId =
    body === undefined ? "" : (await createComment(issue.key, { body }, agentSession)).id;
  return { commentId, key: issue.key };
}

async function withAlice(browser: Browser, check: (page: Page) => Promise<void>): Promise<void> {
  const alice = await asUser(browser, "alice");
  try {
    await check(await alice.newPage());
  } finally {
    await alice.close();
  }
}

/** The whole text of every element `locator` matches, in document order and as written: these
 *  rows turn on a space, a line break or a replacement character that `toHaveText` normalises
 *  away. */
function textsOf(locator: Locator): Promise<string[]> {
  return locator.evaluateAll((elements) => elements.map((element) => element.textContent ?? ""));
}

/** Opens the issue's Conversation and expands the comment's thread: a collapsed card previews the
 *  body inline, and the expanded thread draws its blocks. Answers the turn and the body it drew. */
async function expandComment(
  page: Page,
  key: string,
  commentId: string
): Promise<{ drawn: Locator; turn: Locator }> {
  await page.goto(`/issues/${key}/conversation`);
  const turn = page.locator(`[data-turn="comment:${commentId}"]`);
  await turn.getByRole("button", { name: "Expand thread" }).click();
  return { drawn: turn.locator(".dispatch-markdown"), turn };
}

/** Opens a past version of the issue's spec and answers the view that draws it: version 1 is the
 *  spec as it was written. */
async function openFirstVersion(page: Page, key: string): Promise<Locator> {
  await page.goto(`/issues/${key}/artifacts/spec?v=1`);
  return page.getByTestId("version-view");
}

/** The 400 answer's own body behind a write the server refused, taken out of the message `request`
 *  builds around it (`<method> <path> failed: <status> <body>`), so a refusal's code and the words
 *  it names are compared as the server wrote them. A write the server accepted fails here. */
async function refusalOf(write: Promise<unknown>): Promise<{ code?: string; error?: string }> {
  const refusal = await write.then(
    () => undefined,
    (error: unknown) => error
  );
  expect(refusal, "the write was accepted").toBeInstanceOf(Error);
  const message = refusal instanceof Error ? refusal.message : "";
  expect(message).toContain(" failed: 400 ");
  return JSON.parse(message.slice(message.indexOf("{")));
}

// 1. A definition's paragraph runs over a line of pipes and dashes, and an indented line of `=`
// underlines both: the whole paragraph is the heading's text, inside the definition.
const definitionUnderline = "Text[^g].\n\n[^g]: other\n|-|-|\n    ===\n";
const underlinedHeading = ["other |-|-|"];

test("a footnote definition over a delimiter-like line and an indented underline draws one heading in the spec editor and in a comment", async ({
  browser,
}) => {
  const { commentId, key } = await seed({
    body: definitionUnderline,
    spec: definitionUnderline,
    title: "A definition before an indented underline",
  });
  await withAlice(browser, async (page) => {
    await openSpec(page, key, "other");
    await expect
      .poll(() => textsOf(documentEditor(page).locator(`${footnoteDefinition} h1`)))
      .toEqual(underlinedHeading);

    const { drawn } = await expandComment(page, key, commentId);
    await expect
      .poll(() => textsOf(drawn.locator(`${footnoteDefinition} h1`)))
      .toEqual(underlinedHeading);
  });
});

// 2. A header row of one cell over a delimiter row of two is no table: the two lines are one
// paragraph, and the line break between them a space.
const narrowHeaderRow = "| a |\n|-|-|\n";
const narrowHeaderParagraphs = ["| a | |-|-|"];

test("a header row of one cell over a delimiter row of two draws a paragraph and no table in the spec editor and in a comment", async ({
  browser,
}) => {
  const { commentId, key } = await seed({
    body: narrowHeaderRow,
    spec: `# Rows\n\n${narrowHeaderRow}`,
    title: "A header row narrower than its delimiter row",
  });
  await withAlice(browser, async (page) => {
    await openSpec(page, key, "Rows");
    const editor = documentEditor(page);
    await expect.poll(() => textsOf(editor.locator("p"))).toEqual(narrowHeaderParagraphs);
    await expect(editor.locator("table")).toHaveCount(0);

    const { drawn } = await expandComment(page, key, commentId);
    await expect.poll(() => textsOf(drawn.locator("p"))).toEqual(narrowHeaderParagraphs);
    await expect(drawn.locator("table")).toHaveCount(0);
  });
});

// 3. A quoted footnote definition whose next line holds one tag: the tag continues the
// definition's paragraph, so the quote, the definition and the word `note` all stay.
const quotedDefinition = "Ref[^f].\n\n>[^f]: note\n>    <b>\n";
const quotedDefinitionBodies = ["note <b>"];
const quotedDefinitionBody = `blockquote ${footnoteDefinition} dd`;

test("a quoted footnote definition before a lone tag line keeps its text in the spec editor, in a past version and in a comment", async ({
  browser,
}) => {
  const { commentId, key } = await seed({
    body: quotedDefinition,
    spec: quotedDefinition,
    title: "A quoted definition before a lone tag line",
  });
  await withAlice(browser, async (page) => {
    await openSpec(page, key, "note");
    await expect
      .poll(() => textsOf(documentEditor(page).locator(quotedDefinitionBody)))
      .toEqual(quotedDefinitionBodies);

    const version = await openFirstVersion(page, key);
    await expect
      .poll(() => textsOf(version.locator(quotedDefinitionBody)))
      .toEqual(quotedDefinitionBodies);

    const { drawn } = await expandComment(page, key, commentId);
    await expect
      .poll(() => textsOf(drawn.locator(quotedDefinitionBody)))
      .toEqual(quotedDefinitionBodies);
  });
});

// 4. The same lazy line in a plain quote: the blockquote keeps the word before it and draws the
// tag as the inline HTML the schema holds.
const lazyQuote = "> a\n<b>\n";
const lazyQuoteText = ["a <b>"];

test("a lazy line holding one tag keeps its blockquote's text in a comment and in the spec editor", async ({
  browser,
}) => {
  const { commentId, key } = await seed({
    body: lazyQuote,
    spec: `# Quote\n\n${lazyQuote}`,
    title: "A lazy line holding one tag",
  });
  await withAlice(browser, async (page) => {
    const { drawn } = await expandComment(page, key, commentId);
    await expect.poll(() => textsOf(drawn.locator("blockquote"))).toEqual(lazyQuoteText);
    await expect(drawn.locator(`blockquote ${inlineHtml}`)).toHaveText(["<b>"]);

    await openSpec(page, key, "Quote");
    const editor = documentEditor(page);
    await expect.poll(() => textsOf(editor.locator("blockquote"))).toEqual(lazyQuoteText);
    await expect(editor.locator(`blockquote ${inlineHtml}`)).toHaveText(["<b>"]);
  });
});

// 5. A code span crossing three quoted lines, the middle one holding a complete tag: written
// without the quote's prefix, that line would open a block of HTML in a reader and take the
// quote, the paragraph and the code with it, so the stored bytes keep the prefix there.
const codeSpanTagLine = "> a `x\n> <b>\n> y` z\n";
const codeSpanStored = "# Quote\n\n> a `x\n> <b>\ny` z\n";
const codeSpanText = ["x\n<b>\ny"];

test("a code span's tag line is stored behind the quote's prefix and draws the same code in a past version and in the spec editor", async ({
  browser,
}) => {
  const { key } = await seed({
    spec: `# Quote\n\n${codeSpanTagLine}`,
    title: "A code span whose line holds one tag",
  });
  expect((await getIssueArtifactText(key, "spec")).markdown).toBe(codeSpanStored);

  await withAlice(browser, async (page) => {
    const version = await openFirstVersion(page, key);
    await expect.poll(() => textsOf(version.locator("blockquote code"))).toEqual(codeSpanText);

    await openSpec(page, key, "Quote");
    await expect
      .poll(() => textsOf(documentEditor(page).locator("blockquote code")))
      .toEqual(codeSpanText);
  });
});

// 6. A quoted closing tag a tab ends opens a block of HTML in the browser's engine and none in
// the server's reader. Where the block holds that one line and nothing else the two agree, and
// the document is stored; where the block swallows the line under it, as here, nothing the
// document could be stored as reads back as written, so the write is refused, naming the line.
test("a spec whose quoted closing tag ends in a tab is refused as invalid markdown, naming the line", async () => {
  await createProject({ key: "CORE", name: "Core" });
  const answer = await refusalOf(
    createIssue(
      { project: "CORE", spec: "> </a\t>\n> text\n", title: "A closing tag a tab ends" },
      agentSession
    )
  );
  expect(answer.code).toBe("INVALID_MARKDOWN");
  expect(answer.error).toContain(
    'line 1, "> </a\\t>", opens with HTML the browser editor\'s parser reads as a block of HTML'
  );
});

// 7. A U+0000 is U+FFFD to CommonMark, and so to both readers. A raw one never reaches a reader:
// PostgreSQL holds no such character, so the API's input layer refuses the write that carries it,
// naming the field, and only a browser's own edit can put one in a live document, as a tree and
// never as markdown (pmdoc's nulAsReplacement). Written as the character reference a caller may
// send instead, it reaches both readers, and the editor's text and the past version's are the
// replacement character alike. This row guards those two answers; the reader change behind it is
// proved where a raw character can reach the reader, in the pmdoc package's own tests.
const nulReference = "a&#0;b\n";
const replacedParagraphs = ["a\uFFFDb"];

test("a NUL reads as the replacement character in the spec editor and in a past version, and a raw one is refused", async ({
  browser,
}) => {
  const { key } = await seed({ spec: `# NUL\n\n${nulReference}`, title: "A NUL in a paragraph" });

  const raw = await refusalOf(
    createIssue({ project: "CORE", spec: "a\u0000b\n", title: "A raw NUL" }, agentSession)
  );
  expect(raw.code).toBe("NUL_CHARACTER");
  expect(raw.error).toContain("spec holds a NUL character (U+0000) at character 2");

  await withAlice(browser, async (page) => {
    await openSpec(page, key, "NUL");
    await expect.poll(() => textsOf(documentEditor(page).locator("p"))).toEqual(replacedParagraphs);
    const editor = await textsOf(documentEditor(page).locator("p"));

    const version = await openFirstVersion(page, key);
    await expect.poll(() => textsOf(version.locator("p"))).toEqual(editor);
  });
});

// 8. A block of HTML stands where only a block node may, and the Proof schema has no node for
// one: the engine used to throw on the whole body, so the card drew none of it. It draws as a
// paragraph holding the block's own bytes, and the Conversation goes on drawing around it.
test("a comment holding a block of HTML draws it as a paragraph, and the Conversation goes on drawing", async ({
  browser,
}) => {
  const { commentId, key } = await seed({ body: "<div>\n", title: "A comment holding a block" });
  const after = await createComment(key, { body: "After the block." }, agentSession);

  await withAlice(browser, async (page) => {
    const { drawn, turn } = await expandComment(page, key, commentId);
    await expect.poll(() => textsOf(drawn.locator("p"))).toEqual(["<div>"]);
    await expect(drawn.locator(inlineHtml)).toHaveText(["<div>"]);
    // A body the engine refuses is written into the card as its literal source instead.
    await expect(turn.locator("[data-markdown-fallback]")).toHaveCount(0);
    await expect(
      page.locator(`[data-turn="comment:${after.id}"]`).getByText("After the block.")
    ).toBeVisible();
  });
});
