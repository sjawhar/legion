import { access, mkdir, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { expect, type Page, test } from "@playwright/test";

import { createAsk, createComment, createIssue, createProject } from "./api";
import { resetDatabase } from "./seed";

const fixtureDirectory = fileURLToPath(new URL("./fixtures", import.meta.url));
const diagramPath = join(fixtureDirectory, "diagram.png");
const notesPath = join(fixtureDirectory, "notes.md");
const tinyPng =
  "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVQIHWP4z8DwHwAFgAI/ScL0eAAAAABJRU5ErkJggg==";

test.use({ extraHTTPHeaders: { "X-Dispatch-User": "alice" } });

test.beforeAll(async () => {
  await mkdir(fixtureDirectory, { recursive: true });
  await access(diagramPath).catch(() => writeFile(diagramPath, Buffer.from(tinyPng, "base64")));
  await writeFile(notesPath, "# Review notes\n\nThese notes replace the initial spec.\n");
});

test.beforeEach(async () => {
  await resetDatabase();
});

async function openArtifacts(page: Page, isPhone: boolean) {
  if (isPhone) {
    const reviewPanel = page.getByRole("button", { name: /review panel/i });
    if ((await reviewPanel.getAttribute("aria-expanded")) !== "true") {
      await reviewPanel.click();
    }
  }
  await page.getByRole("tab", { name: "Artifacts" }).click();
}

test("artifacts upload, version, primary selection, references, and phone layout", async ({
  page,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "# Initial spec\n\nThe original document.",
    title: "Artifact review",
  });
  await createAsk(issue.key, { question: "Can this ship?" });

  await page.goto(`/issues/${issue.key}`);
  await openArtifacts(page, testInfo.project.name === "iphone");

  const upload = page.getByLabel("Upload artifact");
  await upload.setInputFiles(diagramPath);
  const diagram = page.getByTestId("artifact-diagram-png");
  await expect(diagram).toContainText("diagram.png");
  await expect(diagram).toContainText("1 version");
  await expect(diagram.getByRole("img", { name: "diagram.png version 1" })).toBeVisible();
  await expect(diagram.getByRole("button", { name: "Make primary" })).toBeDisabled();
  await expect(diagram.getByRole("button", { name: "Make primary" })).toHaveAttribute(
    "title",
    "Only documents can be primary"
  );

  await upload.setInputFiles(diagramPath);
  await expect(diagram).toContainText("2 versions");
  await diagram.getByRole("button", { name: "Show all versions" }).click();
  await expect(diagram).toContainText("Version 1");
  await expect(diagram).toContainText("Version 2");
  await expect(diagram).toContainText("SHA-256");
  await expect(diagram.getByRole("link", { name: "Download version 2" })).toHaveAttribute(
    "download",
    ""
  );

  await upload.setInputFiles(notesPath);
  const notes = page.getByTestId("artifact-notes-md");
  await expect(notes).toContainText("notes.md");
  await notes.getByRole("button", { name: "Make primary" }).click();
  await expect(notes).toContainText("Primary");
  await expect(page.getByTestId("artifact-spec")).toContainText("Not primary");

  await page.getByRole("tab", { name: "Spec" }).click();
  await expect(page.getByRole("textbox", { name: "Document editor" })).toContainText(
    "These notes replace the initial spec."
  );

  await createComment(issue.key, {
    body: `See dispatch://${issue.key}/artifact/notes-md@v1 before deciding.`,
  });

  await createComment(issue.key, {
    body: `See dispatch://${issue.key}/artifact/diagram-png before deciding.`,
  });
  await page.reload();
  await openArtifacts(page, testInfo.project.name === "iphone");
  await expect(diagram.getByLabel("Referenced by")).toContainText("Comment");
  await expect(diagram.getByLabel("Referenced by")).toContainText("diagram.png");
  const documentRequests: string[] = [];
  page.on("websocket", (socket) => {
    if (socket.url().includes("/ws/doc/")) {
      documentRequests.push(socket.url());
    }
  });
  await diagram.getByLabel("Referenced by").getByRole("link", { name: "diagram.png" }).click();
  await expect(page).toHaveURL(`/issues/${issue.key}/artifacts/diagram-png`);
  await expect(page.getByTestId("artifact-diagram-png")).toHaveClass(/ring-2/);
  expect(documentRequests).toEqual([]);

  await expect(notes.getByLabel("Referenced by")).toContainText("notes.md");
  await notes.getByLabel("Referenced by").getByRole("link", { name: "notes.md" }).click();
  await expect(page).toHaveURL(`/issues/${issue.key}/artifacts/notes-md?v=1`);
  await expect(page.getByRole("heading", { name: "Version 1" })).toBeVisible();
  await expect(page.getByRole("region", { name: "Document version 1" })).toContainText(
    "Review notes"
  );
  await page.screenshot({ path: testInfo.outputPath("artifacts-tab.png"), fullPage: true });

  if (testInfo.project.name === "iphone") {
    await page.goto("/");
    await expect(page.getByTestId(/ask-/).first()).toContainText("Can this ship?");
    await page.screenshot({ path: testInfo.outputPath("iphone-inbox.png"), fullPage: true });

    const openNavigation = page.getByRole("button", { name: "Open navigation" });
    await expect(openNavigation).toBeVisible();
    await openNavigation.click();
    await expect(page.getByRole("navigation", { name: "Issues" })).toBeVisible();
    await page.getByRole("button", { name: "Close navigation" }).click();
    await page.goto(`/issues/${issue.key}`);
    const openMargin = page.getByRole("button", { name: /review panel/i });
    if ((await openMargin.getAttribute("aria-expanded")) === "true") {
      await openMargin.click();
    }
    const [navigationBox, marginBox] = await Promise.all([
      openNavigation.boundingBox(),
      openMargin.boundingBox(),
    ]);
    expect(navigationBox?.width).toBeGreaterThanOrEqual(44);
    expect(navigationBox?.height).toBeGreaterThanOrEqual(44);
    expect(marginBox?.width).toBeGreaterThanOrEqual(44);
    expect(marginBox?.height).toBeGreaterThanOrEqual(44);
    await openMargin.click();
    await expect(page.getByTestId("margin-sheet")).toHaveAttribute("data-expanded", "true");
    await page.screenshot({ path: testInfo.outputPath("iphone-margin-sheet.png"), fullPage: true });
    expect(
      await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)
    ).toBe(true);
  }
});
