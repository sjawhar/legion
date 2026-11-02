import { expect, type Page, test } from "@playwright/test";

import type { IssueDetails } from "../web/src/api/types";
import {
  createIssue,
  createProject,
  getIssue,
  getIssueArtifactText,
  putReportSettings,
} from "./api";
import { documentEditor } from "./editor";
import { resetDatabase } from "./seed";
import { pastePicture } from "./sends";
import { asUser } from "./users";

// Report a problem, from any page: the `?` panel's button and the Ctrl+K palette open it; the
// person's words and a pasted picture become an issue in the project Settings names, with the page
// it came from, and ticking Ask an agent to fix this routes the issue to the agent's role.

test.beforeEach(async () => {
  await resetDatabase();
});

function reportDialog(page: Page) {
  return page.getByRole("dialog", { name: "Report a problem" });
}

test("a report with a picture, sent from the ? panel, files an issue routed to the agent", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  await createProject({ key: "DSP", name: "Dispatch reports" });
  await putReportSettings({ agent_route: "role:dispatch-fixer", project: "DSP" });
  const issue = await createIssue({ project: "CORE", title: "Somewhere a problem shows" });
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}`);
    await expect(
      page.getByTestId("issue-header").getByRole("heading", { level: 1, name: issue.title })
    ).toBeVisible();
    await page.locator("body").focus();
    await page.keyboard.press("?");
    await page
      .getByRole("dialog", { name: "Keyboard shortcuts" })
      .getByRole("button", { name: "Report a problem" })
      .click();

    const dialog = reportDialog(page);
    const field = dialog.getByLabel("What went wrong");
    await field.fill("The Send button overlaps the box");
    await pastePicture(field, "shot.png");
    await expect(dialog.getByRole("list", { name: "Pictures" })).toContainText("shot.png");
    await dialog.getByLabel("Ask an agent to fix this").check();

    const sent = page.waitForResponse(
      (response) =>
        response.request().method() === "POST" &&
        new URL(response.url()).pathname === "/api/v1/reports"
    );
    await dialog.getByRole("button", { name: "Send report" }).click();
    const answer = await sent;
    expect(answer.status()).toBe(201);
    const filed = (await answer.json()) as IssueDetails;
    expect(filed.key).toMatch(/^DSP-\d+$/);
    await expect(dialog.getByRole("link", { name: filed.key })).toHaveAttribute(
      "href",
      `/issues/${filed.key}`
    );
    await expect(dialog).toContainText("No agent holds role:dispatch-fixer right now");

    const stored = await getIssue(filed.key);
    expect(stored.route).toBe("role:dispatch-fixer");
    expect(stored.labels).toEqual(["report"]);
    expect(stored.artifacts.map((artifact) => artifact.slug)).toContain("shot-png");
    const spec = await getIssueArtifactText(filed.key, "spec");
    expect(spec.markdown).toContain("The Send button overlaps the box");
    expect(spec.markdown).toContain(`![shot.png](dispatch://${filed.key}/artifact/shot-png@v1)`);
    expect(spec.markdown).toContain(`/issues/${issue.key}`);
    expect(spec.markdown).toContain("Reported by: alice");

    // The link opens the new issue, whose spec shows the picture.
    await dialog.getByRole("link", { name: filed.key }).click();
    await expect(page).toHaveURL(`/issues/${filed.key}`);
    await expect(reportDialog(page)).toHaveCount(0);
    const picture = documentEditor(page).getByRole("img", { name: "shot.png" });
    await expect(picture).toHaveAttribute(
      "src",
      `/api/v1/issues/${filed.key}/artifacts/shot-png/versions/1`
    );
    await picture.scrollIntoViewIfNeeded();
    await expect
      .poll(() => picture.evaluate((image: HTMLImageElement) => image.naturalWidth))
      .toBeGreaterThan(0);
  } finally {
    await alice.close();
  }
});

test("from the palette, with nothing set, the dialog says reports are not set up", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Reports are off here" });
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}`);
    await expect(
      page.getByTestId("issue-header").getByRole("heading", { level: 1, name: issue.title })
    ).toBeVisible();
    await page.locator("body").focus();
    await page.keyboard.press("Control+k");
    await page.getByRole("combobox", { name: "Search" }).fill("Report");
    await page
      .getByRole("dialog", { name: "Search" })
      .getByRole("group", { name: "Actions" })
      .getByRole("option", { name: "Report a problem" })
      .click();
    const dialog = reportDialog(page);
    await expect(dialog).toContainText("Reports are not set up yet");
    await dialog.getByRole("link", { name: "Settings → Problem reports" }).click();
    await expect(page).toHaveURL("/settings#problem-reports");
    await expect(page.getByRole("heading", { name: "Problem reports" })).toBeInViewport();
  } finally {
    await alice.close();
  }
});
