import { expect, type Locator, type Page, test } from "@playwright/test";

import type { Issue } from "../web/src/api/types";
import {
  baseUrl,
  createAsk,
  createComment,
  createIssue,
  createMessage,
  createProject,
  getAsk,
} from "./api";
import { openSpec } from "./editor";
import { openMarginThread, referenceAgent, settledBlockAsk } from "./reference-surfaces";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

// Each row seeds two issues, waits for a question block to settle, and reads three pages.
test.setTimeout(90_000);

test.beforeEach(async () => {
  await resetDatabase();
});

const ANCHOR = "Anchor for the thread.";

/** What `seedCards` made: the issue a reference names, and the host whose spec holds the
 *  question `#q2` names and the anchor its margin threads quote. */
interface Seeded {
  readonly host: Issue;
  readonly target: Issue;
  readonly q2: { readonly id: string; readonly question: string };
}

async function seedCards(): Promise<Seeded> {
  await createProject({ key: "CORE", name: "Core" });
  const target = await createIssue({ project: "CORE", title: "Ship the cache" });
  const spec = [
    ANCHOR,
    ':::ask{#q2 urgency="med" multiple="false"}\nWhich cache backs the rollout?\n\n- Local\n- Shared\n:::',
    "",
  ].join("\n\n");
  const host = await createIssue({ project: "CORE", spec, title: "Card host" });
  const q2 = await settledBlockAsk(host.key, "q2");
  return { host, q2, target };
}

/** One form a reference-only reply takes, what its card reads as, and where the card leads. */
interface Form {
  readonly name: string;
  reference(seeded: Seeded): string;
  title(seeded: Seeded): string;
  href(seeded: Seeded): string;
  /** For a target that does not exist, the API path whose 404 says so: until that read answers,
   *  every card shows what was written, so the cards are read once it has. */
  readonly missing?: string;
}

const issueTitle = (seeded: Seeded) => `${seeded.target.key} Ship the cache`;
const issueHref = (seeded: Seeded) => `/issues/${seeded.target.key}`;
const missingIssue = `${baseUrl}/issues/CORE-999999`;

