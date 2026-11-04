import { expect, type Locator, type Page, test } from "@playwright/test";

import { exchangeDelivery } from "../web/src/features/agents/exchange-delivery";
import { setLiveSessions } from "./agents";
import {
  acceptAgentDelivery,
  createAgentMessage,
  createArtifactComment,
  createComment,
  createIssue,
  createMessage,
  createProject,
  createProjectDocument,
  listAgentMessages,
  listAgents,
  listComments,
  listIssueMessages,
  listMyReplies,
  markThreadRead,
  putAgentState,
} from "./api";
import { inboxPane } from "./inbox-pane";
import {
  agentRowId,
  asSession,
  holdReadMarks,
  replyRowId,
  seedAgentThread,
  sessionActor,
  THREAD_SESSION,
} from "./inbox-threads";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

/**
 * The Inbox's reading pane over a thread (INFRA-450): a comment thread anchored in a document
 * opened at its section with the thread under its own block, the fold and the reply box of a
 * long thread, the three thread shapes drawn alike, and a reply sent from the pane.
 *
 * Everything here is made up: this repository is public, so nothing names a real person, host or
 * project.
 */

test.beforeEach(async () => {
  await resetDatabase();
});

/** Selects a row the way the pointer does: the click lands on the row's own time. */
async function selectRow(page: Page, id: string): Promise<Locator> {
  const row = page.locator(`[data-inbox-row="${id}"]`);
  await expect(row).toBeVisible();
  await row.locator("time").first().click();
  await expect(row).toHaveAttribute("data-inbox-selected", "true");
  return row;
}

// ---------------------------------------------------------------------------
// A4: the anchored document thread.
// ---------------------------------------------------------------------------

const SECTION_HEADING = "Replay window";
const ANCHORED_LINE = "The mirror replays the ledger on a cold start, window by window.";
const ANCHOR_QUOTE = "replays the ledger";
const documentContent = [
  "# Mirror runbook",
  "",
  ...Array.from({ length: 30 }, (_, index) => [
    `Runbook note ${index + 1}: the ledger and the mirror each keep a clock of their own.`,
    "",
  ]).flat(),
  `## ${SECTION_HEADING}`,
  "",
  ANCHORED_LINE,
  "",
  ...Array.from({ length: 20 }, (_, index) => [
    `Window note ${index + 1}: the replay budget is three attempts per lease.`,
    "",
  ]).flat(),
].join("\n");

