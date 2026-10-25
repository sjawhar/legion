import { ApiError } from "../../api/client";
import { SendStoppedError } from "./send-request";

/**
 * Where a keyed send waits out a reload. A send that names itself to Dispatch by a key - a message
 * to one session (`held-sends.ts`), a broadcast (`broadcast-queue.ts`) - is safe to make again,
 * since Dispatch answers a repeat of the key with what the first request made; so the page keeps
 * every such send in `sessionStorage`, which a reload keeps and closing the tab drops, and resumes
 * it on load under its key. Each send is one entry, `dispatch.keyed-send:<key>`, in one versioned
 * format for both kinds, and an entry that does not parse - another version's, or damaged - is
 * dropped when the page loads. While any of them is unconfirmed the page asks the browser to
 * confirm leaving (`beforeunload`), since a closed tab would lose it.
 */

const ENTRY_PREFIX = "dispatch.keyed-send:";

/** The format's version: an entry of any other is dropped on load, never read. */
const FORMAT_VERSION = 1;

/** Which store a kept send belongs to: a message to one session, or a broadcast. */
export type KeptKind = "broadcast" | "session";

/** Where a kept send stood when it was last written: waiting its turn (a broadcast behind another),
 *  out within its deadline, out past it, out while the reader stopped it, or refused. */
export type KeptState = "late" | "queued" | "refused" | "sending" | "stopping";

/** A refusal as it is kept: the reader stopped the send, Dispatch refused it, or another error. */
export type KeptRefusal =
  | { readonly kind: "stopped" }
  | {
      readonly kind: "api";
      readonly status: number;
      readonly code: string | null;
      readonly message: string;
    }
  | { readonly kind: "error"; readonly message: string };

/** One keyed send as it is kept: its key, its store, its state, its refusal once refused, and the
 *  store's own record of it (`send`), which that store writes and parses. */
export interface KeptSend<T> {
  readonly key: string;
  readonly kind: KeptKind;
  readonly state: KeptState;
  readonly refusal: KeptRefusal | null;
  readonly send: T;
}

/** A value's shape, checked: what a kept record is parsed by before it is trusted. */
export type Shape = (value: unknown) => boolean;

/** The shapes a store builds its kept record's check from: a record has each named field in its
 *  shape (a missing one reads as undefined, and others are not read), and `either` fits a value
 *  that one of its shapes fits. */
export const shape = {
  array:
    (item: Shape): Shape =>
    (value) =>
      Array.isArray(value) && value.every(item),
  boolean: (value: unknown) => typeof value === "boolean",
  either:
    (...shapes: readonly Shape[]): Shape =>
    (value) =>
      shapes.some((fits) => fits(value)),
  nullable:
    (inner: Shape): Shape =>
    (value) =>
      value === null || inner(value),
  number: (value: unknown) => typeof value === "number" && Number.isFinite(value),
  oneOf:
    (...values: readonly unknown[]): Shape =>
    (value) =>
      values.includes(value),
  optional:
    (inner: Shape): Shape =>
    (value) =>
      value === undefined || inner(value),
  record:
    (fields: Readonly<Record<string, Shape>>): Shape =>
    (value) =>
      typeof value === "object" &&
      value !== null &&
      !Array.isArray(value) &&
      Object.entries(fields).every(([name, field]) => field(Reflect.get(value, name))),
  string: (value: unknown) => typeof value === "string",
} as const;

const refusalShape = shape.either(
  shape.record({ kind: shape.oneOf("stopped") }),
  shape.record({
    code: shape.nullable(shape.string),
    kind: shape.oneOf("api"),
    message: shape.string,
    status: shape.number,
  }),
  shape.record({ kind: shape.oneOf("error"), message: shape.string })
);

/** An entry of this format: a refused send carries its refusal, and no other does. */
const entryShape = shape.either(
  shape.record({
    key: shape.string,
    kind: shape.oneOf("broadcast", "session"),
    refusal: refusalShape,
    state: shape.oneOf("refused"),
    version: shape.oneOf(FORMAT_VERSION),
  }),
  shape.record({
    key: shape.string,
    kind: shape.oneOf("broadcast", "session"),
    refusal: shape.oneOf(null),
    state: shape.oneOf("late", "queued", "sending", "stopping"),
    version: shape.oneOf(FORMAT_VERSION),
  })
);

/** How `error`, a send's refusal, is kept. */
export function keptRefusal(error: Error): KeptRefusal {
  if (error instanceof SendStoppedError) return { kind: "stopped" };
  if (error instanceof ApiError) {
    return { code: error.code ?? null, kind: "api", message: error.message, status: error.status };
  }
  return { kind: "error", message: error.message };
}

/** The refusal `kept` names, rebuilt as the error the send was refused with. */
export function refusalFrom(kept: KeptRefusal): Error {
  switch (kept.kind) {
    case "stopped":
      return new SendStoppedError();
    case "api":
      return new ApiError(kept.status, { code: kept.code ?? undefined, error: kept.message });
    case "error":
      return new Error(kept.message);
  }
}

/** The kept sends of one page: `storage` holds them across a reload, and `unload` is where the
 *  page asks to confirm leaving while any of them is unconfirmed. */
