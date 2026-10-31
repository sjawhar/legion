import { expect, test } from "@playwright/test";

import { RECONNECT_SPREAD_MS } from "../web/src/api/live-socket";
import { createIssue, createProject, getArtifactText, patchIssue } from "./api";
import { connectedDot, documentEditor, documentTransport, typeAtEnd } from "./editor";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

const INITIAL = "# Offline edits\n\nOriginal paragraph.\n";

/** Every fact this file polls for lands in milliseconds: Playwright's default 100/250/500/1000
 *  ladder would first see it a quarter-second late. */
const PROBE = { intervals: [50, 50, 100, 200, 500] };

/** Spends the document socket's reconnect draw - a uniform [0, RECONNECT_SPREAD_MS) set at the
 *  previous open (`web/src/features/doc/connection.ts`) - on the page's own clock. The close that
 *  arms the draw may not have reached the page when the first jump lands, so the jump repeats
 *  until the page is connected again. */
async function reconnectNow(page: Parameters<typeof documentTransport>[0]): Promise<void> {
  await expect
    .poll(async () => {
      await page.clock.fastForward(RECONNECT_SPREAD_MS);
      return connectedDot(page).count();
    }, PROBE)
    .toBe(1);
  await expect(connectedDot(page)).toHaveText("connected");
}

async function pendingEditCount(page: Parameters<typeof documentTransport>[0], artifactId: string) {
  return page.evaluate(async (id) => {
    const database = await new Promise<IDBDatabase | undefined>((resolve, reject) => {
      let request: IDBOpenDBRequest;
      try {
        request = indexedDB.open("dispatch-pending-edits");
      } catch {
        resolve(undefined);
        return;
      }
      request.onerror = () => reject(request.error);
      request.onsuccess = () => resolve(request.result);
    });
    if (database === undefined) {
      return 0;
    }
    try {
      if (!database.objectStoreNames.contains("edits")) {
        return 0;
      }
      return await new Promise<number>((resolve, reject) => {
        const request = database
          .transaction("edits", "readonly")
          .objectStore("edits")
          .count(IDBKeyRange.bound([id], [id, []]));
        request.onerror = () => reject(request.error);
        request.onsuccess = () => resolve(request.result);
      });
    } finally {
      database.close();
    }
  }, artifactId);
}

async function makeIssue() {
  await createProject({ key: "OFFLINE", name: "Offline edits" });
  return createIssue({ project: "OFFLINE", spec: INITIAL, title: "Persist local edits" });
}

async function waitForPersistedTyping(
  page: Parameters<typeof documentTransport>[0],
  artifactId: string,
  text: string
): Promise<void> {
  await expect
    .poll(() => pendingEditCount(page, artifactId), PROBE)
    .toBeGreaterThanOrEqual(text.length);
}

test.beforeEach(async () => {
  await resetDatabase();
});

