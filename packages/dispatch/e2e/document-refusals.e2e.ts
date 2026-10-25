import { expect, type Page, test } from "@playwright/test";

import {
  createIssue,
  createIssueArtifact,
  createProject,
  dashboardOrigin,
  getArtifactText,
  patchIssue,
  quiesceDocuments,
} from "./api";
import {
  connectedDot,
  countDocumentSockets,
  documentEditor,
  documentTransport,
  openSpec,
  paste,
  typeAtEnd,
} from "./editor";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

// What a person sees when the document server refuses an edit: as too large for the document
// (DOCUMENT_TOO_LARGE_CLOSE_CODE, permanent: the edit is dropped), or for want of room for open
// documents (DOCUMENT_BUSY_CLOSE_CODE, transient: the edit waits and saves once room frees).

const TOO_LARGE = "Your last edit was not saved: it would make this document larger";
const OUT_OF_ROOM = "Dispatch is out of room for open documents right now.";
const ROOM_BUDGET = "/api/v1/artifacts/_test/room-budget";

/** The most markdown one document may hold (pmdoc.MaxDocumentBytes). */
const MAX_DOCUMENT_BYTES = 1 << 20;
/** More than the 8 KiB the server admits into a document at its bound (maxPeerInsertAtTheBound). */
const REFUSED_PASTE = `refused-paste ${"x".repeat(10_000)}`;
/** A row that opens a document at the bound loads and syncs a mebibyte of it, more than once, so
 * it runs past the suite's 30-second default: about 17 and 24 seconds here in Chromium. */
const AT_THE_BOUND_TIMEOUT_MS = 90_000;

/** Markdown about 2 KiB under the document bound, in plain paragraphs: a paste of `REFUSED_PASTE`
 * takes it past the bound, and a keystroke does not. */
function markdownAtTheBound(): string {
  const paragraph = `${"Prose at the document bound.".repeat(36)}\n\n`;
  let markdown = "# At the bound\n\n";
  while (markdown.length + paragraph.length <= MAX_DOCUMENT_BYTES - 2048) {
    markdown += paragraph;
  }
  return markdown;
}

async function documentAtTheBound(title: string) {
  await createProject({ key: "BOUND", name: "Document bounds" });
  const issue = await createIssue({ project: "BOUND", title });
  const { artifact } = await createIssueArtifact(issue.key, {
    content: markdownAtTheBound(),
    name: "at-the-bound.md",
  });
  return { artifact, issue, path: `/issues/${issue.key}/artifacts/${artifact.slug}` };
}

