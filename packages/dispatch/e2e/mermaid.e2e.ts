// A fenced `mermaid` block in a body Dispatch renders in full - an issue comment, an ask's
// question on the Inbox, an issue message, a reply under its comment - draws as the diagram the
// document editor draws (an image named "Rendered Mermaid diagram"), with its source closed behind
// a `Mermaid source` disclosure. A source the renderer refuses shows the reason and the source; a
// diagram the worker cannot draw within the time limit is refused the same way while the page
// keeps responding; a label holding markup draws as text, never as an element; a block in another
// language stays code. Every drawing row checks both halves: the image with its labels, and the
// source hidden until the disclosure opens. At the commit before the change each body shows its
// source as code, so every drawing row and both refusal rows fail on their diagram or status
// assertion.
import { expect, type Locator, type Page, type TestInfo, test } from "@playwright/test";
import { createAsk, createComment, createIssue, createMessage, createProject } from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

const session = {
  actor: { id: "e2e-mermaid-agent", kind: "session" as const },
  as: "agent" as const,
};

const diagramName = "Rendered Mermaid diagram";

/** A body of `lead` and then one flowchart from `from` to `to`, whose source's first line is
 *  `graph TD`. */
function flowchart(lead: string, from: string, to: string): string {
  return `${lead}\n\n\`\`\`mermaid\ngraph TD\n  ${from} --> ${to}\n\`\`\`\n`;
}

function diagramIn(card: Locator): Locator {
  return card.getByRole("img", { exact: true, name: diagramName });
}

function conversationTurns(page: Page): Locator {
  return page.getByRole("list", { name: "Conversation turns" });
}

/** The comment thread `rootId` opened in the Conversation, where its bodies render in full rather
 *  than as the collapsed card's preview: the turn itself on a desktop, the full-screen Thread
 *  view on a phone. */
async function openCommentThread(page: Page, testInfo: TestInfo, rootId: string): Promise<Locator> {
  const turn = conversationTurns(page).locator(`:scope > li[data-turn="comment:${rootId}"]`);
  await turn.getByRole("button", { exact: true, name: "Expand thread" }).click();
  if (testInfo.project.name === "iphone") {
    const thread = page.getByRole("dialog", { name: "Thread" });
    await expect(thread).toBeVisible();
    return thread.locator(`li[data-turn="comment:${rootId}"]`);
  }
  await expect(turn.getByRole("button", { exact: true, name: "Collapse thread" })).toBeVisible();
  return turn;
}

/** Both halves of the control: `card` shows the drawn diagram holding `labels`, and its source
 *  (whose first line is `graph TD`) stays out of sight until `Mermaid source` is opened. */
async function expectDrawnDiagram(card: Locator, labels: readonly string[]): Promise<void> {
  const diagram = diagramIn(card);
  await expect(diagram).toBeVisible();
  for (const label of labels) {
    await expect(diagram).toContainText(label);
  }
  const source = card.getByText("graph TD");
  // Mounted, closed inside the disclosure: toBeHidden alone also passes on no match at all.
  await expect(source).toHaveCount(1);
  await expect(source).toBeHidden();
  await card.getByText("Mermaid source", { exact: true }).click();
  await expect(source).toBeVisible();
}

test.beforeEach(async () => {
  await resetDatabase();
});

test("a comment holding a mermaid block draws a diagram, its source behind Mermaid source", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Comment diagram" });
  const comment = await createComment(issue.key, { body: flowchart("Flow:", "Start", "Finish") });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    const card = await openCommentThread(page, testInfo, comment.id);
    await expect(card).toContainText("Flow:");
    await expectDrawnDiagram(card, ["Start", "Finish"]);
  } finally {
    await alice.close();
  }
});

test("an ask's question holding a mermaid block draws a diagram on the Inbox card", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Ask diagram" });
  const ask = await createAsk(
    issue.key,
    { question: flowchart("Does this order hold?", "Asked", "Answered") },
    session
  );

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    const card = page.getByTestId(`ask-${ask.id}`);
    await expect(card).toContainText("Does this order hold?");
    await expectDrawnDiagram(card, ["Asked", "Answered"]);
  } finally {
    await alice.close();
  }
});

test("an issue message holding a mermaid block draws a diagram in the Conversation", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Message diagram" });
  const message = await createMessage(
    issue.key,
    { body: flowchart("The pipeline:", "Sent", "Delivered") },
    session
  );

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    const card = conversationTurns(page).locator(`:scope > li[data-turn="message:${message.id}"]`);
    await expect(card).toContainText("The pipeline:");
    await expectDrawnDiagram(card, ["Sent", "Delivered"]);
  } finally {
    await alice.close();
  }
});

