import { type BrowserContext, expect, type Locator, type Page, test } from "@playwright/test";

import { createIssue, createProject, getArtifactText, getIssue } from "./api";
import {
  connectedDot,
  documentEditor,
  documentTransport,
  indexedBlockAsk,
  placeCaret,
} from "./editor";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

// INFRA-624: two collaborators editing one decision block at once can leave it holding what its
// content rule (`paragraph+ bullet_list?`) refuses: each adds an option list while their edits
// cross, and the merged block holds two lists. y-prosemirror could not build such a block and
// deleted it from the shared document, so every editor and the server lost the question and every
// option, and settlement retracted the ask. The block now keeps every word, reads as malformed
// until it is fixed, and its ask stays open; joining the two lists makes it a decision again.

const SPEC = 'Intro.\n\n:::ask{#a1 urgency="med" multiple="false"}\nWhich one?\n:::\n\nAfter.\n';

test.beforeEach(async () => {
  await resetDatabase();
});

function askBlock(page: Page): Locator {
  return documentEditor(page).locator('[data-dispatch-ask-block="a1"]');
}

/** Creates the issue whose spec holds the decision a1 and waits until it is an open ask. */
async function seedDecision() {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", spec: SPEC, title: "Crossed options" });
  await indexedBlockAsk(issue.key, "a1");
  return issue;
}

/** Opens the decision in one page per context, then has each add its own option list under the
 * question while neither edit reaches the server or the other page, and lets both edits through
 * at once: the merged block holds two lists. Each page's errors are collected into `errors`. */
async function crossOptionLists(
  issueKey: string,
  editors: readonly { context: BrowserContext; option: string }[],
  errors: string[]
): Promise<Page[]> {
  const opened = [];
  for (const { context, option } of editors) {
    const page = await context.newPage();
    page.on("pageerror", (error) => errors.push(error.message));
    const transport = await documentTransport(page);
    await page.goto(`/issues/${issueKey}`);
    opened.push({ option, page, transport });
  }
  for (const { page } of opened) {
    await expect(connectedDot(page)).toHaveText("connected");
    await expect(askBlock(page)).toContainText("Which one?");
  }
  for (const { transport } of opened) {
    transport.hold();
    await transport.sever();
  }
  for (const { option, page } of opened) {
    // The option first, then the `- ` input rule before it: typed into an empty item, the text
    // lands in the question, since the block hides an empty option while it reads as malformed.
    await placeCaret(page, "after", "Which one?");
    await page.keyboard.press("Enter");
    await page.keyboard.type(option);
    await page.keyboard.press("Home");
    await page.keyboard.type("- ");
    await expect(askBlock(page).getByRole("listitem")).toHaveText(option);
  }
  await Promise.all(opened.map(({ transport }) => transport.release()));
  return opened.map(({ page }) => page);
}

/** Waits until every page's decision block and the stored markdown hold each of `words`. */
async function expectWordsKept(
  pages: readonly Page[],
  artifactId: string,
  words: readonly string[]
): Promise<void> {
  for (const page of pages) {
    for (const word of words) {
      await expect(askBlock(page)).toContainText(word);
    }
  }
  await expect
    .poll(async () => {
      const { markdown } = await getArtifactText(artifactId);
      return words.filter((word) => !markdown.includes(word));
    })
    .toEqual([]);
}

test("two editors that each add an option list to an ask while their edits cross keep the question and both options", async ({
  browser,
}) => {
  const issue = await seedDecision();
  const alice = await asUser(browser, "alice");
  const bob = await asUser(browser, "bob");
  const errors: string[] = [];
  try {
    const pages = await crossOptionLists(
      issue.key,
      [
        { context: alice, option: "Option A" },
        { context: bob, option: "Option B" },
      ],
      errors
    );
    await expectWordsKept(pages, issue.primary_artifact_id, ["Which one?", "Option A", "Option B"]);

    for (const page of pages) {
      await expect(askBlock(page)).toHaveAttribute("data-dispatch-ask-malformed", "true");
    }
    expect((await getIssue(issue.key)).open_asks.map((ask) => ask.block_id)).toContain("a1");

    // One list again: Backspace at the start of the second list joins it to the first, and the
    // block parses, so the ask takes both options. Which list comes first is the merge's order of
    // the two editors' Yjs client ids, so the second list is read from the page; Backspace at the
    // start of the first list would lift its item into the question instead.
    const [alicePage] = pages;
    const secondList = (await askBlock(alicePage).locator("ul").nth(1).textContent()) ?? "";
    await placeCaret(
      alicePage,
      "before",
      secondList.includes("Option B") ? "Option B" : "Option A"
    );
    await alicePage.keyboard.press("Backspace");
    for (const page of pages) {
      await expect(askBlock(page)).not.toHaveAttribute("data-dispatch-ask-malformed");
    }
    await expect
      .poll(async () =>
        (await getIssue(issue.key)).open_asks
          .find((ask) => ask.block_id === "a1")
          ?.options.map(({ label }) => label)
          .sort()
      )
      .toEqual(["Option A", "Option B"]);
    expect(errors).toEqual([]);
  } finally {
    await bob.close();
    await alice.close();
  }
});

test("Enter in the question of an ask holding two lists keeps every word", async ({ browser }) => {
  const issue = await seedDecision();
  const alice = await asUser(browser, "alice");
  const bob = await asUser(browser, "bob");
  const errors: string[] = [];
  try {
    const pages = await crossOptionLists(
      issue.key,
      [
        { context: alice, option: "Option A" },
        { context: bob, option: "Option B" },
      ],
      errors
    );
    for (const page of pages) {
      await expect(askBlock(page)).toHaveAttribute("data-dispatch-ask-malformed", "true");
    }

    const [alicePage] = pages;
    await placeCaret(alicePage, "after", "Which one?");
    await alicePage.keyboard.press("Enter");
    await alicePage.keyboard.type("More");
    await expectWordsKept(pages, issue.primary_artifact_id, [
      "Which one?",
      "More",
      "Option A",
      "Option B",
    ]);
    expect(errors).toEqual([]);
  } finally {
    await bob.close();
    await alice.close();
  }
});
