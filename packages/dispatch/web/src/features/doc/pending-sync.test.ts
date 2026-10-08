import { afterEach, expect, spyOn, test } from "bun:test";
import { waitFor } from "@testing-library/react";
import * as Y from "yjs";

import { type DocumentConnection, loadDocumentTransport } from "./connection";
import { openPendingEdits, type PendingEdits, type PendingKey } from "./pending-edits";
import { type PendingState, type PendingSync, startPendingSync } from "./pending-sync";

// The frames a Hocuspocus peer exchanges: the document name, a message tag, then the message.
// Built by hand here so the expected bytes never come from the code under test.
function varUint(value: number): number[] {
  const bytes: number[] = [];
  let rest = value;
  while (rest >= 0x80) {
    bytes.push((rest & 0x7f) | 0x80);
    rest = Math.floor(rest / 0x80);
  }
  bytes.push(rest);
  return bytes;
}
function varBytes(value: Uint8Array): number[] {
  return [...varUint(value.length), ...value];
}
function frame(name: string, ...parts: number[][]): Uint8Array {
  return Uint8Array.from([...varBytes(new TextEncoder().encode(name)), ...parts.flat()]);
}
const NAME = "the-document";
const updateFrame = (update: Uint8Array) => frame(NAME, [0, 2], varBytes(update));
const syncStep2Frame = (update: Uint8Array, name = NAME) => frame(name, [0, 1], varBytes(update));
const syncStep1Frame = (stateVector: Uint8Array, name: string) =>
  frame(name, [0, 0], varBytes(stateVector));
const syncStatusFrame = (applied: boolean, name = NAME) => frame(name, [8, applied ? 1 : 0]);
const authenticatedFrame = (scope: string, name: string) =>
  frame(name, [2, 2], varBytes(new TextEncoder().encode(scope)));

/** Stands in for the provider: updates it applies from the server carry it as their origin. */
const SERVER = { server: true };

let next = 0;
function artifactId(): string {
  next += 1;
  return `pending-sync-test-${next}`;
}

const cleanups: (() => void)[] = [];
afterEach(() => {
  for (const cleanup of cleanups.splice(0)) {
    cleanup();
  }
});

async function storeFor(artifact: string, clientId: number): Promise<PendingEdits> {
  const store = await openPendingEdits(artifact, clientId);
  if (store === undefined) {
    throw new Error("fake-indexeddb refused to open");
  }
  cleanups.push(() => store.close());
  return store;
}

/** The keys a document's rows hold, read through a store of a client that wrote none. */
async function rowsOf(artifact: string): Promise<PendingKey[]> {
  return (await (await storeFor(artifact, -1)).restore())?.keys ?? [];
}

interface Scripted {
  artifact: string;
  doc: Y.Doc;
  states: PendingState[];
  sync: PendingSync;
  /** Types `text` at the end of the document's text, as a local edit, and returns its update. */
  type(text: string): Uint8Array;
}

async function scripted(artifact = artifactId(), seed?: Uint8Array): Promise<Scripted> {
  const doc = new Y.Doc();
  if (seed !== undefined) {
    Y.applyUpdate(doc, seed, SERVER);
  }
  const store = await storeFor(artifact, doc.clientID);
  const states: PendingState[] = [];
  const sync = startPendingSync({
    doc,
    onChange: (state) => states.push(state),
    remoteOrigin: SERVER,
    store: Promise.resolve(store),
  });
  cleanups.push(() => sync.destroy());
  return {
    artifact,
    doc,
    states,
    sync,
    type(text) {
      let emitted: Uint8Array | undefined;
      const capture = (update: Uint8Array) => {
        emitted = update;
      };
      doc.on("update", capture);
      doc.getText("t").insert(doc.getText("t").length, text);
      doc.off("update", capture);
      if (emitted === undefined) {
        throw new Error("a local edit emits an update");
      }
      return emitted;
    },
  };
}

