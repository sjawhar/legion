import { expect, type Locator, type Page, test } from "@playwright/test";

import { baseUrl, createAsk, createComment, createMessage, getAsk } from "./api";
import { openSpec } from "./editor";
import {
  ANCHOR,
  conversationTurn,
  openMarginThread,
  questionText,
  type ReferenceWorld,
  referenceAgent,
  seedReferenceWorld,
} from "./reference-surfaces";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

/**
 * The cards under a reply whose whole text is references: one card per reference, reading as
 * its target's title over two lines of what that target says, in the margin, in the Inbox
 * thread and in a Conversation message alike — the three surfaces the one card predicate is
 * mounted on (`features/refs/Unfurl.tsx`). A reply that writes anything beside its reference
 * shows none, since the reference already reads as the title inline.
 */

// Each row seeds the shared world, writes three replies to each of three surfaces and reads
// three pages.
test.setTimeout(180_000);

test.beforeEach(async () => {
  await resetDatabase();
});

/** One form a reference-only reply takes, what its card reads as, and where it leads. */
interface Form {
  readonly name: string;
  reference(world: ReferenceWorld): string;
  title(world: ReferenceWorld): string;
  href(world: ReferenceWorld): string;
  /** For a target that does not exist, the API path whose 404 says so: until that read answers
   *  every card shows what was written, so the cards are read once it has. */
  readonly missing?: string;
}

const missingIssue = `${baseUrl}/issues/CORE-999999`;

// A card reads as its target's own title, which for an issue is its words alone: the key stands
// beside a reference drawn inline, never on the card, whose own line already names the issue.
const FORMS: readonly Form[] = [
  {
    href: (world) => `/issues/${world.target.key}`,
    name: "a dispatch:// address",
    reference: (world) => world.targets.Issue.address,
    title: (world) => world.target.title,
  },
  {
    href: (world) => `/issues/${world.target.key}`,
    name: "a dashboard address",
    reference: (world) => `${baseUrl}/issues/${world.target.key}`,
    title: (world) => world.target.title,
  },
  {
    href: (world) => `/issues/${world.target.key}`,
    name: "a bare issue key",
    reference: (world) => world.target.key,
    title: (world) => world.target.title,
  },
  {
    href: (world) => `/issues/${world.host.key}/asks/${world.questions.q2?.id}`,
    name: "a #id",
    reference: () => "#q2",
    title: () => questionText("q2", "spec"),
  },
  {
    href: (world) => `/issues/${world.host.key}/comments/${world.items.comment.id}`,
    name: "a dispatch:// address whose id is written short",
    reference: (world) =>
      `dispatch://${world.host.key}/comment/${world.items.comment.id.slice(0, 8)}`,
    title: () => "The rollout waits on the cache.",
  },
  // A card whose target is missing shows the address as it was written.
  {
    href: () => "/issues/CORE-999999",
    missing: "/api/v1/issues/CORE-999999/summary",
    name: "a dashboard address whose issue is missing",
    reference: () => missingIssue,
    title: () => missingIssue,
  },
];

/** A card a reply must show: the title it reads as and where it leads. */
interface Card {
  readonly title: string;
  readonly href: string;
}

/** Checks that `reply` shows exactly `cards`, in order, each reading as its title and leading
 *  to its target. */
async function expectCards(reply: Locator, cards: readonly Card[]): Promise<void> {
  const links = reply.getByRole("region", { name: "References" }).getByRole("link");
  await expect(links).toHaveCount(cards.length);
  for (const [index, { href, title }] of cards.entries()) {
    await expect(links.nth(index)).toHaveAttribute("href", href);
    await expect(links.nth(index).locator("span").first()).toHaveText(title);
  }
}

/** Where `seedReplies` wrote its replies: a margin thread on the host's spec, an open ask in
 *  alice's Inbox, and the host's Conversation messages. */
interface Replies {
  readonly root: string;
  readonly ask: string;
  readonly messages: readonly string[];
  readonly count: number;
}

/** Writes each of `bodies`, in order, as a reply in a margin thread on the host's spec, as a
 *  reply in an open ask's thread, and as a Conversation message. */
async function seedReplies(world: ReferenceWorld, bodies: readonly string[]): Promise<Replies> {
  const root = await createComment(
    world.host.key,
    { anchor: { artifact: "spec", quote: ANCHOR }, body: "Where does this land?" },
    referenceAgent
  );
  expect(root.anchor?.quote).toBe(ANCHOR);
  for (const body of bodies) {
    await createComment(world.host.key, { body, reply_to: root.id }, referenceAgent);
  }
  const ask = await createAsk(
    world.host.key,
    { options: [{ label: "Now" }, { label: "Later" }], question: "When should it roll out?" },
    referenceAgent
  );
  for (const body of bodies) {
    await createComment(world.host.key, { ask_id: ask.id, body }, referenceAgent);
  }
  expect((await getAsk(ask.id)).replies.map((reply) => reply.body)).toEqual([...bodies]);
  const messages: string[] = [];
  for (const body of bodies) {
    messages.push((await createMessage(world.host.key, { body }, referenceAgent)).id);
  }
  return { ask: ask.id, count: bodies.length, messages, root: root.id };
}

