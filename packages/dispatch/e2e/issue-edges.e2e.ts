import { expect, test } from "@playwright/test";
import { createIssue, createMessage, createProject, patchIssue } from "./api";
import { resetDatabase } from "./seed";
import { signIn } from "./users";

test("@phone issue Links shows a derived duplicate and leaves the parent to the Parent line", async ({
  page,
  context,
}, testInfo) => {
  await resetDatabase();
  await signIn(context, "alice");
  await createProject({ key: "EDGE", name: "Issue edges" });
  const parent = await createIssue({ project: "EDGE", title: "Parent specification" });
  const canonical = await createIssue({ project: "EDGE", title: "Canonical defect" });
  const duplicate = await createIssue({
    project: "EDGE",
    title: "Repeated report",
    parent: parent.key,
  });
  await createMessage(duplicate.key, { body: `Duplicate of dispatch://${canonical.key}.` });
  await patchIssue(duplicate.key, { status: "done" });
  await page.goto(`/issues/${duplicate.key}`);
  const links = page.getByRole("region", { name: "Links", exact: true });
  await expect(links).toBeVisible();
  await expect(links).toContainText("duplicate_of");
  await expect(links).not.toContainText("child_of");
  await expect(links.getByRole("link", { name: canonical.key })).toHaveAttribute(
    "href",
    `/issues/${canonical.key}`
  );
  await expect(
    page.getByTestId("issue-metadata-rail").getByRole("link", { name: parent.key })
  ).toBeVisible();
  await expect(page.getByRole("link", { name: parent.key, exact: true })).toHaveCount(1);
  await expect(
    page.evaluate(
      () => document.documentElement.scrollWidth <= document.documentElement.clientWidth
    )
  ).resolves.toBe(true);
  await page.screenshot({ path: testInfo.outputPath("issue-links.png") });
  await links.getByRole("link", { name: canonical.key }).click();
  await expect(page).toHaveURL(new RegExp(`/issues/${canonical.key}$`));
});