test("the pane opens an anchored thread's document at its section, quote lit, thread under its block", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const document = await createProjectDocument("CORE", {
    content: documentContent,
    name: "mirror-runbook.md",
  });
  const root = await createArtifactComment(document.artifact.id, {
    anchor: { artifact: document.artifact.slug, quote: ANCHOR_QUOTE },
    body: "How far back does a cold start replay?",
  });
  expect(root.anchor).not.toBeNull();
  const blockId = root.anchor?.block_id;
  const markId = root.anchor?.mark_id;
  expect(blockId).toBeTruthy();
  await createArtifactComment(
    document.artifact.id,
    { body: "One hour, then it gives up.", reply_to: root.id },
    asSession()
  );

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await holdReadMarks(page);
    await page.goto("/");
    await selectRow(page, replyRowId(root.id));

    const pane = inboxPane(page);
    const anchored = pane.locator(`[data-dispatch-thread-anchor="${blockId}"]`);
    await expect(anchored).toBeVisible();
    // The quote the thread is anchored to is lit in the document.
    await expect(pane.locator(`[data-id="${markId}"] .dispatch-mark-active`)).toHaveCount(1);
    // The thread is drawn inside the document column, directly after the block it names.
    await expect(anchored.locator("[data-inbox-thread-body]")).toBeVisible();
    expect(
      await anchored.evaluate((frame, id) => {
        const slot = frame.querySelector(`[data-dispatch-thread-slot="${id}"]`);
        const block = frame.querySelector(`[data-block-id="${id}"]`);
        return slot !== null && block !== null && slot.previousElementSibling === block;
      }, blockId)
    ).toBe(true);

    // The column is opened at the section the block sits in, with the block itself on screen.
    // The editor draws its blocks after the socket syncs, so the placement settles a moment
    // after the thread is on the page.
    const placement = () =>
      pane.evaluate(
        (node, { heading, id }: { heading: string; id: string }) => {
          // The box the column actually scrolls in, which is the one the pane measures its
          // placement against.
          let scroller = node;
          for (const candidate of node.querySelectorAll<HTMLElement>("*")) {
            const overflow = window.getComputedStyle(candidate).overflowY;
            if (
              (overflow === "auto" || overflow === "scroll") &&
              candidate.scrollHeight > candidate.clientHeight
            ) {
              scroller = candidate;
              break;
            }
          }
          const blocks = [...node.querySelectorAll<HTMLElement>("[data-block-id]")];
          const section = blocks.find((block) => (block.textContent ?? "").trim() === heading);
          const at = blocks.findIndex((block) => block.dataset.blockId === id);
          const anchoredBlock = node.querySelector<HTMLElement>("[data-dispatch-thread-anchor]");
          const box = scroller.getBoundingClientRect();
          return {
            anchored: anchoredBlock?.getBoundingClientRect().top ?? null,
            bottom: box.bottom,
            scrollTop: scroller.scrollTop,
            section: section?.getBoundingClientRect().top ?? null,
            tags: blocks.slice(Math.max(0, at - 2), at + 1).map((block) => block.tagName),
            where: `${at}/${blocks.length}`,
            top: box.top,
          };
        },
        { heading: SECTION_HEADING, id: blockId ?? "" }
      );
    await expect
      .poll(async () => {
        const seen = await placement();
        const delta = seen.section === null ? null : Math.round(Math.abs(seen.section - seen.top));
        return `${delta} (scrollTop ${Math.round(seen.scrollTop)}, block ${seen.where}, before ${seen.tags.join(",")})`;
      })
      .toMatch(/^[0-8] /);
    const settled = await placement();
    expect(settled.anchored ?? 0).toBeGreaterThanOrEqual(settled.top);
    expect(settled.anchored ?? 0).toBeLessThan(settled.bottom);
  } finally {
    await alice.close();
  }
});

// ---------------------------------------------------------------------------
// A4: the fold, the three shapes, and a reply sent from the pane.
// ---------------------------------------------------------------------------

test("a thread of six read replies folds the oldest four and ends in its reply box", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Replay budget" });
  const root = await createComment(issue.key, { body: "How far back does a replay go?" });
  for (let index = 1; index <= 6; index += 1) {
    await createComment(
      issue.key,
      { body: `Reply ${index} about the replay budget.`, reply_to: root.id },
      asSession()
    );
  }
  const listed = await listMyReplies({ state: "unread" });
  const row = listed.rows.find((candidate) => candidate.thread_id === root.id);
  expect(row?.unread).toBe(true);
  // Every reply read: the thread leaves the unread list and stands under **All threads**.
  await markThreadRead(root.id, row?.reply.created_at ?? "");
  expect((await listMyReplies({ state: "unread" })).total).toBe(0);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await holdReadMarks(page);
    await page.goto("/?threads=all");
    await selectRow(page, replyRowId(root.id));
    const body = inboxPane(page).locator("[data-inbox-thread-body]");
    await expect(body).toBeVisible();

    // The root, the fold, the newest two replies, then the reply box.
    const fold = body.getByRole("button", { name: "Show 4 earlier replies" });
    await expect(fold).toBeVisible();
    await expect(body.locator("[data-inbox-thread-turn]")).toHaveCount(3);
    await expect(body).toContainText("Reply 5 about the replay budget.");
    await expect(body).toContainText("Reply 6 about the replay budget.");
    await expect(body).not.toContainText("Reply 4 about the replay budget.");
    const order = await body.evaluate((node) => {
      const marks = [
        ...node.querySelectorAll("[data-inbox-thread-turn], [data-inbox-thread-fold]"),
      ];
      const box = node.querySelector('textarea, [role="textbox"]');
      return {
        box: box === null ? -1 : box.getBoundingClientRect().top,
        last: marks.at(-1)?.getBoundingClientRect().top ?? -1,
      };
    });
    expect(order.box).toBeGreaterThan(order.last);

    // Opening the fold brings the other four back.
    await fold.click();
    await expect(body.locator("[data-inbox-thread-turn]")).toHaveCount(7);
    await expect(body).toContainText("Reply 1 about the replay budget.");
  } finally {
    await alice.close();
  }
});

