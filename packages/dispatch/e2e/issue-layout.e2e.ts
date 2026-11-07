import { readFile } from "node:fs/promises";
import { fileURLToPath } from "node:url";

import { type Browser, expect, type Page, test } from "@playwright/test";

import {
  CHECK_WHEN_WORDS,
  CONTROL_LABELS,
  DECLARED_WORDS,
  EMPTY_STATE_LINES,
  GLYPH_NAMES,
  GLYPH_TOKENS,
  NODE_STATE_WORDS,
  NODE_TYPE_NAMES,
  type NodeType,
  nodeStateWords,
  PRESET_NAMES,
  PRESET_ORDER,
  type Preset,
  wordsIn,
} from "../web/src/features/issue/page-words";
import { getUnsubscribeCalls } from "./agents";
import {
  answerAsk,
  claimIssue,
  createProjectDocument,
  getArtifact,
  getArtifactVersion,
  getAsk,
  getIssue,
  getIssueArtifactText,
  getMyState,
  listComments,
  patchIssue,
  requestApproval,
} from "./api";
import {
  seedFrontierDeep,
  seedFrontierShallow,
  seedNoChildren,
  seedNoSummary,
  seedRequestBaseline,
  seedSpineCases,
  seedTwiceTheNodes,
  seedWholeIssue,
} from "./issue-layout-seed";
import { asPlanner } from "./issue-layout-seed-support";
import {
  checkBlockId,
  OPEN_QUESTION_BLOCK_ID,
  planTaskBlockId,
  requirementBlockId,
  requirementText,
  SPINE_CHECK_BLOCK_ID,
  SPINE_REQUIREMENTS,
  SUMMARY_SENTENCES,
} from "./issue-layout-seed-text";
import {
  activePreset,
  addControl,
  ancestorsOfRow,
  blockNodeId,
  boxesPastTheirCard,
  branchMoreRows,
  checksRow,
  childRow,
  childRows,
  childrenBar,
  childrenDirectCount,
  childrenDisclosure,
  childrenSection,
  childrenSubtreeCount,
  chromeTexts,
  conversationStateControl,
  copyCommandControls,
  type DrawnRow,
  drawnRows,
  filterBox,
  focusedNodeFacts,
  focusedNodeId,
  focusThePage,
  gapKinds,
  gaps,
  gotoIssue,
  identityBlock,
  identityBlockBottom,
  issuePage,
  marginHideControl,
  marginResizeHandle,
  marginSection,
  marginSectionDisclosure,
  marginShell,
  marginShowControl,
  marginSlot,
  nodeBodies,
  nodeCommands,
  nodeMenuButton,
  nodeMenuItems,
  nodeRow,
  nodeRows,
  offered,
  openMenu,
  openPreset,
  optionsOf,
  overlappingTexts,
  pageStatus,
  panelsRightOfMain,
  parentOfRow,
  pinnedBodies,
  planRow,
  presetAddress,
  presetChooser,
  presetCount,
  presetMenuItem,
  presetRow,
  presetTab,
  presetTablist,
  presetTabs,
  presetToolbar,
  rowAuthor,
  rowBody,
  rowGaps,
  rowPictures,
  rowsOfType,
  rowsWithoutAuthorScrollers,
  scrollersOutsideAuthorText,
  threeThings,
  threeThingsSentences,
  tooltipTexts,
  tree,
  treeText,
  undividedTexts,
  uploadInput,
  visibleText,
} from "./issue-page";
import { assertInventoryCarried, type InventoryEntry } from "./page-inventory";
import { resetDatabase } from "./seed";
import { watchApiRequests } from "./traffic";
import { assertPageFits, assertWhole, assertWholeAcross } from "./unclipped";
import { asUser } from "./users";

/**
 * The redesigned issue page (dispatch://INFRA-1196), one test per `check` block of
 * `dispatch://INFRA-1196/artifact/verification-md`, each named by that check's own `-g` string.
 * Every test reads the page through `e2e/issue-page.ts`, which is the one statement of what the
 * page's DOM is, and spells no selector of its own.
 *
 * Four checks are partial on today's data, and each test says where it stops:
 *
 * - **c-tree** proves the fold at the depth today's data reaches (issue, document, block); the
 *   four-levels-down seed waits on dispatch://INFRA-1290.
 * - **c-three-things** proves the three sentences and the two rows; who wrote the bullets and
 *   when they were last read back wait on dispatch://INFRA-1278, so the test asserts the page
 *   names that gap and never that an author is drawn.
 * - **c-home** proves the Spec preset over today's documents, where every requirement sits at the
 *   issue root because `from` waits on dispatch://INFRA-1098 item 21.
 * - **c-spine** proves what a document's own `serves` gives, because the model holds no `from`
 *   edge to seed.
 *
 * A seed makes the project `LAY`, and a second `POST /api/v1/projects` for it answers
 * `409 PROJECT_EXISTS`, so a test that reads two seeded issues runs each in a `phase` of its own:
 * the reset truncates `user_sessions`, so each phase signs in again (`packages/dispatch/AGENTS.md`,
 * "End-to-end tests").
 */

/** The two sizes every check names. */
const DESKTOP = { height: 900, width: 1440 } as const;
const PHONE = { height: 844, width: 390 } as const;

/** The title the fake Envoy gives the session that holds the claim and takes the route. */
const PLANNER_TITLE = "Planner (issue layout)";

/** One seeded issue, read by one signed-in page. The viewport starts at the desktop size in every
 *  project, because the `iphone` project runs each `@phone` test too and a test that asserts a
 *  desktop width sets it itself. */
async function phase<T>(
  browser: Browser,
  seed: () => Promise<T>,
  body: (page: Page, seeded: T) => Promise<void>
): Promise<void> {
  await resetDatabase();
  const seeded = await seed();
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.setViewportSize({ height: DESKTOP.height, width: DESKTOP.width });
    await body(page, seeded);
  } finally {
    await context.close();
  }
}

/** Fails unless the redesigned page is on screen, so every assertion after it reads the new page
 *  and not whatever else answered the address. Both readings are web-first, so a check made
 *  straight after a reload waits for the page's own chunk and its reads instead of racing them:
 *  the shell paints the frame from the address before the page is mounted (`page-frame.tsx`). */
async function expectIssuePageDrawn(page: Page): Promise<void> {
  await expect(
    issuePage(page),
    "the page draws no [data-issue-page] root, so this is not the redesigned issue page"
  ).toHaveCount(1);
  await expect(
    tree(page),
    'the page draws no role="tree" named "Issue tree" inside [data-issue-page]'
  ).toHaveCount(1);
}

/** The slug of the issue's own spec, which is how its blocks are addressed. */
async function primarySlug(key: string): Promise<string> {
  const primary = (await getIssue(key)).artifacts.find((artifact) => artifact.primary);
  expect(primary, `${key} carries no primary document`).toBeDefined();
  return primary?.slug ?? "";
}

function rowOf(rows: readonly DrawnRow[], id: string): DrawnRow | undefined {
  return rows.find((row) => row.id === id);
}

function idsOf(rows: readonly DrawnRow[]): string[] {
  return rows.map((row) => row.id ?? "<no id>");
}

/**
 * Every row carries its type's word and its type's shape, its state word and its state's shape
 * where the model holds a state, and nothing where it holds none. Every word and token comes from
 * `page-words.ts`, so a type or state the page draws without declaring one fails here.
 */
function expectRowVocabulary(rows: readonly DrawnRow[]): void {
  for (const row of rows) {
    const type = row.type ?? "";
    expect(
      Object.keys(NODE_TYPE_NAMES),
      `row ${row.id} carries data-node-type "${type}", which the model does not name`
    ).toContain(type);
    const typeName = NODE_TYPE_NAMES[type as NodeType];
    expect(row.text.toLowerCase(), `row ${row.id} does not say it is a ${typeName}`).toContain(
      typeName
    );
    const typeGlyph = GLYPH_TOKENS.types[type as NodeType];
    expect(
      row.glyphs.map((glyph) => glyph.token),
      `row ${row.id} draws no shape for its type (${typeGlyph})`
    ).toContain(typeGlyph);
    for (const glyph of row.glyphs) {
      expect(glyph.name, `the ${glyph.token} shape on row ${row.id} has no accessible name`).toBe(
        GLYPH_NAMES[glyph.token]
      );
    }

    const states = nodeStateWords(type as NodeType);
    if (states === undefined) {
      expect(
        row.state,
        `row ${row.id} is a ${typeName}, which the model gives no state, and it carries data-node-state "${row.state}"`
      ).toBeNull();
      expect(row.stateWord, `row ${row.id} draws a state word for a ${typeName}`).toBeNull();
      continue;
    }
    const state = row.state ?? "";
    expect(
      Object.keys(states),
      `row ${row.id} is a ${typeName} carrying data-node-state "${state}", which is no state of that type`
    ).toContain(state);
    expect(row.stateWord, `row ${row.id} draws no word for its state ${state}`).toBe(states[state]);
    const stateGlyphs: Readonly<Record<string, Readonly<Record<string, string>>>> =
      GLYPH_TOKENS.states;
    const stateGlyph = stateGlyphs[type]?.[state];
    expect(
      row.glyphs.map((glyph) => glyph.token),
      `row ${row.id} draws no shape for its state (${stateGlyph})`
    ).toContain(stateGlyph);
  }
}

/** The word and the shape each state is drawn with, one entry per state: a state told apart by
 *  colour alone collides with another here. */
function statesDrawn(rows: readonly DrawnRow[]): Map<string, string> {
  const drawn = new Map<string, string>();
  for (const row of rows) {
    if (row.state === null) continue;
    const token = row.glyphs.map((glyph) => glyph.token).join("+");
    drawn.set(`${row.type}:${row.state}`, `${row.stateWord} ${token}`);
  }
  return drawn;
}

/** A first line that ends inside a word: the stored text carries on without a break where the
 *  drawn line stops. */
function cutsAWord(stored: string, drawn: string): boolean {
  const line = drawn.replace(/\s*(?:…|\.\.\.)$/u, "").trimEnd();
  if (!stored.startsWith(line) || stored === line) return false;
  return !/\s/.test(stored.charAt(line.length)) && !/[.!?]$/.test(line);
}

