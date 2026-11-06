import { useEffect, useState } from "react";

import type { AskBlockHost } from "./typed-block-drawing";

/**
 * The decision blocks of `hosts` whose card is to be drawn now: those already drawn, and one
 * more in each task until every host has its card.
 *
 * Every decision block of a document is a React card portalled into the shell its node view
 * draws (`AskBlockCard`, `ask-block.ts`), and the whole set of them arrives at once. The
 * progressive build creates every `ask` node view as its batch lands
 * (`@legion/proof-editor`'s `progressive-build.ts`), each one publishing the hosts again, and
 * the renders those publishes ask for are owed until something flushes them. React then drew
 * all of them in one task - the one task the host swaps the editor in with, because that swap
 * is a `flushSync` and a flush takes every render still owed. Measured on this pod, `iphone`
 * at 4x CPU throttling, the five decision blocks of the AHXV6-1-shaped document were 470 ms
 * of a 678 ms task, over the 500 ms a reader feels as a freeze (dispatch://INFRA-341, R11).
 * What costs that is each card's own first draw: its header, the ask card inside it and its
 * options.
 *
 * So one card is drawn in each task. A host is taken on a timer rather than in the effect
 * that sees it, because a render asked for inside the commit's own effects can be flushed
 * into the task that commit ran in; a timer is a task of its own, which the build lets
 * through between its batches - where these cards belong, since the reader is waiting for the
 * document, and a card drawn beside the batch that made its block is a card they can answer
 * before the build ends.
 *
 * Which hosts are drawn is held by key rather than as a count, so a decision block inserted
 * above the others pushes nobody's card off the screen: a reader half-way through answering
 * one keeps it. A host that leaves the document leaves this set with it.
 */
export function useAskCardsDrawnInTasks(hosts: readonly AskBlockHost[]): readonly AskBlockHost[] {
  const [drawn, setDrawn] = useState<ReadonlySet<number>>(() => new Set());
  const next = hosts.find((host) => !drawn.has(host.key));
  useEffect(() => {
    if (next === undefined) {
      return;
    }
    const timer = window.setTimeout(() => {
      setDrawn((already) => {
        const kept = new Set<number>();
        for (const host of hosts) {
          if (host === next || already.has(host.key)) {
            kept.add(host.key);
          }
        }
        return kept;
      });
    }, 0);
    return () => window.clearTimeout(timer);
  }, [hosts, next]);
  return hosts.filter((host) => drawn.has(host.key));
}
