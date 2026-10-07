import { afterEach, expect, spyOn, test } from "bun:test";
import * as Y from "yjs";

import { deletePendingEdits, openPendingEdits, type PendingEdits } from "./pending-edits";

// Each test names its own artifact: fake-indexeddb keeps one database for the whole test process.
let next = 0;
function artifactId(): string {
  next += 1;
  return `pending-edits-test-${next}`;
}

const opened: PendingEdits[] = [];
afterEach(() => {
  for (const store of opened.splice(0)) {
    store.close();
  }
});

async function open(artifact: string, clientId: number): Promise<PendingEdits> {
  const store = await openPendingEdits(artifact, clientId);
  if (store === undefined) {
    throw new Error("fake-indexeddb refused to open");
  }
  opened.push(store);
  return store;
}

/** The updates a document emits for `texts`, typed one after another into its "t" text. */
function edits(clientId: number, texts: string[]): Uint8Array[] {
  const doc = new Y.Doc();
  doc.clientID = clientId;
  const updates: Uint8Array[] = [];
  doc.on("update", (update: Uint8Array) => updates.push(update));
  for (const text of texts) {
    doc.getText("t").insert(doc.getText("t").length, text);
  }
  return updates;
}

function edit(clientId: number, text: string): Uint8Array {
  const [update] = edits(clientId, [text]);
  if (update === undefined) {
    throw new Error("an insert emits an update");
  }
  return update;
}

function textOf(update: Uint8Array): string {
  const doc = new Y.Doc();
  Y.applyUpdate(doc, update);
  return doc.getText("t").toString();
}

test("two clients on one document keep their own rows, under the same sequence numbers", async () => {
  const artifact = artifactId();
  const first = await open(artifact, 1);
  const second = await open(artifact, 2);

  const recordedA = first.record(edit(1, "a"));
  const recordedB = second.record(edit(2, "b"));
  await Promise.all([recordedA.written, recordedB.written]);

  expect([recordedA.seq, recordedB.seq]).toEqual([1, 1]);
  expect(await first.count()).toBe(2);
  await first.clear(1);
  expect(await second.count()).toBe(1);
  expect(textOf((await second.restore())?.update ?? new Uint8Array())).toBe("b");
});

test("clear deletes this client's row at exactly that sequence number", async () => {
  const artifact = artifactId();
  const store = await open(artifact, 7);
  for (const update of edits(7, ["a", "b", "c"])) {
    store.record(update);
  }

  await store.clear(2);

  expect((await store.restore())?.keys).toEqual([
    [artifact, 7, 1],
    [artifact, 7, 3],
  ]);
});

test("clearThrough leaves a row recorded after the clear was queued", async () => {
  const artifact = artifactId();
  const store = await open(artifact, 9);
  const [a, b, c] = edits(9, ["a", "b", "c"]);
  store.record(a ?? new Uint8Array());
  store.record(b ?? new Uint8Array());
  // Not awaited: IndexedDB runs transactions on one store in the order they were created, so the
  // clear runs before the append queued after it, whichever the caller waits for first.
  const cleared = store.clearThrough(2);
  const third = store.record(c ?? new Uint8Array());
  await Promise.all([cleared, third.written]);

  expect(third.seq).toBe(3);
  expect((await store.restore())?.keys).toEqual([[artifact, 9, 3]]);
});

test("restore merges every client's rows for the document, with the keys it read", async () => {
  const artifact = artifactId();
  const first = await open(artifact, 11);
  const second = await open(artifact, 12);
  const other = await open(artifactId(), 11);
  await Promise.all([
    ...edits(11, ["one ", "two "]).map((update) => first.record(update).written),
    second.record(edit(12, "three")).written,
    other.record(edit(11, "elsewhere")).written,
  ]);

  const restored = await first.restore();

  expect(restored?.keys).toEqual([
    [artifact, 11, 1],
    [artifact, 11, 2],
    [artifact, 12, 1],
  ]);
  const merged = textOf(restored?.update ?? new Uint8Array());
  expect(merged).toContain("one two ");
  expect(merged).toContain("three");
  expect(merged).not.toContain("elsewhere");
  expect(restored?.at).toBeLessThanOrEqual(Date.now());
});

test("restore of a document with no rows answers undefined", async () => {
  const store = await open(artifactId(), 3);
  expect(await store.restore()).toBeUndefined();
});

test("drop deletes exactly the keys it is given", async () => {
  const artifact = artifactId();
  const first = await open(artifact, 21);
  const second = await open(artifact, 22);
  await Promise.all([
    ...edits(21, ["a", "b"]).map((update) => first.record(update).written),
    second.record(edit(22, "c")).written,
  ]);

  await first.drop([
    [artifact, 21, 1],
    [artifact, 22, 1],
  ]);

  expect((await first.restore())?.keys).toEqual([[artifact, 21, 2]]);
});

test("a client reopened on a document continues after its last sequence number", async () => {
  const artifact = artifactId();
  const before = await open(artifact, 31);
  await Promise.all(edits(31, ["a", "b"]).map((update) => before.record(update).written));

  const after = await open(artifact, 31);

  expect(after.record(edit(31, "c")).seq).toBe(3);
});

test("deleting a document's edits removes every client's rows and no other document's", async () => {
  const artifact = artifactId();
  const elsewhere = artifactId();
  const first = await open(artifact, 41);
  const second = await open(artifact, 42);
  const other = await open(elsewhere, 41);
  await Promise.all([
    first.record(edit(41, "a")).written,
    second.record(edit(42, "b")).written,
    other.record(edit(41, "c")).written,
  ]);

  await deletePendingEdits(artifact);

  expect(await first.count()).toBe(0);
  expect(await other.count()).toBe(1);
});

test("an IndexedDB that throws on open leaves the browser keeping no edits, and says so", async () => {
  const real = globalThis.indexedDB;
  const refusing = {
    open() {
      throw new DOMException("The user denied permission.", "SecurityError");
    },
  };
  const logged = spyOn(console, "error").mockImplementation(() => {});
  Object.defineProperty(globalThis, "indexedDB", { configurable: true, value: refusing });
  try {
    expect(await openPendingEdits(artifactId(), 1)).toBeUndefined();
    expect(logged).toHaveBeenCalledTimes(1);
  } finally {
    Object.defineProperty(globalThis, "indexedDB", { configurable: true, value: real });
    logged.mockRestore();
  }
});

test("a browser without IndexedDB keeps no edits", async () => {
  const real = globalThis.indexedDB;
  Object.defineProperty(globalThis, "indexedDB", { configurable: true, value: undefined });
  try {
    expect(await openPendingEdits(artifactId(), 1)).toBeUndefined();
  } finally {
    Object.defineProperty(globalThis, "indexedDB", { configurable: true, value: real });
  }
});