test.beforeEach(async () => {
  await resetDatabase();
});

// c-tree. Partial on today's data: `document_blocks` holds no parent, so the tree reaches two
// levels (issue, document, block) and the four-levels-down seed is dispatch://INFRA-1290's. What
// is proved here is the fold itself, at the depth the data reaches, on both frontier seeds.
test("the body is one folded tree whose rows never truncate a word", async ({ browser }) => {
  await phase(browser, seedFrontierShallow, async (page, seeded) => {
    await gotoIssue(page, `/issues/${seeded.key}`);
    const specSlug = await primarySlug(seeded.key);

    const rows = await drawnRows(page);
    expect(
      rows.length,
      `the body is not one folded tree: ${seeded.key} draws no node row at all`
    ).toBeGreaterThan(0);
    await expectIssuePageDrawn(page);
    expect(
      rows.filter((row) => row.role !== "treeitem").map((row) => row.id),
      "these rows of the tree are no treeitem"
    ).toEqual([]);
    expectRowVocabulary(rows);

    // No row truncates a word: each first line is the start of the node's own text and the text
    // carries on at a word break, or the line ends a sentence.
    for (const row of rows) {
      if (row.firstLine === null) continue;
      const line = row.firstLine.replace(/\s*(?:…|\.\.\.)$/u, "").trimEnd();
      expect(
        line.length,
        `row ${row.id} draws a first line of ${line.length} characters`
      ).toBeLessThanOrEqual(100);
      expect(row.text, `row ${row.id}'s first line is not its own text`).toContain(line);
    }
    const long = rowOf(rows, blockNodeId(specSlug, requirementBlockId(1)));
    expect(long, "the two-sentence requirement draws no row").toBeDefined();
    expect(
      cutsAWord(requirementText(1), long?.firstLine ?? ""),
      `the first line "${long?.firstLine}" cuts the stored text inside a word`
    ).toBe(false);

    // The folded branch: one document row, folded, counting what it holds, none of it drawn.
    const branch = rowOf(rows, seeded.foldedBranch.slug);
    expect(branch, `${seeded.key} draws no row for the folded document`).toBeDefined();
    expect(branch?.expanded, "the folded branch is not marked folded").toBe("false");
    expect(branch?.foldedCount, "the folded branch does not count what it holds").toBe(
      String(seeded.foldedBranch.blockIds.length)
    );
    expect(
      seeded.foldedBranch.blockIds.filter(
        (block) => rowOf(rows, blockNodeId(seeded.foldedBranch.slug, block)) !== undefined
      ),
      "the folded branch draws its blocks while it is folded"
    ).toEqual([]);

    // The frontier: the open question one level below the issue is drawn, unfolded to, with
    // nothing unfolded by hand.
    const question = rowOf(rows, seeded.askId);
    expect(question, `${seeded.key} draws no row for its open question`).toBeDefined();
    expect(question?.visible, "the open question's row is not drawn").toBe(true);
    expect(question?.level, "the open question does not sit one level below the issue").toBe(
      String(seeded.depth + 1)
    );

    // The focused node's body holds the whole text, which the first line is only the start of.
    const first = nodeRow(page, blockNodeId(specSlug, requirementBlockId(1))).first();
    await first.focus();
    expect(await rowBody(first).count(), "the focused row draws no body").toBe(1);
    expect(
      (await rowBody(first).innerText()).replace(/\s+/g, " "),
      "the focused node's body does not hold the node's whole stored text"
    ).toContain(requirementText(1));
  });

  await phase(browser, seedFrontierDeep, async (page, seeded) => {
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    const specSlug = await primarySlug(seeded.key);
    const question = blockNodeId(specSlug, OPEN_QUESTION_BLOCK_ID);

    const rows = await drawnRows(page);
    expectRowVocabulary(rows);
    const row = rowOf(rows, question);
    expect(row, "the open question as deep as the model reaches draws no row").toBeDefined();
    expect(row?.visible, "the open question's row is not drawn").toBe(true);
    expect(row?.level, "the open question does not sit two levels below the issue").toBe(
      String(seeded.depth + 1)
    );
    for (const ancestor of await ancestorsOfRow(page, question)) {
      expect(
        rowOf(rows, ancestor)?.expanded,
        `the question's ancestor ${ancestor} is folded, so its path was not unfolded`
      ).toBe("true");
    }

    // Every other branch is folded to its first level with a count of what it holds, and no
    // branch is deeper than the drawn levels on today's data.
    expect(
      rowOf(rows, seeded.foldedBranch.slug)?.foldedCount,
      "the other branch does not count what it holds"
    ).toBe(String(seeded.foldedBranch.blockIds.length));
    expect(
      await branchMoreRows(page).count(),
      "a branch is drawn as deeper than the three levels, which today's model cannot hold"
    ).toBe(0);
  });
});

// c-focus-opens.
test("a body opens on focus and nothing opens on hover", async ({ browser }) => {
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    await gotoIssue(page, `/issues/${seeded.key}`);

    // One body is drawn: the focused node's.
    await focusThePage(page);
    await page.keyboard.press("Tab");
    const focused = await focusedNodeId(page);
    expect(
      focused,
      "Tab put the focus on no row of the tree, so no body can open on focus"
    ).not.toBeNull();
    await expectIssuePageDrawn(page);
    expect(await nodeBodies(page).count(), "more than the focused node's body is drawn").toBe(1);
    expect(
      (await drawnRows(page)).filter((row) => row.body).map((row) => row.id),
      "the body drawn is not the focused node's"
    ).toEqual([focused]);

    // A node pinned open keeps its body when the focus leaves it.
    await page.keyboard.press("o");
    expect(await pinnedBodies(page).count(), "o pinned no body open").toBe(1);
    await page.keyboard.press("j");
    expect(
      await nodeBodies(page).count(),
      "the pinned body and the focused node's are not the two bodies drawn"
    ).toBe(2);
    await page.keyboard.press("k");
    await page.keyboard.press("o");
    expect(await pinnedBodies(page).count(), "o did not close the pinned body").toBe(0);

    // With the keyboard focus off the tree nothing is open, and the pointer opens nothing either.
    await focusThePage(page);
    expect(
      await nodeBodies(page).count(),
      "a body is drawn with the keyboard focus off the tree"
    ).toBe(0);
    const before = await treeText(page);
    const rows = nodeRows(page);
    const count = await rows.count();
    for (let index = 0; index < count; index += 1) {
      await rows.nth(index).hover();
      expect(
        await nodeBodies(page).count(),
        `the pointer over row ${index} opened a body with the focus elsewhere`
      ).toBe(0);
    }
    expect(await treeText(page), "the pointer crossing every row changed the tree's text").toBe(
      before
    );
    expect(
      await tooltipTexts(page),
      "the page carries tooltips, which a reader on a phone or on the keyboard never sees"
    ).toEqual([]);

    // Nothing is carried by colour: each state and type has a word and a shape no other carries,
    // and the reading holds with the colours forced. A greyscale filter is not the test - today's
    // three muted states already differ in luminance and would pass one.
    const drawn = await drawnRows(page);
    expectRowVocabulary(drawn);
    const states = statesDrawn(drawn);
    expect(
      [...new Set(states.values())].length,
      `two states are drawn the same way: ${JSON.stringify([...states])}`
    ).toBe(states.size);
    const types = new Map(drawn.map((row) => [row.type ?? "", row.glyphs[0]?.token ?? ""]));
    expect(
      [...new Set(types.values())].length,
      `two types draw the same shape: ${JSON.stringify([...types])}`
    ).toBe(types.size);
    await page.emulateMedia({ forcedColors: "active" });
    const forced = await drawnRows(page);
    expectRowVocabulary(forced);
    expect(
      [...statesDrawn(forced)],
      "the states are told apart differently once the colours are forced"
    ).toEqual([...states]);
  });
});

// c-presets.
test("the presets are queries over one tree and fit every width @phone", async ({ browser }) => {
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    await page.setViewportSize({ height: DESKTOP.height, width: DESKTOP.width });
    await gotoIssue(page, `/issues/${seeded.key}`);

    // At 1280 px and above: a row of five tabs, each named by its preset and carrying its count.
    expect(await presetTabs(page).count(), "no row of five presets is drawn at 1440 px").toBe(5);
    expect(await presetTablist(page).count(), 'no role="tablist" named "Issue presets"').toBe(1);
    await expectIssuePageDrawn(page);
    for (const preset of PRESET_ORDER) {
      expect(
        await presetTab(page, preset).count(),
        `no tab is named exactly "${PRESET_NAMES[preset]}"`
      ).toBe(1);
      expect(
        await presetCount(page, preset).count(),
        `the ${PRESET_NAMES[preset]} preset draws no count of its own`
      ).toBe(1);
    }

    // Each preset is its own address, draws every match with the chain of ancestors above it, and
    // duplicates nothing.
    const chains = new Map<string, { chain: string; preset: Preset }>();
    for (const preset of PRESET_ORDER) {
      await gotoIssue(page, presetAddress(seeded.key, preset));
      await expectIssuePageDrawn(page);
      expect(
        await activePreset(page),
        `${presetAddress(seeded.key, preset)} does not open the ${PRESET_NAMES[preset]} preset`
      ).toBe(PRESET_NAMES[preset]);

      const rows = await drawnRows(page);
      const ids = idsOf(rows);
      expect(
        ids.filter((id, index) => ids.indexOf(id) !== index),
        `the ${PRESET_NAMES[preset]} preset draws a node twice`
      ).toEqual([]);
      const matches = rows.filter((row) => !row.context);
      expect(matches.length, `the ${PRESET_NAMES[preset]} preset matches nothing`).toBeGreaterThan(
        0
      );
      for (const row of rows) {
        const chain = (await ancestorsOfRow(page, row.id ?? "")).join(" > ");
        for (const ancestor of chain === "" ? [] : chain.split(" > ")) {
          expect(
            rowOf(rows, ancestor)?.role,
            `the ${PRESET_NAMES[preset]} preset draws ${row.id} without its ancestor ${ancestor} as a treeitem the arrows reach`
          ).toBe("treeitem");
        }
        // Nothing moves between presets: where two presets draw one node, it sits in the same
        // chain in both.
        const seen = chains.get(row.id ?? "");
        if (seen === undefined) {
          chains.set(row.id ?? "", { chain, preset });
          continue;
        }
        expect(
          chain,
          `${row.id} sits under "${seen.chain}" in ${PRESET_NAMES[seen.preset]} and under "${chain}" in ${PRESET_NAMES[preset]}`
        ).toBe(seen.chain);
      }
    }

    // The two shapes, and no sideways scroll at any width between them.
    await gotoIssue(page, `/issues/${seeded.key}`);
    for (const width of [1440, 1280]) {
      await page.setViewportSize({ height: DESKTOP.height, width });
      expect(await presetTabs(page).count(), `the row of five is gone at ${width} px`).toBe(5);
      expect(
        await presetChooser(page).count(),
        `one control replaced the row of five at ${width} px`
      ).toBe(0);
      await assertPageFits(page);
    }
    for (const width of [1279, 1100]) {
      await page.setViewportSize({ height: DESKTOP.height, width });
      const tabs = await presetTabs(page).count();
      const chooser = await presetChooser(page).count();
      expect(
        `${tabs}/${chooser}`,
        `at ${width} px the page draws neither the row of five nor the one control (${tabs} tabs, ${chooser} controls)`
      ).toMatch(/^(5\/0|0\/1)$/);
      await assertPageFits(page);
      await assertWhole(presetRow(page));
    }
    for (const width of [1024, 390, 320]) {
      await page.setViewportSize({ height: PHONE.height, width });
      expect(await presetTabs(page).count(), `the row of five is still drawn at ${width} px`).toBe(
        0
      );
      expect(await presetChooser(page).count(), `the one control is missing at ${width} px`).toBe(
        1
      );
      await assertPageFits(page);
    }
    await presetChooser(page).click();
    expect(await openMenu(page).count(), "the one control opens no menu").toBe(1);
    for (const preset of PRESET_ORDER) {
      expect(
        await presetMenuItem(page, preset).count(),
        `the menu holds no item for the ${PRESET_NAMES[preset]} preset`
      ).toBe(1);
    }
  });
});

