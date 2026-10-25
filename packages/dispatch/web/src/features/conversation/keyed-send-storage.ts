import type { QueryClient } from "@tanstack/react-query";

import { ApiError } from "../../api/client";
import type { AuthenticatedUser } from "../../api/types";
import { SendInterruptedError, SendStoppedError } from "./send-request";

/**
 * Where a keyed send waits out a reload. A send that names itself to Dispatch by a key - a message
 * to one session (`held-sends.ts`), a broadcast (`broadcast-queue.ts`) - is safe to make again,
 * since Dispatch answers a repeat of the key with what the first request made; so the page keeps
 * every such send in `sessionStorage`, which a reload keeps and closing the tab drops, and resumes
 * it on load under its key. Each send is one entry, `dispatch.keyed-send:<key>`, in one versioned
 * format for both kinds, stamped with the login of the person who pressed it, and an entry that
 * does not parse - another version's, or damaged - is dropped when the page loads, as is every
 * entry of another person's. While any of them is unconfirmed the page asks the browser to
 * confirm leaving (`beforeunload`), since a closed tab would lose it.
 */

const ENTRY_PREFIX = "dispatch.keyed-send:";

/** The format's version: an entry of any other is dropped on load, never read. Version 1 kept no
 *  login, so no entry of it can be given back to the person who pressed it. */
const FORMAT_VERSION = 2;

/** How long after its press a send a reload interrupted is made again on its own. One kept longer
 *  comes back refused (`SendInterruptedError`), for the reader to Retry under its key or discard:
 *  a message they wrote that long ago may no longer be one they mean. */
export const KEPT_SEND_RESUME_MS = 5 * 60_000;

/** Whether a send pressed at `pressedAt` that a reload interrupted is made again on its own now. */
export function resumesOnItsOwn(pressedAt: number, now = Date.now()): boolean {
  return now - pressedAt <= KEPT_SEND_RESUME_MS;
}

/** The login of the person `client`'s app is signed in as (`["whoami"]`), which every kept send is
 *  stamped with; undefined while no one is signed in, when nothing is kept. */
export function signedInLogin(client: QueryClient): string | undefined {
  return client.getQueryData<AuthenticatedUser>(["whoami"])?.login;
}

/** Which store a kept send belongs to: a message to one session, or a broadcast. */
export type KeptKind = "broadcast" | "session";

/** Where a kept send stood when it was last written: waiting its turn (a broadcast behind another),
 *  out within its deadline, out past it, out while the reader stopped it, or refused. */
export type KeptState = "late" | "queued" | "refused" | "sending" | "stopping";

/** A refusal as it is kept: the reader stopped the send, a reload interrupted it too long ago to be
 *  made again on its own, Dispatch refused it, or another error. */
export type KeptRefusal =
  | { readonly kind: "interrupted" }
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
  shape.record({ kind: shape.oneOf("interrupted", "stopped") }),
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
    login: shape.string,
    refusal: refusalShape,
    state: shape.oneOf("refused"),
    version: shape.oneOf(FORMAT_VERSION),
  }),
  shape.record({
    key: shape.string,
    kind: shape.oneOf("broadcast", "session"),
    login: shape.string,
    refusal: shape.oneOf(null),
    state: shape.oneOf("late", "queued", "sending", "stopping"),
    version: shape.oneOf(FORMAT_VERSION),
  })
);

/** An entry as it is written: the send, the login of the person who pressed it, and the format. */
type KeptEntry = KeptSend<unknown> & { readonly login: string; readonly version: number };

/** How `error`, a send's refusal, is kept. */
export function keptRefusal(error: Error): KeptRefusal {
  if (error instanceof SendInterruptedError) return { kind: "interrupted" };
  if (error instanceof SendStoppedError) return { kind: "stopped" };
  if (error instanceof ApiError) {
    return { code: error.code ?? null, kind: "api", message: error.message, status: error.status };
  }
  return { kind: "error", message: error.message };
}

