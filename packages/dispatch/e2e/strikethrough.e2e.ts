import { type Browser, expect, type Locator, type Page, test } from "@playwright/test";

import { createComment, createIssue, createProject, getIssueArtifactText } from "./api";
import { documentEditor, openSpec } from "./editor";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

// A run of tildes strikes only when it pairs with a run of its own length, and a run may open
// right after an escaped tilde: the browser's engine (micromark's GFM strikethrough) reads them
// so, and the server's reader reads a document the same way. One spec, written by an agent, reads
// alike in its live editor, in a past version and in a comment holding the same paragraphs; its
// stored text spells the unpaired tildes so that they read back as tildes.

const agentSession = {
  actor: { kind: "session" as const, id: "e2e-strikethrough" },
  as: "agent" as const,
};

const paragraphs =
  "Plain ~a~~ text.\n\nPlain ~~a~ text.\n\nPlain ~\\~~ end.\n\nPlain ~~b~~ text.\n";

test.beforeEach(async () => {
  await resetDatabase();
});

async function seed(): Promise<{ commentId: string; key: string }> {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue(
    { project: "CORE", spec: `# Strikes\n\n${paragraphs}`, title: "Tilde runs" },
    agentSession
  );
  const comment = await createComment(issue.key, { body: paragraphs }, agentSession);
  return { commentId: comment.id, key: issue.key };
}

/** What the engine draws for the four paragraphs, in order: each paragraph's whole text and the
 *  text of each `<del>` in it. Rows of mismatched run lengths keep their tildes and strike
 *  nothing, a run after an escaped tilde strikes it, and the paired control strikes `b`. */
const engineReading = [
  { struck: [], text: "Plain ~a~~ text." },
  { struck: [], text: "Plain ~~a~ text." },
  { struck: ["~"], text: "Plain ~ end." },
  { struck: ["b"], text: "Plain b text." },
];

/** Asserts the paragraphs `view` draws read as `engineReading`. The control row strikes under
 *  either reader, so once it shows the view has drawn; the whole table is then compared at once,
 *  so a failure reports every row's reading. */
async function expectEngineReading(view: Locator): Promise<void> {
  const rows = view.locator("p");
  await expect(rows).toHaveCount(engineReading.length);
  await expect(rows.nth(3).locator("del")).toHaveText(["b"]);
  await expect
    .poll(() =>
      rows.evaluateAll((paragraphs) =>
        paragraphs.map((paragraph) => ({
          struck: [...paragraph.querySelectorAll("del")].map((del) => del.textContent),
          text: paragraph.textContent?.replace(/\s+/g, " ").trim(),
        }))
      )
    )
    .toEqual(engineReading);
}

async function withAlice(browser: Browser, check: (page: Page) => Promise<void>): Promise<void> {
  const alice = await asUser(browser, "alice");
  try {
    await check(await alice.newPage());
  } finally {
    await alice.close();
  }
}

test("tilde runs of different lengths stay text in the spec editor, and a run after an escaped tilde strikes", async ({
  browser,
}) => {
  const { key } = await seed();
  await withAlice(browser, async (page) => {
    await openSpec(page, key, "Plain b text.");
    await expectEngineReading(documentEditor(page));
  });
});

test("tilde runs of different lengths stay text in a past version of the spec, and a run after an escaped tilde strikes", async ({
  browser,
}) => {
  const { key } = await seed();
  await withAlice(browser, async (page) => {
    await page.goto(`/issues/${key}/artifacts/spec?v=1`);
    await expectEngineReading(page.getByTestId("version-view"));
  });
});

test("tilde runs of different lengths stay text in a comment, and a run after an escaped tilde strikes", async ({
  browser,
}, testInfo) => {
  const { commentId, key } = await seed();
  await withAlice(browser, async (page) => {
    await page.goto(`/issues/${key}/conversation`);
    // A collapsed card previews the body inline; the expanded thread draws its paragraphs, in
    // place on a wide screen and in the Thread sheet on a phone.
    const turn = page.locator(`[data-turn="comment:${commentId}"]`);
    await turn.getByRole("button", { name: "Expand thread" }).click();
    const thread =
      testInfo.project.name === "iphone" ? page.getByRole("dialog", { name: "Thread" }) : turn;
    await expectEngineReading(thread.locator(".dispatch-markdown"));
  });
});

test("the stored spec spells unpaired tildes as character references and keeps paired runs", async () => {
  const { key } = await seed();
  const { markdown } = await getIssueArtifactText(key, "spec");
  expect.soft(markdown).toContain("Plain &#126;a&#126;&#126; text.");
  expect.soft(markdown).toContain("Plain &#126;&#126;a&#126; text.");
  expect.soft(markdown).toContain("Plain ~~&#126;~~ end.");
  expect.soft(markdown).toContain("Plain ~~b~~ text.");
});
