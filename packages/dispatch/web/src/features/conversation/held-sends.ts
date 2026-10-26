import { DELIVERY_CAPABILITIES } from "@legion/contracts";
import { ASK_URGENCIES } from "@legion/contracts/dispatch-tools";
import {
  hashKey,
  type MutationKey,
  partialMatchKey,
  type QueryClient,
  useQueryClient,
} from "@tanstack/react-query";
import { type Dispatch, type SetStateAction, useEffect, useRef, useState } from "react";

import { api } from "../../api/client";
import { agentMessagesQuery } from "../../api/queries";
import { pastDeadlineKey } from "../../hooks/useSending";
import { SEND_DEADLINE_MS } from "../writes/keyed-write";
import { type Shape, shape } from "../writes/write-storage";
import {
  defineWriteKind,
  ofKind,
  useWriteSelection,
  type Write,
  type Writes,
  writesOf,
} from "../writes/write-store";
import type { ReplyTarget } from "./composer-model";
import { type SentDraft, type SentRequest, sendAttempt } from "./send-request";

/** A held send's draft as its composer restores it: the text, its accepted mentions and a
 *  suggestion's replacement, and an ask's fields. */
export interface HeldDraft {
  readonly ask: SentRequest["ask"];
  readonly draft: SentDraft;
}

/** A composer's send as the write store holds it (`composerSend`): its name, which names its
 *  target - an issue's Conversation, a thread, an agent session, a margin compose's document - the
 *  request it went out as, and the draft its composer last held: the one sent, and, once refused,
 *  whatever the reader wrote over it before the composer unmounted. */
export interface ComposerSend {
  readonly mutationKey: readonly string[];
  readonly request: SentRequest;
  readonly draft: HeldDraft;
}

/** A composer's send as it is kept across a reload: an edit by the comment it saves, since the
 *  host that gave the page its save is gone with the page. */
interface KeptComposerSend {
  readonly draft: HeldDraft;
  readonly mutationKey: readonly string[];
  readonly request: Omit<SentRequest, "edit"> & { readonly edit: string | null };
}

/**
 * A composer's send that has not landed, as its composers read it: waiting its turn behind an
 * earlier send to its session, never sent (`waiting`, which the reader may discard), out
 * (`sending`, which counts one sent before that waits to go again), out past its deadline
 * (`late`), or refused and not yet sent again or discarded. It belongs to the composer's target,
 * named by the composer's send name, not to the composer: whatever unmounts the composer, it
 * stays, and a composer that mounts under the same name shows it.
 */
export type HeldSend = {
  readonly id: number;
  readonly mutationKey: readonly string[];
  readonly request: SentRequest;
  readonly draft: HeldDraft;
  /** When Send or Retry was pressed for it, which a reload keeps: a send kept for longer than
   *  `KEPT_SEND_RESUME_MS` is not made again on its own. */
  readonly pressedAt: number;
} & (
  | { readonly status: "waiting" }
  | { readonly status: "sending" }
  | { readonly status: "late"; readonly stopping: boolean }
  | { readonly status: "refused"; readonly refusal: Error }
);

/** Whether `held` is a late send the reader may stop: it is being tried again under its key until
 *  Dispatch decides it, which a lasting outage can hold off for good. */
export function stoppable(held: HeldSend | undefined): boolean {
  return held?.status === "late" && !held.stopping;
}

/** What became of a held send: it landed, the server refused it, or the reader dropped its
 *  refusal. */
export interface HeldSendOutcome {
  readonly kind: "discarded" | "landed" | "refused";
  readonly send: HeldSend;
}

/** What a send that landed wrote, refreshed whether or not its composer is still on screen: the
 *  caches it is named by its own request. */
function refreshAfter(client: QueryClient, { anchor, owner }: SentRequest): void {
  if (owner.kind === "session") {
    void client.invalidateQueries({ queryKey: agentMessagesQuery(owner.sessionId).queryKey });
    return;
  }
  void client.invalidateQueries({
    queryKey:
      owner.kind === "issue"
        ? ["comments", owner.issueKey]
        : ["artifact", owner.artifactId, "comments"],
  });
  void client.invalidateQueries({ queryKey: ["inbox"] });
  if (anchor !== undefined) {
    void client.invalidateQueries({ queryKey: ["artifact", anchor.artifact, "blocks"] });
  }
  if (owner.kind === "issue") {
    void client.invalidateQueries({ queryKey: ["events", owner.issueKey] });
    void client.invalidateQueries({ queryKey: ["issue", owner.issueKey] });
    // The Conversation lists the issue's messages itself, so a message the reader just wrote is
    // a new card in that list; a comment is one already, through the issue's comments above.
    void client.invalidateQueries({ queryKey: ["issue-messages", owner.issueKey] });
  } else {
    void client.invalidateQueries({ queryKey: ["artifact", owner.artifactId] });
    void client.invalidateQueries({ queryKey: ["project", owner.project, "artifacts"] });
  }
}

