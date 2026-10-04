import { expect, test } from "@playwright/test";

import { createIssue, createProject, getIssue, listIssues, patchIssue } from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

test.beforeEach(async () => {
  await resetDatabase();
});

test("an issue shows its spec's task list and its children counted, on its page, in the list and on the board", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const root = await createIssue({
    project: "CORE",
    spec: "## Plan\n\n- [x] decide\n- [ ] build\n  - [x] nested done\n- [ ] ship\n",
    title: "Tracked work",
  });
  const first = await createIssue({ parent: root.key, project: "CORE", title: "First child" });
  await createIssue({ parent: root.key, project: "CORE", title: "Second child" });
  await patchIssue(first.key, { status: "done" });
  const plain = await createIssue({ project: "CORE", spec: "Just prose.\n", title: "Plain" });

  // The API, before any screen: the contract the check-in collector reads.
  const read = await getIssue(root.key);
  expect(read.progress).toEqual({
    tasks: { done: 2, total: 4 },
    children: { done: 1, total: 2 },
  });
  const rows = await listIssues("CORE");
  expect(rows.find((row) => row.key === root.key)?.progress).toEqual(read.progress);
  expect(rows.find((row) => row.key === plain.key)?.progress).toEqual({
    tasks: null,
    children: null,
  });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const isPhone = testInfo.project.name === "iphone";
    if (!isPhone) {
      await page.setViewportSize({ height: 900, width: 1280 });
    }
    const shot = async (name: string) => {
      const path = testInfo.outputPath(`progress-${name}-${testInfo.project.name}.png`);
      await page.screenshot({ fullPage: true, path });
      await testInfo.attach(`progress ${name} ${testInfo.project.name}`, {
        contentType: "image/png",
        path,
      });
    };

    await page.goto(`/issues/${root.key}`);
    await expect(page.getByTestId("issue-progress-tasks")).toHaveText("2/4 tasks");
    await expect(page.getByTestId("issue-progress-children")).toHaveText("1/2 children");
    await shot("issue");

    await page.goto(`/issues/${plain.key}`);
    await expect(page.getByRole("heading", { name: "Plain" })).toBeVisible();
    await expect(page.getByTestId("issue-progress-tasks")).toHaveCount(0);
    await expect(page.getByTestId("issue-progress-children")).toHaveCount(0);

    await page.goto("/projects/CORE/issues");
    const rootRow = page.getByRole("listitem", { name: `${root.key} Tracked work` });
    await expect(rootRow.getByTestId("issue-progress-tasks")).toHaveText("2/4 tasks");
    await expect(rootRow.getByTestId("issue-progress-children")).toHaveText("1/2 children");
    const plainRow = page.getByRole("listitem", { name: `${plain.key} Plain` });
    await expect(plainRow).toBeVisible();
    await expect(plainRow.getByTestId("issue-progress-tasks")).toHaveCount(0);
    await shot("list");

    await page.goto("/projects/CORE");
    await page.getByRole("button", { name: "Board" }).click();
    const rootCard = page.getByRole("article", { name: `${root.key} Tracked work` });
    await expect(rootCard.getByTestId("issue-progress-tasks")).toHaveText("2/4 tasks");
    await expect(rootCard.getByTestId("issue-progress-children")).toHaveText("1/2 children");
    await shot("board");
  } finally {
    await alice.close();
  }
});
