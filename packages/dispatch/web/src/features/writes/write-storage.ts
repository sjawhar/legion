import { ApiError, WRITE_METHODS, type WriteMethod, type WriteRequest } from "../../api/client";
import { SendInterruptedError, SendStoppedError } from "./keyed-write";

/**
 * Where every write to Dispatch waits out a reload (`write-store.ts`). Every `POST` and `PATCH`
 * names itself to Dispatch by a key (`Idempotency-Key`), so making it again is safe - Dispatch
 * answers a repeat of the key with what the first request did - and a `PUT` or a `DELETE` is the
 * same write however often it is made, so the page keeps each write in `sessionStorage`, which a
 * reload keeps and closing the tab drops, from its press until Dispatch answers it, and the store
 * resumes it on load under its key. Each write is one entry, `dispatch.write:<key>`, in one
 * versioned format for every kind (`KeptWrite`): its key, its kind and how it is shown, the login
 * of the person who pressed it and when, the target its line is ordered on, where it stood and its
 * refusal, the request as it last went out, and its kind's own record. An entry that does not
 * parse - another version's, or damaged - is dropped when the page loads, as is every entry of
 * another person's. While any of them the reader sees is unconfirmed the page asks the browser to
 * confirm leaving (`beforeunload`), since a closed tab would lose it; a quiet one (a read mark) is
 * never shown, so it never asks.
 */

const ENTRY_PREFIX = "dispatch.write:";

/** The format's version: an entry of any other is dropped on load, never read. */
const FORMAT_VERSION = 1;

/** How long after its press a write a reload interrupted is made again on its own. One kept longer
 *  comes back refused (`SendInterruptedError`), for the reader to Retry under its key or discard:
 *  something they did that long ago may no longer be what they mean. A write the reader never
 *  sees (`quiet`) is let go instead. */
export const KEPT_SEND_RESUME_MS = 5 * 60_000;

/** Whether a write pressed at `pressedAt` that a reload interrupted is made again on its own now. */
export function resumesOnItsOwn(pressedAt: number, now = Date.now()): boolean {
  return now - pressedAt <= KEPT_SEND_RESUME_MS;
}

/** Where a write shows: a composer's own held state (`composer`), the shell's pending-writes
 *  indicator (`indicator`), or nowhere at all (`quiet`: a read mark, tried and kept like any write,
 *  and let go unconfirmed five minutes after its press). */
export const PRESENTATIONS = ["composer", "indicator", "quiet"] as const;

export type Presentation = (typeof PRESENTATIONS)[number];

/** A refusal as it is kept: the reader stopped the write, a reload interrupted it too long ago to
 *  be made again on its own, Dispatch refused it, or another error. */
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

/** Where a kept write stood when it was last written: waiting its turn behind an earlier write to
 *  its target, out within its deadline, out past it (`stopping` once the reader pressed Stop
 *  trying), or refused. */
export type KeptState =
  | { readonly status: "queued" }
  | { readonly status: "sending" }
  | { readonly status: "late"; readonly stopping: boolean }
  | { readonly status: "refused"; readonly refusal: KeptRefusal };

/** One field of an upload's multipart form as it is kept: a value, or the place of its file, which
 *  is kept apart (`kept-files.ts`) since no entry can hold one. */
export type KeptField =
  | { readonly name: string; readonly value: string }
  | { readonly name: string; readonly file: true };

/** A request as it is kept: its method, its path and its JSON body, or an upload's form. */
export type KeptRequest =
  | { readonly method: WriteMethod; readonly path: string; readonly body?: unknown }
  | { readonly method: WriteMethod; readonly path: string; readonly form: readonly KeptField[] };

/** One write as it is kept: everything a reload needs to make it again, show it and order it. */
export interface KeptWrite {
  readonly key: string;
  /** The kind of write (`WriteKind.name`), whose record `detail` is. */
  readonly kind: string;
  readonly login: string;
  readonly label: string;
  readonly presentation: Presentation;
  readonly target: string | null;
  readonly pressedAt: number;
  readonly state: KeptState;
  /** The request it makes: as it last went out, or, for one waiting its turn that has not gone
   *  out yet, as it will (`Writes`); `null` only for a kind that makes its writes again from its
   *  own record. */
  readonly request: KeptRequest | null;
  /** Whether an attempt of it went out under its key: one that never did is the reader's to
   *  discard while it waits its turn. */
  readonly sent: boolean;
  readonly detail: unknown;
}

/** A value's shape, checked: what a kept record is parsed by before it is trusted, and a type
 *  guard, so each is held to the type it stands for (`satisfies Shape<T>`) and nothing is cast. */
export type Shape<T> = (value: unknown) => value is T;

/** The type a shape checks for. */
export type ShapeOf<S> = S extends Shape<infer T> ? T : never;

