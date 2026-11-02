/**
 * The `view` block a reader meets (dispatch://INFRA-682): the page in its frame, the data the
 * page draws, a new version of either reaching it, and the line a block shows when Dispatch
 * cannot show it.
 *
 * - A1 (R1, R2): one block in a document, a comment, a reply, an ask's question in the Inbox and
 *   a message; the document's version view; a clipped preview; the frame's height.
 * - A2 (R3): a new version of a data file, a pinned `@v1`, an edit of the caption and of the
 *   comment, ten uploads inside one second, and a project's own data file.
 * - A3 (R4): a new version of the page file.
 * - A6 (R7): every case of "When the block cannot show", save the two the other files hold: a
 *   file no grant covers is `view-grants.e2e.ts`, and a page that moved away from its address is
 *   `view-sandbox.e2e.ts`.
 * - A7 (R8): every row here that shows the page and its value loads
 *   `skills/dispatch-view/minimal-page.html`, the skill's own page, so a green row is the skill's
 *   page working rather than a fixture written to pass.
 *
 * The rows run in `chromium`, `firefox`, `webkit`, `webkit-iphone` and `iphone`: what a frame
 * does with its page, its messages and its height is each engine's own.
 */
import { readFile } from "node:fs/promises";

import {
  expect,
  type Frame,
  type FrameLocator,
  type Locator,
  type Page,
  test,
} from "@playwright/test";

import {
  createArtifactGrant,
  createAsk,
  createComment,
  createIssue,
  createMessage,
  createProject,
  createProjectDocument,
  editArtifact,
  editComment,
  uploadIssueFile,
  uploadProjectFile,
} from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

/** The other session every row writes as, so an edit a row makes is another session's. */
const session = { actor: { id: "e2e-view", kind: "session" as const }, as: "agent" as const };

/** The skill's own minimal page, read from the repository rather than written here (A7). */
let skillPageBytes: Promise<string> | undefined;
function skillPage(): Promise<string> {
  skillPageBytes ??= readFile(
    new URL("../../../skills/dispatch-view/minimal-page.html", import.meta.url),
    "utf8"
  );
  return skillPageBytes;
}

/** The data the skill's page draws as one bar: its `.value` is what a row reads back. */
function attempts(value: number): string {
  return JSON.stringify([{ label: "attempts", value }]);
}

/** A `view` block as an author writes it. */
function viewDirective(block: {
  caption: string;
  data: string;
  id?: string;
  page: string;
}): string {
  const id = block.id === undefined ? "" : `#${block.id} `;
  return `:::view{${id}page="${block.page}" data="${block.data}"}\n${block.caption}\n:::\n`;
}

/** One `dispatch-view:data` message, as a row reads it back out of a frame. */
interface ViewDataMessage {
  files: { error?: string; ref: string; text?: string; version?: number }[];
}

/** The marker a row sets inside a running frame, and the messages it counts there. The frame's
 *  own window holds both, so a reload or a new frame element loses them, which is what the rows
 *  about an edit and about ten uploads read. */
interface MarkedWindow extends Window {
  __viewMark?: string;
  __viewData?: ViewDataMessage[];
}

/** The block whose caption is `caption`, in a document or in any body. */
function viewBlock(page: Page, caption: string): Locator {
  return page.locator('section[data-proof-block-type="view"]').filter({ hasText: caption });
}

function viewFrame(block: Locator): FrameLocator {
  return block.frameLocator("iframe");
}

/** The value the skill's page drew for the first data file it was given. */
function drawnValue(block: Locator): Locator {
  return viewFrame(block).locator(".bars .value").first();
}

/** The one running frame of the page file `pageId`, as a frame a row can evaluate in. */
function pageFrame(page: Page, pageId: string): Frame {
  const frame = page.frames().find((item) => item.url().includes(`/artifacts/${pageId}/versions/`));
  if (frame === undefined) {
    throw new Error(`no frame of the page file ${pageId} is on the page`);
  }
  return frame;
}

/** Marks the running frame and starts counting the `dispatch-view:data` messages it gets. Both
 *  live in the frame's own window, which only a new frame or a reload clears. */
async function markFrame(page: Page, pageId: string): Promise<void> {
  await pageFrame(page, pageId).evaluate(() => {
    // The page's own window, with the two fields this spec writes on it.
    const marked: MarkedWindow = window;
    marked.__viewMark = "set";
    marked.__viewData = [];
    window.addEventListener("message", (event) => {
      if (event.source !== window.parent) {
        return;
      }
      const message = event.data;
      if (
        message !== null &&
        typeof message === "object" &&
        message.type === "dispatch-view:data"
      ) {
        marked.__viewData?.push(message);
      }
    });
  });
}

/** `"set"` while the frame marked above is still the frame running now, `"gone"` once it was
 *  replaced or its page reloaded. */
