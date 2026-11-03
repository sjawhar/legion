import { expect, type Locator, type Page } from "@playwright/test";

import type { Actor, Message } from "../web/src/api/types";
import type { FakeSession } from "./agents";
import {
  acceptAgentDelivery,
  answerAsk,
  createAgentMessage,
  createAsk,
  createBroadcast,
  createComment,
  createMessage,
  replyToMessageDelivery,
} from "./api";
import { sql } from "./psql";
import { sessionIds } from "./session-ids";

/**
 * The Inbox's thread rows, seeded: a Reply row (a thread the reader wrote in that somebody else
 * has written in since) in each of its three shapes, and an Agent row (a one-off message the
 * reader sent an agent session that has a reply) in each of its three.
 *
 * Every spec that lists thread rows seeds them through here, so what a Reply row and an Agent
 * row are is written once. A handful of rows is seeded through the API (`e2e/api.ts`), as a
 * person and a session would write them; a bulk seed goes through `e2e/psql.ts` in one statement
 * (`seedBulkReplyThreads`, `seedBulkAgentThreads`), because the suite runs one worker under a CI
 * budget, and inserts into `comments` and `messages` so that the `user_threads` triggers of
 * migrations 0096 and 0097 run and the generated `unread` column is right - never into
 * `user_threads` itself.
 */

/** The session every seeded thread is answered by: live, and taking all three delivery modes, so
 *  a thread's composer has somewhere to send and a Send is a Send. */
export const THREAD_SESSION: FakeSession = {
  capabilities: ["aside", "btw", "steer"],
  roles: [],
  session_id: sessionIds.planner,
  title: "Release bot",
};

/** A second session, for a row that must stay the other session's: its reply is unread in the
 *  Agents page's own count whatever the Inbox marks read. */
export const OTHER_SESSION: FakeSession = {
  capabilities: ["aside", "btw", "steer"],
  roles: [],
  session_id: sessionIds.reviewer,
  title: "Review bot",
};

/** The session as an author: what a write of its own names itself. */
export function sessionActor(session: FakeSession = THREAD_SESSION): Actor {
  return { id: session.session_id, kind: "session" };
}

/** The options a write made as the session carries. */
export function asSession(session: FakeSession = THREAD_SESSION) {
  return { actor: sessionActor(session), as: "agent" as const };
}

/** A comment thread the reader wrote in: their root, then the session's reply. */
export async function seedCommentThread(
  issueKey: string,
  root: string,
  reply: string,
  session: FakeSession = THREAD_SESSION
): Promise<string> {
  const head = await createComment(issueKey, { body: root });
  await createComment(issueKey, { body: reply, reply_to: head.id }, asSession(session));
  return head.id;
}

/** An issue message conversation the reader started, with the session's reply under it. */
export async function seedMessageThread(
  issueKey: string,
  root: string,
  reply: string,
  session: FakeSession = THREAD_SESSION
): Promise<string> {
  const head = await createMessage(issueKey, { body: root });
  await createMessage(issueKey, { body: reply, in_reply_to: head.id }, asSession(session));
  return head.id;
}

/**
 * An ask the session wrote and the reader answered, with the session's reply in its thread. The
 * answer is the reader's own write in the thread, which is what makes it theirs; the ask is
 * answered, so it is no open ask and the Inbox lists it as a Reply row alone.
 */
export async function seedAskThread(
  issueKey: string,
  question: string,
  reply: string,
  session: FakeSession = THREAD_SESSION
): Promise<string> {
  const ask = await createAsk(
    issueKey,
    { options: [{ label: "Ship" }, { label: "Hold" }], question },
    asSession(session)
  );
  await answerAsk(ask.id, { expected_edited_at: null, selected: ["Ship"] });
  await createComment(issueKey, { ask_id: ask.id, body: reply }, asSession(session));
  return ask.id;
}

/** One session reply to a one-off message, or a follow-up under the reply already there. */
async function sessionReply(
  messageId: string,
  body: string,
  session: FakeSession,
  followUp: boolean
): Promise<void> {
  await replyToMessageDelivery(
    messageId,
    { attempt: 1, body },
    sessionActor(session),
    followUp ? { followUp: true } : {}
  );
}