/** The refusal `kept` names, rebuilt as the error the send was refused with. */
export function refusalFrom(kept: KeptRefusal): Error {
  switch (kept.kind) {
    case "interrupted":
      return new SendInterruptedError();
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

  /** Keeps `sent` under its key for `login`, the person who pressed it, replacing what was kept
   *  there. A send with no one signed in to stamp it with is not kept, nor is one the storage
   *  refuses to write (full, or refused the page): what was kept under its key before is then
   *  removed rather than left to stand for it, so a reload never resumes a state the send has
   *  left - a send the reader stopped never comes back as one still being tried. */
  keep<T>(sent: KeptSend<T>, login: string | undefined): void {
    if (this.#retired) return;
    const name = `${ENTRY_PREFIX}${sent.key}`;
    if (login === undefined) {
      this.#remove(name);
    } else {
      try {
        this.#storage?.setItem(
          name,
          JSON.stringify({ ...sent, login, version: FORMAT_VERSION } satisfies KeptEntry)
        );
      } catch {
        // The send is still sent; only its reload is lost.
        this.#remove(name);
      }
    }
    this.#confirm(sent.key, sent.state === "refused");
  }

  /** Drops the send kept under `key`: it landed, or the reader dropped its refusal. */
  forget(key: string): void {
    if (this.#retired) return;
    this.#remove(`${ENTRY_PREFIX}${key}`);
    this.#confirm(key, true);
  }

  /** Every send `login` kept for `kind` whose record `parse` accepts, handed its key. An entry that
   *  is not one of this format's, one another person kept, or one whose record `parse` refuses, is
   *  dropped; the other kind's entries of `login` stay for their store. */
  load<T>(
    kind: KeptKind,
    login: string,
    parse: (send: unknown, key: string) => T | undefined
  ): KeptSend<T>[] {
    const storage = this.#storage;
    if (storage === undefined || this.#retired) return [];
    const loaded: KeptSend<T>[] = [];
    for (const name of this.#names(storage)) {
      const entry = this.#read(storage, name);
      const ours =
        entry !== undefined && `${ENTRY_PREFIX}${entry.key}` === name && entry.login === login;
      if (ours && entry.kind !== kind) continue;
      const send = ours ? parse(entry.send, entry.key) : undefined;
      if (!ours || send === undefined) {
        this.#remove(name);
        continue;
      }
      loaded.push({ key: entry.key, kind, refusal: entry.refusal, send, state: entry.state });
    }
    return loaded;
  }

  /** Drops every kept send, and stops asking to confirm leaving: the person who pressed them
   *  signed out. A send still out goes on, and is kept again only once someone is signed in. */
  clear(): void {
    if (this.#retired) return;
    this.#dropAll();
  }

  /** Ends this storage, for the unit-test preload between tests: what it kept goes, and it keeps
   *  nothing more and asks nothing on leaving, whatever the stores it served still send. */
  retire(): void {
    this.#retired = true;
    this.#dropAll();
  }

  #dropAll(): void {
    if (this.#unconfirmed.size > 0) {
      this.#unload.removeEventListener("beforeunload", this.#confirmLeaving);
    }
    this.#unconfirmed.clear();
    const storage = this.#storage;
    if (storage !== undefined) for (const name of this.#names(storage)) this.#remove(name);
  }

  #remove(name: string): void {
    try {
      this.#storage?.removeItem(name);
    } catch {
      // A storage that refuses even a removal holds nothing this page wrote.
    }
  }

  /** The names of every entry kept, read before any is removed. */
  #names(storage: Storage): string[] {
    return Array.from({ length: storage.length }, (_, index) => storage.key(index)).filter(
      (name): name is string => name?.startsWith(ENTRY_PREFIX) === true
    );
  }

  #read(storage: Storage, name: string): KeptEntry | undefined {
    try {
      const value: unknown = JSON.parse(storage.getItem(name) ?? "null");
      return entryShape(value) ? (value as KeptEntry) : undefined;
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
