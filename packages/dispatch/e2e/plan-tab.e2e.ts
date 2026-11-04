import { expect, test } from "@playwright/test";

import { createIssue, createIssueArtifact, createPlanReview, createProject } from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

/** The spec the plan's tasks serve. A `serves` pointer is refused unless it names a block that
 *  exists, so the requirements are written with the issue, before the plan points at one. */
const spec = `:::requirement{#r1}
Every issue page shows a Plan tab.
:::

:::requirement{#r2}
The Plan tab shows the plan review's approval.
:::
`;

test.beforeEach(async () => {
  await resetDatabase();
});

test("the Plan tab shows each plan task with the requirements it serves and the tasks it needs, and the plan review's approval", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "PLN", name: "Planning" });
  const issue = await createIssue({ project: "PLN", spec, title: "Plan tab" });
  const plan = await createIssueArtifact(issue.key, {
    // A `needs` pointer names a task of the plan's own document, so both tasks are written in
    // the one upload that makes the plan.
    content: `How ${issue.key} is built.

:::plan_task{#t0 serves="dispatch://${issue.key}/spec#b-r1"}
Add the routes.
:::

:::plan_task{#t1 serves="dispatch://${issue.key}/spec#b-r2" needs="dispatch://${issue.key}/artifact/plan-md#b-t0"}
Draw the panel.
:::
`,
    name: "plan.md",
  });
  expect(plan.artifact.slug).toBe("plan-md");
  await createPlanReview(plan.artifact.id, 1);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/plan`);
    await expect(page.getByTestId("plan-tab")).toBeVisible();

    // A plan is approved by its plan review, never by a person, and the chip beside it says so.
    await expect(page.getByTestId("plan-approval")).toContainText("Approved by plan review v1");

    // Only the `plan_task` blocks are tasks; the plan's prose is the document's to draw.
    const tasks = page.getByTestId("plan-task");
    await expect(tasks).toHaveCount(2);

    // The tasks are listed in document order; a task is not picked out by its words, since the
    // task after it draws those same words in its `needs` row.
    const routes = tasks.nth(0);
    await expect(routes).toContainText("Add the routes.");
    await expect(
      routes.getByTestId("plan-task-serves").getByRole("link", {
        name: "Every issue page shows a Plan tab.",
      })
    ).toHaveAttribute("href", `/issues/${issue.key}/spec#b-r1`);
    // A task nothing comes before says so rather than showing an empty row.
    await expect(routes.getByTestId("plan-task-needs")).toHaveText("Needsnone");

    const panel = tasks.nth(1);
    await expect(panel).toContainText("Draw the panel.");
    await expect(
      panel.getByTestId("plan-task-serves").getByRole("link", {
        name: "The Plan tab shows the plan review's approval.",
      })
    ).toHaveAttribute("href", `/issues/${issue.key}/spec#b-r2`);
    await expect(
      panel.getByTestId("plan-task-needs").getByRole("link", { name: "Add the routes." })
    ).toHaveAttribute("href", `/issues/${issue.key}/artifacts/plan-md#b-t0`);
    await page.screenshot({ path: testInfo.outputPath("plan-tasks.png") });
  } finally {
    await alice.close();
  }
});

test("a plan that marks no tasks says so", async ({ browser }) => {
  await createProject({ key: "PLN", name: "Planning" });
  const issue = await createIssue({ project: "PLN", spec, title: "Prose only" });
  await createIssueArtifact(issue.key, {
    content: "We will decide the tasks once the spec is approved.\n",
    name: "plan.md",
  });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/plan`);

    await expect(page.getByTestId("plan-no-tasks")).toContainText("This plan marks no tasks.");
    await expect(page.getByTestId("plan-task")).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

test("an issue with no plan document shows the command that makes one", async ({ browser }) => {
  await createProject({ key: "PLN", name: "Planning" });
  const issue = await createIssue({ project: "PLN", title: "Nothing planned yet" });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/plan`);

    const empty = page.getByTestId("plan-empty");
    await expect(empty).toContainText(`${issue.key} has no plan document.`);
    await expect(empty).toContainText(`dispatch artifact --issue ${issue.key} --name plan.md`);
    await expect(page.getByTestId("plan-tasks")).toHaveCount(0);
  } finally {
    await alice.close();
  }
});
