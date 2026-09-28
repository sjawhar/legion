import type { Agent } from "../../api/types";

export type MatchingSelectionState = "none" | "some" | "all";

export interface MatchingSelection {
  /** Selected sessions among the rows the filters match, folded sections included. */
  readonly selectedMatching: number;
  /** Selected sessions outside the filter: hidden by it, or no longer live. */
  readonly selectedOutside: number;
  /** The header checkbox's state, read from the matching rows alone. */
  readonly state: MatchingSelectionState;
}

/** How much of what the filters match is selected. A selected session outside the filter
 *  never makes the header mixed; it is counted apart, so nothing passes part of the selection
 *  off as all of it. */
export function matchingSelection(
  matching: readonly Agent[],
  selected: ReadonlySet<string>
): MatchingSelection {
  let selectedMatching = 0;
  for (const agent of matching) if (selected.has(agent.session_id)) selectedMatching += 1;
  const state =
    selectedMatching === 0 ? "none" : selectedMatching === matching.length ? "all" : "some";
  return { selectedMatching, selectedOutside: selected.size - selectedMatching, state };
}

/** The header's count: how many rows the filters match and how many of them are selected,
 *  then any selected sessions outside the filter, since the composer names and sends to those
 *  too. */
export function selectionSummary(matchingCount: number, selection: MatchingSelection): string {
  const { selectedMatching, selectedOutside } = selection;
  const matchingPart =
    selectedMatching === 0
      ? `${matchingCount} matching`
      : `${selectedMatching} of ${matchingCount} matching selected`;
  if (selectedOutside === 0) return matchingPart;
  const more = selectedMatching === 0 ? "" : "more ";
  return `${matchingPart} · ${selectedOutside} ${more}selected outside the filter`;
}

/** The header checkbox's click: select every matching session unless all of them already
 *  are, and otherwise unselect them. Selected sessions outside the filter stay selected either
 *  way. */
export function toggleMatching(
  matching: readonly Agent[],
  selected: ReadonlySet<string>
): Set<string> {
  const next = new Set(selected);
  const selectAll = matchingSelection(matching, selected).state !== "all";
  for (const agent of matching) {
    if (selectAll) next.add(agent.session_id);
    else next.delete(agent.session_id);
  }
  return next;
}