/** A browser of an earlier page load: it applied `server`, typed `text`, and kept the edit. */
async function savedEdit(artifact: string, server: Y.Doc, text: string): Promise<Uint8Array> {
  const browser = new Y.Doc();
  Y.applyUpdate(browser, Y.encodeStateAsUpdate(server));
  let update: Uint8Array | undefined;
  browser.on("update", (emitted: Uint8Array) => {
    update = emitted;
  });
  browser.getText("t").insert(browser.getText("t").length, text);
  if (update === undefined) {
    throw new Error("a local edit emits an update");
  }
  const store = await storeFor(artifact, browser.clientID);
  await store.record(update).written;
  return update;
}

function serverWith(text: string, clientId: number): Y.Doc {
  const server = new Y.Doc();
  server.clientID = clientId;
  server.getText("t").insert(0, text);
  return server;
}

test("a local edit is recorded, and an acknowledgement of its frame clears that row only", async () => {
  const { artifact, doc, sync, type } = await scripted();
  sync.socketOpened();
  const first = type("first");
  const second = type(" second");
  sync.frameWritten(updateFrame(first));
  sync.frameWritten(updateFrame(second));

  await sync.frameReceived(syncStatusFrame(true));

  expect(await rowsOf(artifact)).toEqual([[artifact, doc.clientID, 2]]);
});

for (const queued of [0, 1, 3]) {
  test(`a reconnect with ${queued} queued updates clears each row only once its own frame is acknowledged`, async () => {
    const { artifact, doc, sync, type } = await scripted();
    // An edit written on a socket that then dropped: its acknowledgement never comes.
    sync.socketOpened();
    sync.frameWritten(updateFrame(type("lost")));
    // Edits typed while the socket is down wait in the websocket's queue, which flushes at the
    // next socket's first server frame, ahead of the provider's reply to the server's SyncStep1.
    const waiting = Array.from({ length: queued }, (_, index) => type(` queued ${index}`));
    sync.socketOpened();
    for (const update of waiting) {
      sync.frameWritten(updateFrame(update));
    }
    sync.frameWritten(syncStep2Frame(Y.encodeStateAsUpdate(doc)));

    const remaining = Array.from({ length: queued }, (_, index) => index + 2);
    for (let index = 0; index < queued; index += 1) {
      await sync.frameReceived(syncStatusFrame(true));
      remaining.shift();
      expect(await rowsOf(artifact)).toEqual(
        [1, ...remaining].map((seq): PendingKey => [artifact, doc.clientID, seq])
      );
    }
    // The SyncStep2 carried the edit the dropped socket never confirmed.
    await sync.frameReceived(syncStatusFrame(true));
    expect(await rowsOf(artifact)).toEqual([]);
  });
}

test("an acknowledgement of 0 clears nothing", async () => {
  const { artifact, doc, sync, type } = await scripted();
  sync.socketOpened();
  sync.frameWritten(updateFrame(type("refused")));
  sync.frameWritten(syncStep2Frame(Y.encodeStateAsUpdate(doc)));

  await sync.frameReceived(syncStatusFrame(false));
  await sync.frameReceived(syncStatusFrame(false));

  expect(await rowsOf(artifact)).toEqual([[artifact, doc.clientID, 1]]);
});

test("a read-only first sync applies nothing and keeps the saved edits", async () => {
  const artifact = artifactId();
  const server = serverWith("shared", 100);
  await savedEdit(artifact, server, " saved");
  const { doc, states, sync } = await scripted(artifact, Y.encodeStateAsUpdate(server));
  const before = Y.encodeStateVector(doc);

  await sync.firstSync(true);

  expect(doc.getText("t").toString()).toBe("shared");
  expect(Y.encodeStateVector(doc)).toEqual(before);
  expect((await rowsOf(artifact)).length).toBe(1);
  expect(states.at(-1)).toEqual({ count: 1, readOnly: true, rebuiltAt: undefined, stored: true });
});