async function pendingEditCount(page: Page, artifactId: string): Promise<number> {
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

interface DocumentSockets {
  /** The close code of every document socket the page saw close, in order. */
  closes: number[];
  /** The flag of every SyncStatus frame a document socket received: 1 applied, 0 not. */
  statuses: number[];
}

declare global {
  interface Window {
    documentSockets?: DocumentSockets;
  }
}

/** Records, inside every page `page` loads, what its document sockets were told: the close codes
 * the server closed them with and the SyncStatus answers it gave. Install before navigating. */
async function recordDocumentSockets(page: Page): Promise<() => Promise<DocumentSockets>> {
  await page.addInitScript(() => {
    const record: DocumentSockets = { closes: [], statuses: [] };
    window.documentSockets = record;
    const readVarUint = (bytes: Uint8Array, at: { pos: number }): number => {
      let value = 0;
      for (let shift = 0; ; shift += 7) {
        const byte = bytes[at.pos] ?? 0;
        at.pos += 1;
        value += (byte & 0x7f) * 2 ** shift;
        if (byte < 0x80) {
          return value;
        }
      }
    };
    const Native = window.WebSocket;
    window.WebSocket = class extends Native {
      constructor(url: string | URL, protocols?: string | string[]) {
        super(url, protocols);
        if (!new URL(String(url), window.location.href).pathname.startsWith("/ws/doc/")) {
          return;
        }
        this.addEventListener("close", (event) => record.closes.push(event.code));
        this.addEventListener("message", (event) => {
          if (!(event.data instanceof ArrayBuffer)) {
            return;
          }
          const bytes = new Uint8Array(event.data);
          const at = { pos: 0 };
          const nameLength = readVarUint(bytes, at);
          at.pos += nameLength; // the document name
          // Tag 8 is Hocuspocus's SyncStatus, the server's answer to each frame it was sent.
          if (readVarUint(bytes, at) === 8) {
            record.statuses.push(readVarUint(bytes, at));
          }
        });
      }
    };
  });
  return async () => {
    const record = await page.evaluate(() => window.documentSockets);
    if (record === undefined) {
      throw new Error("the page loaded before recordDocumentSockets installed its script");
    }
    return record;
  };
}

// `page.request` sends the page's session cookie and no `Origin`, and the server refuses a
// cookie-authenticated write whose `Origin` is not the dashboard's (enforceCookieOrigin).
function postAsPage(page: Page, path: string, data?: object) {
  return page.request.post(path, { data, headers: { Origin: dashboardOrigin } });
}

test.beforeEach(async () => {
  await resetDatabase();
});

test("an edit past the document bound is dropped with its reason, and the next edit saves", async ({
  browser,
}) => {
  test.setTimeout(AT_THE_BOUND_TIMEOUT_MS);
  const { artifact, path } = await documentAtTheBound("Refuse a paste past the bound");
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const sockets = await recordDocumentSockets(page);
    await page.goto(path);
    await expect(documentEditor(page)).toContainText("Prose at the document bound.");
    await expect(connectedDot(page)).toHaveText("connected");

    await typeAtEnd(page, "kept before the paste");
    await expect
      .poll(async () => (await getArtifactText(artifact.id)).markdown)
      .toContain("kept before the paste");
    await paste(page, { html: "", text: REFUSED_PASTE });

    await expect(page.getByText(TOO_LARGE)).toBeVisible();
    await expect(connectedDot(page)).toHaveText("connected");
    await expect(documentEditor(page)).toContainText("kept before the paste");
    await expect(documentEditor(page)).not.toContainText("refused-paste");
    await expect.poll(() => pendingEditCount(page, artifact.id)).toBe(0);
    expect((await getArtifactText(artifact.id)).markdown).not.toContain("refused-paste");

    await typeAtEnd(page, "saved after the refusal");
    await expect
      .poll(async () => (await getArtifactText(artifact.id)).markdown)
      .toContain("saved after the refusal");
    await expect.poll(() => pendingEditCount(page, artifact.id)).toBe(0);
    // One refusal, and no reconnect that met it again.
    expect((await sockets()).closes.filter((code) => code === 4413)).toHaveLength(1);
    expect((await getArtifactText(artifact.id)).markdown).not.toContain("refused-paste");
  } finally {
    await alice.close();
  }
});

test("a read-only viewer's saved edits are held through a not-applied answer, then sent", async ({
  browser,
}) => {
  test.setTimeout(AT_THE_BOUND_TIMEOUT_MS);
  const { artifact, issue, path } = await documentAtTheBound("Hold edits while read-only");
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const sockets = await recordDocumentSockets(page);
    const transport = await documentTransport(page);
    await page.goto(path);
    await expect(documentEditor(page)).toContainText("Prose at the document bound.");
    await expect(connectedDot(page)).toHaveText("connected");

    transport.hold();
    await transport.sever();
    await typeAtEnd(page, "held while read-only");
    await paste(page, { html: "", text: REFUSED_PASTE });
    await expect
      .poll(() => pendingEditCount(page, artifact.id))
      .toBeGreaterThan("held while read-only".length);
    const held = await pendingEditCount(page, artifact.id);

    // The reconnect is admitted read-only, and the server answers the edits it carries as not
    // applied: the browser keeps every one of them, the paste included, and is not closed.
    await patchIssue(issue.key, { status: "done" });
    await transport.release();
    await expect(page.getByText("Reload to edit.")).toBeVisible();
    await expect.poll(async () => (await sockets()).statuses).toContain(0);
    expect(await pendingEditCount(page, artifact.id)).toBe(held);
    expect((await sockets()).closes).not.toContain(4413);
    const readOnlyText = (await getArtifactText(artifact.id)).markdown;
    expect(readOnlyText).not.toContain("held while read-only");
    expect(readOnlyText).not.toContain("refused-paste");

    // Read-write again, the edits are sent: the paste past the bound is dropped with its reason,
    // and the rest saves.
    await patchIssue(issue.key, { status: "backlog" });
    await page.reload();
    await expect(page.getByText(TOO_LARGE)).toBeVisible();
    await expect
      .poll(async () => (await getArtifactText(artifact.id)).markdown)
      .toContain("held while read-only");
    await expect.poll(() => pendingEditCount(page, artifact.id)).toBe(0);
    await expect(connectedDot(page)).toHaveText("connected");
    expect((await getArtifactText(artifact.id)).markdown).not.toContain("refused-paste");
    expect((await sockets()).closes.filter((code) => code === 4413)).toHaveLength(1);
  } finally {
    await alice.close();
  }
});

