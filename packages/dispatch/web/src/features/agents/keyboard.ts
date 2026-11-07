/**
 * The `agents` scope: `j`/`k` over the session rows, and the four keys that act on the row in
 * hand. The four find their control through a `data-agent-*` handle on the row, so the page's
 * markup is the only contract between them; `j` and `k` walk the page's own list of sessions
 * instead (`AgentListNavigation`), because the list is windowed.
 */
import type { RefObject } from "react";

import { useKeymap, useKeymapScope } from "../shell/keymap";
import { closestMatching, focusedMatching } from "../shell/roving";

/** The page renders these handles and this module acts on them; one spelling of each, so a
 *  reader who moves one knows where it is read. */
const AGENT_ROW_ATTRIBUTE = "data-agent-row";
export const AGENT_ROW_SELECTOR = `[${AGENT_ROW_ATTRIBUTE}]`;
const AGENT_COMPOSER_SELECTOR = "[data-agent-composer]";
export const ISSUE_PICKER_SELECTOR = "[data-agent-issue-picker]";
const ISSUE_SELECT_SELECTOR = "[data-agent-issue-select]";
const AGENT_FOLD_ATTRIBUTE = "data-agent-fold";
/** A fold's toggle: the one item of the list that is not a row, sitting between the rows. */
const AGENT_FOLD_SELECTOR = `[${AGENT_FOLD_ATTRIBUTE}]`;
/** The windowed list itself, which holds every drawn row and every fold toggle. */
const AGENT_LIST_SELECTOR = "[data-agents-list]";

/**
 * An item of the page's list a reader reaches with `j` and `k`: a session's row, or a group's
 * fold toggle, which is where a pin into a closed group leaves focus.
 */
export type AgentListItem =
  | { readonly kind: "row"; readonly id: string }
  | { readonly kind: "fold"; readonly group: string };

/**
 * The page's list as the keys walk it. The walk is over this rather than over the rows in the
 * DOM: the list is windowed (`features/delivery/lib/rowWindow.ts`), so the row `j` wants next is
 * often not drawn, and a walk over `[data-agent-row]` would stop dead at the bottom of the drawn
 * range. The page draws, scrolls to and focuses whichever row this asks for.
 */
export interface AgentListNavigation {
  /** Every item a reader can reach, in the order the page draws them. */
  readonly items: readonly AgentListItem[];
  /** Draws `sessionId`'s row if the window has not, scrolls the list to it, and focuses it. */
  readonly focusRow: (sessionId: string) => void;
}

/** The toggle of the fold `row` sits in, by the row's `data-agent-section`: where focus goes
 *  when a pin lands the row in that fold closed. */
export function foldToggleOf(row: HTMLElement): HTMLElement | null {
  return (
    row
      .closest(AGENT_LIST_SELECTOR)
      ?.querySelector<HTMLElement>(
        `[${AGENT_FOLD_ATTRIBUTE}="${row.dataset.agentSection}"] button`
      ) ?? null
  );
}

/** Where the reader stands in `items`: the row that holds focus, or the fold toggle they are
 *  on, and -1 when focus is outside the list. */
function focusedItem(items: readonly AgentListItem[]): number {
  const row = closestMatching(document.activeElement, AGENT_ROW_SELECTOR)?.dataset.agentRow;
  if (row !== undefined) {
    return items.findIndex((item) => item.kind === "row" && item.id === row);
  }
  const fold = closestMatching(document.activeElement, AGENT_FOLD_SELECTOR)?.dataset.agentFold;
  if (fold === undefined) return -1;
  return items.findIndex((item) => item.kind === "fold" && item.group === fold);
}

/**
 * The session one step from `at` in `items`, or `null` where the list ends - the keys clamp at
 * both ends, as every roving list does. From outside the list `j` takes the first session and
 * `k` the last; from a fold's toggle, which sits between the rows, `j` takes the first session
 * after it and `k` the last one before it, rather than either end of the list.
 */
function sessionBeside(items: readonly AgentListItem[], at: number, delta: 1 | -1): string | null {
  if (at === -1) {
    const rows = items.filter((item) => item.kind === "row");
    return (delta === 1 ? rows[0] : rows.at(-1))?.id ?? null;
  }
  for (let index = at + delta; index >= 0 && index < items.length; index += delta) {
    const item = items[index];
    if (item?.kind === "row") return item.id;
  }
  return null;
}

/** The agent row that holds keyboard focus itself — not one merely containing a focused control. */
function focusedAgentRow(): HTMLElement | null {
  return focusedMatching(AGENT_ROW_SELECTOR);
}

/**
 * Out of a row's composer and back to the row, closing anything the reader opened on the way in.
 * The issue picker is the one of those: it is a `<select>`, where only `inEditable` bindings run,
 * so this Escape is the only dismissal that control has - leaving it expanded would make the key
 * a move rather than a way back.
 *
 * Two callers run this, and they are the same act. `MentionComposer` owns Escape on its own form
 * and stops it before the window dispatcher sees it (its `handleEscape`), so the composer
 * calls this through its `onClose`; the `agents` scope registers the same key over the same
 * function so `?` describes what Escape does there - and so the binding takes over if the
 * composer ever stops swallowing it.
 */
export function leaveAgentComposer(): void {
  const row = closestMatching(document.activeElement, AGENT_ROW_SELECTOR);
  row?.querySelector<HTMLElement>(`${ISSUE_PICKER_SELECTOR}[aria-expanded="true"]`)?.click();
  row?.focus();
}

