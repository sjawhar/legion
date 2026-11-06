import { useNavigate } from "react-router-dom";

import type { IssuePriority } from "../../api/types";
import { outsideAskCard } from "../inbox/ask-card";
import { buildIssuePath, type IssueTab } from "../refs/routes";
import { useKeymap, useKeymapScope } from "../shell/keymap";
import { bringOnScreen } from "./identity-block";
import { presetPath } from "./issue-address";
import { CONTROL_LABELS } from "./page-words";
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

/**
 * The parts of the page a chord brings the reader to, by the mark the page draws each one
 * with. They are the marks `e2e/issue-page.ts` reads the page by, so the keyboard and the
 * checks look for the same element and neither has a name of its own for a part of the page.
 */
const PARTS = {
  children: '[data-testid="issue-children"]',
  filter: `input[aria-label="${CONTROL_LABELS.filter}"]`,
  margin: '[data-testid="issue-margin"]',
  menu: '[role="menu"]',
  tree: '[role="tree"]',
} as const;

/** What takes the focus inside each part, where the part itself does not: the tree's one tab
 *  stop, the panel's selected conversation state, the children's own disclosure, and a menu's
 *  first item once it holds any (`dispatch://INFRA-1196/artifact/plan-md#b-t18`). */
const TAKES_FOCUS: Readonly<Record<keyof typeof PARTS, string | undefined>> = {
  children: "button",
  filter: undefined,
  margin: '[data-conversation-state][aria-selected="true"]',
  menu: '[role="menuitem"]',
  tree: '[data-node-row][tabindex="0"]',
};

/** How many frames a chord waits for the page to draw what its address opened before it gives
 *  up on putting the focus there: a navigation renders on the next frame, and a panel that
 *  mounts its content takes a few more. */
const DRAWING_FRAMES = 20;

/**
 * Brings the part of the page `part` names on screen and puts the focus in it, once the page
 * has drawn it: a chord navigates, and what it opens is on the page a frame or two later (R9,
 * c-keyboard, which reads both the box and the focus).
 */
function reachPart(part: keyof typeof PARTS, left = DRAWING_FRAMES): void {
  const found = document.querySelector<HTMLElement>(PARTS[part]);
  if (found === null) {
    if (left > 0) requestAnimationFrame(() => reachPart(part, left - 1));
    return;
  }
  const inside = TAKES_FOCUS[part];
  const takes = inside === undefined ? found : (found.querySelector<HTMLElement>(inside) ?? found);
  // The focus is taken without scrolling to it, and what the chord opens is then shown from its
  // own top: a control inside a panel that scrolls of its own would otherwise leave the panel's
  // head above the screen (R9, c-keyboard reads the box). A part taller than the screen cannot
  // be shown whole - the page's panel is a full-height column (`margin/MarginSheet.tsx`'s
  // `h-dvh`) - so there the chord shows the control it leaves the reader on instead, and shows
  // it in the middle of the screen rather than at the panel's own top, which is the one place
  // in that column the page's sticky identity block reaches across.
  takes.focus({ preventScroll: true });
  const whole = found.getBoundingClientRect().height <= window.innerHeight;
  const shown = whole ? found : takes;
  shown.scrollIntoView({ block: whole ? "start" : "center" });
  bringOnScreen(shown);
}

/**
 * Every chord the page binds, which is the `t` table of the spec's Design section. Each one
 * opens an address and puts the focus in what that address draws: the eight the page bound
 * before the redesign keep the addresses their tabs had, every one of which still resolves
 * (R19), and `t t` and `t f` are the two the table adds. The labels are the tabs' own, because
 * `?` names a key by where it goes and those addresses have not moved.
 */
const CHORDS: readonly {
  keys: string;
  label: string;
  /** The address the chord opens, or undefined for a chord that opens none. */
  path?: (key: string) => string;
  part: keyof typeof PARTS;
  /** What names the binding, which is the tab the address was that tab's. */
  tab: IssueTab | "tree" | "filter";
}[] = [
  {
    keys: "t t",
    label: "Tree preset",
    part: "tree",
    path: (key) => presetPath(key, "tree"),
    tab: "tree",
  },
  {
    keys: "t s",
    label: "Spec tab",
    part: "tree",
    path: (key) => presetPath(key, "spec"),
    tab: "spec",
  },
  // `t v` is Activity and `t c` the Conversation, so Verification takes its checks' `k`; `t p`
  // is free (`p` alone focuses the priority, which is a different scope of key).
  {
    keys: "t k",
    label: "Verification tab",
    part: "tree",
    path: (key) => presetPath(key, "checks"),
    tab: "verification",
  },
  {
    keys: "t p",
    label: "Plan tab",
    part: "tree",
    path: (key) => presetPath(key, "plan"),
    tab: "plan",
  },
  {
    keys: "t d",
    label: "Decisions tab",
    part: "tree",
    path: (key) => presetPath(key, "decisions"),
    tab: "decisions",
  },
  {
    keys: "t c",
    label: "Conversation tab",
    part: "margin",
    path: (key) => buildIssuePath({ key, kind: "conversation" }),
    tab: "conversation",
  },
  // `t a` is Artifacts and `l` is the issue's label editor, so Activity takes `t v`.
  {
    keys: "t v",
    label: "Activity tab",
    part: "margin",
    path: (key) => buildIssuePath({ key, kind: "activity" }),
    tab: "activity",
  },
  {
    keys: "t h",
    label: "Children tab",
    part: "children",
    path: (key) => buildIssuePath({ key, kind: "children" }),
    tab: "children",
  },
  {
    keys: "t a",
    label: "Artifacts tab",
    part: "menu",
    path: (key) => buildIssuePath({ key, kind: "artifacts" }),
    tab: "artifacts",
  },
  // The filter narrows the preset the reader is already on, so it opens no address of its own:
  // what it types is `?q=`, which the box itself writes.
  { keys: "t f", label: "Filter the preset", part: "filter", tab: "filter" },
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
    ...CHORDS.map(({ keys, label, part, path, tab }) => ({
      id: `tab-${tab}`,
      keys,
      label,
      run: () => {
        if (path !== undefined) navigate(path(issueKey));
        reachPart(part);
      },
    })),
  ]);
}
