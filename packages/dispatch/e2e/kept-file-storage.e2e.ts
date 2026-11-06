import { expect, type Page, test } from "@playwright/test";

import { resetDatabase } from "./seed";
import { asUser } from "./users";

// Every value here is read from `web/src/features/writes/kept-files.ts`, the product's own store
// of the files it keeps for uploads: the database name and the store name (`DATABASE`, `STORE`),
// the version and the key path `openDatabase` opens and creates with (`factory.open(DATABASE, 1)`,
// `createObjectStore(STORE, { keyPath: "key" })`), and the seven fields of a row (`KeptFileRow`:
// `key`, `login`, `at`, `bytes`, `name`, `type`, `lastModified`). A row written under another
// name, version or shape would prove nothing about the database the page reads on a reload.
const DATABASE = "dispatch-kept-files";
const STORE = "files";
const VERSION = 1;
const KEY_PATH = "key";

/** The probe's own row: a key no write of the page mints, so nothing but this row touches it. */
const KEY = "kept-file-storage-probe";
const FILE_NAME = "kept-upload.txt";
const FILE_TYPE = "text/plain";
const LAST_MODIFIED = 1_700_000_000_000;
/** `kept`, a NUL and 0xFF: the last two bytes do not survive a round trip through text, so a
 *  comparison of these says the bytes came back and not merely something of the same length. */
const BYTES = [0x6b, 0x65, 0x70, 0x74, 0x00, 0xff];

/** What the engine handed back, read from IndexedDB itself and made into a `File` as `get` makes
 *  one: `got` is `File` when the row's bytes came back, `no row`, or whatever else the row held
 *  in their place, and the rest are that file's own. */
interface ReadBack {
  got: string;
  name: string | null;
  type: string | null;
  lastModified: number | null;
  size: number | null;
  bytes: number[] | null;
}

/** Writes the probe's row into the page's own `dispatch-kept-files`, in the shape `put` writes -
 *  the file's bytes, with the name, type and last-modified time a `File` is made from them again
 *  with - stamped with `login` as a kept upload is, and waits for the transaction to commit. */
async function keepFile(page: Page, login: string): Promise<void> {
  await page.evaluate(
    async ({ bytes, database, key, keyPath, lastModified, name, store, type, version, who }) => {
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
        transaction.objectStore(store).put({
          at: Date.now(),
          bytes: new Uint8Array(bytes).buffer,
          key,
          lastModified,
          login: who,
          name,
          type,
        });
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
      lastModified: LAST_MODIFIED,
      name: FILE_NAME,
      store: STORE,
      type: FILE_TYPE,
      version: VERSION,
      who: login,
    }
  );
}

/** Reads the probe's row back and describes the `File` the page would resume with. */
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
      const absent = {
        bytes: null,
        lastModified: null,
        name: null,
        size: null,
        type: null,
      };
      try {
        const request = connection.transaction(store, "readonly").objectStore(store).get(key);
        const settled = Promise.withResolvers<Record<string, unknown> | undefined>();
        request.onsuccess = () => settled.resolve(request.result);
        request.onerror = () => settled.reject(request.error);
        const row = await settled.promise;
        if (row === undefined) return { ...absent, got: "no row" };
        const { bytes, lastModified, name, type } = row;
        if (!(bytes instanceof ArrayBuffer) || typeof name !== "string" || typeof type !== "string")
          return { ...absent, got: `${Object.prototype.toString.call(bytes)}` };
        // What `kept-files.ts` makes of the row it read: the `File` the resumed upload is made
        // from (`get`, `requestFrom`).
        const file = new File([bytes], name, {
          lastModified: typeof lastModified === "number" ? lastModified : undefined,
          type,
        });
        return {
          bytes: [...new Uint8Array(await file.arrayBuffer())],
          got: "File",
          lastModified: file.lastModified,
          name: file.name,
          size: file.size,
          type: file.type,
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
// `indexedDbFiles`), as the bytes a `File` is made from again, so the page can only resume that
// upload after a reload where the engine stored the row and handed those bytes back. Where they
// do not come back, `Writes.#resume` (`write-store.ts`) drops the kept write without a word, and
// the reloaded page shows nothing - the same thing a defect in the resume path would show. This
// row writes the product's own shape into that same database by hand, reloads, and makes the
// `File` from what came back as `get` does, so each engine's result says which of the two is
// happening in it. An engine that will not store the row fails this row at the write, which is
// what WebKit did while the row held a `File`.
// SLOW(>=2s): the kept file is only observable across a real reload, so the row pays two loads
// of the dashboard - the one that signs alice in and writes the row, and the reload that reads it
// back. Measured at 2.5 s in an issue pod, one worker, `--project chromium`.
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
      `${browserName} did not give the kept file's bytes back from IndexedDB across a reload, so ` +
        `an upload this engine keeps has no file to resume with and Writes.#resume drops it ` +
        `silently`
    ).toEqual({
      bytes: BYTES,
      got: "File",
      lastModified: LAST_MODIFIED,
      name: FILE_NAME,
      size: BYTES.length,
      type: FILE_TYPE,
    });
  } finally {
    await alice.close();
  }
});
