import { expect, test } from "@playwright/test";

import {
  createIssue,
  createIssueArtifact,
  createProject,
  editArtifact,
  getIssue,
  patchIssue,
  resolveAsk,
} from "./api";
import { documentEditor } from "./editor";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

test.beforeEach(async () => {
  await resetDatabase();
});

test("agentWrittenAskBlockAppearsAndAnswersInPlace", async ({ browser }, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "## Specification\n\nContext\n",
    title: "Ask block",
  });
  await patchIssue(issue.key, { labels: ["dispatch", "decision", "priority"] });
  await editArtifact(
    issue.primary_artifact_id,
    {
      ops: [
        {
          after: "end",
          markdown:
            ':::ask{#ship-decision urgency="high" multiple="false" state="open"}\nShould we ship?\n\n- Ship: Release it\n- Hold: Wait for review\n:::\n',
          op: "insert",
        },
      ],
    },
    { as: "agent" }
  );
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.setViewportSize({ width: 1770, height: 900 });
    await page.goto(`/issues/${issue.key}`);
    const ask = documentEditor(page).locator('[data-dispatch-ask-block="ship-decision"]');
    await expect(ask).toContainText("Should we ship?");
    const decisions = page.getByRole("navigation", { name: "Open decisions" });
    await expect(decisions.getByText("1 open decision", { exact: true })).toBeVisible();
    const decisionLink = decisions.getByRole("link", { name: "Should we ship?" });
    await expect(decisionLink).toHaveAttribute("title", "Should we ship?");
    await expect(decisions).toHaveCSS("height", "28px");
    const header = page.getByTestId("issue-header");
    const specification = documentEditor(page).getByRole("heading", { name: "Specification" });
    const desktopMetrics = await Promise.all([
      header.evaluate((element) => element.getBoundingClientRect().height),
      specification.evaluate((element) => element.getBoundingClientRect().y),
    ]);
    await testInfo.attach("specchrome-1770-metrics", {
      body: JSON.stringify({ headerHeight: desktopMetrics[0], specificationY: desktopMetrics[1] }),
      contentType: "application/json",
    });
    await page.screenshot({ path: "/tmp/specchrome-1770.png" });
    expect(desktopMetrics[0]).toBeLessThanOrEqual(96);

    await page.setViewportSize({ width: 1280, height: 800 });
    const tabletMetrics = await Promise.all([
      header.evaluate((element) => element.getBoundingClientRect().height),
      specification.evaluate((element) => element.getBoundingClientRect().y),
    ]);
    expect(tabletMetrics[0]).toBeLessThanOrEqual(140);
    await testInfo.attach("specchrome-1280-metrics", {
      body: JSON.stringify({ headerHeight: tabletMetrics[0], specificationY: tabletMetrics[1] }),
      contentType: "application/json",
    });

    await page.setViewportSize({ width: 390, height: 844 });
    await page.screenshot({ path: "/tmp/specchrome-390.png" });

    const form = ask.locator("form");
    await expect(form).toBeVisible({ timeout: 10_000 });
    const answer = form.locator('textarea[name="answer"]');
    await answer.fill("Yes.");
    await answer.press("Enter");
    await expect(answer).toHaveValue("Yes.\n");
    await answer.press("Control+Enter");
    await expect(ask).toContainText("Answered by alice");
    await expect(ask).toContainText("Yes.");
  } finally {
    await alice.close();
  }
});

