import { type Browser, expect, type Page, test } from "@playwright/test";

import { createComment, createIssue, createProject, getAsk, listComments } from "./api";
import {
  barAction,
  connectedDot,
  deleteEditorText,
  documentEditor,
  expectMark,
  placeCaret,
  selectEditorText,
} from "./editor";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

// The margin composer a selection-bar action opens can switch between Comment, Suggest and Ask;
// the switch retypes the provisional mark the bar wrote (LEGION-363). Alice acts, Bob watches the
// same document in his own browser.
const initialMarkdown = "The quick brown fox";

interface Readers {
  alicePage: Page;
  bobPage: Page;
  issueKey: string;
  artifactId: string;
}

async function withReaders(
  browser: Browser,
  title: string,
  run: (readers: Readers) => Promise<void>
): Promise<void> {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", spec: initialMarkdown, title });
  const alice = await asUser(browser, "alice");
  const bob = await asUser(browser, "bob");
  try {
    const alicePage = await alice.newPage();
    const bobPage = await bob.newPage();
    await Promise.all([
      alicePage.goto(`/issues/${issue.key}/spec`),
      bobPage.goto(`/issues/${issue.key}/spec`),
    ]);
    await expect(documentEditor(alicePage)).toContainText(initialMarkdown);
    await expect(documentEditor(bobPage)).toContainText(initialMarkdown);
    await expect(connectedDot(alicePage)).toHaveText("connected");
    await expect(connectedDot(bobPage)).toHaveText("connected");
    await run({ alicePage, artifactId: issue.primary_artifact_id, bobPage, issueKey: issue.key });
  } finally {
    await bob.close();
    await alice.close();
  }
}

function askMarks(page: Page) {
  return documentEditor(page).locator('span[data-dispatch="ask"][data-id]');
}

function commentMarks(page: Page) {
  return documentEditor(page).locator('span[data-proof="comment"][data-id]');
}

function anyMarks(page: Page) {
  return documentEditor(page).locator("span[data-id]");
}

function composer(page: Page) {
  return page.getByRole("form", { name: "Comment composer" });
}

function kindButton(page: Page, name: "Comment" | "Suggest" | "Ask") {
  return composer(page).getByRole("group", { name: "Kind" }).getByRole("button", {
    exact: true,
    name,
  });
}

/** A bar Comment on `quote`, then the composer's Kind switched to Ask: the bar's comment mark is
 *  retyped into an ask mark over the same text. */
async function commentThenAsk(page: Page, quote: string): Promise<void> {
  await selectEditorText(page, quote);
  await barAction(page, "Comment");
  await expect(composer(page)).toContainText(quote);
  await kindButton(page, "Ask").click();
  await expect(askMarks(page)).toHaveText([quote]);
}

/** Mod+Z, Mod+Z, Mod+Shift+Z in the editor, with the caret after "fox": the chords reach the
 *  editor's history keymap, and the collab keymap when that one declines. */
async function undoUndoRedo(page: Page): Promise<void> {
  await placeCaret(page, "after", "fox");
  await page.keyboard.press("ControlOrMeta+z");
  await page.keyboard.press("ControlOrMeta+z");
  await page.keyboard.press("ControlOrMeta+Shift+z");
}

test.beforeEach(async () => {
  await resetDatabase();
});

test("switching an anchored composer from Comment to Ask retypes its mark and asks about the selected text", async ({
  browser,
}) => {
  await withReaders(
    browser,
    "Switch and send",
    async ({ alicePage, bobPage, issueKey, artifactId }) => {
      await selectEditorText(alicePage, "brown");
      await barAction(alicePage, "Comment");
      await expect(composer(alicePage)).toContainText("brown");
      await kindButton(alicePage, "Ask").click();
      await expect(kindButton(alicePage, "Ask")).toHaveAttribute("aria-pressed", "true");
      await expect(askMarks(alicePage)).toHaveText(["brown"]);
      await expect(askMarks(bobPage)).toHaveText(["brown"]);
      await expect(commentMarks(alicePage)).toHaveCount(0);
      await expect(commentMarks(bobPage)).toHaveCount(0);

      await composer(alicePage).getByLabel("Question").fill("Why brown?");
      await composer(alicePage).locator('button[type="submit"]').click();
      const askCard = alicePage
        .getByRole("region", { name: "Needs you" })
        .locator("[data-margin-item]")
        .filter({ hasText: "Why brown?" });
      await expect(askCard).toBeVisible();
      const askId = await askCard.getAttribute("data-margin-item");
      if (askId === null) {
        throw new Error("The switched ask has no margin id.");
      }
      const ask = await getAsk(askId);
      if (ask.ask.anchor === null) {
        throw new Error("The switched ask has no anchor.");
      }
      expect(ask.ask.anchor.quote).toBe("brown");
      await Promise.all([
        expectMark(alicePage, ask.ask.anchor.mark_id, "brown"),
        expectMark(bobPage, ask.ask.anchor.mark_id, "brown"),
      ]);
      expect(await listComments(issueKey, artifactId)).toEqual([]);
    }
  );
});