/** One issue holding the three thread shapes, each a root and five replies of which the three
 *  oldest are read, so each thread folds exactly the same three. */
async function seedThreeShapes(): Promise<{
  agent: string;
  comment: string;
  message: string;
}> {
  await setLiveSessions([THREAD_SESSION]);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Replay budget" });

  const comment = await createComment(issue.key, { body: "How far back does a replay go?" });
  for (let index = 1; index <= 5; index += 1) {
    await createComment(
      issue.key,
      { body: `Comment reply ${index}.`, reply_to: comment.id },
      asSession()
    );
  }
  const message = await createMessage(issue.key, { body: "Where is the nightly build?" });
  for (let index = 1; index <= 5; index += 1) {
    await createMessage(
      issue.key,
      { body: `Message reply ${index}.`, in_reply_to: message.id },
      asSession()
    );
  }
  const agent = await seedAgentThread({
    body: "What is the rollout status?",
    delivery: "aside",
    replies: [1, 2, 3, 4, 5].map((index) => `Agent reply ${index}.`),
  });

  // A Reply row is read to a moment, an Agent row reply by reply: either way the three oldest
  // replies of each thread are read and the newest two are not.
  const comments = (await listComments(issue.key))
    .filter((candidate) => candidate.reply_to === comment.id)
    .sort((left, right) => (left.created_at < right.created_at ? -1 : 1));
  await markThreadRead(comment.id, comments[2]?.created_at ?? "");
  const messages = (await listIssueMessages(issue.key)).find(
    (read) => read.message.id === message.id
  );
  await markThreadRead(message.id, messages?.replies[2]?.created_at ?? "");
  const exchange = (await listAgentMessages(THREAD_SESSION.session_id)).find(
    (read) => read.message.id === agent
  );
  await putAgentState(THREAD_SESSION.session_id, {
    read_replies: (exchange?.replies ?? []).slice(0, 3).map((reply) => reply.id),
  });

  const listed = await listMyReplies({ kind: ["reply", "agent"], state: "unread" });
  expect(listed.rows.map((row) => row.unread_count).sort((left, right) => left - right)).toEqual([
    2, 2, 2,
  ]);
  return { agent, comment: comment.id, message: message.id };
}

test("a comment thread, a message conversation and an Agent thread are drawn alike, New on each unread message", async ({
  browser,
}) => {
  const seeded = await seedThreeShapes();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await holdReadMarks(page);
    await page.goto("/");
    for (const [id, kind] of [
      [replyRowId(seeded.comment), "comment"],
      [replyRowId(seeded.message), "message"],
      [agentRowId(seeded.agent), "agent"],
    ] as const) {
      await selectRow(page, id);
      const body = inboxPane(page).locator(`[data-inbox-thread-body="${kind}"]`);
      await expect(body, kind).toBeVisible();
      // The root and the two unread replies are drawn whole; the three read ones fold.
      await expect(
        body.getByRole("button", { name: "Show 3 earlier replies" }),
        kind
      ).toBeVisible();
      await expect(body.locator("[data-inbox-thread-turn]"), kind).toHaveCount(3);
      await expect(body.locator("[data-inbox-thread-unread]"), kind).toHaveCount(2);
      await expect(body.getByText("New", { exact: true }), kind).toHaveCount(2);
    }
  } finally {
    await alice.close();
  }
});