/** The shapes a kept record is checked by: a record has each named field in its shape (a missing
 *  one reads as undefined, and others are not read), and `either` fits a value that one of its
 *  shapes fits. */
export const shape = {
  array:
    <T>(item: Shape<T>): Shape<readonly T[]> =>
    (value): value is readonly T[] =>
      Array.isArray(value) && value.every(item),
  boolean: ((value) => typeof value === "boolean") as Shape<boolean>,
  either:
    <S extends readonly Shape<unknown>[]>(...shapes: S): Shape<ShapeOf<S[number]>> =>
    (value): value is ShapeOf<S[number]> =>
      shapes.some((fits) => fits(value)),
  nullable:
    <T>(inner: Shape<T>): Shape<T | null> =>
    (value): value is T | null =>
      value === null || inner(value),
  number: ((value) => typeof value === "number" && Number.isFinite(value)) as Shape<number>,
  oneOf:
    <const V extends readonly (boolean | number | string | null)[]>(
      ...values: V
    ): Shape<V[number]> =>
    (value): value is V[number] =>
      values.includes(value as V[number]),
  optional:
    <T>(inner: Shape<T>): Shape<T | undefined> =>
    (value): value is T | undefined =>
      value === undefined || inner(value),
  record:
    <F extends Readonly<Record<string, Shape<unknown>>>>(
      fields: F
    ): Shape<{ readonly [K in keyof F]: ShapeOf<F[K]> }> =>
    (value): value is { readonly [K in keyof F]: ShapeOf<F[K]> } =>
      typeof value === "object" &&
      value !== null &&
      !Array.isArray(value) &&
      Object.entries(fields).every(([name, field]) => field(Reflect.get(value, name))),
  string: ((value) => typeof value === "string") as Shape<string>,
  unknown: ((_value) => true) as Shape<unknown>,
} as const;

const keptRefusalShape = shape.either(
  shape.record({ kind: shape.oneOf("interrupted", "stopped") }),
  shape.record({
    code: shape.nullable(shape.string),
    kind: shape.oneOf("api"),
    message: shape.string,
    status: shape.number,
  }),
  shape.record({ kind: shape.oneOf("error"), message: shape.string })
) satisfies Shape<KeptRefusal>;

const keptStateShape = shape.either(
  shape.record({ status: shape.oneOf("queued", "sending") }),
  shape.record({ status: shape.oneOf("late"), stopping: shape.boolean }),
  shape.record({ refusal: keptRefusalShape, status: shape.oneOf("refused") })
) satisfies Shape<KeptState>;

const keptFieldShape = shape.either(
  shape.record({ name: shape.string, value: shape.string }),
  shape.record({ file: shape.oneOf(true), name: shape.string })
) satisfies Shape<KeptField>;

const keptRequestShape = shape.either(
  shape.record({
    form: shape.array(keptFieldShape),
    method: shape.oneOf(...WRITE_METHODS),
    path: shape.string,
  }),
  shape.record({ body: shape.unknown, method: shape.oneOf(...WRITE_METHODS), path: shape.string })
) satisfies Shape<KeptRequest>;

/** An entry as it is written: the write and the format's version. */
const keptEntryShape = shape.record({
  detail: shape.unknown,
  key: shape.string,
  kind: shape.string,
  label: shape.string,
  login: shape.string,
  presentation: shape.oneOf(...PRESENTATIONS),
  pressedAt: shape.number,
  request: shape.nullable(keptRequestShape),
  sent: shape.boolean,
  state: keptStateShape,
  target: shape.nullable(shape.string),
  version: shape.oneOf(FORMAT_VERSION),
}) satisfies Shape<KeptWrite & { readonly version: typeof FORMAT_VERSION }>;

/** How `error`, a write's refusal, is kept. */
export function keptRefusal(error: Error): KeptRefusal {
  if (error instanceof SendInterruptedError) return { kind: "interrupted" };
  if (error instanceof SendStoppedError) return { kind: "stopped" };
  if (error instanceof ApiError) {
    return { code: error.code ?? null, kind: "api", message: error.message, status: error.status };
  }
  return { kind: "error", message: error.message };
}

/** The refusal `kept` names, rebuilt as the error the write was refused with. */
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

/** `request` as it is kept: an upload's form by its fields, its file's place among them. */
export function keptRequest({ body, method, path }: WriteRequest): KeptRequest {
  if (!(body instanceof FormData))
    return body === undefined ? { method, path } : { body, method, path };
  const form: KeptField[] = [];
  for (const [name, value] of body) {
    form.push(typeof value === "string" ? { name, value } : { file: true, name });
  }
  return { form, method, path };
}