test("undo and redo after a switch write back no comment mark and take no ask mark", async ({
  browser,
}) => {
  await withReaders(browser, "Undo after a switch", async ({ alicePage, bobPage }) => {
    await commentThenAsk(alicePage, "brown");
    await undoUndoRedo(alicePage);
    await expect(askMarks(alicePage)).toHaveText(["brown"]);
    await expect(askMarks(bobPage)).toHaveText(["brown"]);
    await expect(commentMarks(alicePage)).toHaveCount(0);
    await expect(commentMarks(bobPage)).toHaveCount(0);
  });
});

test("a composer cancelled after a switch leaves no mark, through undo and redo", async ({
  browser,
}) => {
  await withReaders(browser, "Cancel after a switch", async ({ alicePage, bobPage }) => {
    await commentThenAsk(alicePage, "brown");
    await alicePage.keyboard.press("Escape");
    await expect(anyMarks(alicePage)).toHaveCount(0);
    await expect(anyMarks(bobPage)).toHaveCount(0);
    await undoUndoRedo(alicePage);
    await expect(anyMarks(alicePage)).toHaveCount(0);
    await expect(anyMarks(bobPage)).toHaveCount(0);
  });
});

test("a cancelled bar Comment stays cancelled through undo and redo", async ({ browser }) => {
  await withReaders(browser, "Cancelled comment", async ({ alicePage, bobPage }) => {
    await selectEditorText(alicePage, "brown");
    await barAction(alicePage, "Comment");
    await expect(composer(alicePage)).toContainText("brown");
    await alicePage.keyboard.press("Escape");
    await expect(anyMarks(alicePage)).toHaveCount(0);
    await undoUndoRedo(alicePage);
    await expect(anyMarks(alicePage)).toHaveCount(0);
    await expect(anyMarks(bobPage)).toHaveCount(0);
  });
});

test("refining the selection under an open composer leaves nothing of the first mark for undo to bring back", async ({
  browser,
}) => {
  await withReaders(browser, "Refined selection", async ({ alicePage, bobPage }) => {
    // The second bar Comment cuts into the first composer's mark, and the margin removes the rest
    // of it.
    await selectEditorText(alicePage, "quick brown");
    await barAction(alicePage, "Comment");
    await expect(composer(alicePage)).toContainText("quick brown");
    await selectEditorText(alicePage, "brown");
    await barAction(alicePage, "Comment");
    await expect(commentMarks(alicePage)).toHaveText(["brown"]);
    // Escape is handled on the composer's form, so focus goes back there from the editor.
    await composer(alicePage).getByLabel("Comment").focus();
    await alicePage.keyboard.press("Escape");
    await expect(anyMarks(alicePage)).toHaveCount(0);
    await undoUndoRedo(alicePage);
    await expect(anyMarks(alicePage)).toHaveCount(0);
    await expect(anyMarks(bobPage)).toHaveCount(0);
  });
});

