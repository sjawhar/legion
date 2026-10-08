import { MessageType } from "@hocuspocus/provider";
import { createDecoder, type Decoder, readVarUint } from "lib0/decoding";
import { messageYjsSyncStep2, messageYjsUpdate } from "y-protocols/sync";
import * as Y from "yjs";

import type { PendingEdits } from "./pending-edits";

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
  /** Restores prior-page rows after this admission's initial sync. */
  firstSync(readOnly: boolean): Promise<void>;
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

/**
 * Reads one length-prefixed field as a view of the frame. lib0's own `readVarUint8Array` builds
 * its view from the frame's backing buffer, so a frame that is a view into a larger buffer reads
 * past its own end instead of failing; this refuses a field longer than what is left.
 */
function readField(decoder: Decoder): Uint8Array {
  const length = readVarUint(decoder);
  const start = decoder.pos;
  decoder.pos += length;
  if (decoder.pos > decoder.arr.length) {
    throw new RangeError("truncated document sync frame");
  }
  return decoder.arr.subarray(start, decoder.pos);
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

/**
 * Applying a restore to a scratch document protects the live document from a history rebuild.
 * yjs 13.6.32 exposes parked structs and deletes only through these `StructStore` internals.
 * `encodeStateAsUpdate` includes the live document's current parked structs and deletes, so only
 * parking the restore adds is a rebuild signal.
 */
function addsParking(doc: Y.Doc, update: Uint8Array): boolean {
  const scratch = new Y.Doc();
  Y.applyUpdate(scratch, Y.encodeStateAsUpdate(doc));
  const beforeMissing = new Set(scratch.store.pendingStructs?.missing.keys() ?? []);
  const beforeDeletes = scratch.store.pendingDs;

  Y.applyUpdate(scratch, update);

  const afterMissing = scratch.store.pendingStructs?.missing;
  const addsMissingClient = [...(afterMissing?.keys() ?? [])].some(
    (client) => !beforeMissing.has(client)
  );
  return addsMissingClient || addsPendingDeletes(beforeDeletes, scratch.store.pendingDs);
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
  const recorded: RecordedUpdate[] = [];
  let sent: SentFrame[] = [];

  const ready = store
    .catch((error: unknown) => {
      console.error("Could not open the browser's pending document edits", error);
      return undefined;
    })
    .then((edits) => {
      storageAvailable = edits !== undefined;
      if (!storageAvailable) {
        recorded.length = 0;
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

  const onUpdate = (update: Uint8Array, origin: unknown) => {
    if (!destroyed && storageAvailable !== false && origin !== remoteOrigin) {
      record(update);
    }
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
    const covered: RecordedUpdate[] = [];
    const remaining: RecordedUpdate[] = [];
    for (const entry of recorded) {
      (entry.ordinal <= through ? covered : remaining).push(entry);
    }
    await Promise.all(covered.map((entry) => entry.written));
    recorded.length = 0;
    recorded.push(...remaining);

    let seq = 0;
    for (const [ordinal, persistedSeq] of persistedSeqByOrdinal) {
      if (ordinal <= through) {
        seq = Math.max(seq, persistedSeq);
      }
    }
    if (seq > 0) {
      await edits.clearThrough(seq);
      for (const ordinal of persistedSeqByOrdinal.keys()) {
        if (ordinal <= through) {
          persistedSeqByOrdinal.delete(ordinal);
        }
      }
    }
    await report();
  };

  void report();

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
    async firstSync(nextReadOnly) {
      readOnly = nextReadOnly;
      const edits = await ready;
      if (destroyed || edits === undefined || readOnly) {
        await report();
        return;
      }
      try {
        const restored = await edits.restoreRows();
        if (destroyed || restored.length === 0) {
          await report();
          return;
        }
        for (const row of restored) {
          if (addsParking(doc, row.update)) {
            rebuiltAt = Math.min(rebuiltAt ?? row.at, row.at);
            await edits.drop([row.key]);
            continue;
          }

          const before = nextOrdinal;
          // Applying through the live document deliberately reuses the normal local-update path:
          // the provider sends this restored diff and this listener records its new client row.
          Y.applyUpdate(doc, row.update, { restored: true });
          const restoredEntry = recorded.at(-1);
          if (nextOrdinal === before || restoredEntry === undefined) {
            await edits.drop([row.key]);
          } else {
            await restoredEntry.written;
            if (!destroyed) {
              await edits.drop([row.key]);
            }
          }
        }
        await report();
      } catch (error) {
        console.error("Could not restore the browser's pending document edits", error);
        await report();
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
