import { expect, type Locator, type Page } from "@playwright/test";

import { CONTROL_LABELS, PRESET_NAMES, type Preset } from "../web/src/features/issue/page-words";
import type { Box } from "./layout";

/**
 * The DOM of the redesigned issue page (dispatch://INFRA-1196), as one module.
 *
 * `e2e/issue-layout.e2e.ts` and the tree's rows in `e2e/keyboard-issue.e2e.ts` read the page only
 * through this module and spell no selector of their own, so this file is the single statement of
 * what the page's DOM is: the page is built to it, and anything named here is a requirement on
 * that page. Each item below names the check that reads it, by the `check` block of
 * `dispatch://INFRA-1196/artifact/verification-md`, so nothing is named that no check reads.
 *
 * ## What the page draws
 *
 * - **The page root** `[data-issue-page]`. Every check reads it, because every test asserts the
 *   page drew at all before it reads anything inside it.
 * - **The identity block** `header[data-issue-header]`: today's attribute, kept, because the
 *   keyboard scope finds the Status, priority, labels, title and pin controls through it
 *   (`web/src/features/issue/IssueHeader.tsx:370-378`). c-properties, c-nothing-lost, c-keyboard.
 * - **The head of the page** `[data-testid="issue-three-things"]`, holding the spec summary's
 *   sentences as `[data-author-text]` and two rows, `[data-testid="issue-plan-row"]` and
 *   `[data-testid="issue-checks-row"]`. c-three-things.
 * - **The children section** `[data-testid="issue-children"]`, its disclosure a `button`, each row
 *   `[data-child-row]` with `data-child-status`, and the collapsed header's two counts
 *   `[data-testid="issue-children-direct-count"]` and
 *   `[data-testid="issue-children-subtree-count"]`, each with a visible label. c-children,
 *   c-nothing-lost, c-keyboard (`t h`).
 * - **The preset row** `[data-testid="issue-presets"]`. At 1280 px and above it is a row of five,
 *   each preset a `tab` in a `role="tablist"` named "Issue presets", its accessible name the
 *   preset's name alone, with its count in `[data-preset-count]` inside it. At 1024 px and below
 *   the row becomes one control: a `button` with `aria-haspopup="menu"` whose name is the active
 *   preset, opening a `role="menu"` of five `role="menuitem"`s. Each preset has its own address
 *   (`presetAddress`). c-presets, c-three-things, c-children, c-urls, c-plain-words, c-keyboard.
 * - **The node menu** beside the presets: a `button` named by `CONTROL_LABELS.other_nodes`,
 *   opening a `role="menu"`. c-nodes, c-urls (`/artifacts`), c-keyboard (`t a`).
 * - **The tree**: `role="tree"` named "Issue tree", rows `role="treeitem"` carrying `aria-level`
 *   and, when they hold children, `aria-expanded`. Each row is `[data-node-row]` with
 *   `data-node-type` (the model's own token, a key of `NODE_TYPE_NAMES`), `data-node-id`,
 *   `data-node-state` (absent for a type the model gives no state) and, when folded,
 *   `data-folded-count`. Inside a row: the type's word (as page chrome, read as the row's text),
 *   its glyph as `svg[data-glyph]` with an accessible name, the author where the model holds one
 *   as `[data-node-author]`, the first line as `[data-node-first-line][data-author-text]`, each
 *   chip as `[data-node-chip]` naming its target, and the state word as `[data-node-state]`'s own
 *   text. A branch deeper than the three drawn levels is one row `[data-branch-more]` whose text
 *   says how many nodes it holds. c-tree, c-spine, c-focus-opens, c-presets, c-checks-readable,
 *   c-reflow, c-plain-words, c-keyboard.
 * - **A node's body** `[data-node-body]`, drawn for the focused row and for a row pinned open
 *   (`[data-node-body][data-pinned]`) and for no other row. c-tree, c-focus-opens, c-keyboard.
 * - **A gap the page names** `[data-gap]` with `data-gap-kind`. c-spine.
 * - **The right-hand panel**: the shell's existing margin, with the issue page's own slot
 *   `[data-testid="issue-margin"]`, its sections
 *   `[data-margin-section="development" | "people" | "references"]`, and the focused node's facts
 *   `[data-testid="focused-node-facts"]`. c-one-panel, c-properties.
 * - **The page's own words** carry `data-page-chrome`; text from a person or an agent carries
 *   `data-author-text` (`PAGE_CHROME_ATTRIBUTE`, `AUTHOR_TEXT_ATTRIBUTE`). c-plain-words.
 *
 * ## What this module adds to that contract, and the check that reads each
 *
 * Each of these is a thing a check's own sentence needs and the contract does not name. They are
 * requirements on the page like every item above.
 *
 * - **A node's id** (`data-node-id`) is the node's own address inside the issue, so two documents
 *   that both hold a block `r1` draw two different rows: the issue is its key, a document or other
 *   artifact its slug, a block `<slug>#b-<block id>` (`blockNodeId`), and an ask, comment,
 *   message, handoff or result the id the model gives it. A child issue is its key. Read by every
 *   test that names one row. An ask the server indexed from a document's `ask` block is that
 *   block's node, under `blockNodeId`; an ask made on the issue itself is drawn under its own id.
 * - **A node's body sits inside its own row**, so a row and its body are read together
 *   (`[data-node-row] [data-node-body]`). c-tree, c-focus-opens.
 * - **`[data-node-context]`** on a row a preset draws only as the ancestor of a match, which is
 *   what "the ancestors faint and reachable with the arrow keys" is read by. c-presets.
 * - **`[data-testid="issue-filter"]`**, the filter the preset's filter key opens: a `searchbox`
 *   carrying the query. c-keyboard (`t f`), c-urls (`?q=`).
 * - **`[data-testid="issue-preset-toolbar"]`**, the toolbar of the preset whose document is open,
 *   holding the version picker, the name-a-version control and the approval control, which the
 *   Design section puts there. c-urls (`?v=`), c-home (the approval), c-nothing-lost (naming a
 *   version).
 * - **`[data-conversation-state]`** on each of the conversation's four state controls, with
 *   `aria-selected` saying which state the panel is in. c-urls (`/conversation`, `/activity`),
 *   c-one-panel.
 * - **`[data-node-command]`** on a check's `run` command, which renders as code beside its copy
 *   control. c-checks-readable.
 * - **The add control** (`CONTROL_LABELS.add`) opens a `role="menu"` listing every property with
 *   no value, which is how "an empty field is reachable from one add control that lists it" is
 *   read. c-properties.
 * - **The preset row holds the presets alone**: the node menu's button sits beside that row and
 *   outside it, so the one control at 1024 px and below is unambiguous. c-presets, c-nodes.
 * - **The children section's completion bar**, a `role="progressbar"` the collapsed header keeps
 *   beside its two counts. c-children.
 * - **A `role="status"` of the page's own** for what a key reports when it has nowhere to go,
 *   such as the next-question key with no further question. c-open-questions, c-keyboard.
 *
 * ## How a test reads the page
 *
 * A `locator` that never resolves times out rather than asserting, so the readers below answer
 * through one `page.evaluate` over the document and return what is there - an empty list for a
 * page that draws none of it - and a test asserts on that answer. `gotoIssue` waits only for the
 * identity block, which the old page and the new one both draw, so a test against the old page
 * fails on its own assertion and never on a navigation that never settles.
 */

