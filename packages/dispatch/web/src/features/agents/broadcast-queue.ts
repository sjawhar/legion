import { DELIVERY_CAPABILITIES } from "@legion/contracts";
import type { QueryClient } from "@tanstack/react-query";

import { api } from "../../api/client";
import { agentMessagesQuery } from "../../api/queries";
import type { BroadcastCreated, CreateBroadcastInput } from "../../api/types";
import {
  type KeyedSendStorage,
  keptRefusal,
  keyedSendStorage,
  refusalFrom,
  shape,
} from "../conversation/keyed-send-storage";
import { SEND_DEADLINE_MS, SendStoppedError, sendUnderKey } from "../conversation/send-request";

/** What one press of Send handed the queue: the request, and the selection it was pressed with,
 *  excluded sessions included, so Restore draft puts back the composer the human pressed. */
export interface BroadcastSend {
  readonly input: CreateBroadcastInput;
  readonly selected: readonly string[];
  /** Whether the sender picked the mode, rather than the page taking the selection's own; Restore
   *  draft puts that back too, so a restored default still follows the selection. */
  readonly picked: boolean;
}

/** A send as the queue holds it: `pressed` is its press's place in the strip, which Retry keeps,
 *  so a retried row stays where it was and nothing moves under the pointer. */
export interface QueuedSend extends BroadcastSend {
  readonly pressed: number;
}

/** One send as its row shows it: waiting its turn, on the wire within its deadline, still being
 *  tried under its key past it (`stopping` once the reader pressed Stop trying), sent with the
 *  broadcast it made, or failed with what refused it. */
export type BroadcastSendRow = {
  readonly id: number;
  readonly send: QueuedSend;
  readonly submittedAt: number;
} & (
  | { readonly status: "queued" }
  | { readonly status: "sending" }
  | { readonly status: "late"; readonly stopping: boolean }
  | { readonly created: BroadcastCreated; readonly status: "sent" }
  | { readonly error: Error; readonly status: "failed" }
);

/** A send as it is kept across a reload (`keyed-send-storage.ts`): its request, its place in the
 *  strip, and when it was pressed, which keeps a send pressed before the reload from opening its
 *  broadcast on a visit after it. */
interface KeptBroadcast {
  readonly send: QueuedSend;
  readonly submittedAt: number;
}

const keptBroadcast = shape.record({
  send: shape.record({
    input: shape.record({
      body: shape.string,
      delivery: shape.oneOf(...DELIVERY_CAPABILITIES),
      idempotency_key: shape.string,
      session_ids: shape.array(shape.string),
    }),
    picked: shape.boolean,
    pressed: shape.number,
    selected: shape.array(shape.string),
  }),
  submittedAt: shape.number,
});

/**
 * The page's broadcast sends, one per press, beside its `QueryClient` (`broadcastQueue`), so a send
 * outlives the Agents page and shows its outcome on the next visit. The requests go out one at a
 * time, in the order they were handed over: each waits for the one before it to be decided, either
 * way (`#line`), with the tab hidden or not. Each is tried under its key until Dispatch decides it
 * (`sendUnderKey`): a failed fetch, a timeout, a 5xx or a 429 is tried again, and the send is past
 * its deadline (`late`) once `SEND_DEADLINE_MS` passes without a verdict, when Stop trying ends
 * it. Every send not yet sent is kept across a reload, out or failed, and `resume` takes it up
 * again on load under its key, which Dispatch answers with the broadcast the first request made,
 * if it made one. A sent row stays until the page goes (`clearSent`); a failed one until Retry or
 * Restore draft takes it, since its row is the only copy of a message Send cleared from the
 * composer.
 */
export class BroadcastQueue {
  readonly #client: QueryClient;
  readonly #kept: KeyedSendStorage;
  /** Every send held, newest press first. */
  #rows: readonly BroadcastSendRow[] = [];
  readonly #listeners = new Set<() => void>();
  /** What stops each send still out, by its row's id (`stop`). */
  readonly #stops = new Map<number, AbortController>();
  /** The last send handed to the line, settled either way: each send's turn waits on it. */
  #line: Promise<void> = Promise.resolve();
  /** Row ids, in the order the sends joined the line. */
  #joined = 0;
  /** Presses this page load has made, so each new send takes the next place in the strip. */
  #presses = 0;
  #resumed = false;

  constructor(client: QueryClient, kept: KeyedSendStorage = keyedSendStorage()) {
    this.#client = client;
    this.#kept = kept;
  }

  /** Every send held, newest press first: one array until a send changes. */
  readonly rows = (): readonly BroadcastSendRow[] => this.#rows;

  readonly subscribe = (listener: () => void): (() => void) => {
    this.#listeners.add(listener);
    return () => this.#listeners.delete(listener);
  };

  /** Hands `send` to the line, behind every send already in it, at the next place in the strip. */
  enqueue(send: BroadcastSend): void {
    this.#presses += 1;
    this.#join({ ...send, pressed: this.#presses }, Date.now());
  }

