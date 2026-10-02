import { access, mkdir, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { expect, type Locator, type Page, test } from "@playwright/test";

import { createAsk, createComment, createIssue, createIssueArtifact, createProject } from "./api";
import { countDocumentSockets, documentEditor, documentTransport } from "./editor";
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
    if ((await reviewPanel.getAttribute("aria-expanded")) === "true") {
      await reviewPanel.click();
    }
  }
  await page.getByRole("tab", { name: "Artifacts" }).click();
}

// Choosing a file only stages it; the row then shows a Summary field plus a confirming
// "Upload" button (or "Cancel" to discard the pick) before it actually creates the version.
async function confirmUpload(page: Page): Promise<void> {
  await page.getByRole("button", { exact: true, name: "Upload" }).click();
}

test("artifacts upload, version, references, and phone layout", async ({ page }, testInfo) => {
  const sockets = countDocumentSockets(page);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "# Initial spec\n\nThe original document.",
    title: "Artifact review",
  });
  await createAsk(issue.key, { question: "Can this ship?" });

  await page.goto(`/issues/${issue.key}`);
  await page.getByRole("tab", { name: "Spec" }).click();
  await expect(documentEditor(page)).toContainText("The original document.");
  await expect.poll(sockets).toBe(1);
  await openArtifacts(page, testInfo.project.name === "iphone");

  const upload = page.getByLabel("Upload artifact");
  await upload.setInputFiles(diagramPath);
  await confirmUpload(page);
  const diagram = page.getByTestId("artifact-diagram-png");
  await expect(diagram).toContainText("diagram.png");
  await expect(diagram).toContainText("1 version");
  await expect(diagram.getByRole("img", { name: "diagram.png version 1" })).toBeVisible();

  await upload.setInputFiles(diagramPath);
  await confirmUpload(page);
  await expect(diagram).toContainText("2 versions");
  await expect(diagram.getByRole("link", { name: "Download version 2" })).toHaveAttribute(
    "download",
    ""
  );
  // The tab is a compact row list now — versions, compare, and references live on the
  // artifact's own page, not stacked under every row.
  await expect(diagram.getByRole("button", { name: "Show all versions" })).toHaveCount(0);
  await expect(page.getByLabel("Filter artifacts")).toBeVisible();

  await page.goto(`/issues/${issue.key}/artifacts/diagram-png`);
  await expect(page).toHaveURL(`/issues/${issue.key}/artifacts/diagram-png`);
  await expect(page.getByTestId("artifact-header")).toContainText("diagram.png");
  expect(sockets()).toBe(1);
  await page.getByRole("button", { name: "Show all versions" }).click();
  const versions = page.getByRole("region", { exact: true, name: "Versions for diagram.png" });
  await expect(versions).toContainText("Version 1");
  await expect(versions).toContainText("Version 2");
  await expect(versions).toContainText("SHA-256");
  await expect(versions.getByRole("link", { name: "Download version 2" })).toHaveAttribute(
    "download",
    ""
  );
  const compare = page.getByRole("region", { name: "Compare versions for diagram.png" });
  await expect(compare).toBeVisible();
  await expect(compare).not.toContainText("From · Version");
  await compare.getByRole("button", { name: "Show comparison" }).click();
  await expect(compare).toContainText("From · Version");
  await expect(compare).toContainText("To · Version");

  await page.goto(`/issues/${issue.key}`);
  await openArtifacts(page, testInfo.project.name === "iphone");

  await upload.setInputFiles(notesPath);
  await confirmUpload(page);
  const notes = page.getByTestId("artifact-notes-md");
  await expect(notes).toContainText("notes.md");
  await expect(notes.getByRole("button", { name: /make primary/i })).toHaveCount(0);
  await page.getByRole("tab", { name: "Spec" }).click();
  await expect(documentEditor(page)).toContainText("The original document.");

  await createComment(issue.key, {
    body: `See dispatch://${issue.key}/artifact/notes-md@v1 before deciding.`,
  });

  await createComment(issue.key, {
    body: `See dispatch://${issue.key}/artifact/diagram-png before deciding.`,
  });
  await page.reload();
  await openArtifacts(page, testInfo.project.name === "iphone");

  await diagram.getByRole("link", { name: "diagram.png" }).click();
  await expect(page).toHaveURL(`/issues/${issue.key}/artifacts/diagram-png`);
  // The panel names the record that cites this artifact and quotes it; its link goes to that
  // source, not back to the artifact the reader is already on.
  const diagramReferencedBy = page.getByRole("region", { name: "Referenced by" });
  await expect(diagramReferencedBy).toContainText(`Comment · ${issue.key}`);
  await expect(diagramReferencedBy).toContainText(
    `See dispatch://${issue.key}/artifact/diagram-png before deciding.`
  );
  await diagramReferencedBy.getByRole("link", { name: `Comment · ${issue.key}` }).click();
  await expect(page).toHaveURL(new RegExp(`/issues/${issue.key}/comments/`));

  await page.goto(`/issues/${issue.key}`);
  await openArtifacts(page, testInfo.project.name === "iphone");
  await notes.getByRole("link", { name: "notes.md" }).click();
  await expect(page).toHaveURL(`/issues/${issue.key}/artifacts/notes-md`);
  const notesReferencedBy = page.getByRole("region", { name: "Referenced by" });
  await expect(notesReferencedBy).toContainText(`Comment · ${issue.key}`);
  await expect(notesReferencedBy).toContainText(
    `See dispatch://${issue.key}/artifact/notes-md@v1 before deciding.`
  );
  await page.goto(`/issues/${issue.key}`);
  await openArtifacts(page, testInfo.project.name === "iphone");
  await page.screenshot({ path: testInfo.outputPath("artifacts-tab.png"), fullPage: true });

  if (testInfo.project.name === "iphone") {
    await page.goto("/");
    await expect(page.getByTestId(/ask-/).first()).toContainText("Can this ship?");
    await page.screenshot({ path: testInfo.outputPath("iphone-inbox.png"), fullPage: true });

    const openNavigation = page.getByRole("button", { name: "Open navigation" });
    await expect(openNavigation).toBeVisible();
    await openNavigation.click();
    await expect(page.getByRole("navigation", { name: "Navigation" })).toBeVisible();
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

test("the artifacts tab filter narrows rows by name and kind", async ({ page }, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Artifact filtering" });
  await page.goto(`/issues/${issue.key}`);
  await openArtifacts(page, testInfo.project.name === "iphone");

  const upload = page.getByLabel("Upload artifact");
  await upload.setInputFiles(diagramPath);
  await confirmUpload(page);
  await expect(page.getByTestId("artifact-diagram-png")).toBeVisible();
  await upload.setInputFiles(notesPath);
  await confirmUpload(page);
  await expect(page.getByTestId("artifact-notes-md")).toBeVisible();

  const filter = page.getByLabel("Filter artifacts");
  await filter.fill("notes");
  await expect(page.getByTestId("artifact-notes-md")).toBeVisible();
  await expect(page.getByTestId("artifact-diagram-png")).toHaveCount(0);

  await filter.fill("image");
  await expect(page.getByTestId("artifact-diagram-png")).toBeVisible();
  await expect(page.getByTestId("artifact-notes-md")).toHaveCount(0);

  await filter.fill("");
  await expect(page.getByTestId("artifact-diagram-png")).toBeVisible();
  await expect(page.getByTestId("artifact-notes-md")).toBeVisible();
});

test("an uploaded document is linked from the Conversation and counted on its tab", async ({
  page,
}, testInfo) => {
  await createProject({ key: "OPS", name: "Ops" });
  const issue = await createIssue({ project: "OPS", title: "Customer update" });
  await page.goto(`/issues/${issue.key}/conversation`);
  // Every issue has its own document, so the tab opens counting one.
  await expect(page.getByRole("tab", { exact: true, name: "Artifacts (1)" })).toBeVisible();

  const name = "cu-update-2026-09-15.md";
  const upload = await createIssueArtifact(issue.key, {
    content: "# Draft\n\nHi team, here is this week's update.\n",
    name,
  });

  // The live event stream refreshes the issue and its Conversation: no reload.
  await expect(page.getByRole("tab", { exact: true, name: "Artifacts (2)" })).toBeVisible();
  // The counted label stays on one line, and all four tabs fit the tablist without scrolling at
  // the iPhone width (the tablist clips what it scrolls, so its own edge is the bound).
  const tablist = page.getByRole("tablist", { name: "Issue detail" });
  const tabs = tablist.getByRole("tab");
  await expect(tabs).toHaveCount(4);
  const insideTablist = async (tab: Locator) => {
    const [listBox, box] = await Promise.all([tablist.boundingBox(), tab.boundingBox()]);
    expect(listBox).not.toBeNull();
    expect(box).not.toBeNull();
    expect(box?.height).toBeLessThanOrEqual(48);
    expect(box?.x).toBeGreaterThanOrEqual(listBox?.x ?? 0);
    expect((box?.x ?? 0) + (box?.width ?? 0)).toBeLessThanOrEqual(
      (listBox?.x ?? 0) + (listBox?.width ?? 0) + 0.5
    );
  };
  for (const tab of await tabs.all()) {
    await insideTablist(tab);
  }
  // At 360 px (a default Android width) the tablist overflows; the keyboard still reaches every
  // tab and the focused one is scrolled fully into view rather than left clipped.
  const viewport = page.viewportSize();
  await page.setViewportSize({ height: viewport?.height ?? 800, width: 360 });
  await page.getByRole("tab", { name: "Conversation" }).focus();
  await page.keyboard.press("End");
  const artifactsTab = page.getByRole("tab", { exact: true, name: "Artifacts (2)" });
  await expect(artifactsTab).toBeFocused();
  await insideTablist(artifactsTab);
  await page.keyboard.press("Home");
  await expect(page.getByRole("tab", { name: "Spec" })).toBeFocused();
  await insideTablist(page.getByRole("tab", { name: "Spec" }));
  await page.setViewportSize({ height: viewport?.height ?? 800, width: viewport?.width ?? 1280 });
  await page.getByRole("tab", { name: "Conversation" }).click();
  const added = page.locator('[data-kind="activity"]', { hasText: `added ${name}` });
  await expect(added).toBeVisible();
  await page.screenshot({ path: testInfo.outputPath("conversation-added-artifact.png") });

  // A second upload under the same name is a new version; its line links that version.
  await createIssueArtifact(issue.key, { content: "# Draft\n\nRevised.\n", name });
  const saved = page.locator('[data-kind="activity"]', { hasText: `saved ${name} v2` });
  await expect(saved).toBeVisible();
  await expect(page.getByRole("tab", { exact: true, name: "Artifacts (2)" })).toBeVisible();
  await expect(saved.getByRole("link", { name })).toHaveAttribute(
    "href",
    `/issues/${issue.key}/artifacts/${upload.artifact.slug}?v=2`
  );

  await added.getByRole("link", { name }).click();
  await expect(page).toHaveURL(`/issues/${issue.key}/artifacts/${upload.artifact.slug}`);
  await expect(page.getByTestId("artifact-header")).toContainText(name);
});

test("the Artifacts badge counts the rows the tab lists, and a row's details are not truncated", async ({
  page,
}) => {
  await createProject({ key: "OPS", name: "Ops" });
  const issue = await createIssue({
    project: "OPS",
    spec: "# Spec\n\nThe issue's own document.\n",
    title: "Counted tab",
  });
  await createIssueArtifact(issue.key, { content: "# Audit\n\nOne more.\n", name: "audit.md" });

  await page.goto(`/issues/${issue.key}/artifacts`);
  const rows = page.locator("[data-testid^='artifact-']");
  await expect(rows).toHaveCount(2);
  await expect(page.getByRole("tab", { exact: true, name: "Artifacts (2)" })).toBeVisible();

  // Every row shows its whole detail line: it wraps where it must rather than truncating on a
  // row with room beside it.
  for (const row of await rows.all()) {
    const details = row.locator("p").first();
    await expect(details).toContainText("Updated");
    expect(await details.evaluate((element) => element.scrollWidth <= element.clientWidth)).toBe(
      true
    );
  }
});

test("an out-of-schema document names its repair and uploads replacement markdown", async ({
  page,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Repair an unreadable document" });
  const upload = await createIssueArtifact(issue.key, {
    content: "before\n",
    name: "repair.md",
  });
  const corrupted = await page.request.post(
    `/api/v1/artifacts/${upload.artifact.id}/_test/outside-schema`
  );
  expect(corrupted.status()).toBe(204);

  await page.goto(`/issues/${issue.key}/artifacts/${upload.artifact.slug}`);
  await expect(page.getByText("replace the document from markdown to repair it")).toBeVisible();
  await page.getByLabel("Upload artifact").setInputFiles({
    buffer: Buffer.from("repaired\n"),
    mimeType: "text/markdown",
    name: "repair.md",
  });
  await confirmUpload(page);
  await expect(documentEditor(page)).toContainText("repaired");
});

test("an out-of-schema document does not reconnect from cached text", async ({ page }) => {
  const sockets = countDocumentSockets(page);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Do not reconnect from stale text" });
  const upload = await createIssueArtifact(issue.key, {
    content: "before\n",
    name: "cached-repair.md",
  });

  await page.goto(`/issues/${issue.key}/artifacts/${upload.artifact.slug}`);
  await expect(documentEditor(page)).toContainText("before");
  await expect.poll(sockets).toBe(1);

  await page.goto(`/issues/${issue.key}`);
  const corrupted = await page.request.post(
    `/api/v1/artifacts/${upload.artifact.id}/_test/outside-schema`
  );
  expect(corrupted.status()).toBe(204);
  await page.goto(`/issues/${issue.key}/artifacts/${upload.artifact.slug}`);

  await expect(page.getByText("replace the document from markdown to repair it")).toBeVisible();
  await expect.poll(sockets).toBe(1);
});

// A mounted editor's provider reconnects on its own after a dropped socket. Another client makes
// the stored tree invalid while that socket is down; the server refuses the reconnect before any
// sync, so the browser never holds the tree it would normalize and write back, and the page turns
// the refusal into the repair message instead of reconnecting again.
test("a mounted editor does not reconnect into a document made unreadable while its socket was down", async ({
  page,
}) => {
  const sockets = countDocumentSockets(page);
  const transport = await documentTransport(page);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    title: "No reconnect into an unreadable room",
  });
  const upload = await createIssueArtifact(issue.key, {
    content: "before\n",
    name: "reconnect-repair.md",
  });

  await page.goto(`/issues/${issue.key}/artifacts/${upload.artifact.slug}`);
  await expect(documentEditor(page)).toContainText("before");
  await expect.poll(sockets).toBe(1);

  transport.hold();
  await transport.sever();
  await expect.poll(sockets).toBe(2);
  const corrupted = await page.request.post(
    `/api/v1/artifacts/${upload.artifact.id}/_test/outside-schema`
  );
  expect(corrupted.status()).toBe(204);
  await transport.release();

  await expect(page.getByText("replace the document from markdown to repair it")).toBeVisible();
  await expect(documentEditor(page)).toBeHidden();
  const text = await page.request.get(`/api/v1/artifacts/${upload.artifact.id}/text`);
  expect(text.status()).toBe(409);
  await expect(text.text()).resolves.toContain(`"code":"DOC_SCHEMA"`);
  expect(sockets()).toBe(2);
});

test("a live document refuses a rebuild", async ({ page }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Refuse live rebuild" });
  const upload = await createIssueArtifact(issue.key, {
    content: "before\n",
    name: "live-rebuild.md",
  });

  await page.goto(`/issues/${issue.key}/artifacts/${upload.artifact.slug}`);
  await expect(documentEditor(page)).toContainText("before");
  const rebuilt = await page.request.post(`/api/v1/artifacts/${upload.artifact.id}/rebuild`, {
    data: {},
  });
  expect(rebuilt.status()).toBe(409);
  await expect(rebuilt.text()).resolves.toContain(`"code":"DOCUMENT_LIVE"`);
  await expect(documentEditor(page)).toContainText("before");
});
