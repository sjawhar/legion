import { DISPATCH_LOGIN_CHANGED } from "@legion/contracts";
import { type QueryClient, useQueryClient } from "@tanstack/react-query";
import { useRef, useSyncExternalStore } from "react";

import { ApiError, api, type WriteBounds, type WriteRequest } from "../../api/client";
import type { AuthenticatedUser } from "../../api/types";
import { newSendKey } from "../../lib/send-key";
import { indexedDbFiles, type KeptFiles } from "./kept-files";
import {
  retriesUnderItsKey,
  SEND_DEADLINE_MS,
  SendInterruptedError,
  SendStoppedError,
  WriteDiscardedError,
  writeUnderKey,
} from "./keyed-write";
import {
  KEPT_SEND_RESUME_MS,
  type KeptState,
  type KeptWrite,
  keptRefusal,
  keptRequest,
  type Presentation,
  refusalFrom,
  requestFrom,
  resumesOnItsOwn,
  type Shape,
  type WriteStorage,
  writeStorage,
} from "./write-storage";

/** Where a write stands: waiting its turn behind an earlier write to its target (`queued`), out
 *  within its deadline (`sending`), out past it (`late`, `stopping` once the reader pressed Stop
 *  trying), refused - by Dispatch, or stopped - and not yet retried or discarded, or made: Dispatch
 *  answered it, and its kind shows the answer until it lets it go (`WriteKind.keepsMade`). */
export type WriteState =
  | { readonly status: "queued" }
  | { readonly status: "sending" }
  | { readonly status: "late"; readonly stopping: boolean }
  | { readonly status: "refused"; readonly refusal: Error }
  | { readonly status: "made"; readonly answer: unknown };

/** A write apart from where it stands: its key, its kind and its kind's record (`detail`), the
 *  login of the person who pressed it (stamped at the press, never again), what it is called and
 *  where it shows, and the target its line is ordered on. */
export interface WriteBase<D = unknown> {
  readonly id: number;
  readonly key: string;
  readonly kind: string;
  readonly login: string | undefined;
  readonly label: string;
  readonly presentation: Presentation;
  /** Whether the surface that made it shows its refusal itself, so the store lets a refused one
   *  go: a form the reader is still in front of, holding their draft. */
  readonly inline: boolean;
  readonly target: string | undefined;
  readonly pressedAt: number;
  /** Its place in its target's line, in the order writes joined it; a Retry joins at the back. */
  readonly joined: number;
  /** Whether an attempt of it under its key has gone out. One that has not was never sent, so
   *  while it waits its turn it reads "Waiting to send" and is the reader's to discard. */
  readonly sent: boolean;
  /** How much of an upload's body its current attempt has sent, in percent; `undefined` for a
   *  write with no body going up. */
  readonly uploaded: number | undefined;
  /** Whether an attempt of it has ended without Dispatch's verdict - a connection that failed, an
   *  attempt gone silent, a 5xx - so it is being tried again, not still on its first try. */
  readonly retried: boolean;
  readonly detail: D;
}

/** One write the store holds, from its press until Dispatch answers it and its refusal is retried
 *  or discarded. */
export type Write<D = unknown> = WriteBase<D> & WriteState;

/** `write` apart from where it stands, for a new state to be put on. */
function baseOf<D>(write: Write<D>): WriteBase<D> {
  const { detail, id, inline, joined, key, kind, label, login, presentation, pressedAt } = write;
  const { retried, sent, target, uploaded } = write;
  return {
    detail,
    id,
    inline,
    joined,
    key,
    kind,
    label,
    login,
    presentation,
    pressedAt,
    retried,
    sent,
    target,
    uploaded,
  };
}

/** One attempt of a write, under the bounds the store gives it: its key, its login and its
 *  deadline. */
export type Attempt<T> = (bounds: WriteBounds) => Promise<T>;

/**
 * A kind of write: what its record is (`D`) and how it is kept (`K`), checked as a reload reads it
 * (`kept`), and what the store does for it beyond making it. Every kind is defined once
 * (`defineWriteKind`), so a reload finds any kept write's kind by its `name`.
 */