// c-spine. Partial on today's data: `requirement` carries no attributes, so no `from` edge exists
// to seed (dispatch://INFRA-1098 item 21). What is proved is what a document's own `serves`
// gives, plus the requirement whose origin is absent.
test("the spine is the reasoning and an unexplained node sits under the issue", async ({
  browser,
}) => {
  await phase(browser, seedSpineCases, async (page, seeded) => {
    await gotoIssue(page, `/issues/${seeded.key}`);

    const specSlug = (await getArtifact(seeded.specArtifactId)).slug;
    const verificationSlug = (await getArtifact(seeded.verificationArtifactId)).slug;
    const unexplained = blockNodeId(specSlug, SPINE_REQUIREMENTS.unexplained);
    const check = blockNodeId(verificationSlug, SPINE_CHECK_BLOCK_ID);

    // A requirement whose origin is absent sits under the issue, drawn as a node nothing explains
    // rather than as an error: the page names that gap and the gap names its kind.
    expect(
      await parentOfRow(page, unexplained),
      "the requirement whose origin is absent sits under no node: the page draws no row for it"
    ).toBe(seeded.key);
    await expectIssuePageDrawn(page);
    expect(
      await gaps(page).count(),
      "the page names no gap at all, so nothing says the requirement is unexplained"
    ).toBeGreaterThan(0);
    expect(
      await rowGaps(nodeRow(page, unexplained).first()).count(),
      "the requirement nothing explains is drawn with no gap on it"
    ).toBeGreaterThan(0);
    expect(await gapKinds(page), "a gap the page draws names no kind").not.toContain("");

    // The check that serves two requirements is drawn under both, each row naming its other
    // parent, and both rows open the same block.
    const rows = await drawnRows(page);
    const drawnUnder = rows
      .map((row, index) => ({ index, row }))
      .filter((entry) => entry.row.id === check);
    expect(
      drawnUnder.map((entry) => entry.index).length,
      "the check that serves two requirements is not drawn under both of them"
    ).toBe(2);
    const parents = new Set<string | null | undefined>();
    for (const entry of drawnUnder) {
      expect(
        entry.row.text,
        "a transcluded check does not name the other requirement it sits under"
      ).toContain(CONTROL_LABELS.also_under);
      parents.add(await parentOfRow(page, check));
    }
    expect(
      [
        blockNodeId(specSlug, SPINE_REQUIREMENTS.servedTwiceA),
        blockNodeId(specSlug, SPINE_REQUIREMENTS.servedTwiceB),
      ].filter((requirement) => rowOf(rows, requirement) === undefined),
      "the two requirements the check serves are not both drawn"
    ).toEqual([]);

    const firstRow = nodeRow(page, check).nth(0);
    const secondRow = nodeRow(page, check).nth(1);
    await firstRow.focus();
    const fromFirstParent = (await rowBody(firstRow).innerText()).replace(/\s+/g, " ");
    await secondRow.focus();
    const fromSecondParent = (await rowBody(secondRow).innerText()).replace(/\s+/g, " ");
    expect(
      fromSecondParent,
      "the two parents open two copies of the block rather than the same block"
    ).toBe(fromFirstParent);

    // Every edge that is not the spine is a chip naming its target, found exactly once: the
    // decision's link to a requirement, and the answered question it names.
    const chips = rows.flatMap((row) => row.chips);
    for (const target of [SPINE_REQUIREMENTS.shapedElsewhere, seeded.answeredAskId]) {
      const naming = chips.filter((chip) => chip.includes(target));
      expect(
        naming.length,
        `the edge to ${target} is drawn ${naming.length} times as a chip, not once: ${JSON.stringify(chips)}`
      ).toBe(1);
    }
  });
});

// c-three-things. Partial on today's data: the node that holds the bullets, its author and
// `last_read_back_at` are dispatch://INFRA-1278, so the head names that gap and this test never
// asserts that an author is drawn.
test("the three things are at the head of every preset @phone", async ({ browser }) => {
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    for (const size of [DESKTOP, PHONE]) {
      await page.setViewportSize({ height: size.height, width: size.width });
      for (const preset of PRESET_ORDER) {
        await gotoIssue(page, presetAddress(seeded.key, preset));
        expect(
          await threeThings(page).count(),
          `the ${PRESET_NAMES[preset]} preset draws no three things at ${size.width} px`
        ).toBe(1);
        await expectIssuePageDrawn(page);

        const head = await threeThings(page).boundingBox();
        const presets = await presetRow(page).boundingBox();
        expect(head, "the three things have no box").not.toBeNull();
        expect(presets, "the preset row has no box").not.toBeNull();
        expect(
          (head?.y ?? 0) + (head?.height ?? 0),
          `the three things are not above the presets at ${size.width} px`
        ).toBeLessThanOrEqual((presets?.y ?? 0) + 1);

        expect(
          (await threeThingsSentences(page).allInnerTexts()).map((sentence) =>
            sentence.replace(/\s+/g, " ").trim()
          ),
          `the ${PRESET_NAMES[preset]} preset does not draw the spec's own three sentences at ${size.width} px`
        ).toEqual([...SUMMARY_SENTENCES]);
        expect(
          (await planRow(page).innerText()).replace(/\s+/g, " "),
          `the ${PRESET_NAMES[preset]} preset's head points at no Plan preset`
        ).toContain(PRESET_NAMES.plan);
        expect(
          (await checksRow(page).innerText()).replace(/\s+/g, " "),
          `the ${PRESET_NAMES[preset]} preset's head points at no Checks preset`
        ).toContain(PRESET_NAMES.checks);
      }
    }

    // Who wrote the bullets and when they were last read back wait on dispatch://INFRA-1278, so
    // the head says neither is known and draws no author at all.
    expect(
      await rowAuthor(threeThings(page)).count(),
      "the head names an author, which the model does not hold"
    ).toBe(0);
    expect(
      await rowGaps(threeThings(page)).count(),
      "the head says nothing about the author and the read-back it does not know"
    ).toBeGreaterThan(0);
  });

  await phase(browser, seedNoSummary, async (page, seeded) => {
    for (const size of [DESKTOP, PHONE]) {
      await page.setViewportSize({ height: size.height, width: size.width });
      await gotoIssue(page, `/issues/${seeded.key}`);
      expect(
        await threeThings(page).count(),
        `a spec with no summary draws no row in that same place at ${size.width} px`
      ).toBe(1);
      await expectIssuePageDrawn(page);
      expect(
        (await threeThings(page).innerText()).replace(/\s+/g, " ").trim(),
        "that row says something other than the page's own line for a spec with no summary"
      ).toContain(EMPTY_STATE_LINES.spec_summary);
      expect(
        await threeThingsSentences(page).count(),
        "a spec with no summary draws sentences all the same"
      ).toBe(0);
    }
  });
});

