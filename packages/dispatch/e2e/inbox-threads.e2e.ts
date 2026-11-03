import { expect, type Locator, type Page, test } from "@playwright/test";
import type { InboxRow } from "../web/src/api/types";

import { setLiveSessions } from "./agents";
import {
  createAsk,
  createIssue,
  createProject,
  getInbox,
  patchIssue,
  requestApproval,
} from "./api";
import { setPendingCredentialRequests } from "./fake-broker-helpers";
import { hasReadingPane, inboxPane } from "./inbox-pane";
import {
  agentRowId,
  asSession,
  compactInboxLink,
  expectSidebarInboxCount,
  replyRowId,
  seedAcceptedSend,
  seedAgentThread,
  seedAskThread,
  seedBroadcastThread,
  seedCommentThread,
  seedMessageThread,
  THREAD_SESSION,
} from "./inbox-threads";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

/**
 * The Inbox's thread rows (INFRA-450): the Reply and Agent rows that stand in the one list
 * beside the reader's open asks and approvals, what the Type filter makes of them, and a reply
 * long enough to be clamped shown whole.
 *
 * Everything here is made up: this repository is public, so nothing names a real person, host or
 * project.
 */

test.beforeEach(async () => {
  await resetDatabase();
});

// ---------------------------------------------------------------------------
// A1: the old Replies link, and the one Inbox count.
// ---------------------------------------------------------------------------

test("the old Replies address opens the Inbox on its Reply rows, under one Inbox count", async ({
  browser,
}) => {
  await setLiveSessions([THREAD_SESSION]);
  await createProject({ key: "CORE", name: "Core" });
  // Two open asks waiting on the reader, one credential request, and three unread threads: the
  // Inbox count is the sum of exactly those.
  for (const title of ["Ledger rollout", "Mirror retries"]) {
    const issue = await createIssue({ project: "CORE", title });
    await createAsk(issue.key, { question: `${title}: ship tonight?` }, asSession());
  }
  await setPendingCredentialRequests([
    {
      approve_url: "https://secrets.internal.example/approve/record-alice",
      approver: "alice",
      identifiers: ["LEDGER_API_KEY"],
      kind: "agent_secret",
      record_id: "record-alice",
      requested_at: new Date().toISOString(),
    },
  ]);
  const commentIssue = await createIssue({ project: "CORE", title: "Audit clock" });
  const comment = await seedCommentThread(
    commentIssue.key,
    "Please fix the flaky clock test.",
    "Done: the clock test is green."
  );
  const messageIssue = await createIssue({ project: "CORE", title: "Build status" });
  const message = await seedMessageThread(
    messageIssue.key,
    "Where is the nightly build?",
    "On the mirror, replaying."
  );
  await seedAgentThread({ body: "What is the rollout status?", replies: ["Green."] });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/replies");

    // The address is the Inbox narrowed to its Reply rows, and the filter says so in a token.
    await expect(page).toHaveURL(/\/\?type=reply$/);
    await expect(page.locator('[data-inbox-filter-token="type:reply"]')).toHaveAttribute(
      "aria-label",
      "Remove filter Type: Reply"
    );
    const rows = page.locator("[data-inbox-row]");
    await expect(rows).toHaveCount(2);
    await expect(page.locator(`[data-inbox-row="${replyRowId(comment)}"]`)).toBeVisible();
    await expect(page.locator(`[data-inbox-row="${replyRowId(message)}"]`)).toBeVisible();

    // The sidebar has no Replies entry at all, and its Inbox entry carries the one count: two
    // asks, one credential request and three unread threads.
    const navigation = page.getByRole("navigation", { name: "Navigation" });
    await expect(navigation.getByRole("link", { name: /Replies/ })).toHaveCount(0);
    await expectSidebarInboxCount(page, 6);

    // Below 1280 px the same number is the compact header's one Inbox link - on a page that is
    // not the Inbox, where the header draws the count as a link to it rather than as plain text.
    await page.setViewportSize({ height: 900, width: 1024 });
    await page.goto("/agents");
    await expect(page.getByRole("heading", { name: "Agents" })).toBeVisible();
    await expect(compactInboxLink(page)).toHaveCount(1);
    await expect(compactInboxLink(page)).toHaveText("Inbox 6");
  } finally {
    await alice.close();
  }
});

// ---------------------------------------------------------------------------
// A2: one list of twelve rows, and the Type filter over it.
// ---------------------------------------------------------------------------