export interface WriteKind<D, K = D> {
  readonly name: string;
  readonly kept: Shape<K>;
  /** The record as it is kept; left out, the record itself. */
  readonly keep?: (detail: D) => K;
  /** The record a kept one names; left out, the kept one itself. */
  readonly restore?: (kept: K) => D;
  /** One attempt of a write of this kind a reload took up, from its record and the request it
   *  last went out as; left out, that request verbatim. `undefined` has nothing to make, and the
   *  write is let go. */
  readonly resume?: (detail: D, request: WriteRequest | null) => Attempt<unknown> | undefined;
  /** What a write of this kind that landed refreshes, whether or not a surface awaits it:
   *  `answer` is Dispatch's, `null` for a landing it had no answer to give for, and `awaited`
   *  says whether the surface that made it is still waiting for it. Left out, every query is read
   *  again for a write no surface awaits and for a landing without an answer. */
  readonly landed?: (
    client: QueryClient,
    write: Write<D>,
    answer: unknown,
    awaited: boolean
  ) => void;
  /** What a write of this kind undoes once it is refused, discarded or dropped. */
  readonly undone?: (client: QueryClient, write: Write<D>) => void;
  /** What a kind lets go of once whoever is signed in changes, or the person signs out: what it
   *  remembered of their answers, which the next person's writes must not build on. */
  readonly reset?: () => void;
  /** Whether a write of this kind Dispatch made and answered stays, `made`, until its surface lets
   *  it go (`letGo`); one made with no answer to give has nothing to show, and goes. */
  readonly keepsMade?: boolean;
  /** Told each time a write of this kind joins its line - its press, a Retry, a reload - with
   *  what settles once it lands or is refused, and whether it is past its deadline already. */
  readonly started?: (
    client: QueryClient,
    write: Write<D>,
    settled: Promise<unknown>,
    late: boolean
  ) => void;
}

// biome-ignore lint/suspicious/noExplicitAny: each write's record is read only by its own kind.
type AnyKind = WriteKind<any, any>;

const kinds = new Map<string, AnyKind>();

/** Defines a kind of write, once, so a reload finds it by name. */
export function defineWriteKind<D, K = D>(kind: WriteKind<D, K>): WriteKind<D, K> {
  kinds.set(kind.name, kind);
  return kind;
}

/** `write` as a write of `kind`, with its record typed, or `undefined` for another kind's. A
 *  write's record is only ever made by its own kind (`run`, `resume`). */
export function ofKind<D, K>(write: Write, kind: WriteKind<D, K>): Write<D> | undefined {
  return write.kind === kind.name ? (write as Write<D>) : undefined;
}

/** The kind of every write that has no record of its own: a status, a priority, an answer, a read
 *  mark. A reload makes it again as the request it last went out as, and a landing no surface
 *  awaits - or one without an answer - reads every query again, since the surface that made it
 *  may be gone. */
export const plainWrite = defineWriteKind<null>({
  kept: (value): value is null => value === null,
  name: "write",
});

/** One write to make: its kind and record, what the indicator names it, where it shows, the
 *  target it is ordered on, and one attempt of it. */
export interface WriteSpec<T, D, K> {
  readonly kind: WriteKind<D, K>;
  readonly detail: D;
  readonly label: string;
  readonly presentation: Presentation;
  readonly inline?: boolean;
  /** Writes to one target go out in press order: a later one waits while an earlier one is still
   *  being tried. */
  readonly target?: string;
  /** The key of a write the surface makes again after it ended without Dispatch's verdict (the
   *  reader stopped it): that write's own, so Dispatch answers it with what the first did. Left
   *  out, the press gets a key of its own. */
  readonly key?: string;
  readonly write: Attempt<T>;
  /** An upload's file, kept apart so a reload can make it again (`kept-files.ts`). */
  readonly file?: File;
  /** Whether the write ends the person's session - a sign-out - so a lapse of that session has
   *  done what it was for, and lets it go rather than holding it for the next sign-in. */
  readonly endsWithSession?: boolean;
}

/** What became of a write: it landed (with Dispatch's answer, `null` for none), was refused, or
 *  the reader discarded its refusal. */
export type WriteOutcome =
  | { readonly kind: "landed"; readonly write: Write; readonly answer: unknown }
  | { readonly kind: "refused"; readonly write: Write; readonly refusal: Error }
  | { readonly kind: "discarded"; readonly write: Write };

/** How a joined write starts once its turn comes: within its deadline, past it, or past it and
 *  stopped. */
type From = "late" | "sending" | "stopping";

/** A promise and what settles it: a press awaiting its write, or a kind awaiting its turn. */
interface Settlement {
  readonly promise: Promise<unknown>;
  readonly resolve: (value: unknown) => void;
  readonly reject: (reason: unknown) => void;
}

