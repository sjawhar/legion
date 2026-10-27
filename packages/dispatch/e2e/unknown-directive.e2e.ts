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

test("Conversation keeps every line of a typed block whose content breaks its rule, drawn as text", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Broken typed block" });
  // An ask takes `paragraph+ bullet_list?`: the drawn :::note's lines after the list break it,
  // and so does a heading, and the browser editor's parser drops such a block with its text.
  const cards = {
    message: {
      body: "Lead **bold**\n\n::::ask\nWhich option?\n\n- first\n\n:::note\nA note line\n:::\n::::\n",
      lines: ["::::ask", "Which option?", "first", ":::note", "A note line"],
    },
    ask: {
      body: "Lead **bold**\n\n:::ask{#a1}\n# A heading\n\nBody text\n:::\n",
      lines: [":::ask{#a1}", "A heading", "Body text"],
    },
  };
  await createMessage(issue.key, { body: cards.message.body }, agent);
  await createAsk(issue.key, { question: cards.ask.body }, agent);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);

    for (const [kind, { lines }] of Object.entries(cards)) {
      const card = page.locator(`[data-turn^="${kind}:"]`);
      await expect(card).toHaveCount(1);
      await expect(card.locator("strong").filter({ hasText: /^bold$/ })).toBeVisible();
      for (const line of lines) await expect(card).toContainText(line);
      await expect(card.locator("[data-markdown-fallback]")).toHaveCount(0);
    }
  } finally {
    await alice.close();
  }
});

test("Conversation draws a message's lone-CR unknown block once, each line on its own", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Lone CR" });
  // A message body is stored as sent, so a lone carriage return still ends its lines: the drawn
  // fence lines must end there too, or the body is shown twice.
  await createMessage(
    issue.key,
    { body: "Lead **bold**\n\n:::a\rbody words\r:::\n\nAfter" },
    agent
  );

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);

    const card = page.locator('[data-turn^="message:"]');
    await expect(card).toHaveCount(1);
    await expect(card.locator("strong").filter({ hasText: /^bold$/ })).toBeVisible();
    for (const line of [":::a", "body words", ":::", "After"]) {
      await expect(card.locator("p").filter({ hasText: new RegExp(`^${line}$`) })).toHaveCount(1);
    }
    await expect(card.locator("[data-markdown-fallback]")).toHaveCount(0);
  } finally {
    await alice.close();
  }
});
