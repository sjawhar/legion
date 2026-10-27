import { expect, test } from "@playwright/test";

import {
  answerAsk,
  claimIssue,
  createAsk,
  createComment,
  createIssue,
  createIssueArtifact,
  createMessage,
  createProject,
  getIssueEvents,
  patchIssue,
  putIssueState,
  resolveAsk,
  resolveComment,
} from "./api";
import { resetDatabase, setEventCreatedAt } from "./seed";
import { asUser } from "./users";

const agent = {
  actor: { id: "e2e-activity-agent", kind: "session" as const },
  as: "agent" as const,
};

test.beforeEach(async () => {
  await resetDatabase();
});

// The other half of `e2e/conversation.e2e.ts`'s "one card per conversation and no event row":
// the rows the Conversation leaves out are all here, on an issue seeded exactly the same way.
test("the Activity tab lists the event rows the Conversation does not draw", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "Review this specification.",
    title: "One card per conversation",
  });
  const answered = await createAsk(
    issue.key,
    { options: [{ label: "Ship" }], question: "Ship it?" },
    agent
  );
  await answerAsk(answered.id, { expected_edited_at: null, selected: ["Ship"] }, { login: "bob" });
  const retracted = await createAsk(issue.key, { question: "Withdrawn question?" }, agent);
  await resolveAsk(retracted.id, { kind: "retracted", reason: "Superseded." }, agent);
  const resolved = await createComment(issue.key, { body: "A resolved thread." }, agent);
  await resolveComment(resolved.id);
  await createComment(issue.key, { body: "An open thread." }, agent);
  await createMessage(issue.key, { body: "A message thread." }, agent);
  await patchIssue(issue.key, { status: "in_progress" });
  await claimIssue(issue.key, agent);
  await createIssueArtifact(issue.key, { content: "# Note\n", name: "note.md" });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/activity`);
    await expect(page.getByRole("tab", { name: "Activity" })).toHaveAttribute(
      "aria-selected",
      "true"
    );
    const rows = page.getByRole("list", { name: "Issue activity" }).getByRole("listitem");
    // The three kinds the Conversation never draws, each a line of its own.
    for (const description of ["updated the issue", "claimed the issue", "added note.md"]) {
      await expect(rows.filter({ hasText: description })).toHaveCount(1);
    }
    expect(await rows.count()).toBeGreaterThanOrEqual(3);
    // The Conversation's list is not here, and this list carries no conversation card.
    await expect(page.getByRole("list", { name: "Conversation turns" })).toHaveCount(0);
    await expect(page.locator("[data-turn]")).toHaveCount(0);
    await page.screenshot({
      fullPage: true,
      path: testInfo.outputPath(`activity-rows-${testInfo.project.name}.png`),
    });
  } finally {
    await alice.close();
  }
});

test("Activity breaks its rows by day and marks where the reader stopped", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Dividers" });
  await createMessage(issue.key, { body: "First yesterday" }, agent);
  await createMessage(issue.key, { body: "Second yesterday" }, agent);
  await createMessage(issue.key, { body: "Third today" }, agent);
  await createMessage(issue.key, { body: "Fourth today" }, agent);

  const events = await getIssueEvents(issue.key, { limit: 100 });
  const first = events.find(
    (event) => event.type === "message.created" && event.payload.body === "First yesterday"
  );
  const second = events.find(
    (event) => event.type === "message.created" && event.payload.body === "Second yesterday"
  );
  const third = events.find(
    (event) => event.type === "message.created" && event.payload.body === "Third today"
  );
  const created = events.find((event) => event.type === "issue.created");
  if (first === undefined || second === undefined || third === undefined || created === undefined) {
    throw new Error("seeded divider events were not recorded");
  }
  const yesterday = new Date(Date.now() - 24 * 60 * 60 * 1_000).toISOString();
  await Promise.all([
    setEventCreatedAt(created.id, yesterday),
    setEventCreatedAt(first.id, yesterday),
    setEventCreatedAt(second.id, yesterday),
  ]);
  await putIssueState(issue.key, { last_read_seq: second.seq });
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    // The Activity tab reads the watermark; it never writes it. Only the Conversation does.
    let stateWrites = 0;
    page.on("request", (request) => {
      if (
        request.method() === "PUT" &&
        new URL(request.url()).pathname === `/api/v1/me/issues/${issue.key}/state`
      ) {
        stateWrites += 1;
      }
    });
    await page.goto(`/issues/${issue.key}/activity`);

    const list = page.getByRole("list", { name: "Issue activity" });
    const dividers = list.getByRole("separator");
    await expect(dividers).toHaveCount(3);
    expect(
      await dividers.evaluateAll((items) => items.map((item) => item.getAttribute("aria-label")))
    ).toEqual(["Today", "Yesterday", "New since you last read"]);

    // A row is an event line: it reads the event's description ("sent a message"), never the
    // message's own body, so the rows the dividers fall between are found by the
    // `data-event-seq` each row carries.
    const rows = await list
      .locator("li")
      .evaluateAll((items) =>
        items.map((item) => item.getAttribute("data-event-seq") ?? item.getAttribute("aria-label"))
      );
    const unreadIndex = rows.indexOf("New since you last read");
    const secondIndex = rows.indexOf(String(second.seq));
    const thirdIndex = rows.indexOf(String(third.seq));
    expect(secondIndex).toBeGreaterThanOrEqual(0);
    expect(thirdIndex).toBeGreaterThanOrEqual(0);
    // Newest first: the divider sits below the oldest unread row and above the newest row the
    // reader had already seen.
    expect(unreadIndex).toBeGreaterThan(thirdIndex);
    expect(unreadIndex).toBeLessThan(secondIndex);

    // Nothing the tab did wrote the reader's watermark.
    await page.waitForTimeout(2_000);
    expect(stateWrites).toBe(0);
  } finally {
    await alice.close();
  }
});

test("an Activity row pins into the review margin and unpins from it @phone", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Pin an event" });
  await patchIssue(issue.key, { status: "in_progress" });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/activity`);
    const row = page
      .getByRole("list", { name: "Issue activity" })
      .getByRole("listitem")
      .filter({ hasText: "updated the issue" });
    await expect(row).toHaveCount(1);
    await row.getByRole("button", { name: "Pin" }).click();
    await expect(row.getByRole("button", { name: "Unpin" })).toHaveAttribute(
      "aria-pressed",
      "true"
    );

    if (testInfo.project.name === "iphone") {
      await page.getByRole("button", { name: /Open review panel/ }).click();
    }
    const margin = page.getByTestId("margin-sheet");
    await margin.getByRole("tab", { name: "Pinned" }).click();
    await expect(margin.getByText("Issue updated")).toBeVisible();
    await margin.getByRole("button", { name: "Unpin" }).click();
    await expect(margin.getByText("No pinned items.")).toBeVisible();
    if (testInfo.project.name === "iphone") {
      await page.getByRole("button", { name: /Close review panel/ }).click();
    }
    await expect(row.getByRole("button", { name: "Pin" })).toHaveAttribute("aria-pressed", "false");
  } finally {
    await alice.close();
  }
});
