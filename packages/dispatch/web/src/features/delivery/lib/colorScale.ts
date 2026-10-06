// Merge colours, shared by the Timeline's dots and the PR list's leading dots so one PR reads the
// same colour in both views. Pure: no React, no ECharts. Unlike the prototype this ports from,
// `priority` and `component` are not colour-by facets here — `DeliveryPR` carries no joined issue
// priority/components (see `facets.ts`'s module comment) — so `ColorFacet` keeps only the fields
// the response actually carries.
import type { DeliveryPR } from "../../../api/types";
import { NO_SESSION, PLACEHOLDER_LABELS, parentAgentPlaceholder } from "./facets";

export type ColorFacet = "repo" | "author" | "parentAgent";

export const COLOR_PALETTE = [
  "#38bdf8",
  "#f472b6",
  "#a3e635",
  "#fb923c",
  "#c084fc",
  "#facc15",
  "#2dd4bf",
  "#f87171",
  "#818cf8",
  "#4ade80",
];

// Placeholder values ("No issue", "No session", ...) recede in neutral grey.
export const PLACEHOLDER_COLOR = "#64748b";

/** The value of `colorBy` a PR is coloured (and laned) by. */
export function colorKeyFor(pr: DeliveryPR, colorBy: ColorFacet): string {
  switch (colorBy) {
    case "repo":
      return pr.repo;
    case "author":
      return pr.author;
    case "parentAgent":
      return pr.parent_agent ?? parentAgentPlaceholder(pr);
    default:
      return NO_SESSION;
  }
}

/** colorKeyFor() value -> colour. Built from the whole (window) PR list, largest group first, so
 *  a value keeps its colour when facets or the window change which PRs are shown. Placeholder
 *  values are grey and don't use up a palette slot. */
export function buildColorScale(
  prs: readonly DeliveryPR[],
  colorBy: ColorFacet
): Map<string, string> {
  const counts = new Map<string, number>();
  for (const pr of prs) {
    const key = colorKeyFor(pr, colorBy);
    counts.set(key, (counts.get(key) ?? 0) + 1);
  }
  const scale = new Map<string, string>();
  let next = 0;
  for (const [key] of Array.from(counts.entries()).sort((a, b) => b[1] - a[1])) {
    if (key in PLACEHOLDER_LABELS) {
      scale.set(key, PLACEHOLDER_COLOR);
      continue;
    }
    scale.set(key, COLOR_PALETTE[next % COLOR_PALETTE.length] ?? "#38bdf8");
    next += 1;
  }
  return scale;
}