/** The selectors the readers below pass into the browser. Nothing outside this module spells one:
 *  a test reads the page through the locators and readers this module exports. */
const SELECTORS = {
  authorText: "[data-author-text]",
  branchMore: "[data-branch-more]",
  childRow: "[data-child-row]",
  gap: "[data-gap]",
  identityBlock: "header[data-issue-header]",
  issuePage: "[data-issue-page]",
  marginSlot: '[data-testid="issue-margin"]',
  nodeAuthor: "[data-node-author]",
  nodeBody: "[data-node-body]",
  nodeChip: "[data-node-chip]",
  nodeFirstLine: "[data-node-first-line]",
  nodeGlyph: "svg[data-glyph]",
  nodeRow: "[data-node-row]",
  nodeState: "[data-node-state]",
  pageChrome: "[data-page-chrome]",
  presetCount: "[data-preset-count]",
  presetRow: '[data-testid="issue-presets"]',
  tree: '[role="tree"]',
} as const;

/** The address of each preset, under `/issues/<key>`: the Checks preset keeps `/verification` and
 *  the Tree preset is the issue's own address (R19, c-urls). */
const PRESET_PATHS: Readonly<Record<Preset, string>> = {
  checks: "/verification",
  decisions: "/decisions",
  plan: "/plan",
  spec: "/spec",
  tree: "",
};

/** The address of `preset` on `key`, which is the address the preset's own tab carries. */
export function presetAddress(key: string, preset: Preset): string {
  return `/issues/${key}${PRESET_PATHS[preset]}`;
}

/** The id of the row of one block of one document: a block id alone is unique only inside its
 *  document, and an issue carries several. */
export function blockNodeId(slug: string, block: string): string {
  return `${slug}#b-${block}`;
}

// ---------------------------------------------------------------------------------------------
// The page's parts, as locators.
// ---------------------------------------------------------------------------------------------

export function issuePage(page: Page): Locator {
  return page.locator(SELECTORS.issuePage);
}

export function identityBlock(page: Page): Locator {
  return page.locator(SELECTORS.identityBlock);
}

export function threeThings(page: Page): Locator {
  return page.getByTestId("issue-three-things");
}

