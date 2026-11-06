import { writeFile } from "node:fs/promises";
import { setTimeout as sleep } from "node:timers/promises";
import { expect, type Locator, type Page, type TestInfo, test } from "@playwright/test";

import { RECONNECT_SPREAD_MS } from "../web/src/api/live-socket";
import { QUERY_RETRY_DELAY_MS } from "../web/src/api/query-client";

import {
  createAsk,
  createComment,
  createIssue,
  createIssueArtifact,
  createProject,
  dashboardOrigin,
  editArtifact,
  getArtifactText,
} from "./api";
import { countDocumentSockets, documentEditor, documentTransport, typeAtEnd } from "./editor";
import { resetDatabase } from "./seed";
import { navigateInApp } from "./sends";
import { signIn } from "./users";

const tinyPng =
  "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVQIHWP4z8DwHwAFgAI/ScL0eAAAAABJRU5ErkJggg==";

// The files a row uploads are written in that row's own output directory, which Playwright gives
// each row of each worker (`testInfo.outputPath`): two workers running these rows at once would
// otherwise write one path in the checkout, and a run must leave no file changed under e2e/.
async function diagramFile(testInfo: TestInfo): Promise<string> {
  const path = testInfo.outputPath("diagram.png");
  await writeFile(path, Buffer.from(tinyPng, "base64"));
  return path;
}

async function notesFile(testInfo: TestInfo): Promise<string> {
  const path = testInfo.outputPath("notes.md");
  await writeFile(path, "# Review notes\n\nThese notes replace the initial spec.\n");
  return path;
}