/** What the store holds of a write beside it: how to make it, the request it last went out as,
 *  what stops its trying, its file, the press that awaits it, and how its turn starts. */
interface Entry {
  write: Write;
  readonly attempt: Attempt<unknown>;
  request: WriteRequest | null;
  stop: AbortController;
  readonly file: File | undefined;
  /** The press waiting for it, until it settles; none for a Retry or a reload. */
  press: Settlement | undefined;
  /** How it starts once its turn comes, and what settles once it has; none while it is out. */
  turn: { readonly from: From; readonly settled: Settlement } | undefined;
  /** Ends a quiet write left unconfirmed five minutes after its press. */
  expiry: ReturnType<typeof setTimeout> | undefined;
  /** Whether its trying stopped for a lapsed session (`signedIn(undefined)`), so the attempt loop
   *  ending puts it back in its line to wait for the sign-in rather than refusing it. */
  held: boolean;
  readonly endsWithSession: boolean;
}

const NO_WRITES: readonly Write[] = Object.freeze([]);

/** The login of the person `client`'s app is signed in as (`["whoami"]`), which every write is
 *  stamped with at its press; undefined while no one is signed in. */
export function signedInLogin(client: QueryClient): string | undefined {
  return client.getQueryData<AuthenticatedUser>(["whoami"])?.login;
}

/**
 * Every write the page makes, one per press, beside the app's `QueryClient` (`writesOf`), above
 * the router, so leaving the page that made one never drops it: a composer's send, a broadcast, a
 * pin, a read mark, a status, an answer, a form's save. Each is made under a key minted at the
 * press and tried under it until Dispatch decides it (`writeUnderKey`), whether or not the browser
 * says it is online; past `SEND_DEADLINE_MS` it is late, and Stop trying ends it. Writes to one
 * target go out in press order: a later one waits only while an earlier one is still being tried,
 * and a refused or stopped one no longer holds the line (its Retry joins at the back). Every write
 * is kept (`write-storage.ts`, an upload's file in `kept-files.ts`) from its press until Dispatch
 * answers, so a reload takes it up again (`resume`). A refusal stays, with Retry and Discard,
 * until the reader takes one - unless its surface shows refusals itself (`inline`), or it is never
 * shown (`quiet`), when it goes at once. A write belongs to the person who pressed it: a session
 * that lapses (whoami answers 401, or the write itself is answered 401) stops every write's trying
 * and starts none, but keeps them all, in memory and in storage, until someone signs in - the same
 * person's sign-in takes them up again, and anyone else's drops every write of another login
 * (`signedIn`). Dispatch refuses one that reaches it under another session first (`Dispatch-Login`,
 * `409 LOGIN_CHANGED`).
 */
export class Writes {
  readonly #client: QueryClient;
  readonly #kept: WriteStorage;
  readonly #files: KeptFiles;
  readonly #entries = new Map<number, Entry>();
  #rows: readonly Write[] = NO_WRITES;
  readonly #listeners = new Set<() => void>();
  readonly #outcomes = new Set<(outcome: WriteOutcome) => void>();
  #ids = 0;
  #joins = 0;
  #resumed = false;
  /** Whether the session of the person signed in has lapsed: no write starts until someone signs
   *  in again. */
  #lapsed = false;
  /** Who was signed in last; `undefined` before the first sign-in and once a session lapsed. */
  #login: string | undefined;

  constructor(
    client: QueryClient,
    kept: WriteStorage = writeStorage(),
    files: KeptFiles = indexedDbFiles()
  ) {
    this.#client = client;
    this.#kept = kept;
    this.#files = files;
  }

  /** Every write held, in press order: one array until a write changes. */
  readonly rows = (): readonly Write[] => this.#rows;

  readonly subscribe = (listener: () => void): (() => void) => {
    this.#listeners.add(listener);
    return () => this.#listeners.delete(listener);
  };

  /** Tells `listener` what becomes of every write from now on. */
  readonly onOutcome = (listener: (outcome: WriteOutcome) => void): (() => void) => {
    this.#outcomes.add(listener);
    return () => this.#outcomes.delete(listener);
  };

