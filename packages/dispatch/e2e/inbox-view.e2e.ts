import { expect, type Locator, type Page, test } from "@playwright/test";
import type { InboxRow, IssuePriority } from "../web/src/api/types";

import { setLiveSessions } from "./agents";
import {
  createAsk,
  createComment,
  createIssue,
  createProject,
  getInbox,
  listMyReplies,
  markThreadRead,
  patchIssue,
  requestApproval,
} from "./api";
import { inboxList, inboxPane } from "./inbox-pane";
import {
  agentRowId,
  asSession,
  replyRowId,
  seedAgentThread,
  seedBulkReplyThreads,
  seedCommentThread,
  seedMessageThread,
  THREAD_SESSION,
  unreadThreadRows,
} from "./inbox-threads";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

/**
 * The Inbox's **View** menu and its **Filter** bar over the one list (INFRA-450): what each of
 * the five fields filters, bands and orders by, how a band folds, how the reading pane is sized,
 * and that a list of a hundred and twenty threads is ordered and counted by the same rules.
 *
 * Everything here is made up: this repository is public, so nothing names a real person, host or
 * project.
 */

/** The list this file reads: every row, whoever holds it, so the fields have something to say. */
const INBOX = "/?assignee=everyone";

/** One seeded row, as the five fields describe it. The expectations are computed from this
 *  table, so a count, a band and an order all mean the same thing by the same row. */
interface ViewRow {
  readonly assignee: string;
  readonly id: string;
  readonly issueKey: string | undefined;
  readonly priority: string;
  readonly project: string;
  readonly type: string;
  readonly urgency: string;
}

const DECISION_BLOCK = "rollout-order";
const decisionSpec = [
  "# Rollout order",
  "",
  "The ledger and the mirror each keep a clock of their own.",
  "",
  `:::ask{#${DECISION_BLOCK} urgency="med" multiple="false" state="open"}`,
  "Which service moves first?",
  "",
  "- Ledger first",
  "- Mirror first",
  ":::",
  "",
].join("\n");

/** The Inbox row `match` names, once the server has indexed the block that carries it. */
async function indexedRow(match: (row: InboxRow) => boolean): Promise<InboxRow> {
  await expect.poll(async () => (await getInbox()).some(match)).toBe(true);
  const row = (await getInbox()).find(match);
  if (row === undefined) throw new Error("the seeded question left the Inbox before it was read");
  return row;
}

/**
 * Ten unread rows covering every kind, every priority and none, every urgency and none, two
 * projects and none, and three holders - plus one thread the reader has already read, which
 * only **Show read threads** lists.
 */
