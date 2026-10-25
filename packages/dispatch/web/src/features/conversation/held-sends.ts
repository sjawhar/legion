import { DELIVERY_CAPABILITIES } from "@legion/contracts";
import { ASK_URGENCIES } from "@legion/contracts/dispatch-tools";
import {
  hashKey,
  type MutationKey,
  partialMatchKey,
  type QueryClient,
  useQueryClient,
} from "@tanstack/react-query";
import {
  type Dispatch,
  type SetStateAction,
  useCallback,
  useState,
  useSyncExternalStore,
} from "react";

import { AnswerNotKeptError, api, type WriteRequest } from "../../api/client";
import { agentMessagesQuery } from "../../api/queries";
import { pastDeadlineKey } from "../../hooks/useSending";
import { SendInterruptedError } from "../writes/keyed-write";
import {
  keptRefusal,
  refusalFrom,
  resumesOnItsOwn,
  shape,
  signedInLogin,
  type WriteStorage,
  writeStorage,
} from "../writes/write-storage";
import type { ReplyTarget } from "./composer-model";
import {
  SendDeadlineError,
  type SentDraft,
  type SentRequest,
  sendRequest,
  sendWithinDeadline,
} from "./send-request";

/** A held send's draft as its composer restores it: the text, its accepted mentions and a
 *  suggestion's replacement, and an ask's fields. */
export interface HeldDraft {
  readonly ask: SentRequest["ask"];
  readonly draft: SentDraft;
}

/**
 * A composer's send that has not landed: out within its deadline (`sending`), out past it
 * (`late`), or refused and not yet sent again or discarded. It belongs to the composer's target,
 * named by the composer's send name, not to the composer: whatever unmounts the composer, the
 * entry stays, and a composer that mounts under the same name shows it.
 */
export interface HeldSend {
  /** The send's name, which names its target: an issue's Conversation, a thread, an agent
   *  session, a margin compose's document. One held send per name. */
  readonly mutationKey: MutationKey;
  /** The request the draft is addressed by: the one sent, or, for a margin compose whose kind
   *  the reader switched after a refusal, the same request on its retyped mark. */
  readonly request: SentRequest;
  /** The draft as its composer last held it: the one sent, and, once refused, whatever the
   *  reader wrote over it before the composer unmounted. */
  readonly draft: HeldDraft;
  readonly status: "late" | "refused" | "sending";
  /** The server's refusal, once it has refused; `SendStoppedError` once the reader stopped it. */
  readonly refusal: Error | undefined;
  /** Whether the reader pressed Stop trying (`stop`) and the attempt in flight is still out. */
  readonly stopping: boolean;
  /** When Send or Retry was pressed for it (`Date.now()`), which a reload keeps: a send kept for
   *  longer than `KEPT_SEND_RESUME_MS` is not made again on its own. */
  readonly pressedAt: number;
}

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

const NO_HELD_SENDS: readonly HeldSend[] = Object.freeze([]);

/** A send as it is kept across a reload (`write-storage.ts`), beside the request it went out as:
 *  its name, its request - an edit's by the comment it saves - and the draft its composer holds. */
interface KeptHeldSend {
  readonly draft: HeldDraft;
  readonly mutationKey: readonly string[];
  readonly request: Omit<SentRequest, "edit"> & { readonly edit: string | null };
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
});

const keptDelivery = shape.oneOf(...DELIVERY_CAPABILITIES);

const keptHeldSend = shape.record({
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
});

/** The request a kept send names, whole again: an edit saves its comment as the composer's own
 *  default save does, since the host that gave the page its save is gone with the page. */
function heldRequest({ edit, ...request }: KeptHeldSend["request"]): SentRequest {
  return {
    ...request,
    edit:
      edit === null
        ? undefined
        : { id: edit, save: (id, body, bounds) => api.editComment(id, { body }, bounds) },
  };
}

/**
 * The app's held sends, one per send name, beside its `QueryClient` (`heldSends`): above the
 * router, so no route change, collapse or unmount below it drops one. An entry leaves when its
 * send lands or the reader discards its refusal; nothing else removes it. Every send goes under
 * its key (`sendRequest`), so each is kept across a reload (`write-storage.ts`), out or refused,
 * for the person who pressed it, with the request it last went out as, and `resume` takes it up
 * again on load. Each send also runs as a TanStack mutation under its name - and under
 * `pastDeadlineKey` once past its deadline - so a host's hold on the name (`useSending`) counts
 * it as before. That mutation runs whether or not the browser says it is online (`networkMode:
 * "always"`): the send's own tries are what wait out a network that is down, and a mutation
 * TanStack paused offline would make none, so the send would never go late and never offer Stop
 * trying.
 */