// c-open-questions.
test("an open question is unfolded and on the first screen @phone", async ({ browser }) => {
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    const question = blockNodeId(await primarySlug(seeded.key), OPEN_QUESTION_BLOCK_ID);

    for (const size of [DESKTOP, PHONE]) {
      await page.setViewportSize({ height: size.height, width: size.width });
      await gotoIssue(page, `/issues/${seeded.key}`);

      const rows = await drawnRows(page);
      const row = rowOf(rows, question);
      expect(
        row,
        `the open question draws no row at ${size.width}x${size.height} with nothing unfolded by hand`
      ).toBeDefined();
      await expectIssuePageDrawn(page);
      const box = row?.box ?? null;
      expect(box, "the open question's row has no box").not.toBeNull();
      const headerBottom = (await identityBlockBottom(page)) ?? 0;
      expect(
        box?.y ?? -1,
        `the open question's row sits under the sticky identity block at ${size.width}x${size.height}`
      ).toBeGreaterThanOrEqual(headerBottom - 1);
      expect(
        (box?.y ?? 0) + (box?.height ?? 0),
        `the open question's row is below the first screen at ${size.width}x${size.height}`
      ).toBeLessThanOrEqual(size.height);
      for (const ancestor of await ancestorsOfRow(page, question)) {
        expect(
          rowOf(rows, ancestor)?.expanded,
          `the question's ancestor ${ancestor} is folded, so its path is not unfolded`
        ).toBe("true");
      }
    }

    // The next-question key moves to the next open question and says so when there is none.
    await page.setViewportSize({ height: DESKTOP.height, width: DESKTOP.width });
    await gotoIssue(page, `/issues/${seeded.key}`);
    await focusThePage(page);
    await page.keyboard.press("q");
    expect(await focusedNodeId(page), "the next-question key did not reach the open question").toBe(
      question
    );
    await page.keyboard.press("q");
    expect(
      await pageStatus(page).count(),
      "with no further open question the key reports nothing"
    ).toBeGreaterThan(0);
    expect(
      wordsIn(await pageStatus(page).first().innerText()).filter(
        (word) => !DECLARED_WORDS.includes(word)
      ),
      "that report uses words the page does not declare"
    ).toEqual([]);

    // Those rows are not counted against the fold budget: with the question answered, the fold
    // closes its path and leaves every other branch exactly as it was.
    const withQuestion = idsOf(await drawnRows(page));
    const branch = await ancestorsOfRow(page, question);
    await answerAsk(seeded.openQuestion.askId, {
      expected_edited_at: null,
      selected: [],
      text: "1280 px.",
    });
    await page.reload();
    await expectIssuePageDrawn(page);
    const withoutQuestion = idsOf(await drawnRows(page));
    expect(
      withoutQuestion,
      "the answered question's row is still drawn as an open question's path would be"
    ).not.toContain(question);
    const outside = (ids: readonly string[]) =>
      ids.filter((id) => !branch.includes(id) && id !== question);
    expect(
      outside(withoutQuestion).filter((id) => !outside(withQuestion).includes(id)),
      "the fold changed outside the question's own path, so those rows were charged against it"
    ).toEqual([]);
    expect(
      withoutQuestion.length,
      "the tree draws as many rows without the open question as with it, so its path cost the fold nothing to prove"
    ).toBeLessThan(withQuestion.length);
  });
});

// c-children.
test("the children section opens each preset and collapses to its counts @phone", async ({
  browser,
}) => {
  // Two whole seeds and fifteen page loads, each one a cold load of the issue page: the work
  // is the check's own, not a page that got slower, and on the emulated-phone project it runs
  // within a second or two of the 30 s default. The repo's way of saying a test is long is to
  // name its own budget (`e2e/heavy-document-writes.e2e.ts`, `e2e/cli-body-cap.e2e.ts`).
  test.setTimeout(90_000);
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    // No seed claims a child, and the check reads who holds the running one, so this test puts a
    // child in progress and claims it: that is the state the assertion is read against.
    const running = seeded.children.keys[0] ?? "";
    await patchIssue(running, { status: "in_progress" });
    await claimIssue(running, asPlanner);
    const blocked = seeded.children.blocked;
    const blockerTitle = (await getIssue(running)).title;

    for (const size of [DESKTOP, PHONE]) {
      await page.setViewportSize({ height: size.height, width: size.width });
      for (const preset of PRESET_ORDER) {
        await gotoIssue(page, presetAddress(seeded.key, preset));
        expect(
          await childrenSection(page).count(),
          `the ${PRESET_NAMES[preset]} preset draws no children section at ${size.width} px`
        ).toBe(1);
        await expectIssuePageDrawn(page);
        const section = await childrenSection(page).boundingBox();
        const body = await tree(page).boundingBox();
        expect(
          section?.y ?? 0,
          `the children section is not the first thing in the ${PRESET_NAMES[preset]} preset's content at ${size.width} px`
        ).toBeLessThanOrEqual((body?.y ?? 0) + 1);
      }
    }

    await page.setViewportSize({ height: DESKTOP.height, width: DESKTOP.width });
    await gotoIssue(page, `/issues/${seeded.key}`);
    expect(await childRows(page).count(), "the open section lists no children").toBe(
      seeded.children.keys.length
    );
    for (const key of seeded.children.keys) {
      expect(
        await childRow(page, key).first().getAttribute("data-child-status"),
        `the row for ${key} does not name its status`
      ).toBe((await getIssue(key)).status);
    }
    expect(
      (await childRow(page, blocked).first().innerText()).replace(/\s+/g, " "),
      "the blocked child's row does not name the issue that blocks it"
    ).toMatch(new RegExp(`${running}|${blockerTitle.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}`));
    expect(
      (await childRow(page, running).first().innerText()).replace(/\s+/g, " "),
      "the running child's row does not say who holds it"
    ).toContain(PLANNER_TITLE);

    // Collapsed, the header keeps the bar and both counts, each labelled.
    await childrenDisclosure(page).click();
    expect(await childRows(page).count(), "the collapse left the child rows on the page").toBe(0);
    expect(await childrenBar(page).count(), "the collapsed header lost its completion bar").toBe(1);
    expect(
      (await childrenDirectCount(page).innerText()).replace(/\s+/g, " "),
      "the collapsed header's direct count carries no visible label"
    ).toContain(CONTROL_LABELS.children_done);
    expect(
      (await childrenSubtreeCount(page).innerText()).replace(/\s+/g, " "),
      "the collapsed header's subtree count carries no visible label"
    ).toContain(CONTROL_LABELS.subtree_done);
    expect(
      (await childrenDirectCount(page).innerText()).replace(/\s+/g, " "),
      `the direct count does not read one of ${seeded.children.keys.length} children done`
    ).toMatch(new RegExp(`1\\D+${seeded.children.keys.length}`));

    // The reader's choice is kept for the next issue they open.
    await gotoIssue(page, `/issues/${seeded.parentKey}`);
    await expectIssuePageDrawn(page);
    expect(
      await childRows(page).count(),
      "the next issue opens the children section this reader collapsed"
    ).toBe(0);
  });

  await phase(browser, seedNoChildren, async (page, seeded) => {
    for (const preset of PRESET_ORDER) {
      await gotoIssue(page, presetAddress(seeded.key, preset));
      await expectIssuePageDrawn(page);
      expect(
        await childrenSection(page).count(),
        `an issue with no children draws a children section on the ${PRESET_NAMES[preset]} preset`
      ).toBe(0);
    }
  });
});

// c-properties.
test("every property with a value is visible at every width @phone", async ({ browser }) => {
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    const issue = await getIssue(seeded.key);
    // Every property the seed set, named one by one and read where the page draws it.
    const properties: readonly (readonly [string, string])[] = [
      ["the parent", seeded.parentKey],
      ["the assignee", "alice"],
      ["the first label", "dispatch"],
      ["the second label", "layout"],
      ["the third label", "redesign"],
      ["the repository", issue.repo ?? ""],
      ["the pull request", "11"],
      ["the claim", PLANNER_TITLE],
      ["a component", "Issue panel"],
      ["the other component", "Issue tree"],
    ];

    for (const width of [1440, 1280, 1024, 390]) {
      await page.setViewportSize({ height: width < 1024 ? PHONE.height : DESKTOP.height, width });
      await gotoIssue(page, `/issues/${seeded.key}`);
      const shown = await visibleText(page);
      for (const [name, value] of properties) {
        expect(shown, `${name} is not visible at ${width} px with no control opened`).toContain(
          value
        );
      }
      await expectIssuePageDrawn(page);
      await assertPageFits(page);

      // The status, the priority, the assignee and the labels stay with the title.
      const title = (await identityBlock(page).innerText()).replace(/\s+/g, " ");
      for (const [name, value] of [
        ["the assignee", "alice"],
        ["the first label", "dispatch"],
      ] as const) {
        expect(title, `${name} left the title block at ${width} px`).toContain(value);
      }
      expect(
        await identityBlock(page).getByRole("combobox", { name: "Status" }).count(),
        `the status control left the title block at ${width} px`
      ).toBe(1);
      expect(
        await identityBlock(page)
          .getByRole("combobox", { name: `Priority of ${seeded.key}` })
          .count(),
        `the priority control left the title block at ${width} px`
      ).toBe(1);
    }

    // An empty field is reachable from one add control that lists it.
    await page.setViewportSize({ height: DESKTOP.height, width: DESKTOP.width });
    await gotoIssue(page, `/issues/${seeded.key}`);
    expect(await addControl(page).count(), "the page offers no one add control").toBe(1);
    await addControl(page).click();
    expect(
      await openMenu(page).count(),
      "the add control opens no list of the fields with no value"
    ).toBe(1);
    expect(
      await openMenu(page).getByRole("menuitem").count(),
      "the add control's list names no field"
    ).toBeGreaterThan(0);
    await page.keyboard.press("Escape");

    // A section opens expanded on a first load, and a collapsed one keeps that state.
    expect(
      await marginSectionDisclosure(page, "development").getAttribute("aria-expanded"),
      "the Development section is collapsed on a first load"
    ).toBe("true");
    await marginSectionDisclosure(page, "development").click();
    expect(
      (await marginSection(page, "development").innerText()).replace(/\s+/g, " "),
      "collapsing the section left its values on the page"
    ).not.toContain(issue.repo ?? "");
    await gotoIssue(page, `/issues/${seeded.parentKey}`);
    await expectIssuePageDrawn(page);
    expect(
      await marginSectionDisclosure(page, "development").getAttribute("aria-expanded"),
      "the next issue opens the section this reader collapsed"
    ).toBe("false");
  });
});

