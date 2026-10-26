/**
 * The files of the uploads the page keeps across a reload (`write-store.ts`): `sessionStorage`
 * holds a write's request, but no entry can hold a `File`, so each upload's is kept here, in
 * IndexedDB, under its write's key, from its press until the write lands or is discarded, and a
 * reload resumes the upload with it. IndexedDB is the origin's, not the tab's, so a row also names
 * the login that pressed it and when: another person's rows, and any older than a day (Dispatch
 * forgets a key after `IDEMPOTENCY_KEY_TTL_MS`, and a tab closed with an upload still kept leaves
 * its row behind), are deleted as the page resumes.
 */
import { IDEMPOTENCY_KEY_TTL_MS } from "@legion/contracts";

const DATABASE = "dispatch-kept-files";
const STORE = "files";

interface KeptFileRow {
  readonly key: string;
  readonly login: string;
  readonly at: number;
  readonly file: File;
}

/** Where kept files live: IndexedDB in the page, an in-memory map in a test. */
export interface KeptFiles {
  put(key: string, login: string, file: File): Promise<void>;
  get(key: string): Promise<File | undefined>;
  delete(key: string): Promise<void>;
  /** Deletes every row but `login`'s from within the last day; every row, for `undefined`. */
  keepOnly(login: string | undefined, now?: number): Promise<void>;
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

/** The database, or `undefined` when this browser has no IndexedDB or refuses to open it (a
 *  private window, a storage policy, a quota error): an upload is then kept without its file, and
 *  a reload lets it go. */
function openDatabase(): Promise<IDBDatabase | undefined> {
  const factory: IDBFactory | undefined = globalThis.indexedDB;
  if (factory === undefined) return Promise.resolve(undefined);
  let request: IDBOpenDBRequest;
  try {
    request = factory.open(DATABASE, 1);
  } catch {
    return Promise.resolve(undefined);
  }
  const { promise, resolve } = Promise.withResolvers<IDBDatabase | undefined>();
  request.onupgradeneeded = () => {
    request.result.createObjectStore(STORE, { keyPath: "key" });
  };
  request.onsuccess = () => resolve(request.result);
  request.onerror = () => resolve(undefined);
  return promise;
}

/** The page's kept files, in IndexedDB. Every call opens the database and closes it again, so no
 *  connection is held while nothing is kept; a failure keeps nothing and loses no write. */
export function indexedDbFiles(): KeptFiles {
  const run = async <T>(
    mode: IDBTransactionMode,
    work: (store: IDBObjectStore) => Promise<T>
  ): Promise<T | undefined> => {
    const database = await openDatabase();
    if (database === undefined) return undefined;
    try {
      const transaction = database.transaction(STORE, mode);
      const [result] = await Promise.all([
        work(transaction.objectStore(STORE)),
        committed(transaction),
      ]);
      return result;
    } catch {
      return undefined;
    } finally {
      database.close();
    }
  };
  return {
    delete: async (key) => {
      await run("readwrite", (store) => settled(store.delete(key)));
    },
    get: async (key) => {
      const row = (await run("readonly", (store) => settled(store.get(key)))) as
        | KeptFileRow
        | undefined;
      return row?.file instanceof File ? row.file : undefined;
    },
    keepOnly: async (login, now = Date.now()) => {
      await run("readwrite", async (store) => {
        const rows = (await settled(store.getAll())) as KeptFileRow[];
        await Promise.all(
          rows
            .filter((row) => row.login !== login || now - row.at > IDEMPOTENCY_KEY_TTL_MS)
            .map((row) => settled(store.delete(row.key)))
        );
      });
    },
    put: async (key, login, file) => {
      const row: KeptFileRow = { at: Date.now(), file, key, login };
      await run("readwrite", (store) => settled(store.put(row)));
    },
  };
}
