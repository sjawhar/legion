import { type Browser, expect, type Locator, type Page, test } from "@playwright/test";

import { createComment, createIssue, createProject, getIssueArtifactText } from "./api";
import { documentEditor, expectCommentBody, openSpec } from "./editor";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

// An inline leaf - raw HTML, a picture, a footnote reference - carries no text of its own, so no
// mark may wrap it: the server's reader holds emphasis, strong, strikethrough and link marks on
// text alone, and the browser's engine reads them the same way, so a document and a comment
// holding the same markdown draw alike. An inline HTML declaration (`<!` and an ASCII letter) is
// raw HTML to both readers, so a document stores it as its author wrote it rather than escaped.
// An autolink inside a link label leaves the label's own link on the text around it instead of
// stacking under the autolink's, which is what makes such a label storable at all. One spec,
// written by an agent, reads alike in its live editor, in a past version and in a comment holding
// the same paragraphs.

const agentSession = {
  actor: { kind: "session" as const, id: "e2e-leaf-marks" },
  as: "agent" as const,
};

/** The five documents this change was filed for, byte for byte from
 *  `packages/envoy/internal/dispatch/pmdoc/testdata/corpus/marks-on-addresses-before-inline-html.md`:
 *  an address beside inline HTML, where the engine's marks reach over the HTML and the server's
 *  do not. */
const corpusDocuments = [
  "**(www\\.a'b@c.test/a~b<b>****",
  "**www\\.a!b@c.test/q?a=1&b=2<b> and more**",
  "**www\\.api.github.com/a`b<b>***`",
  "**\\(HTTPS://localhost/a`b<b>***`",
  "***www\\.api.github.com/q?a=1&b=2<b>***",
];

/** The blocks of the document, in order: inline HTML inside strong, a footnote reference inside
 *  emphasis and its definition, a picture inside a link inside strong, an inline HTML declaration,
 *  an autolink alone in a link label, an autolink between a label's words, then the corpus. */
const blocks = [
  "**a<b>c**",
  "*[^d]*",
  "[^d]: note",
  "**[![x](i.png)](https://u.test)**",
  "p <!a> q",
  "[<https://u.test>](u)",
  "[a <https://u.test> b](u)",
  ...corpusDocuments,
];

const paragraphs = `${blocks.join("\n\n")}\n`;

test.beforeEach(async () => {
  await resetDatabase();
});

async function seed(): Promise<{ commentId: string; key: string }> {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue(
    { project: "CORE", spec: `# Leaves\n\n${paragraphs}`, title: "Inline leaves" },
    agentSession
  );
  const comment = await createComment(issue.key, { body: paragraphs }, agentSession);
  return { commentId: comment.id, key: issue.key };
}

/** One inline leaf a paragraph draws. */
interface Leaf {
  /** What names the leaf: an `html` span's `data-value`, a picture's `src`, a footnote
   *  reference's `data-label`. */
  readonly identity: string;
  /** The element the leaf is drawn as: `span` for raw HTML, `img`, `sup`. */
  readonly kind: string;
  /** The mark elements between the leaf and its paragraph, innermost first: always none, since a
   *  mark never reaches an inline leaf. */
  readonly marks: readonly string[];
}

interface Row {
  readonly leaves: readonly Leaf[];
  /** The paragraph's text exactly as it is drawn; whitespace is part of the reading, so no
   *  matcher that collapses it (`toHaveText`) may stand in for this. */
  readonly text: string;
}

function html(value: string): Leaf {
  return { identity: value, kind: "span", marks: [] };
}

function picture(src: string): Leaf {
  return { identity: src, kind: "img", marks: [] };
}

function footnote(label: string): Leaf {
  return { identity: label, kind: "sup", marks: [] };
}

/** What every surface draws for the document's paragraphs, in order: each paragraph's whole text
 *  and each inline leaf in it. The definition's own paragraph (`note`) follows the reference's.
 *  No leaf carries a mark, the declaration `<!a>` is an `html` node like any other raw HTML, and
 *  the two labels holding an autolink draw their words beside its address. */
