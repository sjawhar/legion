import { useNavigate } from "react-router-dom";

import type { IssuePriority } from "../../api/types";
import { outsideAskCard } from "../inbox/ask-card";
import { buildIssuePath, type IssueTab } from "../refs/routes";
import { useKeymap, useKeymapScope } from "../shell/keymap";
import type { IssuePriorityWrite } from "./useIssuePriority";

/**
 * What the `issue` scope acts on. Every target is a control the header or the tablist already
 * renders, so a key does exactly what a click on it does. Scoping the lookup to
 * `[data-issue-header]` keeps a child issue's row in the Children tab - it carries the same
 * accessible names - out of reach.
 */
const HEADER = "[data-issue-header]";
const LABELS_TRIGGER = 'button[aria-label="Edit labels"]';
const PIN_TOGGLE = 'button[aria-label="Pin issue"], button[aria-label="Unpin issue"]';
const PRIORITY_SELECT = 'select[aria-label^="Priority of "]';
const STATUS_SELECT = 'select[aria-label="Status"]';
const TITLE_HEADING = 'h1[tabindex="0"]';

/** The header control `selector` names, or `null` when it is absent or refuses input (a closed
 *  issue, a save in flight): the one fact both the key's `run` and the `when` that offers it read,
 *  so a shortcut is listed exactly while its control would take a click. */
function usableControl(selector: string): HTMLElement | null {
  const node = document.querySelector(HEADER)?.querySelector<HTMLElement>(selector);
  if (node == null) {
    return null;
  }
  const disabled =
    (node instanceof HTMLButtonElement || node instanceof HTMLSelectElement) && node.disabled;
  return disabled ? null : node;
}

/**
 * Whether focus is outside every `<input>` and outside every ask card. `0`-`3` and `Shift+P` are
 * the two keys that write the issue without focusing a control, so they are the two that have to
 * say where the reader is not: a digit typed straight after pressing the Conversation's `Open`
 * filter or picking an ask's option belongs to that control's own surface, not to the issue's
 * priority. The registry passes single keys through every control that takes no typed text, so
 * this is the page's own question to answer.
 *
 * A focused `<button>` is deliberately not excluded. Every single-key binding in the registry
 * fires from one: a digit has no native meaning on a button, and after any click - a tab, a
 * disclosure, Copy - focus sits on the button that was clicked, which is where a reader reaching
 * for the next shortcut stands. Withholding these keys there would make them work only from the
 * page's own background. An ask card's buttons are the exception, and that is what
 * `outsideAskCard` covers (`inbox/ask-card.ts`, the one definition both pages read): inside the
 * card, the card's controls own the keys. `l` and `e` open a control rather
 * than write, so a stray press is visible and undoable and they are offered throughout.
 */
function outsideInputsAndAskCards(): boolean {
  return !(document.activeElement instanceof HTMLInputElement) && outsideAskCard();
}

/** The address each chord opens, by the tab whose address it was: the presets replaced the
 *  tablist, and every one of those addresses still resolves (R19), so a chord navigates to it
 *  rather than pressing a button that is no longer there. */
const tabChords: readonly { keys: string; label: string; tab: IssueTab }[] = [
  { keys: "t s", label: "Spec tab", tab: "spec" },
  // `t v` is Activity and `t c` the Conversation, so Verification takes its checks' `k`; `t p`
  // is free (`p` alone focuses the priority, which is a different scope of key).
  { keys: "t k", label: "Verification tab", tab: "verification" },
  { keys: "t p", label: "Plan tab", tab: "plan" },
  { keys: "t c", label: "Conversation tab", tab: "conversation" },
  { keys: "t h", label: "Children tab", tab: "children" },
  { keys: "t a", label: "Artifacts tab", tab: "artifacts" },
  // `t a` is Artifacts and `l` is the issue's label editor, so Activity takes `t v`.
  { keys: "t v", label: "Activity tab", tab: "activity" },
  { keys: "t d", label: "Decisions tab", tab: "decisions" },
];

/**
 * Every key the issue page binds in its own `issue` scope, and the scope itself.
 *
 * The scope is the issue page; every binding drives a control the identity block or the tablist
 * already draws - the same handler a click reaches, offered exactly while that control would
 * take one. The digits go through the page's one priority write, so a keyed priority lands in
 * the same cache the picker writes. `IssueHeader` registers the palette-only issue actions into
 * this same scope (`useIssuePalette.ts`); neither pushes a scope of its own.
 */
export function useIssueKeymap(priorityWrite: IssuePriorityWrite, issueKey: string): void {
  const navigate = useNavigate();
  useKeymapScope("issue");
  useKeymap("issue", [
    {
      id: "status",
      keys: "s",
      label: "Focus the status",
      run: () => usableControl(STATUS_SELECT)?.focus(),
      when: () => usableControl(STATUS_SELECT) !== null,
    },
    {
      id: "priority",
      keys: "p",
      label: "Focus the priority",
      run: () => usableControl(PRIORITY_SELECT)?.focus(),
      when: () => usableControl(PRIORITY_SELECT) !== null,
    },
    {
      id: "set-priority",
      keys: ["0", "1", "2", "3"],
      label: "Set priority P0–P3",
      run: (event) => priorityWrite.submit(Number(event.key) as IssuePriority),
      when: () => usableControl(PRIORITY_SELECT) !== null && outsideInputsAndAskCards(),
    },
    {
      id: "labels",
      keys: "l",
      label: "Edit labels",
      run: () => usableControl(LABELS_TRIGGER)?.click(),
      when: () => usableControl(LABELS_TRIGGER) !== null,
    },
    {
      // The heading takes focus only while the issue is open: closed, its `tabIndex` is -1 and
      // `onFocus` does not open the editor, so the key is offered exactly while it edits.
      id: "title",
      keys: "e",
      label: "Edit the title",
      run: () => usableControl(TITLE_HEADING)?.focus(),
      when: () => usableControl(TITLE_HEADING) !== null,
    },
    {
      id: "pin",
      keys: "Shift+P",
      label: "Pin or unpin the issue",
      run: () => usableControl(PIN_TOGGLE)?.click(),
      when: () => usableControl(PIN_TOGGLE) !== null && outsideInputsAndAskCards(),
    },
    {
      // `s` and `p` put focus in a native select, where every single-key binding is suspended
      // and the next keystroke is the select's own type-ahead - `t` would pick Todo or Testing
      // rather than start the tab chord. Escape is the way back out, as it is on the Inbox
      // rows, the board's cards and the architecture rows.
      id: "back",
      inEditable: true,
      keys: "Escape",
      label: "Back out of the focused header control",
      run: () => (document.activeElement as HTMLElement | null)?.blur(),
      when: () => {
        const active = document.activeElement;
        return active === usableControl(STATUS_SELECT) || active === usableControl(PRIORITY_SELECT);
      },
    },
    ...tabChords.map(({ keys, label, tab }) => ({
      id: `tab-${tab}`,
      keys,
      label,
      run: () => navigate(buildIssuePath({ key: issueKey, kind: tab })),
    })),
  ]);
}