test("a reply sent from the pane reaches the thread, and an Agent reply goes in the Agents page's mode", async ({
  browser,
}) => {
  await setLiveSessions([THREAD_SESSION]);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Replay budget" });
  const root = await createComment(issue.key, { body: "How far back does a replay go?" });
  await createComment(
    issue.key,
    { body: "One hour, then it gives up.", reply_to: root.id },
    asSession()
  );
  // A Send would be delivered as `steer`; this exchange's own attempts are Asides, which is
  // what a reply into it inherits.
  const agent = await seedAgentThread({
    body: "What is the rollout status?",
    delivery: "aside",
    replies: ["Green."],
  });
  const before = (await listAgentMessages(THREAD_SESSION.session_id)).find(
    (read) => read.message.id === agent
  );
  const live = (await listAgents()).find(
    (candidate) => candidate.session_id === THREAD_SESSION.session_id
  );
  if (before === undefined || live === undefined) throw new Error("the seeded exchange is missing");
  const expected = exchangeDelivery(live, before);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await holdReadMarks(page);
    // Under **All threads**: the reader's own reply moves their read mark past every write, so
    // the row would leave an Unread list the moment it landed and take the pane with it.
    await page.goto("/?threads=all");

    // A reply to a comment thread, sent from the pane, lands in the thread. The box is the
    // thread's own, drawn with it: a row is selected before its thread has been read, so the
    // wait is for the thread on the page and then for the box holding what was typed, never
    // for the box alone - a draft written while the thread is still landing goes with the
    // mount that lands it.
    //
    // Every body is matched exactly: the column draws the thread's newest line twice, once as
    // its own turn and once as the head's one-line preview (`— <body>`), and which of the two
    // the page holds when a loose match runs is a race.
    await selectRow(page, replyRowId(root.id));
    const pane = inboxPane(page);
    await expect(pane.getByText("One hour, then it gives up.", { exact: true })).toBeVisible();
    const reply = pane.getByRole("textbox", { name: "Reply" });
    await reply.fill("Then we widen the window.");
    await expect(reply).toHaveValue("Then we widen the window.");
    await pane.getByRole("button", { name: "Send" }).click();
    await expect(pane.getByText("Then we widen the window.", { exact: true })).toBeVisible();

    // A reply to an Agent thread goes to the session in the mode the Agents page would use.
    await selectRow(page, agentRowId(agent));
    await expect(pane.getByText("Green.", { exact: true })).toBeVisible();
    const comment = pane.getByRole("textbox", { name: "Comment" });
    await comment.fill("Hold the mirror for an hour.");
    await expect(comment).toHaveValue("Hold the mirror for an hour.");
    await pane.getByRole("button", { name: "Send" }).click();
    await expect(pane.getByText("Hold the mirror for an hour.", { exact: true })).toBeVisible();

    // The mode the sent reply was delivered in, read back off the session's conversation, is
    // the one the Agents page would have picked for this exchange.
    expect(expected.target).toBe(`session:${THREAD_SESSION.session_id}`);
    expect(expected.delivery).toBe("aside");
    await expect
      .poll(async () => {
        const read = (await listAgentMessages(THREAD_SESSION.session_id)).find(
          (candidate) => candidate.message.id === agent
        );
        const sent = read?.replies.find((reply) => reply.body === "Hold the mirror for an hour.");
        return sent?.deliveries.at(-1)?.delivery;
      })
      .toBe(expected.delivery);
  } finally {
    await alice.close();
  }
});

test("a reply the session takes as its own turn says so and links the live view", async ({
  browser,
}) => {
  await setLiveSessions([THREAD_SESSION]);
  const root = await seedAgentThread({
    body: "What is the rollout status?",
    delivery: "btw",
    replies: ["Green."],
  });
  // The reader's own follow-up into the same conversation, which the session takes as a turn of
  // its live conversation; the Inbox still lists the row, because its root was never accepted.
  const followUp = await createAgentMessage(THREAD_SESSION.session_id, {
    body: "Then hold the mirror.",
    delivery: "steer",
    in_reply_to: root,
  });
  await acceptAgentDelivery(followUp.id, 1, sessionActor());

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await holdReadMarks(page);
    await page.goto("/");
    await selectRow(page, agentRowId(root));
    const turn = inboxPane(page).locator(`[data-inbox-thread-turn="${followUp.id}"]`);
    await expect(turn).toBeVisible();
    await expect(turn.getByTestId("agent-reply-accepted")).toHaveText(
      "Taken as a turn of the live conversation."
    );
    await expect(turn.getByRole("link", { name: "Live view" })).toHaveAttribute(
      "href",
      `/agents/${THREAD_SESSION.session_id}/live`
    );
  } finally {
    await alice.close();
  }
});