function frameMark(page: Page, pageId: string): Promise<string> {
  return pageFrame(page, pageId).evaluate(() => {
    const marked: MarkedWindow = window;
    return marked.__viewMark ?? "gone";
  });
}

function frameMessages(page: Page, pageId: string): Promise<ViewDataMessage[]> {
  return pageFrame(page, pageId).evaluate(() => {
    const marked: MarkedWindow = window;
    return marked.__viewData ?? [];
  });
}

/** How many times the browser read one file's bytes, which is what the ten-uploads row counts.
 *  The page route (`/versions/{n}/page`) is a read of the page, not of a data file, so the
 *  pattern ends at the version. */
function countVersionReads(page: Page, artifactId: string): () => number {
  const pattern = new RegExp(`^/api/v1/artifacts/${artifactId}/versions/\\d+$`);
  let reads = 0;
  page.on("request", (request) => {
    if (pattern.test(new URL(request.url()).pathname)) {
      reads += 1;
    }
  });
  return () => reads;
}

interface Fixture {
  readonly dataId: string;
  readonly dataRef: string;
  readonly issue: string;
  readonly pageId: string;
  readonly pageRef: string;
  readonly project: string;
}

/** An issue holding the skill's page and one JSON data file granted to it: where nearly every
 *  row starts. A page file and a data file are uploaded as files (multipart), since a JSON
 *  upload is stored as markdown, which is a document, and a view reads no document. */
async function seedView(value = 7): Promise<Fixture> {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Kernel boot loop" });
  const pageFile = await uploadIssueFile(issue.key, {
    content: await skillPage(),
    name: "chart.html",
    type: "text/html; charset=utf-8",
  });
  const dataFile = await uploadIssueFile(issue.key, {
    content: attempts(value),
    name: "loop.json",
    type: "application/json",
  });
  const dataRef = `dispatch://${issue.key}/artifact/loop-json`;
  await createArtifactGrant(pageFile.artifact.id, dataRef);
  return {
    dataId: dataFile.artifact.id,
    dataRef,
    issue: issue.key,
    pageId: pageFile.artifact.id,
    pageRef: `dispatch://${issue.key}/artifact/chart-html`,
    project: "CORE",
  };
}

/** A project document holding `content`, opened. */
async function openDocument(page: Page, content: string, name = "Loop view"): Promise<string> {
  const document = await createProjectDocument("CORE", { content, name });
  await page.goto(`/projects/CORE/documents/${document.artifact.slug}`);
  return document.artifact.id;
}

test.beforeEach(async () => {
  await resetDatabase();
});

// ---------------------------------------------------------------------------
// A1 (R1, R2), with A7: the block shows the skill's page, in every place a block shows.
// ---------------------------------------------------------------------------

test("a view block in a document shows the skill's page drawing its data file @phone", async ({
  browser,
}) => {
  const fixture = await seedView();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await openDocument(
      page,
      viewDirective({
        caption: "Attempts of the kernel boot loop.",
        data: fixture.dataRef,
        id: "loop-chart",
        page: fixture.pageRef,
      })
    );

    const block = viewBlock(page, "Attempts of the kernel boot loop.");
    await expect(block, "A1: the caption shows beside the frame").toContainText(
      "Attempts of the kernel boot loop."
    );
    await expect(drawnValue(block), "A1: the skill's page drew the data file's value").toHaveText(
      "7"
    );
    await expect(
      viewFrame(block).locator(".card .name"),
      "A1: the page names the file Dispatch gave it"
    ).toHaveText("loop.json");
  } finally {
    await alice.close();
  }
});