export class HeldSends {
  readonly #client: QueryClient;
  readonly #kept: WriteStorage;
  readonly #entries = new Map<string, HeldSend>();
  /** The request each send last went out as, under its key, by its name's hash: what a reload
   *  makes again. */
  readonly #wires = new Map<string, { readonly key: string; readonly request: WriteRequest }>();
  /** `under`'s answers that hold a send, by the prefix's hash, each dropped when a send beneath
   *  its prefix changes. */
  readonly #under = new Map<
    string,
    { readonly prefix: MutationKey; readonly sends: readonly HeldSend[] }
  >();
  readonly #listeners = new Set<() => void>();
  readonly #outcomes = new Set<(outcome: HeldSendOutcome) => void>();
  /** What stops each send still out, by its name's hash (`stop`). */
  readonly #stops = new Map<string, AbortController>();
  #resumed = false;

  constructor(client: QueryClient, kept: WriteStorage = writeStorage()) {
    this.#client = client;
    this.#kept = kept;
  }

  get(mutationKey: MutationKey): HeldSend | undefined {
    return this.#entries.get(hashKey(mutationKey));
  }

  /** Every held send whose name starts with `prefix`, as one array that changes only when one of
   *  those sends does, whatever else the store holds. */
  under(prefix: MutationKey): readonly HeldSend[] {
    const prefixId = hashKey(prefix);
    const cached = this.#under.get(prefixId);
    if (cached !== undefined) return cached.sends;
    const sends = [...this.#entries.values()].filter((held) =>
      partialMatchKey(held.mutationKey, prefix)
    );
    // An empty answer is the one shared array, so a prefix nothing is sent under holds no entry.
    if (sends.length === 0) return NO_HELD_SENDS;
    this.#under.set(prefixId, { prefix, sends });
    return sends;
  }

  /** Whether a send under `mutationKey` is out now - from the task that starts it on. */
  sendingNow(mutationKey: MutationKey): boolean {
    const held = this.get(mutationKey);
    return held !== undefined && held.status !== "refused";
  }

  subscribe(listener: () => void): () => void {
    this.#listeners.add(listener);
    return () => this.#listeners.delete(listener);
  }

  /** Tells `listener` what becomes of every held send from now on. */
  onOutcome(listener: (outcome: HeldSendOutcome) => void): () => void {
    this.#outcomes.add(listener);
    return () => this.#outcomes.delete(listener);
  }

  /** Sends `request` under `mutationKey` and holds it until it lands - unless a send under that
   *  name is out already, which refuses this one. A refused send's entry gives way to the new
   *  one. */
  send(mutationKey: MutationKey, request: SentRequest): boolean {
    if (this.sendingNow(mutationKey)) return false;
    this.#start(mutationKey, request, Date.now(), "sending");
    return true;
  }