const FORMS: readonly Form[] = [
  {
    href: issueHref,
    name: "a dispatch:// address",
    reference: (seeded) => `dispatch://${seeded.target.key}`,
    title: issueTitle,
  },
  {
    href: issueHref,
    name: "a dashboard address",
    reference: (seeded) => `${baseUrl}/issues/${seeded.target.key}`,
    title: issueTitle,
  },
  {
    href: issueHref,
    name: "a bare issue key",
    reference: (seeded) => seeded.target.key,
    title: issueTitle,
  },
  {
    href: (seeded) => `/issues/${seeded.host.key}/asks/${seeded.q2.id}`,
    name: "a #id",
    reference: () => "#q2",
    title: (seeded) => seeded.q2.question,
  },
  // A card whose target is missing shows the address as it was written.
  {
    href: () => "/issues/CORE-999999",
    missing: "/api/v1/issues/CORE-999999",
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

/** Checks that `reply` shows exactly `cards`, in order, each reading as its title and leading to
 *  its target. */
async function expectCards(reply: Locator, cards: readonly Card[]): Promise<void> {
  const links = reply.getByRole("region", { name: "References" }).getByRole("link");
  await expect(links).toHaveCount(cards.length);
  for (const [index, { href, title }] of cards.entries()) {
    await expect(links.nth(index)).toHaveAttribute("href", href);
    await expect(links.nth(index).locator("span").first()).toHaveText(title);
  }
}

/**
 * Checks two replies drawn side by side: `only`, whose whole text is the reference, shows one
 * card reading as its target's title and leading to it; `beside`, which writes other text next to
 * the same reference, shows no card once its inline reference has resolved to that title (so the
 * card had every chance to appear).
 */
async function expectCard(only: Locator, beside: Locator, card: Card) {
  await expectCards(only, [card]);
  await expect(beside).toContainText("Beside the reference");
  await expect(beside.getByRole("link", { exact: true, name: card.title })).toBeVisible();
  await expect(beside.getByRole("region", { name: "References" })).toHaveCount(0);
}

/** Where `seedReplies` wrote its replies: a margin thread on the spec, an open ask in alice's
 *  Inbox, and the Conversation's messages. */
interface Replies {
  readonly root: string;
  readonly ask: string;
  readonly messages: readonly string[];
  readonly count: number;
}

/** Writes each of `bodies`, in order, as a reply in a margin thread on the host's spec, in an open
 *  ask's thread in alice's Inbox and as a Conversation message. */
async function seedReplies(host: Issue, bodies: readonly string[]): Promise<Replies> {
  const root = await createComment(
    host.key,
    { anchor: { artifact: "spec", quote: ANCHOR }, body: "Where does this land?" },
    referenceAgent
  );
  expect(root.anchor?.quote).toBe(ANCHOR);
  for (const body of bodies) {
    await createComment(host.key, { body, reply_to: root.id }, referenceAgent);
  }
  const ask = await createAsk(
    host.key,
    { options: [{ label: "Now" }, { label: "Later" }], question: "When should it roll out?" },
    referenceAgent
  );
  for (const body of bodies) {
    await createComment(host.key, { ask_id: ask.id, body }, referenceAgent);
  }
  expect((await getAsk(ask.id)).replies.map((reply) => reply.body)).toEqual(bodies);
  const messages: string[] = [];
  for (const body of bodies) {
    messages.push((await createMessage(host.key, { body }, referenceAgent)).id);
  }
  return { ask: ask.id, count: bodies.length, messages, root: root.id };
}

/**
 * Opens each surface `seedReplies` wrote to in turn - the margin thread, the Inbox thread (newest
 * first) and the Conversation - and hands `check` that surface's replies in the order they were
 * written. `settled`, started before each surface is opened, is awaited before its replies are.
 */
async function readReplies(
  page: Page,
  project: string,
  host: Issue,
  replies: Replies,
  check: (drawn: readonly Locator[]) => Promise<void>,
  settled: () => Promise<unknown> = async () => {}
): Promise<void> {
  let answered = settled();
  await openSpec(page, host.key, ANCHOR);
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
  await check(
    Array.from({ length: replies.count }, (_, index) => inboxReplies.nth(replies.count - 1 - index))
  );

  answered = settled();
  await page.goto(`/issues/${host.key}/conversation`);
  await answered;
  await check(replies.messages.map((id) => page.locator(`[data-turn="message:${id}"]`)));
}

for (const form of FORMS) {
  test(`a reply that is only ${form.name} shows its card in the margin, the Inbox thread and a Conversation message; one with text beside it shows none`, async ({
    browser,
  }, testInfo) => {
    const seeded = await seedCards();
    const reference = form.reference(seeded);
    const card = { href: form.href(seeded), title: form.title(seeded) };
    const replies = await seedReplies(seeded.host, [
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
        seeded.host,
        replies,
        async ([only, beside]) => {
          if (only === undefined || beside === undefined) throw new Error("two replies");
          await expectCard(only, beside, card);
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
  const seeded = await seedCards();
  const { host, q2, target } = seeded;
  const replies = await seedReplies(host, [`${target.key} dispatch://${host.key}/ask/${q2.id}`]);
  const cards = [
    { href: issueHref(seeded), title: issueTitle(seeded) },
    { href: `/issues/${host.key}/asks/${q2.id}`, title: q2.question },
  ];

  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await readReplies(page, testInfo.project.name, host, replies, async ([reply]) => {
      if (reply === undefined) throw new Error("one reply");
      await expectCards(reply, cards);
    });
  } finally {
    await context.close();
  }
});

test("a reference to a message reads as the message's text, inline and on its card", async ({
  browser,
}) => {
  const seeded = await seedCards();
  const { host, target } = seeded;
  const text = "The rollout finished at noon.";
  const message = await createMessage(target.key, { body: text }, referenceAgent);
  const reference = `dispatch://${target.key}/message/${message.id}`;
  const comment = await createComment(
    host.key,
    { body: `Status: ${reference} as planned.` },
    referenceAgent
  );
  const onlyMessage = await createMessage(host.key, { body: reference }, referenceAgent);

  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.goto(`/issues/${host.key}/conversation`);
    const inline = page
      .locator(`[data-turn="comment:${comment.id}"]`)
      .locator(`a[data-dispatch-ref="${reference}"]`);
    await expect(inline).toHaveText(text);

    const card = page
      .locator(`[data-turn="message:${onlyMessage.id}"]`)
      .getByRole("region", { name: "References" })
      .getByRole("link");
    await expect(card).toHaveCount(1);
    await expect(card.locator("span").first()).toHaveText(text);
  } finally {
    await context.close();
  }
});
