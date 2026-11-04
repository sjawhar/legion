import { expect, test } from "@playwright/test";

import { createIssue, createIssueArtifact, createProject } from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

test.beforeEach(async () => {
  await resetDatabase();
});

test("Verification shows the requirements each check serves from one named graph read", async ({
  browser,
}) => {
  await createProject({ key: "VGR", name: "Verification graph" });
  const issue = await createIssue({
    project: "VGR",
    spec: ":::requirement{#r1}\nThe saved answer is preserved.\n:::\n",
    title: "One verification source",
  });
  await createIssueArtifact(issue.key, {
    content: `:::check{#c1 serves="dispatch://${issue.key}/spec#b-r1" surface="browser"}\nThe answer round trips.\n:::\n\n:::check{#c2}\nAn independent check.\n:::\n`,
    name: "verification.md",
  });
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const reads: string[] = [];
    page.on("request", (request) => {
      const path = new URL(request.url()).pathname;
      if (request.method() === "GET" && /\/(verification|results|coverage)$/.test(path)) {
        reads.push(path);
      }
    });
    await page.goto(`/issues/${issue.key}/verification`);
    await expect(page.getByTestId("verification-check")).toHaveCount(2);
    const serving = page.getByTestId("verification-check").filter({ hasText: "The answer round trips." });
    const requirement = serving.getByRole("link", { name: "dispatch://" + issue.key + "/spec#b-r1", exact: true });
    await expect(requirement).toBeVisible();
    await expect(requirement).toHaveAttribute("href", `/issues/${issue.key}/spec#b-r1`);
    await expect(page.getByTestId("verification-check-state")).toHaveText(["No result", "No result"]);
    expect(reads).toEqual([`/api/v1/issues/${issue.key}/verification`]);
  } finally {
    await alice.close();
  }
});