export class KeyedSendStorage {
  readonly #storage: Storage | undefined;
  readonly #unload: EventTarget;
  /** The keys of the sends kept as still out - queued, sending, late or stopping. */
  readonly #unconfirmed = new Set<string>();
  #retired = false;

  /** `storage` is undefined where the browser gives the page none: sends are then not kept across
   *  a reload, and leaving is still confirmed while one is out. */
  constructor(storage: Storage | undefined, unload: EventTarget) {
    this.#storage = storage;
    this.#unload = unload;
  }

  /** Keeps `sent` under its key, replacing what was kept there. */
  keep<T>(sent: KeptSend<T>): void {
    if (this.#retired) return;
    try {
      this.#storage?.setItem(
        `${ENTRY_PREFIX}${sent.key}`,
        JSON.stringify({ ...sent, version: FORMAT_VERSION })
      );
    } catch {
      // A full or refused storage loses only the send's reload; it is still sent.
    }
    this.#confirm(sent.key, sent.state === "refused");
  }

  /** Drops the send kept under `key`: it landed, or the reader dropped its refusal. */
  forget(key: string): void {
    if (this.#retired) return;
    this.#storage?.removeItem(`${ENTRY_PREFIX}${key}`);
    this.#confirm(key, true);
  }

  /** Every send kept for `kind` whose record `parse` accepts, handed its key. An entry that is not
   *  one of this format's, or whose record `parse` refuses, is dropped. */
  load<T>(kind: KeptKind, parse: (send: unknown, key: string) => T | undefined): KeptSend<T>[] {
    const storage = this.#storage;
    if (storage === undefined || this.#retired) return [];
    const loaded: KeptSend<T>[] = [];
    for (const name of this.#names(storage)) {
      const entry = this.#read(storage, name);
      const ours = entry !== undefined && `${ENTRY_PREFIX}${entry.key}` === name;
      if (ours && entry.kind !== kind) continue;
      const send = ours ? parse(entry.send, entry.key) : undefined;
      if (!ours || send === undefined) {
        storage.removeItem(name);
        continue;
      }
      loaded.push({ key: entry.key, kind, refusal: entry.refusal, send, state: entry.state });
    }
    return loaded;
  }

  /** Ends this storage, for the unit-test preload between tests: what it kept goes, and it keeps
   *  nothing more and asks nothing on leaving, whatever the stores it served still send. */
  retire(): void {
    this.#retired = true;
    if (this.#unconfirmed.size > 0) {
      this.#unload.removeEventListener("beforeunload", this.#confirmLeaving);
    }
    this.#unconfirmed.clear();
    const storage = this.#storage;
    if (storage !== undefined) for (const name of this.#names(storage)) storage.removeItem(name);
  }

  /** The names of every entry kept, read before any is removed. */
  #names(storage: Storage): string[] {
    return Array.from({ length: storage.length }, (_, index) => storage.key(index)).filter(
      (name): name is string => name?.startsWith(ENTRY_PREFIX) === true
    );
  }

  #read(storage: Storage, name: string): KeptSend<unknown> | undefined {
    try {
      const value: unknown = JSON.parse(storage.getItem(name) ?? "null");
      return entryShape(value) ? (value as KeptSend<unknown>) : undefined;
    } catch {
      return undefined;
    }
  }

  /** Marks the send under `key` confirmed - landed, refused or dropped - or still out, and asks to
   *  confirm leaving exactly while one is still out. */
  #confirm(key: string, confirmed: boolean): void {
    const before = this.#unconfirmed.size;
    if (confirmed) this.#unconfirmed.delete(key);
    else this.#unconfirmed.add(key);
    if (before === 0 && this.#unconfirmed.size > 0) {
      this.#unload.addEventListener("beforeunload", this.#confirmLeaving);
    } else if (before > 0 && this.#unconfirmed.size === 0) {
      this.#unload.removeEventListener("beforeunload", this.#confirmLeaving);
    }
  }

  /** Asks the browser to confirm leaving the page. This storage's own listener, so another's
   *  adding and removing its own leaves it be. */
  readonly #confirmLeaving = (event: Event): void => {
    event.preventDefault();
    // A browser older than the `preventDefault` form (Chrome before 119) asks only for this.
    (event as BeforeUnloadEvent).returnValue = true;
  };
}

/** The page's sessionStorage, or undefined where the browser refuses it the page. */
function pageStorage(): Storage | undefined {
  try {
    return window.sessionStorage;
  } catch {
    return undefined;
  }
}

let pageKeyedSends: KeyedSendStorage | undefined;

/** The page's kept sends: its `sessionStorage`, and its window's `beforeunload`. */
export function keyedSendStorage(): KeyedSendStorage {
  pageKeyedSends ??= new KeyedSendStorage(pageStorage(), window);
  return pageKeyedSends;
}

/** Lets the page's kept sends go (`retire`), and the next store keeps its own afresh. The unit-test
 *  preload calls it after each test, since a store outlives its test while a send of it is out,
 *  and a later test's app would take up what it kept. */
export function resetKeyedSendStorage(): void {
  pageKeyedSends?.retire();
  pageKeyedSends = undefined;
}
