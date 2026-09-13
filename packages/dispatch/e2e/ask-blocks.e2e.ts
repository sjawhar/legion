import { expect, test } from "@playwright/test";

import { createIssue, createIssueArtifact, createProject, editArtifact, patchIssue } from "./api";
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
    await form.locator('textarea[name="answer"]').fill("Yes.");
    await form.getByRole("button", { name: "Answer" }).click();
    await expect(ask).toContainText("Answered by alice");
    await expect(ask).toContainText("Yes.");
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