const engineReading: Row[] = [
  { leaves: [html("<b>")], text: "a<b>c" },
  { leaves: [footnote("d")], text: "d" },
  { leaves: [], text: "note" },
  { leaves: [picture("i.png")], text: "" },
  { leaves: [html("<!a>")], text: "p <!a> q" },
  { leaves: [], text: "https://u.test" },
  { leaves: [], text: "a https://u.test b" },
  { leaves: [html("<b>")], text: "(www.a'b@c.test/a~b<b>**" },
  { leaves: [html("<b>")], text: "www.a!b@c.test/q?a=1&b=2<b> and more" },
  { leaves: [], text: "**www.api.github.com/ab<b>***" },
  { leaves: [html("<b>")], text: "(HTTPS://localhost/a`b<b>*`" },
  { leaves: [html("<b>")], text: "www.api.github.com/q?a=1&b=2<b>" },
];

/** Reads every paragraph `view` draws in one pass over the rendered DOM, so a failure reports the
 *  whole table rather than the first row that differs. Only the document's own leaves are read:
 *  after a paragraph whose last child is a leaf node, ProseMirror appends an
 *  `<img class="ProseMirror-separator" alt="">` of its own, beside its trailing `<br>`, so that
 *  the line keeps its height and the caret has somewhere to stand. That image is editor chrome,
 *  carrying no `src` and standing for nothing in the document, so it is no leaf of it. */
function readParagraphs(view: Locator): Promise<Row[]> {
  return view.locator("p").evaluateAll((rendered) =>
    rendered.map((paragraph) => ({
      leaves: [
        ...paragraph.querySelectorAll(
          'span[data-type="html"], img:not(.ProseMirror-separator), sup[data-type="footnote_reference"]'
        ),
      ].map((leaf) => {
        const marks: string[] = [];
        for (
          let ancestor = leaf.parentElement;
          ancestor !== null && ancestor !== paragraph;
          ancestor = ancestor.parentElement
        ) {
          const tag = ancestor.tagName.toLowerCase();
          if (tag === "strong" || tag === "em" || tag === "del" || tag === "a") {
            marks.push(tag);
          }
        }
        return {
          identity:
            leaf.getAttribute("data-value") ??
            leaf.getAttribute("src") ??
            leaf.getAttribute("data-label") ??
            "",
          kind: leaf.tagName.toLowerCase(),
          marks,
        };
      }),
      text: paragraph.textContent ?? "",
    }))
  );
}

/** Asserts the paragraphs `view` draws read as `engineReading`. The paragraph count shows once the
 *  view has drawn; the whole table is then compared at once. */
async function expectEngineReading(view: Locator): Promise<void> {
  await expect(view.locator("p")).toHaveCount(engineReading.length);
  await expect.poll(() => readParagraphs(view)).toEqual(engineReading);
}

async function withAlice(browser: Browser, check: (page: Page) => Promise<void>): Promise<void> {
  const alice = await asUser(browser, "alice");
  try {
    await check(await alice.newPage());
  } finally {
    await alice.close();
  }
}

test("no mark reaches an inline leaf in the spec editor, and a declaration is raw HTML", async ({
  browser,
}) => {
  const { key } = await seed();
  await withAlice(browser, async (page) => {
    await openSpec(page, key, "p <!a> q");
    await expectEngineReading(documentEditor(page));
  });
});

test("no mark reaches an inline leaf in a past version of the spec, and a declaration is raw HTML", async ({
  browser,
}) => {
  const { key } = await seed();
  await withAlice(browser, async (page) => {
    await page.goto(`/issues/${key}/artifacts/spec?v=1`);
    await expectEngineReading(page.getByTestId("version-view"));
  });
});

test("no mark reaches an inline leaf in a comment, and a declaration is raw HTML", async ({
  browser,
}, testInfo) => {
  const { commentId, key } = await seed();
  // expectCommentBody opens the thread as every comment spec opens it and proves the engine drew
  // the body, with this spec's own reader of the paragraphs in place of the shared one.
  await withAlice(browser, (page) =>
    expectCommentBody(page, key, commentId, testInfo.project.name, expectEngineReading)
  );
});

test("the stored spec keeps a declaration unescaped and a label's link beside an autolink", async () => {
  const { key } = await seed();
  const { markdown } = await getIssueArtifactText(key, "spec");
  // The declaration is raw HTML to the server's reader too, so it is stored as written, with no
  // backslash before it.
  expect.soft(markdown).toContain("p <!a> q");
  // A label that is nothing but an autolink keeps the autolink's own address alone, written as the
  // bare URL both readers link again.
  expect.soft(markdown.split("\n")).toContain("https://u.test");
  // A label holding an autolink between its words keeps its own link over both of them; how the
  // address between them is spelled is the renderer's choice.
  expect.soft(markdown).toContain("[a ](u)");
  expect.soft(markdown).toContain("[ b](u)");
});
