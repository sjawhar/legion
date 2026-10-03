/**
 * The `agents` scope: `j`/`k` over the session rows, and the four keys that act on the row in
 * hand. Every one of them finds its control through a `data-agent-*` handle on the row, so the
 * page's markup is the only contract between them.
 */
import type { RefObject } from "react";

import { useKeymap, useKeymapScope } from "../shell/keymap";
import { closestMatching, focusedMatching, roveFocus } from "../shell/roving";

/** The page renders these handles and this module acts on them; one spelling of each, so a
 *  reader who moves one knows where it is read. */
const AGENT_ROW_ATTRIBUTE = "data-agent-row";
export const AGENT_ROW_SELECTOR = `[${AGENT_ROW_ATTRIBUTE}]`;
const AGENT_COMPOSER_SELECTOR = "[data-agent-composer]";
export const ISSUE_PICKER_SELECTOR = "[data-agent-issue-picker]";

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

/** Registers the `agents` scope over the rows inside `listRef` for the page's lifetime. */
export function useAgentsKeymap(listRef: RefObject<HTMLElement | null>): void {
  const rows = () => [
    ...(listRef.current?.querySelectorAll<HTMLElement>(AGENT_ROW_SELECTOR) ?? []),
  ];
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
    // Movement and marking walk the list with the row in hand; a palette row for either helps
    // nobody.
    {
      id: "next",
      keys: "j",
      label: "Next agent",
      palette: false,
      run: () => roveFocus(rows(), closestMatching(document.activeElement, AGENT_ROW_SELECTOR), 1),
      when: () => rows().length > 0,
    },
    {
      id: "previous",
      keys: "k",
      label: "Previous agent",
      palette: false,
      run: () => roveFocus(rows(), closestMatching(document.activeElement, AGENT_ROW_SELECTOR), -1),
      when: () => rows().length > 0,
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
      // The picker takes focus itself once its list lands (`AgentsPage.tsx`'s `issueSelect`), so
      // the key that opens it leaves the reader inside it, where the arrows choose an issue and
      // Escape is the control's own way out.
      run: () => inOpenRow((row) => row.querySelector<HTMLElement>(ISSUE_PICKER_SELECTOR)?.click()),
      when: () => focusedAgentRow()?.hasAttribute("data-agent-can-pick-issue") === true,
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