/** The request `kept` names, whole again: an upload's form with `file` in its file's place, or
 *  `undefined` when it names a file and none is given. */
export function requestFrom(kept: KeptRequest, file?: File): WriteRequest | undefined {
  if (!("form" in kept)) return kept;
  const body = new FormData();
  for (const field of kept.form) {
    if ("value" in field) body.append(field.name, field.value);
    else if (file === undefined) return undefined;
    else body.append(field.name, file);
  }
  return { body, method: kept.method, path: kept.path };
}

/** The kept writes of one page: `storage` holds them across a reload, and `unload` is where the
 *  page asks to confirm leaving while any of them is unconfirmed. */
export class WriteStorage {
  readonly #storage: Storage | undefined;
  readonly #unload: EventTarget;
  /** The keys of the writes still out - queued, sending, late or stopping - that the reader sees:
   *  a quiet one (a read mark) is never shown, so leaving never asks for it. */
  readonly #unconfirmed = new Set<string>();
  #retired = false;

  /** `storage` is undefined where the browser gives the page none: writes are then not kept
   *  across a reload, and leaving is still confirmed while one is out. */
  constructor(storage: Storage | undefined, unload: EventTarget) {
    this.#storage = storage;
    this.#unload = unload;
  }

  /** Keeps `write` under its key, replacing what was kept there. One the storage refuses to write
   *  (full, or refused the page) removes what was kept under its key before rather than leaving
   *  it to stand for it, so a reload never resumes a state the write has left - a write the reader
   *  stopped never comes back as one still being tried. */
  keep(write: KeptWrite): void {
    if (this.#retired) return;
    const name = `${ENTRY_PREFIX}${write.key}`;
    try {
      this.#storage?.setItem(name, JSON.stringify({ ...write, version: FORMAT_VERSION }));
    } catch {
      // The write is still made; only its reload is lost.
      this.#remove(name);
    }
    this.#confirm(write.key, write.state.status === "refused" || write.presentation === "quiet");
  }

  /** Drops the write kept under `key`: it landed, or the reader dropped its refusal. */
  forget(key: string): void {
    if (this.#retired) return;
    this.#remove(`${ENTRY_PREFIX}${key}`);
    this.#confirm(key, true);
  }

  /** Every write `login` kept, in press order. An entry that is not one of this format's, or one
   *  another person kept, is dropped. */
  load(login: string): KeptWrite[] {
    const storage = this.#storage;
    if (storage === undefined || this.#retired) return [];
    const loaded: KeptWrite[] = [];
    for (const name of this.#names(storage)) {
      const entry = this.#read(storage, name);
      if (entry === undefined || `${ENTRY_PREFIX}${entry.key}` !== name || entry.login !== login) {
        this.#remove(name);
        continue;
      }
      const { version: _version, ...write } = entry;
      loaded.push(write);
    }
    return loaded.sort((a, b) => a.pressedAt - b.pressedAt);
  }

  /** Drops every kept write, and stops asking to confirm leaving: the person who pressed them
   *  signed out, or someone else signed in. */
  clear(): void {
    if (this.#retired) return;
    this.#dropAll();
  }

  /** Ends this storage, for the unit-test preload between tests: what it kept goes, and it keeps
   *  nothing more and asks nothing on leaving, whatever the store it served still sends. */
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
    if (storage === undefined) return;
    for (const name of this.#names(storage)) this.#remove(name);
  }

  #remove(name: string): void {
    try {
      this.#storage?.removeItem(name);
    } catch {
      // A storage that refuses even a removal holds nothing this page wrote.
    }
  }

  /** The names of every entry, read before any is removed. */
  #names(storage: Storage): string[] {
    return Array.from({ length: storage.length }, (_, index) => storage.key(index)).filter(
      (name): name is string => name?.startsWith(ENTRY_PREFIX) === true
    );
  }

  #read(storage: Storage, name: string) {
    try {
      const value: unknown = JSON.parse(storage.getItem(name) ?? "null");
      return keptEntryShape(value) ? value : undefined;
    } catch {
      return undefined;
    }
  }

  /** Marks the write under `key` confirmed - landed, refused or dropped - or still out, and asks
   *  to confirm leaving exactly while one is still out. */
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

let pageWrites: WriteStorage | undefined;

/** The page's kept writes: its `sessionStorage`, and its window's `beforeunload`. */
export function writeStorage(): WriteStorage {
  pageWrites ??= new WriteStorage(pageStorage(), window);
  return pageWrites;
}

/** Lets the page's kept writes go (`retire`), and the next store keeps its own afresh. The
 *  unit-test preload calls it after each test, since a store outlives its test while a write of
 *  it is out, and a later test's app would take up what it kept. */
export function resetWriteStorage(): void {
  pageWrites?.retire();
  pageWrites = undefined;
}