/**
 * A one-off message the reader sent the session, with one reply per entry of `replies`: the
 * first answers the delivery, and each after it is a follow-up threaded under it, which is how a
 * session writes more than once into one exchange.
 */
export async function seedAgentThread({
  body,
  delivery = "btw",
  replies,
  session = THREAD_SESSION,
}: {
  body: string;
  delivery?: "aside" | "btw" | "steer";
  replies: readonly string[];
  session?: FakeSession;
}): Promise<string> {
  const sent = await createAgentMessage(session.session_id, { body, delivery });
  for (const [index, reply] of replies.entries()) {
    await sessionReply(sent.id, reply, session, index > 0);
  }
  return sent.id;
}

/** The session's own copy of a broadcast, with its reply: an Agent row like any other one-off. */
export async function seedBroadcastThread({
  body,
  reply,
  session = THREAD_SESSION,
}: {
  body: string;
  reply: string;
  session?: FakeSession;
}): Promise<string> {
  const broadcast = await createBroadcast({
    body,
    delivery: "btw",
    session_ids: [session.session_id],
  });
  const copy = broadcast.recipients.find(
    (recipient) => recipient.session_id === session.session_id
  );
  if (copy === undefined) {
    throw new Error("the broadcast reached no copy for the seeded session");
  }
  await sessionReply(copy.message.id, reply, session, false);
  return copy.message.id;
}

/**
 * A Send the session took as a turn of its live conversation, with a reply under it. The Inbox
 * lists no Agent row for such a message however the session answers it, while the Agents page's
 * **New replies** badge still counts that reply.
 */
export async function seedAcceptedSend({
  body,
  reply,
  session = THREAD_SESSION,
}: {
  body: string;
  reply: string;
  session?: FakeSession;
}): Promise<string> {
  const sent = await createAgentMessage(session.session_id, { body, delivery: "steer" });
  await acceptAgentDelivery(sent.id, 1, sessionActor(session));
  await sessionReply(sent.id, reply, session, false);
  return sent.id;
}

/** How many of the reader's thread rows are unread, read from the table the triggers keep: what
 *  a bulk seed asserts of itself before the test proceeds. */
export async function unreadThreadRows(login = "alice"): Promise<number> {
  const counted = await sql(
    `select count(*) from user_threads where login = '${login}' and unread`
  );
  return Number(counted);
}

/** A jsonb author literal for a SQL seed. */
function authorLiteral(author: Actor): string {
  return `jsonb_build_object('kind', '${author.kind}', 'id', '${author.id}')`;
}

/**
 * `count` unread comment threads on `issueKey` in one statement: a root by the reader and a
 * reply by the session under it, the pair `count` times. Thread `g`'s reply is `olderThan + 2g`
 * minutes old, so thread 1 is the newest of them and thread `count` the oldest, and a second
 * seed given a large enough `olderThan` lies wholly before the first.
 *
 * One statement, so the suite's one worker pays one psql for the whole seed, and `comments`
 * rather than `user_threads`, so the triggers of migrations 0095 to 0097 write the rows and
 * their generated `unread` column.
 */
export async function seedBulkReplyThreads({
  count,
  issueKey,
  login = "alice",
  olderThan = 0,
  prefix,
  session = THREAD_SESSION,
}: {
  count: number;
  issueKey: string;
  login?: string;
  /** Minutes every thread of this seed is pushed back by, so two seeds never interleave. */
  olderThan?: number;
  /** What each root and reply says, before its number: `<prefix> root 7`, `<prefix> reply 7`. */
  prefix: string;
  session?: FakeSession;
}): Promise<void> {
  await sql(`
    with roots as (
      insert into comments (issue_key, author, body, created_at)
      select '${issueKey}', ${authorLiteral({ id: login, kind: "user" })},
             '${prefix} root ' || g,
             now() - ((${olderThan} + 2 * g + 1) || ' minutes')::interval
      from generate_series(1, ${count}) g
      returning id, body, created_at
    )
    insert into comments (issue_key, author, body, reply_to, created_at)
    select '${issueKey}', ${authorLiteral(sessionActor(session))},
           replace(roots.body, ' root ', ' reply '), roots.id,
           roots.created_at + interval '1 minute'
    from roots
  `);
}