test("a view block shows the same in a comment, a reply, an ask in the Inbox and a message @phone", async ({
  browser,
}) => {
  const fixture = await seedView();
  const caption = (where: string) => `Attempts, in ${where}.`;
  const body = (where: string) =>
    viewDirective({ caption: caption(where), data: fixture.dataRef, page: fixture.pageRef });

  const comment = await createComment(fixture.issue, { body: body("a comment") }, session);
  await createComment(fixture.issue, { body: body("a reply"), reply_to: comment.id }, session);
  const message = await createMessage(fixture.issue, { body: body("a message") }, session);
  const ask = await createAsk(
    fixture.issue,
    {
      options: [{ label: "Ship" }, { label: "Hold" }],
      question: body("an ask"),
    },
    session
  );

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${fixture.issue}/conversation`);

    // A root comment's card shows a clipped preview until the reader opens its thread, where
    // the comment and its replies are drawn in full.
    await page
      .locator(`[data-turn="comment:${comment.id}"]`)
      .getByRole("button", { name: "Expand thread" })
      .click();
    await expect(
      drawnValue(viewBlock(page, caption("a comment"))),
      "A1: the block of a comment shows the page and its value"
    ).toHaveText("7");
    await expect(
      drawnValue(viewBlock(page, caption("a reply"))),
      "A1: the block of a reply shows the page and its value"
    ).toHaveText("7");
    await expect(
      drawnValue(viewBlock(page, caption("a message"))),
      "A1: the block of a message shows the page and its value"
    ).toHaveText("7");

    // The ask, as the Inbox shows it.
    await page.goto(`/?item=${ask.id}&assignee=everyone`);
    await expect(
      drawnValue(viewBlock(page, caption("an ask"))),
      "A1: the block of an ask's question shows the page and its value in the Inbox"
    ).toHaveText("7");
    expect(message.id, "A1: the message was written").not.toBe("");
  } finally {
    await alice.close();
  }
});

test("a view block shows its page in a document's version view @phone", async ({ browser }) => {
  const fixture = await seedView();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const documentId = await openDocument(
      page,
      viewDirective({
        caption: "Attempts, as of version 1.",
        data: fixture.dataRef,
        page: fixture.pageRef,
      })
    );
    await expect(drawnValue(viewBlock(page, "Attempts, as of version 1."))).toHaveText("7");

    // A second version, so the picker has a version to go back to.
    await editArtifact(
      documentId,
      { ops: [{ find: "as of version 1", op: "replace", with: "as of version 2" }] },
      session
    );
    await expect(viewBlock(page, "Attempts, as of version 2.")).toBeVisible();

    await page.getByRole("combobox", { name: "Version" }).selectOption("1");
    const versionView = page.getByTestId("version-view");
    await expect(versionView, "A1: the version view holds the block").toContainText(
      "Attempts, as of version 1."
    );
    await expect(
      drawnValue(versionView.locator('section[data-proof-block-type="view"]')),
      "A1: the version view shows the page and the newest data"
    ).toHaveText("7");
  } finally {
    await alice.close();
  }
});

// A version view sits on the page beside the live editor, which `ProofDocument` only hides. Both
// hold their own `view` node views, and a block of either must be drawn once: one frame and one
// copy of each line. Drawn from one registry shared by both editors, every block got two.
test("a view block is drawn once in a version view open over the live editor @phone", async ({
  browser,
}) => {
  const fixture = await seedView();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    // A caption of one empty paragraph, so each block carries a line a reader can count: the
    // note that it has no caption, which a block shows whether or not its frame runs.
    await openDocument(
      page,
      `:::view{page="${fixture.pageRef}" data="${fixture.dataRef}"}\n&#x20;\n:::\n`
    );
    const noCaption = `${fixture.pageRef} — this view has no caption`;
    const live = page
      .locator('article[aria-label="Document"][hidden]')
      .locator('section[data-proof-block-type="view"]');
    const historical = page
      .getByTestId("version-view")
      .locator('section[data-proof-block-type="view"]');
    await expect(
      drawnValue(page.locator('section[data-proof-block-type="view"]')),
      "A1: the live editor's block shows its page first"
    ).toHaveText("7");

    await page.getByRole("combobox", { name: "Version" }).selectOption("1");
    await expect(historical, "A1: the version view holds the block").toBeVisible();
    await expect(
      drawnValue(historical),
      "A1: the version view's block shows the page and its data"
    ).toHaveText("7");
    await expect(
      historical.locator("iframe"),
      "A1: one frame in the version view's block"
    ).toHaveCount(1);
    await expect(historical.getByText(noCaption), "A1: and one copy of its line").toHaveCount(1);
    await expect(
      historical.getByText("waiting"),
      "A1: and nothing waiting beside the frame it has"
    ).toHaveCount(0);

    // The live editor is hidden, so its own block is off screen for the budget: it waits, once,
    // and holds no frame while the version is open.
    await expect(
      live.getByText(noCaption),
      "A1: one copy of the live editor's block's line"
    ).toHaveCount(1);
    await expect(live.getByText("waiting"), "A1: which waits, once").toHaveCount(1);
    await expect(live.locator("iframe"), "A1: and holds no frame").toHaveCount(0);
  } finally {
    await alice.close();
  }
});

