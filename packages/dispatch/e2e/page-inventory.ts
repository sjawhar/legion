import { expect, type Locator, type Page } from "@playwright/test";

import type { Box } from "./layout";
import { assertPageFits } from "./unclipped";

/**
 * What a page offers a reader, recorded before a redesign and asserted after it: every landmark
 * and every control, each as the role it exposes and the name a screen reader announces for it.
 *
 * The walk is Playwright's own accessibility snapshot (`page.ariaSnapshotJSON`), so a name is the
 * one Playwright computes from `aria-label`, `aria-labelledby`, the element's own text, `title`
 * and `alt`, which is the name `getByRole(role, { name })` resolves and the name a screen reader
 * reads. Nothing here extracts DOM text by hand, and nothing here knows anything about the
 * redesign: this module records what is on a page and asserts that a later page still carries it.
 *
 * The snapshot leaves out what no reader can reach - an element with `display: none`, `hidden`,
 * or `aria-hidden` - and renders only one view of the page at a time, so a page that hides part
 * of itself behind a tab is recorded one view at a time and the views merged (`mergeInventory`).
 */
export interface InventoryEntry {
  /** The accessible name, with every server-generated uuid in it written as `<id>` (`collect`),
   *  and empty for a control that exposes no name at all. */
  readonly name: string;
  readonly role: string;
}

/** The landmarks an inventory records: the regions a reader navigates a page by. */
export const INVENTORY_LANDMARKS = [
  "banner",
  "complementary",
  "contentinfo",
  "form",
  "main",
  "navigation",
  "region",
  "search",
] as const;

/** The controls an inventory records: everything a reader can act on. */
export const INVENTORY_CONTROLS = [
  "button",
  "checkbox",
  "combobox",
  "link",
  "menuitem",
  "option",
  "radio",
  "searchbox",
  "slider",
  "spinbutton",
  "switch",
  "tab",
  "textbox",
] as const;

const inventoryRoles: ReadonlySet<string> = new Set<string>([
  ...INVENTORY_LANDMARKS,
  ...INVENTORY_CONTROLS,
]);

/** One entry with where it sits, which is how reachability is read. A node the snapshot gave no
 *  box for is recorded with none and only its presence is asserted. */
interface MeasuredEntry extends InventoryEntry {
  readonly box?: Box;
}

/** `page.ariaSnapshotJSON` answers a free-form value, so every field is read defensively. */
interface AriaSnapshotNode {
  readonly box?: unknown;
  readonly children?: unknown;
  readonly name?: unknown;
  readonly role?: unknown;
}

function readBox(value: unknown): Box | undefined {
  if (typeof value !== "object" || value === null) return undefined;
  const { height, width, x, y } = value as Record<string, unknown>;
  if (
    typeof height !== "number" ||
    typeof width !== "number" ||
    typeof x !== "number" ||
    typeof y !== "number"
  ) {
    return undefined;
  }
  return { height, width, x, y };
}

/**
 * What a name carries that belongs to the run rather than to the page, and what it is recorded
 * as. A control names the row it acts on - `Copy reference dispatch://LAY-2/comment/<id>`, `2
 * replies · last reply <time>` - so its name holds the uuid the server gave that row and the
 * relative time `Timestamp` draws for it (`web/src/features/refs/Timestamp.tsx`), and neither is a
 * property of the page: the uuid is new in every run, and the time crosses from `just now` to `1
 * minute ago` while the recording is still being made. Recording them as placeholders is what
 * makes two runs of one page give the same list.
 */
const runSpecific: readonly (readonly [RegExp, string])[] = [
  [/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/g, "<id>"],
  [
    /just now|yesterday|tomorrow|(?:last|next) (?:week|month|year)|(?:in )?\d+ (?:second|minute|hour|day|week|month|year)s?(?: ago)?/g,
    "<time>",
  ],
];