/**
 * `count` unread Agent rows for `login` in one statement: a one-off message to the session and
 * a reply from it, the pair `count` times. An Agent row needs no delivery row - the Inbox reads
 * the message and its reply, and leaves out only a root an attempt was accepted on.
 */
export async function seedBulkAgentThreads({
  count,
  login = "alice",
  prefix,
  session = THREAD_SESSION,
}: {
  count: number;
  login?: string;
  /** What each message and reply says, before its number: `<prefix> ask 7`, `<prefix> answer 7`. */
  prefix: string;
  session?: FakeSession;
}): Promise<void> {
  await sql(`
    with roots as (
      insert into messages (issue_key, author, body, target, created_at)
      select null, ${authorLiteral({ id: login, kind: "user" })},
             '${prefix} ask ' || g, 'session:${session.session_id}',
             now() - ((2 * g + 1) || ' minutes')::interval
      from generate_series(1, ${count}) g
      returning id, body, created_at
    )
    insert into messages (issue_key, author, body, in_reply_to, target, created_at)
    select null, ${authorLiteral(sessionActor(session))},
           replace(roots.body, ' ask ', ' answer '), roots.id,
           'session:${session.session_id}', roots.created_at + interval '1 minute'
    from roots
  `);
}

/** The `?item=` a thread row is addressed by, which is also its `data-inbox-row`. */
export function replyRowId(threadId: string): string {
  return `reply:${threadId}`;
}

export function agentRowId(messageId: string): string {
  return `agent:${messageId}`;
}

/** The sidebar's own Inbox entry, which carries the one count. */
export function sidebarInbox(page: Page): Locator {
  return page.getByRole("navigation", { name: "Navigation" }).getByRole("link", { name: /^Inbox/ });
}

/** The sidebar's Inbox count: the work waiting on the reader plus their unread threads. */
export async function expectSidebarInboxCount(page: Page, count: number): Promise<void> {
  await expect(sidebarInbox(page)).toHaveText(
    count === 0 ? "Inbox" : new RegExp(`^Inbox\\s*${count}$`)
  );
}

/** The compact header's one Inbox link, which carries the same number. */
export function compactInboxLink(page: Page): Locator {
  return page
    .locator("header")
    .first()
    .getByRole("link", { name: /^Inbox( \d+)?$/ });
}

/** The width the shell folds its sidebar into the compact top bar at (`COMPACT_VIEWPORT_QUERY`,
 *  1280 px), which is also the width the Inbox draws its reading column from. */
export const COMPACT_VIEWPORT_WIDTH = 1280;

/**
 * The one Inbox number this width shows, whichever surface carries it: the sidebar's entry
 * above the compact width, and below it the top bar's own badge - a link from any other page,
 * plain text on the Inbox itself - so a row runs under `chromium` and `iphone` alike.
 */
export function inboxCountBadge(page: Page): Locator {
  if ((page.viewportSize()?.width ?? 0) >= COMPACT_VIEWPORT_WIDTH) return sidebarInbox(page);
  return page
    .locator("header")
    .first()
    .getByText(/^Inbox( \d+)?$/);
}

/** What that badge reads at `count`: `Inbox` alone with nothing waiting. */
export function inboxCountText(count: number): RegExp {
  return count === 0 ? /^Inbox$/ : new RegExp(`^Inbox\\s*${count}$`);
}

export async function expectInboxCount(page: Page, count: number): Promise<void> {
  await expect(inboxCountBadge(page)).toHaveText(inboxCountText(count));
}

/** The newest delivery attempt across a message and its replies, as `newestAttempt` picks it. */
export function newestDeliveryOf(items: readonly Pick<Message, "deliveries">[]) {
  return items
    .flatMap((item) => item.deliveries)
    .reduce<(typeof items)[number]["deliveries"][number] | undefined>(
      (last, attempt) =>
        last === undefined || attempt.created_at >= last.created_at ? attempt : last,
      undefined
    );
}