const DECISION_BLOCK = "ledger-order";
const DECISION_QUESTION = "Which service moves first?";
const decisionSpec = [
  "# Rollout order",
  "",
  "The ledger and the mirror each keep a clock of their own.",
  "",
  `:::ask{#${DECISION_BLOCK} urgency="med" multiple="false" state="open"}`,
  DECISION_QUESTION,
  "",
  "- Ledger first",
  "- Mirror first",
  ":::",
  "",
].join("\n");

/** What one seeded list holds, by row id, so each assertion names the row it means. */
interface Twelve {
  readonly accepted: string;
  readonly agents: readonly string[];
  readonly askThread: string;
  readonly replies: readonly string[];
}

/** The Inbox row `match` names, once the server has indexed the block that carries it. */
async function indexedRow(match: (row: InboxRow) => boolean): Promise<InboxRow> {
  await expect.poll(async () => (await getInbox()).some(match)).toBe(true);
  const row = (await getInbox()).find(match);
  if (row === undefined) throw new Error("the seeded question left the Inbox before it was read");
  return row;
}

/**
 * Twelve rows: three asks (two questions and a decision block), two approval requests, four
 * unread Reply threads and three unread Agent rows - plus a Send the session took as a turn of
 * its live conversation, which is no row at all.
 *
 * The ask thread's issue is bob's and its ask is answered, so the Inbox lists it as a Reply row
 * and as no Ask row: an open ask on an issue assigned to the reader is their Waiting-on-you row
 * instead (`myRepliesOwner`, the server's replies.go).
 */
async function seedTwelve(): Promise<Twelve> {
  await setLiveSessions([THREAD_SESSION]);
  await createProject({ key: "CORE", name: "Core" });

  for (const title of ["Lease handover", "Retry budget"]) {
    const issue = await createIssue({ project: "CORE", title });
    await createAsk(issue.key, { question: `${title}: what is the rule?` }, asSession());
  }
  await createIssue({ project: "CORE", spec: decisionSpec, title: "Rollout order" });
  await indexedRow((row) => row.block_id === DECISION_BLOCK);

  for (const title of ["Mirror runbook", "Clock drift"]) {
    const issue = await createIssue({
      project: "CORE",
      spec: `# ${title}\n\nThe mirror replays what the ledger wrote.\n`,
      title,
    });
    await requestApproval(
      issue.primary_artifact_id,
      { summary: `The ${title} draft.` },
      asSession()
    );
  }

  const replies: string[] = [];
  for (const [title, root, reply] of [
    ["Audit clock", "Please fix the flaky clock test.", "Done: the clock test is green."],
    ["Lease renewal", "The lease renews twice an hour.", "It renews once an hour now."],
  ] as const) {
    const issue = await createIssue({ project: "CORE", title });
    replies.push(await seedCommentThread(issue.key, root, reply));
  }
  const messageIssue = await createIssue({ project: "CORE", title: "Build status" });
  replies.push(
    await seedMessageThread(messageIssue.key, "Where is the nightly build?", "On the mirror.")
  );
  const askIssue = await createIssue({ project: "CORE", title: "Window size" });
  await patchIssue(askIssue.key, { assignee: "bob" });
  const askThread = await seedAskThread(
    askIssue.key,
    "Which window does the mirror replay?",
    "Replaying the last hour now."
  );
  replies.push(askThread);

  const agents = [
    await seedAgentThread({ body: "What is the rollout status?", replies: ["Green."] }),
    await seedBroadcastThread({ body: "Stand by for the rollout.", reply: "Standing by." }),
    await seedAgentThread({
      body: "Steer: hold the mirror.",
      delivery: "steer",
      replies: ["Holding."],
    }),
  ];
  const accepted = await seedAcceptedSend({
    body: "Steer: take this as your own turn.",
    reply: "Took it.",
  });
  return { accepted, agents, askThread, replies };
}

/** Opens the bar's Filter panel, which is shut until the reader presses **Filter**. */
async function openFilterFields(page: Page): Promise<Locator> {
  const fields = page.locator("[data-inbox-filter-fields]");
  if ((await fields.count()) === 0) {
    await page.locator("[data-inbox-filter]").click();
  }
  await expect(fields).toHaveCount(1);
  return fields;
}