  /**
   * Takes up, once, the sends a reload interrupted that `login`, the person signed in, pressed
   * (`write-storage.ts`); another person's are dropped unsent. One still out and pressed within
   * `KEPT_SEND_RESUME_MS` is made again under its key, as the request it last went out as, past
   * its deadline already if it was then: Dispatch answers the repeat with what the first request
   * did, if it did anything. One the reader was stopping, whose last attempt went with the page,
   * is asked once more under its key, and lands if Dispatch takes it, or ends stopped. One pressed
   * longer ago is not made again on its own: it shows refused (`SendInterruptedError`), for a
   * Retry under its key or Discard. A refusal shows again with the draft written over it. The app
   * calls this once signed in.
   */
  resume(login: string): void {
    if (this.#resumed) return;
    this.#resumed = true;
    const kept = this.#kept.load("composer", login, (detail, key) =>
      keptHeldSend(detail) && (detail as KeptHeldSend).request.sendKey === key
        ? (detail as KeptHeldSend)
        : undefined
    );
    const now = Date.now();
    for (const { detail, pressedAt, refusal, request: wire, state } of kept) {
      const { draft, mutationKey } = detail;
      const request = heldRequest(detail.request);
      const id = hashKey(mutationKey);
      if (this.#entries.has(id)) continue;
      if (wire !== null) this.#wires.set(id, { key: request.sendKey, request: wire });
      if (state !== "refused" && state !== "queued" && resumesOnItsOwn(pressedAt, now)) {
        this.#start(mutationKey, request, pressedAt, state);
        continue;
      }
      this.#set(id, {
        draft,
        mutationKey,
        pressedAt,
        refusal:
          state === "refused" && refusal !== null
            ? refusalFrom(refusal)
            : new SendInterruptedError(),
        request,
        status: "refused",
        stopping: false,
      });
    }
  }

  /** Holds `request`, pressed at `pressedAt`, under `mutationKey` as out, and sends it: within its
   *  deadline (`sending`), as a send already past it (`late`), or as one past it the reader has
   *  stopped (`stopping`), which makes one attempt and no other. A send under the key it last went
   *  out under goes as that request again (`#wires`), so Dispatch sees the same write. */
  #start(
    mutationKey: MutationKey,
    request: SentRequest,
    pressedAt: number,
    from: "late" | "sending" | "stopping"
  ): void {
    const id = hashKey(mutationKey);
    const stop = new AbortController();
    if (from === "stopping") stop.abort();
    this.#stops.set(id, stop);
    const kept = this.#wires.get(id);
    if (kept !== undefined && kept.key !== request.sendKey) this.#wires.delete(id);
    const options = {
      sent: (wire: WriteRequest) => {
        if (wire.body instanceof FormData || this.#stops.get(id) !== stop) return;
        this.#wires.set(id, { key: request.sendKey, request: wire });
        const held = this.#entries.get(id);
        if (held !== undefined) this.#remember(held, held);
      },
      signal: stop.signal,
      wire: kept?.key === request.sendKey ? kept.request : undefined,
    };
    this.#set(id, {
      draft: { ask: request.ask, draft: request.draft },
      mutationKey,
      pressedAt,
      refusal: undefined,
      request,
      status: from === "sending" ? "sending" : "late",
      stopping: from === "stopping",
    });
    const landed = () => {
      this.#stops.delete(id);
      const held = this.#entries.get(id);
      this.#set(id, undefined);
      refreshAfter(this.#client, request);
      if (held !== undefined) this.#emit({ kind: "landed", send: held });
    };
    const refused = (error: Error) => {
      this.#stops.delete(id);
      // A write that ran but whose answer Dispatch did not keep landed; what it wrote is read
      // again (`refreshAfter`).
      if (error instanceof AnswerNotKeptError) {
        landed();
        return;
      }
      const held = this.#update(id, { refusal: error, status: "refused", stopping: false });
      if (held !== undefined) this.#emit({ kind: "refused", send: held });
    };
    if (from !== "sending") {
      this.#run(pastDeadlineKey(mutationKey), () => sendRequest(request, options), {
        onError: refused,
        onSuccess: landed,
      });
      return;
    }
    this.#run(mutationKey, () => sendWithinDeadline(request, options), {
      onError: (error) => {
        // Past its deadline the request is not refused: it goes on under `pastDeadlineKey`,
        // started before the first mutation settles, so a hold on the name never drops between
        // them, and its answer is the send's outcome.
        if (error instanceof SendDeadlineError) {
          this.#update(id, { status: "late" });
          this.#run(pastDeadlineKey(mutationKey), () => error.answer, {
            onError: refused,
            onSuccess: landed,
          });
          return;
        }
        refused(error);
      },
      onSuccess: landed,
    });
  }

  /** Stops trying the late send under `mutationKey` (Stop trying): no attempt is made after the
   *  one in flight, whose answer is still the send's, and the send is then refused with
   *  `SendStoppedError`, which a Retry sends again under the same key. Says whether it stopped
   *  one. */
  stop(mutationKey: MutationKey): boolean {
    const id = hashKey(mutationKey);
    if (!stoppable(this.#entries.get(id))) return false;
    this.#stops.get(id)?.abort();
    this.#update(id, { stopping: true });
    return true;
  }

  /** Drops the refused send under `mutationKey`, its draft and refusal with it, and says whether
   *  there was one. A send still out is not the reader's to drop: it stays until the server
   *  answers. */
  discard(mutationKey: MutationKey): boolean {
    const id = hashKey(mutationKey);
    const held = this.#entries.get(id);
    if (held?.status !== "refused") return false;
    this.#set(id, undefined);
    this.#emit({ kind: "discarded", send: held });
    return true;
  }

  /** Keeps what the reader wrote over a refusal, as its composer unmounts. */
  keep(mutationKey: MutationKey, draft: HeldDraft): void {
    const id = hashKey(mutationKey);
    if (this.#entries.get(id)?.status === "refused") this.#update(id, { draft });
  }

  /** Re-addresses the refused send under `mutationKey`: a margin compose's kind switch after a
   *  refusal retypes its mark, and its Retry sends to the retyped one. */
  readdress(mutationKey: MutationKey, request: SentRequest): void {
    const id = hashKey(mutationKey);
    if (this.#entries.get(id)?.status === "refused") this.#update(id, { request });
  }

  #run(
    mutationKey: MutationKey,
    mutationFn: () => Promise<unknown>,
    callbacks: { onError: (error: Error) => void; onSuccess: () => void }
  ): void {
    // A mutation of the cache's own, with no observer, so it leaves the cache once settled; run
    // offline too, since the send's own tries wait out the network (the class comment).
    this.#client
      .getMutationCache()
      .build(this.#client, { mutationFn, mutationKey, networkMode: "always", ...callbacks })
      .execute(undefined)
      .catch(() => {});
  }

  #update(id: string, change: Partial<HeldSend>): HeldSend | undefined {
    const held = this.#entries.get(id);
    if (held === undefined) return undefined;
    const next = { ...held, ...change };
    this.#set(id, next);
    return next;
  }

  #set(id: string, held: HeldSend | undefined): void {
    const before = this.#entries.get(id);
    const changed = held ?? before;
    if (held === undefined) {
      this.#entries.delete(id);
      this.#wires.delete(id);
    } else {
      this.#entries.set(id, held);
    }
    if (changed !== undefined) {
      for (const [prefixId, { prefix }] of this.#under) {
        if (partialMatchKey(changed.mutationKey, prefix)) this.#under.delete(prefixId);
      }
    }
    this.#remember(before, held);
    for (const listener of this.#listeners) listener();
  }

  /** Keeps a send across a reload as it changes, for the person signed in, with the request it
   *  last went out as, and lets it go once it leaves. */
  #remember(before: HeldSend | undefined, held: HeldSend | undefined): void {
    if (before !== undefined && before.request.sendKey !== held?.request.sendKey) {
      this.#kept.forget(before.request.sendKey);
    }
    if (held === undefined) return;
    const { edit, ...request } = held.request;
    const wire = this.#wires.get(hashKey(held.mutationKey));
    this.#kept.keep<KeptHeldSend>(
      {
        detail: {
          draft: held.draft,
          mutationKey: held.mutationKey.map(String),
          request: { ...request, edit: edit?.id ?? null },
        },
        key: request.sendKey,
        kind: "composer",
        pressedAt: held.pressedAt,
        refusal:
          held.status === "refused" && held.refusal !== undefined
            ? keptRefusal(held.refusal)
            : null,
        request: wire?.key === request.sendKey ? wire.request : null,
        state: held.status === "refused" ? "refused" : held.stopping ? "stopping" : held.status,
      },
      signedInLogin(this.#client)
    );
  }

  #emit(outcome: HeldSendOutcome): void {
    for (const listener of this.#outcomes) listener(outcome);
  }
}

/** The reply target a composer named `mutationKey` must show for a Reply tap on `target`: the
 *  comment or message a refusal is already held there, when one is and it names a different id,
 *  so the reader's Reply elsewhere redirects onto it instead of silently overwriting its draft -
 *  the docked composer's and the phone thread composer's `beginReply` both read this, since
 *  `composerKey` and `sendKey` are both plain `MutationKey`s read the same way; `target` itself
 *  otherwise. The caller's own `sendingNow` guard must run first and refuse the tap while a send
 *  under `mutationKey` is out: an entry this reads can then only be `"refused"` or absent, never
 *  `"sending"`/`"late"`. */
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

const stores = new WeakMap<QueryClient, HeldSends>();

/** The held sends that live with `client`: one store per app, as there is one client. */
export function heldSends(client: QueryClient): HeldSends {
  let store = stores.get(client);
  if (store === undefined) {
    store = new HeldSends(client);
    stores.set(client, store);
  }
  return store;
}

export function useHeldSends(): HeldSends {
  return heldSends(useQueryClient());
}

/** The held send under `mutationKey`, re-rendering when it changes. */
export function useHeldSend(mutationKey: MutationKey | undefined): HeldSend | undefined {
  const store = useHeldSends();
  const subscribe = useCallback((listener: () => void) => store.subscribe(listener), [store]);
  return useSyncExternalStore(subscribe, () =>
    mutationKey === undefined ? undefined : store.get(mutationKey)
  );
}

/** Every held send whose name starts with `prefix`, re-rendering only when one of them changes. */
export function useHeldSendsUnder(prefix: MutationKey): readonly HeldSend[] {
  const store = useHeldSends();
  const subscribe = useCallback((listener: () => void) => store.subscribe(listener), [store]);
  return useSyncExternalStore(subscribe, () => store.under(prefix));
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
