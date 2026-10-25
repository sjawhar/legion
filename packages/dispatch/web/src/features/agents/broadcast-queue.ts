import { DELIVERY_CAPABILITIES } from "@legion/contracts";
import type { QueryClient } from "@tanstack/react-query";

import { AnswerNotKeptError, api, type WriteRequest } from "../../api/client";
import { agentMessagesQuery } from "../../api/queries";
import type { BroadcastCreated, CreateBroadcastInput } from "../../api/types";
import { newSendKey } from "../../lib/send-key";
import {
  retriesUnderItsKey,
  SEND_DEADLINE_MS,
  SendInterruptedError,
  writeUnderKey,
} from "../writes/keyed-write";
import {
  keptRefusal,
  refusalFrom,
  resumesOnItsOwn,
  shape,
  signedInLogin,
  type WriteStorage,
  writeStorage,
} from "../writes/write-storage";

/** What one press of Send handed the queue: the request, the key it goes under, and the selection
 *  it was pressed with, excluded sessions included, so Restore draft puts back the composer the
 *  human pressed. */
export interface BroadcastSend {
  readonly input: CreateBroadcastInput;
  readonly key: string;
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

/** A send as it is kept across a reload (`write-storage.ts`), beside its request, key and press
 *  time - which keeps a send pressed before the reload from opening its broadcast on a visit after
 *  it: its request, the selection it was pressed with, and its place in the strip. */
interface KeptBroadcast {
  readonly send: QueuedSend;
}

const keptBroadcast = shape.record({
  send: shape.record({
    input: shape.record({
      body: shape.string,
      delivery: shape.oneOf(...DELIVERY_CAPABILITIES),
      session_ids: shape.array(shape.string),
    }),
    key: shape.string,
    picked: shape.boolean,
    pressed: shape.number,
    selected: shape.array(shape.string),
  }),
});

/**
 * The page's broadcast sends, one per press, beside its `QueryClient` (`broadcastQueue`), so a send
 * outlives the Agents page and shows its outcome on the next visit. The requests go out one at a
 * time, in the order they were handed over: each waits for the one before it to be decided, either
 * way (`#line`), with the tab hidden or not. Each is tried under its key until Dispatch decides it
 * (`writeUnderKey`): a failed fetch, a timeout, a 5xx, a 429 or a `409 KEY_IN_FLIGHT` is tried
 * again, as is a 2xx whose broadcast still has a recipient whose one attempt is pending - Dispatch
 * delivers behind its answer, and a repeat under the key resumes a recipient a dead sender left
 * owed - and the send is past its deadline (`late`) once `SEND_DEADLINE_MS` passes without a
 * verdict, when Stop trying ends it. Every send not yet sent is kept in the page's write queue
 * (`write-storage.ts`), out or failed, for the person who pressed it, and `resume` takes it up
 * again on load under its key, which Dispatch answers with the broadcast the first request made,
 * if it made one. A sent row stays until the page goes (`clearSent`); a failed one until Retry,
 * Restore draft or - for one a reload interrupted too long ago - Discard takes it, since its row is
 * the only copy of a message Send cleared from the composer.
 */
export class BroadcastQueue {
  readonly #client: QueryClient;
  readonly #kept: WriteStorage;
  /** Every send held, newest press first. */
  #rows: readonly BroadcastSendRow[] = [];
  readonly #listeners = new Set<() => void>();
  /** What stops each send still out, by its row's id (`stop`). */
  readonly #stops = new Map<number, AbortController>();
  /** The request each send last went out as, by its row's id: what a reload makes again. */
  readonly #wires = new Map<number, WriteRequest>();
  /** The last send handed to the line, settled either way: each send's turn waits on it. */
  #line: Promise<void> = Promise.resolve();
  /** Row ids, in the order the sends joined the line. */
  #joined = 0;
  /** Presses this page load has made, so each new send takes the next place in the strip. */
  #presses = 0;
  #resumed = false;

  constructor(client: QueryClient, kept: WriteStorage = writeStorage()) {
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

  /** Sends a failed send again, at the back of the line and in a row of its own that keeps the
   *  failed row's place in the strip: its very request, key included, when it ended without
   *  Dispatch's verdict, and under a new key when Dispatch refused it, since Dispatch answers that
   *  key with its refusal (`retriesUnderItsKey`). */
  retry(row: BroadcastSendRow): void {
    const failed = this.#find(row.id);
    if (failed?.status !== "failed") return;
    const underItsKey = retriesUnderItsKey(failed.error);
    const wire = underItsKey ? (this.#wires.get(row.id) ?? null) : null;
    this.#replace(row.id, undefined);
    this.#join(
      underItsKey ? failed.send : { ...failed.send, key: newSendKey() },
      Date.now(),
      "sending",
      wire
    );
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
   * Takes up, once, the broadcasts a reload interrupted that `login`, the person signed in,
   * pressed (`write-storage.ts`), in the order they were pressed; another person's are dropped
   * unsent. One not yet sent and pressed within `KEPT_SEND_RESUME_MS` joins the line again under
   * its key, as the request it last went out as, past its deadline already if it was then; one the
   * reader was stopping, whose last attempt went with the page, is asked once more under its key,
   * and is sent if Dispatch takes it, or ends stopped. One pressed longer ago is not sent on its
   * own: it shows failed (`SendInterruptedError`), with Retry under its key, Restore draft and
   * Discard. A failure shows again with Retry and Restore draft. The app calls this once signed in.
   */
  resume(login: string): void {
    if (this.#resumed) return;
    this.#resumed = true;
    const kept = this.#kept
      .load("broadcast", login, (detail, key) =>
        keptBroadcast(detail) && (detail as KeptBroadcast).send.key === key
          ? (detail as KeptBroadcast)
          : undefined
      )
      .sort((a, b) => a.detail.send.pressed - b.detail.send.pressed);
    const now = Date.now();
    for (const {
      detail: { send },
      pressedAt,
      refusal,
      request,
      state,
    } of kept) {
      this.#presses = Math.max(this.#presses, send.pressed);
      if (state !== "refused" && resumesOnItsOwn(pressedAt, now)) {
        this.#join(send, pressedAt, state === "queued" ? "sending" : state, request);
        continue;
      }
      this.#joined += 1;
      if (request !== null) this.#wires.set(this.#joined, request);
      this.#replace(this.#joined, {
        error:
          state === "refused" && refusal !== null
            ? refusalFrom(refusal)
            : new SendInterruptedError(),
        id: this.#joined,
        send,
        status: "failed",
        submittedAt: pressedAt,
      });
    }
  }

  /** Puts `send` at the back of the line, to take its turn within its deadline (`sending`), as a
   *  send already past it (`late`), or as one past it the reader has stopped (`stopping`), which
   *  makes one attempt and no other; `wire`, the request a reload kept, goes out verbatim. */
  #join(
    send: QueuedSend,
    submittedAt: number,
    from: "late" | "sending" | "stopping" = "sending",
    wire: WriteRequest | null = null
  ): void {
    this.#joined += 1;
    const id = this.#joined;
    const stop = new AbortController();
    if (from === "stopping") stop.abort();
    this.#stops.set(id, stop);
    if (wire !== null) this.#wires.set(id, wire);
    this.#replace(id, { id, send, status: "queued", submittedAt });
    // A turn never rejects; were one to, the line still goes on to the next.
    this.#line = this.#line.then(() => this.#send(id, stop.signal, from)).catch(() => undefined);
  }