// c-one-panel.
test("one right-hand panel, and the document page keeps its margin", async ({ browser }) => {
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    await gotoIssue(page, `/issues/${seeded.key}`);

    expect(await marginSlot(page).count(), "the issue page draws no panel of its own").toBe(1);
    await expectIssuePageDrawn(page);
    expect(await marginShell(page).count(), "the shell's margin is not the panel it sits in").toBe(
      1
    );
    expect(
      await panelsRightOfMain(page),
      "the properties and the focused node's facts are not in one panel right of the main column"
    ).toHaveLength(1);
    for (const section of ["development", "people", "references"] as const) {
      expect(
        await marginSection(page, section).count(),
        `the panel holds no ${section} section`
      ).toBe(1);
    }
    expect(
      await focusedNodeFacts(page).count(),
      "the panel holds no facts of the focused node"
    ).toBe(1);
    expect(
      await conversationStateControl(page, "words").count(),
      "the panel holds no conversation"
    ).toBe(1);

    // The facts follow the focus.
    await focusThePage(page);
    await page.keyboard.press("Tab");
    const first = await focusedNodeFacts(page).innerText();
    const firstNode = await focusedNodeId(page);
    await page.keyboard.press("j");
    expect(await focusedNodeId(page), "j moved the focus to no other row").not.toBe(firstNode);
    expect(
      await focusedNodeFacts(page).innerText(),
      "the panel's facts did not follow the focus to the next node"
    ).not.toBe(first);

    // The handle, the collapse and the width the reader gave it.
    expect(await marginResizeHandle(page).count(), "the panel has no resize handle").toBe(1);
    const handle = await marginResizeHandle(page).boundingBox();
    expect(handle, "the resize handle has no box").not.toBeNull();
    const before = (await marginShell(page).boundingBox())?.width ?? 0;
    await page.mouse.move((handle?.x ?? 0) + 2, (handle?.y ?? 0) + 2);
    await page.mouse.down();
    await page.mouse.move((handle?.x ?? 0) - 120, (handle?.y ?? 0) + 2);
    await page.mouse.up();
    const resized = (await marginShell(page).boundingBox())?.width ?? 0;
    expect(resized, "dragging the handle did not widen the panel").toBeGreaterThan(before + 50);
    await page.reload();
    await expectIssuePageDrawn(page);
    expect(
      (await marginShell(page).boundingBox())?.width ?? 0,
      "the panel did not keep the width the reader gave it"
    ).toBeCloseTo(resized, 0);
    await marginHideControl(page).click();
    expect(await marginSlot(page).count(), "the collapse left the panel open").toBe(0);
    await marginShowControl(page).click();
    expect(await marginSlot(page).count(), "the panel did not come back").toBe(1);

    // A project document page draws its margin as before.
    const document = await createProjectDocument("LAY", {
      content: "# A project document\n\nIts margin is unchanged.\n",
      name: "project-notes.md",
    });
    await page.goto(`/projects/LAY/documents/${document.artifact.slug}`);
    await expect(page.getByRole("heading", { level: 1, name: "A project document" })).toBeVisible();
    expect(await marginShell(page).count(), "the project document page lost its margin").toBe(1);
  });
});

// c-home. Partial on today's data: `from` waits on dispatch://INFRA-1098 item 21, so every
// requirement sits at the issue root and this test asserts that, not a decision's parentage.
test("the spec is a preset over the tree and an approval still names it", async ({ browser }) => {
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    // The approval is requested before the page draws, so the target it pins is read back after.
    await requestApproval(seeded.specArtifactId, { summary: "The spec as the tree draws it." });

    await openPreset(page, seeded.key, "spec");
    expect(await activePreset(page), "the Spec address does not open the Spec preset").toBe(
      PRESET_NAMES.spec
    );
    await expectIssuePageDrawn(page);

    const specSlug = (await getArtifact(seeded.specArtifactId)).slug;
    const rows = await drawnRows(page);
    for (const requirement of seeded.requirements) {
      const id = blockNodeId(specSlug, requirement);
      expect(rowOf(rows, id), `the Spec preset does not draw ${requirement}`).toBeDefined();
      expect(
        await parentOfRow(page, id),
        `${requirement} does not sit at the issue root while its own from pointer is absent`
      ).toBe(seeded.key);
    }
    // A block with no `from` is never drawn as a block that had no conversation: every gap the
    // preset draws names its kind, so none is an unnamed mark on those rows.
    expect(await gapKinds(page), "a gap the Spec preset draws names no kind").not.toContain("");

    // The spec stays a thing an approval resolves: the version it pins still resolves after the
    // page has drawn.
    const artifact = await getArtifact(seeded.specArtifactId);
    expect(artifact.approval, "the spec carries no approval after the page drew").toBeDefined();
    const pinned = artifact.approval?.version ?? artifact.approval?.latest_version ?? 0;
    expect(pinned, "the approval pins no version of the spec").toBeGreaterThan(0);
    expect(
      (await getArtifactVersion(seeded.specArtifactId, pinned)).markdown,
      "the document version the approval pins does not resolve to the spec's own text"
    ).toContain(requirementText(2));
    expect(
      await presetToolbar(page).count(),
      "the preset whose document is open draws no toolbar to carry the approval"
    ).toBe(1);
  });

  // The same drawing on an issue that carries one document.
  await phase(browser, seedNoChildren, async (page, seeded) => {
    await openPreset(page, seeded.key, "spec");
    await expectIssuePageDrawn(page);
    const specSlug = (await getArtifact(seeded.specArtifactId)).slug;
    for (const requirement of seeded.requirements) {
      expect(
        await parentOfRow(page, blockNodeId(specSlug, requirement)),
        `${requirement} is drawn differently on an issue that carries one document`
      ).toBe(seeded.key);
    }
  });
});

// c-checks-readable. The seeded Verification document carries no screenshot - no `check` block in
// `e2e/issue-layout-seed-text.ts` holds a picture - so "a screenshot renders as a picture" is
// asserted on the issue's image node in c-nodes instead, and not here.
test("the Checks preset is readable @phone", async ({ browser }) => {
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    const verificationSlug = (await getArtifact(seeded.verificationArtifactId)).slug;

    for (const width of [1440, 1024, 390]) {
      await page.setViewportSize({ height: width < 1024 ? PHONE.height : DESKTOP.height, width });
      await openPreset(page, seeded.key, "checks");
      expect(await activePreset(page), `the preset is not named Checks at ${width} px`).toBe(
        PRESET_NAMES.checks
      );
      await expectIssuePageDrawn(page);

      const checks = await rowsOfType(page, "check").count();
      expect(checks, `the Checks preset draws ${checks} checks, not the six seeded`).toBe(6);
      const ids = (await drawnRows(page))
        .filter((row) => row.type === "check")
        .map((row) => row.id ?? "");
      expect(
        ids.filter((id, index) => ids.indexOf(id) !== index),
        `a check is drawn twice at ${width} px`
      ).toEqual([]);
      expect(
        await boxesPastTheirCard(page, "rows"),
        `an element reaches past its card at ${width} px`
      ).toEqual([]);
      expect(
        await overlappingTexts(page, "rows"),
        `two pieces of text are drawn over each other at ${width} px`
      ).toEqual([]);
    }

    // A command renders as code with a copy control, and `when` reads as words.
    await page.setViewportSize({ height: DESKTOP.height, width: DESKTOP.width });
    await openPreset(page, seeded.key, "checks");
    const passed = nodeRow(page, blockNodeId(verificationSlug, checkBlockId(1))).first();
    await passed.focus();
    expect(await nodeCommands(page).count(), "the check's command is not drawn as code").toBe(1);
    expect(
      (
        await nodeCommands(page)
          .first()
          .evaluate((element) => getComputedStyle(element).fontFamily)
      ).toLowerCase(),
      "the command is not set in a monospace face"
    ).toMatch(/mono/);
    expect(await copyCommandControls(page).count(), "the command has no copy control").toBe(1);
    expect(
      (await passed.innerText()).replace(/\s+/g, " "),
      "the check draws its when attribute as a schema token instead of words"
    ).toContain(CHECK_WHEN_WORDS.before_merge);
    expect(
      await boxesPastTheirCard(page, "bodies"),
      "an element of the opened check reaches past its body"
    ).toEqual([]);

    // The five states carry a word and a shape no other state carries, colours forced as well.
    const states = statesDrawn((await drawnRows(page)).filter((row) => row.type === "check"));
    expect([...states.keys()].sort(), "the Checks preset does not draw all five states").toEqual(
      Object.keys(NODE_STATE_WORDS.check)
        .map((state) => `check:${state}`)
        .sort()
    );
    expect(
      [...new Set(states.values())].length,
      `two of the five states are drawn the same way: ${JSON.stringify([...states])}`
    ).toBe(states.size);
    await page.emulateMedia({ forcedColors: "active" });
    expect(
      [...statesDrawn((await drawnRows(page)).filter((row) => row.type === "check"))],
      "the five states are told apart differently once the colours are forced"
    ).toEqual([...states]);
  });
});

// c-plain-words.
test("the page draws no internal word where a name belongs @phone", async ({ browser }) => {
  // The check reads the page's chrome in ten views and in all 56 node bodies of the seeded
  // issue: about 11,700 readings, each read against five patterns and the declared list. One
  // `expect` per reading was 70,000 calls at about 4 ms each - 281 s of assertion beside 20 s
  // of page work - so each view gathers every offender it finds and names them in one
  // assertion, which reads the same strength and says more: a view that draws three internal
  // words names all three rather than the first. The row still carries a deadline of its own,
  // because ten navigations and 56 bodies outlast the 30 s default on their own.
  test.setTimeout(600_000);
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    // An identifier, a fragment, a slug, and the schema and enumeration tokens the check names.
    const banned = [
      /[0-9a-f]{8}-[0-9a-f]{4}-/i,
      /#b-/,
      /before_merge/,
      /no-result/,
      /plan_task/,
      /\bartifact\b/i,
    ];

    const readChrome = async (where: string) => {
      const texts = await chromeTexts(page);
      expect(texts.length, `${where} marks none of its own words as the page's`).toBeGreaterThan(0);
      const offenders: string[] = [];
      for (const text of texts) {
        for (const pattern of banned) {
          if (pattern.test(text)) offenders.push(`"${text}" holds ${pattern}`);
        }
        const undeclared = wordsIn(text).filter((word) => !DECLARED_WORDS.includes(word));
        if (undeclared.length > 0) {
          offenders.push(
            `"${text}" draws ${JSON.stringify(undeclared)}, which the page declares nowhere`
          );
        }
      }
      expect(
        offenders,
        `${where} draws internal words where a name belongs: ${offenders.join("; ")}`
      ).toEqual([]);
      // The attribute is what decides what this test covers, so it is asserted too: text the page
      // divides into neither its own words nor an author's would never be read above.
      expect(
        await undividedTexts(page),
        `${where} draws text that is neither the page's own words nor an author's`
      ).toEqual([]);
    };

    for (const size of [DESKTOP, PHONE]) {
      await page.setViewportSize({ height: size.height, width: size.width });
      for (const preset of PRESET_ORDER) {
        await gotoIssue(page, presetAddress(seeded.key, preset));
        await readChrome(`the ${PRESET_NAMES[preset]} preset at ${size.width} px`);
        await expectIssuePageDrawn(page);
      }
    }

    // Every node's body in turn, where a type's attributes and its state are drawn in full.
    await page.setViewportSize({ height: DESKTOP.height, width: DESKTOP.width });
    await gotoIssue(page, `/issues/${seeded.key}`);
    const rows = nodeRows(page);
    const count = await rows.count();
    for (let index = 0; index < count; index += 1) {
      await rows.nth(index).focus();
      await readChrome(`the body of row ${index}`);
    }
  });
});

