import { expect, type Page, test } from "@playwright/test";

import { type FakeSession, setLiveSessions, setSessionSendStatus } from "./agents";
import { createIssue, createProject, listComments } from "./api";
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

async function post(page: Page): Promise<Record<string, unknown>> {
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

async function selectMention(page: Page, text: string, option = "planner"): Promise<void> {
  const field = page.getByRole("form", { name: "Comment composer" }).getByLabel("Comment");
  await field.fill(text);
  await page
    .getByRole("listbox", { name: "Mention suggestions" })
    .getByRole("option", { name: option })
    .click();
}

test.beforeEach(async () => {
  await Promise.all([resetDatabase(), setLiveSessions([])]);
});

test("/btw with a surviving mention strips its token and sends one comment-level mode", async ({
  browser,
}) => {
  await setLiveSessions([planner]);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "BTW comment" });
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    await selectMention(page, "/btw @");
    expect(await post(page)).toEqual({
      body: "@planner",
      delivery: "btw",
      mentions: [{ target: "session:s1" }],
    });
    await expect
      .poll(() => listComments(issue.key))
      .toMatchObject([{ body: "@planner", mentions: [{ target: "session:s1" }] }]);
  } finally {
    await alice.close();
  }
});

test("/btw without a surviving mention remains verbatim and never sends delivery", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Plain BTW comment" });
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    await page
      .getByRole("form", { name: "Comment composer" })
      .getByLabel("Comment")
      .fill("/btw hello");
    expect(await post(page)).toEqual({ body: "/btw hello" });
    await expect
      .poll(() => listComments(issue.key))
      .toMatchObject([{ body: "/btw hello", deliveries: [], mentions: [] }]);
  } finally {
    await alice.close();
  }
});

test("an unsupported selected mode warns but does not prevent Send", async ({ browser }) => {
  const offline: FakeSession = { ...planner, capabilities: [], session_id: "s2", title: "worker" };
  await setLiveSessions([offline]);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Unadvertised delivery" });
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    await selectMention(page, "/aside @", "worker");
    await expect(
      page.getByText("worker does not advertise Aside; Send will record the failed attempt.")
    ).toBeVisible();
    await setSessionSendStatus("s2", 404);
    expect(await post(page)).toEqual({
      body: "@worker",
      delivery: "aside",
      mentions: [{ target: "session:s2" }],
    });
  } finally {
    await alice.close();
  }
});