const keptAsk = shape.record({
  multiple: shape.boolean,
  options: shape.array(
    shape.record({ description: shape.optional(shape.string), label: shape.string })
  ),
  urgency: shape.oneOf(...ASK_URGENCIES),
});

const keptDraft = shape.record({
  body: shape.string,
  mentions: shape.array(
    shape.record({
      end: shape.number,
      start: shape.number,
      target: shape.string,
      text: shape.string,
    })
  ),
  replacement: shape.string,
}) satisfies Shape<SentDraft>;

const keptDelivery = shape.oneOf(...DELIVERY_CAPABILITIES);

const keptComposerSend = shape.record({
  draft: shape.record({ ask: keptAsk, draft: keptDraft }),
  mutationKey: shape.array(shape.string),
  request: shape.record({
    anchor: shape.optional(
      shape.record({ artifact: shape.string, mark_id: shape.string, quote: shape.string })
    ),
    ask: keptAsk,
    draft: keptDraft,
    edit: shape.nullable(shape.string),
    kind: shape.oneOf("ask", "comment", "suggestion"),
    owner: shape.either(
      shape.record({ issueKey: shape.string, kind: shape.oneOf("issue") }),
      shape.record({
        artifactId: shape.string,
        kind: shape.oneOf("artifact"),
        project: shape.string,
      }),
      shape.record({ kind: shape.oneOf("session"), sessionId: shape.string })
    ),
    pickedDelivery: shape.optional(keptDelivery),
    plainDelivery: keptDelivery,
    replyTo: shape.nullable(
      shape.record({
        author: shape.string,
        excerpt: shape.string,
        id: shape.string,
        mentions: shape.optional(
          shape.array(shape.record({ target: shape.string, title: shape.string }))
        ),
        parentKind: shape.oneOf("comment", "message"),
        thread: shape.optional(
          shape.record({ delivery: keptDelivery, target: shape.string, title: shape.string })
        ),
        to: shape.optional(shape.string),
      })
    ),
    sendKey: shape.string,
  }),
}) satisfies Shape<KeptComposerSend>;

/** A `MutationKey` of a composer's: a send name is built of strings alone, so it is kept as it is
 *  and read back the same. */
function sendName(mutationKey: MutationKey): readonly string[] {
  return mutationKey.map((part) => (typeof part === "string" ? part : JSON.stringify(part)));
}

/** Holds `mutationKey` in TanStack's mutation cache while `answer` is out, so a host's hold on the
 *  name (`useSending`) counts a send from the task that starts it on, whether or not its composer
 *  is still mounted. Run whether or not the browser says it is online (`networkMode: "always"`):
 *  a mutation TanStack paused offline would hold nothing. */
function hold(client: QueryClient, mutationKey: MutationKey, answer: () => Promise<unknown>): void {
  client
    .getMutationCache()
    .build(client, { mutationFn: answer, mutationKey, networkMode: "always" })
    .execute(undefined)
    .catch(() => {});
}

/**
 * A composer's send, one write of the store per press (`Writes`), kept across a reload with its
 * name, its request and its draft. A send to one session is ordered on that session
 * (`session:<id>`), so two presses reach it in press order, each once. A send that lands
 * refreshes what it wrote whether or not a composer is mounted (`refreshAfter`). Each send also
 * holds its name in TanStack's mutation cache - under `pastDeadlineKey` once past its deadline -
 * so a host's hold on the name (`useSending`) counts it; its `untilDeadline` hold lets go at the
 * deadline.
 */
