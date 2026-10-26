import { MessageType } from "@hocuspocus/provider";
import { createDecoder, readVarUint } from "lib0/decoding";
import { messageYjsSyncStep2, messageYjsUpdate } from "y-protocols/sync";
import * as Y from "yjs";

import type { PendingEdits, PendingKey, RestoredEdit } from "./pending-edits";
import { FRAGMENT_NAME, MARKS_MAP_NAME } from "./roots";
import { readField } from "./sync-frame";

export interface PendingState {
  /** Rows this document holds across every tab in this browser. */
  count: number;
  /** This admission cannot send the rows it holds. */
  readOnly: boolean;
  /** When the oldest dropped row was saved before a document rebuild. */
  rebuiltAt: number | undefined;
  /** Whether this browser has an IndexedDB store for these edits. */
  stored: boolean;
}

export interface PendingSync {
  /** Starts a fresh acknowledgement sequence for a newly opened socket. */
  socketOpened(): void;
  /** Counts a SyncStep2 or Update frame that actually reached the socket. */
  frameWritten(frame: Uint8Array): void;
  /** Pairs an incoming SyncStatus frame with the next counted sent frame. */
  frameReceived(frame: Uint8Array): Promise<void>;
  /**
   * Restores prior-page rows after this admission's initial sync. A read-only admission restores
   * none, records no local update and drops the stored rows that change nothing.
   */
  firstSync(readOnly: boolean): Promise<void>;
  /**
   * Takes a later admission of the same connection, once the reconnect's sync has applied: whether
   * it is read-only decides from then on whether a local update is a person's edit to store. One
   * that makes a read-only connection writable restores the stored rows as a writable first sync
   * does, since the first sync left them for a writable admission to send; it judges each row
   * against this document, so a row that builds on what the server took meanwhile applies.
   */
  admitted(readOnly: boolean): Promise<void>;
  destroy(): void;
}

interface RecordedUpdate {
  ordinal: number;
  seq: number | undefined;
  update: Uint8Array;
  written: Promise<void>;
}

type SentFrame = { kind: "update"; update: Uint8Array } | { kind: "step2"; through: number };

export interface PendingSyncOptions {
  doc: Y.Doc;
  onChange(state: PendingState): void;
  /** Updates received from the provider must never be stored as local edits. */
  remoteOrigin: unknown;
  /** Opening IndexedDB must not delay constructing the provider. */
  store: Promise<PendingEdits | undefined>;
}

function equals(left: Uint8Array, right: Uint8Array): boolean {
  return left.length === right.length && left.every((byte, index) => byte === right[index]);
}

function covers(
  ranges: readonly { clock: number; len: number }[],
  clock: number,
  len: number
): boolean {
  let cursor = clock;
  const end = clock + len;
  for (const range of ranges) {
    if (range.clock > cursor) {
      return false;
    }
    cursor = Math.max(cursor, range.clock + range.len);
    if (cursor >= end) {
      return true;
    }
  }
  return false;
}

/** Whether applying an update added a parked delete that the live document did not already hold. */
function addsPendingDeletes(before: Uint8Array | null, after: Uint8Array | null): boolean {
  if (after === null) {
    return false;
  }
  if (before === null) {
    return true;
  }
  const beforeDeletes = Y.decodeUpdateV2(before).ds.clients;
  const afterDeletes = Y.decodeUpdateV2(after).ds.clients;
  for (const [client, afterRanges] of afterDeletes) {
    const beforeRanges = beforeDeletes.get(client) ?? [];
    for (const range of afterRanges) {
      if (!covers(beforeRanges, range.clock, range.len)) {
        return true;
      }
    }
  }
  return false;
}

/** A copy of `doc` to test restored rows on, so the live document never takes a row that parks. */
function scratchOf(doc: Y.Doc): Y.Doc {
  const scratch = new Y.Doc();
  Y.applyUpdate(scratch, Y.encodeStateAsUpdate(doc));
  return scratch;
}