  /** Sends a failed send's very request again, key included, at the back of the line and in a row
   *  of its own that keeps the failed row's place in the strip. */
  retry(row: BroadcastSendRow): void {
    if (this.#find(row.id)?.status !== "failed") return;
    this.#replace(row.id, undefined);
    this.#join(row.send, Date.now());
  }

  /** Takes a failed send off the strip, for Restore draft to hand its draft back. */
  dismiss(row: BroadcastSendRow): void {
    if (this.#find(row.id)?.status === "failed") this.#replace(row.id, undefined);
  }

  /** Stops trying the late send `row` (Stop trying): no attempt is made after the one in flight,
   *  whose answer is still the send's, and the send then fails with `SendStoppedError`, which a
   *  Retry sends again under the same key. Says whether it stopped one. */
  stop(row: BroadcastSendRow): boolean {
    const held = this.#find(row.id);
    if (held?.status !== "late" || held.stopping) return false;
    this.#stops.get(row.id)?.abort();
    this.#replace(row.id, { ...held, stopping: true });
    return true;
  }

  /** Lets every sent row go, as the page that listed them does. */
  clearSent(): void {
    for (const row of this.#rows) if (row.status === "sent") this.#replace(row.id, undefined);
  }

  /**
   * Takes up, once, the broadcasts a reload interrupted (`keyed-send-storage.ts`), in the order
   * they were pressed: one not yet sent joins the line again under its key, past its deadline
   * already if it was then, and a failure shows again with Retry and Restore draft, as does a send
   * the reader was stopping, whose last attempt went with the page. The app calls this once signed
   * in.
   */
  resume(): void {
    if (this.#resumed) return;
    this.#resumed = true;
    const kept = this.#kept
      .load("broadcast", (send, key) =>
        keptBroadcast(send) && (send as KeptBroadcast).send.input.idempotency_key === key
          ? (send as KeptBroadcast)
          : undefined
      )
      .sort((a, b) => a.send.send.pressed - b.send.send.pressed);
    for (const {
      refusal,
      send: { send, submittedAt },
      state,
    } of kept) {
      this.#presses = Math.max(this.#presses, send.pressed);
      if (state === "refused" || state === "stopping") {
        this.#joined += 1;
        this.#replace(this.#joined, {
          error: refusal === null ? new SendStoppedError() : refusalFrom(refusal),
          id: this.#joined,
          send,
          status: "failed",
          submittedAt,
        });
      } else {
        this.#join(send, submittedAt, state === "late");
      }
    }
  }

  /** Puts `send` at the back of the line; `late` takes its turn as a send already past its
   *  deadline. */
  #join(send: QueuedSend, submittedAt: number, late = false): void {
    this.#joined += 1;
    const id = this.#joined;
    const stop = new AbortController();
    this.#stops.set(id, stop);
    this.#replace(id, { id, send, status: "queued", submittedAt });
    // A turn never rejects; were one to, the line still goes on to the next.
    this.#line = this.#line.then(() => this.#send(id, stop.signal, late)).catch(() => undefined);
  }

  /** The send `id`'s turn on the line: tried under its key until Dispatch decides it. */
  async #send(id: number, signal: AbortSignal, late: boolean): Promise<void> {
    const queued = this.#find(id);
    if (queued === undefined) return;
    const { send, submittedAt } = queued;
    this.#replace(
      id,
      late
        ? { id, send, status: "late", stopping: false, submittedAt }
        : { id, send, status: "sending", submittedAt }
    );
    const deadline = setTimeout(() => {
      if (this.#find(id)?.status === "sending") {
        this.#replace(id, { id, send, status: "late", stopping: false, submittedAt });
      }
    }, SEND_DEADLINE_MS);
    try {
      const created = await sendUnderKey(
        (bounds) => api.createBroadcast(send.input, bounds),
        signal
      );
      this.#replace(id, { created, id, send, status: "sent", submittedAt });
      for (const recipient of created.recipients) {
        void this.#client.invalidateQueries({
          queryKey: agentMessagesQuery(recipient.session_id).queryKey,
        });
      }
      void this.#client.invalidateQueries({ queryKey: ["broadcast"] });
    } catch (error) {
      const failure = error instanceof Error ? error : new Error(String(error));
      this.#replace(id, { error: failure, id, send, status: "failed", submittedAt });
    } finally {
      clearTimeout(deadline);
      this.#stops.delete(id);
    }
  }

  #find(id: number): BroadcastSendRow | undefined {
    return this.#rows.find((row) => row.id === id);
  }

  /** Puts `row` in the place of the send `id`, or takes that send off the strip, keeps what a
   *  reload must take up, and tells every listener. */
  #replace(id: number, row: BroadcastSendRow | undefined): void {
    const before = this.#find(id);
    const others = this.#rows.filter((held) => held.id !== id);
    this.#rows = (row === undefined ? others : [...others, row]).sort(
      (a, b) => b.send.pressed - a.send.pressed
    );
    this.#remember(row, before?.send.input.idempotency_key);
    for (const listener of this.#listeners) listener();
  }

  /** Keeps the send under `key` as `row` shows it, or lets it go once sent or off the strip. */
  #remember(row: BroadcastSendRow | undefined, key: string | undefined): void {
    if (row === undefined || row.status === "sent") {
      if (key !== undefined) this.#kept.forget(key);
      return;
    }
    this.#kept.keep<KeptBroadcast>({
      key: row.send.input.idempotency_key,
      kind: "broadcast",
      refusal: row.status === "failed" ? keptRefusal(row.error) : null,
      send: { send: row.send, submittedAt: row.submittedAt },
      state:
        row.status === "failed"
          ? "refused"
          : row.status === "late" && row.stopping
            ? "stopping"
            : row.status,
    });
  }
}

const queues = new WeakMap<QueryClient, BroadcastQueue>();

/** The broadcast sends that live with `client`: one queue per app, as there is one client. */
export function broadcastQueue(client: QueryClient): BroadcastQueue {
  let queue = queues.get(client);
  if (queue === undefined) {
    queue = new BroadcastQueue(client);
    queues.set(client, queue);
  }
  return queue;
}