export const composerSend = defineWriteKind<ComposerSend, KeptComposerSend>({
  keep: ({ draft, mutationKey, request: { edit, ...request } }) => ({
    draft,
    mutationKey,
    request: { ...request, edit: edit?.id ?? null },
  }),
  kept: keptComposerSend,
  landed: (client, write) => refreshAfter(client, write.detail.request),
  name: "composer",
  restore: ({ draft, mutationKey, request: { edit, ...request } }) => ({
    draft,
    mutationKey,
    request: {
      ...request,
      // The host that gave the page its save is gone with it: an edit saves as the composer's
      // own default save does.
      edit:
        edit === null
          ? undefined
          : { id: edit, save: (id, body, bounds) => api.editComment(id, { body }, bounds) },
    },
  }),
  resume: ({ request }, wire) =>
    wire === null ? sendAttempt(request) : (bounds) => api.send(wire, bounds),
  started: (client, write, settled, late) => {
    const name = write.detail.mutationKey;
    if (late) {
      hold(client, pastDeadlineKey(name), () => settled);
      return;
    }
    // Past its deadline the request is not refused: it goes on under `pastDeadlineKey`, held
    // before the name lets go, so a hold on the name never drops between them.
    hold(client, name, () => {
      const { promise, reject, resolve } = Promise.withResolvers<unknown>();
      const deadline = setTimeout(() => {
        hold(client, pastDeadlineKey(name), () => settled);
        resolve(undefined);
      }, SEND_DEADLINE_MS);
      settled.then(resolve, reject).finally(() => clearTimeout(deadline));
      return promise;
    });
  },
});

const views = new WeakMap<Write, HeldSend>();

/** `write`, a composer's send, as its composers read it: one value while the write stands. */
function heldSend(write: Write<ComposerSend>): HeldSend | undefined {
  let held = views.get(write);
  if (held !== undefined) return held;
  const base = {
    draft: write.detail.draft,
    id: write.id,
    mutationKey: write.detail.mutationKey,
    pressedAt: write.pressedAt,
    request: write.detail.request,
  };
  switch (write.status) {
    case "made":
      return undefined;
    case "queued":
      held = write.sent ? { ...base, status: "sending" } : { ...base, status: "waiting" };
      break;
    case "sending":
      held = { ...base, status: "sending" };
      break;
    case "late":
      held = { ...base, status: "late", stopping: write.stopping };
      break;
    case "refused":
      held = { ...base, refusal: write.refusal, status: "refused" };
      break;
  }
  views.set(write, held);
  return held;
}

/** Every composer's send `rows` holds, as its composers read them. */
function heldIn(rows: readonly Write[]): HeldSend[] {
  const held: HeldSend[] = [];
  for (const row of rows) {
    const write = ofKind(row, composerSend);
    const view = write === undefined ? undefined : heldSend(write);
    if (view !== undefined) held.push(view);
  }
  return held;
}

/** The send `rows` holds under `mutationKey`, if any. */
function heldUnder(rows: readonly Write[], mutationKey: MutationKey): HeldSend | undefined {
  const name = hashKey(sendName(mutationKey));
  return heldIn(rows)
    .filter((held) => hashKey(held.mutationKey) === name)
    .at(-1);
}

/**
 * The app's held sends, as its composers read and make them, over the one write store
 * (`writesOf`): one send per composer's name, which a composer that mounts under that name shows,
 * out or refused, whatever unmounted the one that made it.
 */
export class HeldSends {
  readonly #writes: Writes;

  constructor(writes: Writes) {
    this.#writes = writes;
  }

  /** Calls `listener` whenever any write the store holds changes. */
  subscribe(listener: () => void): () => void {
    return this.#writes.subscribe(listener);
  }

  get(mutationKey: MutationKey): HeldSend | undefined {
    return heldUnder(this.#writes.rows(), mutationKey);
  }

  /** Whether a send under `mutationKey` is out now - from the task that starts it on. */
  sendingNow(mutationKey: MutationKey): boolean {
    const held = this.get(mutationKey);
    return held !== undefined && held.status !== "refused";
  }

  /** Tells `listener` what becomes of every held send from now on. */
  onOutcome(listener: (outcome: HeldSendOutcome) => void): () => void {
    return this.#writes.onOutcome((outcome) => {
      const write = ofKind(outcome.write, composerSend);
      const send = write === undefined ? undefined : heldSend(write);
      if (send !== undefined) listener({ kind: outcome.kind, send });
    });
  }

  /** Sends `request` under `mutationKey` and holds it until it lands - unless a send under that
   *  name is out already, which refuses this one: a composer holds one draft. A refused send's
   *  entry gives way to the new one. */
  send(mutationKey: MutationKey, request: SentRequest): boolean {
    const held = this.get(mutationKey);
    if (held !== undefined && held.status !== "refused") return false;
    if (held !== undefined) this.#writes.discard(held.id);
    const { owner } = request;
    this.#writes
      .run({
        detail: {
          draft: { ask: request.ask, draft: request.draft },
          mutationKey: sendName(mutationKey),
          request,
        },
        key: request.sendKey,
        kind: composerSend,
        label: "Message",
        presentation: "composer",
        target:
          owner.kind === "session" && request.edit === undefined
            ? `session:${owner.sessionId}`
            : undefined,
        write: sendAttempt(request),
      })
      .catch(() => {});
    return true;
  }

  /** Stops trying the late send under `mutationKey` (Stop trying). Says whether it stopped one. */
  stop(mutationKey: MutationKey): boolean {
    const held = this.get(mutationKey);
    return held !== undefined && this.#writes.stop(held.id);
  }

  /** Drops the refused send under `mutationKey`, its draft and refusal with it, and says whether
   *  there was one. */
  discard(mutationKey: MutationKey): boolean {
    const held = this.get(mutationKey);
    return held !== undefined && this.#writes.discard(held.id);
  }

  /** Keeps what the reader wrote over a refusal, as its composer unmounts. */
  keep(mutationKey: MutationKey, draft: HeldDraft): void {
    const held = this.get(mutationKey);
    if (held?.status === "refused") {
      this.#writes.amend(held.id, composerSend, (detail) => ({ ...detail, draft }));
    }
  }

  /** Re-addresses the refused send under `mutationKey`: a margin compose's kind switch after a
   *  refusal retypes its mark, and its Retry sends to the retyped one. */
  readdress(mutationKey: MutationKey, request: SentRequest): void {
    const held = this.get(mutationKey);
    if (held?.status === "refused") {
      this.#writes.amend(held.id, composerSend, (detail) => ({ ...detail, request }));
    }
  }
}

