import { expect, type Page, test } from "@playwright/test";

import { holdPosts, refusePosts } from "./agents";
import { createComment, createIssue, createProject, patchIssue } from "./api";
import { barAction, connectedDot, documentEditor, selectEditorText } from "./editor";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

// The composer a selection-bar action opens holds a send's draft until the server answers, like
// every composer. These rows put each thing that used to unmount it, or move it to another mark,
// in the middle of a send, and expect the send's draft and its refusal back on the composer that
// sent it.
const spec = "The quick brown fox";

test.beforeEach(async () => {
  await resetDatabase();
});

function composer(page: Page) {
  return page.getByRole("form", { name: "Comment composer" });
}

function compact(page: Page): boolean {
  return (page.viewportSize()?.width ?? 1280) < 1280;
}

/** Below `xl` the margin is a sheet: open or close it so the composer in it is on screen or not. */
async function setSheet(page: Page, open: boolean): Promise<void> {
  if (!compact(page)) return;
  const sheet = page.getByTestId("margin-sheet");
  const expanded = open ? "true" : "false";
  if ((await sheet.getAttribute("data-expanded")) !== expanded) {
    await page
      .getByRole("button", { name: open ? /Open review panel/ : /Close review panel/ })
      .click();
  }
  await expect(sheet).toHaveAttribute("data-expanded", expanded);
}

async function openSpec(page: Page, issueKey: string): Promise<void> {
  await page.goto(`/issues/${issueKey}/spec`);
  await expect(documentEditor(page)).toContainText(spec);
  await expect(connectedDot(page)).toHaveText("connected");
}

/** A bar Comment on `quote`, and `body` sent from the composer it opens. */
async function sendFromBar(page: Page, quote: string, body: string): Promise<void> {
  await selectEditorText(page, quote);
  await barAction(page, "Comment");
  const form = composer(page);
  await expect(form.locator("blockquote")).toHaveText(quote);
  await form.getByLabel("Comment").fill(body);
  await form.getByRole("button", { exact: true, name: "Send" }).click();
  await expect(form.getByLabel("Comment")).toBeDisabled();
}

function refusal(page: Page, issueKey: string, status: number) {
  return page.waitForResponse(
    (response) =>
      response.request().method() === "POST" &&
      response.url().endsWith(`/api/v1/issues/${issueKey}/comments`) &&
      response.status() === status
  );
}

// A newer selection-bar action while a send is out would move the open composer to the newer mark:
// the send's success would close it as saved, and a refusal's Retry would post to the newer mark.
// It is refused instead, and the editor takes back the mark it wrote.
test("a newer bar Comment while a margin send is out leaves the composer on the send's own mark", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", spec, title: "Newer bar comment" });
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await openSpec(page, issue.key);
    const refuse = await refusePosts(page, `**/api/v1/issues/${issue.key}/comments`);
    await sendFromBar(page, "quick", "About quick");

    await setSheet(page, false);
    await selectEditorText(page, "fox");
    await barAction(page, "Comment");
    const marks = documentEditor(page).locator('span[data-proof="comment"][data-id]');
    await expect(marks).toHaveText(["quick"]);
    await setSheet(page, true);
    const form = composer(page);
    await expect(form.locator("blockquote")).toHaveText("quick");
    await expect(form.getByLabel("Comment")).toHaveValue("About quick");

    const refused = refusal(page, issue.key, 503);
    refuse();
    await refused;
    await expect(form.getByText("Couldn't send — the server is down")).toBeVisible();
    await expect(form.getByLabel("Comment")).toHaveValue("About quick");
    await expect(form.locator("blockquote")).toHaveText("quick");
  } finally {
    await alice.close();
  }
});

// The Pinned tab used to unmount the Comments tab, and the composer at its top with it.
test("the Pinned tab while a margin send is out keeps the composer, its draft and its refusal", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", spec, title: "Pinned tab mid-send" });
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await openSpec(page, issue.key);
    const refuse = await refusePosts(page, `**/api/v1/issues/${issue.key}/comments`);
    await sendFromBar(page, "brown", "About brown");

    const margin = page.getByTestId("margin-sheet");
    await margin.getByRole("tab", { name: "Pinned" }).click();
    await expect(composer(page)).toHaveCount(0);
    const refused = refusal(page, issue.key, 503);
    refuse();
    await refused;
    await margin.getByRole("tab", { name: "Comments" }).click();

    const form = composer(page);
    await expect(form.getByText("Couldn't send — the server is down")).toBeVisible();
    await expect(form.getByLabel("Comment")).toHaveValue("About brown");
    await expect(form.locator("blockquote")).toHaveText("brown");
  } finally {
    await alice.close();
  }
});

