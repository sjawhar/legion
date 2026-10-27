import { expect, test } from "@playwright/test";

import { createIssue, createProject, getArtifactText, patchIssue } from "./api";
import { connectedDot, documentEditor, documentTransport, typeAtEnd } from "./editor";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

const INITIAL = "# Offline edits\n\nOriginal paragraph.\n";

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
  await expect.poll(() => pendingEditCount(page, artifactId)).toBeGreaterThanOrEqual(text.length);
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
      .poll(async () => (await getArtifactText(issue.primary_artifact_id)).markdown)
      .toContain(paragraph);
  } finally {
    await alice.close();
  }
});

test("unsent edits are named, then cleared after acknowledgement and a connected reload", async ({
  browser,
}) => {
  const issue = await makeIssue();
  const alice = await asUser(browser, "alice");
  const paragraph = "waits for acknowledgement";
  try {
    const page = await alice.newPage();
    const transport = await documentTransport(page);
    await page.goto(`/issues/${issue.key}/spec`);
    await expect(connectedDot(page)).toHaveText("connected");

    transport.hold();
    await transport.sever();
    await typeAtEnd(page, paragraph);
    await expect(
      page.getByRole("status", { name: /edit(?:s)? saved in this browser, not sent yet/u })
    ).toBeVisible();
    await expect.poll(() => pendingEditCount(page, issue.primary_artifact_id)).toBeGreaterThan(0);

    await transport.release();
    await expect(connectedDot(page)).toHaveText("connected");
    await expect.poll(() => pendingEditCount(page, issue.primary_artifact_id)).toBe(0);
    await page.reload();
    await expect(connectedDot(page)).toHaveText("connected");
    await expect.poll(() => pendingEditCount(page, issue.primary_artifact_id)).toBe(0);
  } finally {
    await alice.close();
  }
});

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

    await expect
      .poll(async () => (await getArtifactText(issue.primary_artifact_id)).markdown)
      .toContain(first);
    await expect
      .poll(async () => (await getArtifactText(issue.primary_artifact_id)).markdown)
      .toContain(second);
    const stored = (await getArtifactText(issue.primary_artifact_id)).markdown;
    expect(stored.match(new RegExp(first, "gu"))?.length).toBe(1);
    expect(stored.match(new RegExp(second, "gu"))?.length).toBe(1);
  } finally {
    await alice.close();
  }
});

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
      .poll(async () => (await getArtifactText(issue.primary_artifact_id)).markdown)
      .toContain(paragraph);
  } finally {
    await alice.close();
  }
});

test("an edit typed after a reopened issue's reconnect authenticates and before it syncs survives a reload", async ({
  browser,
}) => {
  // The spec opens with a heading: the read-only page's editor stamps the heading's id as it loads,
  // an update of this page's own client that the server refuses, and the typing that follows
  // continues that client's clock, so its rows apply on a reload only after the stamp's.
  const issue = await makeIssue();
  await patchIssue(issue.key, { status: "done" });
  const alice = await asUser(browser, "alice");
  const paragraph = "typed before the reconnect syncs";
  try {
    const page = await alice.newPage();
    const transport = await documentTransport(page);
    await page.goto(`/issues/${issue.key}/spec`);
    await expect(connectedDot(page)).toHaveText("connected");
    const editor = documentEditor(page);
    await expect(editor).toHaveAttribute("contenteditable", "false");

    // The issue reopens while the page is open. Its socket stays admitted read-only until the
    // connection drops; the reconnect is admitted writable and, held here, never syncs.
    await patchIssue(issue.key, { status: "backlog" });
    await expect(page.getByText("This issue is closed. Its document is read-only.")).toBeHidden();
    transport.admitWithoutSync();
    await transport.sever();
    await expect(editor).toHaveAttribute("contenteditable", "true");
    await typeAtEnd(page, paragraph);
    await waitForPersistedTyping(page, issue.primary_artifact_id, paragraph);

    transport.hold();
    await transport.sever();
    await page.reload();
    await expect(
      page.getByRole("status", { name: /edit(?:s)? saved in this browser, not sent yet/u })
    ).toBeVisible();
    await transport.release();
    await expect
      .poll(async () => (await getArtifactText(issue.primary_artifact_id)).markdown)
      .toContain(paragraph);
  } finally {
    await alice.close();
  }
});

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
      .poll(async () => (await getArtifactText(issue.primary_artifact_id)).markdown)
      .toContain(paragraph);
    await expect.poll(() => pendingEditCount(page, issue.primary_artifact_id)).toBeGreaterThan(0);

    await page.reload();
    await expect.poll(() => pendingEditCount(page, issue.primary_artifact_id)).toBe(0);
    await transport.releaseServerFrames();
    expect((await getArtifactText(issue.primary_artifact_id)).markdown).toContain(paragraph);
  } finally {
    await alice.close();
  }
});