/** The sentences of the spec's summary, each one the author's own text. */
export function threeThingsSentences(page: Page): Locator {
  return threeThings(page).locator(SELECTORS.authorText);
}

export function planRow(page: Page): Locator {
  return page.getByTestId("issue-plan-row");
}

export function checksRow(page: Page): Locator {
  return page.getByTestId("issue-checks-row");
}

export function childrenSection(page: Page): Locator {
  return page.getByTestId("issue-children");
}

/** The one control that opens and closes the children section. */
export function childrenDisclosure(page: Page): Locator {
  return childrenSection(page).getByRole("button").first();
}

export function childRows(page: Page): Locator {
  return childrenSection(page).locator(SELECTORS.childRow);
}

export function childRow(page: Page, key: string): Locator {
  return childRows(page).filter({ hasText: key });
}

export function childrenDirectCount(page: Page): Locator {
  return page.getByTestId("issue-children-direct-count");
}

export function childrenSubtreeCount(page: Page): Locator {
  return page.getByTestId("issue-children-subtree-count");
}

/** The completion bar the collapsed header keeps beside its two counts. */
export function childrenBar(page: Page): Locator {
  return childrenSection(page).getByRole("progressbar");
}

export function presetRow(page: Page): Locator {
  return page.locator(SELECTORS.presetRow);
}

/** The row of five, at 1280 px and above. */
export function presetTablist(page: Page): Locator {
  return presetRow(page).getByRole("tablist", { name: "Issue presets" });
}

export function presetTabs(page: Page): Locator {
  return presetRow(page).getByRole("tab");
}

export function presetTab(page: Page, preset: Preset): Locator {
  return presetRow(page).getByRole("tab", { exact: true, name: PRESET_NAMES[preset] });
}

export function presetCount(page: Page, preset: Preset): Locator {
  return presetTab(page, preset).locator(SELECTORS.presetCount);
}

/** The one control the row becomes at 1024 px and below, named by the active preset. */
export function presetChooser(page: Page): Locator {
  return presetRow(page).locator('button[aria-haspopup="menu"]');
}

/** The one menu the page has open, whether the preset chooser's or the node menu's: the page
 *  opens at most one at a time, which a test asserts by counting this. */
export function openMenu(page: Page): Locator {
  return page.getByRole("menu");
}

export function presetMenuItem(page: Page, preset: Preset): Locator {
  return openMenu(page).getByRole("menuitem", { exact: true, name: PRESET_NAMES[preset] });
}

export function nodeMenuButton(page: Page): Locator {
  return issuePage(page).getByRole("button", { exact: true, name: CONTROL_LABELS.other_nodes });
}

export function nodeMenuItems(page: Page): Locator {
  return openMenu(page).getByRole("menuitem");
}

/** The one control that adds a property with no value yet (R11 bans "more"). */
export function addControl(page: Page): Locator {
  return identityBlock(page).getByRole("button", { exact: true, name: CONTROL_LABELS.add });
}

export function tree(page: Page): Locator {
  return issuePage(page).getByRole("tree", { name: "Issue tree" });
}

export function treeItems(page: Page): Locator {
  return tree(page).getByRole("treeitem");
}

export function nodeRows(page: Page): Locator {
  return issuePage(page).locator(SELECTORS.nodeRow);
}

export function nodeRow(page: Page, id: string): Locator {
  return nodeRows(page).filter({ has: page.locator(`:scope[data-node-id="${cssValue(id)}"]`) });
}

export function rowsOfType(page: Page, type: string): Locator {
  return nodeRows(page).filter({
    has: page.locator(`:scope[data-node-type="${cssValue(type)}"]`),
  });
}

/** The author named on one row, or on the head of the page, where the model holds one. */
export function rowAuthor(scope: Locator): Locator {
  return scope.locator(SELECTORS.nodeAuthor);
}

/** The pictures drawn inside one row: an image node's own, and a screenshot in a node's text. */
export function rowPictures(row: Locator): Locator {
  return row.locator("img");
}

export function rowBody(row: Locator): Locator {
  return row.locator(SELECTORS.nodeBody);
}

export function nodeBodies(page: Page): Locator {
  return issuePage(page).locator(SELECTORS.nodeBody);
}

export function pinnedBodies(page: Page): Locator {
  return issuePage(page).locator(`${SELECTORS.nodeBody}[data-pinned]`);
}

export function branchMoreRows(page: Page): Locator {
  return issuePage(page).locator(SELECTORS.branchMore);
}

export function gaps(page: Page): Locator {
  return issuePage(page).locator(SELECTORS.gap);
}

/** The gaps drawn on one row. */
export function rowGaps(row: Locator): Locator {
  return row.locator(SELECTORS.gap);
}

/** The hidden file input the upload opens, which is how a test hands it a file. */
export function uploadInput(page: Page): Locator {
  return page.locator('input[type="file"]');
}