  /** Makes `spec`'s write, stamped with the person signed in now, and answers with Dispatch's 2xx,
   *  `null` for a write that landed with no answer to give, or rejects with its refusal - or
   *  `SendStoppedError` once the reader stops it. */
  run<T, D, K>(spec: WriteSpec<T, D, K>): Promise<T | null> {
    const press = Promise.withResolvers<unknown>();
    const login = signedInLogin(this.#client);
    const key = spec.key ?? newSendKey();
    if (spec.file !== undefined && login !== undefined) void this.#files.put(key, login, spec.file);
    const entry: Entry = {
      attempt: spec.write,
      endsWithSession: spec.endsWithSession ?? false,
      expiry: undefined,
      file: spec.file,
      held: false,
      press,
      request: null,
      stop: new AbortController(),
      turn: undefined,
      write: {
        detail: spec.detail,
        id: this.#nextId(),
        inline: spec.inline ?? false,
        joined: this.#nextJoin(),
        key,
        kind: spec.kind.name,
        label: spec.label,
        login,
        presentation: spec.presentation,
        pressedAt: Date.now(),
        retried: false,
        sent: false,
        status: "queued",
        target: spec.target,
        uploaded: undefined,
      },
    };
    this.#join(entry, "sending");
    if (entry.write.status === "queued") this.#plan(entry);
    return press.promise as Promise<T | null>;
  }

  /** Stops trying the late write `id` (Stop trying): no attempt is made after the one in flight,
   *  whose answer is still the write's, and it is then refused with `SendStoppedError`, which a
   *  Retry makes again under the same key. Says whether it stopped one. */
  stop(id: number): boolean {
    const entry = this.#entries.get(id);
    if (entry?.write.status !== "late" || entry.write.stopping) return false;
    entry.stop.abort();
    this.#update(entry, { ...baseOf(entry.write), status: "late", stopping: true });
    return true;
  }

  /** Makes the refused write `id` again, at the back of its target's line: under its own key when
   *  it ended without Dispatch's verdict, under a new one when Dispatch refused it
   *  (`retriesUnderItsKey`). */
  retry(id: number): void {
    const entry = this.#entries.get(id);
    if (entry?.write.status !== "refused") return;
    const key = retriesUnderItsKey(entry.write.refusal) ? entry.write.key : newSendKey();
    if (key !== entry.write.key) this.#kept.forget(entry.write.key);
    entry.stop = new AbortController();
    entry.write = {
      ...baseOf(entry.write),
      joined: this.#nextJoin(),
      key,
      pressedAt: Date.now(),
      retried: false,
      sent: key === entry.write.key && entry.write.sent,
      status: "queued",
      uploaded: undefined,
    };
    this.#join(entry, "sending");
  }

  /** Drops the write `id` the reader discards, and says whether there was one: a refused one, or
   *  one waiting its turn that was never sent, whose press is refused (`WriteDiscardedError`). A
   *  write that is out, or that went out before and waits to go again, is not the reader's to
   *  drop: it may have reached Dispatch, so it stays until Dispatch answers or the reader stops
   *  it. */
  discard(id: number): boolean {
    const entry = this.#entries.get(id);
    if (entry === undefined) return false;
    const waiting = entry.write.status === "queued" && !entry.write.sent;
    if (entry.write.status !== "refused" && !waiting) return false;
    const discarded = new WriteDiscardedError();
    entry.press?.reject(discarded);
    entry.press = undefined;
    entry.turn?.settled.reject(discarded);
    entry.turn = undefined;
    this.#remove(entry);
    kinds.get(entry.write.kind)?.undone?.(this.#client, entry.write);
    this.#emit({ kind: "discarded", write: entry.write });
    this.#advance(entry.write.target);
    return true;
  }

  /** Lets the made write `id` go, as its surface does once it is gone. */
  letGo(id: number): void {
    const entry = this.#entries.get(id);
    if (entry?.write.status === "made") this.#remove(entry);
  }

  /** Writes `change` over the record of the refused write `id`, a write of `kind`: what the
   *  reader wrote over a refusal, which its Retry sends and a reload restores. */
  amend<D, K>(id: number, kind: WriteKind<D, K>, change: (detail: D) => D): void {
    const entry = this.#entries.get(id);
    const write = entry === undefined ? undefined : ofKind(entry.write, kind);
    if (entry === undefined || write?.status !== "refused") return;
    this.#update(entry, { ...write, detail: change(write.detail) });
  }

  /**
   * Who is signed in now: `login`, or no one (`undefined`: the session lapsed, whoami answered
   * 401). A lapse keeps every write, in memory and in storage, a refused one's draft and an
   * upload's file with it: each one out stops trying after the attempt in flight, and none starts,
   * until someone signs in. The same person's sign-in takes them up again where they stood, each in
   * its line; anyone else's drops every write not theirs, its trying stopped and its press refused.
   * A sign-out still out (`endsWithSession`) has nothing left to do once the session lapses, and
   * goes. Each kind lets go of what it remembered of the person before (`reset`) whenever who is
   * signed in changes.
   */
  signedIn(login: string | undefined): void {
    if (login !== this.#login) this.#resetKinds();
    this.#login = login;
    if (login === undefined) {
      this.#lapsed = true;
      for (const entry of [...this.#entries.values()]) this.#hold(entry);
      return;
    }
    this.#lapsed = false;
    for (const entry of [...this.#entries.values()]) {
      if (entry.write.login !== login) this.#drop(entry);
    }
    void this.#files.keepOnly(login);
    const targets = new Set([...this.#entries.values()].map((entry) => entry.write.target));
    for (const target of targets) this.#advance(target);
  }

  /** Lets every write go, out or refused, and everything kept with them, every kept file
   *  included: the person who made them signed out. */
  clear(): void {
    for (const entry of [...this.#entries.values()]) this.#drop(entry);
    this.#resetKinds();
    void this.#files.keepOnly(undefined);
    this.#kept.clear();
  }

  /** Drops every write `login` pressed, out or refused, its trying stopped and everything kept
   *  of it, its file included: that person signed out in another tab of this browser, which
   *  discards their writes in every tab (`app.tsx`). */
  dropLogin(login: string): void {
    for (const entry of [...this.#entries.values()]) {
      if (entry.write.login === login) this.#drop(entry);
    }
  }

  /** Has every kind let go of what it remembered of the person signed in until now. */
  #resetKinds(): void {
    for (const kind of kinds.values()) kind.reset?.();
  }

  /**
   * Takes up, once, the writes a reload interrupted that `login`, the person signed in, pressed
   * (`write-storage.ts`), in press order, each in its target's line; another person's are dropped
   * unmade. One still out and pressed within `KEPT_SEND_RESUME_MS` is made again under its key, as
   * the request it last was, past its deadline already if it was then, and Dispatch answers the
   * repeat with what the first did; one the reader was stopping is asked once more and lands or
   * ends stopped. One pressed longer ago is not made again on its own: it shows refused
   * (`SendInterruptedError`), for a Retry under its key or Discard. A refusal shows again. A quiet
   * write is let go rather than shown. The app calls this once signed in.
   */
  resume(login: string): void {
    if (this.#resumed) return;
    this.#resumed = true;
    const now = Date.now();
    for (const kept of this.#kept.load(login)) {
      const kind = kinds.get(kept.kind);
      if (kind === undefined || !kind.kept(kept.detail)) {
        this.#kept.forget(kept.key);
        continue;
      }
      const detail = kind.restore === undefined ? kept.detail : kind.restore(kept.detail);
      if (kept.request !== null && "form" in kept.request) {
        void this.#files.get(kept.key).then((file) => this.#resume(kept, kind, detail, now, file));
      } else {
        this.#resume(kept, kind, detail, now, undefined);
      }
    }
    void this.#files.keepOnly(login);
  }

  #resume(kept: KeptWrite, kind: AnyKind, detail: unknown, now: number, file?: File): void {
    const request = kept.request === null ? null : (requestFrom(kept.request, file) ?? null);
    let attempt: Attempt<unknown> | undefined;
    if (kind.resume !== undefined) attempt = kind.resume(detail, request);
    else if (request !== null) attempt = (bounds) => api.send(request, bounds);
    const out = kept.state.status !== "refused";
    const onItsOwn = out && resumesOnItsOwn(kept.pressedAt, now);
    if (attempt === undefined || (kept.presentation === "quiet" && !onItsOwn)) {
      this.#kept.forget(kept.key);
      if (file !== undefined) void this.#files.delete(kept.key);
      return;
    }
    const entry: Entry = {
      attempt,
      endsWithSession: false,
      expiry: undefined,
      file,
      held: false,
      press: undefined,
      request,
      stop: new AbortController(),
      turn: undefined,
      write: {
        detail,
        id: this.#nextId(),
        inline: false,
        joined: this.#nextJoin(),
        key: kept.key,
        kind: kept.kind,
        label: kept.label,
        login: kept.login,
        presentation: kept.presentation,
        pressedAt: kept.pressedAt,
        retried: false,
        sent: kept.sent,
        status: "queued",
        target: kept.target ?? undefined,
        uploaded: undefined,
      },
    };
    if (onItsOwn) {
      this.#join(entry, resumedFrom(kept.state));
      return;
    }
    const refusal =
      kept.state.status === "refused"
        ? refusalFrom(kept.state.refusal)
        : new SendInterruptedError();
    this.#entries.set(entry.write.id, entry);
    this.#update(entry, { ...entry.write, refusal, status: "refused" });
  }

  /** Puts `entry` in its target's line, to start at once if nothing ahead of it is still being
   *  tried - within its deadline (`sending`), as a write already past it (`late`), or as one past
   *  it the reader has stopped (`stopping`), which makes one attempt and no other. */
  #join(entry: Entry, from: From): void {
    this.#entries.set(entry.write.id, entry);
    const settled = Promise.withResolvers<unknown>();
    // The press settles its own surface; this one only tells the kind (`started`).
    settled.promise.catch(() => {});
    entry.turn = { from, settled };
    if (entry.write.presentation === "quiet" && entry.expiry === undefined) {
      const left = entry.write.pressedAt + KEPT_SEND_RESUME_MS - Date.now();
      entry.expiry = setTimeout(() => this.#drop(entry), Math.max(0, left));
    }
    this.#update(entry, entry.write);
    kinds
      .get(entry.write.kind)
      ?.started?.(this.#client, entry.write, settled.promise, from !== "sending");
    this.#advance(entry.write.target);
  }

  /** Starts every write of `target` whose turn has come: in join order, each queued one that no
   *  earlier one is still being tried ahead of. A write with no target starts at once. None starts
   *  while the session is lapsed. */
  #advance(target: string | undefined): void {
    if (this.#lapsed) return;
    const line = [...this.#entries.values()]
      .filter((entry) => entry.write.target === target)
      .sort((a, b) => a.write.joined - b.write.joined);
    for (const entry of line) {
      const { status } = entry.write;
      if (status === "queued") {
        this.#start(entry);
        if (target !== undefined) return;
      } else if (target !== undefined && (status === "sending" || status === "late")) {
        return;
      }
    }
  }

  /** Makes the write `entry` under its key until Dispatch decides it. */
  #start(entry: Entry): void {
    const { turn } = entry;
    if (turn === undefined) return;
    entry.turn = undefined;
    const { stop } = entry;
    if (turn.from === "stopping") stop.abort();
    this.#update(
      entry,
      turn.from === "sending"
        ? { ...baseOf(entry.write), status: "sending" }
        : { ...baseOf(entry.write), status: "late", stopping: turn.from === "stopping" }
    );
    const current = () => this.#entries.get(entry.write.id) === entry && entry.stop === stop;
    const deadline =
      turn.from === "sending"
        ? setTimeout(() => {
            if (current() && entry.write.status === "sending") {
              this.#update(entry, { ...baseOf(entry.write), status: "late", stopping: false });
            }
          }, SEND_DEADLINE_MS)
        : undefined;
    writeUnderKey(entry.attempt, entry.write.key, {
      login: entry.write.login,
      sent: (request) => {
        if (!current()) return;
        entry.request = request;
        if (entry.write.sent) this.#keep(entry);
        else this.#update(entry, { ...entry.write, sent: true });
      },
      uploading: (loaded, total) => {
        if (!current() || total <= 0) return;
        const uploaded = Math.min(100, Math.floor((loaded * 100) / total));
        if (uploaded !== entry.write.uploaded) this.#show(entry, { ...entry.write, uploaded });
      },
      failed: () => {
        if (current()) this.#show(entry, { ...entry.write, retried: true, uploaded: undefined });
      },
      signal: stop.signal,
      // The person's session lapsed under the write: the page learns who is signed in, and a lapse
      // holds every write until the sign-in that follows (`signedIn`).
      unauthorized: () => void this.#client.invalidateQueries({ queryKey: ["whoami"] }),
    })
      .then(
        (answer) => {
          entry.held = false;
          if (current()) this.#landed(entry, answer);
          turn.settled.resolve(answer);
        },
        (error: unknown) => {
          const refusal = error instanceof Error ? error : new Error(String(error));
          const held = entry.held;
          entry.held = false;
          if (current()) {
            if (held && refusal instanceof SendStoppedError) this.#rejoin(entry);
            else this.#refused(entry, refusal);
          }
          turn.settled.reject(refusal);
        }
      )
      .finally(() => clearTimeout(deadline));
  }

  /** Stops `entry`'s trying for a lapsed session: one out ends after the attempt in flight, whose
   *  answer is still its own, and goes back in its line (`#rejoin`); a queued one waits where it
   *  is. A sign-out goes instead, its session ended, and one the reader was stopping ends as they
   *  asked. */
  #hold(entry: Entry): void {
    if (entry.endsWithSession) {
      this.#drop(entry);
      return;
    }
    const { write } = entry;
    if (write.status === "sending" || (write.status === "late" && !write.stopping)) {
      entry.held = true;
      entry.stop.abort();
    }
  }

  /** Puts `entry`, whose trying a lapse stopped, back in its line where it stood - past its
   *  deadline already if it was then - to go once its person signs in again. */
  #rejoin(entry: Entry): void {
    const late = entry.write.status === "late";
    entry.stop = new AbortController();
    entry.write = { ...baseOf(entry.write), status: "queued" };
    this.#join(entry, late ? "late" : "sending");
  }

  /** The write `entry` landed with `answer` (`null` for a landing with none): it leaves, or stays
   *  made for its kind to show, and its kind refreshes what it wrote. */
  #landed(entry: Entry, answer: unknown): void {
    const kind = kinds.get(entry.write.kind);
    const awaited = entry.press !== undefined;
    entry.press?.resolve(answer);
    entry.press = undefined;
    if (kind?.keepsMade === true && answer !== null) {
      clearTimeout(entry.expiry);
      this.#forget(entry);
      this.#update(entry, { ...baseOf(entry.write), answer, status: "made" });
    } else {
      this.#remove(entry);
    }
    if (kind?.landed !== undefined) kind.landed(this.#client, entry.write, answer, awaited);
    else if (!awaited || answer === null) void this.#client.invalidateQueries();
    this.#emit({ answer, kind: "landed", write: entry.write });
    this.#advance(entry.write.target);
  }

  /** The write `entry` was refused: it stays for Retry or Discard - unless its surface shows the
   *  refusal itself or it is never shown - and the next write of its target goes. One Dispatch
   *  refused because someone else is signed in goes with every write of its login, and the page
   *  reads who is signed in again. */
  #refused(entry: Entry, refusal: Error): void {
    entry.press?.reject(refusal);
    entry.press = undefined;
    if (
      refusal instanceof ApiError &&
      refusal.status === 409 &&
      refusal.code === DISPATCH_LOGIN_CHANGED
    ) {
      for (const other of [...this.#entries.values()]) {
        if (other.write.login === entry.write.login) this.#drop(other);
      }
      void this.#client.invalidateQueries({ queryKey: ["whoami"] });
      return;
    }
    const write: Write = { ...baseOf(entry.write), refusal, status: "refused" };
    if (entry.write.inline || entry.write.presentation === "quiet") this.#remove(entry);
    else this.#update(entry, write);
    kinds.get(write.kind)?.undone?.(this.#client, write);
    this.#emit({ kind: "refused", refusal, write });
    this.#advance(write.target);
  }

  /** Drops `entry` wherever it stands: its trying stops, its press is refused, nothing of it is
   *  kept, and its kind undoes what it showed of it. */
  #drop(entry: Entry): void {
    if (this.#entries.get(entry.write.id) !== entry) return;
    entry.stop.abort();
    entry.press?.reject(new SendStoppedError());
    entry.press = undefined;
    entry.turn?.settled.reject(new SendStoppedError());
    entry.turn = undefined;
    this.#remove(entry);
    kinds.get(entry.write.kind)?.undone?.(this.#client, entry.write);
    this.#advance(entry.write.target);
  }

  #remove(entry: Entry): void {
    clearTimeout(entry.expiry);
    if (this.#entries.get(entry.write.id) !== entry) return;
    this.#entries.delete(entry.write.id);
    this.#forget(entry);
    this.#publish();
  }

  /** Lets go of what is kept of `entry`: its entry, and an upload's file. */
  #forget(entry: Entry): void {
    this.#kept.forget(entry.write.key);
    if (entry.file !== undefined) void this.#files.delete(entry.write.key);
  }

  /** Puts `write` in the place of `entry`'s, keeps what a reload must take up, and tells every
   *  listener. */
  #update(entry: Entry, write: Write): void {
    entry.write = write;
    if (this.#entries.get(write.id) !== entry) return;
    this.#keep(entry);
    this.#publish();
  }

  /** Keeps the write `entry` as it stands, for the person who pressed it. */
  #keep(entry: Entry): void {
    const { write } = entry;
    const state = keptState(write);
    if (write.login === undefined || state === undefined) return;
    const kind = kinds.get(write.kind);
    this.#kept.keep({
      detail: kind?.keep === undefined ? write.detail : kind.keep(write.detail),
      key: write.key,
      kind: write.kind,
      label: write.label,
      login: write.login,
      presentation: write.presentation,
      pressedAt: write.pressedAt,
      request: entry.request === null ? null : keptRequest(entry.request),
      sent: write.sent,
      state,
      target: write.target ?? null,
    });
  }

  /** Puts `write` in the place of `entry`'s and tells every listener, keeping nothing: what changed
   *  - an upload's progress, a failed attempt - is not what a reload takes up. */
  #show(entry: Entry, write: Write): void {
    entry.write = write;
    if (this.#entries.get(write.id) === entry) this.#publish();
  }

  /** Keeps `entry`, which waits its turn before any attempt of it went out, with the request it
   *  will make, so a reload makes it again in its place in the line: its attempt is asked for the
   *  request (`sent`) and stopped there, before anything goes out. A kind that makes its writes
   *  again from its own record (`resume`) needs none. */
  #plan(entry: Entry): void {
    if (entry.request !== null || kinds.get(entry.write.kind)?.resume !== undefined) return;
    const planned = new Error("the request is planned, not sent");
    const sent = (request: WriteRequest) => {
      if (entry.request === null && this.#entries.get(entry.write.id) === entry) {
        entry.request = request;
        this.#keep(entry);
      }
      throw planned;
    };
    try {
      entry.attempt({ key: entry.write.key, login: entry.write.login, sent }).catch(() => {});
    } catch {
      // The attempt stopped at its request, as it was asked to.
    }
  }

  #publish(): void {
    const rows = [...this.#entries.values()]
      .map((entry) => entry.write)
      .sort((a, b) => a.id - b.id);
    this.#rows = rows.length === 0 ? NO_WRITES : rows;
    for (const listener of this.#listeners) listener();
  }

  #emit(outcome: WriteOutcome): void {
    for (const listener of this.#outcomes) listener(outcome);
  }

  #nextId(): number {
    this.#ids += 1;
    return this.#ids;
  }

  #nextJoin(): number {
    this.#joins += 1;
    return this.#joins;
  }
}

/** How a kept state resumes once its write is made again. */
function resumedFrom(state: KeptState): From {
  if (state.status === "late") return state.stopping ? "stopping" : "late";
  return "sending";
}

/** How `write`'s state is kept, or `undefined` for one that is made and so kept no longer. */
function keptState(write: Write): KeptState | undefined {
  switch (write.status) {
    case "queued":
    case "sending":
      return { status: write.status };
    case "late":
      return { status: "late", stopping: write.stopping };
    case "refused":
      return { refusal: keptRefusal(write.refusal), status: "refused" };
    case "made":
      return undefined;
  }
}

const stores = new WeakMap<QueryClient, Writes>();

/** The writes that live with `client`: one store per app, as there is one client. */
export function writesOf(client: QueryClient): Writes {
  let store = stores.get(client);
  if (store === undefined) {
    store = new Writes(client);
    stores.set(client, store);
  }
  return store;
}

export function useWrites(): Writes {
  return writesOf(useQueryClient());
}

/**
 * What `select` picks out of every write the store holds, re-rendering only when that changes:
 * the pick is made again only when a write changes or `key` - what the pick is of, by name - does,
 * and handed back as the same value while it holds the same items (an array) or is the same value.
 */
export function useWriteSelection<R>(key: string, select: (rows: readonly Write[]) => R): R {
  const store = useWrites();
  const last = useRef<{
    readonly key: string;
    readonly rows: readonly Write[];
    readonly picked: R;
  }>(undefined);
  const pick = useRef(select);
  pick.current = select;
  return useSyncExternalStore(store.subscribe, () => {
    const rows = store.rows();
    const before = last.current;
    if (before?.rows === rows && before.key === key) return before.picked;
    const picked = pick.current(rows);
    const same =
      before !== undefined &&
      (Object.is(before.picked, picked) ||
        (Array.isArray(before.picked) &&
          Array.isArray(picked) &&
          before.picked.length === picked.length &&
          before.picked.every((item, index) => Object.is(item, picked[index]))));
    const kept = same ? before.picked : picked;
    last.current = { key, picked: kept, rows };
    return kept;
  });
}