test("a view block in a clipped preview of a comment shows its caption and no frame @phone", async ({
  browser,
}) => {
  const fixture = await seedView();
  const issue = await createIssue({
    project: "CORE",
    spec: "The loop attempts are drawn beside this line.",
    title: "Loop review",
  });
  const comment = await createComment(
    issue.key,
    {
      anchor: { artifact: "spec", quote: "drawn beside this line" },
      body: viewDirective({
        caption: "Attempts, in a margin card.",
        data: fixture.dataRef,
        page: fixture.pageRef,
      }),
    },
    session
  );

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/spec`);
    const card = page.locator(`[data-margin-item="${comment.id}"]`);
    await expect(card, "A1: the clipped preview shows the caption").toContainText(
      "Attempts, in a margin card."
    );
    await expect(card.locator("iframe"), "A1: a clipped preview draws no frame").toHaveCount(0);
  } finally {
    await alice.close();
  }
});

test("a view block's frame is 400 pixels until its page reports, then held to 100 and 2,000 @phone", async ({
  browser,
}) => {
  const fixture = await seedView();
  // A page that waits two seconds, so the frame is measured before its first report, then
  // reports 50, 5,000 and "abc", one a second, so each is applied in a frame of its own.
  const reporting = `<!doctype html>
<html lang="en"><body><p>Heights.</p><script>
  const post = (height) => parent.postMessage({ height, type: "dispatch-view:height" }, "*");
  parent.postMessage({ type: "dispatch-view:ready" }, "*");
  setTimeout(() => {
    post(50);
    setTimeout(() => {
      post(5000);
      setTimeout(() => post("abc"), 1000);
    }, 1000);
  }, 2000);
</script></body></html>
`;
  const reporter = await uploadIssueFile(fixture.issue, {
    content: reporting,
    name: "heights.html",
    type: "text/html; charset=utf-8",
  });
  await createArtifactGrant(reporter.artifact.id, fixture.dataRef);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await openDocument(
      page,
      viewDirective({
        caption: "A page that reports heights.",
        data: fixture.dataRef,
        page: `dispatch://${fixture.issue}/artifact/heights-html`,
      })
    );
    const frame = viewBlock(page, "A page that reports heights.").locator("iframe");
    const height = async () => Math.round((await frame.boundingBox())?.height ?? 0);

    await expect(frame).toBeVisible();
    expect(await height(), "A1: 400 pixels before the page reports a height").toBe(400);
    await expect
      .poll(height, { message: "A1: a reported 50 is held to 100", timeout: 10_000 })
      .toBe(100);
    await expect
      .poll(height, { message: "A1: a reported 5,000 is held to 2,000", timeout: 10_000 })
      .toBe(2_000);
    await page.waitForTimeout(2_000);
    expect(await height(), 'A1: a reported "abc" changes nothing').toBe(2_000);
  } finally {
    await alice.close();
  }
});

// ---------------------------------------------------------------------------
// A2 (R3), with A7: a new version of a data file reaches the running page.
// ---------------------------------------------------------------------------

test("a view block shows a new data version within 5 seconds and keeps its page running @phone", async ({
  browser,
}) => {
  const fixture = await seedView();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await openDocument(
      page,
      `${viewDirective({
        caption: "Attempts, newest.",
        data: fixture.dataRef,
        page: fixture.pageRef,
      })}\n${viewDirective({
        caption: "Attempts, pinned to version 1.",
        data: `${fixture.dataRef}@v1`,
        page: fixture.pageRef,
      })}`
    );
    const newest = viewBlock(page, "Attempts, newest.");
    const pinned = viewBlock(page, "Attempts, pinned to version 1.");
    await expect(drawnValue(newest)).toHaveText("7");
    await expect(drawnValue(pinned)).toHaveText("7");
    await markFrame(page, fixture.pageId);

    await uploadIssueFile(fixture.issue, {
      content: attempts(9),
      name: "loop.json",
      type: "application/json",
    });
    const uploaded = Date.now();
    await expect
      .poll(() => drawnValue(newest).textContent(), {
        message: "A2: the new value reaches the page within 5 seconds of the upload's answer",
        timeout: 5_000,
      })
      .toBe("9");
    expect(Date.now() - uploaded, "A2: within 5 seconds").toBeLessThanOrEqual(5_000);
    expect(
      await frameMark(page, fixture.pageId),
      "A2: the page kept running, so it was never reloaded"
    ).toBe("set");
    await expect(
      drawnValue(pinned),
      "A2: a block that names @v1 still shows the first value"
    ).toHaveText("7");
  } finally {
    await alice.close();
  }
});

test("a view block keeps its page running through another session's edit of the caption @phone", async ({
  browser,
}) => {
  const fixture = await seedView();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const documentId = await openDocument(
      page,
      viewDirective({
        caption: "Attempts of the loop.",
        data: fixture.dataRef,
        page: fixture.pageRef,
      })
    );
    await expect(drawnValue(viewBlock(page, "Attempts of the loop."))).toHaveText("7");
    await markFrame(page, fixture.pageId);

    await editArtifact(
      documentId,
      { ops: [{ find: "Attempts of the loop.", op: "replace", with: "Attempts, re-worded." }] },
      session
    );
    await expect(
      viewBlock(page, "Attempts, re-worded."),
      "A2: the other session's caption edit landed"
    ).toBeVisible();
    expect(
      await frameMark(page, fixture.pageId),
      "A2: a caption edit by another session leaves the page running"
    ).toBe("set");
  } finally {
    await alice.close();
  }
});

