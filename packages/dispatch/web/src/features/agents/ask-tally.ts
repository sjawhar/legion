import type { InboxRow } from "../../api/types";

/**
 * The one walk over an inbox read that counts a session's open asks, in a module of its own
 * rather than on the Agents page: the machine pages count the same asks per pane and per row, and
 * importing them from `AgentsPage.tsx` would pull that whole page - its composer, its broadcast
 * queue, its keymap - into every machine page's chunk.
 */

/** Open asks from each session whose turn is the viewer's, keyed by session ID. */
export type NeedsYouBySession = ReadonlyMap<string, number>;

/** Open asks a session authored, tallied by that session's id: the walk both card numbers make
 *  over the one inbox query, each over the rows it counts (`keep`, every row by default). */
export function tallyBySession(
  asks: readonly InboxRow[],
  keep: (ask: InboxRow) => boolean = () => true
): Map<string, number> {
  const counts = new Map<string, number>();
  for (const ask of asks) {
    if (ask.author.kind !== "session" || !keep(ask)) continue;
    counts.set(ask.author.id, (counts.get(ask.author.id) ?? 0) + 1);
  }
  return counts;
}
