import { hashKey, type MutationKey } from "@tanstack/react-query";
import { useEffect, useId, useMemo, useRef } from "react";

import { type HeldDraft, type HeldSend, useHeldSend, useHeldSends } from "./held-sends";
import type { SentRequest } from "./send-request";

/** A composer's send, read and written through the held-send store (`held-sends.ts`). */
export interface ComposerSend {
  /** The send its target holds - out, or refused - which a composer mounting now shows. */
  readonly held: HeldSend | undefined;
  /** Builds the request and sends it, unless a send under the composer's name is out; `build`
   *  runs only when it goes. */
  readonly send: (build: () => SentRequest) => void;
  /** Whether a send is out, past its deadline included, as of the last render. */
  readonly sending: boolean;
  /** Whether the send is past its deadline and still waiting for its answer. */
  readonly late: boolean;
  /** What the server refused, for the reader: a deadline is not a refusal, and the answer that
   *  follows one is. */
  readonly refusal: Error | undefined;
  /** Whether a send is out now, from Send's own task on, before React has rendered it. */
  readonly sendingNow: () => boolean;
  /** Drops a refusal, its draft with it. */
  readonly discard: () => void;
  /** Keeps what the reader wrote over a refusal, for the composer that mounts next. */
  readonly keep: (draft: HeldDraft) => void;
}

/**
 * The send behind a composer, held by its target rather than by the composer: `mutationKey` names
 * both, and a composer with none gets a name of its own mount. `landed` and `refused` are the
 * composer's own part of an outcome - its draft, its host's callbacks - and run only while it is
 * mounted; the store refreshes what a landed send wrote either way.
 */
export function useComposerSend(
  mutationKey: MutationKey | undefined,
  {
    landed,
    refused,
  }: {
    readonly landed: (sent: SentRequest) => void;
    /** A refusal hands back exactly the draft the request turned down, not a later edit. */
    readonly refused: (held: HeldSend) => void;
  }
): ComposerSend {
  const mount = useId();
  const key = useMemo<MutationKey>(() => mutationKey ?? ["composer", mount], [mount, mutationKey]);
  const store = useHeldSends();
  const held = useHeldSend(key);
  const outcomes = useRef({ landed, refused });
  outcomes.current = { landed, refused };
  const id = useMemo(() => hashKey(key), [key]);
  useEffect(
    () =>
      store.onOutcome(({ kind, send }) => {
        if (hashKey(send.mutationKey) !== id) return;
        if (kind === "landed") outcomes.current.landed(send.request);
        if (kind === "refused") outcomes.current.refused(send);
      }),
    [id, store]
  );
  return {
    discard: () => store.discard(key),
    held,
    keep: (draft) => store.keep(key, draft),
    late: held?.status === "late",
    refusal: held?.status === "refused" ? held.refusal : undefined,
    send: (build) => {
      if (store.sendingNow(key)) return;
      store.send(key, build());
    },
    sending: held !== undefined && held.status !== "refused",
    sendingNow: () => store.sendingNow(key),
  };
}