  /** The send `id`'s turn on the line: tried under its key until Dispatch decides it. */
  async #send(
    id: number,
    signal: AbortSignal,
    from: "late" | "sending" | "stopping"
  ): Promise<void> {
    const queued = this.#find(id);
    if (queued === undefined) return;
    const { send, submittedAt } = queued;
    this.#replace(
      id,
      from === "sending"
        ? { id, send, status: "sending", submittedAt }
        : { id, send, status: "late", stopping: from === "stopping", submittedAt }
    );
    const deadline = setTimeout(() => {
      if (this.#find(id)?.status === "sending") {
        this.#replace(id, { id, send, status: "late", stopping: false, submittedAt });
      }
    }, SEND_DEADLINE_MS);
    const wire = this.#wires.get(id);
    try {
      // The exclusions are a fact about the send that Dispatch states once, in the answer that
      // made the broadcast: it stores none, so a repeat under the key names every excluded
      // session only as "excluded by the original send". The row keeps the first answer's,
      // where Dispatch said why, and the latest answer's recipients, which carry their delivery
      // as it stands.
      let excluded: BroadcastCreated["excluded"] | undefined;
      const answer = await writeUnderKey(
        async (bounds) => {
          const made =
            wire === undefined
              ? await api.createBroadcast(send.input, bounds)
              : await api.send<BroadcastCreated>(wire, bounds);
          excluded ??= made.excluded;
          return made;
        },
        send.key,
        {
          // Dispatch answers before it delivers, so a recipient whose one attempt is pending is
          // not yet the outcome: a repeat under the key resumes it if its sender died, and leaves
          // it to a live one. A recipient with more than one attempt was retried from its page,
          // which Dispatch never resumes, so that answer is as it is.
          decided: ({ recipients }) =>
            recipients.every(
              ({
                message: {
                  deliveries: [only, ...more],
                },
              }) => only?.state !== "pending" || more.length > 0
            ),
          sent: (request) => {
            this.#wires.set(id, request);
            const row = this.#find(id);
            if (row !== undefined) this.#remember(row, row.send.key);
          },
          signal,
        }
      );
      const created = { ...answer, excluded: excluded ?? answer.excluded };
      this.#replace(id, { created, id, send, status: "sent", submittedAt });
      for (const recipient of created.recipients) {
        void this.#client.invalidateQueries({
          queryKey: agentMessagesQuery(recipient.session_id).queryKey,
        });
      }
      void this.#client.invalidateQueries({ queryKey: ["broadcast"] });
    } catch (error) {
      if (error instanceof AnswerNotKeptError) {
        // Dispatch made the broadcast and has no answer to give for it: it leaves the strip, with
        // no refusal, and every recipient's conversation and the broadcasts are read again.
        this.#replace(id, undefined);
        for (const session of send.input.session_ids) {
          void this.#client.invalidateQueries({ queryKey: agentMessagesQuery(session).queryKey });
        }
        void this.#client.invalidateQueries({ queryKey: ["broadcast"] });
        return;
      }
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
    if (row === undefined) this.#wires.delete(id);
    this.#remember(row, before?.send.key);
    for (const listener of this.#listeners) listener();
  }

  /** Keeps the send under `key` as `row` shows it, for the person signed in, with the request it
   *  last went out as, or lets it go once sent or off the strip. */
  #remember(row: BroadcastSendRow | undefined, key: string | undefined): void {
    if (row === undefined || row.status === "sent") {
      if (key !== undefined) this.#kept.forget(key);
      return;
    }
    this.#kept.keep<KeptBroadcast>(
      {
        detail: { send: row.send },
        key: row.send.key,
        kind: "broadcast",
        pressedAt: row.submittedAt,
        refusal: row.status === "failed" ? keptRefusal(row.error) : null,
        request: this.#wires.get(row.id) ?? null,
        state:
          row.status === "failed"
            ? "refused"
            : row.status === "late" && row.stopping
              ? "stopping"
              : row.status,
      },
      signedInLogin(this.#client)
    );
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
