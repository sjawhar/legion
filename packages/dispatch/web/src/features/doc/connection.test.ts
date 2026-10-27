import { afterEach, expect, spyOn, test } from "bun:test";
import { waitFor } from "@testing-library/react";
import { createDecoder, readVarString, readVarUint, readVarUint8Array } from "lib0/decoding";
import * as Y from "yjs";

import {
  type ConnectionState,
  colorForLogin,
  connectionLabel,
  type DocumentConnection,
  isSchemaReadOnly,
  loadDocumentTransport,
  pendingNotice,
  wsUrl,
} from "./connection";
import {
  artifactId,
  authenticatedFrame,
  awarenessFrame,
  closeStores,
  frame,
  paragraphText,
  rowsOf,
  savedChanges,
  savedEdit,
  serverWith,
  serverWithHeading,
  stampHeadingId,
  syncStatusFrame,
  syncStep1Frame,
  syncStep2Frame,
  typeInto,
  typeParagraph,
} from "./pending-fixture";
import * as pendingSyncModule from "./pending-sync";
import { type PendingState, startPendingSync } from "./pending-sync";

afterEach(closeStores);

test("wsUrl builds an encoded same-origin document websocket URL", () => {
  const protocol = window.location.protocol === "https:" ? "wss:" : "ws:";
  expect(wsUrl("artifact / one")).toBe(
    `${protocol}//${window.location.host}/ws/doc/artifact%20%2F%20one`
  );
});

test("colorForLogin assigns each login a stable, valid CSS presence color", () => {
  for (const login of ["alice", "bob"]) {
    const color = colorForLogin(login);
    expect(color).toBe(colorForLogin(login));
    expect(CSS.supports("color", color)).toBe(true);
  }
});

test("a read-only Hocuspocus admission signals a schema reload", () => {
  expect(isSchemaReadOnly("readonly")).toBe(true);
  expect(isSchemaReadOnly("read-write")).toBe(false);
  expect(isSchemaReadOnly(undefined)).toBe(false);
});