/**
 * Opens each surface `seedReplies` wrote to in turn — the margin thread, the Inbox thread and
 * the Conversation — and hands `check` that surface's replies in the order they were written.
 * `settled`, started before each surface is opened, is awaited before its replies are.
 */
async function readReplies(
  page: Page,
  project: string,
  world: ReferenceWorld,
  replies: Replies,
  check: (drawn: readonly Locator[]) => Promise<void>,
  settled: () => Promise<unknown> = async () => {}
): Promise<void> {
  let answered = settled();
  await openSpec(page, world.host.key, ANCHOR);
  const thread = (await openMarginThread(page, project, replies.root)).locator("li[data-reply]");
  await expect(thread).toHaveCount(replies.count);
  await answered;
  await check(Array.from({ length: replies.count }, (_, index) => thread.nth(index)));

  answered = settled();
  await page.goto("/");
  const inbox = page.getByTestId(`ask-${replies.ask}`).getByTestId(`thread-${replies.ask}`);
  const inboxReplies = inbox.locator("li");
  await expect(inboxReplies).toHaveCount(replies.count);
  await answered;
  // Drawn order is written order on both threads now: which way a thread reads is the reader's
  // own setting (`features/threads/thread-order.ts`), oldest first until they say otherwise, so
  // the Inbox's ask thread is indexed exactly as the margin thread above it is.
  await check(Array.from({ length: replies.count }, (_, index) => inboxReplies.nth(index)));

  answered = settled();
  await page.goto(`/issues/${world.host.key}/conversation`);
  await answered;
  await check(replies.messages.map((id) => conversationTurn(page, "message", id)));
}

for (const form of FORMS) {
  test(`a reply that is only ${form.name} shows its card in the margin, the Inbox thread and a Conversation message; one with text beside it shows none`, async ({
    browser,
  }, testInfo) => {
    const world = await seedReferenceWorld();
    const reference = form.reference(world);
    const card = { href: form.href(world), title: form.title(world) };
    const replies = await seedReplies(world, [
      reference,
      `Beside the reference: ${reference} today.`,
    ]);

    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      const { missing } = form;
      await readReplies(
        page,
        testInfo.project.name,
        world,
        replies,
        async ([only, beside]) => {
          if (only === undefined || beside === undefined) {
            throw new Error("two replies");
          }
          // The card is read first, so everything its reference needed has been read by the
          // time the reply beside it is asked to show none.
          await expectCards(only, [card]);
          await expect(beside).toContainText("Beside the reference");
          await expect(beside.getByRole("region", { name: "References" })).toHaveCount(0);
        },
        missing === undefined
          ? undefined
          : () =>
              page.waitForResponse(
                (response) =>
                  new URL(response.url()).pathname === missing && response.status() === 404
              )
      );
    } finally {
      await context.close();
    }
  });
}

test("a reply made only of a bare key and a dispatch:// address shows a card for each in the margin, the Inbox thread and a Conversation message", async ({
  browser,
}, testInfo) => {
  const world = await seedReferenceWorld();
  const question = world.questions.q2;
  if (question === undefined) {
    throw new Error("setup: the host's spec declares no q2");
  }
  const replies = await seedReplies(world, [
    `${world.target.key} dispatch://${world.host.key}/ask/${question.id}`,
  ]);
  const cards = [
    { href: `/issues/${world.target.key}`, title: world.target.title },
    { href: `/issues/${world.host.key}/asks/${question.id}`, title: question.question },
  ];

  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await readReplies(page, testInfo.project.name, world, replies, async ([reply]) => {
      if (reply === undefined) {
        throw new Error("one reply");
      }
      await expectCards(reply, cards);
    });
  } finally {
    await context.close();
  }
});

test("a reference to a message reads as the message's text, inline and on its card", async ({
  browser,
}) => {
  const world = await seedReferenceWorld();
  const { address, title } = world.targets.Message;
  const comment = await createComment(
    world.host.key,
    { body: `Status: ${address} as planned.` },
    referenceAgent
  );
  const only = await createMessage(world.host.key, { body: address }, referenceAgent);

  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.goto(`/issues/${world.host.key}/conversation`);
    const inline = conversationTurn(page, "comment", comment.id).locator(
      `a[data-dispatch-ref="${address}"]`
    );
    await expect(inline).toHaveText(title);

    const card = conversationTurn(page, "message", only.id)
      .getByRole("region", { name: "References" })
      .getByRole("link");
    await expect(card).toHaveCount(1);
    await expect(card.locator("span").first()).toHaveText(title);
  } finally {
    await context.close();
  }
});
