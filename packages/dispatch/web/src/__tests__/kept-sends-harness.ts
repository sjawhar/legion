import type { KeptFiles } from "../features/writes/kept-files";
import { WriteStorage } from "../features/writes/write-storage";

/** A browser's kept upload files, in memory: what IndexedDB holds for the page across a reload. */
export function memoryFiles(): KeptFiles & { readonly rows: Map<string, File> } {
  const rows = new Map<string, { login: string; file: File }>();
  const files = new Map<string, File>();
  return {
    delete: async (key) => {
      rows.delete(key);
      files.delete(key);
    },
    get: async (key) => rows.get(key)?.file,
    keepOnly: async (login) => {
      for (const [key, row] of rows) {
        if (row.login !== login) {
          rows.delete(key);
          files.delete(key);
        }
      }
    },
    put: async (key, login, file) => {
      rows.set(key, { file, login });
      files.set(key, file);
    },
    rows: files,
  };
}

/** A `beforeunload` target that counts its listeners: the page asks to confirm leaving exactly
 *  while it holds one. */
export class UnloadTarget extends EventTarget {
  listeners = 0;

  override addEventListener(
    type: string,
    listener: EventListenerOrEventListenerObject | null,
    options?: AddEventListenerOptions | boolean
  ): void {
    if (type === "beforeunload") this.listeners += 1;
    super.addEventListener(type, listener, options);
  }

  override removeEventListener(
    type: string,
    listener: EventListenerOrEventListenerObject | null,
    options?: EventListenerOptions | boolean
  ): void {
    if (type === "beforeunload") this.listeners -= 1;
    super.removeEventListener(type, listener, options);
  }

  /** Whether leaving now asks the reader to confirm: a listener cancelled the event. */
  asks(): boolean {
    const leaving = new Event("beforeunload", { cancelable: true });
    this.dispatchEvent(leaving);
    return leaving.defaultPrevented;
  }
}

/** One browser tab's sessionStorage, which a reload keeps, and the pages it holds in turn: each
 *  `page` is the kept writes of one load of the app, the last one after a reload. */
export interface KeptTab {
  readonly storage: Storage;
  /** The kept writes of the next page load, asking on `unload` to confirm leaving. */
  page: (unload?: EventTarget) => WriteStorage;
  /** What the tab keeps for the write under `key`, as written. */
  kept: (key: string) => unknown;
  /** Ends every page, its kept sends with it. */
  close: () => void;
}

export function keptTab(): KeptTab {
  const items = new Map<string, string>();
  const storage: Storage = {
    clear: () => items.clear(),
    getItem: (name) => items.get(name) ?? null,
    key: (index) => [...items.keys()][index] ?? null,
    get length() {
      return items.size;
    },
    removeItem: (name) => {
      items.delete(name);
    },
    setItem: (name, value) => {
      items.set(name, String(value));
    },
  };
  const pages: WriteStorage[] = [];
  return {
    close: () => {
      for (const page of pages) page.retire();
    },
    kept: (key) => {
      const value = items.get(`dispatch.write:${key}`);
      return value === undefined ? undefined : JSON.parse(value);
    },
    page: (unload = new UnloadTarget()) => {
      const page = new WriteStorage(storage, unload);
      pages.push(page);
      return page;
    },
    storage,
  };
}
