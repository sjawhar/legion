import { type QueryClient, useQueryClient } from "@tanstack/react-query";
import { useCallback, useSyncExternalStore } from "react";

import { AnswerNotKeptError, api, type WriteBounds, type WriteRequest } from "../../api/client";
import { newSendKey } from "../../lib/send-key";
import {
  retriesUnderItsKey,
  SEND_DEADLINE_MS,
  SendInterruptedError,
  writeUnderKey,
} from "./keyed-write";
import {
  keptRefusal,
  refusalFrom,
  resumesOnItsOwn,
  shape,
  signedInLogin,
  type WriteStorage,
  writeStorage,
} from "./write-storage";

/** One write the shell's indicator shows (`PendingWritesIndicator`): out within its deadline
 *  (`sending`), out past it (`late`, `stopping` once the reader pressed Stop trying), or refused
 *  and not yet retried or discarded. `label` names it in the reader's words. */
export interface PendingWrite {
  readonly id: number;
  readonly key: string;
  readonly label: string;
  readonly pressedAt: number;
  readonly refusal: Error | undefined;
  readonly status: "late" | "refused" | "sending";
  readonly stopping: boolean;
}

/** A write to make: what the indicator names it, and one attempt of it under the bounds the queue
 *  gives - its key and its deadline. */
export interface WriteSpec<T> {
  readonly label: string;
  readonly write: (bounds: WriteBounds) => Promise<T>;
  /** Whether the surface that made the write shows a refusal itself - a form the reader is still
   *  in front of, holding their draft - so the indicator shows the write only while it is out. */
  readonly inline?: boolean;
  /** The key of a write the surface makes again after it ended without Dispatch's verdict (the
   *  reader stopped it): that write's own, so Dispatch answers it with what the first did. Left
   *  out, the press gets a key of its own. */
  readonly key?: string;
}

/** What the queue holds of a write beside its row: how to make it again, and how to stop it. */
interface Running {
  readonly attempt: (bounds: WriteBounds) => Promise<unknown>;
  readonly inline: boolean;
  /** The request as it last went out, kept for a reload to make again; `null` before the first
   *  attempt and for a write whose body is no JSON (an upload's file). */
  request: WriteRequest | null;
  readonly stop: AbortController;
}

/** A write as it is kept across a reload, beside its request: what the indicator names it. */
interface KeptPendingWrite {
  readonly label: string;
}

const keptPendingWrite = shape.record({ label: shape.string });

const NO_WRITES: readonly PendingWrite[] = Object.freeze([]);

/**
 * Every write the page makes that no composer shows - a status, a priority, an assignee, labels,
 * an answer, a comment resolved, a delivery retried, a form's save - one per press, beside the
 * app's `QueryClient` (`pendingWrites`), above the router, so leaving the page that made one never
 * drops it. Each is made under a key minted at the press (`run`) and tried under it until Dispatch
 * decides it (`writeUnderKey`), whether or not the browser says it is online; past
 * `SEND_DEADLINE_MS` it is late, and Stop trying ends it. It is kept in the page's write queue
 * (`write-storage.ts`) from its first attempt until Dispatch answers, so a reload takes it up
 * again (`resume`). A refusal stays, with Retry and Discard, until the reader takes one - unless
 * the surface that made it shows refusals itself (`inline`). The surface keeps its own optimistic
 * state, and rolls it back when the write it awaited is refused or stopped; a Retry that lands
 * later refreshes every query, since that surface may be gone.
 */
export class PendingWrites {
  readonly #client: QueryClient;
  readonly #kept: WriteStorage;
  #rows: readonly PendingWrite[] = NO_WRITES;
  readonly #running = new Map<number, Running>();
  readonly #listeners = new Set<() => void>();
  #ids = 0;
  #resumed = false;

  constructor(client: QueryClient, kept: WriteStorage = writeStorage()) {
    this.#client = client;
    this.#kept = kept;
  }

  /** Every write held, in press order: one array until a write changes. */
  readonly rows = (): readonly PendingWrite[] => this.#rows;

  readonly subscribe = (listener: () => void): (() => void) => {
    this.#listeners.add(listener);
    return () => this.#listeners.delete(listener);
  };

