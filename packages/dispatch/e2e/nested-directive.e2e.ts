import { type Browser, expect, test } from "@playwright/test";

import { createComment, createIssue, createIssueArtifact, createProject } from "./api";
import { expectSettled } from "./heavy-bodies-page";
import { expectUnderTheBound, type Measured, measureDraw, record } from "./main-thread-hold";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

const agent = {
  actor: { id: "e2e-nested-directive-agent", kind: "session" as const },
  as: "agent" as const,
};

/** The two bodies that froze a reader's tab for about a minute and 15 s (INFRA-581). */
const inputs = [
  {
    name: "2,000 nested unclosed openers then a 21,000-character line",
    body: ":::a\n".repeat(2000) + "x".repeat(21000),
  },
  {
    name: "1,000 nested openers then 1,000 closing lines",
    body: ":::a\n".repeat(1000) + ":::\n".repeat(1000),
  },
];

test.beforeEach(async () => {
  await resetDatabase();
});

/**
 * Opens the issue's Conversation tab as alice and measures what the page's own work holds the
 * main thread for while the comment card draws, in the milliseconds of a main thread running at
 * its own best (`main-thread-hold.ts`). The thread is left as a reader finds it, collapsed, so
 * what the card draws is its two-line preview of the body (`ThreadCard`); the body is parsed
 * whole for that preview, which is where the freeze was. The record starts again when the page
 * first asks for the issue's comments, before that request goes out: the bundle has been
 * evaluated and the tab drawn by then, and the card cannot draw before the answer. The
 * measurement ends where the card has drawn a `:::a` line and owes no formatting: nothing on it
 * is still pending, and the parser refused nothing.
 *
 * What the rows answer with is the parse's own cost: on an issue pod of two cores the hold is
 * about 50 ms with the parse in the worker, and 238 and 434 ms with the worker's threshold
 * raised past these bodies, so that the page parses them itself - against the 1,000 ms bound,
 * and against the minute and a quarter the tab was held for before the parser was fixed.
 */
async function holdUntilTheCommentDraws(browser: Browser, issueKey: string): Promise<Measured> {
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const card = page.locator('[data-turn^="comment:"]');
    return await measureDraw(page, {
      drawn: async () => {
        await expect(card.getByText(":::a").first()).toBeVisible();
        await expectSettled(card);
      },
      open: async () => {
        await page.goto(`/issues/${issueKey}/conversation`);
        await expect(card).toHaveCount(1);
      },
      reset: new RegExp(`/api/v1/issues/${issueKey}/comments(\\?.*)?$`),
    });
  } finally {
    await alice.close();
  }
}

for (const input of inputs) {
  test(`Conversation draws a comment of ${input.name} without holding the main thread for a second`, async ({
    browser,
  }) => {
    await createProject({ key: "CORE", name: "Core" });
    const issue = await createIssue({ project: "CORE", title: "Nested directives" });
    await createComment(issue.key, { body: input.body }, agent);

    const measured = await holdUntilTheCommentDraws(browser, issue.key);
    record(measured);
    expectUnderTheBound(measured);
  });
}

test("a document write of either nested-directive body is refused as invalid markdown in under a second", async () => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Nested directive documents" });

  for (const input of inputs) {
    const started = performance.now();
    const refusal = await createIssueArtifact(
      issue.key,
      { content: input.body, name: "nested.md" },
      agent
    ).then(
      () => undefined,
      (error: unknown) => error
    );
    const took = performance.now() - started;

    expect(refusal, `${input.name}: the write was accepted`).toBeInstanceOf(Error);
    const message = refusal instanceof Error ? refusal.message : "";
    expect(message, input.name).toContain(" failed: 400 ");
    expect(message, input.name).toContain('"code":"INVALID_MARKDOWN"');
    expect(took, `${input.name}: the refusal took ${Math.round(took)} ms`).toBeLessThan(1_000);
  }
});