// c-nodes.
test("documents are nodes and nothing is a file list", async ({ browser }) => {
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    await gotoIssue(page, `/issues/${seeded.key}`);
    const specSlug = (await getArtifact(seeded.specArtifactId)).slug;

    // The cited document is a node under the block that cites it, and opens from there.
    expect(
      await parentOfRow(page, seeded.documents.cited),
      "the cited document does not sit under the block that cites it"
    ).toBe(blockNodeId(specSlug, seeded.documents.citingBlock));
    await expectIssuePageDrawn(page);
    const cited = nodeRow(page, seeded.documents.cited).first();
    await cited.focus();
    // The read is the node's own, made when the body opens: the page reads no document's text
    // before a reader opens it (R20, c-requests), so the body says so until the read lands.
    await expect
      .poll(async () => (await rowBody(cited).innerText()).replace(/\s+/g, " "), {
        message: "the cited document's node does not open its own text",
      })
      .toContain("The measurements behind the first requirement.");

    // What nothing cites is in the node menu, marked as cited by nothing, and so is the upload.
    expect(
      await nodeMenuButton(page).count(),
      "the page draws no node menu beside the presets"
    ).toBe(1);
    await nodeMenuButton(page).click();
    expect(await openMenu(page).count(), "the node menu opens nothing").toBe(1);
    const items = (await nodeMenuItems(page).allInnerTexts()).map((text) =>
      text.replace(/\s+/g, " ").trim()
    );
    for (const name of ["background.md", "shot.png"]) {
      const item = items.find((text) => text.includes(name));
      expect(item, `the node menu does not hold ${name}: ${JSON.stringify(items)}`).toBeDefined();
      expect(item, `${name} is not marked as cited by nothing`).toContain(
        CONTROL_LABELS.nothing_cites_it
      );
    }
    const upload = openMenu(page).getByRole("menuitem", { name: CONTROL_LABELS.upload });
    expect(await upload.count(), "the upload is not in the node menu").toBe(1);
    await upload.click();
    await uploadInput(page)
      .first()
      .setInputFiles({
        buffer: Buffer.from("# Uploaded\n\nFrom the node menu.\n"),
        mimeType: "text/markdown",
        name: "uploaded.md",
      });
    await expect
      .poll(async () => (await getIssue(seeded.key)).artifacts.map((artifact) => artifact.name))
      .toContain("uploaded.md");

    // An artifact's versions and the download of a version are reachable from the artifact.
    await page.keyboard.press("Escape");
    await cited.focus();
    expect(
      await offered(page, "button", CONTROL_LABELS.versions).count(),
      "the artifact's own versions are not reachable from it"
    ).toBeGreaterThan(0);
    expect(
      await page.getByRole("link", { name: CONTROL_LABELS.download }).count(),
      "a version's download is not reachable from the artifact"
    ).toBeGreaterThan(0);

    // The image is a node that draws its picture, and no preset is a file list.
    const image = nodeRow(page, seeded.documents.image).first();
    await image.focus();
    expect(await rowPictures(image).count(), "the image node draws no picture").toBeGreaterThan(0);
    expect(
      await presetTabs(page).allInnerTexts(),
      "a preset is still named Artifacts"
    ).not.toContain("Artifacts");
  });
});

// c-reflow.
test("the page never scrolls in two dimensions @phone", async ({ browser }) => {
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    // The test sets the viewport and writes no width rule of its own: every measure is the page's.
    for (const width of [320, 390, 768, 1024, 1280, 1440]) {
      await page.setViewportSize({ height: width < 1024 ? PHONE.height : DESKTOP.height, width });
      await gotoIssue(page, `/issues/${seeded.key}`);
      await assertPageFits(page);
      await assertWhole(identityBlock(page));
      // The walk is read against the rows it covers, so the count is an assertion of its own: a
      // walk over no row would pass every clipping check for want of anything to measure.
      const walked = await rowsWithoutAuthorScrollers(page);
      expect(
        walked.length,
        `the clipping walk covered no row of the tree at ${width} px: the page draws none`
      ).toBeGreaterThan(0);
      await expectIssuePageDrawn(page);
      await assertWhole(presetRow(page));
      // The panel holds the properties, the focused node's facts and the whole conversation, so
      // it is taller than the screen and scrolls down by design; R17 bans two dimensions, not
      // one, so the panel is read for a sideways clip and a clamped line (`e2e/unclipped.ts`).
      if ((await marginSlot(page).count()) === 1) await assertWholeAcross(marginSlot(page));
      for (const row of walked) {
        await assertWhole(row);
      }
      expect(
        await scrollersOutsideAuthorText(page),
        `a code block, a table or a picture is drawn outside a node's own text at ${width} px`
      ).toEqual([]);

      if (width > 1024) continue;
      // At 1024 px and below the tree is zoomed one node at a time, with no second indent level.
      const rows = (await drawnRows(page)).filter((row) => row.visible);
      const levels = [...new Set(rows.map((row) => row.level))];
      expect(
        levels.length,
        `the tree draws ${levels.length} indent levels at ${width} px: ${levels.join(", ")}`
      ).toBeLessThanOrEqual(2);
      expect(
        await nodeBodies(page).count(),
        `the zoomed tree draws more than the one node's body at ${width} px`
      ).toBeLessThanOrEqual(1);
    }
  });
});

/** The fixture `e2e/record-page-inventory.e2e.ts` wrote: what the old page offered, by state. */
interface PageInventoryFixture {
  readonly commit: string;
  readonly renames: Readonly<Record<string, string>>;
  readonly "open-1440": readonly InventoryEntry[];
  readonly "open-390": readonly InventoryEntry[];
  readonly "closed-1440": readonly InventoryEntry[];
  readonly "closed-390": readonly InventoryEntry[];
}

/** Opens the identity block's one actions control, which holds Close, Reopen and the control
 *  that sets the parent (R11). A press inside it closes it again, so it is opened only while
 *  it is closed. */
async function openIssueActions(page: Page): Promise<void> {
  const control = offered(page, "button", CONTROL_LABELS.issue_actions);
  if ((await control.getAttribute("aria-expanded")) !== "true") await control.click();
}