async function seedEveryField(): Promise<{
  read: string;
  rows: readonly ViewRow[];
}> {
  await setLiveSessions([THREAD_SESSION]);
  await createProject({ key: "CORE", name: "Core" });
  await createProject({ key: "MIRROR", name: "Mirror" });
  const rows: ViewRow[] = [];

  const question = async (
    title: string,
    priority: IssuePriority | null,
    urgency: "blocking" | "high" | "low" | "med" | undefined,
    assignee: string | null
  ): Promise<string> => {
    const issue = await createIssue({ project: "CORE", title });
    if (priority !== null) await patchIssue(issue.key, { priority });
    if (assignee !== "alice") await patchIssue(issue.key, { assignee });
    const ask = await createAsk(
      issue.key,
      { question: `${title}: what is the rule?`, ...(urgency === undefined ? {} : { urgency }) },
      asSession()
    );
    rows.push({
      assignee: assignee ?? "unassigned",
      id: ask.id,
      issueKey: issue.key,
      priority: priority === null ? "none" : `P${priority}`,
      project: "CORE",
      type: "question",
      // An ask whose author names no urgency is asked at `med` (the server's asks.go), so
      // only a thread row ever has none at all.
      urgency: urgency ?? "med",
    });
    return issue.key;
  };

  await question("Ledger rollout", 0, "blocking", "alice");
  await question("Mirror retries", 1, "high", "alice");
  // Bob's question, which a clarification hands back to the session: the one row under Waiting
  // on agents, so that banding has more than one band to show.
  const handed = await question("Lease handover", 2, "low", "bob");
  const handedAsk = rows[rows.length - 1];
  await createComment(
    handed,
    { ask_id: handedAsk?.id, body: "Checking the lease now.", turn: "agent" },
    asSession()
  );
  await question("Clock drift", 3, "med", "alice");
  await question("Replay budget", null, undefined, null);

  const decisionIssue = await createIssue({
    project: "CORE",
    spec: decisionSpec,
    title: "Rollout order",
  });
  await patchIssue(decisionIssue.key, { priority: 1 });
  const decision = await indexedRow((row) => row.block_id === DECISION_BLOCK);
  rows.push({
    assignee: "alice",
    id: decision.id,
    issueKey: decisionIssue.key,
    priority: "P1",
    project: "CORE",
    type: "decision",
    urgency: "med",
  });

  const approvalIssue = await createIssue({
    project: "CORE",
    spec: "# Window size\n\nThe mirror replays the last hour.\n",
    title: "Window size",
  });
  await patchIssue(approvalIssue.key, { priority: 2 });
  const approval = await requestApproval(
    approvalIssue.primary_artifact_id,
    { summary: "The window draft." },
    asSession()
  );
  rows.push({
    assignee: "alice",
    id: approval.ask.id,
    issueKey: approvalIssue.key,
    // A review request is asked at `high` (the server's reviews.go), which no caller sets.
    priority: "P2",
    project: "CORE",
    type: "approval",
    urgency: "high",
  });

  const commentIssue = await createIssue({ project: "CORE", title: "Audit clock" });
  await patchIssue(commentIssue.key, { priority: 3 });
  const comment = await seedCommentThread(
    commentIssue.key,
    "Please fix the flaky clock test.",
    "Done: the clock test is green."
  );
  rows.push({
    assignee: "alice",
    id: replyRowId(comment),
    issueKey: commentIssue.key,
    priority: "P3",
    project: "CORE",
    type: "reply",
    urgency: "none",
  });

  const mirrorIssue = await createIssue({ project: "MIRROR", title: "Mirror runbook" });
  const message = await seedMessageThread(
    mirrorIssue.key,
    "Where is the nightly build?",
    "On the mirror, replaying."
  );
  rows.push({
    assignee: "alice",
    id: replyRowId(message),
    issueKey: mirrorIssue.key,
    priority: "none",
    project: "MIRROR",
    type: "reply",
    urgency: "none",
  });

  const agent = await seedAgentThread({ body: "What is the rollout status?", replies: ["Green."] });
  rows.push({
    assignee: "alice",
    id: agentRowId(agent),
    issueKey: undefined,
    priority: "none",
    project: "none",
    type: "agent",
    urgency: "none",
  });

  // One thread the reader has already read: no row under Unread, a row drawn as read under
  // **Show read threads**.
  const readIssue = await createIssue({ project: "CORE", title: "Retired note" });
  const read = await seedCommentThread(
    readIssue.key,
    "Is the nightly mirror job still at 02:00?",
    "It moved to 03:00 last week."
  );
  const listed = await listMyReplies({ state: "unread" });
  const readRow = listed.rows.find((row) => row.thread_id === read);
  if (readRow === undefined) throw new Error("the thread to mark read was never listed");
  await markThreadRead(read, readRow.reply.created_at);

  return { read: replyRowId(read), rows };
}

/** When each row was last written in, as the two reads answer it: what `newest` orders by. */
async function lastActivity(): Promise<ReadonlyMap<string, string>> {
  const at = new Map<string, string>();
  for (const ask of await getInbox()) {
    at.set(ask.id, ask.last_reply?.created_at ?? ask.created_at);
  }
  for (const thread of (await listMyReplies({ kind: ["reply", "agent"], state: "all" })).rows) {
    at.set(
      `${thread.kind === "agent" ? "agent" : "reply"}:${thread.thread_id}`,
      thread.reply.created_at
    );
  }
  return at;
}

/** Where a value sits among its own field's, as `valueRank` orders them. */
const VALUE_ORDER: Record<string, readonly string[]> = {
  assignee: [],
  priority: ["P0", "P1", "P2", "P3"],
  project: [],
  type: ["question", "decision", "approval", "reply", "agent"],
  urgency: ["blocking", "high", "med", "low"],
};

/** What each value is called where the reader reads it: a Filter row and a filter token. */
const VALUE_LABELS: Record<string, string> = {
  agent: "Agent message",
  approval: "Approval",
  blocking: "Blocking",
  decision: "Decision",
  high: "High",
  low: "Low",
  med: "Medium",
  question: "Question",
  reply: "Reply",
  unassigned: "Unassigned",
};

/** The plural each Type band is headed by. */
const TYPE_BANDS: Record<string, string> = {
  agent: "Agent messages",
  approval: "Approvals",
  decision: "Decisions",
  question: "Questions",
  reply: "Replies",
};

