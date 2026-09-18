import { expect, type Page, test } from "@playwright/test";

import { type FakeSession, getSentMessages, setLiveSessions, setSessionSendStatus } from "./agents";
import { createIssue, createProject, patchIssue } from "./api";
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

async function selectMention(page: Page, name: string): Promise<void> {
  const composer = page.getByRole("form", { name: "Comment composer" });
  await composer.getByLabel("Comment").fill("@");
  await composer.getByRole("option", { name }).click();
}

async function postPayload(page: Page): Promise<Record<string, unknown>> {
  const response = page.waitForResponse(
    (candidate) =>
      candidate.request().method() === "POST" &&
      /\/api\/v1\/issues\/[^/]+\/comments$/.test(candidate.url())
  );
  await page
    .getByRole("form", { name: "Comment composer" })
    .getByRole("button", { name: "Send" })
    .click();
  return (await response).request().postDataJSON() as Record<string, unknown>;
}

test.beforeEach(async () => {
  await Promise.all([resetDatabase(), setLiveSessions([])]);
});

test("the unified composer offers live roles and sessions and posts canonical mention targets", async ({
  browser,
}) => {
  await setLiveSessions([planner, worker]);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Mention the worker" });
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    await selectMention(page, "worker");
    const payload = await postPayload(page);
    expect(payload).toEqual({
      body: "@worker",
      delivery: "steer",
      mentions: [{ target: "session:B" }],
    });
  } finally {
    await alice.close();
  }
});

test("an issue route remains an owner route, not a recipient control", async ({ browser }) => {
  await setLiveSessions([planner]);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Plain comment" });
  await patchIssue(issue.key, { route: "role:legion-planner" });
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    const composer = page.getByRole("form", { name: "Comment composer" });
    await expect(
      composer.getByRole("button", { name: /Choose recipient|BTW|Aside|Steer/ })
    ).toHaveCount(0);
    await composer.getByLabel("Comment").fill("Stored as a plain comment");
    const payload = await postPayload(page);
    expect(payload).toEqual({ body: "Stored as a plain comment" });
  } finally {
    await alice.close();
  }
});

test("an offline selected mention is posted and exposes its failed delivery", async ({
  browser,
}) => {
  await setLiveSessions([worker]);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Offline mention" });
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    await selectMention(page, "worker");
    await setSessionSendStatus("B", 404);
    const payload = await postPayload(page);
    expect(payload.mentions).toEqual([{ target: "session:B" }]);
    await expect(page.getByRole("list", { name: "Mention deliveries" })).toContainText("failed");
    expect((await getSentMessages()).length).toBe(1);
  } finally {
    await alice.close();
  }
});