test("an edit typed while the document transport is down survives a reload", async ({
  browser,
}) => {
  const issue = await makeIssue();
  const alice = await asUser(browser, "alice");
  const paragraph = "kept-through-reload";
  try {
    const page = await alice.newPage();
    const transport = await documentTransport(page);
    await page.goto(`/issues/${issue.key}/spec`);
    await expect(connectedDot(page)).toHaveText("connected");

    transport.hold();
    await transport.sever();
    await typeAtEnd(page, paragraph);
    await waitForPersistedTyping(page, issue.primary_artifact_id, paragraph);
    await page.reload();
    await transport.release();

    await expect
      .poll(async () => (await getArtifactText(issue.primary_artifact_id)).markdown, PROBE)
      .toContain(paragraph);
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): two document loads and the acknowledgement round trip, in three engines.
test("unsent edits are named, then cleared after acknowledgement and a connected reload", async ({
  browser,
}) => {
  const issue = await makeIssue();
  const alice = await asUser(browser, "alice");
  const paragraph = "waits for acknowledgement";
  try {
    const page = await alice.newPage();
    const transport = await documentTransport(page);
    // The reconnect draw below is spent on the page's clock, not in real time.
    await page.clock.install();
    await page.goto(`/issues/${issue.key}/spec`);
    await expect(connectedDot(page)).toHaveText("connected");

    transport.hold();
    await transport.sever();
    await typeAtEnd(page, paragraph);
    await expect(
      page.getByRole("status", { name: /edit(?:s)? saved in this browser, not sent yet/u })
    ).toBeVisible();
    await expect
      .poll(() => pendingEditCount(page, issue.primary_artifact_id), PROBE)
      .toBeGreaterThan(0);

    await transport.release();
    await reconnectNow(page);
    await expect.poll(() => pendingEditCount(page, issue.primary_artifact_id), PROBE).toBe(0);
    await page.reload();
    await expect(connectedDot(page)).toHaveText("connected");
    await expect.poll(() => pendingEditCount(page, issue.primary_artifact_id), PROBE).toBe(0);
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): four document loads across two tabs, each with a Yjs restore of its own.
test("two disconnected tabs restore each browser-held edit once", async ({ browser }) => {
  const issue = await makeIssue();
  const alice = await asUser(browser, "alice");
  const first = "first-offline-tab";
  const second = "second-offline-tab";
  try {
    const firstPage = await alice.newPage();
    const secondPage = await alice.newPage();
    const firstTransport = await documentTransport(firstPage);
    const secondTransport = await documentTransport(secondPage);
    await Promise.all([
      firstPage.goto(`/issues/${issue.key}/spec`),
      secondPage.goto(`/issues/${issue.key}/spec`),
    ]);
    await Promise.all([
      expect(connectedDot(firstPage)).toHaveText("connected"),
      expect(connectedDot(secondPage)).toHaveText("connected"),
    ]);

    firstTransport.hold();
    secondTransport.hold();
    await Promise.all([firstTransport.sever(), secondTransport.sever()]);
    await typeAtEnd(firstPage, first);
    await typeAtEnd(secondPage, second);
    await Promise.all([
      waitForPersistedTyping(firstPage, issue.primary_artifact_id, first),
      waitForPersistedTyping(secondPage, issue.primary_artifact_id, second),
    ]);
    await Promise.all([firstPage.reload(), secondPage.reload()]);
    await Promise.all([firstTransport.release(), secondTransport.release()]);

    // One read of the markdown for both tabs, which the count below reads once more.
    await expect
      .poll(async () => {
        const { markdown } = await getArtifactText(issue.primary_artifact_id);
        return markdown.includes(first) && markdown.includes(second);
      }, PROBE)
      .toBe(true);
    const stored = (await getArtifactText(issue.primary_artifact_id)).markdown;
    expect(stored.match(new RegExp(first, "gu"))?.length).toBe(1);
    expect(stored.match(new RegExp(second, "gu"))?.length).toBe(1);
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): three document loads, two status writes and two admissions decided at socket open.
test("read-only admission preserves edits until a later read-write reload", async ({ browser }) => {
  const issue = await makeIssue();
  const alice = await asUser(browser, "alice");
  const paragraph = "survives a read-only admission";
  try {
    const page = await alice.newPage();
    const transport = await documentTransport(page);
    await page.goto(`/issues/${issue.key}/spec`);
    await expect(connectedDot(page)).toHaveText("connected");

    transport.hold();
    await transport.sever();
    await typeAtEnd(page, paragraph);
    await waitForPersistedTyping(page, issue.primary_artifact_id, paragraph);
    await patchIssue(issue.key, { status: "done" });
    await page.reload();
    await transport.release();
    await expect(
      page.getByRole("status", { name: /can't be sent: this document is read-only/u })
    ).toBeVisible();
    expect((await getArtifactText(issue.primary_artifact_id)).markdown).not.toContain(paragraph);

    await patchIssue(issue.key, { status: "backlog" });
    await page.reload();
    await expect
      .poll(async () => (await getArtifactText(issue.primary_artifact_id)).markdown, PROBE)
      .toContain(paragraph);
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): two document loads, a reopen, the reconnect's SyncStep2 and a restore.
test("a reopened issue's page names no unsent edit before typing, and an edit typed after its reconnect survives a reload", async ({
  browser,
}) => {
  // The spec opens with a heading: the read-only page's editor stamps the heading's id as it loads,
  // an update of this page's own client that the read-only server refuses. The reconnect after the
  // reopen sends it in its SyncStep2, and what the person types later continues that client's
  // clock, so its rows apply on a reload only because the server holds the stamp.
  const issue = await makeIssue();
  await patchIssue(issue.key, { status: "done" });
  const alice = await asUser(browser, "alice");
  const paragraph = "typed after the reopened reconnect";
  const unsentNotice = /saved in this browser/u;
  try {
    const page = await alice.newPage();
    const transport = await documentTransport(page);
    // The reconnect draw the reopen below arms is spent on the page's clock, not in real time.
    await page.clock.install();
    await page.goto(`/issues/${issue.key}/spec`);
    await expect(connectedDot(page)).toHaveText("connected");
    const editor = documentEditor(page);
    await expect(editor).toHaveAttribute("contenteditable", "false");
    await expect(page.getByRole("status", { name: unsentNotice })).toHaveCount(0);
    expect(await pendingEditCount(page, issue.primary_artifact_id)).toBe(0);

    // The issue reopens while the page is open. The reopen drops the document's socket; the
    // reconnect syncs and is then admitted writable, as the server orders it. The document
    // socket's first reconnect is a uniform draw from [0, 2 s) (#2323), which `reconnectNow`
    // spends on the page's clock: the row waits for the state the page settles in either way and
    // never depends on which side of the draw a run lands.
    await patchIssue(issue.key, { status: "backlog" });
    await expect(page.getByText("This issue is closed. Its document is read-only.")).toBeHidden();
    await reconnectNow(page);
    await expect(editor).toHaveAttribute("contenteditable", "true");
    await expect.poll(() => pendingEditCount(page, issue.primary_artifact_id), PROBE).toBe(0);

    // The connection is severed and its reconnect held. A page that is not connected names every
    // row this browser holds, so a row the reconnect left would show here.
    transport.hold();
    await transport.sever();
    await expect(connectedDot(page)).toHaveCount(0);
    await expect(page.getByRole("status", { name: unsentNotice })).toHaveCount(0);
    expect(await pendingEditCount(page, issue.primary_artifact_id)).toBe(0);

    // The typing waits in this browser while the reconnect is held.
    await typeAtEnd(page, paragraph);
    await waitForPersistedTyping(page, issue.primary_artifact_id, paragraph);
    await page.reload();
    await expect(
      page.getByRole("status", { name: /edit(?:s)? saved in this browser, not sent yet/u })
    ).toBeVisible();
    await transport.release();
    await expect
      .poll(async () => (await getArtifactText(issue.primary_artifact_id)).markdown, PROBE)
      .toContain(paragraph);
    await expect.poll(() => pendingEditCount(page, issue.primary_artifact_id), PROBE).toBe(0);
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): two document loads and the apply-before-acknowledgement round trip.
test("an edit applied before its acknowledgement clears its row on reload", async ({ browser }) => {
  const issue = await makeIssue();
  const alice = await asUser(browser, "alice");
  const paragraph = "applied before acknowledgement";
  try {
    const page = await alice.newPage();
    const transport = await documentTransport(page);
    await page.goto(`/issues/${issue.key}/spec`);
    await expect(connectedDot(page)).toHaveText("connected");

    transport.holdServerFrames();
    await typeAtEnd(page, paragraph);
    await expect
      .poll(async () => (await getArtifactText(issue.primary_artifact_id)).markdown, PROBE)
      .toContain(paragraph);
    await expect
      .poll(() => pendingEditCount(page, issue.primary_artifact_id), PROBE)
      .toBeGreaterThan(0);

    await page.reload();
    await expect.poll(() => pendingEditCount(page, issue.primary_artifact_id), PROBE).toBe(0);
    await transport.releaseServerFrames();
    expect((await getArtifactText(issue.primary_artifact_id)).markdown).toContain(paragraph);
  } finally {
    await alice.close();
  }
});
