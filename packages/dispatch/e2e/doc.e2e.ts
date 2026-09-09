import { expect, test } from "@playwright/test";

import { createIssue, createProject, editArtifact, getArtifact, getArtifactVersion } from "./api";
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

test("document edits synchronize, version, and compare across users", async ({
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
    const aliceEditor = alicePage.getByRole("textbox", { name: "Document editor" });
    await expect(aliceEditor).toContainText("Use SQLite");
    await aliceEditor.click();
    await aliceEditor.press("Control+End");
    await aliceEditor.press("Enter");
    await aliceEditor.type("hello");

    await expect
      .poll(async () => {
        const version = (await getArtifact(artifactId)).versions.find(({ number }) => number === 2);
        return version;
      })
      .toMatchObject({ authors: [{ id: "alice", kind: "user" }], named: false, number: 2 });
    await expect
      .poll(() => getArtifactVersion(artifactId, 2).then(({ markdown }) => markdown))
      .toContain("hello");

    const bobPage = await bob.newPage();
    await bobPage.goto(`/issues/${issue.key}`);
    await bobPage.getByRole("tab", { name: "Spec" }).click();
    const bobEditor = bobPage.getByRole("textbox", { name: "Document editor" });
    await expect(bobEditor).toContainText("hello");
    await bobEditor.click();
    const bobCaret = alicePage.locator(".cm-ySelectionCaret");
    await expect(bobCaret).toContainText("bob");
    await bobCaret.hover();
    await expect(bobCaret.locator(".cm-ySelectionInfo")).toHaveCSS("opacity", "1");
    await alicePage.screenshot({
      path: testInfo.outputPath("live-document-edit.png"),
      fullPage: true,
    });

    await editArtifact(
      artifactId,
      { ops: [{ find: "SQLite", op: "replace", with: "Postgres" }] },
      session
    );
    await expect(aliceEditor).toContainText("Postgres");
    await expect(bobEditor).toContainText("Postgres");
    // A summary-less API edit still records an unnamed version and its event.
    await expect
      .poll(async () => {
        const version = (await getArtifact(artifactId)).versions.find(({ number }) => number === 3);
        return version;
      })
      .toMatchObject({ named: false, number: 3, summary: null });
    await expect
      .poll(() => getArtifactVersion(artifactId, 3).then(({ markdown }) => markdown))
      .toContain("Postgres");

    alicePage.once("dialog", (dialog) => dialog.accept("Decided Postgres"));
    await alicePage.getByRole("button", { name: "Name version" }).click();
    await expect
      .poll(async () => {
        const version = (await getArtifact(artifactId)).versions.find(({ number }) => number === 4);
        return version;
      })
      .toMatchObject({ named: true, number: 4, summary: "Decided Postgres" });

    const versionPicker = alicePage.getByLabel("Version");
    await expect(versionPicker).toContainText("Version 4 — Decided Postgres");
    await versionPicker.selectOption("2");
    await expect(alicePage.getByTestId("version-view")).toContainText("Use SQLite");
    await alicePage.getByRole("button", { name: "Diff vs current" }).click();
    await expect(alicePage.getByTestId("version-diff").locator("del")).toContainText("Use SQLite");
    await expect(alicePage.getByTestId("version-diff").locator("ins")).toContainText(
      "Use Postgres"
    );
    await alicePage.screenshot({
      path: testInfo.outputPath("document-version-diff.png"),
      fullPage: true,
    });
  } finally {
    await bob.close();
    await alice.close();
  }
});
