import { DELIVERY_CAPABILITIES } from "@legion/contracts";
import type { QueryClient } from "@tanstack/react-query";

import { api } from "../../api/client";
import { agentMessagesQuery } from "../../api/queries";
import type { BroadcastCreated, CreateBroadcastInput } from "../../api/types";
import { type Shape, shape } from "../writes/write-storage";
import { defineWriteKind, ofKind, type Write, type Writes } from "../writes/write-store";

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

/** A broadcast as the write store holds it: its request, the selection it was pressed with, and
 *  its place in the strip (`pressed`), which a Retry keeps so nothing moves under the pointer. */
interface Broadcast {
  readonly input: CreateBroadcastInput;
  readonly selected: readonly string[];
  readonly picked: boolean;
  readonly pressed: number;
}

/** A send as its row shows it: `send` with its key, and its place in the strip. */
export interface QueuedSend extends BroadcastSend {
  readonly pressed: number;
}

/** One send as its row shows it: waiting its turn and never sent (`queued`, which the reader may
 *  discard), on the wire within its deadline (or sent before and waiting to go again), still being
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

const keptBroadcast = shape.record({
  input: shape.record({
    body: shape.string,
    delivery: shape.oneOf(...DELIVERY_CAPABILITIES),
    session_ids: shape.array(shape.string),
  }),
  picked: shape.boolean,
  pressed: shape.number,
  selected: shape.array(shape.string),
}) satisfies Shape<Broadcast>;

/** Reads again what a broadcast made: every recipient's conversation and the broadcasts. */
function refreshRecipients(client: QueryClient, sessions: readonly string[]): void {
  for (const session of sessions) {
    void client.invalidateQueries({ queryKey: agentMessagesQuery(session).queryKey });
  }
  void client.invalidateQueries({ queryKey: ["broadcast"] });
}

/**
 * A broadcast, one write of the store per press, kept across a reload with its request and the
 * selection it was pressed with. Every broadcast is ordered on one line (`broadcast`), so the
 * requests go out one at a time, in press order: a later one waits only while an earlier one is
 * still being tried, and a failed or stopped one no longer holds it. The request is all a send
 * orders: Dispatch answers as soon as it has made the broadcast, delivers behind that answer and
 * finishes every delivery itself, so two broadcasts can reach one agent in either order. A made
 * broadcast stays in the strip, sent, until the page lets it go (`clearSent`); a failed one until
 * Retry, Restore draft or - for one a reload interrupted too long ago - Discard takes it, since
 * its row is the only copy of a message Send cleared from the composer.
 */
const broadcastSend = defineWriteKind<Broadcast>({
  keepsMade: true,
  kept: keptBroadcast,
  landed: (client, write, answer) =>
    refreshRecipients(
      client,
      answer === null
        ? write.detail.input.session_ids
        : (answer as BroadcastCreated).recipients.map((recipient) => recipient.session_id)
    ),
  name: "broadcast",
  resume:
    ({ input }, wire) =>
    (bounds) =>
      wire === null ? api.createBroadcast(input, bounds) : api.send(wire, bounds),
});

/** Every broadcast `rows` holds, as the strip shows it, newest press first. */
export function broadcastRows(rows: readonly Write[]): BroadcastSendRow[] {
  const shown: BroadcastSendRow[] = [];
  for (const row of rows) {
    const write = ofKind(row, broadcastSend);
    if (write === undefined) continue;
    const { input, picked, pressed, selected } = write.detail;
    const base = {
      id: write.id,
      send: { input, key: write.key, picked, pressed, selected },
      submittedAt: write.pressedAt,
    };
    switch (write.status) {
      case "queued":
        shown.push({ ...base, status: write.sent ? "sending" : "queued" });
        break;
      case "sending":
        shown.push({ ...base, status: "sending" });
        break;
      case "late":
        shown.push({ ...base, status: "late", stopping: write.stopping });
        break;
      case "made":
        shown.push({ ...base, created: write.answer as BroadcastCreated, status: "sent" });
        break;
      case "refused":
        shown.push({ ...base, error: write.refusal, status: "failed" });
        break;
    }
  }
  return shown.sort((a, b) => b.send.pressed - a.send.pressed);
}

/** Hands `send` to the line, behind every broadcast already in it, at the next place in the
 *  strip. */
export function enqueueBroadcast(writes: Writes, send: BroadcastSend): void {
  const pressed = Math.max(0, ...broadcastRows(writes.rows()).map((row) => row.send.pressed)) + 1;
  writes
    .run({
      detail: { input: send.input, picked: send.picked, pressed, selected: send.selected },
      key: send.key,
      kind: broadcastSend,
      label: `Broadcast to ${send.input.session_ids.length}`,
      presentation: "composer",
      target: "broadcast",
      write: (bounds) => api.createBroadcast(send.input, bounds),
    })
    .catch(() => {});
}