/** Opens one field's popover and hands back its listbox. */
async function openField(page: Page, field: string, label: string): Promise<Locator> {
  const fields = await openFilterFields(page);
  const trigger = fields.getByRole("button", { name: `Filter by ${field}` });
  if ((await trigger.getAttribute("aria-expanded")) !== "true") {
    await trigger.click();
  }
  const options = page.getByRole("listbox", { name: `${label} options` });
  await expect(options).toHaveCount(1);
  return options;
}

test("the Type filter counts and narrows the one list of asks, approvals, replies and agent messages", async ({
  browser,
}) => {
  const seeded = await seedTwelve();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    const rows = page.locator("[data-inbox-row]");
    await expect(rows).toHaveCount(12);

    // Every thread row is unread: a bold title and a dot of its own.
    const unread = page.locator("[data-inbox-row] [aria-label='Unread']");
    await expect(unread).toHaveCount(7);
    const threadTitle = page
      .locator(`[data-inbox-row="${replyRowId(seeded.askThread)}"] a[data-inbox-owner]`)
      .first();
    await expect(threadTitle).toHaveClass(/font-semibold/);

    // The Send the session took as a turn of its live conversation is no row of the Inbox.
    await expect(page.locator(`[data-inbox-row="${agentRowId(seeded.accepted)}"]`)).toHaveCount(0);

    // The Type field counts every kind under the other four filters: the two question shapes
    // add to the three asks, and the threads are counted as what they are.
    const options = await openField(page, "type", "Type");
    const counted = async (name: string): Promise<number> => {
      const label = await options
        .getByRole("option", { name: new RegExp(`^${name}, `) })
        .getAttribute("aria-label");
      return Number((label ?? "").replace(`${name}, `, "").replace(" rows", ""));
    };
    expect((await counted("Question")) + (await counted("Decision"))).toBe(3);
    expect(await counted("Approval")).toBe(2);
    expect(await counted("Reply")).toBe(4);
    expect(await counted("Agent message")).toBe(3);

    // The search box narrows the values the popover lists.
    const search = page.getByRole("combobox", { name: "Search type" });
    await search.fill("rep");
    await expect(options.getByRole("option")).toHaveCount(1);
    await expect(options.getByRole("option").first()).toHaveAttribute(
      "aria-label",
      "Reply, 4 rows"
    );
    await search.fill("");

    // Each value alone lists only its own rows.
    for (const [value, name, count] of [
      ["approval", "Approval", 2],
      ["reply", "Reply", 4],
      ["agent", "Agent message", 3],
    ] as const) {
      await options.getByRole("option", { name: new RegExp(`^${name}, `) }).click();
      await expect(rows).toHaveCount(count);
      await expect(page.locator(`[data-inbox-row] [data-inbox-kind="${value}"]`)).toHaveCount(
        count
      );
      await options.getByRole("option", { name: new RegExp(`^${name}, `) }).click();
      await expect(rows).toHaveCount(12);
    }

    // Two values together are the union, and a reload keeps what the reader chose.
    await options.getByRole("option", { name: /^Approval, / }).click();
    await options.getByRole("option", { name: /^Reply, / }).click();
    await expect(rows).toHaveCount(6);
    await expect(page).toHaveURL(/type=approval%2Creply/);
    await page.reload();
    await expect(rows).toHaveCount(6);
    await expect(page.locator('[data-inbox-filter-token="type:approval"]')).toHaveCount(1);
    await expect(page.locator('[data-inbox-filter-token="type:reply"]')).toHaveCount(1);
  } finally {
    await alice.close();
  }
});