// `PATCH /api/v1/comments/{id}` is `authHuman` and refuses anyone but the comment's author
// (`403 HUMAN_ONLY`, `403 NOT_AUTHOR`), so the session that edits the comment around the block
// is the reader's own login acting through the API, beside the tab that is watching. The
// reader's tab never touches the comment: the edit reaches it over the live socket, which is
// what this row is about.
test("a view block keeps its page running through another session's edit of its comment @phone", async ({
  browser,
}) => {
  const fixture = await seedView();
  const comment = await createComment(fixture.issue, {
    body: viewDirective({
      caption: "Attempts, in a comment.",
      data: fixture.dataRef,
      page: fixture.pageRef,
    }),
  });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${fixture.issue}/conversation`);
    // The card draws the comment in full once its thread is open. On the phone layout the
    // thread opens as a dialog over the tab, which mounts a second card under the same
    // `data-turn`; the one that draws the body is the one holding the block's section, and the
    // other shows a clipped preview with no section at all.
    await page
      .locator(`[data-turn="comment:${comment.id}"]`)
      .getByRole("button", { name: "Expand thread" })
      .click();
    const opened = page
      .locator(`[data-turn="comment:${comment.id}"]`)
      .filter({ has: page.locator('section[data-proof-block-type="view"]') });
    await expect(drawnValue(viewBlock(page, "Attempts, in a comment."))).toHaveText("7");
    await markFrame(page, fixture.pageId);

    await editComment(comment.id, {
      body: `A line the edit added.\n\n${viewDirective({
        caption: "Attempts, in a comment.",
        data: fixture.dataRef,
        page: fixture.pageRef,
      })}`,
    });
    await expect(opened, "A2: the other session's edit of the comment landed").toContainText(
      "A line the edit added."
    );
    expect(
      await frameMark(page, fixture.pageId),
      "A2: an edit of the comment around the block leaves the page running"
    ).toBe("set");
  } finally {
    await alice.close();
  }
});

test("a view block sends ten uploads inside a second as at most two messages and two reads @phone", async ({
  browser,
}) => {
  const fixture = await seedView();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const reads = countVersionReads(page, fixture.dataId);
    await openDocument(
      page,
      viewDirective({
        caption: "Attempts, under ten uploads.",
        data: fixture.dataRef,
        page: fixture.pageRef,
      })
    );
    const block = viewBlock(page, "Attempts, under ten uploads.");
    await expect(drawnValue(block)).toHaveText("7");
    await markFrame(page, fixture.pageId);
    const readsBefore = reads();

    const started = Date.now();
    for (let upload = 1; upload <= 10; upload += 1) {
      await uploadIssueFile(fixture.issue, {
        content: attempts(10 + upload),
        name: "loop.json",
        type: "application/json",
      });
    }
    const spent = Date.now() - started;
    expect(spent, "A2: the ten uploads were made inside one second").toBeLessThanOrEqual(1_000);

    await expect
      .poll(() => drawnValue(block).textContent(), {
        message: "A2: the newest value reaches the page",
        timeout: 10_000,
      })
      .toBe("20");
    // Past two send windows, so a third message would have gone out by now.
    await page.waitForTimeout(5_000);
    const messages = await frameMessages(page, fixture.pageId);
    expect(
      messages.length,
      "A2: ten uploads inside a second are at most two messages"
    ).toBeLessThanOrEqual(2);
    expect(
      messages.at(-1)?.files.map((file) => file.text),
      "A2: the last message carries the newest version"
    ).toEqual([attempts(20)]);
    expect(
      reads() - readsBefore,
      "A2: and cost at most two reads of the file's bytes"
    ).toBeLessThanOrEqual(2);
    expect(await frameMark(page, fixture.pageId), "A2: the page never reloaded").toBe("set");
  } finally {
    await alice.close();
  }
});

test("a view block shows a new version of a project's data file within 5 seconds @phone", async ({
  browser,
}) => {
  const fixture = await seedView();
  await uploadProjectFile("CORE", {
    content: attempts(3),
    name: "shared.json",
    type: "application/json",
  });
  const projectData = "dispatch://CORE/artifact/shared-json";
  await createArtifactGrant(fixture.pageId, projectData);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await openDocument(
      page,
      viewDirective({
        caption: "Attempts from the project's file.",
        data: projectData,
        page: fixture.pageRef,
      })
    );
    const block = viewBlock(page, "Attempts from the project's file.");
    await expect(drawnValue(block)).toHaveText("3");
    await markFrame(page, fixture.pageId);

    await uploadProjectFile("CORE", {
      content: attempts(4),
      name: "shared.json",
      type: "application/json",
    });
    await expect
      .poll(() => drawnValue(block).textContent(), {
        message: "A2: a project file's new version reaches the page within 5 seconds",
        timeout: 5_000,
      })
      .toBe("4");
    expect(
      await frameMark(page, fixture.pageId),
      "A2: the page kept running for a project file too"
    ).toBe("set");
  } finally {
    await alice.close();
  }
});

// ---------------------------------------------------------------------------
// A3 (R4): a new version of the page file.
// ---------------------------------------------------------------------------

test("a view block shows a new version of its page file within 5 seconds @phone", async ({
  browser,
}) => {
  const fixture = await seedView();
  const pageOf = (words: string) =>
    `<!doctype html>\n<html lang="en"><body><p id="words">${words}</p><script>parent.postMessage({ type: "dispatch-view:ready" }, "*");</script></body></html>\n`;
  await uploadIssueFile(fixture.issue, {
    content: pageOf("page one"),
    name: "story.html",
    type: "text/html; charset=utf-8",
  });
  const story = `dispatch://${fixture.issue}/artifact/story-html`;

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await openDocument(
      page,
      viewDirective({ caption: "The page itself changes.", data: fixture.dataRef, page: story })
    );
    const block = viewBlock(page, "The page itself changes.");
    await expect(viewFrame(block).locator("#words")).toHaveText("page one");

    await uploadIssueFile(fixture.issue, {
      content: pageOf("page two"),
      name: "story.html",
      type: "text/html; charset=utf-8",
    });
    const uploaded = Date.now();
    await expect(
      viewFrame(block).locator("#words"),
      "A3: the new page replaces the old within 5 seconds of the upload's answer"
    ).toHaveText("page two", { timeout: 5_000 });
    expect(Date.now() - uploaded, "A3: within 5 seconds").toBeLessThanOrEqual(5_000);
  } finally {
    await alice.close();
  }
});

