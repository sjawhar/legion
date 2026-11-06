import { afterAll, afterEach, expect, test } from "bun:test";
import { File as PlatformFile } from "node:buffer";
import { IDEMPOTENCY_KEY_TTL_MS } from "@legion/contracts";

import { indexedDbFiles } from "./kept-files";

// The database the page writes, named here as `kept-files.ts` names it (`DATABASE`, `STORE`, the
// version and key path `openDatabase` opens and creates with): these tests read the row the store
// wrote and write rows of their own into that same store, so a row under another name or version
// would say nothing about what the page reads on a reload.
const DATABASE = "dispatch-kept-files";
const STORE = "files";
const VERSION = 1;
const KEY_PATH = "key";

// These tests run on `fake-indexeddb` (`__tests__/setup.ts`), whose clone of happy-dom's `File`
// is a plain object: no row holding one can be read back as a file at all, which is neither what
// a browser does nor what this store has to answer for. The platform's own `File` is the one
// that clone keeps, as every browser's IndexedDB keeps a real one, so the file is the platform's
// while this file runs and happy-dom's again after it.
const pageFile = globalThis.File;
globalThis.File = PlatformFile as unknown as typeof File;
afterAll(() => {
  globalThis.File = pageFile;
});

const files = indexedDbFiles();

/** `kept`, a NUL and 0xFF: the last two bytes do not survive a round trip through text, so a
 *  comparison of these says the bytes came back and not merely something of the same length. */
const BYTES = Uint8Array.of(0x6b, 0x65, 0x70, 0x74, 0x00, 0xff);
const NAME = "kept-upload.txt";
// Bun's `File` normalises a textual type by adding its charset, which no browser does, so the
// probe's type is one it hands back as given.
const TYPE = "application/octet-stream";
const LAST_MODIFIED = 1_700_000_000_000;

const upload = (): File => new File([BYTES], NAME, { lastModified: LAST_MODIFIED, type: TYPE });

/** The bytes of `file`, as a comparison prints them. */
async function bytesOf(file: File): Promise<number[]> {
  return [...new Uint8Array(await file.arrayBuffer())];
}

/** An upload whose bytes arrive only once `release` is called: the window `put` reads its file
 *  in, which a `delete` of the same key can land inside. */
function slowUpload(): { file: File; release: () => void } {
  const read = upload();
  const { promise, resolve } = Promise.withResolvers<void>();
  const file = {
    arrayBuffer: async () => {
      await promise;
      return await read.arrayBuffer();
    },
    lastModified: LAST_MODIFIED,
    name: NAME,
    type: TYPE,
  } as unknown as File;
  return { file, release: resolve };
}

function settled<T>(request: IDBRequest<T>): Promise<T> {
  const { promise, resolve, reject } = Promise.withResolvers<T>();
  request.onsuccess = () => resolve(request.result);
  request.onerror = () => reject(request.error);
  return promise;
}

/** The page's own database, opened by hand. */
async function connection(): Promise<IDBDatabase> {
  const open = indexedDB.open(DATABASE, VERSION);
  open.onupgradeneeded = () => {
    open.result.createObjectStore(STORE, { keyPath: KEY_PATH });
  };
  return await settled<IDBDatabase>(open);
}

/** Puts `row` into the store as it stands, so a test writes a shape the store no longer writes. */
async function keepRow(row: object): Promise<void> {
  const database = await connection();
  try {
    const transaction = database.transaction(STORE, "readwrite");
    transaction.objectStore(STORE).put(row);
    const { promise, resolve, reject } = Promise.withResolvers<void>();
    transaction.oncomplete = () => resolve();
    transaction.onerror = () => reject(transaction.error);
    transaction.onabort = () => reject(transaction.error);
    await promise;
  } finally {
    database.close();
  }
}

/** The row under `key` as the store holds it, fields and all. */
async function rowOf(key: string): Promise<Record<string, unknown> | undefined> {
  const database = await connection();
  try {
    return await settled<Record<string, unknown> | undefined>(
      database.transaction(STORE, "readonly").objectStore(STORE).get(key)
    );
  } finally {
    database.close();
  }
}

// One database serves the whole test file, as `fake-indexeddb` keeps one per process.
afterEach(async () => {
  await files.keepOnly(undefined);
});