test("undo and redo after a switch write back no comment mark when a sent suggestion is in the document", async ({
  browser,
}) => {
  await withReaders(
    browser,
    "Undo beside a suggestion",
    async ({ alicePage, bobPage, issueKey, artifactId }) => {
      // A recorded suggestion's marks map entry carries the server's replacement, which its mark's
      // attributes do not: every later mark write in this editor restamps that mark.
      await selectEditorText(alicePage, "quick");
      await barAction(alicePage, "Suggest");
      await composer(alicePage).getByLabel("Replacement").fill("red");
      await composer(alicePage).getByRole("button", { exact: true, name: "Send" }).click();
      await expect
        .poll(() =>
          listComments(issueKey, artifactId).then((items) => items[0]?.suggestion?.replace_with)
        )
        .toBe("red");
      const [suggestion] = await listComments(issueKey, artifactId);
      if (suggestion?.anchor == null) {
        throw new Error("The suggestion has no anchor.");
      }
      await expect(
        documentEditor(alicePage).locator(
          `.mark-replace-insert[data-mark-id="${suggestion.anchor.mark_id}"]`
        )
      ).toHaveText("red");

      await commentThenAsk(alicePage, "brown");
      await undoUndoRedo(alicePage);
      await expect(askMarks(alicePage)).toHaveText(["brown"]);
      await expect(askMarks(bobPage)).toHaveText(["brown"]);
      await expect(commentMarks(alicePage)).toHaveCount(0);
      await expect(commentMarks(bobPage)).toHaveCount(0);
      await Promise.all([
        expectMark(alicePage, suggestion.anchor.mark_id, "quick"),
        expectMark(bobPage, suggestion.anchor.mark_id, "quick"),
      ]);
    }
  );
});

test("Suggest on a mid-word selection is refused, and the comment stays", async ({ browser }) => {
  await withReaders(browser, "Mid-word suggestion", async ({ alicePage }) => {
    // The bar's Comment accepts a mid-word selection; a suggestion over it is one upstream will
    // not write.
    await selectEditorText(alicePage, "ick");
    await barAction(alicePage, "Comment");
    await expect(composer(alicePage)).toContainText("ick");
    await kindButton(alicePage, "Suggest").click();
    await expect(composer(alicePage).getByRole("status")).toHaveText(
      "A suggestion needs whole words inside one table cell. Comment or ask about this selection instead, or close this composer and select again."
    );
    await expect(kindButton(alicePage, "Comment")).toHaveAttribute("aria-pressed", "true");
    await expect(kindButton(alicePage, "Suggest")).toHaveAttribute("aria-pressed", "false");
    await expect(commentMarks(alicePage)).toHaveText(["ick"]);
  });
});

test("Comment over text another reader's comment covers is refused, and that comment stays whole", async ({
  browser,
}) => {
  await withReaders(
    browser,
    "Overlapping comment",
    async ({ alicePage, bobPage, issueKey, artifactId }) => {
      // Bob's recorded comment covers "quick brown"; a comment of Alice's over "brown" would cut
      // "brown" out of it.
      const bobsComment = await createComment(
        issueKey,
        { anchor: { artifact: "spec", quote: "quick brown" }, body: "whole phrase" },
        { login: "bob" }
      );
      if (bobsComment.anchor === null) {
        throw new Error("Bob's comment has no anchor.");
      }
      await expectMark(alicePage, bobsComment.anchor.mark_id, "quick brown");
      await selectEditorText(alicePage, "brown");
      await barAction(alicePage, "Ask");
      await expect(composer(alicePage)).toContainText("brown");
      await kindButton(alicePage, "Comment").click();
      await expect(composer(alicePage).getByRole("status")).toHaveText(
        "Someone else's comment already covers part of this text. Close this composer and select text outside it."
      );
      await expect(kindButton(alicePage, "Ask")).toHaveAttribute("aria-pressed", "true");
      await expectMark(alicePage, bobsComment.anchor.mark_id, "quick brown");
      await expectMark(bobPage, bobsComment.anchor.mark_id, "quick brown");
      await alicePage.keyboard.press("Escape");
      await expect(askMarks(alicePage)).toHaveCount(0);
      await expectMark(alicePage, bobsComment.anchor.mark_id, "quick brown");
      await expect
        .poll(() => listComments(issueKey, artifactId).then((items) => items[0]?.anchor?.quote))
        .toBe("quick brown");
    }
  );
});

test("a switch after another reader deleted the text says the highlight is gone", async ({
  browser,
}) => {
  await withReaders(browser, "Deleted text", async ({ alicePage, bobPage }) => {
    // Bob deletes the text Alice's composer is about; her switch has no mark left to retype.
    await selectEditorText(alicePage, "fox");
    await barAction(alicePage, "Comment");
    await expect(composer(alicePage)).toContainText("fox");
    await deleteEditorText(bobPage, "fox");
    await expect(documentEditor(alicePage)).not.toContainText("fox");
    await kindButton(alicePage, "Ask").click();
    await expect(composer(alicePage).getByRole("status")).toHaveText(
      "That highlight is gone from the document. Close this composer and select the text again."
    );
    await expect(kindButton(alicePage, "Comment")).toHaveAttribute("aria-pressed", "true");
  });
});
