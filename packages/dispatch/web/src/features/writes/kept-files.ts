/**
 * The files of the uploads the page keeps across a reload (`write-store.ts`): `sessionStorage`
 * holds a write's request, but no entry can hold a `File`, so each upload's is kept here, in
 * IndexedDB, under its write's key, from its press until the write lands or is discarded, and a
 * reload resumes the upload with it. A row holds the file's bytes, not the `File` itself - WebKit
 * stores no `File` and fails the write of a row holding one - with its name, type and
 * last-modified time beside them, from which `get` makes the `File` again. IndexedDB is the
 * origin's, not the tab's, so a row also names the login that pressed it and when: another
 * person's rows, and any older than a day (Dispatch forgets a key after `IDEMPOTENCY_KEY_TTL_MS`,
 * and a tab closed with an upload still kept leaves its row behind), are deleted as the page
 * resumes.
 */
import { IDEMPOTENCY_KEY_TTL_MS } from "@legion/contracts";

const DATABASE = "dispatch-kept-files";
const STORE = "files";

/** A row as this version writes it: the file's bytes, which every engine stores, and the three
 *  things a `File` is made from them again with. */
interface KeptFileRow {
  readonly key: string;
  readonly login: string;
  readonly at: number;
  readonly bytes: ArrayBuffer;
  readonly name: string;
  readonly type: string;
  readonly lastModified: number;
}

/** A row the release before this one wrote, holding the `File` itself. Rows of this shape are
 *  still in the stores of readers who loaded that release, so they are read - a reader whose page
 *  reloads across the deploy resumes their upload - but none is ever written again.
 *
 *  This shape has an end, and `keepOnly` is what ends it: every load of the page sweeps each row
 *  of the signed-in person older than `IDEMPOTENCY_KEY_TTL_MS`, and every row of anyone else
 *  whatever its age. So the last row of this shape is gone one key lifetime after the release
 *  reaches a reader, at that reader's next load - and a store nobody opens again is read by
 *  nobody either. Once every reader has loaded the page once a day after the deploy, this
 *  interface, `fileFrom`'s `file` branch and `EitherKeptFileRow` can go together. */
interface KeptFileRowBefore {
  readonly key: string;
  readonly login: string;
  readonly at: number;
  readonly file: File;
}

/** Every field either shape may hold: what a row is read as before its shape is known. */
type EitherKeptFileRow = Partial<KeptFileRow & KeptFileRowBefore>;

/** The `File` a row of either shape holds: the one the release before this one stored, or one
 *  made again from this version's bytes. The shapes are told apart by the field each has and the
 *  other never does, `file` against `bytes`, so neither can be read as the other, and a row that
 *  is neither - a half-written row, or one from a version after this - is nothing to resume with
 *  and is answered as absent. */
function fileFrom(row: unknown): File | undefined {
  if (typeof row !== "object" || row === null) return undefined;
  const { bytes, file, lastModified, name, type } = row as EitherKeptFileRow;
  if (file instanceof File) return file;
  if (!(bytes instanceof ArrayBuffer) || typeof name !== "string" || typeof type !== "string") {
    return undefined;
  }
  return new File([bytes], name, {
    lastModified: typeof lastModified === "number" ? lastModified : undefined,
    type,
  });
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
 *  a reload lets it go. The version stays 1 across the change of shape: both shapes live in the
 *  one store under the one key path, `get` reads either, and an upgrade that rewrote the rows
 *  already there could not run - remaking a row needs the bytes of its `File`, and the await of
 *  `File.arrayBuffer()` ends the version-change transaction the rewrite would have to run in. */
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
  // The puts whose row is not written yet, each with the key, the person and the time its row
  // would hold. A `put` must read its file's bytes before it can open a transaction - the await
  // of `File.arrayBuffer()` would end one - so a `delete` of that key, and the sweep that ends
  // a person's rows, mark every put of theirs on the way past, and a marked put writes nothing.
  // The sweep reads the person and the time off the put itself, since a put that has written no
  // row has none to be found by. The mark is read inside the put's own transaction, so a
  // removal made before that moment is seen here and one made after opens its transaction later
  // and takes the row out again: either way the row is gone. It answers for this page's
  // removals, the only ones ordered against its puts at all; another tab's rows are
  // `keepOnly`'s to reap, as they were.
  const putting = new Set<{
    readonly at: number;
    readonly key: string;
    readonly login: string;
    discarded: boolean;
  }>();
  return {
    delete: async (key) => {
      for (const pending of putting) {
        if (pending.key === key) pending.discarded = true;
      }
      await run("readwrite", (store) => settled(store.delete(key)));
    },
    get: async (key) => fileFrom(await run("readonly", (store) => settled(store.get(key)))),
    keepOnly: async (login, now = Date.now()) => {
      // What this sweep takes out: a file of another person, or one older than the day a key
      // lives. One rule, asked of the rows and of the puts still on their way.
      const sweeps = (file: { readonly at: number; readonly login: string }): boolean =>
        file.login !== login || now - file.at > IDEMPOTENCY_KEY_TTL_MS;
      await run("readwrite", async (store) => {
        // Both shapes name the three fields this reads, so a row of either answers for them.
        const rows = (await settled(store.getAll())) as Pick<KeptFileRow, "at" | "key" | "login">[];
        const swept = rows.filter(sweeps);
        // A put still reading its bytes would write its row back behind the sweep, as it would
        // behind a `delete`, so the sweep marks every put its own rule covers. A pending put has
        // no row here to be swept, so the rule is read off the put rather than off the rows
        // going out.
        for (const pending of putting) {
          if (sweeps(pending)) pending.discarded = true;
        }
        await Promise.all(swept.map((row) => settled(store.delete(row.key))));
      });
    },
    // Reading the bytes comes before the row is written, so a row lands later than it did in the
    // release before this one, which stored the `File` and read nothing; a `delete` of the key
    // in that window leaves the file let go rather than kept, since a put that wrote its row
    // back would keep the file of a write that has landed or been discarded until `keepOnly`
    // reaps it. `put` still answers once the row is committed, or once there is nothing kept,
    // and a file the browser can no longer read - one moved or deleted since the press - is let
    // go like any other IndexedDB failure.
    put: async (key, login, file) => {
      const at = Date.now();
      const pending = { at, discarded: false, key, login };
      putting.add(pending);
      try {
        let bytes: ArrayBuffer;
        try {
          bytes = await file.arrayBuffer();
        } catch {
          return;
        }
        const row: KeptFileRow = {
          at,
          bytes,
          key,
          lastModified: file.lastModified,
          login,
          name: file.name,
          type: file.type,
        };
        await run("readwrite", (store) =>
          pending.discarded ? Promise.resolve(undefined) : settled(store.put(row))
        );
      } finally {
        putting.delete(pending);
      }
    },
  };
}