// ---------------------------------------------------------------------------
// A6 (R7): every case of "When the block cannot show".
// ---------------------------------------------------------------------------

test("a view block whose attributes name no file shows its caption and one line each @phone", async ({
  browser,
}) => {
  const fixture = await seedView();
  const nine = Array.from({ length: 9 }, (_, index) => `${fixture.dataRef}-${index}`).join(" ");
  const agentFile = "dispatch://agent/11111111-2222-3333-4444-555555555555/artifact/chart-html";
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await openDocument(
      page,
      [
        viewDirective({ caption: "No page named.", data: fixture.dataRef, page: "" }),
        viewDirective({ caption: "No data named.", data: "", page: fixture.pageRef }),
        viewDirective({ caption: "Nine data files.", data: nine, page: fixture.pageRef }),
        viewDirective({
          caption: "The page is an issue.",
          data: fixture.dataRef,
          page: `dispatch://${fixture.issue}`,
        }),
        viewDirective({
          caption: "The page is an agent's file.",
          data: fixture.dataRef,
          page: agentFile,
        }),
      ].join("\n")
    );

    await expect(
      viewBlock(page, "No page named.").getByText("page — this view names no page file"),
      "A6: an empty page attribute"
    ).toBeVisible();
    await expect(
      viewBlock(page, "No data named.").getByText("data — this view names no data file"),
      "A6: an empty data attribute"
    ).toBeVisible();
    await expect(
      viewBlock(page, "Nine data files.").getByText(
        "data — this view names 9 data files, and a view shows at most 8"
      ),
      "A6: more than eight data references"
    ).toBeVisible();
    await expect(
      viewBlock(page, "The page is an issue.").getByText(
        `dispatch://${fixture.issue} — this is not an issue file or a project file`
      ),
      "A6: a reference that names no file"
    ).toBeVisible();
    await expect(
      viewBlock(page, "The page is an agent's file.").getByText(
        `${agentFile} — this is not an issue file or a project file`
      ),
      "A6: an agent conversation file"
    ).toBeVisible();
    await expect(
      page.locator('section[data-proof-block-type="view"] iframe'),
      "A6: none of them starts a frame"
    ).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

test("a view block whose page or data file is not found says so, and draws once it is uploaded @phone", async ({
  browser,
}) => {
  const fixture = await seedView();
  const missingPage = `dispatch://${fixture.issue}/artifact/absent-html`;
  const missingData = `dispatch://${fixture.issue}/artifact/absent-json`;
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await openDocument(
      page,
      [
        viewDirective({
          caption: "The page is missing.",
          data: fixture.dataRef,
          page: missingPage,
        }),
        viewDirective({
          caption: "The data is missing.",
          data: missingData,
          page: fixture.pageRef,
        }),
        viewDirective({
          caption: "The data is a document.",
          data: "dispatch://CORE/artifact/loop-view",
          page: fixture.pageRef,
        }),
      ].join("\n")
    );

    await expect(
      viewBlock(page, "The page is missing.").getByText(`${missingPage} — this file is not found`),
      "A6: a page file that is not found"
    ).toBeVisible();
    const missingBlock = viewBlock(page, "The data is missing.");
    await expect(
      missingBlock.getByText(`${missingData} — this file is not found`),
      "A6: a data file that is not found"
    ).toBeVisible();
    await expect(
      viewBlock(page, "The data is a document.").getByText(
        "dispatch://CORE/artifact/loop-view — a document is not a file a view reads"
      ),
      "A6: a document is no data file"
    ).toBeVisible();

    // The same block draws the file as soon as it exists.
    const uploaded = await uploadIssueFile(fixture.issue, {
      content: attempts(5),
      name: "absent.json",
      type: "application/json",
    });
    await createArtifactGrant(fixture.pageId, missingData);
    const at = Date.now();
    await expect(
      drawnValue(missingBlock),
      "A6: the data arrives once the file is uploaded"
    ).toHaveText("5", { timeout: 5_000 });
    expect(Date.now() - at, "A6: within 5 seconds of the first upload").toBeLessThanOrEqual(5_000);
    expect(uploaded.artifact.slug, "A6: the file was uploaded under the named slug").toBe(
      "absent-json"
    );
  } finally {
    await alice.close();
  }
});

test("a view block whose page file is no page, or is over 5 MiB, says which @phone", async ({
  browser,
}) => {
  const fixture = await seedView();
  await uploadIssueFile(fixture.issue, {
    content: "attempts,count\n1,7\n",
    name: "table.csv",
    type: "text/csv",
  });
  const big = await uploadIssueFile(fixture.issue, {
    content: `<!doctype html><html lang="en"><body><p>${"x".repeat(5 * 1024 * 1024)}</p></body></html>`,
    name: "huge.html",
    type: "text/html; charset=utf-8",
  });
  const notAPage = `dispatch://${fixture.issue}/artifact/table-csv`;
  const tooLarge = `dispatch://${fixture.issue}/artifact/huge-html`;
  await createArtifactGrant(big.artifact.id, fixture.dataRef);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await openDocument(
      page,
      [
        viewDirective({ caption: "The page is a table.", data: fixture.dataRef, page: notAPage }),
        viewDirective({ caption: "The page is huge.", data: fixture.dataRef, page: tooLarge }),
      ].join("\n")
    );

    await expect(
      viewBlock(page, "The page is a table.").getByText(
        `${notAPage} — this file is not a page file`
      ),
      "A6: a page file that is not a page"
    ).toBeVisible();
    await expect(
      viewBlock(page, "The page is huge.").getByText(
        `${tooLarge} — this page is larger than 5 MiB`
      ),
      "A6: a page over 5 MiB"
    ).toBeVisible();
    await expect(
      page.locator('section[data-proof-block-type="view"] iframe'),
      "A6: neither starts a frame"
    ).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

// A 1x1 PNG. Its first byte, 0x89, is a continuation byte with no lead byte before it, so a
// fatal UTF-8 decode of these bytes throws.
const tinyPng =
  "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVQIHWP4z8DwHwAFgAI/ScL0eAAAAABJRU5ErkJggg==";

/** A document whose one block names the JSON data file and a PNG, in that order, both granted. */
async function openNotTextBlock(
  page: Page
): Promise<{ block: Locator; jsonRef: string; pngRef: string }> {
  const fixture = await seedView();
  const pngRef = `dispatch://${fixture.issue}/artifact/picture-png`;
  await uploadIssueFile(fixture.issue, {
    content: Buffer.from(tinyPng, "base64"),
    name: "picture.png",
    type: "image/png",
  });
  await createArtifactGrant(fixture.pageId, pngRef);
  await openDocument(
    page,
    viewDirective({
      caption: "Attempts, with a picture beside them.",
      data: `${fixture.dataRef} ${pngRef}`,
      page: fixture.pageRef,
    })
  );
  const block = viewBlock(page, "Attempts, with a picture beside them.");
  await expect(block).toContainText("Attempts, with a picture beside them.");
  return { block, jsonRef: fixture.dataRef, pngRef };
}

test("a view block still sends every other file when one is not UTF-8 text @phone", async ({
  browser,
}) => {
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const { block, jsonRef, pngRef } = await openNotTextBlock(page);
    await expect(drawnValue(block), "A6: the JSON file still reached the page").toHaveText("7");
    await expect(
      viewFrame(block).locator(`.card[data-ref="${pngRef}"]`),
      "A6: and the picture reached it as a not-text entry"
    ).toHaveAttribute("data-error", "not-text");
    await expect(
      viewFrame(block).locator(`.card[data-ref="${jsonRef}"]`),
      "A6: the entries keep the order of the data attribute"
    ).toBeVisible();
  } finally {
    await alice.close();
  }
});

test("a view block names the file that is not UTF-8 text and says why @phone", async ({
  browser,
}) => {
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const { block, pngRef } = await openNotTextBlock(page);
    await expect(
      block.getByText(`${pngRef} — this file is not UTF-8 text`),
      "A6: the line names the picture's reference and its cause"
    ).toBeVisible();
  } finally {
    await alice.close();
  }
});

test("a view block whose data passes 16 MiB sends no data and says so @phone", async ({
  browser,
}) => {
  const fixture = await seedView();
  await uploadIssueFile(fixture.issue, {
    content: "x".repeat(17 * 1024 * 1024),
    name: "huge.json",
    type: "application/json",
  });
  const hugeRef = `dispatch://${fixture.issue}/artifact/huge-json`;
  await createArtifactGrant(fixture.pageId, hugeRef);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await openDocument(
      page,
      viewDirective({
        caption: "Attempts and one huge file.",
        data: `${fixture.dataRef} ${hugeRef}`,
        page: fixture.pageRef,
      })
    );
    const block = viewBlock(page, "Attempts and one huge file.");
    await expect(
      block.getByText(
        "data — the data files together are larger than 16 MiB, so Dispatch sends no data"
      ),
      "A6: the block says the data is over the limit"
    ).toBeVisible();
    await expect(
      viewFrame(block).locator("#waiting"),
      "A6: and the page is given no data at all"
    ).toBeVisible();
  } finally {
    await alice.close();
  }
});

