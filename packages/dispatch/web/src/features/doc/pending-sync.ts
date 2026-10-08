import {
  createDecoder,
  readVarInt,
  readVarString,
  readVarUint,
  readVarUint8Array,
} from "lib0/decoding";
import * as Y from "yjs";

import type { PendingEdits } from "./pending-edits";

const MESSAGE_SYNC = 0;
const MESSAGE_SYNC_STATUS = 8;
const SYNC_STEP_2 = 1;
const SYNC_UPDATE = 2;

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
  let reportTail = Promise.resolve();
  const recorded: RecordedUpdate[] = [];
  let sent: SentFrame[] = [];

  const ready = store.catch((error: unknown) => {
    console.error("Could not open the browser's pending document edits", error);
    return undefined;
  });

  const report = (): Promise<void> => {
    reportTail = reportTail
      .then(async () => {
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
      })
      .catch((error: unknown) => {
        console.error("Could not count the browser's pending document edits", error);
      });
    return reportTail;
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
    if (!destroyed && origin !== remoteOrigin) {
      record(update);
    }
  };
  doc.on("update", onUpdate);

  const clearUpdate = async (frame: SentFrame & { kind: "update" }): Promise<void> => {
    const edits = await ready;
    if (edits === undefined) {
      return;
    }
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
    await report();
  };

  const clearThrough = async (through: number): Promise<void> => {
    const edits = await ready;
    if (edits === undefined || through === 0) {
      return;
    }
    const covered = recorded.filter((entry) => entry.ordinal <= through);
    if (covered.length === 0) {
      return;
    }
    await Promise.all(covered.map((entry) => entry.written));
    const seq = Math.max(...covered.map((entry) => entry.seq ?? 0));
    if (seq === 0) {
      return;
    }
    for (const entry of covered) {
      const index = recorded.indexOf(entry);
      if (index !== -1) {
        recorded.splice(index, 1);
      }
    }
    await edits.clearThrough(seq);
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
      if (destroyed) {
        return;
      }
      const decoder = createDecoder(frame);
      readVarString(decoder);
      if (readVarUint(decoder) !== MESSAGE_SYNC) {
        return;
      }
      switch (readVarUint(decoder)) {
        case SYNC_STEP_2:
          readVarUint8Array(decoder);
          sent.push({ kind: "step2", through: nextOrdinal });
          break;
        case SYNC_UPDATE:
          sent.push({ kind: "update", update: readVarUint8Array(decoder) });
          break;
      }
    },
    async frameReceived(frame) {
      if (destroyed) {
        return;
      }
      const decoder = createDecoder(frame);
      readVarString(decoder);
      if (readVarUint(decoder) !== MESSAGE_SYNC_STATUS) {
        return;
      }
      const entry = sent.shift();
      if (entry === undefined) {
        console.error("The document server acknowledged a frame this browser did not send.");
        return;
      }
      const applied = readVarInt(decoder) === 1;
      if (!applied) {
        await Promise.all(recorded.map((record) => record.written));
        return;
      }
      if (entry.kind === "update") {
        await clearUpdate(entry);
      } else {
        await clearThrough(entry.through);
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
        const restored = await edits.restore();
        if (destroyed || restored === undefined) {
          await report();
          return;
        }
        if (addsParking(doc, restored.update)) {
          rebuiltAt = restored.at;
          await edits.drop(restored.keys);
          await report();
          return;
        }

        const before = nextOrdinal;
        Y.applyUpdate(doc, restored.update, { restored: true });
        const restoredEntry = recorded.at(-1);
        if (nextOrdinal === before || restoredEntry === undefined) {
          await edits.drop(restored.keys);
        } else {
          await restoredEntry.written;
          if (!destroyed) {
            await edits.drop(restored.keys);
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