/**
 * Applies a restored row to `scratch` and says whether it parked something the scratch did not
 * already park: a missing client, or a delete of items it does not hold, which is what a row typed
 * against a history the server rebuilt since does. yjs 13.6.32 exposes parked structs and deletes
 * only through these `StructStore` internals. `encodeStateAsUpdate` carries the live document's
 * own parked structs and deletes into the scratch, so only what the row adds counts.
 */
function addsParking(scratch: Y.Doc, update: Uint8Array): boolean {
  const beforeMissing = new Set(scratch.store.pendingStructs?.missing.keys() ?? []);
  const beforeDeletes = scratch.store.pendingDs;

  Y.applyUpdate(scratch, update);

  const afterMissing = scratch.store.pendingStructs?.missing;
  const addsMissingClient = [...(afterMissing?.keys() ?? [])].some(
    (client) => !beforeMissing.has(client)
  );
  return addsMissingClient || addsPendingDeletes(beforeDeletes, scratch.store.pendingDs);
}

/** An object's entries ordered by key, so the order Yjs keeps them in does not count. */
function sortedEntries(record: Record<string, unknown>): [string, unknown][] {
  return Object.entries(record).sort(([left], [right]) => left.localeCompare(right));
}

/** One run of an `XmlText` delta: its text or embed, and the formats it carries. */
interface DeltaRun {
  insert: unknown;
  attributes?: Record<string, unknown>;
}

/**
 * The runs a reader sees of a text: adjacent text of the same formats is joined, so how Yjs
 * happens to split the same text into items does not count as a change.
 */
function readableRuns(delta: readonly DeltaRun[]): unknown[] {
  const runs: { text?: string; embed?: unknown; marks: [string, unknown][] }[] = [];
  for (const { insert, attributes } of delta) {
    const marks = sortedEntries(attributes ?? {});
    const last = runs.at(-1);
    if (typeof insert !== "string") {
      runs.push({ embed: readableValue(insert), marks });
    } else if (last?.text !== undefined && JSON.stringify(last.marks) === JSON.stringify(marks)) {
      last.text += insert;
    } else {
      runs.push({ text: insert, marks });
    }
  }
  return runs;
}

/** What a reader sees of one node of the editor's tree: its name, attributes and content. */
function readableNode(node: Y.XmlElement | Y.XmlText | Y.XmlHook): unknown {
  if (node instanceof Y.XmlText) {
    return ["text", sortedEntries(node.getAttributes()), readableRuns(node.toDelta())];
  }
  if (node instanceof Y.XmlElement) {
    return [node.nodeName, sortedEntries(node.getAttributes()), node.toArray().map(readableNode)];
  }
  return [node.hookName, sortedEntries(node.toJSON())];
}

/** What a reader sees of an embed: a node of the tree read as one, another type as JSON. */
function readableValue(value: unknown): unknown {
  if (value instanceof Y.XmlElement || value instanceof Y.XmlText || value instanceof Y.XmlHook) {
    return readableNode(value);
  }
  return value instanceof Y.AbstractType ? value.toJSON() : value;
}

/**
 * Everything a reader of `scratch` sees, as one string to compare: the editor's tree and the
 * margin records, the only two roots a Dispatch document holds. A root nobody has asked for by
 * its class yet - the editor's fragment before the editor exists - is a bare type that shows
 * nothing, so both are asked for by their class here. That defines them on the document it is
 * called on, so it is only ever called on a scratch copy, never on the live document the editor
 * binds. Only public reads are used: children, names, attributes, a text's delta with its
 * formats, and the margin records as JSON. Values are compared as JSON, so an object attribute is
 * compared by what it holds rather than printed as `[object Object]`.
 */
function readable(scratch: Y.Doc): string {
  return JSON.stringify([
    scratch.getXmlFragment(FRAGMENT_NAME).toArray().map(readableNode),
    sortedEntries(scratch.getMap(MARKS_MAP_NAME).toJSON()),
  ]);
}