/** What the page says when a key has nowhere to go, such as the next-question key with no
 *  further question (c-open-questions, c-keyboard). */
export function pageStatus(page: Page): Locator {
  return issuePage(page).getByRole("status");
}

/** A check's `run` command, drawn as code with its copy control beside it. */
export function nodeCommands(page: Page): Locator {
  return issuePage(page).locator("[data-node-command]");
}

export function copyCommandControls(page: Page): Locator {
  return issuePage(page).getByRole("button", { exact: true, name: CONTROL_LABELS.copy_command });
}

/** The filter the preset's filter key opens. */
export function filterBox(page: Page): Locator {
  return page.getByTestId("issue-filter");
}

/** The toolbar of the preset whose document is open: the version picker, the name-a-version
 *  control and the approval control. */
export function presetToolbar(page: Page): Locator {
  return page.getByTestId("issue-preset-toolbar");
}

/** The issue page's own slot in the shell's margin. */
export function marginSlot(page: Page): Locator {
  return page.getByTestId("issue-margin");
}

export type MarginSection = "development" | "people" | "references";

export function marginSection(page: Page, section: MarginSection): Locator {
  return marginSlot(page).locator(`[data-margin-section="${section}"]`);
}

/** The one control that opens and closes a margin section. */
export function marginSectionDisclosure(page: Page, section: MarginSection): Locator {
  return marginSection(page, section).getByRole("button").first();
}

export function focusedNodeFacts(page: Page): Locator {
  return page.getByTestId("focused-node-facts");
}

/** The shell's own margin, which the issue page's slot sits inside and which every other page of
 *  Dispatch keeps as it is (`web/src/features/margin/Margin.tsx`). */
export function marginShell(page: Page): Locator {
  return page.getByTestId("desktop-margin-shell");
}

export function marginResizeHandle(page: Page): Locator {
  return page.getByLabel("Resize margin");
}

export function marginHideControl(page: Page): Locator {
  return page.getByRole("button", { exact: true, name: "Hide margin" });
}

export function marginShowControl(page: Page): Locator {
  return page.getByRole("button", { exact: true, name: "Show margin" });
}

export type ConversationState = "everything" | "pinned" | "questions" | "words";

/** One of the conversation's four state controls, inside the panel. */
export function conversationStateControl(page: Page, state: ConversationState): Locator {
  return marginSlot(page).locator(`[data-conversation-state="${state}"]`);
}

/** A control of the page by the role and accessible name the old page's inventory recorded for it
 *  (`e2e/fixtures/issue-page-inventory.json`), which is how c-nothing-lost acts on each name that
 *  writes today. */
export function offered(page: Page, role: Parameters<Page["getByRole"]>[0], name: string): Locator {
  return page.getByRole(role, { exact: true, name });
}

/** What a `select` offers, as its value and its label: a test that writes a property picks the
 *  option it means rather than an index (c-nothing-lost). */
export function optionsOf(control: Locator): Promise<{ label: string; value: string }[]> {
  return control.evaluateAll((found) =>
    found.flatMap((element) =>
      [...(element as HTMLSelectElement).options].map((option) => ({
        label: option.label,
        value: option.value,
      }))
    )
  );
}

/** Leaves whatever control holds the focus, so a key the page binds in reading mode reaches it.
 *  `body.focus()` does not do it: `body` takes no focus and the control keeps it. */
export async function focusThePage(page: Page): Promise<void> {
  await page.evaluate(() => (document.activeElement as HTMLElement | null)?.blur());
  await expect.poll(() => page.evaluate(() => document.activeElement === document.body)).toBe(true);
}

/** A `"` inside an attribute value would end the selector's string, and no id, slug or kind the
 *  page draws holds one; a value that did would otherwise match nothing silently. */
function cssValue(value: string): string {
  expect(value, `${value} cannot be written into a selector`).not.toContain('"');
  return value;
}

// ---------------------------------------------------------------------------------------------
// The readers: one `page.evaluate` each, so a page that draws none of this answers an empty list
// instead of timing out.
// ---------------------------------------------------------------------------------------------