test("a resolved decision block is read-only", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", spec: "Context\n", title: "Resolved ask" });
  await editArtifact(
    issue.primary_artifact_id,
    {
      ops: [
        {
          after: "end",
          markdown:
            ':::ask{#resolved-decision urgency="med" multiple="false" state="open"}\nShould we ship?\n\n- Ship\n- Hold\n:::\n',
          op: "insert",
        },
      ],
    },
    { as: "agent" }
  );
  await expect
    .poll(
      async () =>
        (await getIssue(issue.key)).open_asks.some(
          (candidate) => candidate.block_id === "resolved-decision"
        ),
      { timeout: 10_000 }
    )
    .toBe(true);
  const blockAsk = (await getIssue(issue.key)).open_asks.find(
    (candidate) => candidate.block_id === "resolved-decision"
  );
  if (blockAsk === undefined) {
    throw new Error("resolved decision ask was not created");
  }
  await resolveAsk(
    blockAsk.id,
    { kind: "resolved", reason: "The decision is no longer needed." },
    { as: "agent" }
  );
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}`);
    const ask = documentEditor(page).locator('[data-dispatch-ask-block="resolved-decision"]');
    await expect(ask.getByText("Resolved", { exact: true })).toBeVisible();
    await expect(ask.locator("form")).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

test("a failed in-document decision answer identifies the failed block", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", spec: "Context\n", title: "Failing ask" });
  await editArtifact(
    issue.primary_artifact_id,
    {
      ops: [
        {
          after: "end",
          markdown:
            ':::ask{#failing-decision urgency="med" multiple="false" state="open"}\nShould we ship?\n\n- Ship\n- Hold\n:::\n',
          op: "insert",
        },
      ],
    },
    { as: "agent" }
  );
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.route("**/api/v1/asks/*/answer", async (route) => {
      await route.fulfill({
        body: JSON.stringify({ error: "simulated failure" }),
        contentType: "application/json",
        status: 500,
      });
    });
    await page.goto(`/issues/${issue.key}`);
    await expect(page.getByRole("navigation", { name: "Open decisions" })).toBeVisible();
    const ask = documentEditor(page).locator('[data-dispatch-ask-block="failing-decision"]');
    const form = ask.locator("form");
    await expect(form).toBeVisible({ timeout: 10_000 });
    const error = page.getByRole("article", { name: "Document" }).getByRole("alert");
    await expect(error).toHaveCount(0);
    await form.locator('textarea[name="answer"]').fill("Ship after review.");
    await form.getByRole("button", { name: "Answer" }).click();
    await expect(error).toHaveAttribute("data-dispatch-ask-error", "failing-decision");
    await expect(error).toHaveText("Could not save your answer.");
  } finally {
    await alice.close();
  }
});

test("malformed decision with a blank option label disables the answer form", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", spec: "Context\n", title: "Malformed ask" });
  const malformed = await createIssueArtifact(
    issue.key,
    {
      content:
        ':::ask{#malformed-decision urgency="high" multiple="false" state="open"}\nWhich path?\n\n- Ship: Release it\n- : No label\n:::\n',
      name: "malformed.md",
    },
    { as: "agent" }
  );
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.setViewportSize({ width: 1280, height: 800 });
    await page.goto(`/issues/${issue.key}/artifacts/${malformed.artifact.slug}`);
    const ask = documentEditor(page).locator('[data-dispatch-ask-block="malformed-decision"]');
    await expect(ask.getByText("MALFORMED DECISION")).toBeVisible();
    await expect(
      ask.getByText(/ask block "malformed-decision" has an option without a label/)
    ).toBeVisible({ timeout: 10_000 });
    await expect(
      ask.getByText("Fix the block text; the decision re-activates once it parses.")
    ).toBeVisible();
    await expect(ask.locator("form")).toHaveCount(0);
    await expect(ask.locator("li:empty")).toHaveCount(0);
    await expect(ask.getByRole("listitem")).toHaveCount(2);
    await page.screenshot({ path: "/tmp/specchrome-malformed-1280.png" });
  } finally {
    await alice.close();
  }
});

test("open decision navigation cycles through the available decisions", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", spec: "Context\n", title: "Two decisions" });
  await editArtifact(
    issue.primary_artifact_id,
    {
      ops: [
        {
          after: "end",
          markdown:
            ':::ask{#first-decision urgency="med" multiple="false" state="open"}\nUse Postgres?\n\n- Yes\n:::\n\n:::ask{#second-decision urgency="med" multiple="false" state="open"}\nUse Redis?\n\n- Yes\n:::\n',
          op: "insert",
        },
      ],
    },
    { as: "agent" }
  );
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}`);
    const decisions = page.getByRole("navigation", { name: "Open decisions" });
    await expect(decisions.getByText("2 open decisions", { exact: true })).toBeVisible();
    const firstDecision = await decisions.getByRole("link").textContent();
    expect(firstDecision).not.toBeNull();
    await decisions.getByRole("button", { name: "Next open decision" }).click();
    await expect(decisions.getByRole("link")).not.toHaveText(firstDecision ?? "");
    await decisions.getByRole("button", { name: "Next open decision" }).click();
    await expect(decisions.getByRole("link")).toHaveText(firstDecision ?? "");
  } finally {
    await alice.close();
  }
});
