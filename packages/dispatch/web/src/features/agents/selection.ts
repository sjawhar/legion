import type { Agent } from "../../api/types";

export type MatchingSelectionState = "none" | "some" | "all";

export interface MatchingSelection {
  /** Rows the filters match, folded sections included. */
  readonly matchingCount: number;
  /** Selected sessions among the rows the filters match, folded sections included. */
  readonly selectedMatching: number;
  /** Selected sessions still listed that an active filter hides. */
  readonly selectedHidden: number;
  /** Selected sessions gone from the list: the registry no longer has them. */
  readonly selectedGone: number;
  /** The header checkbox's state, read from the matching rows alone. */
  readonly state: MatchingSelectionState;
}

function countSelected(rows: readonly Agent[], selected: ReadonlySet<string>): number {
  let count = 0;
  for (const agent of rows) if (selected.has(agent.session_id)) count += 1;
  return count;
}

/** How much of what the filters match is selected, out of every listed session. A selected
 *  session the filters hide, or one gone from the list, never makes the header mixed; each is
 *  counted apart, so nothing passes part of the selection off as all of it. */
export function matchingSelection(
  listed: readonly Agent[],
  matching: readonly Agent[],
  selected: ReadonlySet<string>
): MatchingSelection {
  const selectedMatching = countSelected(matching, selected);
  const selectedListed = countSelected(listed, selected);
  const state =
    selectedMatching === 0 ? "none" : selectedMatching === matching.length ? "all" : "some";
  return {
    matchingCount: matching.length,
    selectedGone: selected.size - selectedListed,
    selectedHidden: selectedListed - selectedMatching,
    selectedMatching,
    state,
  };
}

/** The header's count: how many rows the filters match and how many of them are selected,
 *  then every other selected session, since the composer names and sends to those too - the
 *  ones an active filter hides apart from the ones gone from the list. */
export function selectionSummary(selection: MatchingSelection): string {
  const { matchingCount, selectedGone, selectedHidden, selectedMatching } = selection;
  const more = selectedMatching === 0 ? "" : "more ";
  const parts = [
    selectedMatching === 0
      ? `${matchingCount} matching`
      : `${selectedMatching} of ${matchingCount} matching selected`,
  ];
  if (selectedHidden > 0) parts.push(`${selectedHidden} ${more}selected outside the filter`);
  if (selectedGone > 0) parts.push(`${selectedGone} ${more}selected, no longer listed`);
  return parts.join(" · ");
}

/** A folded section's disclosure label: `<label> (N)`, and `<label> (N, K selected)` once any
 *  of its rows is selected, so the header checkbox never selects a row out of sight silently. */
export function foldLabel(
  label: string,
  rows: readonly Agent[],
  selected: ReadonlySet<string>
): string {
  const count = countSelected(rows, selected);
  return `${label} (${rows.length}${count === 0 ? "" : `, ${count} selected`})`;
}

/** The header checkbox's click: select every matching session unless all of them already
 *  are, and otherwise unselect them. Every other selected session stays selected either way. */
export function toggleMatching(
  matching: readonly Agent[],
  selected: ReadonlySet<string>
): Set<string> {
  const next = new Set(selected);
  const selectAll = countSelected(matching, selected) !== matching.length;
  for (const agent of matching) {
    if (selectAll) next.add(agent.session_id);
    else next.delete(agent.session_id);
  }
  return next;
}
