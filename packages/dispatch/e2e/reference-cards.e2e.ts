import { expect, type Locator, test } from "@playwright/test";

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
}

const issueTitle = (seeded: Seeded) => `${seeded.target.key} Ship the cache`;
const issueHref = (seeded: Seeded) => `/issues/${seeded.target.key}`;

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
];

/**
 * Checks two replies drawn side by side: `only`, whose whole text is the reference, shows one
 * card reading as its target's title and leading to it; `beside`, which writes other text next to
 * the same reference, shows no card once its inline reference has resolved to that title (so the
 * card had every chance to appear).
 */
async function expectCard(only: Locator, beside: Locator, title: string, href: string) {
  const card = only.getByRole("region", { name: "References" }).getByRole("link");
  await expect(card).toHaveCount(1);
  await expect(card).toHaveAttribute("href", href);
  await expect(card.locator("span").first()).toHaveText(title);

  await expect(beside).toContainText("Beside the reference");
  await expect(beside.getByRole("link", { exact: true, name: title })).toBeVisible();
  await expect(beside.getByRole("region", { name: "References" })).toHaveCount(0);
}

for (const form of FORMS) {
  test(`a reply that is only ${form.name} shows its card in the margin, the Inbox thread and a Conversation message; one with text beside it shows none`, async ({
    browser,
  }, testInfo) => {
    const seeded = await seedCards();
    const { host } = seeded;
    const reference = form.reference(seeded);
    const besideText = `Beside the reference: ${reference} today.`;
    const title = form.title(seeded);
    const href = form.href(seeded);

    // A margin thread on the spec with both replies, oldest first.
    const root = await createComment(
      host.key,
      { anchor: { artifact: "spec", quote: ANCHOR }, body: "Where does this land?" },
      referenceAgent
    );
    expect(root.anchor?.quote).toBe(ANCHOR);
    await createComment(host.key, { body: reference, reply_to: root.id }, referenceAgent);
    await createComment(host.key, { body: besideText, reply_to: root.id }, referenceAgent);

    // An open ask in alice's Inbox with both replies in its thread, newest first.
    const ask = await createAsk(
      host.key,
      { options: [{ label: "Now" }, { label: "Later" }], question: "When should it roll out?" },
      referenceAgent
    );
    await createComment(host.key, { ask_id: ask.id, body: reference }, referenceAgent);
    await createComment(host.key, { ask_id: ask.id, body: besideText }, referenceAgent);
    expect((await getAsk(ask.id)).replies.map((reply) => reply.body)).toEqual([
      reference,
      besideText,
    ]);

    // Two messages in the Conversation.
    const onlyMessage = await createMessage(host.key, { body: reference }, referenceAgent);
    const besideMessage = await createMessage(host.key, { body: besideText }, referenceAgent);

    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openSpec(page, host.key, ANCHOR);
      const thread = await openMarginThread(page, testInfo.project.name, root.id);
      const replies = thread.locator("li[data-reply]");
      await expect(replies).toHaveCount(2);
      await expectCard(replies.nth(0), replies.nth(1), title, href);

      await page.goto("/");
      const inboxThread = page.getByTestId(`ask-${ask.id}`).getByTestId(`thread-${ask.id}`);
      const inboxReplies = inboxThread.locator("li");
      await expect(inboxReplies).toHaveCount(2);
      await expectCard(inboxReplies.nth(1), inboxReplies.nth(0), title, href);

      await page.goto(`/issues/${host.key}/conversation`);
      await expectCard(
        page.locator(`[data-turn="message:${onlyMessage.id}"]`),
        page.locator(`[data-turn="message:${besideMessage.id}"]`),
        title,
        href
      );
    } finally {
      await context.close();
    }
  });
}

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
