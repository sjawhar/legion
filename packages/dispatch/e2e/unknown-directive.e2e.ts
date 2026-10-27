import { expect, test } from "@playwright/test";

import { createAsk, createComment, createIssue, createMessage, createProject } from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

const agent = {
  actor: { id: "e2e-directive-agent", kind: "session" as const },
  as: "agent" as const,
};

/** A body whose `:::note` container directive no Dispatch block type claims. */
const body = "Some **bold** text\n\n:::note\nA note with **inner** words\n:::\n";

test.beforeEach(async () => {
  await resetDatabase();
});

test("Conversation renders comment, message and ask bodies holding an unknown container directive as formatted markdown with the fence lines as text", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Unknown directive" });
  await createComment(issue.key, { body }, agent);
  await createMessage(issue.key, { body }, agent);
  await createAsk(issue.key, { question: body }, agent);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);

    // Each card is found by its turn id, not by its formatted content: a body that fell back to
    // raw text renders `**bold**` and no `strong`, and these assertions then fail.
    for (const kind of ["comment", "message", "ask"]) {
      const card = page.locator(`[data-turn^="${kind}:"]`);
      await expect(card).toHaveCount(1);
      await expect(card.locator("strong").filter({ hasText: /^bold$/ })).toBeVisible();
      await expect(card.locator("strong").filter({ hasText: /^inner$/ })).toBeVisible();
      // A collapsed comment thread shows its body as a one-line preview, so the fences are
      // asserted as text rather than as paragraphs of their own.
      await expect(card).toContainText(":::note");
      await expect(card).toContainText(/words\s*:::/);
      await expect(card).not.toContainText("**");
      await expect(card.locator("[data-markdown-fallback]")).toHaveCount(0);
    }
  } finally {
    await alice.close();
  }
});
