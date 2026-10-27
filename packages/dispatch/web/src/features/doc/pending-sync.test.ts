import { afterEach, expect, spyOn, test } from "bun:test";
import { waitFor } from "@testing-library/react";
import * as Y from "yjs";

import type { PendingEdits, PendingKey } from "./pending-edits";
import {
  artifactId,
  closeStores,
  frame,
  NAME,
  paragraphText,
  rowsOf,
  savedChanges,
  savedEdit,
  serverWith,
  serverWithHeading,
  stampHeadingId,
  storeFor,
  syncStatusFrame,
  syncStep2Frame,
  typeInto,
  typeParagraph,
  updateFrame,
} from "./pending-fixture";
import { type PendingState, type PendingSync, startPendingSync } from "./pending-sync";

/** Stands in for the provider: updates it applies from the server carry it as their origin. */
const SERVER = { server: true };

const cleanups: (() => void)[] = [];
afterEach(() => {
  for (const cleanup of cleanups.splice(0)) {
    cleanup();
  }
  closeStores();
});

/** Takes an admission whose socket has synced and authenticated, as the transport hands it over. */
async function admit(sync: PendingSync, readOnly: boolean): Promise<void> {
  sync.authenticated(readOnly);
  await sync.synced();
}

interface Scripted {
  artifact: string;
  doc: Y.Doc;
  states: PendingState[];
  sync: PendingSync;
  /** Types `text` at the end of the document's paragraph, as a local edit, and returns its update. */
  type(text: string): Uint8Array;
}

/**
 * A `startPendingSync` over a fresh document and its store. `wrapStore` stands in for the store
 * with one that changes how it behaves, given the real store of the document's client.
 */
async function scripted(
  artifact = artifactId(),
  seed?: Uint8Array,
  wrapStore: (store: PendingEdits) => PendingEdits = (store) => store
): Promise<Scripted> {
  const doc = new Y.Doc();
  if (seed !== undefined) {
    Y.applyUpdate(doc, seed, SERVER);
  }
  const store = wrapStore(await storeFor(artifact, doc.clientID));
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
      typeInto(doc, text);
      doc.off("update", capture);
      if (emitted === undefined) {
        throw new Error("a local edit emits an update");
      }
      return emitted;
    },
  };
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