function valueLabel(field: string, value: string): string {
  if (value === "none") {
    if (field === "priority") return "No priority";
    if (field === "urgency") return "No urgency";
    return "No project";
  }
  return VALUE_LABELS[value] ?? value;
}

function bandLabel(field: string, value: string): string {
  if (field === "type") return TYPE_BANDS[value] ?? value;
  return valueLabel(field, value);
}

/** The values of `field` the seed holds, in the order the control and the bands list them. */
function valuesOf(rows: readonly ViewRow[], field: keyof ViewRow): string[] {
  const fixed = VALUE_ORDER[field] ?? [];
  const seen = [...new Set(rows.map((row) => String(row[field])))];
  const ranked = (value: string) =>
    value === "none" || value === "unassigned"
      ? Number.MAX_SAFE_INTEGER
      : fixed.length === 0
        ? 0
        : fixed.indexOf(value);
  return seen.sort((left, right) => ranked(left) - ranked(right) || left.localeCompare(right));
}

function countsOf(rows: readonly ViewRow[], field: keyof ViewRow): Map<string, number> {
  const counts = new Map<string, number>();
  for (const row of rows) {
    const value = String(row[field]);
    counts.set(value, (counts.get(value) ?? 0) + 1);
  }
  return counts;
}

/** The row ids in the order `sort` puts them under **Group by None**: the field's own order,
 *  `none` last, ties newest first and the row id breaking a tie that reaches it. */
function sortedIds(
  rows: readonly ViewRow[],
  field: keyof ViewRow | "newest",
  at: ReadonlyMap<string, string>
): string[] {
  const order = field === "newest" ? [] : valuesOf(rows, field);
  return [...rows]
    .sort((left, right) => {
      if (field !== "newest") {
        const byField = order.indexOf(String(left[field])) - order.indexOf(String(right[field]));
        if (byField !== 0) return byField;
      }
      const leftAt = at.get(left.id) ?? "";
      const rightAt = at.get(right.id) ?? "";
      if (leftAt !== rightAt) return leftAt < rightAt ? 1 : -1;
      return left.id.localeCompare(right.id);
    })
    .map((row) => row.id);
}

/** The ids the list is drawing, top to bottom. */
async function drawnIds(page: Page): Promise<string[]> {
  return page
    .locator("[data-inbox-row]")
    .evaluateAll((nodes) => nodes.map((node) => node.getAttribute("data-inbox-row") ?? ""));
}

/** Every band heading the list is drawing, top to bottom, with its count. */
async function bandHeadings(page: Page): Promise<string[]> {
  return inboxList(page)
    .locator("h2 button")
    .evaluateAll((nodes) => nodes.map((node) => (node.textContent ?? "").trim()));
}

/** Opens the bar's Filter panel, which is shut until the reader presses **Filter**. */
async function openFilterFields(page: Page): Promise<Locator> {
  const fields = page.locator("[data-inbox-filter-fields]");
  if ((await fields.count()) === 0) await page.locator("[data-inbox-filter]").click();
  await expect(fields).toHaveCount(1);
  return fields;
}

/** Opens one field's popover and hands back its listbox, shutting whichever was open. */
async function openField(page: Page, field: string): Promise<Locator> {
  const fields = await openFilterFields(page);
  const trigger = fields.getByRole("button", { name: `Filter by ${field}` });
  if ((await trigger.getAttribute("aria-expanded")) !== "true") await trigger.click();
  const label = field === "assignee" ? "Assignee" : field[0]?.toUpperCase() + field.slice(1);
  const options = page.getByRole("listbox", { name: `${label} options` });
  await expect(options).toHaveCount(1);
  return options;
}

/** The **View** menu, opened. */
async function openView(page: Page): Promise<Locator> {
  const menu = page.getByRole("menu", { name: "View" });
  if ((await menu.count()) === 0) await page.locator("[data-inbox-view]").click();
  await expect(menu).toHaveCount(1);
  return menu;
}

/** Picks one choice of one **View** section; the sections share value words, so each is read
 *  through its own fieldset. */
async function chooseView(page: Page, section: string, value: string): Promise<void> {
  const menu = await openView(page);
  await menu
    .getByRole("group", { name: section })
    .locator(`[data-inbox-view-choice="${value}"]`)
    .click();
}

test.beforeEach(async () => {
  await resetDatabase();
});

