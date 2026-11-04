import { expect, test } from "@playwright/test";
import type { RequirementCoverage } from "../web/src/api/types";

import { createIssue, createIssueArtifact, createProject, getIssueCoverage } from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

/** Two requirements, so one can be served while the other is not. */
const spec = `:::requirement{#r1}
Every requirement of the spec is listed.
:::

:::requirement{#r2}
A requirement nothing checks is flagged.
:::
`;

/** One requirement's coverage, read once. The flag and the Referenced-by list are read from the
 *  block index of the version just written, so each call site polls this until the answer it
 *  waits for lands rather than trusting the first read. */
async function coverageOf(issue: string, blockId: string): Promise<RequirementCoverage> {
  const coverage = await getIssueCoverage(issue);
  const requirement = coverage.requirements.find((row) => row.block_id === blockId);
  if (requirement === undefined) {
    throw new Error(`${issue} has no requirement ${blockId}: ${JSON.stringify(coverage)}`);
  }
  return requirement;
}

test.beforeEach(async () => {
  await resetDatabase();
});

test("a requirement no check serves is flagged on the Spec tab, and a check that serves it clears the flag", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "COV", name: "Coverage" });
  const issue = await createIssue({ project: "COV", spec, title: "Requirement coverage" });
  const oneCheck = `:::check{#c1 serves="dispatch://${issue.key}/spec#b-r1" surface="browser" run="bun run e2e -- e2e/coverage.e2e.ts"}
Every requirement of the spec is listed.
:::
`;
  await createIssueArtifact(issue.key, { content: oneCheck, name: "verification.md" });
  await expect.poll(() => coverageOf(issue.key, "r2").then((row) => row.checks)).toBe(0);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}`);

    const requirements = page.getByTestId("spec-requirement");
    await expect(requirements).toHaveCount(2);
    const served = requirements.filter({ hasText: "Every requirement of the spec is listed." });
    const unserved = requirements.filter({ hasText: "A requirement nothing checks is flagged." });
    await expect(served).toHaveAttribute("data-covered", "true");
    await expect(served.getByTestId("spec-requirement-gap")).toHaveCount(0);
    // The issue has no plan document yet, so the one thing this requirement lacks is a check.
    await expect(unserved).toHaveAttribute("data-covered", "false");
    await expect(unserved.getByTestId("spec-requirement-gap")).toHaveText("has no check");
    await page.screenshot({ path: testInfo.outputPath("spec-requirement-gap.png") });

    // A check that serves it is the whole of what it lacked: the next version of the
    // Verification document clears the flag.
    await createIssueArtifact(issue.key, {
      content: `${oneCheck}
:::check{#c2 serves="dispatch://${issue.key}/spec#b-r2" surface="browser" run="bun run e2e -- e2e/coverage.e2e.ts"}
A requirement nothing checks is flagged.
:::
`,
      name: "verification.md",
    });
    await expect.poll(() => coverageOf(issue.key, "r2").then((row) => row.checks)).toBe(1);

    await page.reload();
    await expect(unserved).toHaveAttribute("data-covered", "true");
    await expect(page.getByTestId("spec-requirement-gap")).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

test("a requirement's Referenced-by list shows the plan task that serves it, and loses it when that task goes", async ({
  browser,
}) => {
  await createProject({ key: "COV", name: "Coverage" });
  const issue = await createIssue({ project: "COV", spec, title: "Referenced by a plan task" });
  const plan = await createIssueArtifact(issue.key, {
    content: `How ${issue.key} is built.

:::plan_task{#t1 serves="dispatch://${issue.key}/spec#b-r1"}
List every requirement under the spec.
:::
`,
    name: "plan.md",
  });
  expect(plan.artifact.slug).toBe("plan-md");
  await expect.poll(() => coverageOf(issue.key, "r1").then((row) => row.plan_tasks)).toBe(1);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}`);
    const requirement = page
      .getByTestId("spec-requirement")
      .filter({ hasText: "Every requirement of the spec is listed." });
    await requirement.getByRole("button", { name: "Referenced by" }).click();

    // A `serves` edge read from the requirement says what the plan task does for it, and links
    // to the task's own block.
    const panel = requirement.getByRole("region", { name: "Referenced by" });
    await expect(panel.getByRole("link", { name: `served by · ${issue.key}` })).toHaveAttribute(
      "href",
      `/issues/${issue.key}/artifacts/plan-md#b-t1`
    );
    await expect(panel).toContainText("List every requirement under the spec.");

    // The next version of the plan drops the task, and with it the edge.
    await createIssueArtifact(issue.key, {
      content: `How ${issue.key} is built.\n`,
      name: "plan.md",
    });
    await expect.poll(() => coverageOf(issue.key, "r1").then((row) => row.plan_tasks)).toBe(0);

    await page.reload();
    await requirement.getByRole("button", { name: "Referenced by" }).click();
    await expect(requirement.getByRole("region", { name: "Referenced by" })).toContainText(
      "Nothing references this yet."
    );
  } finally {
    await alice.close();
  }
});