/** Every landmark and control of one accessibility snapshot, in the order the tree holds them. */
function collect(node: unknown, found: MeasuredEntry[]): void {
  if (Array.isArray(node)) {
    for (const child of node) collect(child, found);
    return;
  }
  if (typeof node !== "object" || node === null) return;
  const { box, children, name, role } = node as AriaSnapshotNode;
  if (typeof role === "string" && inventoryRoles.has(role)) {
    const announced =
      typeof name === "string"
        ? runSpecific.reduce(
            (text, [carried, placeholder]) => text.replace(carried, placeholder),
            name
          )
        : "";
    found.push({ box: readBox(box), name: announced, role });
  }
  collect(children, found);
}

/** The key two entries are the same control under: its role and its name. */
function entryKey(entry: InventoryEntry): string {
  return `${entry.role}\u0000${entry.name}`;
}

/** How an entry is written where a person reads it: in a failure, and as both halves of the
 *  fixture's rename table, so a rename is pasted straight from the failure that named it. */
function describe(entry: InventoryEntry): string {
  return `${entry.role} "${entry.name}"`;
}

/** The entry `described` names, read back from the form `describe` writes. A value that is not
 *  that form names no entry, and the caller fails rather than quietly looking for nothing. */
function entryOf(described: string): InventoryEntry | undefined {
  const opened = described.indexOf(' "');
  if (opened <= 0 || !described.endsWith('"')) return undefined;
  return { name: described.slice(opened + 2, -1), role: described.slice(0, opened) };
}

/**
 * One list out of however many views were recorded: every `{role, name}` once, ordered by role
 * and then by name, so the view order, the DOM order and two runs of the same page all give the
 * same list.
 */
export function mergeInventory(entries: readonly InventoryEntry[]): InventoryEntry[] {
  const byKey = new Map<string, InventoryEntry>();
  for (const entry of entries) {
    byKey.set(entryKey(entry), { name: entry.name, role: entry.role });
  }
  return [...byKey.values()].sort(
    (a, b) => a.role.localeCompare(b.role) || a.name.localeCompare(b.name)
  );
}

/** How long one reading waits before it is taken again, and how long the page has to stop. */
const SETTLE_INTERVAL_MS = 400;
const SETTLE_TIMEOUT_MS = 20_000;

/** Every landmark and every control the page offers, de-duplicated and ordered, once the page has
 *  stopped changing them. */
export async function recordInventory(page: Page): Promise<InventoryEntry[]> {
  return mergeInventory(await settled(() => measured(page.ariaSnapshotJSON({ boxes: true }))));
}

/** The same walk over one part of the page, which is how a view that keeps its other panels
 *  `hidden` records what one panel of it offers. */
export async function recordScopeInventory(scope: Locator): Promise<InventoryEntry[]> {
  return mergeInventory(await settled(() => measured(scope.ariaSnapshotJSON({ boxes: true }))));
}

/**
 * One reading of `read` once two readings `SETTLE_INTERVAL_MS` apart agree. A panel still filling
 * - a reference chip that arrives with its own read, a card the list mounts late - is on the page
 * after the network has gone quiet, so a single reading records a half-drawn page and two runs of
 * it differ. A page that never stops changing fails here rather than being recorded mid-render.
 */
async function settled(read: () => Promise<MeasuredEntry[]>): Promise<MeasuredEntry[]> {
  const deadline = Date.now() + SETTLE_TIMEOUT_MS;
  let before = mergeInventory(await read())
    .map(describe)
    .join("\n");
  for (;;) {
    const { promise, resolve } = Promise.withResolvers<void>();
    setTimeout(resolve, SETTLE_INTERVAL_MS);
    await promise;
    const entries = await read();
    const after = mergeInventory(entries).map(describe).join("\n");
    if (after === before) return entries;
    expect(
      Date.now() <= deadline,
      `what the page offers kept changing for ${SETTLE_TIMEOUT_MS} ms; it last went from\n${before}\nto\n${after}`
    ).toBe(true);
    before = after;
  }
}

async function measured(snapshot: Promise<unknown>): Promise<MeasuredEntry[]> {
  const found: MeasuredEntry[] = [];
  collect(await snapshot, found);
  return found;
}