test("a restore that parks on a scratch document is dropped, reported, and leaves the live document alone", async () => {
  const artifact = artifactId();
  // The edit was typed against a history the server no longer has: a rebuild reseeded the same
  // text from a fresh document, so the items the edit sits beside are gone.
  const before = Date.now();
  await savedEdit(artifact, serverWith("shared", 100), " saved");
  const rebuilt = serverWith("shared", 200);
  const { doc, states, sync } = await scripted(artifact, Y.encodeStateAsUpdate(rebuilt));
  const stateVector = Y.encodeStateVector(doc);

  await sync.firstSync(false);

  expect(Y.encodeStateVector(doc)).toEqual(stateVector);
  expect(doc.store.pendingStructs).toBeNull();
  expect(doc.getText("t").toString()).toBe("shared");
  expect(await rowsOf(artifact)).toEqual([]);
  const reported = states.at(-1);
  expect(reported?.count).toBe(0);
  expect(reported?.rebuiltAt).toBeGreaterThanOrEqual(before);
  expect(reported?.rebuiltAt).toBeLessThanOrEqual(Date.now());
});

test("a restore the server already holds fires no update and drops its rows", async () => {
  const artifact = artifactId();
  const server = serverWith("shared", 100);
  const saved = await savedEdit(artifact, server, " saved");
  // The server applied the edit; the tab reloaded before its acknowledgement cleared the row.
  Y.applyUpdate(server, saved);
  const { doc, sync } = await scripted(artifact, Y.encodeStateAsUpdate(server));
  const updates: Uint8Array[] = [];
  doc.on("update", (update: Uint8Array) => updates.push(update));

  await sync.firstSync(false);

  expect(updates).toEqual([]);
  expect(doc.getText("t").toString()).toBe("shared saved");
  expect(await rowsOf(artifact)).toEqual([]);
});

test("a restore that merges is recorded under the new client and the old rows are dropped", async () => {
  const artifact = artifactId();
  const server = serverWith("shared", 100);
  await savedEdit(artifact, server, " saved");
  const { doc, states, sync } = await scripted(artifact, Y.encodeStateAsUpdate(server));

  await sync.firstSync(false);

  expect(doc.getText("t").toString()).toBe("shared saved");
  expect(await rowsOf(artifact)).toEqual([[artifact, doc.clientID, 1]]);
  expect(states.at(-1)).toEqual({ count: 1, readOnly: false, rebuiltAt: undefined, stored: true });
});

test("a stale saved row does not discard another row that still applies", async () => {
  const artifact = artifactId();
  await savedEdit(artifact, serverWith("shared", 100), " stale");
  const server = serverWith("shared", 200);
  await savedEdit(artifact, server, " valid");
  const { doc, states, sync } = await scripted(artifact, Y.encodeStateAsUpdate(server));

  await sync.firstSync(false);

  expect(doc.getText("t").toString()).toBe("shared valid");
  expect(await rowsOf(artifact)).toEqual([[artifact, doc.clientID, 1]]);
  expect(states.at(-1)?.rebuiltAt).toBeDefined();
});

test("a later SyncStep2 acknowledgement retries an earlier row whose individual clear failed", async () => {
  const artifact = artifactId();
  const doc = new Y.Doc();
  const edits = await storeFor(artifact, doc.clientID);
  let refuseFirstClear = true;
  const store: PendingEdits = {
    ...edits,
    clear: async (seq) => {
      if (refuseFirstClear) {
        refuseFirstClear = false;
        throw new Error("clear failed");
      }
      return edits.clear(seq);
    },
  };
  const logged = spyOn(console, "error").mockImplementation(() => {});
  const sync = startPendingSync({
    doc,
    onChange() {},
    remoteOrigin: SERVER,
    store: Promise.resolve(store),
  });
  cleanups.push(() => sync.destroy());
  try {
    sync.socketOpened();
    const first = (() => {
      let update: Uint8Array | undefined;
      doc.on("update", (emitted: Uint8Array) => {
        update = emitted;
      });
      doc.getText("t").insert(0, "first");
      if (update === undefined) {
        throw new Error("a local edit emits an update");
      }
      return update;
    })();
    sync.frameWritten(updateFrame(first));
    await sync.frameReceived(syncStatusFrame(true));
    expect(await rowsOf(artifact)).toEqual([[artifact, doc.clientID, 1]]);

    doc.getText("t").insert(doc.getText("t").length, " second");
    sync.frameWritten(syncStep2Frame(Y.encodeStateAsUpdate(doc)));
    await sync.frameReceived(syncStatusFrame(true));

    expect(await rowsOf(artifact)).toEqual([]);
  } finally {
    logged.mockRestore();
  }
});

