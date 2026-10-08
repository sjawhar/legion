import { mergeUpdates } from "yjs";

/**
 * The edits a browser made to a live document that the server has not yet said it applied, kept
 * in IndexedDB so a reload does not lose them. One database for every document, one object store
 * of rows keyed `[artifactId, clientId, seq]`: each tab writes under its own document's client id,
 * so no tab overwrites another's rows, and `seq` counts that client's edits in the order it made
 * them. `pending-sync.ts` decides what is recorded and when a row is cleared.
 */

const DATABASE = "dispatch-pending-edits";
const STORE = "edits";

export type PendingKey = [artifactId: string, clientId: number, seq: number];

interface PendingRow {
  artifactId: string;
  clientId: number;
  seq: number;
  update: Uint8Array;
  /** When the edit was recorded, in milliseconds since the epoch. */
  at: number;
}

export interface RestoredEdits {
  /** Every row's update, merged into one. */
  update: Uint8Array;
  /** The rows read, so the caller deletes exactly those. */
  keys: PendingKey[];
  /** When the oldest of them was recorded, in milliseconds since the epoch. */
  at: number;
}

export interface RestoredEdit {
  /** The exact persisted row key to delete only after this update is handled. */
  key: PendingKey;
  /** The Yjs update this one row holds. */
  update: Uint8Array;
  /** When this row was recorded, in milliseconds since the epoch. */
  at: number;
}

export interface PendingEdits {
  /** Appends `update` as this client's next row. The sequence number is assigned at once;
   * `written` settles when the row is stored. */
  record(update: Uint8Array): { seq: number; written: Promise<void> };
  /** Deletes this client's row at exactly `seq`. */
  clear(seq: number): Promise<void>;
  /** Deletes this client's rows at `seq` and below. */
  clearThrough(seq: number): Promise<void>;
  /** Every row this document holds, from any client, in IndexedDB key order. */
  restoreRows(): Promise<readonly RestoredEdit[]>;
  /** Every row this document holds, merged; `undefined` when there are none. */
  restore(): Promise<RestoredEdits | undefined>;
  /** Deletes exactly the rows `keys` names. */
  drop(keys: readonly PendingKey[]): Promise<void>;
  /** How many rows this document holds, from every client. */
  count(): Promise<number>;
  close(): void;
}

function settled<T>(request: IDBRequest<T>): Promise<T> {
  const { promise, resolve, reject } = Promise.withResolvers<T>();
  request.onsuccess = () => resolve(request.result);
  request.onerror = () => reject(request.error);
  return promise;
}

function committed(transaction: IDBTransaction): Promise<void> {
  const { promise, resolve, reject } = Promise.withResolvers<void>();
  transaction.oncomplete = () => resolve();
  transaction.onerror = () => reject(transaction.error);
  transaction.onabort = () => reject(transaction.error ?? new Error("IndexedDB aborted a write"));
  return promise;
}

/** Every key of one document: an array key sorts before any longer array that extends it, and
 * an array sorts after every number, so `[artifactId, []]` follows every `[artifactId, n, m]`. */
function documentRange(artifactId: string): IDBKeyRange {
  return IDBKeyRange.bound([artifactId], [artifactId, []]);
}

const REFUSED = "Edits typed in a document will not survive a reload: IndexedDB refused";

/** The database, or `undefined` when this browser has no IndexedDB or refuses to open it (a
 * private window, a storage policy, a quota error). */
function openDatabase(): Promise<IDBDatabase | undefined> {
  const factory: IDBFactory | undefined = globalThis.indexedDB;
  if (factory === undefined) {
    return Promise.resolve(undefined);
  }
  let request: IDBOpenDBRequest;
  try {
    request = factory.open(DATABASE, 1);
  } catch (error) {
    console.error(REFUSED, error);
    return Promise.resolve(undefined);
  }
  const { promise, resolve } = Promise.withResolvers<IDBDatabase | undefined>();
  request.onupgradeneeded = () => {
    request.result.createObjectStore(STORE, { keyPath: ["artifactId", "clientId", "seq"] });
  };
  request.onsuccess = () => resolve(request.result);
  request.onerror = () => {
    console.error(REFUSED, request.error);
    resolve(undefined);
  };
  return promise;
}

export async function openPendingEdits(
  artifactId: string,
  clientId: number
): Promise<PendingEdits | undefined> {
  const database = await openDatabase();
  if (database === undefined) {
    return undefined;
  }
  const last = await settled(
    database
      .transaction(STORE, "readonly")
      .objectStore(STORE)
      .openKeyCursor(
        IDBKeyRange.bound([artifactId, clientId, 0], [artifactId, clientId, Number.MAX_VALUE]),
        "prev"
      )
  );
  let seq = last === null ? 0 : (last.primaryKey as PendingKey)[2];

  const write = (change: (store: IDBObjectStore) => void): Promise<void> => {
    const transaction = database.transaction(STORE, "readwrite");
    change(transaction.objectStore(STORE));
    return committed(transaction);
  };

  const restoreRows = async (): Promise<readonly RestoredEdit[]> => {
    const rows = (await settled(
      database.transaction(STORE, "readonly").objectStore(STORE).getAll(documentRange(artifactId))
    )) as PendingRow[];
    return rows.map(
      (row): RestoredEdit => ({
        at: row.at,
        key: [row.artifactId, row.clientId, row.seq],
        update: row.update,
      })
    );
  };

  return {
    record(update) {
      seq += 1;
      const row: PendingRow = { artifactId, at: Date.now(), clientId, seq, update };
      return { seq, written: write((store) => store.add(row)) };
    },
    clear(at) {
      return write((store) => store.delete([artifactId, clientId, at]));
    },
    clearThrough(through) {
      return write((store) =>
        store.delete(IDBKeyRange.bound([artifactId, clientId, 0], [artifactId, clientId, through]))
      );
    },
    restoreRows,
    async restore() {
      const rows = await restoreRows();
      if (rows.length === 0) {
        return undefined;
      }
      return {
        at: Math.min(...rows.map((row) => row.at)),
        keys: rows.map((row) => row.key),
        update: mergeUpdates(rows.map((row) => row.update)),
      };
    },
    drop(keys) {
      return write((store) => {
        for (const key of keys) {
          store.delete(key);
        }
      });
    },
    count() {
      return settled(
        database.transaction(STORE, "readonly").objectStore(STORE).count(documentRange(artifactId))
      );
    },
    close() {
      database.close();
    },
  };
}

/** Deletes every row a document holds, from every client: the server said the document is gone. */
export async function deletePendingEdits(artifactId: string): Promise<void> {
  const database = await openDatabase();
  if (database === undefined) {
    return;
  }
  try {
    const transaction = database.transaction(STORE, "readwrite");
    transaction.objectStore(STORE).delete(documentRange(artifactId));
    await committed(transaction);
  } finally {
    database.close();
  }
}
