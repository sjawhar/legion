import { expect, test } from "@playwright/test";

import {
  createComment,
  createIssue,
  createProject,
  editArtifact,
  getArtifact,
  getArtifactVersion,
} from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

const session = {
  actor: { kind: "session" as const, id: "e2e-doc-editor", origin: { tmux: "dispatch:1.3" } },
  as: "agent" as const,
};
const initialMarkdown = "## Database\nUse SQLite";

test.beforeEach(async () => {
  await resetDatabase();
});

test("document versions and comparison stay current across users", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: initialMarkdown,
    title: "Database decision",
  });
  const artifactId = issue.primary_artifact_id;
  const alice = await asUser(browser, "alice");
  const bob = await asUser(browser, "bob");

  try {
    const alicePage = await alice.newPage();
    await alicePage.goto(`/issues/${issue.key}`);
    await alicePage.getByRole("tab", { name: "Spec" }).click();
    const aliceDocument = alicePage.getByRole("article");
    await expect(aliceDocument).toContainText("Use SQLite");

    const bobPage = await bob.newPage();
    await bobPage.goto(`/issues/${issue.key}`);
    await bobPage.getByRole("tab", { name: "Spec" }).click();
    const bobDocument = bobPage.getByRole("article");
    await expect(bobDocument).toContainText("Use SQLite");

    await editArtifact(
      artifactId,
      { ops: [{ find: "SQLite", op: "replace", with: "Postgres" }] },
      session
    );
    await expect(aliceDocument).toContainText("Postgres");
    await expect(bobDocument).toContainText("Postgres");
    await expect
      .poll(async () => {
        const version = (await getArtifact(artifactId)).versions.find(({ number }) => number === 2);
        return version;
      })
      .toMatchObject({ named: false, number: 2, summary: null });
    await expect
      .poll(() => getArtifactVersion(artifactId, 2).then(({ markdown }) => markdown))
      .toContain("Postgres");

    alicePage.once("dialog", (dialog) => dialog.accept("Decided Postgres"));
    await alicePage.getByRole("button", { name: "Name version" }).click();
    await expect
      .poll(async () => {
        const version = (await getArtifact(artifactId)).versions.find(({ number }) => number === 3);
        return version;
      })
      .toMatchObject({ named: true, number: 3, summary: "Decided Postgres" });

    const versionPicker = alicePage.getByRole("combobox", { name: "Version" });
    await expect(versionPicker).toContainText("Version 3 — Decided Postgres");
    await versionPicker.selectOption("1");
    await expect(alicePage.getByTestId("version-view")).toContainText("Use SQLite");
    await alicePage.getByRole("button", { name: "Diff vs current" }).click();
    await expect(alicePage.getByTestId("version-diff").locator("del")).toContainText("SQLite");
    await expect(alicePage.getByTestId("version-diff").locator("ins")).toContainText("Postgres");
    await alicePage.screenshot({
      path: testInfo.outputPath("rendered-document-version-diff.png"),
      fullPage: true,
    });
  } finally {
    await bob.close();
    await alice.close();
  }
});

test("the spec renders as a formatted document by default, with no click required", async ({
  browser,
}) => {
  await createProject({ key: "READ", name: "Readable" });
  const issue = await createIssue({
    project: "READ",
    spec: "# Database\n\nUse SQLite",
    title: "Rendering check",
  });
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}`);
    await page.getByRole("tab", { name: "Spec" }).click();

    const heading = page.getByRole("article").getByRole("heading", { level: 1 });
    const paragraph = page.getByRole("article").locator("p").first();
    await expect(heading).toBeVisible();
    await expect(paragraph).toBeVisible();

    const [headingSize, paragraphSize] = await Promise.all([
      heading.evaluate((element) => Number.parseFloat(getComputedStyle(element).fontSize)),
      paragraph.evaluate((element) => Number.parseFloat(getComputedStyle(element).fontSize)),
    ]);
    expect(headingSize).toBeGreaterThan(paragraphSize);
  } finally {
    await alice.close();
  }
});

test("a document-version link renders its anchored quote", async ({ browser }, testInfo) => {
  await createProject({ key: "LINK", name: "Linked documents" });
  const issue = await createIssue({
    project: "LINK",
    spec: "SQLite is local",
    title: "Deep link highlight",
  });
  const comment = await createComment(
    issue.key,
    { anchor: { artifact: "spec", quote: "SQLit" }, body: "Use the embedded store." },
    session
  );
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto(
      `/issues/${issue.key}/artifacts/spec?v=1&comment=${encodeURIComponent(comment.id)}`
    );

    await expect(page.getByRole("article")).toBeVisible();
    const highlight = page.locator("mark.dispatch-anchor-history");
    await expect(highlight).toBeVisible();
    await expect(highlight).toHaveText("SQLit");
    await page.screenshot({
      path: testInfo.outputPath("spec-deep-link-highlight.png"),
      fullPage: true,
    });
  } finally {
    await alice.close();
  }
});