/** One row of the tree, as the page draws it. */
export interface DrawnRow {
  /** The author the row names, or null where the model holds none. */
  readonly author: string | null;
  /** Whether the row draws a body of its own. */
  readonly body: boolean;
  /** Where the row is, or null while it is not rendered. */
  readonly box: Box | null;
  /** The text of each chip on the row, in order. */
  readonly chips: readonly string[];
  /** `aria-expanded`, or null for a row that holds no children. */
  readonly expanded: string | null;
  /** `data-folded-count`, or null for a row that is not folded. */
  readonly foldedCount: string | null;
  /** The `data-glyph` token of each shape drawn on the row, with the accessible name of each. */
  readonly glyphs: readonly { readonly name: string; readonly token: string }[];
  readonly id: string | null;
  /** The row's own first line, as drawn. */
  readonly firstLine: string | null;
  /** `aria-level`, as drawn. */
  readonly level: string | null;
  /** Whether the row is drawn only as the ancestor of a match in this preset. */
  readonly context: boolean;
  /** Whether the row's body is pinned open. */
  readonly pinned: boolean;
  readonly role: string | null;
  /** `data-node-state`, the model's own token, or null for a type it gives no state. */
  readonly state: string | null;
  /** The state word the row draws, or null where it draws none. */
  readonly stateWord: string | null;
  /** The row's own `tabindex`, which the WAI-ARIA tree pattern roves over its rows. */
  readonly tabIndex: number;
  /** The whole visible text of the row, its body included. */
  readonly text: string;
  /** `data-node-type`, the model's own token for the kind of node. */
  readonly type: string | null;
  readonly visible: boolean;
}

/** Every row the page draws, in document order. */
export function drawnRows(page: Page): Promise<DrawnRow[]> {
  return page.evaluate((selectors) => {
    const text = (element: Element | null): string | null =>
      element === null ? null : (element.textContent ?? "").replace(/\s+/g, " ").trim();
    const box = (element: Element) => {
      const rect = element.getBoundingClientRect();
      if (rect.width === 0 && rect.height === 0) return null;
      return { height: rect.height, width: rect.width, x: rect.x, y: rect.y };
    };
    const rows = [...document.querySelectorAll(`${selectors.issuePage} ${selectors.nodeRow}`)];
    return rows.map((row) => ({
      author: text(row.querySelector(selectors.nodeAuthor)),
      body: row.querySelector(selectors.nodeBody) !== null,
      box: box(row),
      chips: [...row.querySelectorAll(selectors.nodeChip)].map((chip) => text(chip) ?? ""),
      context: row.hasAttribute("data-node-context"),
      expanded: row.getAttribute("aria-expanded"),
      firstLine: text(row.querySelector(selectors.nodeFirstLine)),
      foldedCount: row.getAttribute("data-folded-count"),
      glyphs: [...row.querySelectorAll(selectors.nodeGlyph)].map((glyph) => ({
        name:
          glyph.getAttribute("aria-label") ??
          text(glyph.querySelector("title")) ??
          text(glyph.querySelector("desc")) ??
          "",
        token: glyph.getAttribute("data-glyph") ?? "",
      })),
      id: row.getAttribute("data-node-id"),
      level: row.getAttribute("aria-level"),
      pinned: row.querySelector(`${selectors.nodeBody}[data-pinned]`) !== null,
      role: row.getAttribute("role"),
      state: row.getAttribute("data-node-state"),
      stateWord: text(row.querySelector(selectors.nodeState)),
      tabIndex: (row as HTMLElement).tabIndex,
      text: text(row) ?? "",
      type: row.getAttribute("data-node-type"),
      visible: row.getClientRects().length > 0,
    }));
  }, SELECTORS);
}

/** The row `id` sits under, by the tree's own `aria-level`: the nearest row before it one level
 *  up. `null` for a row at the top level, and `undefined` for a row the page does not draw, which
 *  a test tells apart from "under the issue". */
export async function parentOfRow(page: Page, id: string): Promise<string | null | undefined> {
  const rows = await drawnRows(page);
  const index = rows.findIndex((row) => row.id === id);
  if (index === -1) return undefined;
  const level = Number(rows[index]?.level ?? "");
  if (!Number.isFinite(level)) return null;
  for (let before = index - 1; before >= 0; before -= 1) {
    const candidate = rows[before];
    if (candidate === undefined) continue;
    if (Number(candidate.level ?? "") === level - 1) return candidate.id;
  }
  return null;
}

/** The chain of ancestors above `id`, outermost first, as the drawn levels give it. */
export async function ancestorsOfRow(page: Page, id: string): Promise<string[]> {
  const chain: string[] = [];
  for (let at: string | null | undefined = id; at !== null && at !== undefined; ) {
    const parent: string | null | undefined = await parentOfRow(page, at);
    if (parent === null || parent === undefined) break;
    chain.unshift(parent);
    at = parent;
  }
  return chain;
}

/** The id of the row the keyboard focus is on: the row holding the focus, or the row the tree's
 *  `aria-activedescendant` names, whichever the page uses. */
export function focusedNodeId(page: Page): Promise<string | null> {
  return page.evaluate((selectors) => {
    const active = document.activeElement;
    const own = active?.closest(selectors.nodeRow) ?? null;
    if (own !== null) return own.getAttribute("data-node-id");
    const named = active?.getAttribute("aria-activedescendant") ?? null;
    if (named === null) return null;
    const row = document.getElementById(named)?.closest(selectors.nodeRow) ?? null;
    return row?.getAttribute("data-node-id") ?? null;
  }, SELECTORS);
}