// Collapsing the desktop margin to its rail used to unmount the whole margin, the composer in it.
test("collapsing the margin while its send is out keeps the composer, its draft and its refusal", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name === "iphone", "below xl the margin is a sheet, with no rail");
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", spec, title: "Collapsed mid-send" });
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.setViewportSize({ height: 900, width: 1440 });
    await openSpec(page, issue.key);
    const refuse = await refusePosts(page, `**/api/v1/issues/${issue.key}/comments`);
    await sendFromBar(page, "brown", "Through the rail");

    await page.getByRole("button", { name: "Hide margin" }).click();
    await expect(page.getByTestId("margin-rail")).toBeVisible();
    const refused = refusal(page, issue.key, 503);
    refuse();
    await refused;
    await page.getByRole("button", { name: "Show margin" }).click();

    const form = composer(page);
    await expect(form.getByText("Couldn't send — the server is down")).toBeVisible();
    await expect(form.getByLabel("Comment")).toHaveValue("Through the rail");
  } finally {
    await alice.close();
  }
});

// On a phone a margin thread opens over the margin's own tabs, which used to unmount them and the
// composer with them; Back finds the composer where it was.
test("on a phone, a margin thread opened while the margin's send is out keeps the composer and its refusal", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", spec, title: "Phone margin thread mid-send" });
  const root = await createComment(issue.key, {
    anchor: { artifact: "spec", quote: "fox" },
    body: "About the fox",
  });
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.setViewportSize({ height: 844, width: 390 });
    await openSpec(page, issue.key);
    const refuse = await refusePosts(page, `**/api/v1/issues/${issue.key}/comments`);
    await sendFromBar(page, "quick", "About quick");

    const margin = page.getByTestId("margin-sheet");
    await margin
      .locator(`[data-margin-item="${root.id}"]`)
      .getByRole("button", { expanded: false })
      .first()
      .click();
    const thread = page.getByRole("dialog", { name: "Thread" });
    await expect(thread).toBeVisible();
    const refused = refusal(page, issue.key, 503);
    refuse();
    await refused;
    await thread.getByRole("button", { name: "Back" }).click();
    await expect(thread).toHaveCount(0);

    const form = composer(page);
    await expect(form.getByText("Couldn't send — the server is down")).toBeVisible();
    await expect(form.getByLabel("Comment")).toHaveValue("About quick");
    await expect(form.locator("blockquote")).toHaveText("quick");
  } finally {
    await alice.close();
  }
});

// The issue closing under a margin send used to cancel the composer - unmounting it and removing
// the very mark the send names. It stays until the send answers, and shows its refusal with the
// draft, and a way to drop it, as a closed issue's composers do.
test("a margin send out when its issue closes keeps the draft and shows the refusal", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", spec, title: "Margin closed mid-send" });
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await openSpec(page, issue.key);
    const send = await holdPosts(page, `**/api/v1/issues/${issue.key}/comments`);
    await sendFromBar(page, "brown", "Before the close");

    await patchIssue(issue.key, { status: "done" }, { login: "bob" });
    await expect(page.getByRole("button", { name: "Reopen" })).toBeVisible();
    const form = composer(page);
    await expect(form.getByLabel("Comment")).toHaveValue("Before the close");

    const refused = refusal(page, issue.key, 409);
    send.release();
    await refused;
    await expect(form.getByText("Couldn't send — issue is closed")).toBeVisible();
    await expect(form.getByLabel("Comment")).toHaveValue("Before the close");
    await expect(form.getByRole("button", { name: "Retry" })).toHaveCount(0);
    await form.getByRole("button", { name: "Discard draft" }).click();
    await expect(composer(page)).toHaveCount(0);
  } finally {
    await alice.close();
  }
});
