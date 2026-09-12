import { expect, type Page, test } from "@playwright/test";

import {
  type FakeSession,
  getSentMessages,
  setLiveSessions,
  setSessionLive,
  setSessionSendStatus,
} from "./agents";
import { createIssue, createProject, patchIssue, replyToMessageDelivery } from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

const planner: FakeSession = {
  capabilities: ["aside", "btw"],
  dir: "/w/legion",
  last_seen: 1_700_000_000_000,
  machine_id: "e2e",
  roles: [],
  session_id: "s1",
  title: "planner",
};

function targetedCard(page: Page, question: string) {
  return page
    .getByRole("list", { name: "Conversation turns" })
    .locator("li", { hasText: question });
}

async function createRoutedIssue(route: string) {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Ask the planner" });
  await patchIssue(issue.key, { route });
  return issue;
}

test.beforeEach(async () => {
  await Promise.all([resetDatabase(), setLiveSessions([])]);
});

test("BTW sends an ephemeral question and shows its answer live in both readers", async ({
  browser,
}) => {
  await setLiveSessions([planner]);
  const issue = await createRoutedIssue("session:s1");
  const alice = await asUser(browser, "alice");
  const bob = await asUser(browser, "bob");
  try {
    const alicePage = await alice.newPage();
    const bobPage = await bob.newPage();
    await Promise.all([
      alicePage.goto(`/issues/${issue.key}/conversation`),
      bobPage.goto(`/issues/${issue.key}/conversation`),
    ]);
    const recipient = alicePage.getByRole("button", { name: "Choose recipient" });
    await expect(recipient).toHaveText("To: planner");
    await expect(alicePage.getByRole("button", { name: "BTW" })).toHaveAttribute(
      "aria-pressed",
      "true"
    );

    const question = "Can this ship?";
    const created = alicePage.waitForResponse(
      (response) =>
        response.request().method() === "POST" &&
        response.url().endsWith(`/api/v1/issues/${issue.key}/messages`) &&
        response.status() === 201
    );
    await alicePage.getByRole("textbox", { name: "Message" }).fill(question);
    await alicePage.getByRole("textbox", { name: "Message" }).press("Enter");
    const message = (await (await created).json()) as { id: string };
    const card = targetedCard(alicePage, question);
    await expect(card).toContainText("Asking planner (BTW) ·");
    expect(await getSentMessages()).toMatchObject([
      {
        expects_reply: "required",
        idempotency_key: expect.stringMatching(/:1$/),
        source: "dispatch",
        target_session: "s1",
      },
    ]);

    await replyToMessageDelivery(
      message.id,
      { attempt: 1, body: "Ship it." },
      {
        id: "s1",
        kind: "session",
      }
    );
    for (const page of [alicePage, bobPage]) {
      const readerCard = targetedCard(page, question);
      await expect(readerCard).toContainText("Answered by planner", { timeout: 1_000 });
      await expect(readerCard).toContainText("Ship it.", { timeout: 1_000 });
    }
  } finally {
    await Promise.all([alice.close(), bob.close()]);
  }
});

test("a failed BTW attempt remains retryable until a reply lands", async ({ browser }) => {
  await setLiveSessions([planner]);
  const issue = await createRoutedIssue("session:s1");
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    await expect(page.getByRole("button", { name: "Choose recipient" })).toHaveText("To: planner");
    await setSessionSendStatus("s1", 404);

    const question = "Try BTW again";
    const initial = page.waitForResponse(
      (response) =>
        response.request().method() === "POST" &&
        response.url().endsWith(`/api/v1/issues/${issue.key}/messages`) &&
        response.status() === 201
    );
    await page.getByRole("textbox", { name: "Message" }).fill(question);
    await page.getByRole("textbox", { name: "Message" }).press("Enter");
    const message = (await (await initial).json()) as { id: string };
    const card = targetedCard(page, question);
    await expect(card).toContainText("Failed: no live session s1");
    const retryBTW = card.getByRole("button", { name: "Ask BTW again" });
    const sendNormally = card.getByRole("button", { name: "Send normally" });
    await expect(retryBTW).toBeEnabled();
    await expect(sendNormally).toBeEnabled();

    for (let attempt = 2; attempt <= 6; attempt += 1) {
      const delivered = page.waitForResponse(
        (response) =>
          response.request().method() === "POST" &&
          response.url().endsWith(`/api/v1/messages/${message.id}/deliveries`) &&
          response.status() === 201
      );
      await retryBTW.click();
      await delivered;
      await expect(card).toContainText(`Attempt ${attempt - 1}: Failed: no live session s1`);
    }
    expect(await getSentMessages()).toHaveLength(6);
    await expect(retryBTW).toBeEnabled();
    await expect(sendNormally).toBeEnabled();

    await setSessionLive("s1", false);
    await setSessionLive("s1", true);
    await setSessionSendStatus("s1", 200);
    const sent = page.waitForResponse(
      (response) =>
        response.request().method() === "POST" &&
        response.url().endsWith(`/api/v1/messages/${message.id}/deliveries`) &&
        response.status() === 201
    );
    await sendNormally.click();
    await sent;
    await expect(card).toContainText("Sent to planner (steer)");
    await replyToMessageDelivery(
      message.id,
      { attempt: 7, body: "Recovered." },
      {
        id: "s1",
        kind: "session",
      }
    );
    await expect(card).toContainText("Answered by planner");
    await expect(card.getByRole("button", { name: "Ask BTW again" })).toHaveCount(0);
    await expect(card.getByRole("button", { name: "Send normally" })).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

test("capabilities constrain BTW and Aside while leaving Steer selected", async ({ browser }) => {
  const worker: FakeSession = {
    ...planner,
    capabilities: ["aside"],
    session_id: "worker",
    title: "worker",
  };
  await setLiveSessions([worker]);
  const issue = await createRoutedIssue("session:worker");
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    await expect(page.getByRole("button", { name: "Choose recipient" })).toHaveText("To: worker");
    const btw = page.getByRole("button", { name: "BTW" });
    await expect(btw).toBeDisabled();
    await expect(btw).toHaveAttribute("title", "worker does not advertise BTW");
    await expect(page.getByRole("button", { name: "Aside" })).toBeEnabled();
    await expect(page.getByRole("button", { name: "Steer" })).toHaveAttribute(
      "aria-pressed",
      "true"
    );

    await setLiveSessions([{ ...worker, capabilities: [] }]);
    await page.reload();
    await expect(page.getByRole("button", { name: "Choose recipient" })).toHaveText("To: worker");
    await expect(page.getByRole("button", { name: "Aside" })).toBeDisabled();
  } finally {
    await alice.close();
  }
});
