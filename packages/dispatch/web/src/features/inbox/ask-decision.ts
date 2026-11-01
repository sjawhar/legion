/**
 * What makes a question a decision block's.
 *
 * The server writes `block_id` for an ask that came from a typed block and `block_artifact` for
 * the document that block sits in, and a question is a decision only with both: a `block_id`
 * without the document names a block nothing can open, and a row awaiting the backfill carries
 * `null`. Three surfaces read that test - the short row's kind badge (`Inbox.tsx`), the reading
 * column's subject (`ReadingPane.tsx`) and the card's "Open in document" link
 * (`AskBlockLink.tsx`) - and each wants the block and the document after it, so it is one type
 * predicate rather than the same three comparisons written three ways.
 */

import type { Ask, AskBlockArtifact } from "../../api/types";

/** What the test reads off a question: the ask's own fields, not a copy of their shapes. */
export type BlockAsk = Pick<Ask, "block_id" | "block_artifact">;

/** The block a decision question was written as, and the document holding it. */
export interface AskDecisionBlock {
  block_id: string;
  block_artifact: AskBlockArtifact;
}

/** Whether `ask` is a question written as a decision block of a document. */
export function isDecisionAsk<T extends BlockAsk>(ask: T): ask is T & AskDecisionBlock {
  return ask.block_id !== undefined && ask.block_id !== null && ask.block_artifact !== undefined;
}
