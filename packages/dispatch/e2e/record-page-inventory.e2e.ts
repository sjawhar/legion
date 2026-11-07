import { execFileSync } from "node:child_process";
import { readFile, writeFile } from "node:fs/promises";
import { fileURLToPath } from "node:url";

import { expect, type Page, test } from "@playwright/test";

import { patchIssue } from "./api";
import { seedWholeIssue } from "./issue-layout-seed";
import {
  INVENTORY_CONTROLS,
  type InventoryEntry,
  mergeInventory,
  recordInventory,
  recordScopeInventory,
} from "./page-inventory";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

/**
 * The recorder of what the issue page offered before dispatch://INFRA-1196 redrew it: every
 * landmark and every control of the whole issue (`seedWholeIssue`), in four states - open and
 * closed, at 1440x900 and at 390x844 CSS pixels - written to
 * `e2e/fixtures/issue-page-inventory.json`. The redesign's own check reads that fixture and
 * asserts the new page still carries all of it (`assertInventoryCarried`, c-nothing-lost), so
 * the fixture is the ground truth and has to be recorded against the page as it stood.
 *
 * **It is a recorder and no check of the page it runs against.** It reads the page through the
 * eight tabs that page had (`#issue-<tab>-tab`), which the redesign replaced with five presets
 * over one tree, so it runs in a worktree at the commit before the change - that is where those
 * tabs are - and `test.skip` leaves it alone everywhere else. Re-run it whenever the branch
 * takes its base again, because the base moves and the fixture describes the base's page.
 *
 * Two things it deliberately is not, so neither is decided again:
 *
 * - It is **not** repointed at the new page. A fixture recorded from the redesign would have
 *   c-nothing-lost compare the redesign against a recording of itself, which proves nothing.
 * - It is **not** retired in favour of c-nothing-lost. Retiring it would leave nothing able to
 *   record the fixture the next time the base moves.
 *
 * `RECORD_PAGE_INVENTORY=1` is what makes it run at all: without it the row skips, because the
 * page it reads is not the page the suite runs against. Any other value is refused rather than
 * read as "do not record", so a typed `RECORD_PAGE_INVENTORY=true` cannot silently skip the
 * recording a person asked for.
 *
 * The fixture names two commits, because a recording is made on a branch: `commit` is the
 * branch's base (`git merge-base HEAD origin/main`), which is the commit whose page the fixture
 * describes and the one a reader of the redesign compares against, and `recordedAtHead` is the
 * working copy's own commit, which carries the seeds and this spec and moves with every later
 * commit. The recorded page is the base's page only while nothing under
 * `packages/dispatch/web/src` differs between the two, so the spec asserts that: a recording
 * made on a branch that has already redrawn the page fails here rather than recording the new
 * page as the old ground truth.
 *
 * That page renders only the active tab's panel - every other panel is `hidden`, which the
 * accessibility snapshot leaves out - so a state's inventory is the merge of the eight tabs'
 * panels, visited in turn at each width, and each panel is asserted to offer a control of its
 * own.
 */

/** The eight tabs of the issue page, in the order `web/src/features/issue/IssueTabs.tsx` lists
 *  them. A tab's button and panel carry the DOM ids `Tabs.tsx`'s `tabButtonId` and
 *  `IssuePage.tsx` give them, which is how a tab is selected and its panel scoped here. */
const ISSUE_TABS = [
  "spec",
  "verification",
  "plan",
  "conversation",
  "children",
  "artifacts",
  "activity",
  "decisions",
] as const;

interface RecordedState {
  /** Whether the issue is closed in this state. */
  readonly closed: boolean;
  /** The fixture key this state is written under. */
  readonly key: "closed-1440" | "closed-390" | "open-1440" | "open-390";
  readonly viewport: { readonly height: number; readonly width: number };
}

/** The four states, open before closed: the issue is closed once, between the two pairs. */
const STATES: readonly RecordedState[] = [
  { closed: false, key: "open-1440", viewport: { height: 900, width: 1440 } },
  { closed: false, key: "open-390", viewport: { height: 844, width: 390 } },
  { closed: true, key: "closed-1440", viewport: { height: 900, width: 1440 } },
  { closed: true, key: "closed-390", viewport: { height: 844, width: 390 } },
];

interface PageInventoryFixture {
  /** The commit whose page this describes: the branch's base. */
  readonly commit: string;
  /** The working copy's own commit when it was recorded, which carries the recorder itself. */
  readonly recordedAtHead: string;
  readonly recordedAt: string;
  readonly "open-1440": readonly InventoryEntry[];
  readonly "open-390": readonly InventoryEntry[];
  readonly "closed-1440": readonly InventoryEntry[];
  readonly "closed-390": readonly InventoryEntry[];
  /** Filled by the redesign: each recorded entry, as the `role "name"` a failure prints, mapped
   *  to the entry the new page offers in its place, in the same form (`page-inventory.ts`). */
  readonly renames: Readonly<Record<string, string>>;
}

const fixturePath = fileURLToPath(new URL("./fixtures/issue-page-inventory.json", import.meta.url));

/** Whether this run records the fixture. The guard is read as an assertion: a value the spec does
 *  not understand fails the test instead of being taken for "check". */
function recording(): boolean {
  const guard = process.env.RECORD_PAGE_INVENTORY ?? "";
  expect(
    ["", "1"],
    `RECORD_PAGE_INVENTORY is "${guard}": set it to 1 to record the fixture, or leave it unset to check the page against it`
  ).toContain(guard);
  return guard === "1";
}

/** `git` in the checkout this spec lives in, as one line. */
function git(...args: readonly string[]): string {
  return execFileSync("git", args, {
    cwd: fileURLToPath(new URL(".", import.meta.url)),
    encoding: "utf8",
  }).trim();
}