test("an update that changes only attributes the editor derives is stored only ahead of a person's edit", async () => {
  const server = new Y.Doc();
  server.clientID = 100;
  const heading = new Y.XmlElement("heading");
  heading.setAttribute("level", "2");
  const list = new Y.XmlElement("ordered_list");
  list.insert(0, [new Y.XmlElement("list_item")]);
  server.getXmlFragment("prosemirror").insert(0, [heading, list]);
  const { artifact, doc, states, sync } = await scripted(
    artifactId(),
    Y.encodeStateAsUpdate(server)
  );
  await admit(sync, false);
  const [loadedHeading, loadedList] = doc.getXmlFragment("prosemirror").toArray();
  const item = loadedList instanceof Y.XmlElement ? loadedList.get(0) : undefined;
  if (!(loadedHeading instanceof Y.XmlElement && item instanceof Y.XmlElement)) {
    throw new Error("the document holds a heading and an ordered list item");
  }

  // The editor stamps the heading's id and labels the ordered list's item: nothing is stored.
  doc.transact(() => {
    loadedHeading.setAttribute("id", "plan");
    item.setAttribute("label", "1.");
    item.setAttribute("listType", "ordered");
  });
  expect(await rowsOf(artifact)).toEqual([]);
  expect(states.every((state) => state.count === 0)).toBe(true);

  // A heading's level is a person's choice: its change is stored, after the update it builds on.
  loadedHeading.setAttribute("level", "3");
  await waitFor(async () =>
    expect(await rowsOf(artifact)).toEqual(
      [1, 2].map((seq): PendingKey => [artifact, doc.clientID, seq])
    )
  );
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

test("a SyncStatus frame that ends before its flag consumes no sent frame", async () => {
  const logged = spyOn(console, "error").mockImplementation(() => {});
  try {
    const { artifact, sync, type } = await scripted();
    sync.socketOpened();
    sync.frameWritten(updateFrame(type("kept")));

    await sync.frameReceived(frame(NAME, [8]));
    await sync.frameReceived(syncStatusFrame(true));

    expect(await rowsOf(artifact)).toEqual([]);
    expect(logged).toHaveBeenCalledTimes(1);
  } finally {
    logged.mockRestore();
  }
});

test("an Update frame whose length overruns the frame is refused, not read past its end", async () => {
  const logged = spyOn(console, "error").mockImplementation(() => {});
  try {
    const { artifact, sync, type } = await scripted();
    sync.socketOpened();
    const whole = updateFrame(type("kept"));
    // The frame cut two bytes short, handed over as a view into a larger buffer whose bytes past
    // the cut are not the update's: its length prefix still claims the whole update.
    const cut = whole.subarray(0, whole.length - 2);
    const backing = new Uint8Array(whole.length + 16);
    backing.set(cut);
    sync.frameWritten(backing.subarray(0, cut.length));
    sync.frameWritten(whole);

    await sync.frameReceived(syncStatusFrame(true));

    expect(await rowsOf(artifact)).toEqual([]);
    expect(logged).toHaveBeenCalledTimes(1);
  } finally {
    logged.mockRestore();
  }
});

test("a read-only first sync applies nothing and keeps the saved edits", async () => {
  const artifact = artifactId();
  const server = serverWith("shared", 100);
  await savedEdit(artifact, server, " saved");
  const { doc, states, sync } = await scripted(artifact, Y.encodeStateAsUpdate(server));
  const before = Y.encodeStateVector(doc);

  await admit(sync, true);

  expect(paragraphText(doc).toString()).toBe("shared");
  expect(Y.encodeStateVector(doc)).toEqual(before);
  expect((await rowsOf(artifact)).length).toBe(1);
  expect(states.at(-1)).toEqual({ count: 1, readOnly: true, rebuiltAt: undefined, stored: true });
});

test("a read-only admission stores none of the editor's own updates", async () => {
  const server = serverWithHeading(100);
  const { artifact, doc, states, sync } = await scripted(
    artifactId(),
    Y.encodeStateAsUpdate(server)
  );
  await admit(sync, true);

  // The editor of a read-only document takes no typing, yet stamps each heading's id on load.
  stampHeadingId(doc);

  expect(await rowsOf(artifact)).toEqual([]);
  expect(states.at(-1)).toEqual({ count: 0, readOnly: true, rebuiltAt: undefined, stored: true });
});

test("a read-only load drops the heading id an earlier read-only load stored once it derives the id again, and keeps a person's saved edit", async () => {
  const artifact = artifactId();
  const server = serverWithHeading(100);
  // An earlier read-only load stored its editor's heading-id stamp as though it were an edit.
  await savedChanges(artifact, server, stampHeadingId);
  // A person typed a paragraph while the document was writable, and it never reached the server.
  const [typed] = await savedChanges(artifact, server, typeParagraph);
  const { doc, states, sync } = await scripted(artifact, Y.encodeStateAsUpdate(server));

  // Before the editor exists, each row still changes what the document shows.
  await admit(sync, true);
  expect((await rowsOf(artifact)).length).toBe(2);
  expect(states.at(-1)?.count).toBe(2);

  stampHeadingId(doc);

  await waitFor(async () => expect(await rowsOf(artifact)).toEqual([typed?.key]));
  await waitFor(() =>
    expect(states.at(-1)).toEqual({ count: 1, readOnly: true, rebuiltAt: undefined, stored: true })
  );
});

test("a read-only load keeps an earlier row of a browser whose later rows build on it", async () => {
  const artifact = artifactId();
  const server = serverWithHeading(100);
  // A writable load stamped the heading's id, then its reader typed a paragraph; the socket was
  // down for both, so the paragraph's row continues the stamp's clock.
  const saved = await savedChanges(artifact, server, stampHeadingId, typeParagraph);
  let judged = 0;
  const { doc, sync } = await scripted(artifact, Y.encodeStateAsUpdate(server), (store) => ({
    ...store,
    async restoreRows() {
      const rows = await store.restoreRows();
      judged += 1;
      return rows;
    },
  }));
  await admit(sync, true);

  // The read-only load's editor derives the same stamp, which leaves the stamp's row alone
  // changing nothing; dropping it would leave the paragraph's row nothing to apply after.
  stampHeadingId(doc);
  await waitFor(() => expect(judged).toBe(2));
  expect(await rowsOf(artifact)).toEqual(saved.map((row) => row.key));

  // The issue reopens: the next, writable load restores the paragraph.
  const reopened = await scripted(artifact, Y.encodeStateAsUpdate(server));
  await admit(reopened.sync, false);
  expect(reopened.doc.getXmlFragment("prosemirror").toString()).toContain("Typed.");
  expect(reopened.states.at(-1)?.rebuiltAt).toBeUndefined();
});

test("a read-only first sync drops a saved row the server already holds", async () => {
  const artifact = artifactId();
  const server = serverWith("shared", 100);
  const saved = await savedEdit(artifact, server, " saved");
  // The server applied the edit before the issue closed; the acknowledgement never came.
  Y.applyUpdate(server, saved);
  const { states, sync } = await scripted(artifact, Y.encodeStateAsUpdate(server));

  await admit(sync, true);

  expect(await rowsOf(artifact)).toEqual([]);
  expect(states.at(-1)).toEqual({ count: 0, readOnly: true, rebuiltAt: undefined, stored: true });
});

test("a read-only first sync keeps a saved row that changes an object attribute", async () => {
  type Attributes = { meta: { size: number } };
  const artifact = artifactId();
  const server = new Y.Doc();
  server.clientID = 100;
  const image = new Y.XmlElement<Attributes>("image");
  image.setAttribute("meta", { size: 1 });
  server.getXmlFragment("prosemirror").insert(0, [image as unknown as Y.XmlElement]);
  await savedChanges(artifact, server, (browser) => {
    const saved = browser
      .getXmlFragment("prosemirror")
      .get(0) as unknown as Y.XmlElement<Attributes>;
    saved.setAttribute("meta", { size: 2 });
  });
  const { states, sync } = await scripted(artifact, Y.encodeStateAsUpdate(server));

  await admit(sync, true);

  expect((await rowsOf(artifact)).length).toBe(1);
  expect(states.at(-1)).toEqual({ count: 1, readOnly: true, rebuiltAt: undefined, stored: true });
});

test("a read-only first sync drops a saved row that only splits the same text into other items", async () => {
  const artifact = artifactId();
  const server = serverWith("shared", 100);
  // The same text typed again in two pieces, the later one before the earlier, which Yjs keeps
  // as two items where the server holds one.
  await savedChanges(artifact, server, (browser) => {
    browser.transact(() => {
      const text = paragraphText(browser);
      text.delete(0, text.length);
      text.insert(0, "red");
      text.insert(0, "sha");
    });
  });
  const { states, sync } = await scripted(artifact, Y.encodeStateAsUpdate(server));

  await admit(sync, true);

  expect(await rowsOf(artifact)).toEqual([]);
  expect(states.at(-1)).toEqual({ count: 0, readOnly: true, rebuiltAt: undefined, stored: true });
});

test("a read-only first sync drops a saved row that inserts and deletes the same text", async () => {
  const artifact = artifactId();
  const server = serverWith("shared", 100);
  await savedChanges(artifact, server, (browser) => {
    browser.transact(() => {
      const text = paragraphText(browser);
      text.insert(3, "typo");
      text.delete(3, "typo".length);
    });
  });
  const { states, sync } = await scripted(artifact, Y.encodeStateAsUpdate(server));

  await admit(sync, true);

  expect(await rowsOf(artifact)).toEqual([]);
  expect(states.at(-1)).toEqual({ count: 0, readOnly: true, rebuiltAt: undefined, stored: true });
});

test("a read-only first sync keeps a saved row that writes a margin record", async () => {
  const artifact = artifactId();
  const server = serverWith("shared", 100);
  await savedChanges(artifact, server, (browser) => {
    browser.getMap("marks").set("c-1", { kind: "comment", by: "ada" });
  });
  const { states, sync } = await scripted(artifact, Y.encodeStateAsUpdate(server));

  await admit(sync, true);

  expect((await rowsOf(artifact)).length).toBe(1);
  expect(states.at(-1)).toEqual({ count: 1, readOnly: true, rebuiltAt: undefined, stored: true });
});

test("a read-only first sync keeps a saved row that writes a root other than the tree and the margin records", async () => {
  const artifact = artifactId();
  const server = serverWith("shared", 100);
  await savedChanges(artifact, server, (browser) => {
    browser.getText("notes").insert(0, "aside");
  });
  const { states, sync } = await scripted(artifact, Y.encodeStateAsUpdate(server));

  await admit(sync, true);

  expect((await rowsOf(artifact)).length).toBe(1);
  expect(states.at(-1)).toEqual({ count: 1, readOnly: true, rebuiltAt: undefined, stored: true });
});

test("a connection first admitted read-only and later writable stores and counts the typing", async () => {
  const artifact = artifactId();
  const server = serverWith("shared", 100);
  // A person typed into the document in another page while it was writable.
  await savedEdit(artifact, server, " saved");
  const { doc, states, sync, type } = await scripted(artifact, Y.encodeStateAsUpdate(server));
  await admit(sync, true);
  expect(paragraphText(doc).toString()).toBe("shared");

  // The issue reopens and the socket reconnects: the new admission is writable. The edit the
  // read-only first sync left is restored, and what the person types now is stored.
  await admit(sync, false);
  expect(paragraphText(doc).toString()).toBe("shared saved");
  type(" typed");

  await waitFor(async () => expect((await rowsOf(artifact)).length).toBe(2));
  await waitFor(() =>
    expect(states.at(-1)).toEqual({ count: 2, readOnly: false, rebuiltAt: undefined, stored: true })
  );
});

test("a later writable admission keeps this tab's own unacknowledged rows", async () => {
  const artifact = artifactId();
  const { sync, type } = await scripted(artifact, Y.encodeStateAsUpdate(serverWith("shared", 100)));
  await admit(sync, false);
  type(" mine");
  await waitFor(async () => expect((await rowsOf(artifact)).length).toBe(1));
  const mine = await rowsOf(artifact);

  // The issue closes and reopens while the server never acknowledged the edit.
  await admit(sync, true);
  await admit(sync, false);

  expect(await rowsOf(artifact)).toEqual(mine);
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

  await admit(sync, false);

  expect(Y.encodeStateVector(doc)).toEqual(stateVector);
  expect(doc.store.pendingStructs).toBeNull();
  expect(paragraphText(doc).toString()).toBe("shared");
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

  await admit(sync, false);

  expect(updates).toEqual([]);
  expect(paragraphText(doc).toString()).toBe("shared saved");
  expect(await rowsOf(artifact)).toEqual([]);
});

test("a restore that merges is recorded under the new client and the old rows are dropped", async () => {
  const artifact = artifactId();
  const server = serverWith("shared", 100);
  await savedEdit(artifact, server, " saved");
  const { doc, states, sync } = await scripted(artifact, Y.encodeStateAsUpdate(server));

  await admit(sync, false);

  expect(paragraphText(doc).toString()).toBe("shared saved");
  expect(await rowsOf(artifact)).toEqual([[artifact, doc.clientID, 1]]);
  expect(states.at(-1)).toEqual({ count: 1, readOnly: false, rebuiltAt: undefined, stored: true });
});

test("a stale saved row does not discard another row that still applies", async () => {
  const artifact = artifactId();
  await savedEdit(artifact, serverWith("shared", 100), " stale");
  const server = serverWith("shared", 200);
  await savedEdit(artifact, server, " valid");
  const { doc, states, sync } = await scripted(artifact, Y.encodeStateAsUpdate(server));

  await admit(sync, false);

  expect(paragraphText(doc).toString()).toBe("shared valid");
  expect(await rowsOf(artifact)).toEqual([[artifact, doc.clientID, 1]]);
  expect(states.at(-1)?.rebuiltAt).toBeDefined();
});

test("a stale row does not let a later row from the same lost history through", async () => {
  const artifact = artifactId();
  // Two browsers typed against a history the server lost to a rebuild: each row parks on the
  // same missing client, so a check that kept the first row's parked structs as its baseline
  // would let the second row onto the live document.
  const lost = serverWith("shared", 100);
  await savedEdit(artifact, lost, " stale one");
  await savedEdit(artifact, lost, " stale two");
  const server = serverWith("shared", 200);
  await savedEdit(artifact, server, " valid");
  const { doc, states, sync } = await scripted(artifact, Y.encodeStateAsUpdate(server));

  await admit(sync, false);

  expect(paragraphText(doc).toString()).toBe("shared valid");
  expect(doc.store.pendingStructs).toBeNull();
  expect(await rowsOf(artifact)).toEqual([[artifact, doc.clientID, 1]]);
  expect(states.at(-1)?.rebuiltAt).toBeDefined();
});

test("a first sync deletes every row it handled in one IndexedDB write", async () => {
  const artifact = artifactId();
  await savedEdit(artifact, serverWith("shared", 100), " stale");
  const server = serverWith("shared", 200);
  await savedEdit(artifact, server, " one");
  await savedEdit(artifact, server, " two");
  const doc = new Y.Doc();
  Y.applyUpdate(doc, Y.encodeStateAsUpdate(server), SERVER);
  const edits = await storeFor(artifact, doc.clientID);
  const drops: (readonly PendingKey[])[] = [];
  const store: PendingEdits = {
    ...edits,
    drop(keys) {
      drops.push(keys);
      return edits.drop(keys);
    },
  };
  const sync = startPendingSync({
    doc,
    onChange() {},
    remoteOrigin: SERVER,
    store: Promise.resolve(store),
  });
  cleanups.push(() => sync.destroy());

  await admit(sync, false);

  expect(drops).toHaveLength(1);
  expect(drops[0]).toHaveLength(3);
  expect(await rowsOf(artifact)).toEqual([
    [artifact, doc.clientID, 1],
    [artifact, doc.clientID, 2],
  ]);
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
      typeInto(doc, "first");
      if (update === undefined) {
        throw new Error("a local edit emits an update");
      }
      return update;
    })();
    sync.frameWritten(updateFrame(first));
    await sync.frameReceived(syncStatusFrame(true));
    expect(await rowsOf(artifact)).toEqual([[artifact, doc.clientID, 1]]);

    typeInto(doc, " second");
    sync.frameWritten(syncStep2Frame(Y.encodeStateAsUpdate(doc)));
    await sync.frameReceived(syncStatusFrame(true));

    expect(await rowsOf(artifact)).toEqual([]);
  } finally {
    logged.mockRestore();
  }
});