test("a row wears its marks without overlap, shows its issue key whole, and the bar holds three buttons", async ({
  browser,
}) => {
  const { rows } = await seedEveryField();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(INBOX);
    await expect(page.locator("[data-inbox-row]")).toHaveCount(rows.length);

    // Every row starts with its kind's icon, at one left edge, before its title and its marks.
    const geometry = await page.locator("[data-inbox-row]").evaluateAll((nodes) =>
      nodes.map((node) => {
        const box = (element: Element | null) => element?.getBoundingClientRect() ?? null;
        const icon = node.querySelector("[data-inbox-kind]");
        const title = node.querySelector("a[data-inbox-owner], span.grow");
        const key = [...node.querySelectorAll("a > span")].find((span) =>
          /^[A-Z][A-Z0-9]*-\d+$/.test((span.textContent ?? "").trim())
        );
        return {
          icon: box(icon),
          id: node.getAttribute("data-inbox-row") ?? "",
          key:
            key === undefined
              ? null
              : {
                  box: key.getBoundingClientRect(),
                  clientWidth: key.clientWidth,
                  scrollWidth: key.scrollWidth,
                  text: (key.textContent ?? "").trim(),
                },
          kind: icon?.getAttribute("data-inbox-kind") ?? null,
          priority: box(node.querySelector("[data-inbox-priority]")),
          title: box(title),
          urgency: box(node.querySelector("[data-inbox-urgency]")),
        };
      })
    );
    const byId = new Map(rows.map((row) => [row.id, row]));
    const lefts = new Set<number>();
    for (const row of geometry) {
      const seeded = byId.get(row.id);
      expect(seeded, row.id).toBeDefined();
      expect(row.kind, row.id).toBe(seeded?.type);
      expect(row.icon, row.id).not.toBeNull();
      lefts.add(Math.round(row.icon?.left ?? -1));
      expect((row.icon?.left ?? 0) < (row.title?.left ?? 0), row.id).toBe(true);
      // The priority tag and, for an ask, the urgency mark.
      expect(row.priority === null, `${row.id} priority`).toBe(seeded?.priority === "none");
      expect(row.urgency === null, `${row.id} urgency`).toBe(seeded?.urgency === "none");
      // No two marks overlap: each box is clear of the others.
      const boxes = [
        ["priority", row.priority],
        ["urgency", row.urgency],
        ["title", row.title],
        ["key", row.key?.box ?? null],
      ] as const;
      for (const [leftName, left] of boxes) {
        for (const [rightName, right] of boxes) {
          if (leftName >= rightName || left === null || right === null) continue;
          const overlaps =
            left.left < right.right &&
            right.left < left.right &&
            left.top < right.bottom &&
            right.top < left.bottom;
          expect(overlaps, `${row.id}: ${leftName} over ${rightName}`).toBe(false);
        }
      }
      // The issue key shows whole: its own text, in a box its text fits.
      if (seeded?.issueKey === undefined) {
        expect(row.key, row.id).toBeNull();
      } else {
        expect(row.key?.text, row.id).toBe(seeded.issueKey);
        expect(row.key?.scrollWidth ?? 0, row.id).toBeLessThanOrEqual(
          (row.key?.clientWidth ?? 0) + 1
        );
      }
    }
    expect(lefts.size).toBe(1);

    // The bar carries three buttons and no other control of this issue's.
    await page.goto("/");
    await expect(page.locator("[data-inbox-row]").first()).toBeVisible();
    await expect(page.locator("[data-inbox-bar] button")).toHaveText([
      "Filter",
      "Mark all read",
      "View",
    ]);
  } finally {
    await alice.close();
  }
});