function resolves(ref: string): boolean {
  try {
    git("rev-parse", "--verify", "--quiet", `${ref}^{commit}`);
    return true;
  } catch {
    return false;
  }
}

const controlRoles: readonly string[] = INVENTORY_CONTROLS;

/** How the recorder's own report names one control. */
function named(entry: InventoryEntry): string {
  return `${entry.role} "${entry.name}"`;
}

/**
 * One state's inventory: the page at that width, in that open or closed state, with every tab
 * visited in turn and what each offers merged into one list. Each panel is asserted to offer a
 * control of its own, so a tab whose panel never rendered fails here rather than being counted as
 * covered. A recording run also prints, for each panel, a control no other panel of that state
 * offers - the evidence that the state covers all eight - falling back on its first control when
 * every one of them is offered elsewhere too.
 */
async function recordState(
  page: Page,
  issueKey: string,
  state: RecordedState,
  report: boolean
): Promise<InventoryEntry[]> {
  await page.setViewportSize(state.viewport);
  await page.goto(`/issues/${issueKey}/spec`);
  await expect(page.getByTestId("issue-header")).toBeVisible();
  const found: InventoryEntry[] = [];
  const panels = new Map<string, readonly InventoryEntry[]>();
  for (const tab of ISSUE_TABS) {
    const button = page.locator(`#issue-${tab}-tab`);
    await button.click();
    await expect(button).toHaveAttribute("aria-selected", "true");
    await page.waitForLoadState("networkidle");
    found.push(...(await recordInventory(page)));
    const panel = await recordScopeInventory(page.locator(`#issue-${tab}-panel`));
    const controls = panel.filter((entry) => controlRoles.includes(entry.role));
    expect(
      controls[0],
      `the ${tab} panel of ${state.key} offers no control at all, so the state covers none of it`
    ).toBeDefined();
    panels.set(tab, controls);
  }
  if (report) {
    for (const [tab, controls] of panels) {
      const elsewhere = new Set(
        [...panels].filter(([other]) => other !== tab).flatMap(([, entries]) => entries.map(named))
      );
      const own = controls.find((entry) => !elsewhere.has(named(entry)));
      const shown = own ?? controls[0];
      console.log(
        `${state.key} ${tab}: ${controls.length} controls, ${shown === undefined ? "none" : named(shown)}${own === undefined ? " (shared)" : ""}`
      );
    }
  }
  return mergeInventory(found);
}

test.beforeEach(async () => {
  await resetDatabase();
});

test("the issue page's landmarks and controls, open and closed, at 1440 and at 390", async ({
  browser,
}) => {
  // A recorder, not a check: it reads the page through the eight tabs the commit before the
  // redesign had, so it runs only when a person asks it to record (the header above).
  test.skip(!recording(), "set RECORD_PAGE_INVENTORY=1, in a worktree at the branch's base");
  test.setTimeout(600_000);
  // The branch's base is the page this records; the working copy's own commit carries the
  // recorder, and the two are the same page only while `packages/dispatch/web/src` is untouched
  // between them, which the write below asserts.
  const commit = git("merge-base", "HEAD", resolves("origin/main") ? "origin/main" : "main");
  const recordedAtHead = git("rev-parse", "HEAD");
  expect(commit, "git merge-base answered no commit").toMatch(/^[0-9a-f]{40}$/);
  expect(recordedAtHead, "git rev-parse HEAD answered no commit").toMatch(/^[0-9a-f]{40}$/);
  const seeded = await seedWholeIssue();

  const alice = await asUser(browser, "alice");
  const recorded = new Map<RecordedState["key"], InventoryEntry[]>();
  try {
    const page = await alice.newPage();
    // The issue is closed once, between the two open states and the two closed ones: a second
    // close is refused (`ISSUE_CLOSED`).
    let closed = false;
    for (const state of STATES) {
      if (state.closed && !closed) {
        await patchIssue(seeded.key, { status: "done" });
        closed = true;
      }
      recorded.set(state.key, await recordState(page, seeded.key, state, true));
    }
  } finally {
    await alice.close();
  }

  const states = {
    "closed-1440": recorded.get("closed-1440") ?? [],
    "closed-390": recorded.get("closed-390") ?? [],
    "open-1440": recorded.get("open-1440") ?? [],
    "open-390": recorded.get("open-390") ?? [],
  };

  const redrawn = git("diff", "--name-only", commit, recordedAtHead)
    .split("\n")
    .filter((path) => path.startsWith("packages/dispatch/web/src/"));
  expect(
    redrawn,
    `${recordedAtHead} redraws the page ${commit} holds, so this run would record the new page as the old ground truth: ${redrawn.join(", ")}`
  ).toEqual([]);
  const fixture: PageInventoryFixture = {
    commit,
    recordedAtHead,
    recordedAt: new Date().toISOString(),
    "open-1440": states["open-1440"],
    "open-390": states["open-390"],
    "closed-1440": states["closed-1440"],
    "closed-390": states["closed-390"],
    renames: {},
  };
  await writeFile(fixturePath, `${JSON.stringify(fixture, null, 2)}\n`);
  const written = JSON.parse(await readFile(fixturePath, "utf8")) as PageInventoryFixture;
  expect(written.commit, "the fixture names another commit than the page it describes").toBe(
    commit
  );
  expect(written.recordedAtHead, "the fixture names another working copy").toBe(recordedAtHead);
  for (const state of STATES) {
    expect(written[state.key], `the fixture holds another ${state.key} than was read`).toEqual(
      states[state.key]
    );
  }
});