/** Whether the keyboard focus is inside `scope`: a chord that reaches a target has to put the
 *  focus in it, not only draw it (c-keyboard). A scope the page does not draw answers false. */
export async function focusInside(scope: Locator): Promise<boolean> {
  if ((await scope.count()) === 0) return false;
  return await scope
    .first()
    .evaluate(
      (element) => element.contains(document.activeElement) || element === document.activeElement
    );
}

/** Whether every pixel of `scope` is inside the viewport and clear of the sticky identity block,
 *  which is what "brings a target on screen" is read as (c-keyboard, c-open-questions). */
export async function onScreen(scope: Locator): Promise<boolean> {
  if ((await scope.count()) === 0) return false;
  return await scope.first().evaluate((element, header) => {
    const rect = element.getBoundingClientRect();
    const sticky = document.querySelector(header)?.getBoundingClientRect().bottom ?? 0;
    return (
      rect.height > 0 &&
      rect.top >= Math.min(sticky, 0) - 1 &&
      rect.top <= window.innerHeight &&
      rect.left >= -1 &&
      rect.right <= window.innerWidth + 1
    );
  }, SELECTORS.identityBlock);
}

/** Whether the caret is in a node's own text: the focus is inside an editable inside a row, which
 *  is what the edit key does and what Escape leaves (c-keyboard). */
export function caretInNode(page: Page): Promise<boolean> {
  return page.evaluate((selectors) => {
    const editable = document.activeElement?.closest('[contenteditable="true"]') ?? null;
    return editable !== null && editable.closest(selectors.nodeRow) !== null;
  }, SELECTORS);
}

/** The node id the tree's `aria-activedescendant` names, for a tree that roves with it rather
 *  than by moving the focus itself. Null when the tree names none. */
export function treeActiveDescendant(page: Page): Promise<string | null> {
  return page.evaluate((selectors) => {
    const tree = document.querySelector(`${selectors.issuePage} ${selectors.tree}`);
    const named = tree?.getAttribute("aria-activedescendant") ?? null;
    if (named === null) return null;
    return (
      document.getElementById(named)?.closest(selectors.nodeRow)?.getAttribute("data-node-id") ??
      null
    );
  }, SELECTORS);
}

/** The name of the preset the page is on: the selected tab's own name at a width that draws the
 *  row, and the one control's name below it. Null while the page draws neither. */
export function activePreset(page: Page): Promise<string | null> {
  return page.evaluate((selectors) => {
    const row = document.querySelector(selectors.presetRow);
    if (row === null) return null;
    const named = (element: Element): string => {
      const label = element.getAttribute("aria-label");
      if (label !== null) return label.trim();
      const copy = element.cloneNode(true) as Element;
      for (const count of copy.querySelectorAll(selectors.presetCount)) count.remove();
      return (copy.textContent ?? "").replace(/\s+/g, " ").trim();
    };
    const selected = row.querySelector('[role="tab"][aria-selected="true"]');
    if (selected !== null) return named(selected);
    const chooser = row.querySelector('button[aria-haspopup="menu"]');
    return chooser === null ? null : named(chooser);
  }, SELECTORS);
}

/** What the page says in its own words: the text of each `[data-page-chrome]` element with every
 *  `[data-author-text]` descendant removed, which is what c-plain-words reads word by word. */
export function chromeTexts(page: Page): Promise<string[]> {
  return page.evaluate((selectors) => {
    const found: string[] = [];
    for (const element of document.querySelectorAll(
      `${selectors.issuePage} ${selectors.pageChrome}`
    )) {
      const copy = element.cloneNode(true) as Element;
      for (const authored of copy.querySelectorAll(selectors.authorText)) authored.remove();
      const text = (copy.textContent ?? "").replace(/\s+/g, " ").trim();
      if (text !== "") found.push(text);
    }
    return found;
  }, SELECTORS);
}

/** Every element of the page that carries visible text and no `[data-page-chrome]` or
 *  `[data-author-text]` ancestor or self, which is text the page divides into neither. */
export function undividedTexts(page: Page): Promise<string[]> {
  return page.evaluate((selectors) => {
    const found: string[] = [];
    const walker = document.createTreeWalker(
      document.querySelector(selectors.issuePage) ?? document.createElement("div"),
      NodeFilter.SHOW_TEXT
    );
    for (let node = walker.nextNode(); node !== null; node = walker.nextNode()) {
      const text = (node.textContent ?? "").replace(/\s+/g, " ").trim();
      if (text === "") continue;
      const element = node.parentElement;
      if (element === null) continue;
      if (element.closest(`${selectors.pageChrome}, ${selectors.authorText}`) !== null) continue;
      if (element.getClientRects().length === 0) continue;
      found.push(text);
    }
    return found;
  }, SELECTORS);
}