test.beforeEach(async ({ context }) => {
  await resetDatabase();
  await signIn(context, "alice");
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

// `page.request` sends the page's session cookie and no `Origin`, and the server refuses a
// cookie-authenticated write whose `Origin` is not the dashboard's (enforceCookieOrigin).
function postAsPage(page: Page, path: string, data?: object) {
  return page.request.post(path, { data, headers: { Origin: dashboardOrigin } });
}

// SLOW(>=2s): three real uploads with the versions they build, the artifact page's version list
// and comparison, and the references and phone layout read after them.
test("artifacts upload, version, references, and phone layout @phone", async ({
  page,
}, testInfo) => {
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
  const diagramUpload = await diagramFile(testInfo);
  await upload.setInputFiles(diagramUpload);
  await confirmUpload(page);
  const diagram = page.getByTestId("artifact-diagram-png");
  await expect(diagram).toContainText("diagram.png");
  await expect(diagram).toContainText("1 version");
  await expect(diagram.getByRole("img", { name: "diagram.png version 1" })).toBeVisible();

  await upload.setInputFiles(diagramUpload);
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

  await navigateInApp(page, `/issues/${issue.key}/artifacts/diagram-png`);
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

  await navigateInApp(page, `/issues/${issue.key}`);
  await openArtifacts(page, testInfo.project.name === "iphone");

  await upload.setInputFiles(await notesFile(testInfo));
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
  // No reload: both citations reach the open page on its event feed, and the panel below reads
  // what the feed left.
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

  // One cold load, so the notes.md panel below is read by a page that cached nothing.
  await page.goto(`/issues/${issue.key}`);
  await openArtifacts(page, testInfo.project.name === "iphone");
  await notes.getByRole("link", { name: "notes.md" }).click();
  await expect(page).toHaveURL(`/issues/${issue.key}/artifacts/notes-md`);
  const notesReferencedBy = page.getByRole("region", { name: "Referenced by" });
  await expect(notesReferencedBy).toContainText(`Comment · ${issue.key}`);
  await expect(notesReferencedBy).toContainText(
    `See dispatch://${issue.key}/artifact/notes-md@v1 before deciding.`
  );
  await navigateInApp(page, `/issues/${issue.key}`);
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
    await navigateInApp(page, `/issues/${issue.key}`);
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

// The From/To compare of a document shows what changed as the change view, word by word. It is
// seeded on a document that is not the issue's primary one, because `ArtifactRoutePanel` mounts
// `ArtifactDetails` - the version history and this compare - only off the primary document's own
// route (`isPrimaryDocumentArtifactRoute`, `features/refs/routes.ts`).
test("the version history compares two versions of a document with the change view", async ({
  page,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "# Lease handover\n\nThe spec itself is the primary document.\n",
    title: "Version compare",
  });
  const notes = await createIssueArtifact(issue.key, {
    content: "# Review notes\n\nThe ledger keeps three leases.\n",
    name: "notes.md",
  });
  await editArtifact(
    notes.artifact.id,
    {
      ops: [
        {
          find: "The ledger keeps three leases.",
          op: "replace",
          with: "The ledger keeps seven leases.",
        },
      ],
      summary: "Seven leases",
    },
    { as: "agent" }
  );

  await page.goto(`/issues/${issue.key}/artifacts/notes-md`);
  const compare = page.getByRole("region", { name: "Compare versions for notes.md" });
  await compare.getByRole("button", { name: "Show comparison" }).click();
  await compare.getByLabel("Compare notes.md from").selectOption("1");
  await compare.getByLabel("Compare notes.md to").selectOption("2");
  const changed = compare.getByTestId("change-view").locator('[data-change="changed"]');
  await expect(changed).toHaveCount(1);
  await expect(changed.locator("del")).toHaveText(["three"]);
  await expect(changed.locator("ins")).toHaveText(["seven"]);
});

test("the artifacts tab filter narrows rows by name and kind @phone", async ({
  page,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Artifact filtering" });
  await page.goto(`/issues/${issue.key}`);
  await openArtifacts(page, testInfo.project.name === "iphone");

  const upload = page.getByLabel("Upload artifact");
  await upload.setInputFiles(await diagramFile(testInfo));
  await confirmUpload(page);
  await expect(page.getByTestId("artifact-diagram-png")).toBeVisible();
  await upload.setInputFiles(await notesFile(testInfo));
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

// SLOW(>=2s): a real upload the live feed counts into the tab, and the keyboard tour of five
// tabs with each tab's box measured, at 390 px and back.
test("an uploaded document is linked from the Activity tab and counted on its own tab @phone", async ({
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

  // The live event feed refreshes the issue and its tabs: no reload.
  await expect(page.getByRole("tab", { exact: true, name: "Artifacts (2)" })).toBeVisible();
  const tablist = page.getByRole("tablist", { name: "Issue detail" });
  const tabs = tablist.getByRole("tab");
  await expect(tabs).toHaveCount(8);
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
  // Eight tabs, one of them counted, no longer fit a 390 px viewport: the tablist scrolls
  // sideways there (as it already did at 360 px). No label is shortened to buy the room, every
  // tab is reachable from the keyboard, each is scrolled fully into view as it takes focus, and
  // each keeps its 44 px touch target.
  const viewport = page.viewportSize();
  await page.setViewportSize({ height: viewport?.height ?? 800, width: 390 });
  await expect(tabs).toHaveText([
    "Spec",
    "Verification",
    "Plan",
    "Conversation",
    "Children",
    "Artifacts2",
    "Activity",
    "Decisions",
  ]);
  expect(await tablist.evaluate((node) => node.scrollWidth > node.clientWidth + 0.5)).toBe(true);
  await page.getByRole("tab", { name: "Spec" }).focus();
  for (const label of [
    "Verification",
    "Plan",
    "Conversation",
    "Children",
    "Artifacts (2)",
    "Activity",
    "Decisions",
  ] as const) {
    await page.keyboard.press("ArrowRight");
    const tab = page.getByRole("tab", { exact: true, name: label });
    await expect(tab).toBeFocused();
    await insideTablist(tab);
    expect((await tab.boundingBox())?.height ?? 0).toBeGreaterThanOrEqual(44);
  }
  await page.keyboard.press("Home");
  const specTab = page.getByRole("tab", { name: "Spec" });
  await expect(specTab).toBeFocused();
  await insideTablist(specTab);
  expect((await specTab.boundingBox())?.height ?? 0).toBeGreaterThanOrEqual(44);
  await page.setViewportSize({ height: viewport?.height ?? 800, width: viewport?.width ?? 1280 });

  await page.getByRole("tab", { name: "Activity" }).click();
  const rows = page.getByRole("list", { name: "Issue activity" }).getByRole("listitem");
  const added = rows.filter({ hasText: `added ${name}` });
  await expect(added).toHaveCount(1);
  await page.screenshot({ path: testInfo.outputPath("activity-added-artifact.png") });

  // A second upload under the same name is a new version; its line links that version.
  await createIssueArtifact(issue.key, { content: "# Draft\n\nRevised.\n", name });
  const saved = rows.filter({ hasText: `saved ${name} v2` });
  await expect(saved).toHaveCount(1);
  await expect(page.getByRole("tab", { exact: true, name: "Artifacts (2)" })).toBeVisible();
  await expect(saved.getByRole("link", { name })).toHaveAttribute(
    "href",
    `/issues/${issue.key}/artifacts/${upload.artifact.slug}?v=2`
  );

  // The upload is an Activity line and nothing else: the Conversation draws no row for it.
  await page.getByRole("tab", { name: "Conversation" }).click();
  await expect(
    page.getByRole("list", { name: "Conversation turns" }).getByText(`added ${name}`)
  ).toHaveCount(0);
  await page.getByRole("tab", { name: "Activity" }).click();

  await added.getByRole("link", { name }).click();
  await expect(page).toHaveURL(`/issues/${issue.key}/artifacts/${upload.artifact.slug}`);
  await expect(page.getByTestId("artifact-header")).toContainText(name);
});

test("the Artifacts badge counts the rows the tab lists, and a row's details are not truncated @phone", async ({
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

// The repair replaces the unreadable document whatever the picked file is called: a second
// document beside the broken one would leave it unreadable.
test("an out-of-schema document names its repair and uploads replacement markdown", async ({
  page,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Repair an unreadable document" });
  const upload = await createIssueArtifact(issue.key, {
    content: "before\n",
    name: "repair.md",
  });
  const corrupted = await postAsPage(
    page,
    `/api/v1/artifacts/${upload.artifact.id}/_test/outside-schema`
  );
  expect(corrupted.status()).toBe(204);

  await page.goto(`/issues/${issue.key}/artifacts/${upload.artifact.slug}`);
  await expect(page.getByText("replace the document from markdown to repair it")).toBeVisible();
  await page.getByLabel("Upload artifact").setInputFiles({
    buffer: Buffer.from("repaired\n"),
    mimeType: "text/markdown",
    name: "repair (1).md",
  });
  await confirmUpload(page);
  await expect(documentEditor(page)).toContainText("repaired");
  expect((await getArtifactText(upload.artifact.id)).markdown).toBe("repaired\n");
});

// The reader leaves the document and opens it again without a page load, so the page still holds
// the text it read on the first visit. The second admission reads the text afresh and shows the
// repair; connecting on the cached text would open a socket into a tree outside the schema. The
// count is this document's sockets alone: the issue's own spec holds a socket of its own.
test("an out-of-schema document does not reconnect from cached text @phone", async ({
  page,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Do not reconnect from stale text" });
  const upload = await createIssueArtifact(issue.key, {
    content: "before\n",
    name: "cached-repair.md",
  });
  const sockets = countDocumentSockets(page, upload.artifact.id);
  const row = page.getByTestId(`artifact-${upload.artifact.slug}`);
  const openDocument = () => row.getByRole("link", { name: "cached-repair.md" }).click();

  await page.goto(`/issues/${issue.key}`);
  await openArtifacts(page, testInfo.project.name === "iphone");
  // A page load would drop this marker, and the page's cached reads with it.
  await page.evaluate(() => Object.assign(window, { __e2eNavMarker: true }));
  await openDocument();
  await expect(documentEditor(page)).toContainText("before");
  await expect.poll(sockets).toBe(1);

  await page.goBack();
  await expect(row).toBeVisible();
  const corrupted = await postAsPage(
    page,
    `/api/v1/artifacts/${upload.artifact.id}/_test/outside-schema`
  );
  expect(corrupted.status()).toBe(204);
  await openDocument();

  await expect(page.getByText("replace the document from markdown to repair it")).toBeVisible();
  await expect(documentEditor(page)).toBeHidden();
  expect(await page.evaluate(() => "__e2eNavMarker" in window)).toBe(true);
  expect(sockets()).toBe(1);
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

  // The reconnect below waits a draw from [0, RECONNECT_SPREAD_MS) set at the previous open
  // (`web/src/features/doc/connection.ts`), which the page's own clock spends.
  await page.clock.install();
  await page.goto(`/issues/${issue.key}/artifacts/${upload.artifact.slug}`);
  await expect(documentEditor(page)).toContainText("before");
  await expect.poll(sockets).toBe(1);

  // The reconnect the drop starts is held, so it reaches the server only after the tree is made
  // invalid. A held socket opens no network connection, so it is counted once it is released.
  transport.hold();
  await transport.sever();
  const corrupted = await postAsPage(
    page,
    `/api/v1/artifacts/${upload.artifact.id}/_test/outside-schema`
  );
  expect(corrupted.status()).toBe(204);
  await transport.release();
  // Exactly the spread: a longer jump would arm the refusal's next re-admission and open a third
  // socket, which is the count this row reads.
  await page.clock.fastForward(RECONNECT_SPREAD_MS);

  await expect(page.getByText("replace the document from markdown to repair it")).toBeVisible();
  await expect(documentEditor(page)).toBeHidden();
  const text = await page.request.get(`/api/v1/artifacts/${upload.artifact.id}/text`);
  expect(text.status()).toBe(409);
  await expect(text.text()).resolves.toContain(`"code":"DOC_SCHEMA"`);
  expect(sockets()).toBe(2);
});

// A refetch of the document's text that fails says nothing about the editor already connected:
// an edit typed while its socket was down lives only in the editor, so an outage that answers the
// refetch 503 leaves the editor, and the edit reaches the server once the socket is back.
test("an edit typed while the socket was down survives a text refetch the server answers 503", async ({
  page,
}) => {
  const transport = await documentTransport(page);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Keep an offline edit" });
  const upload = await createIssueArtifact(issue.key, {
    content: "before\n",
    name: "offline-edit.md",
  });

  // Two page timers below are the page's, not the server's: the read's two retries
  // (`QUERY_RETRY_DELAY_MS`) and the reconnect draw from [0, RECONNECT_SPREAD_MS).
  await page.clock.install();
  await page.goto(`/issues/${issue.key}/artifacts/${upload.artifact.slug}`);
  await expect(documentEditor(page)).toContainText("before");

  transport.hold();
  await transport.sever();
  await typeAtEnd(page, "offline edit");
  let refused = 0;
  const textRoute = `**/api/v1/artifacts/${upload.artifact.id}/text`;
  await page.route(textRoute, async (route) => {
    refused += 1;
    await route.fulfill({
      body: JSON.stringify({
        code: "DOC_SERVICE_UNAVAILABLE",
        error: "document service is unavailable",
      }),
      contentType: "application/json",
      status: 503,
    });
  });
  // A comment anchored in the document names it, so its event refreshes the document's reads, its
  // text among them; the app retries a 5xx twice before it fails the read. A drop of the page's
  // live socket would not: Dispatch replays every event the page missed (`replay_complete`), so a
  // gap that replay covers refreshes nothing.
  await createComment(issue.key, {
    anchor: { artifact: upload.artifact.slug, quote: "before" },
    body: "On the text.",
  });
  // Each refusal arms the next retry, so the jump repeats until the read has been given up.
  await expect
    .poll(
      async () => {
        await page.clock.runFor(QUERY_RETRY_DELAY_MS + 50);
        return refused;
      },
      { intervals: [50, 50, 100, 200, 500] }
    )
    .toBeGreaterThanOrEqual(3);
  // The third answer fails the read; nothing on a page that keeps its editor marks that moment, so
  // the page is given a moment of real time to render the failure before what it shows is read.
  await sleep(250);
  await expect(documentEditor(page)).toBeVisible();
  await expect(documentEditor(page)).toContainText("offline edit");
  await expect(page.getByRole("button", { name: "Rebuild from the latest version" })).toHaveCount(
    0
  );
  await expect(page.getByText("This document could not load")).toHaveCount(0);

  await page.unroute(textRoute);
  await transport.release();
  await page.clock.fastForward(RECONNECT_SPREAD_MS);
  await expect
    .poll(async () => (await getArtifactText(upload.artifact.id)).markdown)
    .toBe("before\n\noffline edit\n");
  await expect(documentEditor(page)).toContainText("offline edit");
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
  const rebuilt = await postAsPage(page, `/api/v1/artifacts/${upload.artifact.id}/rebuild`, {});
  expect(rebuilt.status()).toBe(409);
  await expect(rebuilt.text()).resolves.toContain(`"code":"DOCUMENT_LIVE"`);
  await expect(documentEditor(page)).toContainText("before");
});

// INFRA-760 k11: every version the two artifact surfaces draw goes through one unit
// (`web/src/features/refs/ArtifactMedia.tsx`), which answers a version the browser refuses with
// a link to the artifact rather than the broken-image glyph those surfaces used to show, and
// draws a video artifact as a player on its own page while its Artifacts-tab row stays the text
// row it was. The Download controls are untouched by any of it.

/** A PNG signature with no picture behind it. Dispatch stores an upload by the bytes it sniffs,
 *  so this is an image artifact whose version the browser cannot decode: the state that used to
 *  leave a broken-image glyph on both surfaces. */
const brokenPng = "iVBORw0KGgpub3QgYSBwaWN0dXJlLCBvbmx5IGEgaGVhZGVy";

/** A 32x32 WebM of a third of a second, VP8 video and Vorbis audio. A non-picture upload is
 *  stored under the type its uploader declares, so this is a file artifact under `video/webm`. */
const tinyWebm = [
  "GkXfo59ChoEBQveBAULygQRC84EIQoKEd2VibUKHgQJChYECGFOAZwEAAAAAAAwFEU2bdLpNu4tTq4QVSalmU6yBoU27i1",
  "OrhBZUrmtTrIHWTbuMU6uEElTDZ1OsggpqTbuMU6uEHFO7a1Osggvv7AEAAAAAAABZAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
  "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAVSa",
  "lmsCrXsYMPQkBNgIxMYXZmNjEuMS4xMDBXQYxMYXZmNjEuMS4xMDBEiYhAdMAAAAAAABZUrmtJjq4BAAAAAAAAONeBAXPF",
  "iE+lHjqpG+5ZnIEAIrWcg3VuZIiBAIaFVl9WUDiDgQEj44OEBfXhAOCJsIEguoEgmoECrgEAAAAAAAlE14ECc8WIr/qx7T",
  "mCiBGcgQAitZyDdW5kiIEAhohBX1ZPUkJJU4OBAuGRn4EBtYhAv0AAAAAAAGJkgSBjokkFAh5cAXZvcmJpcwAAAAABQB8A",
  "AP////9AHwAA/////5kBA3ZvcmJpczQAAABYaXBoLk9yZyBsaWJWb3JiaXMgSSAyMDIwMDcwNCAoUmVkdWNpbmcgRW52aX",
  "Jvbm1lbnQpAQAAABQAAABlbmNvZGVyPUxhdmM2MS4zLjEwMAEFdm9yYmlzD0JDVgEAAAEADFIUISUZU0pjCJVSUikFHWNQ",
  "W0cdY9Q5RiFkEFOISRmle08qlVhKyBFSWClFHVNMU0mVUpYpRR1jFFNIIVPWMWWhcxRLhkkJJWxNrnQWS+iZY5YxRh1jzl",
  "pKnWPWMUUdY1JSSaFzGDpmJWQUOkbF6GJ8MDqVokIovsfeUukthYpbir3XGlPrLYQYS2nBCGFz7bXV3EpqxRhjjDHGxeJT",
  "KILQkFUAAAEAAEAEAUJDVgEACgAAwlAMRVGA0JBVAEAGAIAAFEVxFMdxHEeSJMsCQkNWAQBAAAACAAAojuEokiNJkmRZlm",
  "VZlqZ5lqi5qi/7ri7rru3qug6EhqwEAIAAABhGoXVMKoMQQ8pDhBRj0DOjEEMMTMYcY040pAwyiDPFkDKIWywuqBAEoSEr",
  "AoAoAADAGMQYYgw556RkUiLnmJROSgOdo9RRyiiVFEuMGaUSW4kxgs5R6ihllEKMpcWMUomxxFQAAECAAwBAgIVQaMiKAC",
  "AKAIAwBimFlEKMKeYUc4gx5RyDDDHGIGTOKegYk05K5ZyTzkmJGGPOMQeVc05K56Ry0EnJpBMAABDgAAAQYCEUGrIiAIgT",
  "ADBIkqZZmiaKlqaJomeKquqJoqpanmeanmmqqmeaqmqqquuaqurKlueZpmeaquqZpqqKpuq6pqq6rqeqtmy6qi6brmrbru",
  "z6tmu7vu6pqmybqivrpurKuurKtu66tu1Lnqeqoqq6rmeqrqu6rm6rrmvbmmrKrqm6sm26ri27smzrrizrumaqrmy6qi2b",
  "rizrruzatirLui+6rm6rsqz7qiz7vmzruu7auvCLrmvrqizrvirLvjHbtvDLui4ckyeqqqeqruuZquuqrmvbquvauqaarm",
  "y6ri2bqivbqizrtivLtq6ZqiybrivbpuvKsirLvu/Ksm6LruvrpizruirLwu7qujHMtq37ouvquirLuq/Ksq67uu77sm4L",
  "u6aqum7Ksq+bsqz7tq4Ly6zbujG6ru+rsi38qiwLv677wjLrPmN0XV9XbVkYVtn2fd33lWPWdWFZbVv5XVtnvL5uDLtu/M",
  "qtC8uy2raxzLqtLK+vG8Mu7Hxb+JWaqtq26bq6bsqyr8u6LrR13VdG1/V91bZ9X5Vl37eFX2kbw7CMrqv7qizrwmvLyi/r",
  "urDswi8sq20rv6vryjDburDcvrAsv+4Ly6rbvu/qutK1dWW5fZ+xK7fxCwAAGHAAAAgwoQwUGrIiAIgTAEAQcg4pBqFiCk",
  "IIoaQQQioVY1Iy5qRkzkkppZQUSkmtYkxK5pyUzDEpoZSWSimphFJaKqXEFEppLaXWYkqpxVBKa6Wk1kpJraWUYkytxRgx",
  "JiVzTkrmnJSSUmslldYy5yhlDkrqIKSUSiqtpNRi5pykDjorHYTUSioxlZRiC6nEVkpqraQUYysx1dRajiGlGEtKsZWUWm",
  "0x1dZaqzViTErmnJTMOSolpdZKKq1lzknqILTUOSippNRiKinFyjlJHYSUMsiolJRaK6nEElKJrbQUYympxdRirinFFkNJ",
  "LZaUWiypxNZijLW1VFMnpcWSUowllRhbrLm21moMpcRWSouxpJRbazHXFmOOoaQWSyuxlZRabLXl2FrLNbVUY0qt1hZjjT",
  "HllGutPafWYk0x1dparLnVllvMtedOSmullBZLSjG21mKNMeYcSmmtpBRbKSnG1lqtrcVcQymxldJaLKnE2GKstcVWY2qt",
  "xhZbraW1WmutvcZWWy6t1dxirD21lGusteZYU20FAAAMOAAABJhQBgoNWQkARAEAAMYwxhiERinHnJPSKOWcc1Iq5yCEkF",
  "LmHIQQUsqcg1BKS5lzEEpJKZSSUmqthVJSaq21AgAAChwAAAJs0JRYHKDQkJUAQCoAgMFxNE0UVdV1fV+xLFFUVdeVbeNX",
  "LE0UVVV2bVv4NVFUVde1bdsWfk0UVVV2Zdm2haKqurJt27JuC8Ooqq5r27Jt66iuq9u6rdu6L1RdWZZtW7d1Hde2dd32dV",
  "34GbNt67Zu677wIwxH3/gh5OP7dEIIAABPcAAAKrBhdYSTorHAQkNWAgAZAACAMUoZhRgzSDGmGGNMMcaYAACAAQcAgAAT",
  "ykChISsCgCgAAMA555xzzjnnnHPOOeecc84555xzjjHGGGOMMcYYY4wxxhhjjDHGGGOMMcYYY4wxxhhjTADAToQDwE6EhV",
  "BoyEoAIBwAAEAIISkppZRSShFTzkFJKaWUUqoUg4xKSimllFKkFHWUUkoppZQipaCkklJKKaWUSUkppZRSSimljDpKKaWU",
  "UkoppZRSSimllFJKKaWUUkoppZRSSimllFJKKaWUUkoppZRSSimllFJKKaWUUkoppZRSSimllFJKKaWUUkoppVRKKaWUUk",
  "oppZRSSimlFABg8uAAAJVg4wwrSWeFo8GFhqwEAHIDAACFGIMQQmmtpFRSSiVVzkEoJZSUSkoplZRSqpiDEEoqqaWSUkop",
  "tdJBKKGUUEopJZRSSiglhBBKCaGUVEIrqYRSSgehhBJCKaGEVEopJZTOQSghhQ5CSaWU1EJIHXRUUikhlVJKKSWllDoIoZ",
  "SSUkstlVJaSql0ElIpqZXUUmqptZJSCaGkVkpJJaXSWkkltRJKSSWllFJLKYVUUkklhJJSKiW1llpKqbXWUkiplZRSSqml",
  "1FJKJaWQSkqplJJSaiWVlFJqIZWUSkkppNRKKaWkVEJJqaVSWkottZRKSam0VFJJpZSUSkkppVJKSymlEkpKqaWUWkkphZ",
  "JSSimVklJLJbVUSgolpZRSSam0lFJLKZWSUgEAQAcOAAABRlRaiJ1mXHkEjihkmIACAABAAECACSAwQFAwCkGAMAIBAAAA",
  "AMAAAB8AAEcBEBHRnMEBQoLCAkODwwMAAAAAAAAAAAAAAIASVMNnQNVzc59jwIBnyJlFo4dFTkNPREVSRIeMTGF2ZjYxLj",
  "EuMTAwc3PVY8CLY8WIT6UeOqkb7llnyKBFo4dFTkNPREVSRIeTTGF2YzYxLjMuMTAwIGxpYnZweGfIoUWjiERVUkFUSU9O",
  "RIeTMDA6MDA6MDAuMzMyMDAwMDAwAHNz2GPAi2PFiK/6se05gogRZ8ijRaOHRU5DT0RFUkSHlkxhdmM2MS4zLjEwMCBsaW",
  "J2b3JiaXNnyKFFo4hEVVJBVElPTkSHkzAwOjAwOjAwLjMzMjAwMDAwMAAfQ7Z1QKTngQCjhYIAAIAAo6SBACCAMAIAnQEq",
  "IAAgAABHCIWFiIWEiAICAAeQ88nA/v+rUICjhYIAIIAAo4WCAECAAKOFggBggACjhYIAgIAAo5WBAIQAsQEAARAQABgAGF",
  "gv9AAIcACjhYIAoIAAo4WCAMCAAKOFggDggACjlYEA6ACxAQABEBAAGAAYWC/0AAhwAKOFggEAgACjhYIBIIAAo4WCAUCA",
  "ABxTu2uRu4+zgSC3iveBAfGCC0XwgQo=",
].join("");

/**
 * Uploads `base64` through the Artifacts tab's own picker, under `name` and `mimeType`, and
 * returns only once that version is stored and the picker has let go of the pick.
 *
 * Both waits are needed, and neither alone would do. The uploader clears its staged file in the
 * upload's own `onSuccess` (`web/src/features/artifacts/ArtifactUpload.tsx`), so a pick staged
 * while an earlier upload is still out is wiped under it, and the "Upload" press that follows
 * finds the idle picker's button of that same name and only reopens the chooser: the second
 * version is never sent. Waiting on the 201 alone leaves that window open, since the response
 * arrives before React has run `onSuccess`; waiting on the staged row's disappearance alone
 * would also answer to a "Cancel". So: the POST's own 201, which the server sends only after it
 * has stored the version, and then the staged row's "Summary (optional)" field gone, which only
 * the cleared pick removes.
 */
async function uploadBytes(
  page: Page,
  name: string,
  mimeType: string,
  base64: string
): Promise<void> {
  await page.getByLabel("Upload artifact").setInputFiles({
    buffer: Buffer.from(base64, "base64"),
    mimeType,
    name,
  });
  const stored = page.waitForResponse(
    (response) =>
      response.request().method() === "POST" &&
      new URL(response.url()).pathname.endsWith("/artifacts") &&
      response.status() === 201
  );
  await confirmUpload(page);
  await stored;
  await expect(page.getByLabel("Summary (optional)")).toHaveCount(0);
}

/** Fails unless nothing under `scope` reports a `naturalWidth` of zero, which is how a picture
 *  the browser refused shows itself. Each picture is scrolled to first, since they load lazily
 *  and one still waiting reports zero as a refused one does. */
async function expectNoBrokenPictures(scope: Locator): Promise<void> {
  for (const picture of await scope.locator("img").all()) {
    await picture.scrollIntoViewIfNeeded();
  }
  await expect
    .poll(() =>
      scope
        .locator("img")
        .evaluateAll(
          (images) =>
            images.filter((image) => (image as HTMLImageElement).naturalWidth === 0).length
        )
    )
    .toBe(0);
}

test("a version the browser cannot draw shows a link to the artifact on both surfaces, never a broken picture @phone", async ({
  page,
}, testInfo) => {
  const isPhone = testInfo.project.name === "iphone";
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "A version that cannot be drawn" });
  await page.goto(`/issues/${issue.key}`);
  await openArtifacts(page, isPhone);
  await uploadBytes(page, "broken.png", "image/png", brokenPng);
  await uploadBytes(page, "panel.png", "image/png", tinyPng);

  // The Artifacts tab: the refused square is gone, the row keeps its link to the artifact, and
  // no picture on the tab reports the zero width a broken glyph has.
  const brokenRow = page.getByTestId("artifact-broken-png");
  await expect(brokenRow.getByRole("link", { name: "broken.png" })).toHaveAttribute(
    "href",
    `/issues/${issue.key}/artifacts/broken-png`
  );
  await expect(brokenRow.locator("img")).toHaveCount(0);
  // The negative control: a version the browser does draw keeps its square on the same tab.
  await expect(page.getByTestId("artifact-panel-png").locator("img")).toHaveCount(1);
  await expectNoBrokenPictures(page.locator("body"));

  // The artifact's own page: the link in place of the picture, and no element left to be broken.
  await page.goto(`/issues/${issue.key}/artifacts/broken-png?v=1`);
  const shown = page.getByRole("region", { name: "broken.png version 1" });
  await expect(shown).toContainText("This version cannot be shown here.");
  await expect(shown.getByRole("link", { name: "broken.png" })).toHaveAttribute(
    "href",
    `/issues/${issue.key}/artifacts/broken-png`
  );
  await expect(shown.locator("img")).toHaveCount(0);
  await expectNoBrokenPictures(page.locator("body"));

  // The negative control: the page of a version the browser draws shows the picture and no link
  // in its place.
  await page.goto(`/issues/${issue.key}/artifacts/panel-png?v=1`);
  const picture = page.getByRole("region", { name: "panel.png version 1" });
  await expect(picture.getByRole("img", { name: "panel.png version 1" })).toBeVisible();
  await expect(picture).not.toContainText("This version cannot be shown here.");
  await expectNoBrokenPictures(page.locator("body"));
});

test("a video artifact plays on its own page and stays a text row on the Artifacts tab @phone", async ({
  page,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "A video artifact" });
  await page.goto(`/issues/${issue.key}`);
  await openArtifacts(page, testInfo.project.name === "iphone");
  await uploadBytes(page, "clip.webm", "video/webm", tinyWebm);

  // The row is the text row it was: no square, and no player either - a frame of a video is a
  // decode this app does not do.
  const row = page.getByTestId("artifact-clip-webm");
  await expect(row).toContainText("clip.webm");
  await expect(row).toContainText("1 version");
  await expect(row.locator("img")).toHaveCount(0);
  await expect(row.locator("video")).toHaveCount(0);

  await page.goto(`/issues/${issue.key}/artifacts/clip-webm?v=1`);
  const shown = page.getByRole("region", { name: "clip.webm version 1" });
  const player = shown.locator("video");
  await expect(player).toHaveCount(1);
  await expect(player).toHaveAttribute(
    "src",
    `/api/v1/issues/${issue.key}/artifacts/clip-webm/versions/1`
  );
  await expect(player).toHaveAttribute("preload", "metadata");
  expect(await player.evaluate((element: HTMLVideoElement) => element.controls)).toBe(true);
  await expect(shown).not.toContainText("This version cannot be shown here.");
});

test("each of the three Download controls still saves the version @phone", async ({
  page,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Downloads still download" });
  await page.goto(`/issues/${issue.key}`);
  await openArtifacts(page, testInfo.project.name === "iphone");
  await uploadBytes(page, "panel.png", "image/png", tinyPng);

  // The Artifacts-tab row's.
  const fromRow = page
    .getByTestId("artifact-panel-png")
    .getByRole("link", { name: "Download version 1" });
  const [rowDownload] = await Promise.all([page.waitForEvent("download"), fromRow.click()]);
  expect(rowDownload.suggestedFilename()).toBe("panel.png");

  await page.goto(`/issues/${issue.key}/artifacts/panel-png?v=1`);
  // The artifact page's own, beside the picture it draws.
  const fromPage = page
    .getByRole("region", { name: "panel.png version 1" })
    .getByRole("link", { name: "Download version 1" });
  const [pageDownload] = await Promise.all([page.waitForEvent("download"), fromPage.click()]);
  expect(pageDownload.suggestedFilename()).toBe("panel.png");

  // The version list's.
  await page.getByRole("button", { name: "Show all versions" }).click();
  const fromList = page
    .getByRole("region", { exact: true, name: "Versions for panel.png" })
    .getByRole("link", { name: "Download version 1" });
  const [listDownload] = await Promise.all([page.waitForEvent("download"), fromList.click()]);
  expect(listDownload.suggestedFilename()).toBe("panel.png");
});
