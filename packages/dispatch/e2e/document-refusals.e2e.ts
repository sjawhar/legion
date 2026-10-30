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
  placeCaret,
  typeAtEnd,
} from "./editor";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

// What a person sees when the document server refuses an edit: as too large for the document
// (DOCUMENT_TOO_LARGE_CLOSE_CODE, permanent: the edit is dropped), as one it could not merge into
// the document quickly enough (the same code with DOC_TOO_TANGLED, permanent too: the edit is
// dropped), as one the document cannot take until it is rebuilt (the same code with
// DOC_NEEDS_REBUILD: the edit is dropped and the rebuild is offered), or for want of room for open
// documents (DOCUMENT_BUSY_CLOSE_CODE, transient: the edit waits and saves once room frees).

const TOO_LARGE = "Your last edit was not saved: it would make this document larger";
const NEEDS_REBUILD =
  "This document has been edited so much that it must be rebuilt before it can take this edit. Your other edits are kept.";
const TOO_TANGLED =
  "Your last edit was not saved: Dispatch could not merge it into this document quickly enough. Your other edits are kept.";
const OUT_OF_ROOM = "Dispatch is out of room for open documents right now.";
const ROOM_BUDGET = "/api/v1/artifacts/_test/room-budget";
/** The sentence both browsers of the used-up row write after, the end of its paragraph's text. */
const MEETING_POINT = "Both browsers write after this sentence.";
/** What one browser types before its caret, each keystroke at the same place: digits in turn, so
 * no two neighbours are equal and the editor's diff inserts each where it was typed. A hundred of
 * them are a hundred items between the place and the end of its text, more than the 64 a used-up
 * room lets one browser's apply scan (usedUpScanBudget). */
const TYPED_IN_PLACE = Array.from({ length: 100 }, (_, index) => String(index % 10)).join("");
/** The paragraph the tangle row types, the end of the document's last text: its last character is
 * the left origin of both the paste that browser makes offline and the tangle chained on after
 * it, which is what makes merging the paste scan the whole tangle. */
const TANGLE_POINT = "This paragraph is where the tangle starts.";

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
    // applied: the browser keeps every one of them, the paste included, and is not closed. A
    // closed issue's read-only admission names no reload - a reload cannot make it editable - so
    // the editor itself is what says the admission landed (close-withdraws-asks.e2e.ts:158-167).
    await patchIssue(issue.key, { status: "done" });
    await transport.release();
    await expect(documentEditor(page)).toHaveAttribute("contenteditable", "false");
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

test("a used-up document refuses a larger merge as needing a rebuild, and saves after the rebuild it offers", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await createProject({ key: "USED", name: "Used-up documents" });
  const issue = await createIssue({
    project: "USED",
    spec: `${MEETING_POINT}\n\nThe tangle follows.\n`,
    title: "Rebuild a used-up document",
  });
  const artifactId = issue.primary_artifact_id;
  const alice = await asUser(browser, "alice");
  const bob = await asUser(browser, "bob");
  try {
    const offline = await alice.newPage();
    const transport = await documentTransport(offline);
    // The hook appends a tangle to the stored history whose re-scan uses up a room's scan total
    // (docs.Service.UseUpScanTotalForTest), at the end of the last paragraph: the room every
    // browser opens below is used up, as a real one is only past about fifteen honest long
    // sessions.
    const usedUp = await postAsPage(offline, `/api/v1/artifacts/${artifactId}/_test/used-up`);
    expect(usedUp.status(), await usedUp.text()).toBe(204);

    // Alice goes offline and pastes at the end of the meeting point's text: one edit, one saved
    // row, made while the server holds nothing after that place.
    await openSpec(offline, issue.key, MEETING_POINT);
    transport.hold();
    await transport.sever();
    await placeCaret(offline, "after", MEETING_POINT);
    await paste(offline, { html: "", text: "offline paste" });
    await expect.poll(() => pendingEditCount(offline, artifactId)).toBe(1);
    await offline.close();

    // Bob types in place there, each keystroke before the last: typing goes on in a used-up room.
    const typing = await bob.newPage();
    await openSpec(typing, issue.key, MEETING_POINT);
    await placeCaret(typing, "after", MEETING_POINT);
    for (const character of TYPED_IN_PLACE) {
      await typing.keyboard.type(character);
      await typing.keyboard.press("ArrowLeft");
    }
    const typedInPlace = [...TYPED_IN_PLACE].reverse().join("");
    await expect
      .poll(async () => (await getArtifactText(artifactId)).markdown)
      .toContain(`${MEETING_POINT}${typedInPlace}`);

    // Alice comes back: her saved paste merges past every one of Bob's keystrokes, a larger apply
    // than the used-up room takes, so it is dropped with its reason and the rebuild is offered.
    const page = await alice.newPage();
    const sockets = await recordDocumentSockets(page);
    await page.goto(`/issues/${issue.key}/spec`);
    await expect(page.getByText(NEEDS_REBUILD)).toBeVisible();
    const offer = page.getByRole("button", { name: "Rebuild this document" });
    await expect(offer).toBeVisible();
    await expect(connectedDot(page)).toHaveText("connected");
    // Bob's cursor label sits between the meeting point and his keystrokes in Alice's editor.
    await expect(documentEditor(page)).toContainText(typedInPlace);
    await expect(documentEditor(page)).not.toContainText("offline paste");
    await expect.poll(() => pendingEditCount(page, artifactId)).toBe(0);
    expect((await getArtifactText(artifactId)).markdown).not.toContain("offline paste");

    let confirmation: string | undefined;
    page.once("dialog", (dialog) => {
      confirmation = dialog.message();
      void dialog.accept();
    });
    await offer.click();
    await expect(page.getByText(NEEDS_REBUILD)).toHaveCount(0);
    expect(confirmation).toContain(
      "Every browser connected to it, this one included, drops the edits it has not sent yet."
    );
    await expect(connectedDot(page)).toHaveText("connected");
    await expect(documentEditor(page)).toContainText(typedInPlace);

    await typeAtEnd(page, "saved after the rebuild");
    await expect
      .poll(async () => (await getArtifactText(artifactId)).markdown)
      .toContain("saved after the rebuild");
    await expect.poll(() => pendingEditCount(page, artifactId)).toBe(0);
    // One refusal: the editor closed its connection before the rebuild, so it handed the replaced
    // history to no rebuilt room.
    expect((await sockets()).closes.filter((code) => code === 4413)).toHaveLength(1);
  } finally {
    await alice.close();
    await bob.close();
  }
});

