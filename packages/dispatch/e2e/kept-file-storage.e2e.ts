import { expect, type Page, test } from "@playwright/test";

import { resetDatabase } from "./seed";
import { asUser } from "./users";

// Every value here is read from `web/src/features/writes/kept-files.ts`, the product's own store
// of the files it keeps for uploads: the database name and the store name (`DATABASE`, `STORE`),
// the version and the key path `openDatabase` opens and creates with (`factory.open(DATABASE, 1)`,
// `createObjectStore(STORE, { keyPath: "key" })`), and the four fields of a row (`KeptFileRow`:
// `key`, `login`, `at`, `file`). A row written under another name, version or shape would prove
// nothing about the database the page reads on a reload.
const DATABASE = "dispatch-kept-files";
const STORE = "files";
const VERSION = 1;
const KEY_PATH = "key";

/** The probe's own row: a key no write of the page mints, so nothing but this row touches it. */
const KEY = "kept-file-storage-probe";
const FILE_NAME = "kept-upload.txt";
const FILE_TYPE = "text/plain";
/** `kept`, a NUL and 0xFF: the last two bytes do not survive a round trip through text, so a
 *  comparison of these says the bytes came back and not merely something of the same length. */
const BYTES = [0x6b, 0x65, 0x70, 0x74, 0x00, 0xff];

/** What the engine handed back for the row's `file`, read from IndexedDB itself: `got` is `File`,
 *  `Blob`, `no row`, or whatever else the row held, and the rest are that value's own. */
interface ReadBack {
  got: string;
  name: string | null;
  size: number | null;
  bytes: number[] | null;
}

/** Writes the probe's row into the page's own `dispatch-kept-files`, stamped with `login` as a
 *  kept upload is, and waits for the transaction to commit. */
async function keepFile(page: Page, login: string): Promise<void> {
  await page.evaluate(
    async ({ bytes, database, key, keyPath, name, store, type, version, who }) => {
      const open = indexedDB.open(database, version);
      open.onupgradeneeded = () => {
        open.result.createObjectStore(store, { keyPath });
      };
      const opened = Promise.withResolvers<IDBDatabase>();
      open.onsuccess = () => opened.resolve(open.result);
      open.onerror = () => opened.reject(open.error);
      const connection = await opened.promise;
      try {
        const transaction = connection.transaction(store, "readwrite");
        const file = new File([new Uint8Array(bytes)], name, { type });
        transaction.objectStore(store).put({ at: Date.now(), file, key, login: who });
        const committed = Promise.withResolvers<void>();
        transaction.oncomplete = () => committed.resolve();
        transaction.onerror = () => committed.reject(transaction.error);
        transaction.onabort = () => committed.reject(transaction.error);
        await committed.promise;
      } finally {
        connection.close();
      }
    },
    {
      bytes: BYTES,
      database: DATABASE,
      key: KEY,
      keyPath: KEY_PATH,
      name: FILE_NAME,
      store: STORE,
      type: FILE_TYPE,
      version: VERSION,
      who: login,
    }
  );
}

/** Reads the probe's row back and describes what the engine kept in it. */
async function readBack(page: Page): Promise<ReadBack> {
  return await page.evaluate(
    async ({ database, key, keyPath, store, version }) => {
      const open = indexedDB.open(database, version);
      open.onupgradeneeded = () => {
        open.result.createObjectStore(store, { keyPath });
      };
      const opened = Promise.withResolvers<IDBDatabase>();
      open.onsuccess = () => opened.resolve(open.result);
      open.onerror = () => opened.reject(open.error);
      const connection = await opened.promise;
      try {
        const request = connection.transaction(store, "readonly").objectStore(store).get(key);
        const settled = Promise.withResolvers<{ file?: unknown } | undefined>();
        request.onsuccess = () => settled.resolve(request.result);
        request.onerror = () => settled.reject(request.error);
        const row = await settled.promise;
        if (row === undefined) return { bytes: null, got: "no row", name: null, size: null };
        const file = row.file;
        if (!(file instanceof Blob)) {
          const got = `${Object.prototype.toString.call(file)}`;
          return { bytes: null, got, name: null, size: null };
        }
        return {
          bytes: [...new Uint8Array(await file.arrayBuffer())],
          got: file instanceof File ? "File" : "Blob",
          name: file instanceof File ? file.name : null,
          size: file.size,
        };
      } finally {
        connection.close();
      }
    },
    { database: DATABASE, key: KEY, keyPath: KEY_PATH, store: STORE, version: VERSION }
  );
}

test.beforeEach(async () => {
  await resetDatabase();
});

// An upload's file is kept in IndexedDB under its write's key (`kept-files.ts`,
// `indexedDbFiles`), so the page can only resume that upload after a reload where the engine
// hands the `File` back. Where it does not come back, `Writes.#resume` (`write-store.ts`) drops
// the kept write without a word, and the reloaded page shows nothing - the same thing a defect in
// the resume path would show. This row writes a `File` into that same database by hand, reloads
// and reads it back, so each engine's result says which of the two is happening in it.
// SLOW(>=2s): the kept file is only observable across a real reload, so the row pays two loads
// of the dashboard - the one that signs alice in and writes the row, and the reload that reads it
// back. Measured at 2,381 ms in an issue pod, one worker, `--project chromium`.
test("a file kept in IndexedDB comes back after a reload", async ({ browser, browserName }) => {
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    // A kept row names the login that pressed the write, and the page deletes every other
    // login's as it signs in and as it resumes (`keepOnly`, from `Writes.signedIn` and
    // `Writes.resume`), so the probe's row is alice's own and is written once the page holds her
    // sign-in and that first sweep is behind it.
    await expect(page.getByText("Signed in as alice")).toBeVisible();
    await keepFile(page, "alice");

    await page.reload();

    expect(
      await readBack(page),
      `${browserName} did not give the kept File back from IndexedDB across a reload, so an ` +
        `upload this engine keeps has no file to resume with and Writes.#resume drops it silently`
    ).toEqual({ bytes: BYTES, got: "File", name: FILE_NAME, size: BYTES.length });
  } finally {
    await alice.close();
  }
});