/** A socket already open, as the browser's is once it connects; the test plays the server. */
class OpenSocket extends EventTarget {
  static opened: OpenSocket[] = [];
  binaryType = "blob";
  readonly readyState: number = 1;
  readonly sent: Uint8Array[] = [];

  constructor(readonly url: string) {
    super();
    OpenSocket.opened.push(this);
    queueMicrotask(() => this.dispatchEvent(new Event("open")));
  }

  close(): void {}

  send(data: Uint8Array): void {
    this.sent.push(new Uint8Array(data));
  }

  receive(data: Uint8Array): void {
    this.dispatchEvent(new MessageEvent("message", { data: data.slice().buffer }));
  }
}

test("a real provider's edit typed on an open socket is cleared by its own acknowledgement", async () => {
  const artifact = artifactId();
  const realSocket = globalThis.WebSocket;
  globalThis.WebSocket = OpenSocket as unknown as typeof WebSocket;
  OpenSocket.opened = [];
  let connection: DocumentConnection | undefined;
  let synced = false;
  try {
    const connect = await loadDocumentTransport();
    connection = connect(artifact, {
      onAdmission() {},
      onOutsideSchema() {},
      onPending() {},
      onStatus() {},
      onSynced: () => {
        synced = true;
      },
      schemaVersion: 1,
    });
    await waitFor(() => expect(OpenSocket.opened[0]?.sent.length).toBeGreaterThan(0));
    const socket = OpenSocket.opened[0] as OpenSocket;
    socket.receive(authenticatedFrame("read-write", artifact));
    // The provider answers the server's SyncStep1 with a SyncStep2 of its own: the first frame on
    // this socket the server acknowledges.
    socket.receive(syncStep1Frame(Uint8Array.of(0), artifact));
    socket.receive(syncStep2Frame(Y.encodeStateAsUpdate(new Y.Doc()), artifact));
    await waitFor(() => expect(synced).toBe(true));
    socket.receive(syncStatusFrame(true, artifact));

    // On an open socket the provider writes each edit's frame inside the document's own update
    // event, before any later listener has stored the edit.
    const text = connection.doc.getText("t");
    text.insert(0, "first");
    text.insert(text.length, " second");
    socket.receive(syncStatusFrame(true, artifact));

    const clientId = connection.doc.clientID;
    await waitFor(async () => expect(await rowsOf(artifact)).toEqual([[artifact, clientId, 2]]));
  } finally {
    connection?.destroy();
    globalThis.WebSocket = realSocket;
  }
});

test("a document whose browser keeps no edits says so, and still connects", async () => {
  const realStorage = globalThis.indexedDB;
  const realSocket = globalThis.WebSocket;
  const logged = spyOn(console, "error").mockImplementation(() => {});
  Object.defineProperty(globalThis, "indexedDB", { configurable: true, value: undefined });
  globalThis.WebSocket = OpenSocket as unknown as typeof WebSocket;
  OpenSocket.opened = [];
  let connection: DocumentConnection | undefined;
  const pending: PendingState[] = [];
  try {
    const connect = await loadDocumentTransport();
    connection = connect(artifactId(), {
      onAdmission() {},
      onOutsideSchema() {},
      onPending: (state) => pending.push(state),
      onStatus() {},
      onSynced() {},
      schemaVersion: 1,
    });
    await waitFor(() => expect(OpenSocket.opened).toHaveLength(1));
    await waitFor(() =>
      expect(pending).toEqual([{ count: 0, readOnly: false, rebuiltAt: undefined, stored: false }])
    );
  } finally {
    connection?.destroy();
    globalThis.WebSocket = realSocket;
    Object.defineProperty(globalThis, "indexedDB", { configurable: true, value: realStorage });
    logged.mockRestore();
  }
});