/** Every `title` attribute the page carries, which is every tooltip a pointer can raise. */
export function tooltipTexts(page: Page): Promise<string[]> {
  return page.evaluate((selectors) => {
    return [...document.querySelectorAll(`${selectors.issuePage} [title]`)].map(
      (element) => element.getAttribute("title") ?? ""
    );
  }, SELECTORS);
}

/** The whole visible text of the tree, for a reading taken before and after a pointer crosses it. */
export function treeText(page: Page): Promise<string> {
  return page.evaluate((selectors) => {
    const tree = document.querySelector(`${selectors.issuePage} ${selectors.tree}`);
    return ((tree?.textContent ?? "") as string).replace(/\s+/g, " ").trim();
  }, SELECTORS);
}

/** Where the sticky identity block ends, which is the line the focused row must clear. */
export function identityBlockBottom(page: Page): Promise<number | null> {
  return page.evaluate((selectors) => {
    const header = document.querySelector(selectors.identityBlock);
    if (header === null) return null;
    const rect = header.getBoundingClientRect();
    return getComputedStyle(header).position === "sticky" || rect.top <= 1 ? rect.bottom : 0;
  }, SELECTORS);
}

/** One element that reaches past the box it is drawn in. */
export interface Overflowing {
  /** The box's own description: its tag and its node id where it has one. */
  readonly card: string;
  /** How far past the card the element reaches, in CSS pixels. */
  readonly over: number;
  readonly what: string;
}

/** Which boxes a measurement is read against: a node's row, or the body it opens. */
export type CardScope = "bodies" | "rows";

/**
 * Every element whose painted box reaches more than a pixel past the card it sits in. A card is a
 * node's row or its body: c-checks-readable reads "no element's box reaches past its card"
 * against both.
 */
export function boxesPastTheirCard(page: Page, cards: CardScope): Promise<Overflowing[]> {
  return page.evaluate(
    ({ cards, selectors }) => {
      const scope = cards === "rows" ? selectors.nodeRow : selectors.nodeBody;
      const describe = (element: Element): string => {
        const id = element.closest(selectors.nodeRow)?.getAttribute("data-node-id") ?? "";
        const own = (element.textContent ?? "").replace(/\s+/g, " ").trim().slice(0, 40);
        return `${element.tagName.toLowerCase()}${id === "" ? "" : ` in ${id}`} "${own}"`;
      };
      const found: { card: string; over: number; what: string }[] = [];
      for (const card of document.querySelectorAll(`${selectors.issuePage} ${scope}`)) {
        const bounds = card.getBoundingClientRect();
        if (bounds.width === 0) continue;
        for (const element of card.querySelectorAll("*")) {
          const rect = element.getBoundingClientRect();
          if (rect.width === 0 && rect.height === 0) continue;
          const over = Math.max(bounds.left - rect.left, rect.right - bounds.right);
          if (over > 1) found.push({ card: describe(card), over, what: describe(element) });
        }
      }
      return found;
    },
    { cards, selectors: SELECTORS }
  );
}

/** Two pieces of text drawn over each other. */
export interface Overlap {
  readonly first: string;
  readonly second: string;
}

/**
 * Every pair of the page's own text boxes that share a point on screen, inside `scope`. Only a
 * leaf that carries text of its own is measured, so a box that holds another is not read as
 * overlapping it.
 */
export function overlappingTexts(page: Page, cards: CardScope): Promise<Overlap[]> {
  return page.evaluate(
    ({ cards, selectors }) => {
      const scope = cards === "rows" ? selectors.nodeRow : selectors.nodeBody;
      const boxes: { label: string; rect: DOMRect }[] = [];
      for (const root of document.querySelectorAll(`${selectors.issuePage} ${scope}`)) {
        for (const element of root.querySelectorAll("*")) {
          if (element.children.length > 0) continue;
          const text = (element.textContent ?? "").replace(/\s+/g, " ").trim();
          if (text === "") continue;
          const rect = element.getBoundingClientRect();
          if (rect.width === 0 || rect.height === 0) continue;
          boxes.push({ label: `"${text.slice(0, 40)}"`, rect });
        }
      }
      const found: { first: string; second: string }[] = [];
      for (let i = 0; i < boxes.length; i += 1) {
        for (let j = i + 1; j < boxes.length; j += 1) {
          const a = boxes[i];
          const b = boxes[j];
          if (a === undefined || b === undefined) continue;
          const apart =
            a.rect.right <= b.rect.left + 0.5 ||
            b.rect.right <= a.rect.left + 0.5 ||
            a.rect.bottom <= b.rect.top + 0.5 ||
            b.rect.bottom <= a.rect.top + 0.5;
          if (!apart) found.push({ first: a.label, second: b.label });
        }
      }
      return found;
    },
    { cards, selectors: SELECTORS }
  );
}