test("each of the five fields filters, bands and orders the list", async ({ browser }) => {
  const { rows } = await seedEveryField();
  const at = await lastActivity();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(INBOX);
    const listed = page.locator("[data-inbox-row]");
    await expect(listed).toHaveCount(rows.length);

    for (const field of ["priority", "urgency", "type", "project", "assignee"] as const) {
      const values = valuesOf(rows, field);
      const counts = countsOf(rows, field);

      // The Filter control counts each value and narrows the list to the one chosen.
      const options = await openField(page, field);
      const offeredOptions = options.getByRole("option");
      // `count()` waits for nothing, so the popover's rows are waited for before they are
      // counted: every value of this field, plus `none` where the field has one.
      await expect(offeredOptions).toHaveCount(values.length);
      const offered = values.length;
      for (const value of values) {
        const name = valueLabel(field, value);
        const option = options.getByRole("option", { name: new RegExp(`^${name},`) });
        await expect(option, `${field}:${value}`).toHaveAttribute(
          "aria-label",
          new RegExp(`\\b${counts.get(value)} rows$`)
        );
      }
      const chosen = values[0] ?? "";
      const chosenName = valueLabel(field, chosen);
      await options.getByRole("option", { name: new RegExp(`^${chosenName},`) }).click();
      await expect(listed).toHaveCount(counts.get(chosen) ?? 0);
      expect(new Set(await drawnIds(page))).toEqual(
        new Set(rows.filter((row) => String(row[field]) === chosen).map((row) => row.id))
      );

      // Its search box narrows the values the popover lists.
      const searchLabel = field === "assignee" ? "Search people" : `Search ${field}`;
      const search = page.getByRole("combobox", { name: searchLabel });
      await search.fill(chosenName.slice(0, 3));
      // The search narrows the rows in a render of its own, which a count would not wait for.
      await expect(offeredOptions).not.toHaveCount(offered);
      const matching = await offeredOptions.count();
      expect(matching, `${field} search`).toBeGreaterThan(0);
      expect(matching, `${field} search`).toBeLessThan(offered);
      await search.fill("");

      // Off again, so the next field counts the whole list.
      await options.getByRole("option", { name: new RegExp(`^${chosenName},`) }).click();
      await expect(listed).toHaveCount(rows.length);
      await page.keyboard.press("Escape");

      // Banding by the field: one band per value, with its own count, in the field's order.
      await chooseView(page, "Group by", field);
      expect(await bandHeadings(page), `group by ${field}`).toEqual(
        values.map((value) => `${bandLabel(field, value)} (${counts.get(value)})`)
      );

      // Ordering by the field, over one list: the field's order, ties newest first.
      await chooseView(page, "Group by", "none");
      await chooseView(page, "Sort by", field);
      expect(await drawnIds(page), `sort by ${field}`).toEqual(sortedIds(rows, field, at));
      await chooseView(page, "Sort by", "newest");
      expect(await drawnIds(page), "sort by newest").toEqual(sortedIds(rows, "newest", at));
      await chooseView(page, "Group by", "priority");
    }

    // One list under Group by None, and today's turn bands under Waiting on.
    await chooseView(page, "Group by", "none");
    expect(await bandHeadings(page)).toEqual([]);
    await expect(listed).toHaveCount(rows.length);
    await chooseView(page, "Group by", "waiting-on");
    expect(await bandHeadings(page)).toEqual([
      `Waiting on a person (${rows.length - 1})`,
      "Waiting on agents (1)",
    ]);
  } finally {
    await alice.close();
  }
});

test("a band folds and stays folded, and Show read threads lists what the reader has read", async ({
  browser,
}) => {
  const { read, rows } = await seedEveryField();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(INBOX);
    const listed = page.locator("[data-inbox-row]");
    await expect(listed).toHaveCount(rows.length);

    // Every band folds and unfolds.
    for (const value of valuesOf(rows, "priority")) {
      const label = `${valueLabel("priority", value)} (${countsOf(rows, "priority").get(value)})`;
      const heading = page.getByRole("button", { name: label });
      const held = countsOf(rows, "priority").get(value) ?? 0;
      await heading.click();
      await expect(listed).toHaveCount(rows.length - held);
      await heading.click();
      await expect(listed).toHaveCount(rows.length);
    }

    // A folded band is still folded after a reload: the fold is the reader's own, kept for
    // their browser rather than in the address.
    const p0 = page.getByRole("button", { name: "P0 (1)" });
    await p0.click();
    await expect(listed).toHaveCount(rows.length - 1);
    await page.reload();
    await expect(listed).toHaveCount(rows.length - 1);
    await expect(p0).toHaveAttribute("aria-expanded", "false");
    await p0.click();
    await expect(listed).toHaveCount(rows.length);

    // Show read threads puts the thread the reader has read among the rows, drawn as read.
    await expect(page.locator(`[data-inbox-row="${read}"]`)).toHaveCount(0);
    await chooseView(page, "Show read threads", "all");
    await expect(page).toHaveURL(/threads=all/);
    const readRow = page.locator(`[data-inbox-row="${read}"]`);
    await expect(readRow).toBeVisible();
    await expect(readRow.locator("[aria-label='Unread']")).toHaveCount(0);
    await expect(readRow.locator("a[data-inbox-owner]").first()).not.toHaveClass(/font-semibold/);
    await expect(listed).toHaveCount(rows.length + 1);
  } finally {
    await alice.close();
  }
});