export interface CarriedOptions {
  /**
   * The fixture's rename table: each recorded entry, written as the `role "name"` a failure
   * prints it as, mapped to the entry the page offers in its place, in the same form. One form
   * serves a name that changed and a role that changed with it - `tab "Children"` became
   * `button "Children"` when the tabs became presets - and a rename is pasted straight from
   * the failure that named it. Where the page offers a different entry at a different width -
   * the preset row is a row of tabs on a desktop and one menu on a phone - the value names
   * each of them, joined by ` | `, and the entry is carried while any one of them is there. A
   * renamed entry is looked for as the entries it maps to and nothing else, so a rename is not
   * reported as a loss; a value that is not that form fails the check rather than quietly
   * matching nothing.
   */
  readonly renames: Readonly<Record<string, string>>;
  /** Which recorded state this is (`open-1440`), named in every failure. */
  readonly state: string;
  /**
   * The page's own views, each a call that puts the page in one of them. The recording is the
   * merge of the views the old page had - its eight tab panels, one at a time, because a page
   * renders one and hides the rest (`record-page-inventory.e2e.ts`) - so the page it is read
   * against is merged the same way, or a page that still offers everything would be read as
   * having lost whatever its first view does not show. Each view is read in turn and the
   * readings merged, and every one of them passes `assertPageFits` on its own.
   */
  readonly views: readonly (() => Promise<void>)[];
}

/**
 * Nothing recorded was lost: every recorded `{role, name}`, under the name the rename table gives
 * it, is a landmark or control of the page as it stands, and a reader reaches each of them
 * without scrolling sideways - the page is no wider than its viewport (`assertPageFits`) and
 * every instance of the entry sits inside the viewport's own width, so nothing is reached by
 * scrolling a row or a card sideways.
 *
 * It asserts nothing about the redesign: a page may offer more than it did, in any order.
 */
export async function assertInventoryCarried(
  page: Page,
  recorded: readonly InventoryEntry[],
  options: CarriedOptions
): Promise<void> {
  // One row per recorded entry, each naming every entry that carries it: itself, or whatever
  // the rename table says stands in its place.
  const seen = new Set<string>();
  const wanted: { carried: InventoryEntry[]; described: string; instances: MeasuredEntry[] }[] = [];
  for (const entry of recorded) {
    const described = describe(entry);
    if (seen.has(described)) continue;
    seen.add(described);
    const renamed = options.renames[described];
    const carried =
      renamed === undefined ? [entry] : renamed.split(" | ").map((one) => entryOf(one));
    expect(
      carried.every((one) => one !== undefined),
      `${options.state}: the rename of ${described} reads ${JSON.stringify(renamed)}, which is no \`role "name"\``
    ).toBe(true);
    wanted.push({
      carried: carried.filter((one): one is InventoryEntry => one !== undefined),
      described,
      instances: [],
    });
  }
  const present = new Map<string, MeasuredEntry[]>();
  for (const view of options.views) {
    await view();
    await assertPageFits(page);
    for (const entry of await settled(() => measured(page.ariaSnapshotJSON({ boxes: true })))) {
      const key = entryKey(entry);
      present.set(key, [...(present.get(key) ?? []), entry]);
    }
  }
  for (const one of wanted) {
    one.instances = one.carried.flatMap((entry) => present.get(entryKey(entry)) ?? []);
  }
  const lost = wanted.filter((one) => one.instances.length === 0).map((one) => one.described);
  expect(lost, `${options.state} no longer offers: ${lost.join(", ")}`).toEqual([]);

  const viewport = await page.evaluate(() => document.documentElement.clientWidth);
  const sideways = wanted
    .filter((one) => {
      // A control a page hides from sight and keeps for the keyboard - the shell's "Skip to
      // content", which is `sr-only` until it is focused - is a 1 by 1 clipped box wherever it
      // sits. It is reached by Tab and not by scrolling, so its box says nothing about
      // scrolling sideways and the rule passes over it.
      const placed = one.instances.filter(
        (instance) =>
          instance.box !== undefined && instance.box.width > 1 && instance.box.height > 1
      );
      return (
        placed.length > 0 &&
        placed.every(({ box }) => box !== undefined && (box.x < 0 || box.x + box.width > viewport))
      );
    })
    .map((one) => one.described);
  expect(
    sideways,
    `${options.state} reaches these only by scrolling sideways in a ${viewport}px viewport: ${sideways.join(", ")}`
  ).toEqual([]);
}