test("a full room budget turns a document away and holds an editor's typing until room frees", async ({
  browser,
}) => {
  // Each refused attempt waits longer than the last (the provider's backoff, up to 30 seconds),
  // so the attempt that finds room can come that long after room frees.
  test.setTimeout(150_000);
  await createProject({ key: "ROOM", name: "Room budget" });
  const editing = await createIssue({
    project: "ROOM",
    spec: "# Open first\n\nThe document already open.\n",
    title: "Already editing",
  });
  const waiting = await createIssue({
    project: "ROOM",
    spec: "# Opened second\n\nThe document asked for after.\n",
    title: "Turned away",
  });
  // No room is resident but the one the editor below opens: a resident room is joined at no cost
  // whatever the budget.
  await quiesceDocuments();
  const alice = await asUser(browser, "alice");
  const page = await alice.newPage();
  try {
    await openSpec(page, editing.key, "The document already open.");
    // A budget of one byte: the open room is in use, so nothing can be closed.
    const full = await postAsPage(page, ROOM_BUDGET, { bytes: 1 });
    expect(full.status(), await full.text()).toBe(200);

    const turnedAway = await alice.newPage();
    const attempts = countDocumentSockets(turnedAway, waiting.primary_artifact_id);
    await turnedAway.goto(`/issues/${waiting.key}/spec`);
    await expect(turnedAway.getByText(OUT_OF_ROOM)).toBeVisible();
    await expect.poll(attempts).toBeGreaterThanOrEqual(2);
    await expect(turnedAway.getByText(/reload the page/u)).toHaveCount(0);

    await typeAtEnd(page, "typed while out of room");
    await expect(page.getByText(OUT_OF_ROOM)).toBeVisible();
    await expect(documentEditor(page)).toContainText("typed while out of room");
    await expect.poll(() => pendingEditCount(page, editing.primary_artifact_id)).toBeGreaterThan(0);
    expect((await getArtifactText(editing.primary_artifact_id)).markdown).not.toContain(
      "typed while out of room"
    );

    const freed = await postAsPage(page, ROOM_BUDGET, { bytes: 0 });
    expect(freed.status(), await freed.text()).toBe(200);
    await expect
      .poll(async () => (await getArtifactText(editing.primary_artifact_id)).markdown, {
        timeout: 60_000,
      })
      .toContain("typed while out of room");
    await expect(page.getByText(OUT_OF_ROOM)).toHaveCount(0);
    await expect.poll(() => pendingEditCount(page, editing.primary_artifact_id)).toBe(0);
    await expect(connectedDot(turnedAway)).toHaveText("connected", { timeout: 60_000 });
    await expect(documentEditor(turnedAway)).toContainText("The document asked for after.");
    await expect(turnedAway.getByText(OUT_OF_ROOM)).toHaveCount(0);
  } finally {
    // The budget is the server's, and every later row shares it.
    await postAsPage(page, ROOM_BUDGET, { bytes: 0 });
    await alice.close();
  }
});
