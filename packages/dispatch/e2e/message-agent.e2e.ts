import { expect, type Page, test } from "@playwright/test";

import {
  type FakeSession,
  getSentMessages,
  setLiveSessions,
  setSessionLive,
  setSessionSendStatus,
} from "./agents";
import { createIssue, createMessage, createProject, patchIssue } from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

const planner: FakeSession = {
  capabilities: ["aside", "btw"],
  dir: "/w/legion",
  last_seen: 1_700_000_000_000,
  machine_id: "e2e",
  roles: ["legion-planner"],
  session_id: "A",
  title: "planner",
};
const worker: FakeSession = {
  capabilities: [],
  dir: "/w/worker",
  last_seen: 1_699_000_000_000,
  machine_id: "e2e",
  roles: [],
  session_id: "B",
  title: "worker",
};

function targetedCard(page: Page, question: string) {
  return page
    .getByRole("list", { name: "Conversation turns" })
    .locator("li", { hasText: question });
}

async function createIssueWithRoute(route?: string) {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Message a live agent" });
  if (route !== undefined) await patchIssue(issue.key, { route });
  return issue;
}

test.beforeEach(async () => {
  await Promise.all([resetDatabase(), setLiveSessions([])]);
});

test("the Message composer lists live roles and sessions, then sends the selected delivery", async ({
  browser,
}) => {
  await setLiveSessions([planner, worker]);
  const issue = await createIssueWithRoute();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    const recipient = page.getByRole("button", { name: "Choose recipient" });
    await recipient.click();
    const picker = page.getByRole("dialog", { name: "Recipient picker" });
    const optionLabels = await picker
      .locator("button[aria-pressed]")
      .evaluateAll((buttons) => buttons.map((button) => button.getAttribute("aria-label") ?? ""));
    expect(optionLabels).toHaveLength(3);
    expect(optionLabels[0]).toContain("legion-planner");
    expect(optionLabels[1]).toContain("planner");
    expect(optionLabels[2]).toContain("worker");
    await picker.getByRole("searchbox", { name: "Search recipients" }).fill("work");
    const workerOption = picker.getByRole("button", { name: /worker \/w\/worker/ });
    await expect(workerOption).toBeVisible();
    await workerOption.click();
    await expect(page.getByRole("button", { name: "BTW" })).toBeDisabled();
    await expect(page.getByRole("button", { name: "Aside" })).toBeDisabled();
    await expect(page.getByRole("button", { name: "Steer" })).toHaveAttribute(
      "aria-pressed",
      "true"
    );

    const question = "Please check the work";
    const sent = page.waitForResponse(
      (response) =>
        response.request().method() === "POST" &&
        response.url().endsWith(`/api/v1/issues/${issue.key}/messages`) &&
        response.status() === 201
    );
    await page.getByRole("textbox", { name: "Message" }).fill(question);
    await page.getByRole("textbox", { name: "Message" }).press("Enter");
    const request = await sent;
    expect(request.request().postDataJSON()).toMatchObject({
      body: question,
      delivery: "steer",
      target: "session:B",
    });
    await expect(targetedCard(page, question)).toContainText("Sent to worker (steer)");
    expect(await getSentMessages()).toMatchObject([{ target_session: "B" }]);
  } finally {
    await alice.close();
  }
});

test("a route preselects its role and displays a bearer reply from another session", async ({
  browser,
}) => {
  await setLiveSessions([planner, worker]);
  const issue = await createIssueWithRoute("role:legion-planner");
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    await expect(page.getByRole("button", { name: "Choose recipient" })).toHaveText(
      "To: legion-planner"
    );
    await expect(page.getByRole("button", { name: "BTW" })).toHaveAttribute("aria-pressed", "true");

    const question = "Can you coordinate?";
    const sent = page.waitForResponse(
      (response) =>
        response.request().method() === "POST" &&
        response.url().endsWith(`/api/v1/issues/${issue.key}/messages`) &&
        response.status() === 201
    );
    await page.getByRole("textbox", { name: "Message" }).fill(question);
    await page.getByRole("textbox", { name: "Message" }).press("Enter");
    const message = (await (await sent).json()) as { id: string };

    await createMessage(
      issue.key,
      { body: "I can take it.", in_reply_to: message.id },
      { actor: { id: "B", kind: "session" }, as: "agent" }
    );
    const card = targetedCard(page, question);
    await expect(card).toContainText("Answered by worker", { timeout: 1_000 });
    await expect(card).toContainText("I can take it.", { timeout: 1_000 });
  } finally {
    await alice.close();
  }
});

test("a listener rejection leaves a capability-aware retry path that succeeds after revival", async ({
  browser,
}) => {
  await setLiveSessions([planner, worker]);
  const issue = await createIssueWithRoute("session:B");
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    await expect(page.getByRole("button", { name: "Choose recipient" })).toHaveText("To: worker");
    await setSessionSendStatus("B", 404);

    const question = "Are you live?";
    const sent = page.waitForResponse(
      (response) =>
        response.request().method() === "POST" &&
        response.url().endsWith(`/api/v1/issues/${issue.key}/messages`) &&
        response.status() === 201
    );
    await page.getByRole("textbox", { name: "Message" }).fill(question);
    await page.getByRole("textbox", { name: "Message" }).press("Enter");
    await sent;
    const card = targetedCard(page, question);
    await expect(card).toContainText("Failed: no live session B");
    const retryBTW = card.getByRole("button", { name: "Ask BTW again" });
    await expect(retryBTW).toBeDisabled();
    await expect(retryBTW).toHaveAttribute("title", "worker does not advertise BTW");
    const sendNormally = card.getByRole("button", { name: "Send normally" });
    await expect(sendNormally).toBeEnabled();
    expect(await getSentMessages()).toHaveLength(1);

    await setSessionLive("B", false);
    await setSessionLive("B", true);
    await setSessionSendStatus("B", 200);
    const retry = page.waitForResponse(
      (response) =>
        response.request().method() === "POST" &&
        response.url().endsWith("/deliveries") &&
        response.status() === 201
    );
    await sendNormally.click();
    await retry;
    await expect(card).toContainText("Sent to worker (steer)");
    expect(await getSentMessages()).toHaveLength(2);
  } finally {
    await alice.close();
  }
});