  /** Makes `spec`'s write under its key - a key of its own unless the surface names the one of the
   *  write it is making again - and answers with Dispatch's 2xx, or rejects with its refusal,
   *  `SendStoppedError` once the reader stops it, or `AnswerNotKeptError` for a write that landed
   *  without an answer to hand back. */
  run<T>({ inline = false, key = newSendKey(), label, write }: WriteSpec<T>): Promise<T> {
    this.#ids += 1;
    return this.#start(this.#ids, write, null, inline, { key, label, pressedAt: Date.now() });
  }

  /** Stops trying the late write `id` (Stop trying): no attempt is made after the one in flight,
   *  whose answer is still the write's, and it is then refused with `SendStoppedError`, which a
   *  Retry makes again under the same key. Says whether it stopped one. */
  stop(id: number): boolean {
    const row = this.#find(id);
    if (row?.status !== "late" || row.stopping) return false;
    this.#running.get(id)?.stop.abort();
    this.#replace(id, { ...row, stopping: true });
    return true;
  }

  /** Makes the refused write `id` again: under its own key when it ended without Dispatch's
   *  verdict, under a new one when Dispatch refused it (`retriesUnderItsKey`). */
  retry(id: number): void {
    const row = this.#find(id);
    const running = this.#running.get(id);
    if (row?.status !== "refused" || running === undefined) return;
    this.#start(id, running.attempt, running.request, false, {
      key: retriesUnderItsKey(row.refusal) ? row.key : newSendKey(),
      label: row.label,
      pressedAt: Date.now(),
    }).then(
      () => void this.#client.invalidateQueries(),
      () => undefined
    );
  }

  /** Drops the refused write `id`, and says whether there was one. A write still out is not the
   *  reader's to drop: it stays until Dispatch answers or the reader stops it. */
  discard(id: number): boolean {
    if (this.#find(id)?.status !== "refused") return false;
    this.#running.delete(id);
    this.#replace(id, undefined);
    return true;
  }

  /** Lets every write go, out or refused: the person who made them signed out. One still out
   *  stops trying, and its answer changes nothing here. */
  clear(): void {
    for (const running of this.#running.values()) running.stop.abort();
    this.#running.clear();
    this.#rows = NO_WRITES;
    for (const listener of this.#listeners) listener();
  }

  /**
   * Takes up, once, the writes a reload interrupted that `login`, the person signed in, made
   * (`write-storage.ts`), in press order; another person's are dropped unmade. One still out and
   * pressed within `KEPT_SEND_RESUME_MS` is made again under its key, as the request it last
   * was, past its deadline already if it was then, and Dispatch answers the repeat with what the
   * first did; one the reader was stopping is asked once more and lands or ends stopped. One
   * pressed longer ago is not made again on its own: it shows refused (`SendInterruptedError`),
   * for a Retry under its key or Discard. A refusal shows again. Each that lands refreshes every
   * query. The app calls this once signed in.
   */
  resume(login: string): void {
    if (this.#resumed) return;
    this.#resumed = true;
    const kept = this.#kept
      .load("write", login, (detail) =>
        keptPendingWrite(detail) ? (detail as KeptPendingWrite) : undefined
      )
      .sort((a, b) => a.pressedAt - b.pressedAt);
    const now = Date.now();
    for (const { detail, key, pressedAt, refusal, request, state } of kept) {
      if (request === null) {
        this.#kept.forget(key);
        continue;
      }
      this.#ids += 1;
      const id = this.#ids;
      const attempt = (bounds: WriteBounds) => api.send(request, bounds);
      const base = { key, label: detail.label, pressedAt };
      if (state !== "refused" && resumesOnItsOwn(pressedAt, now)) {
        this.#start(id, attempt, request, false, base, state === "queued" ? "sending" : state).then(
          () => void this.#client.invalidateQueries(),
          () => undefined
        );
        continue;
      }
      this.#running.set(id, { attempt, inline: false, request, stop: new AbortController() });
      this.#replace(id, {
        ...base,
        id,
        refusal:
          state === "refused" && refusal !== null
            ? refusalFrom(refusal)
            : new SendInterruptedError(),
        status: "refused",
        stopping: false,
      });
    }
  }

  /** Holds the write `id` as out - within its deadline (`sending`), past it (`late`), or past it
   *  with the reader stopping it, which makes one attempt and no other - and makes it. */
  #start<T>(
    id: number,
    attempt: (bounds: WriteBounds) => Promise<T>,
    request: WriteRequest | null,
    inline: boolean,
    base: Pick<PendingWrite, "key" | "label" | "pressedAt">,
    from: "late" | "sending" | "stopping" = "sending"
  ): Promise<T> {
    const stop = new AbortController();
    if (from === "stopping") stop.abort();
    const running: Running = { attempt, inline, request, stop };
    this.#running.set(id, running);
    this.#replace(id, {
      ...base,
      id,
      refusal: undefined,
      status: from === "sending" ? "sending" : "late",
      stopping: from === "stopping",
    });
    const deadline =
      from === "sending"
        ? setTimeout(() => {
            const row = this.#find(id);
            if (row?.status === "sending" && row.key === base.key) {
              this.#replace(id, { ...row, status: "late" });
            }
          }, SEND_DEADLINE_MS)
        : undefined;
    return writeUnderKey(attempt, base.key, {
      sent: (sent) => {
        if (sent.body instanceof FormData || this.#running.get(id) !== running) return;
        running.request = sent;
        const row = this.#find(id);
        if (row !== undefined) this.#remember(row, row.key);
      },
      signal: stop.signal,
    })
      .then(
        (answer) => {
          this.#settled(id, running, undefined);
          return answer;
        },
        (error: unknown) => {
          const failure = error instanceof Error ? error : new Error(String(error));
          if (failure instanceof AnswerNotKeptError) {
            this.#settled(id, running, undefined);
            void this.#client.invalidateQueries();
          } else {
            this.#settled(id, running, failure);
          }
          throw failure;
        }
      )
      .finally(() => clearTimeout(deadline));
  }

  /** The write `id` has its answer: it leaves on a landing, and on a refusal the surface shows
   *  itself; any other refusal stays for Retry or Discard. Nothing changes for a write the queue
   *  let go of meanwhile (`clear`). */
  #settled(id: number, running: Running, refusal: Error | undefined): void {
    if (this.#running.get(id) !== running) return;
    const row = this.#find(id);
    if (row === undefined) return;
    if (refusal === undefined || running.inline) {
      this.#running.delete(id);
      this.#replace(id, undefined);
      return;
    }
    this.#replace(id, { ...row, refusal, status: "refused", stopping: false });
  }

  #find(id: number): PendingWrite | undefined {
    return this.#rows.find((row) => row.id === id);
  }

  /** Puts `row` in the place of the write `id`, or lets that write go, keeps what a reload must
   *  take up, and tells every listener. */
  #replace(id: number, row: PendingWrite | undefined): void {
    const before = this.#find(id);
    const others = this.#rows.filter((held) => held.id !== id);
    const rows = row === undefined ? others : [...others, row].sort((a, b) => a.id - b.id);
    this.#rows = rows.length === 0 ? NO_WRITES : rows;
    if (row === undefined) {
      if (before !== undefined) this.#kept.forget(before.key);
    } else {
      if (before !== undefined && before.key !== row.key) this.#kept.forget(before.key);
      this.#remember(row, row.key);
    }
    for (const listener of this.#listeners) listener();
  }

  /** Keeps the write `row` as it stands, for the person signed in; one with no request to make
   *  again is only counted for the leave prompt while it is out. */
  #remember(row: PendingWrite, key: string): void {
    const request = this.#running.get(row.id)?.request ?? null;
    const out = row.status !== "refused";
    if (request === null) {
      this.#kept.hold(key, out);
      return;
    }
    this.#kept.keep<KeptPendingWrite>(
      {
        detail: { label: row.label },
        key,
        kind: "write",
        pressedAt: row.pressedAt,
        refusal: !out && row.refusal !== undefined ? keptRefusal(row.refusal) : null,
        request,
        state: out ? (row.stopping ? "stopping" : row.status) : "refused",
      },
      signedInLogin(this.#client)
    );
  }
}

const stores = new WeakMap<QueryClient, PendingWrites>();

/** The pending writes that live with `client`: one queue per app, as there is one client. */
export function pendingWrites(client: QueryClient): PendingWrites {
  let store = stores.get(client);
  if (store === undefined) {
    store = new PendingWrites(client);
    stores.set(client, store);
  }
  return store;
}

export function usePendingWrites(): PendingWrites {
  return pendingWrites(useQueryClient());
}

/** Every write the queue holds, re-rendering when one changes. */
export function usePendingWriteRows(): readonly PendingWrite[] {
  const store = usePendingWrites();
  const subscribe = useCallback((listener: () => void) => store.subscribe(listener), [store]);
  return useSyncExternalStore(subscribe, store.rows);
}
