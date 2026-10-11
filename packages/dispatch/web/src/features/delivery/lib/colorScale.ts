// Merge colours, shared by the Timeline's dots and the PR list's leading dots so one PR reads the
// same colour in both views. Pure: no React, no ECharts. The chart draws on a canvas, so these are
// colour values rather than Tailwind classes.
import type { DeliveryColorFacet, DeliveryPR } from "../../../api/types";
import { NO_COMPONENT, NO_ISSUE, NO_PRIORITY, NO_SESSION, PLACEHOLDER_LABELS } from "./facets";

export type ColorFacet = "repo" | "author" | "priority" | "component" | "parentAgent";

/** The "Color merges by" choices, in the prototype's order. */
export const COLOR_FACET_OPTIONS: { value: ColorFacet; label: string }[] = [
  { value: "repo", label: "Repository" },
  { value: "author", label: "Author" },
  { value: "priority", label: "Priority" },
  { value: "component", label: "Component" },
  { value: "parentAgent", label: "Parent agent" },
];

/** Each colour-by facet's `color_counts` key. */
export const COLOR_COUNT_KEYS: Record<ColorFacet, DeliveryColorFacet> = {
  repo: "repo",
  author: "author",
  priority: "priority",
  component: "component",
  parentAgent: "parent_agent",
};

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

/** Placeholder values ("No issue", "No session", ...) recede in neutral grey. */
export const PLACEHOLDER_COLOR = "#64748b";

/** The values left out when some are chosen to have their own colour share this one. It sits
 *  outside the palette and apart from the placeholders' grey. */
export const OTHER_COLOR = "#a8a29e";

/** The value of `colorBy` a PR is coloured (and, with swimlanes on, laned) by: the first of its
 *  values, as the server's `color_counts` counts it. */
export function colorKeyFor(pr: DeliveryPR, colorBy: ColorFacet): string {
  switch (colorBy) {
    case "repo":
      return pr.repo;
    case "author":
      return pr.author;
    case "parentAgent":
      return pr.parent_agent ?? NO_SESSION;
    case "priority":
      if (pr.issue === null) return NO_ISSUE;
      return pr.priority ?? NO_PRIORITY;
    case "component":
      if (pr.issue === null) return NO_ISSUE;
      return pr.components[0] ?? NO_COMPONENT;
  }
}

/** colorKeyFor() value -> colour, from the window's `color_counts` for the facet (nothing
 *  applied), largest group first, so a value keeps its colour when facets or a brush window change
 *  which PRs are shown. Placeholder values are grey and don't use up a palette slot. With values
 *  `chosen`, those take the palette in the order chosen and every other value shares
 *  OTHER_COLOR, so the chosen ones stand out however many values there are. */
export function buildColorScale(
  counts: Readonly<Record<string, number>>,
  chosen: readonly string[] = []
): Map<string, string> {
  const scale = new Map<string, string>();
  const ranked = Object.keys(counts).sort((a, b) => (counts[b] ?? 0) - (counts[a] ?? 0));
  const order = chosen.length === 0 ? ranked : [...new Set(chosen)];
  let next = 0;
  for (const key of order) {
    if (key in PLACEHOLDER_LABELS) continue;
    scale.set(key, COLOR_PALETTE[next % COLOR_PALETTE.length] ?? "#38bdf8");
    next += 1;
  }
  for (const key of ranked) {
    if (key in PLACEHOLDER_LABELS) scale.set(key, PLACEHOLDER_COLOR);
    else if (!scale.has(key)) scale.set(key, OTHER_COLOR);
  }
  return scale;
}
