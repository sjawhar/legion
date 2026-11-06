import type { ReactNode } from "react";

import { GLYPH_NAMES } from "./page-words";

/**
 * The shape the page draws beside a node's type and beside its state (dispatch://INFRA-1196 R21):
 * an inline `<svg>` carrying its `GLYPH_TOKENS` token in `data-glyph` and its own accessible
 * name from `GLYPH_NAMES`, drawn beside the word rather than in place of it, so a type and a
 * state are told apart by a word and a shape and never by colour alone.
 *
 * Every shape is one `path` in a 16 by 16 box, and no two tokens draw the same one: a reader who
 * tells the shapes apart tells the nodes apart with the colours forced off.
 */
const GLYPH_PATHS: Readonly<Record<string, string>> = {
  /** The types: one outline each. */
  ask: "M8 1 L15 8 L8 15 L1 8 Z",
  callout: "M2 3 h12 v8 h-5 l-3 4 v-4 h-4 Z",
  check: "M2 8 l4 5 l8 -11",
  "child-issue": "M3 2 v8 a3 3 0 0 0 3 3 h7 M10 10 l3 3 l-3 3",
  comment: "M2 3 h12 v8 h-7 l-5 4 Z",
  decision: "M8 1 L15 5 v6 L8 15 L1 11 V5 Z",
  document: "M3 1 h7 l3 3 v11 h-10 Z",
  handoff: "M1 8 h10 M8 4 l4 4 l-4 4 M14 2 v12",
  image: "M2 3 h12 v10 h-12 Z M2 11 l4 -4 l3 3 l2 -2 l3 3",
  issue: "M8 1 a7 7 0 1 0 0.01 0 Z",
  message: "M1 4 h14 v8 h-14 Z M1 4 l7 5 l7 -5",
  "plan-task": "M2 2 h12 v12 h-12 Z M5 8 h6",
  requirement: "M3 1 h10 v14 l-5 -4 l-5 4 Z",
  result: "M8 2 l2 4 l4 1 l-3 3 l1 4 l-4 -2 l-4 2 l1 -4 l-3 -3 l4 -1 Z",
  /** An ask's three states. */
  "ask-answered": "M2 9 l4 4 l8 -10",
  "ask-open": "M8 2 a6 6 0 1 0 0.01 0 Z M8 6 v3 M8 11.5 v0.5",
  "ask-resolved": "M3 3 l10 10 M13 3 l-10 10",
  /** A check's five states. */
  "check-failed": "M4 4 l8 8 M12 4 l-8 8",
  "check-no-result": "M2 8 h12",
  "check-older": "M8 2 a6 6 0 1 0 0.01 0 Z M8 5 v3 l2 2",
  "check-passed": "M2 9 l4 4 l8 -10",
  "check-unmarked": "M8 2 a6 6 0 1 0 0.01 0 Z M5 8 h6",
  /** A decision's three states. */
  "decision-replaced": "M2 11 h12 M4 4 l8 4 l-8 4 Z",
  "decision-waiting-review": "M1 8 a7 4 0 0 0 14 0 a7 4 0 0 0 -14 0 Z M8 6 v4",
  "decision-yours": "M8 2 a3 3 0 1 0 0.01 0 Z M2 15 a6 6 0 0 1 12 0",
};

/** One shape, by its token: `svg[data-glyph]` with the accessible name the token carries. */
export function NodeGlyph({ className, token }: { className?: string; token: string }): ReactNode {
  return (
    <svg
      aria-label={GLYPH_NAMES[token]}
      className={className ?? "h-4 w-4 shrink-0"}
      data-glyph={token}
      fill="none"
      role="img"
      stroke="currentColor"
      strokeWidth="1.5"
      viewBox="0 0 16 16"
    >
      <path d={GLYPH_PATHS[token] ?? "M2 8 h12"} />
    </svg>
  );
}