test("a merge the server cannot scan through is dropped with its reason, and the next edit saves", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await createProject({ key: "TANGLE", name: "Tangled documents" });
  const issue = await createIssue({
    project: "TANGLE",
    spec: "# Tangled\n\nThe tangle follows the next paragraph.\n",
    title: "Refuse a merge that scans too far",
  });
  const artifactId = issue.primary_artifact_id;
  const alice = await asUser(browser, "alice");
  try {
    const offline = await alice.newPage();
    const transport = await documentTransport(offline);
    await openSpec(offline, issue.key, "The tangle follows the next paragraph.");
    await typeAtEnd(offline, TANGLE_POINT);
    await expect
      .poll(async () => (await getArtifactText(artifactId)).markdown)
      .toContain(TANGLE_POINT);
    await expect.poll(() => pendingEditCount(offline, artifactId)).toBe(0);

    // Alice goes offline and pastes at the end of what she typed: one edit, one saved row, made
    // while the server holds nothing after that place, so it names that place's last character as
    // its left origin and nothing as its right.
    transport.hold();
    await transport.sever();
    await placeCaret(offline, "after", TANGLE_POINT);
    await paste(offline, { html: "", text: "tangled paste" });
    await expect.poll(() => pendingEditCount(offline, artifactId)).toBe(1);

    // The hook chains tens of thousands of items onto the end of that text
    // (docs.Service.TangleBeyondOneApplyForTest), each naming the place Alice's paste names: a
    // real document is tangled that far only where as many items were made at one place while a
    // browser was away, which no browser test can type. Merging her paste scans the chain to its
    // end, past the items one apply of hers is allowed (27,528 of them for her 36-byte frame,
    // scanCeiling), while the room keeps 932,466 of its 1,000,000-item scan total, which is what
    // makes this refusal the too-tangled one and not a rebuild's.
    const tangled = await postAsPage(offline, `/api/v1/artifacts/${artifactId}/_test/tangled`);
    expect(tangled.status(), await tangled.text()).toBe(204);
    await offline.close();

    // Alice comes back: her saved paste is sent, refused whole, and dropped with its reason.
    const page = await alice.newPage();
    const sockets = await recordDocumentSockets(page);
    await page.goto(`/issues/${issue.key}/spec`);
    await expect(page.getByText(TOO_TANGLED)).toBeVisible();
    // Nothing to rebuild: the document is nowhere near a cap, only this one merge was too far.
    await expect(page.getByRole("button", { name: "Rebuild this document" })).toHaveCount(0);
    await expect(connectedDot(page)).toHaveText("connected");
    await expect(documentEditor(page)).toContainText(TANGLE_POINT);
    await expect(documentEditor(page)).not.toContainText("tangled paste");
    await expect.poll(() => pendingEditCount(page, artifactId)).toBe(0);
    const refused = (await getArtifactText(artifactId)).markdown;
    expect(refused).not.toContain("tangled paste");
    expect(refused).toContain(TANGLE_POINT);

    await typeAtEnd(page, "saved after the refusal");
    await expect
      .poll(async () => (await getArtifactText(artifactId)).markdown)
      .toContain("saved after the refusal");
    await expect.poll(() => pendingEditCount(page, artifactId)).toBe(0);
    // One refusal, and no reconnect that met it again.
    expect((await sockets()).closes.filter((code) => code === 4413)).toHaveLength(1);
    expect((await getArtifactText(artifactId)).markdown).not.toContain("tangled paste");
  } finally {
    await alice.close();
  }
});