test("a reply to a comment holding a mermaid block draws a diagram under its comment", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Reply diagram" });
  const root = await createComment(issue.key, { body: "How does a reply travel?" });
  await createComment(
    issue.key,
    { body: flowchart("Like this:", "Replied", "Read"), reply_to: root.id },
    session
  );

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    const thread = await openCommentThread(page, testInfo, root.id);
    const reply = thread.getByRole("list", { name: "Replies" }).locator("li[data-reply]");
    await expect(reply).toHaveCount(1);
    await expect(reply).toContainText("Like this:");
    await expectDrawnDiagram(reply, ["Replied", "Read"]);
  } finally {
    await alice.close();
  }
});

test("a mermaid source the renderer refuses shows the reason and its source, and no diagram", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Refused diagram" });
  const comment = await createComment(issue.key, {
    body: "Broken:\n\n```mermaid\ngraph TD\n  A -->\n```\n",
  });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    const card = await openCommentThread(page, testInfo, comment.id);
    await expect(card).toContainText("Broken:");
    const refusal = card
      .getByRole("status")
      .filter({ hasText: "Unable to render Mermaid diagram" });
    await expect(refusal).toHaveText(/^Unable to render Mermaid diagram: Dangling Mermaid edge/);
    // The disclosure opens on a refusal, so the source is readable without a click. The status
    // quotes the dangling line too, so the source is read from its code block.
    await expect(card.locator("pre code").filter({ hasText: "A -->" })).toBeVisible();
    await expect(diagramIn(card)).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

// ELK's layout time grows faster than the diagram: a flowchart of 800 nodes with 2 edges each
// takes over a minute. Drawn on the page's own thread it held every reader of the comment there;
// in the worker the page keeps answering and the time limit refuses it.
test("a diagram too large to draw in time shows the reason, and the page keeps responding", async ({
  browser,
}, testInfo) => {
  const nodes = 800;
  const edges = Array.from(
    { length: nodes },
    (_, node) => `  n${node} --> n${(node + 1) % nodes}\n  n${node} --> n${(node + 7) % nodes}`
  );
  const body = `Large:\n\n\`\`\`mermaid\ngraph TD\n${edges.join("\n")}\n\`\`\`\n`;
  // Within Dispatch's 32,000-character body limit.
  expect(body.length).toBeLessThan(32_000);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Large diagram" });
  const comment = await createComment(issue.key, { body });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    const card = await openCommentThread(page, testInfo, comment.id);
    await expect(card).toContainText("Large:");
    const refusal = card
      .getByRole("status")
      .filter({ hasText: "Unable to render Mermaid diagram" });
    // While it is being drawn, a 100 ms timer on the page's thread fires on time, give or take
    // a scheduling hiccup; a layout on that thread would hold it for the whole layout.
    for (let sample = 0; sample < 5; sample += 1) {
      const elapsed = await page.evaluate(
        () =>
          new Promise<number>((resolve) => {
            const start = performance.now();
            setTimeout(() => resolve(performance.now() - start), 100);
          })
      );
      expect(elapsed).toBeLessThan(1_000);
    }
    // The samples ran while the diagram was still being drawn.
    await expect(refusal).toHaveCount(0);
    await expect(refusal).toHaveText(
      "Unable to render Mermaid diagram: it did not finish within 5 seconds"
    );
    await expect(card.locator("pre code").filter({ hasText: "n799 --> n0" })).toBeVisible();
    await expect(diagramIn(card)).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

test("a mermaid label holding markup draws as text, never as an element", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Markup label" });
  const comment = await createComment(issue.key, {
    body: 'Label:\n\n```mermaid\ngraph TD\n  A["<img src=x onerror=window.__mermaidHit=1>"] --> B\n```\n',
  });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    const card = await openCommentThread(page, testInfo, comment.id);
    await expect(card).toContainText("Label:");
    // The label's characters are the node's text. Only its head is asserted, since a long label
    // may wrap across text lines whose content joins without the space between them.
    await expectDrawnDiagram(card, ["img src=x"]);
    await expect(diagramIn(card).locator("img")).toHaveCount(0);
    expect(await page.evaluate(() => "__mermaidHit" in window)).toBe(false);
  } finally {
    await alice.close();
  }
});

test("a code block in another language stays code", async ({ browser }, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "TypeScript block" });
  const comment = await createComment(issue.key, {
    body: "Code:\n\n```ts\nconst answer = 42;\n```\n",
  });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    const card = await openCommentThread(page, testInfo, comment.id);
    await expect(card.getByText("const answer = 42;")).toBeVisible();
    await expect(diagramIn(card)).toHaveCount(0);
    await expect(card.getByText("Mermaid source", { exact: true })).toHaveCount(0);
  } finally {
    await alice.close();
  }
});