test("the divider resizes the two columns, the width survives a reload, and the pane opens at about 60%", async ({
  browser,
}) => {
  const { rows } = await seedEveryField();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(INBOX);
    await expect(page.locator("[data-inbox-row]")).toHaveCount(rows.length);
    const list = inboxList(page);
    const pane = inboxPane(page);
    await expect(pane).toBeVisible();

    const widths = async (): Promise<{ list: number; pane: number }> => {
      const [listBox, paneBox] = await Promise.all([list.boundingBox(), pane.boundingBox()]);
      if (listBox === null || paneBox === null) throw new Error("the Inbox drew one column");
      return { list: listBox.width, pane: paneBox.width };
    };
    const opened = await widths();
    expect(opened.pane / (opened.pane + opened.list)).toBeCloseTo(0.6, 2);

    // Dragging the line left widens the pane and narrows the list.
    const divider = page.getByTestId("inbox-pane-divider");
    await expect(divider).toHaveAttribute("aria-label", "Resize reading pane");
    const handle = await divider.boundingBox();
    if (handle === null) throw new Error("the divider drew no box");
    await page.mouse.move(handle.x + handle.width / 2, handle.y + handle.height / 2);
    await page.mouse.down();
    await page.mouse.move(handle.x - 120, handle.y + handle.height / 2, { steps: 8 });
    await page.mouse.up();
    const dragged = await widths();
    expect(dragged.pane).toBeGreaterThan(opened.pane + 50);
    expect(dragged.list).toBeLessThan(opened.list - 50);

    // The reader's browser keeps it.
    await page.reload();
    await expect(page.locator("[data-inbox-row]")).toHaveCount(rows.length);
    const reloaded = await widths();
    expect(Math.abs(reloaded.pane - dragged.pane)).toBeLessThanOrEqual(2);
  } finally {
    await alice.close();
  }
});

// ---------------------------------------------------------------------------
// A11: a hundred and twenty threads.
// ---------------------------------------------------------------------------

test("a hundred and twenty threads sort by priority, count up, and follow their issue's priority live", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const urgent = await createIssue({ project: "CORE", title: "Ledger rollout" });
  await patchIssue(urgent.key, { priority: 0 });
  const later = await createIssue({ project: "CORE", title: "Mirror retries" });
  await patchIssue(later.key, { priority: 2 });
  // A hundred and twenty threads across the two priorities, in one statement apiece. The P0
  // issue's one thread is seeded wholly behind the rest, so its last message is the oldest of
  // all of them and recency alone would put its row last.
  await seedBulkReplyThreads({ count: 119, issueKey: later.key, prefix: "Mirror" });
  await seedBulkReplyThreads({
    count: 1,
    issueKey: urgent.key,
    olderThan: 600,
    prefix: "Ledger",
  });
  expect(await unreadThreadRows()).toBe(120);
  const listed = await listMyReplies({ state: "unread" });
  const oldest = [...listed.rows].sort((left, right) =>
    left.reply.created_at < right.reply.created_at ? -1 : 1
  )[0];
  expect(oldest?.subject).toBe("Ledger root 1");

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    // Sorted by priority, over one list: the oldest message of all is still the first row,
    // because it is the P0 thread.
    await page.goto("/?group=none&sort=priority");
    const rows = page.locator("[data-inbox-row]");
    await expect(rows.first()).toBeVisible();
    expect((await drawnIds(page))[0]).toBe(replyRowId(oldest?.thread_id ?? ""));

    // The Priority filter's counts add up to every thread.
    const options = await openField(page, "priority");
    await expect(options.getByRole("option", { name: /^P0,/ })).toHaveAttribute(
      "aria-label",
      "P0, 1 rows"
    );
    await expect(options.getByRole("option", { name: /^P2,/ })).toHaveAttribute(
      "aria-label",
      "P2, 119 rows"
    );
    await page.keyboard.press("Escape");

    // Banded by priority, the P0 thread's row follows its issue to P3 with no reload.
    await page.goto("/?sort=priority");
    const row = page.locator(`[data-inbox-row="${replyRowId(oldest?.thread_id ?? "")}"]`);
    await expect(row).toHaveAttribute("data-inbox-group", "P0");
    await expect(page.getByRole("button", { name: "P0 (1)" })).toBeVisible();
    await patchIssue(urgent.key, { priority: 3 });
    await expect(row).toHaveAttribute("data-inbox-group", "P3");
    await expect(page.getByRole("button", { name: "P3 (1)" })).toBeVisible();
  } finally {
    await alice.close();
  }
});
