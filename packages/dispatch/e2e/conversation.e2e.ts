import { expect, type Page, test } from "@playwright/test";
import { setInterests, setLiveSessions } from "./agents";
import {
  createAsk,
  createComment,
  createIssue,
  createMessage,
  createProject,
  editAsk,
  getAsk,
  getIssueEvents,
  patchIssue,
  putIssueState,
} from "./api";
import { resetDatabase, setEventCreatedAt } from "./seed";
import { asUser } from "./users";

const agent = {
  actor: { id: "e2e-conv-agent", kind: "session" as const },
  as: "agent" as const,
};
const bob = {
  actor: { id: "e2e-conv-bob", kind: "session" as const },
  as: "agent" as const,
};

function turn(page: Page, text: string) {
  return page
    .getByRole("list", { name: "Conversation turns" })
    .locator("li")
    .filter({ has: page.getByText(text, { exact: true }) });
}

test.beforeEach(async () => {
  await resetDatabase();
  if (!process.env.PLAYWRIGHT_BASE_URL) {
    await setLiveSessions([]);
  }
});

test("Conversation owns the route, groups chronological Markdown turns, and resolves live agent names", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Conversation route" });
  await createMessage(issue.key, { body: "First **bold** line" }, agent);
  await createMessage(issue.key, { body: "```ts\nconst x = 1;\n```" }, agent);
  await createMessage(issue.key, { body: "From bob" }, bob);
  await createMessage(issue.key, { body: "Another from bob" }, bob);
  await createMessage(
    issue.key,
    { body: "From a disconnected agent" },
    { actor: { id: "ghost-session-0000", kind: "session" }, as: "agent" }
  );

  const events = await getIssueEvents(issue.key, { limit: 20 });
  const first = events.find(
    (event) => event.type === "message.created" && event.payload.body === "First **bold** line"
  );
  const code = events.find(
    (event) => event.type === "message.created" && event.payload.body.includes("const x = 1")
  );
  if (first === undefined || code === undefined) {
    throw new Error("seeded conversation messages were not recorded as events");
  }
  await setEventCreatedAt(first.id, new Date(Date.now() - 20 * 60 * 1_000).toISOString());
  await setEventCreatedAt(code.id, new Date(Date.now() - 10 * 60 * 1_000).toISOString());
  if (!process.env.PLAYWRIGHT_BASE_URL) {
    await setLiveSessions([{ session_id: "e2e-conv-agent", title: "Planner (e2e)" }]);
    await setInterests([
      {
        session_id: "e2e-conv-agent",
        topics: [
          `notifications.dispatch.issue.${issue.key}`,
          `notifications.dispatch.issue.${issue.key}.>`,
        ],
      },
    ]);
  }

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    if (!process.env.PLAYWRIGHT_BASE_URL) {
      const subscribedAgents = page.getByRole("region", { name: "Subscribed agents" });
      await expect(subscribedAgents.getByText("Planner (e2e)", { exact: true })).toBeVisible();
      await expect(subscribedAgents.getByText("ghost-session-0000", { exact: true })).toHaveCount(
        0
      );
    }

    await expect(page.getByRole("tab", { name: "Conversation" })).toHaveAttribute(
      "aria-selected",
      "true"
    );
    await expect(page.getByRole("tablist", { name: "Issue detail" }).getByRole("tab")).toHaveText([
      "Spec",
      "Conversation",
      "Children",
      "Artifacts",
    ]);

    await page.goto(`/issues/${issue.key}/log`);
    await expect(page).toHaveURL(new RegExp(`/issues/${issue.key}/conversation$`));
    await expect(page.getByRole("tab", { name: "Conversation" })).toHaveAttribute(
      "aria-selected",
      "true"
    );

    await expect(turn(page, "First bold line")).toHaveAttribute("data-continued", "false");
    await expect(turn(page, "const x = 1;")).toHaveAttribute("data-continued", "false");
    await expect(turn(page, "From bob")).toHaveAttribute("data-continued", "true");
    await expect(turn(page, "Another from bob")).toHaveAttribute("data-continued", "false");
    await expect(turn(page, "First bold line").locator("strong")).toHaveText("bold");
    await expect(turn(page, "const x = 1;").locator("pre code")).toContainText("const x = 1;");
    if (!process.env.PLAYWRIGHT_BASE_URL) {
      await expect(turn(page, "First bold line")).toContainText("Planner (e2e)");
    }
    await expect(turn(page, "From a disconnected agent")).toContainText("session:ghost-se…");

    const screenshot = testInfo.outputPath("conversation-desktop.png");
    await page.screenshot({ path: screenshot, fullPage: true });
    await testInfo.attach("conversation desktop", { contentType: "image/png", path: screenshot });
  } finally {
    await alice.close();
  }
});

