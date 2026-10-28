import { type Browser, expect, type Locator, type Page, test } from "@playwright/test";

import { createComment, createIssue, createProject, getIssueArtifactText } from "./api";
import { conversationThreadScope, documentEditor, openSpec } from "./editor";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

// An asterisk beside a tilde opens or closes emphasis as the browser's engine (micromark) says:
// a tilde is punctuation, so `b*~*` emphasizes the tilde, while `~**b<*` and `*b**~~` pair
// nothing. A document reads them so too: one spec, written by an agent, reads alike in its live
// editor, in a past version and in a comment holding the same paragraphs, and its stored text
// spells each run so that it reads back the same.

const agentSession = {
  actor: { kind: "session" as const, id: "e2e-emphasis-beside-a-tilde" },
  as: "agent" as const,
};

const paragraphs = "Plain b*~* text.\n\nPlain ~**b<* end.\n\nPlain *b**~~ end.\n";

test.beforeEach(async () => {
  await resetDatabase();
});

async function seed(): Promise<{ commentId: string; key: string }> {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue(
    { project: "CORE", spec: `# Emphasis\n\n${paragraphs}`, title: "Emphasis beside a tilde" },
    agentSession
  );
  const comment = await createComment(issue.key, { body: paragraphs }, agentSession);
  return { commentId: comment.id, key: issue.key };
}

/** What the engine draws for the three paragraphs, in order: each paragraph's whole text and the
 *  text of each `<em>`, `<strong>` and `<del>` in it. Only the first paragraph's asterisks pair,
 *  around the tilde; the other two keep every asterisk and tilde as text. */
const engineReading = [
  { emphasized: ["~"], strong: [], struck: [], text: "Plain b~ text." },
  { emphasized: [], strong: [], struck: [], text: "Plain ~**b<* end." },
  { emphasized: [], strong: [], struck: [], text: "Plain *b**~~ end." },
];

/** Asserts the paragraphs `view` draws read as `engineReading`, the whole table at once, so a
 *  failure reports every row's reading. */
async function expectEngineReading(view: Locator): Promise<void> {
  const rows = view.locator("p");
  await expect(rows).toHaveCount(engineReading.length);
  await expect
    .poll(() =>
      rows.evaluateAll((paragraphs) =>
        paragraphs.map((paragraph) => {
          const texts = (selector: string) =>
            [...paragraph.querySelectorAll(selector)].map((element) => element.textContent);
          return {
            emphasized: texts("em"),
            strong: texts("strong"),
            struck: texts("del"),
            text: paragraph.textContent?.replace(/\s+/g, " ").trim(),
          };
        })
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

test("an asterisk beside a tilde emphasizes as the engine reads it in the spec editor", async ({
  browser,
}) => {
  const { key } = await seed();
  await withAlice(browser, async (page) => {
    await openSpec(page, key, "Emphasis");
    await expectEngineReading(documentEditor(page));
  });
});

test("an asterisk beside a tilde emphasizes as the engine reads it in a past version of the spec", async ({
  browser,
}) => {
  const { key } = await seed();
  await withAlice(browser, async (page) => {
    await page.goto(`/issues/${key}/artifacts/spec?v=1`);
    await expectEngineReading(page.getByTestId("version-view"));
  });
});

test("an asterisk beside a tilde emphasizes as the engine reads it in a comment", async ({
  browser,
}, testInfo) => {
  const { commentId, key } = await seed();
  await withAlice(browser, async (page) => {
    await page.goto(`/issues/${key}/conversation`);
    // A collapsed card previews the body inline; the expanded thread draws its paragraphs, in
    // place on a wide screen and in the Thread sheet on a phone.
    const turn = page.locator(`[data-turn="comment:${commentId}"]`);
    await turn.getByRole("button", { name: "Expand thread" }).click();
    const thread = conversationThreadScope(page, testInfo.project.name, turn);
    await expectEngineReading(thread.locator(".dispatch-markdown"));
    // The engine drew the body: no literal-text fallback stands in for it.
    await expect(turn.locator("[data-markdown-fallback]")).toHaveCount(0);
    await expect(thread.locator("[data-markdown-fallback]")).toHaveCount(0);
  });
});

test("the stored spec spells each asterisk beside a tilde so it reads back the same", async () => {
  const { key } = await seed();
  const { markdown } = await getIssueArtifactText(key, "spec");
  expect.soft(markdown).toContain("Plain &#98;*~* text.");
  expect.soft(markdown).toContain("Plain ~\\*\\*b<* end.");
  expect.soft(markdown).toContain("Plain \\*b\\*\\*~~ end.");
});
