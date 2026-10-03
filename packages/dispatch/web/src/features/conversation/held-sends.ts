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

import { agentMessagesQuery } from "../../api/queries";
import { pastDeadlineKey } from "../../hooks/useSending";
import type { ReplyTarget } from "./composer-model";
import {
  SendDeadlineError,
  type SentDraft,
  type SentRequest,
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
  /** The server's refusal, once it has refused. */
  readonly refusal: Error | undefined;
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
  } else {
    void client.invalidateQueries({ queryKey: ["artifact", owner.artifactId] });
    void client.invalidateQueries({ queryKey: ["project", owner.project, "artifacts"] });
  }
}

/**
 * The app's held sends, one per send name, beside its `QueryClient` (`heldSends`): above the
 * router, so no route change, collapse or unmount below it drops one. An entry leaves when its
 * send lands or the reader discards its refusal; nothing else removes it, and a reload loses
 * every one, as it loses the page. Each send also runs as a TanStack mutation under its name -
 * and under `pastDeadlineKey` once past its deadline - so a host's hold on the name
 * (`useSending`) counts it as before.
 */
export class HeldSends {
  readonly #client: QueryClient;
  readonly #entries = new Map<string, HeldSend>();
  /** `under`'s answers, by the prefix's hash, each dropped when a send beneath its prefix
   *  changes. */
  readonly #under = new Map<
    string,
    { readonly prefix: MutationKey; readonly sends: readonly HeldSend[] }
  >();
  readonly #listeners = new Set<() => void>();
  readonly #outcomes = new Set<(outcome: HeldSendOutcome) => void>();

  constructor(client: QueryClient) {
    this.#client = client;
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
    const id = hashKey(mutationKey);
    if (this.sendingNow(mutationKey)) return false;
    this.#set(id, {
      draft: { ask: request.ask, draft: request.draft },
      mutationKey,
      refusal: undefined,
      request,
      status: "sending",
    });
    const landed = () => {
      const held = this.#entries.get(id);
      this.#set(id, undefined);
      refreshAfter(this.#client, request);
      if (held !== undefined) this.#emit({ kind: "landed", send: held });
    };
    const refused = (error: Error) => {
      const held = this.#update(id, { refusal: error, status: "refused" });
      if (held !== undefined) this.#emit({ kind: "refused", send: held });
    };
    this.#run(mutationKey, () => sendWithinDeadline(request), {
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
    return true;
  }

  /** Drops the refused send under `mutationKey`, its draft and refusal with it. A send still out
   *  is not the reader's to drop: it stays until the server answers. */
  discard(mutationKey: MutationKey): void {
    const id = hashKey(mutationKey);
    const held = this.#entries.get(id);
    if (held?.status !== "refused") return;
    this.#set(id, undefined);
    this.#emit({ kind: "discarded", send: held });
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
    // A mutation of the cache's own, with no observer, so it leaves the cache once settled.
    this.#client
      .getMutationCache()
      .build(this.#client, { mutationFn, mutationKey, ...callbacks })
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
    const changed = held ?? this.#entries.get(id);
    if (held === undefined) this.#entries.delete(id);
    else this.#entries.set(id, held);
    if (changed !== undefined) {
      for (const [prefixId, { prefix }] of this.#under) {
        if (partialMatchKey(changed.mutationKey, prefix)) this.#under.delete(prefixId);
      }
    }
    for (const listener of this.#listeners) listener();
  }

  #emit(outcome: HeldSendOutcome): void {
    for (const listener of this.#outcomes) listener(outcome);
  }
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