// c-nothing-lost.
test("the new page carries every element of the old one @phone", async ({ browser }) => {
  // Four states, each read through the page's own views - its five presets, the two addresses
  // beside them, the three views behind a control, the three a width opens, one per row of the
  // tree, and, where the narrow tree is the way in, the presets and the zooms walked again.
  // Each walk loads the page once and moves through its rows from there, which is how a reader
  // moves through them: 42 loads of the issue page per 390 px state and 13 per 1440 px one,
  // 124 and 36 views, each settled over two readings of its accessibility tree, and the write
  // walk below after them. Measured on an issue pod's two CPUs: a view costs about 1 s, which
  // is 5.8 minutes for the desktop project's four states and 4.4 for the phone project's two.
  // The row carries a deadline of its own because that outlasts the 30 s default many times
  // over, and the deadline leaves room for a loaded box.
  test.setTimeout(900_000);
  const fixture = JSON.parse(
    await readFile(
      fileURLToPath(new URL("./fixtures/issue-page-inventory.json", import.meta.url)),
      "utf8"
    )
  ) as PageInventoryFixture;

  await phase(browser, seedWholeIssue, async (page, seeded) => {
    // The inventory first: what the old page offered is this check's own subject, and the page
    // root below it says whether the page carrying it is the redesigned one. The fixture is the
    // merge of the old page's eight tab panels, one view at a time, so the new page is read the
    // same way: its five presets and the two addresses beside them, the conversation and the
    // activity (R3, R12). Three more views are the page's own, because what they hold sits
    // behind a control rather than behind an address: the node menu, where a document no block
    // cites lives (R16); the identity block's actions menu, where Close and the parent editor
    // live (R11); and each document's own node, whose body is what a document opens (R2).
    const views = (width: number) => [
      ...PRESET_ORDER.map((preset) => async () => {
        await gotoIssue(page, presetAddress(seeded.key, preset));
      }),
      async () => {
        await gotoIssue(page, `/issues/${seeded.key}/conversation`);
      },
      async () => {
        await gotoIssue(page, `/issues/${seeded.key}/activity`);
      },
      async () => {
        await gotoIssue(page, `/issues/${seeded.key}`);
        await nodeMenuButton(page).click();
        await expect(openMenu(page)).toHaveCount(1);
      },
      async () => {
        await gotoIssue(page, `/issues/${seeded.key}`);
        await offered(page, "button", CONTROL_LABELS.issue_actions).click();
      },
      // The preset row below 1280 px is one chooser rather than a row of tabs, the children
      // section opens closed below `md` (`useChildrenSection.ts`), and the panel is a sheet a
      // reader opens below the width the margin keeps a column at (R12): each is a view of the
      // page at that width, and at a width where it is already open it is the page as it
      // stands and adds nothing.
      async () => {
        await gotoIssue(page, `/issues/${seeded.key}`);
        if ((await presetChooser(page).count()) === 1) await presetChooser(page).click();
      },
      async () => {
        await gotoIssue(page, `/issues/${seeded.key}`);
        const disclosure = childrenDisclosure(page);
        if ((await disclosure.getAttribute("aria-expanded")) === "false") await disclosure.click();
      },
      async () => {
        await gotoIssue(page, `/issues/${seeded.key}`);
        const sheet = page.getByRole("button", { name: /review panel/i });
        if ((await sheet.count()) > 0) await sheet.first().click();
      },
      // One view per row of the tree, because a tree opens one body at a time and a node's own
      // text, its facts and its controls are in that body (R2). The seeded issue draws fewer
      // rows than this at either width, and a view whose row is not there reads the tree as it
      // stands and adds nothing. The walk loads the page once and then moves through the rows,
      // which is how a reader moves through them: each view is still read, and still fitted,
      // on its own.
      ...Array.from({ length: 24 }, (_unused, index) => async () => {
        if (index === 0) await gotoIssue(page, `/issues/${seeded.key}`);
        const rows = nodeRows(page);
        if ((await rows.count()) > index) await rows.nth(index).focus();
      }),
      // Below 1025 px the tree is read one node at a time (R17), so what a reader reaches by
      // zooming the walk reaches by zooming too. The presets first, because a decision, a
      // check and a plan task sit under the issue in the preset that draws them; then one
      // zoom per row of the issue's own address, because a node that hangs off a row - the
      // document a requirement cites - is behind that row's own twisty and nowhere else. At a
      // width that draws the whole tree the views above already open every row, so neither
      // walk runs there. Each walk loads its page once, as the walk above does.
      ...(width > 1024
        ? []
        : PRESET_ORDER.flatMap((preset) =>
            Array.from({ length: 8 }, (_unused, index) => async () => {
              if (index === 0) await gotoIssue(page, presetAddress(seeded.key, preset));
              const rows = nodeRows(page);
              if ((await rows.count()) > index) await rows.nth(index).focus();
            })
          )),
      ...(width > 1024
        ? []
        : Array.from({ length: 24 }, (_unused, into) =>
            Array.from({ length: 2 }, (_also, index) => async () => {
              if (index === 0) {
                await gotoIssue(page, `/issues/${seeded.key}`);
                const rows = nodeRows(page);
                if ((await rows.count()) <= into) return;
                const twisty = rows.nth(into).getByRole("button", { name: CONTROL_LABELS.unfold });
                if ((await twisty.count()) === 0) return;
                await twisty.first().click();
              }
              // The zoomed node is the first row; its own nodes follow it.
              const under = nodeRows(page);
              if ((await under.count()) > index + 1) await under.nth(index + 1).focus();
            })
          ).flat()),
    ];
    // Which states this project reads. The fixture was recorded in a desktop browser, so its
    // 1440 px states are a desktop shell's: setting 1440 px on an iPhone profile draws a
    // different shell around the same page, and what that shell offers - its search control
    // among it - is no part of what the issue page carries. So the desktop project reads all
    // four states and the phone project the two 390 px ones, and between them the check's own
    // words, at 1440 px and at 390 px, are both still asserted.
    const desktopProject = test.info().project.name !== "iphone";
    const states = (
      [
        ["open-1440", DESKTOP],
        ["open-390", PHONE],
      ] as const
    ).filter(([state]) => desktopProject || state.endsWith("-390"));
    for (const [state, size] of states) {
      await page.setViewportSize({ height: size.height, width: size.width });
      await assertInventoryCarried(page, fixture[state], {
        renames: fixture.renames,
        state: `${state}, recorded at ${fixture.commit}`,
        views: views(size.width),
      });
    }
    await patchIssue(seeded.key, { status: "done" });
    const closed = (
      [
        ["closed-1440", DESKTOP],
        ["closed-390", PHONE],
      ] as const
    ).filter(([state]) => desktopProject || state.endsWith("-390"));
    for (const [state, size] of closed) {
      await page.setViewportSize({ height: size.height, width: size.width });
      await assertInventoryCarried(page, fixture[state], {
        renames: fixture.renames,
        state: `${state}, recorded at ${fixture.commit}`,
        views: views(size.width),
      });
    }
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);

    // Each recorded name that writes today still writes: the write is made through the page and
    // the value read back from Dispatch. Close, Reopen and the parent editor are behind the
    // identity block's one actions control now (R11), so each is reached through it.
    await page.setViewportSize({ height: DESKTOP.height, width: DESKTOP.width });
    await gotoIssue(page, `/issues/${seeded.key}`);
    await openIssueActions(page);
    await offered(page, "button", "Reopen issue").click();
    await expect.poll(async () => (await getIssue(seeded.key)).status).not.toBe("done");

    await identityBlock(page).getByRole("combobox", { name: "Status" }).selectOption("todo");
    await expect.poll(async () => (await getIssue(seeded.key)).status).toBe("todo");

    await offered(page, "combobox", `Priority of ${seeded.key}`).selectOption("3");
    await expect.poll(async () => (await getIssue(seeded.key)).priority).toBe(3);

    // A picker whose list is read when the reader reaches for it - the people and the
    // repositories (`AssigneeControl.tsx`, `RepoControl.tsx`) - is reached for first, as a
    // reader reaches for it; without that it offers the value it already holds and nothing else.
    const assignee = offered(page, "combobox", `Assignee of ${seeded.key}`);
    await assignee.focus();
    await expect(assignee.locator('option[value="bob"]')).toHaveCount(1);
    await assignee.selectOption("bob");
    await expect.poll(async () => (await getIssue(seeded.key)).assignee).toBe("bob");

    await offered(page, "button", "Edit labels").click();
    await page.getByRole("combobox", { name: "Search or create label" }).fill("carried");
    await page
      .getByRole("option", { name: /carried/ })
      .first()
      .click();
    await page.keyboard.press("Escape");
    await expect.poll(async () => (await getIssue(seeded.key)).labels).toContain("carried");

    await openIssueActions(page);
    await offered(page, "button", "Edit parent issue").click();
    await page.getByRole("textbox", { name: "Parent" }).fill("");
    await offered(page, "button", CONTROL_LABELS.save_parent).click();
    await expect.poll(async () => (await getIssue(seeded.key)).parent).toBeNull();

    await offered(page, "button", `Messages default to ${PLANNER_TITLE}`).click();
    const route = page.getByRole("textbox", { name: "Message route" });
    await route.fill("role:reviewer");
    await offered(page, "button", CONTROL_LABELS.save_route).click();
    await expect.poll(async () => (await getIssue(seeded.key)).route).toBe("role:reviewer");

    const repository = offered(page, "combobox", `Repository of ${seeded.key}`);
    await repository.focus();
    const repoBefore = (await getIssue(seeded.key)).repo;
    await expect.poll(async () => (await optionsOf(repository)).length).toBeGreaterThan(1);
    const other = (await optionsOf(repository)).find((option) => option.value !== repoBefore);
    await repository.selectOption(other?.value ?? "");
    // The control's empty option is "No repository", which the issue reads back as none.
    const wroteRepo = other?.value === "" || other === undefined ? null : other.value;
    await expect.poll(async () => (await getIssue(seeded.key)).repo).toBe(wroteRepo);

    await offered(page, "button", `Edit components of ${seeded.key}`).click();
    await page.getByRole("option", { name: "Issue panel" }).first().click();
    await page.keyboard.press("Escape");
    await expect
      .poll(async () => (await getIssue(seeded.key)).components.ids)
      .not.toContain("panel");

    await offered(page, "button", "Unpin issue").click();
    await expect.poll(async () => (await getMyState("alice"))[seeded.key]?.pinned).toBe(false);

    // The close before the closed-state inventory took the seed's claim with it, so the issue
    // is claimed again for the one control that hands a claim back.
    await claimIssue(seeded.key, asPlanner);
    await expect(offered(page, "button", `Release the claim on ${seeded.key}`)).toBeVisible();
    await offered(page, "button", `Release the claim on ${seeded.key}`).click();
    await expect.poll(async () => (await getIssue(seeded.key)).claim).toBeNull();

    await openIssueActions(page);
    await offered(page, "button", "Close issue").click();
    await expect.poll(async () => (await getIssue(seeded.key)).status).toBe("done");
    await openIssueActions(page);
    await offered(page, "button", "Reopen issue").click();
    await expect.poll(async () => (await getIssue(seeded.key)).status).not.toBe("done");

    await offered(page, "button", "Subscribers: 3").click();
    await page
      .getByRole("button", { name: /Unsubscribe/ })
      .first()
      .click();
    // Unsubscribing asks first: Confirm is the only path that removes a subscriber
    // (`UnsubscribeDialog.tsx`).
    await page
      .getByRole("dialog", { name: "Unsubscribe" })
      .getByRole("button", { name: "Confirm" })
      .click();
    await expect.poll(async () => (await getUnsubscribeCalls()).length).toBeGreaterThan(0);

    // The approval: the seed requests none, so it is requested here and approved from the page.
    await requestApproval(seeded.specArtifactId, { summary: "Read it back." });
    await openPreset(page, seeded.key, "spec");
    // Approve is the identity block's own chip: the preset's toolbar draws how the document
    // stands and no action (`features/doc/ApprovalChip.tsx`'s `showActions`).
    await page
      .getByTestId("issue-state-actions")
      .getByRole("button", { name: /^Approve( v\d+)?$/ })
      .click();
    await expect
      .poll(async () => (await getArtifact(seeded.specArtifactId)).approval?.state)
      .toBe("approved");

    // Keep and Change, on the decision node they moved to.
    await openPreset(page, seeded.key, "decisions");
    await offered(page, "button", CONTROL_LABELS.keep_decision).click();
    await offered(page, "button", CONTROL_LABELS.change_decision).click();
    // The change is written in the decision's own composer, which names its box and its send
    // apart from the conversation's (`DecisionActions.tsx`, R7).
    await page
      .getByRole("textbox", { name: CONTROL_LABELS.your_change })
      .fill("Change this decision.");
    await offered(page, "button", CONTROL_LABELS.send_change).click();
    await expect
      .poll(async () => (await listComments(seeded.key)).map((comment) => comment.body))
      .toContain("Change this decision.");

    // The upload, the composer, an ask inside a document, a node's text, and a named version.
    await nodeMenuButton(page).click();
    await openMenu(page).getByRole("menuitem", { name: CONTROL_LABELS.upload }).click();
    await uploadInput(page)
      .first()
      .setInputFiles({
        buffer: Buffer.from("# Carried\n"),
        mimeType: "text/markdown",
        name: "carried.md",
      });
    await expect
      .poll(async () => (await getIssue(seeded.key)).artifacts.map((artifact) => artifact.name))
      .toContain("carried.md");

    // The panel's conversation, and its composer with it, is mounted once the conversation's
    // own address has been opened (`IssuePage.tsx`'s `activatedTabs`), so the composer is
    // written in where a reader writes in it.
    await gotoIssue(page, `/issues/${seeded.key}/conversation`);
    await marginSlot(page).getByRole("textbox", { name: "Comment" }).fill("The composer writes.");
    await marginSlot(page).getByRole("button", { name: "Send" }).click();
    await expect
      .poll(async () => (await listComments(seeded.key)).map((comment) => comment.body))
      .toContain("The composer writes.");

    // Back to the issue's own address, where the tree opens on the spec's nodes.
    await gotoIssue(page, `/issues/${seeded.key}`);
    const specSlug = (await getArtifact(seeded.specArtifactId)).slug;
    // The ask is drawn as a row of the tree and again as a card in the panel (R1, R12), so the
    // answer is written on the row the reader focused.
    const question = marginSlot(page).getByTestId(`ask-${seeded.openQuestion.askId}`);
    await question.getByRole("radio").first().check();
    await question.getByRole("button", { name: /^Answer/ }).click();
    await expect
      .poll(async () => (await getAsk(seeded.openQuestion.askId)).ask.state)
      .toBe("answered");

    await nodeRow(page, blockNodeId(specSlug, requirementBlockId(3)))
      .first()
      .focus();
    await page.keyboard.press("Enter");
    await page.keyboard.type(" Edited in place.");
    await expect
      .poll(async () => (await getIssueArtifactText(seeded.key, specSlug)).markdown)
      .toContain("Edited in place.");

    await presetToolbar(page).getByRole("button", { name: "Name version" }).click();
    const naming = page.getByRole("dialog", { name: "Name this version" });
    await naming.getByRole("textbox", { name: "What changed?" }).fill("Carried over");
    await naming.getByRole("button", { name: /^Save/ }).click();
    await expect
      .poll(async () =>
        (await getArtifact(seeded.specArtifactId)).versions.map((version) => version.summary)
      )
      .toContain("Carried over");
  });
});

