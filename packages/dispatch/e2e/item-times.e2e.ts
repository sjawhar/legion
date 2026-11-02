import { expect, test } from "@playwright/test";

import { createComment, createIssue, createMessage, createProject } from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

test.beforeEach(async () => {
  await resetDatabase();
});

// The time on a reply is a link to the reply, as a comment's time is on GitHub. Clicking it on
// the Conversation tab moves the reader to the reply's own address without reloading the tab:
// the reply is marked as a deep link marks it, and the turns list the reader was in is the same
// one, never swapped for the issue page's "Loading issue…".
test("a reply's time puts the reply's address in the address bar and marks it, on the tab already open", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Sextant calibration" });
  const root = await createMessage(issue.key, { body: "Which mirror is out of true?" });
  const reply = await createMessage(issue.key, {
    body: "The horizon mirror, by two arc-minutes.",
    in_reply_to: root.id,
  });
  // Later turns put the reply above the newest one, where the Conversation opens.
  for (let index = 0; index < 12; index++) {
    await createMessage(issue.key, { body: `Later note ${index}.` });
  }
  const replyPath = `/issues/${issue.key}/messages/${reply.id}`;

  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    const turns = page.getByRole("list", { name: "Conversation turns" });
    await expect(turns.getByText("Later note 11.")).toBeVisible();
    await turns.evaluate((list) => {
      list.setAttribute("data-e2e-before-click", "");
    });

    const row = turns.locator("[data-reply]").filter({ hasText: "The horizon mirror" });
    const time = row.getByRole("link").filter({ has: page.locator("time") });
    // Copy link and a new tab take the reply's own address.
    await expect(time).toHaveAttribute("href", replyPath);
    // Hovering still shows the full date.
    await expect(time.locator("time")).toHaveAttribute("title", /\d/);

    await time.click();
    await expect(page).toHaveURL(replyPath);
    await expect(row).toHaveAttribute("aria-current", "true");
    await expect(row).toBeInViewport();
    // Its thread's card is marked too, as a link to the reply marks it.
    await expect(turns.locator(':scope > li[aria-current="true"]')).toContainText(
      "Which mirror is out of true?"
    );
    // The same list: the click moved within the tab rather than reloading the issue page.
    await expect(turns).toHaveAttribute("data-e2e-before-click", "");
    await expect(page.getByText("Loading issue…")).toHaveCount(0);

    // Back returns to the Conversation the reader came from.
    await page.goBack();
    await expect(page).toHaveURL(`/issues/${issue.key}/conversation`);
  } finally {
    await context.close();
  }
});

// On a document the times of the threads beside it and of its decision blocks keep the reader on
// the document: each lands on the document's own address for that item, which is where the
// item's own address redirects, and the document is never opened again on the way.
test("a margin comment's time and a decision block's time keep the reader on the document", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const spec = [
    "# Instruments",
    ...Array.from({ length: 30 }, (_, index) => `Paragraph ${index}.`),
    "The astrolabe measures altitude.",
    ":::ask{#mirror}\nWhich mirror is out of true?\n\n- [ ] Horizon\n- [ ] Index\n:::",
  ].join("\n\n");
  const issue = await createIssue({ project: "CORE", spec, title: "Astrolabe calibration" });
  const comment = await createComment(issue.key, {
    anchor: { artifact: "spec", quote: "astrolabe" },
    body: "Calibrate it against the horizon.",
  });

  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.goto(`/issues/${issue.key}/spec`);
    const doc = page.locator(".dispatch-doc").first();
    await expect(doc.getByText("The astrolabe measures altitude.")).toBeVisible();

    const card = page.locator(`article[data-margin-item="${comment.id}"]`);
    await card.click();
    const commentTime = card.getByRole("link").filter({ has: page.locator("time") });
    await expect(commentTime).toHaveAttribute(
      "href",
      `/issues/${issue.key}/comments/${comment.id}`
    );
    await doc.evaluate((element) => {
      element.setAttribute("data-e2e-before-click", "");
    });
    await commentTime.click();
    await expect(page).toHaveURL(`/issues/${issue.key}/spec?comment=${comment.id}`);
    await expect(card).toHaveAttribute("aria-current", "true");
    await expect(doc).toHaveAttribute("data-e2e-before-click", "");

    const askTime = doc.getByRole("link").filter({ has: page.locator("time") });
    await expect(askTime).toHaveAttribute("href", new RegExp(`^/issues/${issue.key}/asks/`));
    await askTime.click();
    await expect(page).toHaveURL(`/issues/${issue.key}/spec#b-mirror`);
    await expect(doc).toHaveAttribute("data-e2e-before-click", "");
    await expect(page.getByText("Loading issue…")).toHaveCount(0);
  } finally {
    await context.close();
  }
});