test("a view block counts the version it sends against 16 MiB, not the newest @phone", async ({
  browser,
}) => {
  const fixture = await seedView();
  // Version 1 is over the limit on its own; version 2 is a few bytes. A reference that pins
  // `@v1` is sent version 1, so version 1's bytes are the block's data.
  await uploadIssueFile(fixture.issue, {
    content: "x".repeat(17 * 1024 * 1024),
    name: "huge.json",
    type: "application/json",
  });
  await uploadIssueFile(fixture.issue, {
    content: attempts(3),
    name: "huge.json",
    type: "application/json",
  });
  const hugeRef = `dispatch://${fixture.issue}/artifact/huge-json`;
  await createArtifactGrant(fixture.pageId, hugeRef);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await openDocument(
      page,
      viewDirective({
        caption: "Attempts and one huge old version.",
        data: `${fixture.dataRef} ${hugeRef}@v1`,
        page: fixture.pageRef,
      })
    );
    const block = viewBlock(page, "Attempts and one huge old version.");
    await expect(
      block.getByText(
        "data — the data files together are larger than 16 MiB, so Dispatch sends no data"
      ),
      "R6: the block weighs the version it sends, so a pinned 17 MiB version is over the limit"
    ).toBeVisible();
    await expect(
      viewFrame(block).locator("#waiting"),
      "R6: and the page is given no data at all"
    ).toBeVisible();
  } finally {
    await alice.close();
  }
});