const facades = new WeakMap<Writes, HeldSends>();

/** The held sends that live with `client`: its write store's composer sends. */
export function heldSends(client: QueryClient): HeldSends {
  const writes = writesOf(client);
  let facade = facades.get(writes);
  if (facade === undefined) {
    facade = new HeldSends(writes);
    facades.set(writes, facade);
  }
  return facade;
}

export function useHeldSends(): HeldSends {
  return heldSends(useQueryClient());
}

/** The held send under `mutationKey`, re-rendering when it changes. */
export function useHeldSend(mutationKey: MutationKey | undefined): HeldSend | undefined {
  return useWriteSelection(mutationKey === undefined ? "" : hashKey(mutationKey), (rows) =>
    mutationKey === undefined ? undefined : heldUnder(rows, mutationKey)
  );
}

/** Every held send whose name starts with `prefix`, re-rendering only when one of them changes. */
export function useHeldSendsUnder(prefix: MutationKey): readonly HeldSend[] {
  return useWriteSelection(hashKey(prefix), (rows) =>
    heldIn(rows).filter((held) => partialMatchKey(held.mutationKey, prefix))
  );
}

/** Every held send to session `sessionId`, from any of its composers - the live view's presses,
 *  its Agents row's - in press order, re-rendering only when one of them changes. */
export function useHeldSendsTo(sessionId: string): readonly HeldSend[] {
  return useWriteSelection(`session:${sessionId}`, (rows) =>
    heldIn(rows).filter(
      ({ request: { edit, owner } }) =>
        owner.kind === "session" && owner.sessionId === sessionId && edit === undefined
    )
  );
}

/** Calls `listener` each time a held send lands or is refused while the caller is mounted. */
export function useHeldSendOutcomes(listener: (outcome: HeldSendOutcome) => void): void {
  const store = useHeldSends();
  const latest = useRef(listener);
  latest.current = listener;
  useEffect(() => store.onOutcome((outcome) => latest.current(outcome)), [store]);
}

/** The reply target a composer named `mutationKey` must show for a Reply tap on `target`: the
 *  comment or message a refusal is already held there, when one is and it names a different id,
 *  so the reader's Reply elsewhere redirects onto it instead of silently overwriting its draft -
 *  the docked composer's and the phone thread composer's `beginReply` both read this. The caller's
 *  own `sendingNow` guard must run first and refuse the tap while a send under `mutationKey` is
 *  out: an entry this reads can then only be refused or absent. */
export function redirectedReplyTarget(
  store: HeldSends,
  mutationKey: MutationKey,
  target: ReplyTarget
): ReplyTarget {
  const heldReply = store.get(mutationKey)?.request.replyTo;
  return heldReply !== undefined && heldReply !== null && heldReply.id !== target.id
    ? heldReply
    : target;
}

/** A host's reply for its composer under `mutationKey`, as state: it starts as the reply the send
 *  the store holds there - out, or refused - was sent with, so a host that mounts again shows that
 *  send under the reply it answers, and as none otherwise. */
export function useHeldReplyTo(
  mutationKey: MutationKey
): [ReplyTarget | null, Dispatch<SetStateAction<ReplyTarget | null>>] {
  const store = useHeldSends();
  return useState<ReplyTarget | null>(() => store.get(mutationKey)?.request.replyTo ?? null);
}