test("a kept file comes back with its name, type, bytes and last-modified time", async () => {
  await files.put("k1", "alice", upload());

  const back = await files.get("k1");

  expect(back).toBeInstanceOf(File);
  expect(back?.name).toBe(NAME);
  expect(back?.type).toBe(TYPE);
  expect(back?.lastModified).toBe(LAST_MODIFIED);
  expect(back?.size).toBe(BYTES.length);
  expect(back === undefined ? null : await bytesOf(back)).toEqual([...BYTES]);
});

test("the row it writes holds the bytes, not a File", async () => {
  await files.put("k2", "alice", upload());

  const row = await rowOf("k2");

  // WebKit stores no `File` and fails the write of a row holding one, so what the row holds is
  // the file's bytes - a plain structured clone every engine stores - and the three things a
  // `File` is made from them again with.
  expect(row?.bytes).toBeInstanceOf(ArrayBuffer);
  expect(row?.file).toBeUndefined();
  expect([...new Uint8Array(row?.bytes as ArrayBuffer)]).toEqual([...BYTES]);
  expect(row?.key).toBe("k2");
  expect(row?.login).toBe("alice");
  expect(row?.name).toBe(NAME);
  expect(row?.type).toBe(TYPE);
  expect(row?.lastModified).toBe(LAST_MODIFIED);
  expect(typeof row?.at).toBe("number");
});

test("a row kept in the shape before this version comes back as its file", async () => {
  // What the release now in production wrote: the `File` itself, under `file`. A reader whose
  // page reloads across the deploy has rows of that shape and resumes their upload from them.
  await keepRow({ at: Date.now(), file: upload(), key: "k3", login: "alice" });

  const back = await files.get("k3");

  expect(back).toBeInstanceOf(File);
  expect(back?.name).toBe(NAME);
  expect(back?.type).toBe(TYPE);
  expect(back === undefined ? null : await bytesOf(back)).toEqual([...BYTES]);
});

test("a row of neither shape is nothing to resume with", async () => {
  await keepRow({ at: Date.now(), key: "k4", login: "alice", name: NAME, type: TYPE });

  expect(await files.get("k4")).toBeUndefined();
  expect(await files.get("never-kept")).toBeUndefined();
});

test("keepOnly keeps the login's own rows from within the day, in either shape", async () => {
  const now = Date.now();
  const stale = now - IDEMPOTENCY_KEY_TTL_MS - 1;
  await files.put("mine", "alice", upload());
  await keepRow({ at: stale, file: upload(), key: "old", login: "alice" });
  await keepRow({ at: now, file: upload(), key: "mine-before", login: "alice" });
  await keepRow({ at: now, file: upload(), key: "theirs", login: "bob" });

  await files.keepOnly("alice", now);

  expect(await files.get("mine")).toBeInstanceOf(File);
  expect(await files.get("mine-before")).toBeInstanceOf(File);
  expect(await files.get("old")).toBeUndefined();
  expect(await files.get("theirs")).toBeUndefined();
});

test("a file let go is not there to resume with", async () => {
  await files.put("k5", "alice", upload());

  await files.delete("k5");

  expect(await files.get("k5")).toBeUndefined();
  expect(await rowOf("k5")).toBeUndefined();
});

// A delete while the put is still reading the file's bytes is the write landing, or the reader
// discarding it: the file is gone, and a put that began before it must not write the row back,
// which would keep a file for a write that no longer exists until `keepOnly` reaps it.
test("a file let go while its put is reading the bytes is not written back", async () => {
  const { file, release } = slowUpload();
  const kept = files.put("k6", "alice", file);

  await files.delete("k6");
  release();
  await kept;

  expect(await rowOf("k6")).toBeUndefined();
  expect(await files.get("k6")).toBeUndefined();
});

// The sweep ends every row but the signed-in person's, and a put still reading its bytes has no
// row yet to be found by: it is marked by what it is for - whose it is, and when it began - so
// another person's upload cannot land behind the sweep that was clearing them.
test("a put of another person's file, still reading its bytes, is not written back", async () => {
  const { file, release } = slowUpload();
  const kept = files.put("k7", "bob", file);

  await files.keepOnly("alice");
  release();
  await kept;

  expect(await rowOf("k7")).toBeUndefined();
  expect(await files.get("k7")).toBeUndefined();
});

// The same put, of the person the sweep keeps: the sweep's rule does not cover it, so it lands.
test("a put of the signed-in person's file is kept across the sweep", async () => {
  const { file, release } = slowUpload();
  const kept = files.put("k8", "alice", file);

  await files.keepOnly("alice");
  release();
  await kept;

  expect(await files.get("k8")).toBeInstanceOf(File);
});