test("a view block whose read of a data file fails names its status @phone", async ({
  browser,
}) => {
  const fixture = await seedView();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    // Only the read of a version's bytes: the page route, `/versions/{n}/page`, is not it.
    await page.route(
      (url) => /^\/api\/v1\/artifacts\/[^/]+\/versions\/\d+$/.test(url.pathname),
      (route) => route.fulfill({ body: "no", status: 503 })
    );
    await openDocument(
      page,
      viewDirective({
        caption: "Attempts, unread.",
        data: fixture.dataRef,
        page: fixture.pageRef,
      })
    );
    await expect(
      viewBlock(page, "Attempts, unread.").getByText(
        `${fixture.dataRef} — Dispatch answered 503 for this file`
      ),
      "A6: a read that failed names the answer"
    ).toBeVisible();
  } finally {
    await alice.close();
  }
});

test("a view block whose page sends no ready message in 10 seconds says so @phone", async ({
  browser,
}) => {
  const fixture = await seedView();
  const silentPage = await uploadIssueFile(fixture.issue, {
    content: '<!doctype html>\n<html lang="en"><body><p>No ready message.</p></body></html>\n',
    name: "silent.html",
    type: "text/html; charset=utf-8",
  });
  const silent = `dispatch://${fixture.issue}/artifact/silent-html`;
  await createArtifactGrant(silentPage.artifact.id, fixture.dataRef);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await openDocument(
      page,
      viewDirective({ caption: "A silent page.", data: fixture.dataRef, page: silent })
    );
    await expect(
      viewBlock(page, "A silent page.").getByText(
        `${silent} — this page sent no ready message in 10 seconds`
      ),
      "A6: a page that never answers ready"
    ).toBeVisible({ timeout: 20_000 });
  } finally {
    await alice.close();
  }
});

test("a view block with an empty caption shows its page and says it has none @phone", async ({
  browser,
}) => {
  const fixture = await seedView();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    // A caption of one empty paragraph: the block's content rule needs a paragraph, and this one
    // holds no words.
    await openDocument(
      page,
      `:::view{page="${fixture.pageRef}" data="${fixture.dataRef}"}\n&#x20;\n:::\n`
    );
    const block = page.locator('section[data-proof-block-type="view"]');
    await expect(
      block.getByText(`${fixture.pageRef} — this view has no caption`),
      "A6: the block says it has no caption"
    ).toBeVisible();
    await expect(drawnValue(block), "A6: and it shows its page all the same").toHaveText("7");
  } finally {
    await alice.close();
  }
});
