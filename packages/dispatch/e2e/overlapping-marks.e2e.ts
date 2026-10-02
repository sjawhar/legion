import { expect, type Page, test } from "@playwright/test";

import {
  createIssue,
  createProject,
  getArtifact,
  getArtifactText,
  getComment,
  listComments,
} from "./api";
import {
  actionBar,
  barAction,
  connectedDot,
  documentEditor,
  marginCard,
  markSpan,
  placeCaret,
  selectEditorText,
} from "./editor";
import { commentWithBody, markText, setSheet } from "./margin-helpers";
import { resetDatabase } from "./seed";
import { centerOf, touchHold } from "./touch";
import { asUser } from "./users";

const initialMarkdown = "The quick brown fox";

test.beforeEach(async () => {
  await resetDatabase();
});

// Bob comments on "quick brown"; Alice starts a comment on "brown" and cancels it, then comments on
// "brown". Each record mark keeps its own text in both browsers and in the server's reading of the
// document, so the anchor refresh a document version runs leaves Bob's quote "quick brown" rather
// than cutting it to "quick ".
test("two readers' comments can cover the same text, and neither cuts the other's anchor", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", spec: initialMarkdown, title: "Overlap" });
  const artifactId = issue.primary_artifact_id;
  const alice = await asUser(browser, "alice");
  const bob = await asUser(browser, "bob");

  try {
    const alicePage = await alice.newPage();
    const bobPage = await bob.newPage();
    const readers = [alicePage, bobPage];
    await Promise.all([
      alicePage.goto(`/issues/${issue.key}/spec`),
      bobPage.goto(`/issues/${issue.key}/spec`),
    ]);
    await Promise.all(
      readers.flatMap((page) => [
        expect(documentEditor(page)).toContainText(initialMarkdown),
        expect(connectedDot(page)).toHaveText("connected"),
      ])
    );

    await selectEditorText(bobPage, "quick brown");
    await barAction(bobPage, "Comment");
    const bobComposer = bobPage.getByRole("form", { name: "Comment composer" });
    await bobComposer.getByLabel("Comment").fill("Bob's comment");
    await bobComposer.getByRole("button", { exact: true, name: "Send" }).click();
    const bobComment = await commentWithBody(issue.key, artifactId, "Bob's comment");
    if (bobComment.anchor === null) throw new Error("Bob's comment has no anchor.");
    expect(bobComment.anchor.quote).toBe("quick brown");
    const bobMark = bobComment.anchor.mark_id;
    await expect.poll(() => markText(alicePage, bobMark)).toBe("quick brown");

    // A comment Alice starts on "brown" and cancels takes its provisional mark away, and only it.
    await setSheet(alicePage, testInfo.project.name, false);
    await selectEditorText(alicePage, "brown");
    await barAction(alicePage, "Comment");
    await expect(alicePage.getByRole("form", { name: "Comment composer" })).toContainText("brown");
    await alicePage.keyboard.press("Escape");
    await Promise.all(
      readers.flatMap((page) => [
        expect
          .poll(() =>
            documentEditor(page)
              .locator("span[data-proof][data-id]")
              .evaluateAll((spans) => [
                ...new Set(spans.map((span) => span.getAttribute("data-id"))),
              ])
          )
          .toEqual([bobMark]),
        expect.poll(() => markText(page, bobMark)).toBe("quick brown"),
      ])
    );

    await setSheet(alicePage, testInfo.project.name, false);
    await selectEditorText(alicePage, "brown");
    await barAction(alicePage, "Comment");
    const aliceComposer = alicePage.getByRole("form", { name: "Comment composer" });
    await aliceComposer.getByLabel("Comment").fill("Alice's comment");
    await aliceComposer.getByRole("button", { exact: true, name: "Send" }).click();
    const aliceComment = await commentWithBody(issue.key, artifactId, "Alice's comment");
    if (aliceComment.anchor === null) throw new Error("Alice's comment has no anchor.");
    expect(aliceComment.anchor.quote).toBe("brown");
    const aliceMark = aliceComment.anchor.mark_id;

    // A mark alone writes no document version. A typed paragraph does, and the transaction that
    // writes the version also refreshes every open anchor from the document (writeVersionTx), so
    // once the version is there each anchor's quote is what the server reads in the document now.
    // The caret goes after "fox" directly: typeAtEnd's click lands wherever the editor's centre
    // is, which can be a highlight, and the Enter then replaces the selection still on "brown".
    const before = (await getArtifact(artifactId)).versions.length;
    await setSheet(alicePage, testInfo.project.name, false);
    await placeCaret(alicePage, "after", "fox");
    await alicePage.keyboard.press("Enter");
    await alicePage.keyboard.type("jumps");
    await expect
      .poll(() => getArtifact(artifactId).then(({ versions }) => versions.length))
      .toBeGreaterThan(before);
    expect((await getArtifactText(artifactId)).markdown).toBe("The quick brown fox\n\njumps\n");
    const [savedBob, savedAlice] = await Promise.all([
      getComment(bobComment.id),
      getComment(aliceComment.id),
    ]);
    expect({
      quote: savedBob.comment.anchor?.quote,
      orphaned: savedBob.comment.anchor?.orphaned,
    }).toEqual({
      quote: "quick brown",
      orphaned: false,
    });
    expect({
      quote: savedAlice.comment.anchor?.quote,
      orphaned: savedAlice.comment.anchor?.orphaned,
    }).toEqual({
      quote: "brown",
      orphaned: false,
    });

    await Promise.all(
      readers.flatMap((page) => [
        expect.poll(() => markText(page, bobMark)).toBe("quick brown"),
        expect.poll(() => markText(page, aliceMark)).toBe("brown"),
        expect(markSpan(page, bobMark).locator(`[data-id="${aliceMark}"]`)).toHaveCount(1),
        expect(marginCard(page, bobComment.id)).toBeAttached(),
        expect(marginCard(page, aliceComment.id)).toBeAttached(),
      ])
    );

    await bobPage.reload();
    await expect(connectedDot(bobPage)).toHaveText("connected");
    await Promise.all([
      expect.poll(() => markText(bobPage, bobMark)).toBe("quick brown"),
      expect.poll(() => markText(bobPage, aliceMark)).toBe("brown"),
      expect(marginCard(bobPage, bobComment.id)).toBeAttached(),
      expect(marginCard(bobPage, aliceComment.id)).toBeAttached(),
    ]);
  } finally {
    await bob.close();
    await alice.close();
  }
});