// c-urls.
test("every address of the old page still resolves", async ({ browser }) => {
  // Eighteen cold loads of the issue page, each one a navigation and its reads: 28 s of the
  // 30 s default when it was last measured, and over it once the panel draws its own cards
  // first. The row carries a deadline of its own rather than racing that default.
  test.setTimeout(300_000);
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    const specSlug = (await getArtifact(seeded.specArtifactId)).slug;

    // The two addresses whose tabs are gone.
    await gotoIssue(page, `/issues/${seeded.key}/verification`);
    expect(await activePreset(page), "/verification does not open the Checks preset").toBe(
      PRESET_NAMES.checks
    );
    await expectIssuePageDrawn(page);
    await gotoIssue(page, `/issues/${seeded.key}/artifacts`);
    await expectIssuePageDrawn(page);
    expect(await openMenu(page).count(), "/artifacts does not open the node menu").toBe(1);

    // Every preset's own address.
    for (const preset of PRESET_ORDER) {
      await gotoIssue(page, presetAddress(seeded.key, preset));
      await expectIssuePageDrawn(page);
      expect(
        await activePreset(page),
        `${presetAddress(seeded.key, preset)} does not open the ${PRESET_NAMES[preset]} preset`
      ).toBe(PRESET_NAMES[preset]);
    }

    // The conversation and the activity: one list, in two of its four states.
    for (const [address, state] of [
      ["conversation", "words"],
      ["activity", "everything"],
    ] as const) {
      await gotoIssue(page, `/issues/${seeded.key}/${address}`);
      await expectIssuePageDrawn(page);
      expect(
        await conversationStateControl(page, state).getAttribute("aria-selected"),
        `/${address} does not open the conversation in its ${state} state`
      ).toBe("true");
    }

    // The children, a document, and each entry of the conversation.
    await gotoIssue(page, `/issues/${seeded.key}/children`);
    await expectIssuePageDrawn(page);
    expect(await childRows(page).count(), "/children opens no children").toBe(
      seeded.children.keys.length
    );

    await gotoIssue(page, `/issues/${seeded.key}/artifacts/${seeded.documents.cited}`);
    await expectIssuePageDrawn(page);
    expect(
      await nodeRow(page, seeded.documents.cited).count(),
      "/artifacts/<slug> opens no node for that document"
    ).toBeGreaterThan(0);

    // An ask the server indexed from a document's `ask` block *is* that block's node
    // (`e2e/issue-page.ts`), which is what the `?ask=` reading below asserts too: no row of the
    // tree carries the ask's own uuid, so each address names the node it opens.
    for (const [kind, id, node] of [
      ["asks", seeded.openQuestion.askId, blockNodeId(specSlug, OPEN_QUESTION_BLOCK_ID)],
      ["comments", seeded.comments.anchored, seeded.comments.anchored],
      ["messages", seeded.messages.root, seeded.messages.root],
    ] as const) {
      await gotoIssue(page, `/issues/${seeded.key}/${kind}/${id}`);
      await expectIssuePageDrawn(page);
      expect(
        await nodeRow(page, node).count(),
        `/${kind}/<id> opens no node for it`
      ).toBeGreaterThan(0);
    }

    // The queries, each doing what it does today.
    await gotoIssue(page, `/issues/${seeded.key}/spec?v=1`);
    await expectIssuePageDrawn(page);
    expect(
      (await presetToolbar(page).innerText()).replace(/\s+/g, " "),
      "?v= does not open that version of the document"
    ).toContain("1");

    await gotoIssue(page, `/issues/${seeded.key}?q=${encodeURIComponent("Requirement 2")}`);
    await expectIssuePageDrawn(page);
    expect(await filterBox(page).inputValue(), "?q= does not fill the preset's filter").toBe(
      "Requirement 2"
    );

    await gotoIssue(page, `/issues/${seeded.key}?ask=${seeded.openQuestion.askId}`);
    await expectIssuePageDrawn(page);
    expect(await focusedNodeId(page), "?ask= does not open that ask's node").toBe(
      blockNodeId(specSlug, OPEN_QUESTION_BLOCK_ID)
    );

    await gotoIssue(page, `/issues/${seeded.key}?comment=${seeded.comments.anchored}`);
    await expectIssuePageDrawn(page);
    expect(
      await nodeRow(page, seeded.comments.anchored).count(),
      "?comment= opens no node for that comment"
    ).toBe(1);

    // A `#b-` fragment scrolls to its node, including one in a document the page does not open
    // first: the plan's last task is in no preset the issue's own address opens.
    const planSlug = (await getArtifact(seeded.planArtifactId)).slug;
    await gotoIssue(page, `/issues/${seeded.key}#b-${planTaskBlockId(9)}`);
    await expectIssuePageDrawn(page);
    const row = rowOf(await drawnRows(page), blockNodeId(planSlug, planTaskBlockId(9)));
    expect(
      row,
      "a #b- fragment into a document the page does not open first draws no node"
    ).toBeDefined();
    expect(
      row?.box?.y ?? -1,
      "the fragment's node is not scrolled into the viewport"
    ).toBeGreaterThanOrEqual(0);
    expect(
      row?.box?.y ?? Number.MAX_SAFE_INTEGER,
      "the fragment's node is below the screen"
    ).toBeLessThanOrEqual(DESKTOP.height);
  });
});

// c-requests.
test("the first load stays inside its request budget", async ({ browser }) => {
  let baseline: { byRoute: string; total: number } | null = null;

  await phase(browser, seedRequestBaseline, async (page, seeded) => {
    const traffic = watchApiRequests(page);
    traffic.reset();
    await gotoIssue(page, `/issues/${seeded.key}`);
    await page.waitForLoadState("networkidle");
    const byRoute = JSON.stringify(traffic.byRoute());

    // No reference read for each pointer: the Spec tab sent 3 of them today and the Plan tab 14.
    const perPointer = traffic.requests.filter(
      (request) => request.route === "/api/v1/references"
    ).length;
    expect(
      perPointer,
      `the page read /api/v1/references ${perPointer} times, one for each pointer: ${byRoute}`
    ).toBe(0);

    // No read of the text of a document the page does not show. `GET /artifacts/{id}/text` is
    // that read and the only one: `GET /artifacts/{id}/blocks` answers `ArtifactBlock`s, which
    // carry a block's id, type, byte offsets, token, reference counts, attributes and a table's
    // cells and no prose at all (`packages/contracts/src/dispatch-api.ts`, `ArtifactBlock`), so
    // it is not a text read and naming it here would ban the one listing the tree is built from.
    // The spec is the one document the page does show on this address: the head draws its
    // summary's three sentences on every preset (R5, c-three-things), so its text is read.
    expect(
      traffic.requests
        .filter(
          (request) =>
            /\/text(\?|$)/.test(request.path) && !request.path.includes(seeded.specArtifactId)
        )
        .map((request) => request.path),
      "the first load read the stored text of a document it does not show"
    ).toEqual([]);

    await expectIssuePageDrawn(page);
    expect(
      traffic.total(),
      `the first load sent ${traffic.total()} requests under /api/: ${byRoute}`
    ).toBeLessThanOrEqual(40);
    baseline = { byRoute, total: traffic.total() };
  });

  // No request for each node: twice the nodes, the same count.
  await phase(browser, seedTwiceTheNodes, async (page, seeded) => {
    const traffic = watchApiRequests(page);
    traffic.reset();
    await gotoIssue(page, `/issues/${seeded.key}`);
    await page.waitForLoadState("networkidle");
    await expectIssuePageDrawn(page);
    const byRoute = JSON.stringify(traffic.byRoute());
    expect(
      traffic.total(),
      `twice the nodes cost ${traffic.total()} requests where the baseline cost ${baseline?.total}: ${byRoute} against ${baseline?.byRoute}`
    ).toBe(baseline?.total);
  });
});
