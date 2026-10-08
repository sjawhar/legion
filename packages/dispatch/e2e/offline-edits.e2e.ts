import { expect, test } from "@playwright/test";

import { createIssue, createProject, getArtifactText, patchIssue } from "./api";
import { connectedDot, documentTransport, typeAtEnd } from "./editor";
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
