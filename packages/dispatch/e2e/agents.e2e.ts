import { expect, type Page, test } from "@playwright/test";

import { type FakeSession, getSentMessages, setLiveSessions } from "./agents";
import { createAsk, createIssue, createProject } from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

const planner: FakeSession = {
  capabilities: ["aside", "btw"],
  dir: "/workspaces/planner",
  last_seen: Date.now() - 30_000,
  machine_id: "planner-host",
  roles: ["planner"],
  session_id: "planner-session",
  title: "Planner",
};
const reviewer: FakeSession = {
  capabilities: ["aside"],
  dir: "/workspaces/reviewer",
  last_seen: Date.now() - 5 * 60_000,
  machine_id: "reviewer-host",
  roles: ["reviewer"],
  session_id: "reviewer-session",
  title: "Reviewer",
};

function targetedCard(page: Page, body: string) {
  return page.getByRole("list", { name: "Conversation turns" }).locator("li", { hasText: body });
}

test.beforeEach(async () => {
  await Promise.all([resetDatabase(), setLiveSessions([])]);
});

test("Agents shows live status and sends a targeted BTW from a selected issue", async ({
  browser,
}, testInfo) => {
  await setLiveSessions([planner, reviewer]);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Agent page message" });
  await Promise.all(
    ["First", "Second"].map((question) =>
      createAsk(
        issue.key,
        { question },
        { actor: { id: planner.session_id, kind: "session" }, as: "agent" }
      )
    )
  );

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/agents");

    const agents = page.getByRole("region", { name: "Agents" });
    if (testInfo.project.name === "chromium") {
      await expect(page.getByRole("link", { name: "Agents", exact: true })).toBeVisible();
    }
    await expect(agents.getByText("Planner", { exact: true })).toBeVisible();
    await expect(agents.getByText("Needs you 2", { exact: true })).toBeVisible();
    await expect(agents.getByText("Open asks 2", { exact: true })).toBeVisible();
    await expect(
      agents.getByRole("status", { name: "Seen less than 2 minutes ago" })
    ).toBeVisible();
    await expect(agents.getByRole("button", { name: "Steer Reviewer" })).toBeVisible();
    await expect(agents.getByRole("button", { name: "BTW Reviewer" })).toBeDisabled();

    if (testInfo.project.name === "chromium") {
      await page.screenshot({ path: "/tmp/agents-1280.png", fullPage: true });
    }
    if (testInfo.project.name === "iphone") {
      await page.screenshot({ path: "/tmp/agents-390.png", fullPage: true });
    }

    await agents.getByRole("button", { name: "BTW Planner" }).click();
    await page.getByRole("combobox", { name: "Issue" }).selectOption(issue.key);
    await expect(page.getByRole("button", { name: "BTW", exact: true })).toHaveAttribute(
      "aria-pressed",
      "true"
    );

    const body = "Please inspect the current implementation.";
    const sent = page.waitForResponse(
      (response) =>
        response.request().method() === "POST" &&
        response.url().endsWith(`/api/v1/issues/${issue.key}/messages`) &&
        response.status() === 201
    );
    await page.getByRole("textbox", { name: "Message" }).fill(body);
    await page.getByRole("textbox", { name: "Message" }).press("Control+Enter");
    const request = await sent;
    expect(request.request().postDataJSON()).toMatchObject({
      body,
      delivery: "btw",
      target: `session:${planner.session_id}`,
    });
    expect(await getSentMessages()).toMatchObject([{ target_session: planner.session_id }]);

    await page.goto(`/issues/${issue.key}/conversation`);
    await expect(targetedCard(page, body)).toContainText("Asking Planner (BTW)");
  } finally {
    await alice.close();
  }
});