test("pending document labels describe retained, read-only, rebuilt, and unretained edits", () => {
  const retained = { count: 2, readOnly: false, rebuiltAt: undefined, stored: true };
  expect(connectionLabel("offline", pendingNotice("offline", retained))).toBe(
    "offline · 2 edits saved in this browser, not sent yet."
  );
  expect(pendingNotice("connected", { ...retained, readOnly: true })).toBe(
    "2 edits saved in this browser can't be sent: this document is read-only."
  );
  expect(pendingNotice("connecting", { ...retained, stored: false })).toBe(
    "Edits typed now will not survive a reload."
  );
  expect(pendingNotice("connected", { ...retained, rebuiltAt: 0 })).toBe(
    `Edits saved in this browser at ${new Date(0).toLocaleString()} could not be applied: the document was rebuilt since.`
  );
  expect(pendingNotice("connected", retained)).toBeUndefined();
  expect(connectionLabel("connected", pendingNotice("connected", retained))).toBe("connected");
  expect(pendingNotice("offline", { ...retained, count: 1 })).toBe(
    "1 edit saved in this browser, not sent yet."
  );
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

  /** The server closes the socket, as a dropped connection does; the provider reconnects. */
  drop(): void {
    this.dispatchEvent(new CloseEvent("close", { code: 1006 }));
  }
}

interface ProviderHarness {
  connection: DocumentConnection;
  /** Each admission the host was told of, read-only or not, in order. */
  admissions: boolean[];
  /** Each pending state the host was told of, in order. */
  pending: PendingState[];
  /** Whether the host was told that the first admission's restore finished. */
  synced(): boolean;
  /** The `index`th socket the provider opened, once the provider has written to it. */
  socket(index: number): Promise<OpenSocket>;
}

/**
 * Connects `artifact` through the real transport and Hocuspocus provider, over `OpenSocket`s the
 * test answers as the server, runs `body` with what the host is told, and then destroys the
 * connection and puts the browser's WebSocket back.
 */
async function withProvider(
  artifact: string,
  body: (harness: ProviderHarness) => Promise<void>
): Promise<void> {
  const realSocket = globalThis.WebSocket;
  globalThis.WebSocket = OpenSocket as unknown as typeof WebSocket;
  OpenSocket.opened = [];
  let connection: DocumentConnection | undefined;
  try {
    const connect = await loadDocumentTransport();
    let synced = false;
    const admissions: boolean[] = [];
    const pending: PendingState[] = [];
    connection = connect(artifact, {
      onAdmission: (readOnly) => admissions.push(readOnly),
      onOutsideSchema() {},
      onPending: (state) => pending.push(state),
      onStatus() {},
      onSynced: () => {
        synced = true;
      },
      schemaVersion: 1,
    });
    await body({
      admissions,
      connection,
      pending,
      async socket(index) {
        await waitFor(() => expect(OpenSocket.opened[index]?.sent.length).toBeGreaterThan(0));
        return OpenSocket.opened[index] as OpenSocket;
      },
      synced: () => synced,
    });
  } finally {
    connection?.destroy();
    globalThis.WebSocket = realSocket;
  }
}

/** The payloads of the Sync frames of `step` (0 SyncStep1, 1 SyncStep2, 2 Update) a socket wrote. */
function syncPayloads(socket: OpenSocket, name: string, step: number): Uint8Array[] {
  const prefix = frame(name, [0, step]);
  return socket.sent
    .filter((written) => prefix.every((byte, index) => written[index] === byte))
    .map((written) => {
      const decoder = createDecoder(written);
      readVarString(decoder); // the document name
      readVarUint(decoder); // Sync
      readVarUint(decoder); // the step
      return readVarUint8Array(decoder);
    });
}

/**
 * Answers a socket that just opened in the order Dispatch's server (ygo) does. ygo queues its
 * SyncStep1, its SyncStep2 and the room's awareness before its read loop starts, and only that
 * loop reads the socket's token, so the socket is synced before it is authenticated. The loop then
 * answers the socket's own SyncStep1 with a SyncStep2, and the SyncStep2 the provider wrote in
 * answer to the server's SyncStep1 with a SyncStatus: a writable admission applies it to `server`
 * and answers 1; a read-only one applies nothing and answers 1 only when `server` already holds
 * every struct in it.
 */
function serveInYgoOrder(
  socket: OpenSocket,
  server: Y.Doc,
  scope: "readonly" | "read-write",
  name: string
): void {
  socket.receive(syncStep1Frame(Y.encodeStateVector(server), name));
  socket.receive(syncStep2Frame(Y.encodeStateAsUpdate(server), name));
  socket.receive(awarenessFrame(name));
  socket.receive(authenticatedFrame(scope, name));
  socket.receive(syncStep2Frame(Y.encodeStateAsUpdate(server), name));
  const answer = syncPayloads(socket, name, 1).at(-1);
  if (answer === undefined) {
    throw new Error("the provider answers the server's SyncStep1 with a SyncStep2");
  }
  if (scope === "readonly") {
    const missing = Y.diffUpdate(answer, Y.encodeStateVector(server));
    socket.receive(syncStatusFrame(Y.decodeUpdate(missing).structs.length === 0, name));
    return;
  }
  Y.applyUpdate(server, answer);
  socket.receive(syncStatusFrame(true, name));
}

/** Applies every Update frame a socket wrote to `server`, and acknowledges each, as a writable ygo room does. */
function answerUpdates(socket: OpenSocket, server: Y.Doc, name: string): void {
  for (const update of syncPayloads(socket, name, 2)) {
    Y.applyUpdate(server, update);
    socket.receive(syncStatusFrame(true, name));
  }
}

/** Types `text` at the end of the paragraph after the heading. */
function typeAfterHeading(doc: Y.Doc, text: string): void {
  const paragraph = doc.getXmlFragment("prosemirror").get(1);
  const typed = paragraph instanceof Y.XmlElement ? paragraph.get(0) : undefined;
  if (!(typed instanceof Y.XmlText)) {
    throw new Error("the block after the heading is a paragraph of text");
  }
  typed.insert(typed.length, text);
}

test("a real provider's edit typed on an open socket is cleared by its own acknowledgement", async () => {
  const artifact = artifactId();
  await withProvider(artifact, async ({ connection, socket: opened, synced }) => {
    const socket = await opened(0);
    socket.receive(authenticatedFrame("read-write", artifact));
    // The provider answers the server's SyncStep1 with a SyncStep2 of its own: the first frame on
    // this socket the server acknowledges.
    socket.receive(syncStep1Frame(Uint8Array.of(0), artifact));
    socket.receive(syncStep2Frame(Y.encodeStateAsUpdate(new Y.Doc()), artifact));
    await waitFor(() => expect(synced()).toBe(true));
    socket.receive(syncStatusFrame(true, artifact));

    // On an open socket the provider writes each edit's frame inside the document's own update
    // event, before any later listener has stored the edit.
    typeInto(connection.doc, "first");
    typeInto(connection.doc, " second");
    socket.receive(syncStatusFrame(true, artifact));

    const clientId = connection.doc.clientID;
    await waitFor(async () => expect(await rowsOf(artifact)).toEqual([[artifact, clientId, 2]]));
  });
});

test("a reconnect's writable admission restores another tab's row only once that reconnect's sync has applied", async () => {
  const artifact = artifactId();
  const server = serverWith("shared", 100);
  const firstState = Y.encodeStateAsUpdate(server);
  // While this tab is admitted read-only, the issue reopens and a person types into the paragraph.
  // Another tab types after that text and closes before its socket sends the edit, so its row is
  // the edit's only copy, and it builds on an update this tab has not received yet.
  const person = new Y.Doc();
  person.clientID = 200;
  Y.applyUpdate(person, firstState);
  typeInto(person, " added");
  Y.applyUpdate(server, Y.encodeStateAsUpdate(person));
  await savedEdit(artifact, server, " typed");

  // Each restore the transport starts for the reconnect, with whether the reconnect's SyncStep2
  // had reached the socket by then.
  const reconnectRestores: boolean[] = [];
  const running: Promise<void>[] = [];
  let syncDelivered = false;
  // The transport imports the pending sync itself, so its sync is wrapped where it is started.
  const realStart = startPendingSync;
  const started = spyOn(pendingSyncModule, "startPendingSync").mockImplementation((options) => {
    const sync = realStart(options);
    return {
      ...sync,
      synced() {
        if (OpenSocket.opened.length > 1) {
          reconnectRestores.push(syncDelivered);
        }
        const restoring = sync.synced();
        running.push(restoring);
        return restoring;
      },
    };
  });
  try {
    await withProvider(artifact, async ({ connection, pending, socket, synced }) => {
      const first = await socket(0);
      first.receive(authenticatedFrame("readonly", artifact));
      first.receive(syncStep2Frame(firstState, artifact));
      await waitFor(() => expect(synced()).toBe(true));
      await waitFor(() => expect(pending.at(-1)).toMatchObject({ count: 1, readOnly: true }));

      first.drop();
      const second = await socket(1);
      // A server that authenticates the reconnect before its SyncStep2 arrives: whatever the
      // authentication starts runs to its end before the sync is delivered.
      second.receive(authenticatedFrame("read-write", artifact));
      await Promise.all(running);
      syncDelivered = true;
      second.receive(syncStep2Frame(Y.encodeStateAsUpdate(server), artifact));

      const doc = connection.doc;
      await waitFor(() => expect(paragraphText(doc).toString()).toBe("shared added typed"));
      await Promise.all(running);
      await waitFor(async () =>
        expect(await rowsOf(artifact)).toEqual([[artifact, doc.clientID, 1]])
      );
      expect(pending.every((state) => state.rebuiltAt === undefined)).toBe(true);
      expect(reconnectRestores).toEqual([true]);
    });
  } finally {
    started.mockRestore();
  }
});

test("a reopened issue's read-only load stores nothing, and its writable reconnect restores another tab's row and stores what is typed after", async () => {
  const artifact = artifactId();
  const server = serverWithHeading(100);
  await withProvider(artifact, async ({ connection, pending, socket, synced }) => {
    const first = await socket(0);
    serveInYgoOrder(first, server, "readonly", artifact);
    await waitFor(() => expect(synced()).toBe(true));
    const doc = connection.doc;

    // The closed issue's editor stamps the heading's id as it loads, and the read-only room
    // refuses the provider's Update: a page that only loads names no edit.
    stampHeadingId(doc);
    first.receive(syncStatusFrame(false, artifact));
    await waitFor(() => expect(pending.at(-1)).toMatchObject({ count: 0, readOnly: true }));
    expect(await rowsOf(artifact)).toEqual([]);

    // While the page is open, the issue reopens and a person types a paragraph. Another tab adds
    // to it and closes before its socket sends the edit, so its row is the edit's only copy, and
    // it builds on an update this tab has not received yet.
    const person = new Y.Doc();
    person.clientID = 200;
    Y.applyUpdate(person, Y.encodeStateAsUpdate(server));
    typeParagraph(person);
    Y.applyUpdate(server, Y.encodeStateAsUpdate(person));
    await savedChanges(artifact, server, (browser) => typeAfterHeading(browser, " More."));

    first.drop();
    const second = await socket(1);
    serveInYgoOrder(second, server, "read-write", artifact);
    // The reconnect's SyncStep2 carried the stamp the read-only room refused.
    expect(server.getXmlFragment("prosemirror").toString()).toContain('id="plan"');

    // The other tab's row is restored against the synced document, and re-recorded under this
    // tab's client: none is dropped as built on a rebuilt history.
    await waitFor(() =>
      expect(doc.getXmlFragment("prosemirror").toString()).toContain("Typed. More.")
    );
    await waitFor(async () =>
      expect(await rowsOf(artifact)).toEqual([[artifact, doc.clientID, 1]])
    );

    typeAfterHeading(doc, " Mine.");
    await waitFor(async () =>
      expect(await rowsOf(artifact)).toEqual([1, 2].map((seq) => [artifact, doc.clientID, seq]))
    );
    await waitFor(() => expect(pending.at(-1)).toMatchObject({ count: 2, readOnly: false }));

    // The room applies and acknowledges the restored row's Update and the typing's: both clear.
    answerUpdates(second, server, artifact);
    await waitFor(async () => expect(await rowsOf(artifact)).toEqual([]));
    await waitFor(() => expect(pending.at(-1)).toMatchObject({ count: 0, readOnly: false }));
    expect(server.getXmlFragment("prosemirror").toString()).toContain("Typed. More. Mine.");
    expect(pending.every((state) => state.rebuiltAt === undefined)).toBe(true);
  });
});

test("a reopen and a second close name no unsent edit to a person who typed nothing", async () => {
  const artifact = artifactId();
  const server = serverWithHeading(100);
  await withProvider(artifact, async ({ connection, pending, socket, synced }) => {
    // No state the host was told of counts an edit or names one, whatever the connection shows.
    const expectNoNotice = async (readOnly: boolean) => {
      await waitFor(() => expect(pending.at(-1)).toMatchObject({ readOnly }));
      expect(await rowsOf(artifact)).toEqual([]);
      for (const state of pending) {
        expect(state.count).toBe(0);
        for (const shown of ["connecting", "connected", "offline"] satisfies ConnectionState[]) {
          expect(pendingNotice(shown, state)).toBeUndefined();
        }
      }
    };

    const first = await socket(0);
    serveInYgoOrder(first, server, "readonly", artifact);
    await waitFor(() => expect(synced()).toBe(true));
    // The closed issue's editor stamps the heading's id as it loads; the room refuses it.
    stampHeadingId(connection.doc);
    first.receive(syncStatusFrame(false, artifact));
    await expectNoNotice(true);

    // The issue reopens, and the socket reconnects writable.
    first.drop();
    const second = await socket(1);
    serveInYgoOrder(second, server, "read-write", artifact);
    expect(server.getXmlFragment("prosemirror").toString()).toContain('id="plan"');
    await expectNoNotice(false);

    // The issue closes again, and the socket reconnects read-only.
    second.drop();
    const third = await socket(2);
    serveInYgoOrder(third, server, "readonly", artifact);
    await expectNoNotice(true);
  });
});

test("a document whose browser keeps no edits says so, and still connects", async () => {
  const realStorage = globalThis.indexedDB;
  const logged = spyOn(console, "error").mockImplementation(() => {});
  Object.defineProperty(globalThis, "indexedDB", { configurable: true, value: undefined });
  try {
    await withProvider(artifactId(), async ({ pending }) => {
      await waitFor(() => expect(OpenSocket.opened).toHaveLength(1));
      await waitFor(() =>
        expect(pending).toEqual([
          { count: 0, readOnly: false, rebuiltAt: undefined, stored: false },
        ])
      );
    });
  } finally {
    Object.defineProperty(globalThis, "indexedDB", { configurable: true, value: realStorage });
    logged.mockRestore();
  }
});