/**
 * Whether one browser client's stored rows, applied in order to a copy of `doc`, park nothing and
 * leave what the copy shows as it was. Rows the document already holds do; so do rows whose only
 * change the editor of this load derived again. Rows that park never do, so a row typed against a
 * rebuilt history stays named. A copy that holds any root but the tree and the margin records,
 * before or after the rows, holds something the comparison cannot read, so its rows are kept
 * rather than risk dropping a person's unsent edit. Applying a row adds roots and never removes
 * one, so the roots after the rows cover those before.
 */
function changesNothing(doc: Y.Doc, updates: readonly Uint8Array[]): boolean {
  const scratch = scratchOf(doc);
  const before = readable(scratch);
  return (
    updates.every((update) => !addsParking(scratch, update)) &&
    [...scratch.share.keys()].every((name) => name === FRAGMENT_NAME || name === MARKS_MAP_NAME) &&
    readable(scratch) === before
  );
}

export function startPendingSync({
  doc,
  onChange,
  remoteOrigin,
  store,
}: PendingSyncOptions): PendingSync {
  let destroyed = false;
  let readOnly = false;
  let rebuiltAt: number | undefined;
  let lastState: PendingState | undefined;
  let nextOrdinal = 0;
  let reporting: Promise<void> | undefined;
  let reportAgain = false;
  let storageAvailable: boolean | undefined;
  const persistedSeqByOrdinal = new Map<number, number>();
  let recorded: RecordedUpdate[] = [];
  let sent: SentFrame[] = [];

  const ready = store
    .catch((error: unknown) => {
      console.error("Could not open the browser's pending document edits", error);
      return undefined;
    })
    .then((edits) => {
      storageAvailable = edits !== undefined;
      if (!storageAvailable) {
        recorded = [];
        sent = [];
      }
      return edits;
    });
  const report = (): Promise<void> => {
    if (reporting !== undefined) {
      reportAgain = true;
      return reporting;
    }
    reporting = (async () => {
      do {
        reportAgain = false;
        const edits = await ready;
        if (destroyed) {
          return;
        }
        const state: PendingState = {
          count: edits === undefined ? 0 : await edits.count(),
          readOnly,
          rebuiltAt,
          stored: edits !== undefined,
        };
        if (
          lastState === undefined ||
          lastState.count !== state.count ||
          lastState.readOnly !== state.readOnly ||
          lastState.rebuiltAt !== state.rebuiltAt ||
          lastState.stored !== state.stored
        ) {
          lastState = state;
          onChange(state);
        }
      } while (reportAgain);
    })()
      .catch((error: unknown) => {
        console.error("Could not count the browser's pending document edits", error);
      })
      .finally(() => {
        reporting = undefined;
      });
    return reporting;
  };

  const record = (update: Uint8Array): RecordedUpdate => {
    nextOrdinal += 1;
    const entry: RecordedUpdate = {
      ordinal: nextOrdinal,
      seq: undefined,
      update,
      written: Promise.resolve(),
    };
    recorded.push(entry);
    entry.written = ready
      .then((edits) => {
        if (edits === undefined) {
          return;
        }
        const row = edits.record(update);
        entry.seq = row.seq;
        persistedSeqByOrdinal.set(entry.ordinal, row.seq);
        return row.written;
      })
      .then(
        () => report(),
        (error: unknown) => {
          console.error("Could not store a pending document edit", error);
        }
      );
    return entry;
  };

  // The stored rows of every client but this tab's. This tab's own rows hold edits this document
  // already has: applying them again records nothing anew, and judged against this document they
  // always look as if they changed nothing, so restoring or pruning them would drop what the
  // server may not have acknowledged yet.
  const otherClientsRows = (rows: readonly RestoredEdit[]): RestoredEdit[] =>
    rows.filter((row) => row.key[1] !== doc.clientID);

  // Judges the stored rows of a read-only admission again, one pass at a time: a pass queued while
  // none has started yet covers every request made before it starts.
  let pruning: Promise<void> = Promise.resolve();
  let pruneQueued = false;
  const dropRedundantRows = (): Promise<void> => {
    if (pruneQueued) {
      return pruning;
    }
    pruneQueued = true;
    pruning = pruning.then(async () => {
      pruneQueued = false;
      try {
        const edits = await ready;
        if (destroyed || edits === undefined) {
          return;
        }
        const rows = await edits.restoreRows();
        if (destroyed) {
          return;
        }
        // A client's rows are judged and dropped together: each continues the clock of the row
        // before it, so a later row cannot apply without the earlier ones. A writable load whose
        // socket was down stores its editor's heading-id stamp first and what its reader typed
        // after; a read-only load that derives the same stamp leaves that first row alone
        // changing nothing, yet dropping it would lose the typing.
        const byClient = new Map<number, RestoredEdit[]>();
        for (const row of otherClientsRows(rows)) {
          const client = byClient.get(row.key[1]);
          if (client === undefined) {
            byClient.set(row.key[1], [row]);
          } else {
            client.push(row);
          }
        }
        const redundant: PendingKey[] = [];
        for (const clientRows of byClient.values()) {
          const updates = clientRows.map((row) => row.update);
          if (changesNothing(doc, updates)) {
            redundant.push(...clientRows.map((row) => row.key));
          }
        }
        if (redundant.length > 0) {
          await edits.drop(redundant);
        }
      } catch (error) {
        console.error("Could not drop the browser's redundant pending document edits", error);
      }
      await report();
    });
    return pruning;
  };

  const onUpdate = (update: Uint8Array, origin: unknown) => {
    if (destroyed || storageAvailable === false || origin === remoteOrigin) {
      return;
    }
    // A read-only admission's editor takes no typing, so a local update it makes is one the editor
    // derives from the document as loaded - Milkdown stamps each heading's id from its text, for
    // one - which the server refuses and the next load derives again: no edit of a person's to
    // keep. What it derived can make a row an earlier load stored the same way change nothing, so
    // the stored rows are judged again.
    if (readOnly) {
      void dropRedundantRows();
      return;
    }
    record(update);
  };
  doc.on("update", onUpdate);

  const clearUpdate = async (frame: SentFrame & { kind: "update" }): Promise<void> => {
    const edits = await ready;
    if (edits === undefined) {
      return;
    }
    // A provider Update frame carries exactly the Yjs event bytes. Separate local Yjs updates
    // have distinct client-id/clock ranges, so only a retry of this same row can match these bytes.
    const index = recorded.findIndex((entry) => equals(entry.update, frame.update));
    if (index === -1) {
      console.error("The document server acknowledged an update this browser did not store.");
      return;
    }
    const [entry] = recorded.splice(index, 1);
    if (entry === undefined) {
      return;
    }
    await entry.written;
    if (entry.seq === undefined) {
      return;
    }
    await edits.clear(entry.seq);
    persistedSeqByOrdinal.delete(entry.ordinal);
    await report();
  };

  const clearThrough = async (through: number): Promise<void> => {
    const edits = await ready;
    if (edits === undefined || through === 0) {
      return;
    }
    const covered = recorded.filter((entry) => entry.ordinal <= through);
    await Promise.all(covered.map((entry) => entry.written));
    // Filter the list as it is now, not as it was before the await: an edit typed while those
    // writes were pending was appended since, and stays tracked for its own acknowledgement.
    const coveredSet = new Set(covered);
    recorded = recorded.filter((entry) => !coveredSet.has(entry));

    let seq = 0;
    const acknowledged: number[] = [];
    for (const [ordinal, persistedSeq] of persistedSeqByOrdinal) {
      if (ordinal <= through) {
        seq = Math.max(seq, persistedSeq);
        acknowledged.push(ordinal);
      }
    }
    if (seq > 0) {
      await edits.clearThrough(seq);
      for (const ordinal of acknowledged) {
        persistedSeqByOrdinal.delete(ordinal);
      }
    }
    await report();
  };

  void report();

  // An admission of this connection, first or later: it decides whether a local update is a
  // person's edit to store (onUpdate), and a writable one restores the rows other tabs and earlier
  // pages left.
  const admit = async (nextReadOnly: boolean): Promise<void> => {
    readOnly = nextReadOnly;
    const edits = await ready;
    if (destroyed || edits === undefined) {
      await report();
      return;
    }
    if (readOnly) {
      // A read-only admission applies no row: each stays for a writable one to send, except a
      // row that changes nothing, which an earlier read-only load may have stored.
      await dropRedundantRows();
      return;
    }
    try {
      const restored = otherClientsRows(await edits.restoreRows());
      if (destroyed || restored.length === 0) {
        await report();
        return;
      }
      // Every row is judged and applied in one synchronous pass, so the scratch copy stays the
      // live document's equal: it takes each row the live document takes, and is copied afresh
      // only after a row it refused, whose parked structs would otherwise hide the next stale
      // row from the same lost history. One document encode, plus one per refused row.
      let scratch = scratchOf(doc);
      const handled: PendingKey[] = [];
      const rerecorded: Promise<void>[] = [];
      for (const row of restored) {
        handled.push(row.key);
        if (addsParking(scratch, row.update)) {
          rebuiltAt = Math.min(rebuiltAt ?? row.at, row.at);
          scratch = scratchOf(doc);
          continue;
        }
        const before = nextOrdinal;
        // Applying through the live document deliberately reuses the normal local-update path:
        // the provider sends this restored diff and this listener records its new client row.
        Y.applyUpdate(doc, row.update, { restored: true });
        const restoredEntry = recorded.at(-1);
        if (nextOrdinal !== before && restoredEntry !== undefined) {
          rerecorded.push(restoredEntry.written);
        }
      }
      // The old rows go only once every re-recorded row is stored, in one IndexedDB write.
      await Promise.all(rerecorded);
      // A destroy here leaves the rows; the next first sync re-applies them, which changes nothing.
      if (!destroyed) {
        await edits.drop(handled);
      }
      await report();
    } catch (error) {
      console.error("Could not restore the browser's pending document edits", error);
      await report();
    }
  };

  return {
    socketOpened() {
      if (!destroyed) {
        sent = [];
      }
    },
    frameWritten(frame) {
      if (destroyed || storageAvailable === false) {
        return;
      }
      try {
        const decoder = createDecoder(frame);
        readField(decoder); // the document name
        if (readVarUint(decoder) !== MessageType.Sync) {
          return;
        }
        switch (readVarUint(decoder)) {
          case messageYjsSyncStep2:
            // A SyncStep2 is acknowledged as covering every row recorded so far, so only its
            // bounds are checked; its content is never copied.
            readField(decoder);
            sent.push({ kind: "step2", through: nextOrdinal });
            break;
          case messageYjsUpdate:
            sent.push({ kind: "update", update: readField(decoder) });
            break;
        }
      } catch (error) {
        console.error("Could not parse an outgoing document sync frame", error);
      }
    },
    async frameReceived(frame) {
      if (destroyed) {
        return;
      }
      let applied: boolean;
      try {
        const decoder = createDecoder(frame);
        readField(decoder); // the document name
        if (readVarUint(decoder) !== MessageType.SyncStatus) {
          return;
        }
        // The server writes the flag as a VarUint: 1 applied, 0 not applied.
        applied = readVarUint(decoder) === 1;
      } catch (error) {
        console.error("Could not parse a document SyncStatus frame", error);
        return;
      }
      const entry = sent.shift();
      if (entry === undefined) {
        console.error("The document server acknowledged a frame this browser did not send.");
        return;
      }
      if (!applied) {
        await Promise.all(recorded.map((record) => record.written));
        return;
      }
      try {
        if (entry.kind === "update") {
          await clearUpdate(entry);
        } else {
          await clearThrough(entry.through);
        }
      } catch (error) {
        console.error("Could not clear an acknowledged pending document edit", error);
      }
    },
    firstSync: admit,
    async admitted(nextReadOnly) {
      if (nextReadOnly !== readOnly) {
        await admit(nextReadOnly);
      }
    },
    destroy() {
      if (destroyed) {
        return;
      }
      destroyed = true;
      doc.off("update", onUpdate);
      void ready.then((edits) => edits?.close());
    },
  };
}