/** The kind each gap the page names carries, in document order: a gap with no kind reads as the
 *  empty string, so an unnamed gap is named in the failure rather than dropped. */
export function gapKinds(page: Page): Promise<string[]> {
  return page.evaluate(
    (selectors) =>
      [...document.querySelectorAll(`${selectors.issuePage} ${selectors.gap}`)].map(
        (gap) => gap.getAttribute("data-gap-kind") ?? ""
      ),
    SELECTORS
  );
}

/** Everything the page shows, as a reader sees it: the rendered text of the page root, which
 *  leaves out what is hidden. A property drawn behind a control a reader has to open is not in
 *  it, which is what c-properties reads each value against. */
export function visibleText(page: Page): Promise<string> {
  return page.evaluate((selectors) => {
    const root = document.querySelector(selectors.issuePage);
    return root === null ? "" : (root as HTMLElement).innerText.replace(/\s+/g, " ").trim();
  }, SELECTORS);
}

/** The three things inside a node's own text that may scroll sideways (R17): a code block, a
 *  table and a picture. c-reflow names them by selector and leaves them out of its walk. */
export const AUTHOR_SCROLLERS = ["pre", "table", "img"] as const;

/** Each of those three that is drawn outside a `[data-author-text]` element, which is where the
 *  exception does not reach. */
export function scrollersOutsideAuthorText(page: Page): Promise<string[]> {
  return page.evaluate(
    ({ scrollers, selectors }) => {
      const found: string[] = [];
      for (const kind of scrollers) {
        for (const element of document.querySelectorAll(`${selectors.issuePage} ${kind}`)) {
          if (element.closest(selectors.authorText) !== null) continue;
          if (element.getClientRects().length === 0) continue;
          found.push(`${kind} in ${element.parentElement?.tagName.toLowerCase() ?? "?"}`);
        }
      }
      return found;
    },
    { scrollers: AUTHOR_SCROLLERS, selectors: SELECTORS }
  );
}

/** Every row of the tree that holds none of those three, which is the set c-reflow walks with
 *  `assertWhole`. */
export async function rowsWithoutAuthorScrollers(page: Page): Promise<Locator[]> {
  const rows = await drawnRows(page);
  const held = await page.evaluate(
    ({ scrollers, selectors }) =>
      [...document.querySelectorAll(`${selectors.issuePage} ${selectors.nodeRow}`)].map(
        (row) => row.querySelector(scrollers.join(", ")) !== null
      ),
    { scrollers: AUTHOR_SCROLLERS, selectors: SELECTORS }
  );
  const walked: Locator[] = [];
  rows.forEach((row, index) => {
    if (held[index] === true || !row.visible) return;
    walked.push(nodeRows(page).nth(index));
  });
  return walked;
}

/** How many panels the page draws right of its main column: every element that holds a margin
 *  section or the focused node's facts, counted by the box it sits in, so properties drawn in a
 *  second column are a second panel. */
export function panelsRightOfMain(page: Page): Promise<string[]> {
  return page.evaluate((selectors) => {
    const main = document.querySelector("main");
    const tree = document.querySelector(`${selectors.issuePage} ${selectors.tree}`);
    const left = tree?.getBoundingClientRect() ?? main?.getBoundingClientRect() ?? null;
    if (left === null) return [];
    const panels = new Set<string>();
    // The margin's own ask cards carry `data-margin-section` as well, with values of their own
    // (`packages/dispatch/AGENTS.md`), so only the three sections the issue page names count.
    const holders = document.querySelectorAll(
      '[data-margin-section="development"], [data-margin-section="people"], [data-margin-section="references"], [data-testid="focused-node-facts"]'
    );
    for (const holder of holders) {
      const rect = holder.getBoundingClientRect();
      if (rect.width === 0) continue;
      const column = Math.round(rect.left);
      panels.add(
        rect.left >= left.right - 1 ? `column at ${column}` : `inside the main column at ${column}`
      );
    }
    return [...panels];
  }, SELECTORS);
}

// ---------------------------------------------------------------------------------------------
// Opening the page.
// ---------------------------------------------------------------------------------------------

/**
 * Opens one address of the issue page and waits for the identity block, which the old page and
 * the redesigned one both draw: the wait is what keeps a test's first failure an assertion of its
 * own rather than a navigation that never settles. The keymap binds only once sign-in resolves,
 * so a key pressed before this returns reaches no handler.
 */
export async function gotoIssue(page: Page, path: string): Promise<void> {
  await page.goto(path);
  await expect(identityBlock(page)).toBeVisible();
}

/** Opens `preset` on `key` at its own address. */
export async function openPreset(page: Page, key: string, preset: Preset): Promise<void> {
  await gotoIssue(page, presetAddress(key, preset));
}