/** The open issue picker of the row that holds focus itself, or `null`. */
function openPickerOfFocusedRow(): HTMLElement | null {
  return (
    focusedAgentRow()?.querySelector<HTMLElement>(
      `${ISSUE_PICKER_SELECTOR}[aria-expanded="true"]`
    ) ?? null
  );
}

/** Whether focus is inside a row's message composer, which is where that Escape applies. The
 *  composer renders only inside a row, so its own ancestor is the whole question. */
function inAgentComposer(): boolean {
  return closestMatching(document.activeElement, AGENT_COMPOSER_SELECTOR) !== null;
}

/** Registers the `agents` scope over the page's list for its lifetime. */
export function useAgentsKeymap(
  navigation: AgentListNavigation,
  searchInputRef: RefObject<HTMLInputElement | null>
): void {
  /** One step from where the reader stands, through the page's list rather than the drawn rows:
   *  the page scrolls the window to the session this lands on and focuses its row, so the keys
   *  reach a session the window has not drawn and the row under focus is never unmounted. */
  const step = (delta: 1 | -1) => {
    const next = sessionBeside(navigation.items, focusedItem(navigation.items), delta);
    if (next !== null) navigation.focusRow(next);
  };
  // Both actions that live inside a row's details open it first; the details render in the click's
  // own commit, so the control they want exists on the next frame.
  const inOpenRow = (act: (row: HTMLElement) => void) => {
    const row = focusedAgentRow();
    if (row === null) return;
    const toggle = row.querySelector<HTMLElement>("[data-agent-toggle]");
    if (toggle?.getAttribute("aria-expanded") === "false") toggle.click();
    requestAnimationFrame(() => act(row));
  };
  useKeymapScope("agents");
  useKeymap("agents", [
    // Shadows the `global` scope's `/`, which elsewhere opens the search dialog on its hits alone:
    // a session this page already lists is faster to find by typing here than by searching
    // Dispatch at large, and `$mod+k` still opens the dialog with this page's actions and hits.
    // `when` answers `false` rather than finding nothing to focus - with no agents connected the
    // filter bar does not render at all - so the key falls through to the global binding instead
    // of doing nothing.
    {
      id: "search",
      keys: "/",
      label: "Search agents",
      run: () => searchInputRef.current?.focus(),
      when: () => searchInputRef.current !== null,
    },
    // Movement and marking walk the list with the row in hand; a palette row for either helps
    // nobody.
    {
      id: "next",
      keys: "j",
      label: "Next agent",
      palette: false,
      run: () => step(1),
      when: () => navigation.items.some((item) => item.kind === "row"),
    },
    {
      id: "previous",
      keys: "k",
      label: "Previous agent",
      palette: false,
      run: () => step(-1),
      when: () => navigation.items.some((item) => item.kind === "row"),
    },
    {
      id: "compose",
      keys: "Enter",
      label: "Message the focused agent",
      run: () =>
        inOpenRow((row) =>
          row.querySelector<HTMLTextAreaElement>(`${AGENT_COMPOSER_SELECTOR} textarea`)?.focus()
        ),
      when: () => focusedAgentRow() !== null,
    },
    {
      id: "issue-picker",
      keys: "i",
      label: "Pick an issue for the message",
      // The picker takes focus itself once its list lands (`AgentMessageComposer.tsx`'s
      // `issueSelect`), so the key that opens it leaves the reader inside it, where the arrows
      // choose an issue and Escape is the control's own way out. It opens rather than toggles:
      // a picker left open when the row collapsed is still open, so the key goes into its select
      // instead of clicking it shut. A row not yet opened has no picker at all and a collapsed
      // one's is hidden, so the row says whether one can open; an open row's picker is disabled
      // while its message is in flight.
      run: () =>
        inOpenRow((row) => {
          const toggle = row.querySelector<HTMLElement>(ISSUE_PICKER_SELECTOR);
          if (toggle?.getAttribute("aria-expanded") === "true") {
            row.querySelector<HTMLElement>(ISSUE_SELECT_SELECTOR)?.focus();
            return;
          }
          toggle?.click();
        }),
      when: () => {
        const row = focusedAgentRow();
        return (
          row?.hasAttribute("data-agent-can-pick-issue") === true &&
          row.querySelector<HTMLButtonElement>(ISSUE_PICKER_SELECTOR)?.disabled !== true
        );
      },
    },
    {
      id: "pin",
      keys: "Shift+P",
      label: "Pin or unpin the focused agent",
      run: () => focusedAgentRow()?.querySelector<HTMLElement>("[data-agent-pin] button")?.click(),
      when: () => focusedAgentRow() !== null,
    },
    {
      id: "select",
      keys: "x",
      label: "Select or deselect the focused agent",
      palette: false,
      run: () => focusedAgentRow()?.querySelector<HTMLElement>("[data-agent-select]")?.click(),
      when: () => focusedAgentRow() !== null,
    },
    {
      id: "back",
      inEditable: true,
      keys: "Escape",
      label: "Close the issue picker, or go back to the agent row",
      run: leaveAgentComposer,
      // From inside the composer, and from the row itself while its picker is open - a read
      // that fails or is still out renders no select, so the row is where `i` leaves the
      // reader, and Escape has to close what it opened from there too.
      when: () => inAgentComposer() || openPickerOfFocusedRow() !== null,
    },
  ]);
}