test("an edit recorded while a SyncStep2 acknowledgement is clearing stays tracked for its own acknowledgement", async () => {
  let releaseFirstWrite = () => {};
  const firstWriteHeld = new Promise<void>((resolve) => {
    releaseFirstWrite = resolve;
  });
  let reachedHold = () => {};
  const holdReached = new Promise<void>((resolve) => {
    reachedHold = resolve;
  });
  let holdNextWrite = true;
  // The first row's IndexedDB write stays open, as a slow readwrite transaction does, so the
  // SyncStep2 acknowledgement that covers it waits on it while the reader keeps typing.
  const wrapStore = (edits: PendingEdits): PendingEdits => ({
    ...edits,
    record(update) {
      const row = edits.record(update);
      if (!holdNextWrite) {
        return row;
      }
      holdNextWrite = false;
      return {
        seq: row.seq,
        written: row.written.then(() => {
          reachedHold();
          return firstWriteHeld;
        }),
      };
    },
  });
  const logged = spyOn(console, "error").mockImplementation(() => {});
  const { artifact, doc, sync, type } = await scripted(artifactId(), undefined, wrapStore);
  try {
    sync.socketOpened();
    type("first");
    sync.frameWritten(syncStep2Frame(Y.encodeStateAsUpdate(doc)));
    const clearing = sync.frameReceived(syncStatusFrame(true));
    // The acknowledgement has passed its read of the recorded rows and now waits on the held write.
    await holdReached;

    const second = type(" second");
    releaseFirstWrite();
    await clearing;
    sync.frameWritten(updateFrame(second));
    await sync.frameReceived(syncStatusFrame(true));

    expect(logged).not.toHaveBeenCalled();
    expect(await rowsOf(artifact)).toEqual([]);
  } finally {
    logged.mockRestore();
  }
});
