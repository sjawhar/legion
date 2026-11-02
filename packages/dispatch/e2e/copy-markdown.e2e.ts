import { expect, test } from "@playwright/test";

import {
  createComment,
  createIssue,
  createProject,
  createProjectDocument,
  editArtifact,
  getArtifact,
  getArtifactText,
  getArtifactVersion,
} from "./api";
import { recordClipboard } from "./clipboard";
import { connectedDot, documentEditor } from "./editor";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

const session = {
  actor: { kind: "session" as const, id: "e2e-copy-markdown" },
  as: "agent" as const,
};

test.beforeEach(async () => {
  await resetDatabase();
});

test("Copy Markdown copies the spec on screen as Dispatch stores it, with no comment or suggestion marks, and a past version from the palette", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue(
    {
      project: "CORE",
      spec: "Plan\n\nUse **SQLite** for the snake_case_name store.\n\nNotes.\n",
      title: "Copy the spec",
    },
    session
  );
  const artifactID = issue.primary_artifact_id;
  await editArtifact(
    artifactID,
    { ops: [{ op: "replace", find: "**SQLite**", with: "**Postgres**" }] },
    session
  );
  await expect
    .poll(async () => (await getArtifact(artifactID, { login: "alice" })).versions.length)
    .toBeGreaterThan(1);
  // A comment and a suggestion each mark words of the current text.
  await createComment(
    issue.key,
    { anchor: { artifact: "spec", quote: "Postgres" }, body: "Why not SQLite?" },
    session
  );
  await createComment(
    issue.key,
    {
      anchor: { artifact: "spec", quote: "case_name" },
      body: "Name it for what it holds.",
      suggestion: { replace_with: "case_label" },
    },
    session
  );

  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    const copied = await recordClipboard(page);
    await page.goto(`/issues/${issue.key}/spec`);
    // The suggestion draws its replacement beside the words it would replace, so the page reads
    // "snake_case_namecase_label"; the copy below holds the stored words alone.
    await expect(documentEditor(page)).toContainText("Use Postgres for the snake_case_name");
    await expect(connectedDot(page)).toHaveText("connected");
    await expect(page.getByText("Why not SQLite?")).toBeVisible();

    // An edit typed a moment before the copy is in it: no version has settled yet.
    await documentEditor(page).getByText("Notes.", { exact: true }).click();
    await page.keyboard.press("End");
    await page.keyboard.type(" Typed just now.");
    const tabs = page.getByTestId("issue-tabs");
    const copyMarkdown = tabs.getByRole("button", { name: "Copy Markdown" });
    await copyMarkdown.click();
    await expect(tabs.getByText("Copied", { exact: true })).toBeVisible();
    await page.screenshot({
      path: testInfo.outputPath(`copy-markdown-spec-${testInfo.project.name}.png`),
    });
    const current = (await getArtifactText(artifactID, { login: "alice" })).markdown;
    expect(current).toContain("Use **Postgres** for the snake_case_name store.");
    expect(current).toContain("Notes. Typed just now.");
    expect(await copied()).toEqual([current]);
    for (const mark of ["<span", "data-proof", "Why not SQLite?", "case_label"]) {
      expect(current).not.toContain(mark);
    }

    // An older version picked from Version: the palette's row copies that one.
    await tabs.getByRole("combobox", { name: "Version" }).selectOption("1");
    await expect(page).toHaveURL(new RegExp(`/issues/${issue.key}/artifacts/spec\\?v=1$`));
    await expect(page.getByRole("region", { name: "Document version 1" })).toContainText(
      "Use SQLite"
    );
    // The first copy's confirmation clears after 1.5 s, so the one below is the palette's.
    await expect(tabs.getByText("Copied", { exact: true })).toHaveCount(0);
    await page.locator("body").focus();
    await page.keyboard.press("Control+k");
    const palette = page.getByRole("dialog", { name: "Search" });
    await palette
      .getByRole("group", { name: "Actions" })
      .getByRole("option", { name: "Copy Markdown" })
      .click();
    await expect(palette).toHaveCount(0);
    await expect(tabs.getByText("Copied", { exact: true })).toBeVisible();
    const first = (await getArtifactVersion(artifactID, 1, { login: "alice" })).markdown;
    expect(first).toContain("Use **SQLite**");
    expect(await copied()).toEqual([current, first]);
  } finally {
    await context.close();
  }
});

test("Copy Markdown on a project's uploaded Markdown document copies its text beside the link button", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const created = await createProjectDocument(
    "CORE",
    { content: "# Runbook\n\n1. Drain the node.\n2. Restart it.\n", name: "runbook.md" },
    session
  );

  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    const copied = await recordClipboard(page);
    await page.goto(`/projects/CORE/documents/${created.artifact.slug}`);
    await expect(documentEditor(page)).toContainText("Drain the node.");
    const header = page.getByTestId("artifact-header");
    const copyMarkdown = header.getByRole("button", { name: "Copy Markdown" });
    await copyMarkdown.click();
    await expect(header.getByText("Copied", { exact: true })).toBeVisible();
    const markdown = (await getArtifactText(created.artifact.id, { login: "alice" })).markdown;
    expect(markdown).toContain("1. Drain the node.");
    expect(await copied()).toEqual([markdown]);
  } finally {
    await context.close();
  }
});

test("Copy Markdown fits a phone next to the link button, a 44 px target that never makes the page scroll sideways @phone", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "iphone", "the 390 px layout applies on iPhone");
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue(
    { project: "CORE", spec: "Plan\n\nShip it.\n", title: "Copy a spec from a phone" },
    session
  );
  const created = await createProjectDocument(
    "CORE",
    {
      content: "# Runbook\n\nDrain the node.\n",
      name: "a-runbook-whose-name-is-long-enough-to-wrap-on-a-phone.md",
    },
    session
  );

  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    const copied = await recordClipboard(page);
    for (const [path, text] of [
      [`/issues/${issue.key}/spec`, "Ship it."],
      [`/projects/CORE/documents/${created.artifact.slug}`, "Drain the node."],
    ] as const) {
      await page.goto(path);
      await expect(documentEditor(page)).toContainText(text);
      const copyMarkdown = page.getByRole("button", { name: "Copy Markdown" });
      expect((await copyMarkdown.boundingBox())?.height ?? 0).toBeGreaterThanOrEqual(44);
      await copyMarkdown.click();
      await expect(page.getByText("Copied", { exact: true })).toBeVisible();
      await page.screenshot({
        path: testInfo.outputPath(
          `copy-markdown-${path.split("/")[1]}-${testInfo.project.name}.png`
        ),
      });
      // The recorder starts empty on each page load.
      expect(await copied()).toEqual([expect.stringContaining(text)]);
      expect(
        await page.evaluate(
          () => document.documentElement.scrollWidth <= document.documentElement.clientWidth
        ),
        `${path} scrolls sideways`
      ).toBe(true);
    }
  } finally {
    await context.close();
  }
});