test("the drawer's own bar filters the drawer alone, and the Inbox page keeps its list", async ({
  browser,
}) => {
  await seedTwelve();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    // The Inbox page, narrowed to its Reply rows: the list the drawer must not touch.
    await page.goto("/?type=reply");
    await expect(page.locator("[data-inbox-row]")).toHaveCount(4);

    // The drawer is offered on every page but the Inbox itself, so the reader opens it with `i`
    // from a page of its own.
    await page.goto("/agents");
    await expect(page.getByRole("heading", { name: "Agents" })).toBeVisible();
    await page.locator("body").focus();
    await page.keyboard.press("i");
    const drawer = page.getByTestId("inbox-drawer");
    await expect(drawer).toBeVisible();
    const drawerRows = drawer.locator("[data-inbox-row]");
    await expect(drawerRows).toHaveCount(12);

    await drawer.locator("[data-inbox-filter]").click();
    await drawer.getByRole("button", { name: "Filter by type" }).click();
    const options = page.getByRole("listbox", { name: "Type options" });
    await options.getByRole("option", { name: /^Agent message, / }).click();
    await expect(drawerRows).toHaveCount(3);
    // The drawer's pick is its own: the address under it never moved.
    await expect(page).toHaveURL(/\/agents$/);

    await page.keyboard.press("Escape");
    await drawer.getByRole("button", { name: "Close" }).click();
    await expect(drawer).toHaveCount(0);
    await page.goto("/?type=reply");
    await expect(page.locator("[data-inbox-row]")).toHaveCount(4);
    await expect(page.locator('[data-inbox-filter-token="type:agent"]')).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

// ---------------------------------------------------------------------------
// A3: a long reply, shown whole.
// ---------------------------------------------------------------------------

/** Forty lines holding a list, a fenced code block and a table: a reply no surface may clamp. */
const LONG_REPLY = [
  "Here is the whole rollout note, in full.",
  "",
  ...Array.from({ length: 10 }, (_, index) => `- Step ${index + 1} of the mirror replay.`),
  "",
  "```sh",
  ...Array.from({ length: 6 }, (_, index) => `dispatch replay --window ${index + 1}h`),
  "```",
  "",
  "| Window | Attempts |",
  "| --- | --- |",
  ...Array.from({ length: 6 }, (_, index) => `| ${index + 1}h | ${index + 2} |`),
  "",
  ...Array.from({ length: 7 }, (_, index) => `Closing note ${index + 1} on the replay budget.`),
  "",
  "The last line of the reply, which has to be reachable.",
].join("\n");

const LAST_LINE = "The last line of the reply, which has to be reachable.";

/**
 * Whether any element of the drawn message is clamped: a `line-clamp-*` class, or a box that
 * hides what does not fit in it. An element whose overflow is visible hides nothing - an
 * inline link's own hit-box padding (`.dispatch-inline-link`, 6px above and below a 16px line
 * box) paints past its line and cuts no word - so only a box that clips is a fault, and each
 * one names its classes so the clamp can be found.
 */
async function clampedParts(turn: Locator): Promise<string[]> {
  return turn.evaluate((node) => {
    const faults: string[] = [];
    for (const element of [node, ...node.querySelectorAll("*")]) {
      if (!(element instanceof HTMLElement)) continue;
      if (/\bline-clamp-/.test(element.className)) faults.push(`clamped: ${element.tagName}`);
      const clips = getComputedStyle(element).overflowY !== "visible";
      if (clips && element.scrollHeight > element.clientHeight + 1) {
        faults.push(
          `cut: ${element.tagName}.${element.className} ${element.scrollHeight} > ${element.clientHeight}`
        );
      }
    }
    return faults;
  });
}

test("a long reply shows whole in the reading pane and on a card @phone", async ({ browser }) => {
  await setLiveSessions([THREAD_SESSION]);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Replay budget" });
  const thread = await seedCommentThread(issue.key, "How far does the replay go?", LONG_REPLY);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    const row = page.locator(`[data-inbox-row="${replyRowId(thread)}"]`);
    await expect(row).toBeVisible();
    // The pane shows the thread of the selected row; below its width the row is its own card.
    const body = hasReadingPane(page)
      ? inboxPane(page).locator("[data-inbox-thread-body]")
      : page.locator(`[data-inbox-row="${replyRowId(thread)}"] [data-inbox-thread-body]`);
    if (hasReadingPane(page)) {
      await row.locator("time").first().click();
      await expect(row).toHaveAttribute("data-inbox-selected", "true");
    }
    await expect(body).toBeVisible();

    // The reply renders as what it is: a list, a code block and a table.
    const turn = body.locator("[data-inbox-thread-turn]").last();
    await expect(turn.locator("li")).toHaveCount(10);
    await expect(turn.locator("pre")).toHaveCount(1);
    await expect(turn.locator("table")).toHaveCount(1);
    await expect(turn.locator("table td")).toHaveCount(12);

    // Its last line is reachable by scrolling, and nothing of it is clamped or cut.
    const last = turn.getByText(LAST_LINE);
    await last.scrollIntoViewIfNeeded();
    await expect(last).toBeInViewport();
    expect(await clampedParts(turn)).toEqual([]);
  } finally {
    await alice.close();
  }
});