test("Conversation updates an open ask in place after an agent edit and preserves its prior question", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Edited decision" });
  const ask = await createAsk(
    issue.key,
    { options: [{ label: "REST" }, { label: "gRPC" }], question: "Which implementation?" },
    agent
  );

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    const conversation = page.getByRole("region", { name: "Conversation" });
    const card = conversation.getByTestId(`ask-${ask.id}`);
    await expect(card).toContainText("Which implementation?");

    await editAsk(
      ask.id,
      {
        options: [{ label: "HTTP" }, { label: "MCP" }],
        question: "Which transport should we implement?",
      },
      agent
    );

    await expect(card).toContainText("Which transport should we implement?");
    await expect(card).toContainText(/Edited /);
    await expect(page.locator(`[data-turn="ask:${ask.id}"]`)).toHaveCount(1);
    await expect(
      page.locator('[data-kind="activity"]', { hasText: 'edited the question "Which transport' })
    ).toBeVisible();
    await card.getByText("Show 1 previous version").click();
    await expect(card.getByText("Which implementation?")).toBeVisible();
    await expect(card.getByText(/Reworded by /)).toBeVisible();
    await expect(card.getByRole("list", { name: "Options" })).toContainText("REST");
    await expect(card.getByRole("list", { name: "Options" })).toContainText("gRPC");

    await page.goto("/");
    const inboxCard = page.getByTestId(`ask-${ask.id}`);
    await expect(page.locator("[data-testid^=ask-]")).toHaveCount(1);
    await expect(inboxCard).toContainText("Which transport should we implement?");
    await expect(inboxCard).toContainText(/Edited /);
    await page.screenshot({ path: testInfo.outputPath("ask-edited-in-place.png"), fullPage: true });
  } finally {
    await alice.close();
  }
});