// Bob suggests on "quick brown" and his thread stays selected while Alice suggests on "brown"
// from the same selection control a person uses. Selecting a thread only highlights its text;
// both browsers keep Bob's anchor whole, show Alice's inside it, and send both quotes to the server.
test("two readers' suggestions keep both action-bar anchors", async ({ browser }, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: initialMarkdown,
    title: "Suggestion overlap",
  });
  const artifactId = issue.primary_artifact_id;
  const alice = await asUser(browser, "alice");
  const bob = await asUser(browser, "bob");

  try {
    const alicePage = await alice.newPage();
    const bobPage = await bob.newPage();
    const readers = [alicePage, bobPage];
    await Promise.all([
      alicePage.goto(`/issues/${issue.key}/spec`),
      bobPage.goto(`/issues/${issue.key}/spec`),
    ]);
    await Promise.all(readers.map((page) => expect(connectedDot(page)).toHaveText("connected")));

    const pressSuggestion = async (page: Page) => {
      const button = actionBar(page).getByRole("button", { exact: true, name: "Suggest" });
      if (testInfo.project.name === "iphone") {
        await touchHold(page, await centerOf(button), 0);
        return;
      }
      await button.click();
    };

    await selectEditorText(bobPage, "quick brown");
    await pressSuggestion(bobPage);
    const bobComposer = bobPage.getByRole("form", { name: "Comment composer" });
    await bobComposer.getByLabel("Replacement").fill("swift umber");
    await bobComposer.getByRole("button", { exact: true, name: "Send" }).click();
    const bobSuggestion = await commentWithBody(issue.key, artifactId, "Suggested replacement.");
    if (bobSuggestion.anchor === null) throw new Error("Bob's suggestion has no anchor.");
    expect(bobSuggestion.anchor.quote).toBe("quick brown");
    const bobMark = bobSuggestion.anchor.mark_id;
    await Promise.all(
      readers.map((page) => expect.poll(() => markText(page, bobMark)).toBe("quick brown"))
    );

    await setSheet(alicePage, testInfo.project.name, false);
    await selectEditorText(alicePage, "brown");
    await pressSuggestion(alicePage);
    const aliceComposer = alicePage.getByRole("form", { name: "Comment composer" });
    await expect(aliceComposer).toBeVisible();

    // Check before Send: the editor itself must hold both marks, with no rewrite on either side.
    const suggestionMarks = documentEditor(alicePage).locator(
      'span[data-proof="suggestion"][data-id]'
    );
    let newMarkIds: string[] = [];
    await expect
      .poll(async () => {
        newMarkIds = await suggestionMarks.evaluateAll(
          (spans, existingId) =>
            [...new Set(spans.map((span) => span.getAttribute("data-id")))].filter(
              (id): id is string => id !== null && id !== existingId
            ),
          bobMark
        );
        return newMarkIds.length;
      })
      .toBe(1);
    const aliceMark = newMarkIds[0];
    if (aliceMark === undefined)
      throw new Error("Alice's action-bar suggestion mark was not created.");
    await Promise.all(
      readers.flatMap((page) => [
        expect.poll(() => markText(page, bobMark)).toBe("quick brown"),
        expect.poll(() => markText(page, aliceMark)).toBe("brown"),
      ])
    );

    await aliceComposer.getByLabel("Replacement").fill("red");
    await aliceComposer.getByRole("button", { exact: true, name: "Send" }).click();
    await expect
      .poll(() =>
        listComments(issue.key, artifactId).then(
          (comments) =>
            comments.find((comment) => comment.anchor?.mark_id === aliceMark)?.anchor?.quote
        )
      )
      .toBe("brown");
    const aliceSuggestion = (await listComments(issue.key, artifactId)).find(
      (comment) => comment.anchor?.mark_id === aliceMark
    );
    if (aliceSuggestion === undefined) throw new Error("Alice's suggestion was not created.");
    const [savedBob, savedAlice] = await Promise.all([
      getComment(bobSuggestion.id),
      getComment(aliceSuggestion.id),
    ]);
    expect({
      quote: savedBob.comment.anchor?.quote,
      orphaned: savedBob.comment.anchor?.orphaned,
    }).toEqual({
      quote: "quick brown",
      orphaned: false,
    });
    expect({
      quote: savedAlice.comment.anchor?.quote,
      orphaned: savedAlice.comment.anchor?.orphaned,
    }).toEqual({
      quote: "brown",
      orphaned: false,
    });

    await bobPage.reload();
    await expect(connectedDot(bobPage)).toHaveText("connected");
    await Promise.all([
      expect.poll(() => markText(bobPage, bobMark)).toBe("quick brown"),
      expect.poll(() => markText(bobPage, aliceMark)).toBe("brown"),
    ]);
  } finally {
    await bob.close();
    await alice.close();
  }
});