test("a deep link to an old unanchored comment scrolls its Conversation turn into view", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Old comment target" });
  const comment = await createComment(issue.key, { body: "Deep linked comment" }, agent);
  for (let index = 0; index < 210; index += 1) {
    await createMessage(issue.key, { body: `Newer message ${index}` }, agent);
  }

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/comments/${comment.id}`);

    const target = page
      .getByRole("list", { name: "Conversation turns" })
      .locator('li[aria-current="true"]');
    await expect(target).toContainText("Deep linked comment");
    await expect(target).toBeInViewport();
  } finally {
    await alice.close();
  }
});

test("a message reply renders its quoted parent link and deep links scroll to the parent turn", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Message reply thread" });
  const root = await createMessage(issue.key, { body: "Ship the build tonight" }, agent);
  await createMessage(issue.key, { body: "Sounds good, thanks!", in_reply_to: root.id }, bob);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);

    const reply = turn(page, "Sounds good, thanks!");
    await expect(reply.getByRole("link", { name: "Ship the build tonight" })).toBeVisible();

    await page.goto(`/issues/${issue.key}/messages/${root.id}`);
    const target = page
      .getByRole("list", { name: "Conversation turns" })
      .locator('li[aria-current="true"]');
    await expect(target).toContainText("Ship the build tonight");
    await expect(target).toBeInViewport();
  } finally {
    await alice.close();
  }
});

test("Conversation coalesces answered asks and toggles activity without remounting turns", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "Review this specification.",
    title: "Activity",
  });
  await createMessage(issue.key, { body: "First turn" }, agent);
  const ask = await createAsk(
    issue.key,
    { options: [{ label: "Ship" }, { label: "Hold" }], question: "Ship it?" },
    agent
  );
  await patchIssue(issue.key, { title: "Renamed" });
  await createComment(
    issue.key,
    { anchor: { artifact: "spec", quote: "Review" }, body: "Review comment" },
    agent
  );
  const child = await createIssue({ parent: issue.key, project: "CORE", title: "Child activity" });
  await patchIssue(child.key, { status: "in_progress" });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);

    const firstTurn = turn(page, "First turn");
    await expect(firstTurn).toBeVisible();
    const handle = await firstTurn.elementHandle();
    if (handle === null) {
      throw new Error("the first conversation turn did not produce an element handle");
    }

    const conversation = page.getByRole("region", { name: "Conversation" });
    const card = conversation.getByTestId(`ask-${ask.id}`);
    await expect(card.getByRole("radio", { name: "Ship" })).toBeVisible();
    await expect(card.getByRole("radio", { name: "Hold" })).toBeVisible();
    await expect(card.getByRole("button", { name: "Answer" })).toBeVisible();
    await card.getByRole("radio", { name: "Ship" }).check();
    await card.getByRole("button", { name: "Answer" }).click();
    await expect(
      card.getByRole("list", { name: "Options" }).locator('li[data-selected="true"]')
    ).toContainText("Ship");
    await expect(card).toContainText(/Asked .* · Answered by alice/);
    await expect(card.locator("time")).toHaveCount(2);
    await expect(page.locator(`[data-turn="ask:${ask.id}"]`)).toHaveCount(1);

    await createComment(issue.key, { ask_id: ask.id, body: "Thanks" }, agent);
    await expect
      .poll(() => getAsk(ask.id))
      .toMatchObject({
        replies: [expect.objectContaining({ body: "Thanks" })],
      });

    await expect(
      page.locator('[data-kind="activity"]', { hasText: "updated the issue" })
    ).toBeVisible();
    await expect(
      page.locator('[data-kind="activity"]', { hasText: "commented on spec" })
    ).toBeVisible();
    await expect(
      page.locator('[data-kind="activity"]', { hasText: `moved ${child.key} from` })
    ).toBeVisible();

    await page.getByRole("checkbox", { name: "Show activity" }).uncheck();
    await expect(page.locator('[data-kind="activity"]')).toHaveCount(0);
    expect(await handle.evaluate((element) => element.isConnected)).toBe(true);
    await page.reload();
    await expect(page.getByRole("checkbox", { name: "Show activity" })).not.toBeChecked();
    const refreshedTurn = page
      .getByRole("region", { name: "Conversation" })
      .locator(`[data-turn="ask:${ask.id}"]`);
    await expect(refreshedTurn.getByRole("button", { name: "1 reply" })).toBeVisible();
    await refreshedTurn.getByRole("button", { name: "1 reply" }).click();
    await expect(refreshedTurn).toContainText("Thanks");

    const screenshot = testInfo.outputPath("conversation-ask-and-activity.png");
    await page.screenshot({ path: screenshot, fullPage: true });
    await testInfo.attach("conversation ask and activity", {
      contentType: "image/png",
      path: screenshot,
    });
  } finally {
    await alice.close();
  }
});

test("Conversation divides unread turns by day, sends with Enter, and jumps to newer turns", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Dividers and follow" });
  await createMessage(issue.key, { body: "First yesterday" }, agent);
  await createMessage(issue.key, { body: "Second yesterday" }, agent);
  await createMessage(issue.key, { body: "Third today" }, agent);
  await createMessage(issue.key, { body: "Fourth today" }, agent);
  for (let index = 0; index < 30; index += 1) {
    await createMessage(issue.key, { body: `Overflow turn ${index}` }, agent);
  }

  const events = await getIssueEvents(issue.key, { limit: 100 });
  const first = events.find(
    (event) => event.type === "message.created" && event.payload.body === "First yesterday"
  );
  const second = events.find(
    (event) => event.type === "message.created" && event.payload.body === "Second yesterday"
  );
  const created = events.find((event) => event.type === "issue.created");
  if (first === undefined || second === undefined || created === undefined) {
    throw new Error("seeded divider events were not recorded");
  }
  const yesterday = new Date(Date.now() - 24 * 60 * 60 * 1_000).toISOString();
  await Promise.all([
    setEventCreatedAt(created.id, yesterday),
    setEventCreatedAt(first.id, yesterday),
    setEventCreatedAt(second.id, yesterday),
  ]);
  await putIssueState(issue.key, { last_read_seq: second.seq });
  if (!process.env.PLAYWRIGHT_BASE_URL) {
    await setLiveSessions([
      { session_id: "e2e-conv-agent", title: "Planner (e2e)" },
      { session_id: "e2e-conv-reviewer", title: "Reviewer (e2e)" },
    ]);
  }

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);

    const dividers = page.getByRole("separator");
    await expect(dividers).toHaveCount(3);
    expect(
      await dividers.evaluateAll((items) => items.map((item) => item.getAttribute("aria-label")))
    ).toEqual(["Today", "Yesterday", "New since you last read"]);
    const turnTexts = await page
      .getByRole("list", { name: "Conversation turns" })
      .locator("li")
      .allTextContents();
    const unreadIndex = turnTexts.indexOf("New since you last read");
    const secondIndex = turnTexts.findIndex((text) => text.includes("Second yesterday"));
    const thirdIndex = turnTexts.findIndex((text) => text.includes("Third today"));
    expect(unreadIndex).toBeGreaterThan(thirdIndex);
    expect(unreadIndex).toBeLessThan(secondIndex);

    const recipient = page.getByRole("button", { name: "Choose recipient" });
    await expect(recipient).toHaveText("To: Choose recipient");
    if (!process.env.PLAYWRIGHT_BASE_URL) {
      await recipient.click();
      const picker = page.getByRole("dialog", { name: "Recipient picker" });
      await expect(picker.getByRole("button", { name: /Planner \(e2e\)/ })).toBeVisible();
      await expect(picker.getByRole("button", { name: /Reviewer \(e2e\)/ })).toBeVisible();
      await page.keyboard.press("Escape");
      await expect(picker).toHaveCount(0);
    }

    const message = page.getByRole("textbox", { name: "Message" });
    let sentRequests = 0;
    page.on("request", (request) => {
      if (
        request.method() === "POST" &&
        request.url().endsWith(`/api/v1/issues/${issue.key}/messages`)
      ) {
        sentRequests += 1;
      }
    });
    await message.fill("Hello from alice");
    await message.press("Shift+Enter");
    await expect(message).toHaveValue("Hello from alice\n");
    expect(sentRequests).toBe(0);
    const request = page.waitForRequest(
      (candidate) =>
        candidate.method() === "POST" &&
        candidate.url().endsWith(`/api/v1/issues/${issue.key}/messages`)
    );
    await message.press("Enter");
    await request;
    const sentTurn = turn(page, "Hello from alice");
    await expect(sentTurn).toBeVisible();
    const sentBox = await sentTurn.boundingBox();
    expect(sentBox).not.toBeNull();
    expect((sentBox?.y ?? 0) + (sentBox?.height ?? 0)).toBeLessThanOrEqual(
      await page.evaluate(() => window.innerHeight)
    );

    await page.evaluate(() => window.scrollTo(0, document.documentElement.scrollHeight));
    const firstTurn = turn(page, "First yesterday");
    const before = (await firstTurn.boundingBox())?.y;
    await createMessage(issue.key, { body: "Tail arrives" }, bob);
    const jump = page.getByTestId("jump-to-latest");
    await expect(jump).toHaveText("Jump to latest · 1 new");
    await createMessage(issue.key, { body: "Another tail arrives" }, bob);
    await expect(jump).toHaveText("Jump to latest · 2 new");
    await expect.poll(async () => (await firstTurn.boundingBox())?.y).toBe(before);
    await jump.click();
    const tail = turn(page, "Tail arrives");
    await expect(tail).toBeVisible();
    const tailBox = await tail.boundingBox();
    expect(tailBox).not.toBeNull();
    expect((tailBox?.y ?? 0) + (tailBox?.height ?? 0)).toBeLessThanOrEqual(
      await page.evaluate(() => window.innerHeight)
    );
    await expect(jump).toHaveCount(0);

    const screenshot = testInfo.outputPath("conversation-follow-latest.png");
    await page.screenshot({ path: screenshot, fullPage: true });
    await testInfo.attach("conversation follow latest", {
      contentType: "image/png",
      path: screenshot,
    });
  } finally {
    await alice.close();
  }
});

test("the iPhone Conversation fits controls and keeps the composer above the review panel", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "iphone", "phone geometry is covered by the iphone project");
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Phone conversation" });
  for (let index = 0; index < 10; index += 1) {
    await createMessage(issue.key, { body: `Phone turn ${index}` }, agent);
  }

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    await expect(turn(page, "Phone turn 9")).toBeVisible();
    expect(
      await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)
    ).toBe(true);

    const controls = page.locator(
      '[aria-label="Conversation"] button, [aria-label="Conversation"] textarea, [aria-label="Conversation"] input'
    );
    for (let index = 0; index < (await controls.count()); index += 1) {
      const control = controls.nth(index);
      const opacity = await control.evaluate((element) =>
        Number(getComputedStyle(element).opacity)
      );
      if (opacity === 0) {
        continue;
      }
      const box = await control.boundingBox();
      expect(box, `Conversation control ${index} has a layout box`).not.toBeNull();
      expect(
        box?.height ?? 0,
        `Conversation control ${index} is a touch target`
      ).toBeGreaterThanOrEqual(44);
    }
    const lastTurn = turn(page, "Phone turn 9");
    await lastTurn.hover();
    const pinBox = await lastTurn.getByRole("button", { name: "Pin" }).boundingBox();
    expect(pinBox).not.toBeNull();
    expect(pinBox?.height ?? 0).toBeGreaterThanOrEqual(44);

    const message = page.getByRole("textbox", { name: "Message" });
    await message.focus();
    const composer = page.getByRole("form", { name: "Message composer" });
    const reviewPanelButton = page.getByRole("button", { name: /Open review panel/ });
    const positions = [0, await page.evaluate(() => document.documentElement.scrollHeight)];
    for (const position of positions) {
      await page.evaluate((top) => window.scrollTo(0, top), position);
      const composerBox = await composer.boundingBox();
      const reviewBox = await reviewPanelButton.boundingBox();
      const viewportHeight = await page.evaluate(() => window.innerHeight);
      expect(composerBox).not.toBeNull();
      expect(composerBox?.y ?? -1).toBeGreaterThanOrEqual(0);
      expect((composerBox?.y ?? 0) + (composerBox?.height ?? 0)).toBeLessThanOrEqual(
        viewportHeight
      );
      expect(reviewBox).not.toBeNull();
      expect((composerBox?.y ?? 0) + (composerBox?.height ?? 0)).toBeLessThanOrEqual(
        reviewBox?.y ?? 0
      );
    }

    const screenshot = testInfo.outputPath("conversation-phone.png");
    await page.screenshot({ path: screenshot, fullPage: true });
    await testInfo.attach("conversation phone", { contentType: "image/png", path: screenshot });
  } finally {
    await alice.close();
  }
});
