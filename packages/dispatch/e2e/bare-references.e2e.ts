import { type Browser, expect, type Locator, type Page, test } from "@playwright/test";

import type { Issue } from "../web/src/api/types";
import {
  createComment,
  createIssue,
  createMessage,
  createProject,
  getIssueArtifactText,
  listMyReplies,
} from "./api";
import { documentEditor, openSpec } from "./editor";
import {
  collapsedMarginThread,
  editorReferencesIn,
  openMarginThread,
  referenceAgent,
  referencesIn,
  settledBlockAsk,
} from "./reference-surfaces";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

// Each row seeds nine issues and waits for two question blocks to settle before it opens a page.
test.setTimeout(90_000);

test.beforeEach(async () => {
  await resetDatabase();
});

/** One run of the line the cases are written in, and what a renderer links it to: `links` is the
 *  `dispatch://` reference of its one link, absent where nothing may link. */
interface Segment {
  readonly text: string;
  readonly links?: string;
  readonly title?: string;
}

const ANCHOR = "The margin anchor sits here.";

/** One link the cases' line must show: the reference it names and the title it reads as. */
interface ExpectedLink {
  readonly ref: string;
  readonly title: string;
}

/** What `seedBareReferences` made. */
interface Seeded {
  /** Every link the line shows, in its order. */
  readonly expected: readonly ExpectedLink[];
  readonly host: Issue;
  /** The cases, as one line of text. */
  readonly line: string;
  readonly q2: { readonly id: string; readonly question: string };
  /** The issues the first two key cases name. */
  readonly targets: { readonly first: string; readonly second: string };
}

/**
 * Seeds the shared-table cases (packages/contracts/src/dispatch-text-references.ts) as one line,
 * every positive key naming an issue of its own, so the ordered list of links says which case each
 * link came from. Every negative key case names the last positive case's issue, which exists: the
 * list can only equal the expected one once that issue has answered, and from then on a negative
 * case that wrongly linked would add an entry. The line stays under the 160 characters the
 * Replies page's first line keeps. The host's spec holds the line, a margin anchor, filler that
 * puts its questions below the fold, and the two question blocks the `#id` cases resolve against.
 */
async function seedBareReferences(): Promise<Seeded> {
  await createProject({ key: "CORE", name: "Core" });
  const keys: string[] = [];
  for (let index = 1; index <= 8; index += 1) {
    keys.push((await createIssue({ project: "CORE", title: `Target ${index}` })).key);
  }
  const key = (index: number) => keys[index - 1] ?? "";
  const linked = (index: number) => ({
    links: `dispatch://${key(index)}`,
    title: `${key(index)} Target ${index}`,
  });
  // The question's ask is known only once it settles; its segment is filled in below.
  const questionSegment: Segment = { text: "#q2" };
  const line: Segment[] = [
    { text: key(1), ...linked(1) },
    { text: "and" },
    { text: `${key(2)},`, ...linked(2) },
    { text: `(${key(3)})`, ...linked(3) },
    { text: `[${key(4)}]`, ...linked(4) },
    { text: `"${key(5)}"`, ...linked(5) },
    { text: `${key(6)}'s`, ...linked(6) },
    { text: `${key(7)}.`, ...linked(7) },
    { text: `*${key(8)}*`, ...linked(8) },
    questionSegment,
    { text: `legion/${key(8)}` },
    { text: `x${key(8)}` },
    { text: key(8).toLowerCase() },
    { text: `${key(8)}abc` },
    { text: "CORE-999999" },
    { text: "#nope" },
    { text: "{#q2}" },
    { text: `\`${key(1)}\`` },
    { text: "`#q2`" },
  ];
  const lineText = line.map((segment) => segment.text).join(" ");
  expect(lineText.length).toBeLessThanOrEqual(160);
  const filler = Array.from(
    { length: 40 },
    (_, index) => `Filler paragraph ${index + 1} keeps the questions below the fold.`
  );
  const spec = [
    lineText,
    ANCHOR,
    ...filler,
    // q2 comes first, so the card q1's block hosts once it loads grows below the landing.
    ':::ask{#q2 urgency="med" multiple="false"}\nWhich cache backs the second answer?\n\n- Local\n- Shared\n:::',
    ':::ask{#q1 urgency="med" multiple="false"}\nWhich store holds the first answer?\n\n- Disk\n- Memory\n:::',
    "",
  ].join("\n\n");
  const host = await createIssue({ project: "CORE", spec, title: "Bare reference host" });
  await settledBlockAsk(host.key, "q1");
  const q2 = await settledBlockAsk(host.key, "q2");
  const expected = line.flatMap((segment): ExpectedLink[] => {
    if (segment === questionSegment) {
      return [{ ref: `dispatch://${host.key}/ask/${q2.id}`, title: q2.question }];
    }
    return segment.links === undefined ? [] : [{ ref: segment.links, title: segment.title ?? "" }];
  });
  return { expected, host, line: lineText, q2, targets: { first: key(1), second: key(2) } };
}

/** Waits until scope's reference links are exactly the expected ones, in order, each reading as
 *  its target's title; and checks the cases that may not link are still there as written. */
async function expectCaseLinks(scope: Locator, seeded: Seeded): Promise<void> {
  await expect.poll(() => referencesIn(scope)).toEqual(seeded.expected.map(({ ref }) => ref));
  await expect(scope.locator("a[data-dispatch-ref]")).toHaveText(
    seeded.expected.map(({ title }) => title)
  );
  await expect(scope).toContainText("CORE-999999");
  await expect(scope).toContainText("#nope");
  await expect(scope).toContainText("{#q2}");
  await expect(scope.locator("code")).toHaveText([seeded.targets.first, "#q2"]);
}

async function signedIn(browser: Browser): Promise<{ close(): Promise<void>; page: Page }> {
  const context = await asUser(browser, "alice");
  return { close: () => context.close(), page: await context.newPage() };
}

test("a margin comment and a Conversation message link each bare key and #id the table links, and a link opens its target", async ({
  browser,
}, testInfo) => {
  const seeded = await seedBareReferences();
  const { host } = seeded;
  const comment = await createComment(
    host.key,
    { anchor: { artifact: "spec", quote: ANCHOR }, body: seeded.line },
    referenceAgent
  );
  expect(comment.anchor?.quote).toBe(ANCHOR);
  const message = await createMessage(host.key, { body: seeded.line }, referenceAgent);
  const alice = await signedIn(browser);
  try {
    const { page } = alice;
    await openSpec(page, host.key, ANCHOR);
    const card = await openMarginThread(page, testInfo.project.name, comment.id);
    await expectCaseLinks(card, seeded);

    await page.goto(`/issues/${host.key}/conversation`);
    const turn = page.locator(`[data-turn="message:${message.id}"]`);
    await expectCaseLinks(turn, seeded);

    // A key opens its issue.
    const [first] = seeded.expected;
    await turn.getByRole("link", { exact: true, name: first?.title }).click();
    await expect(page).toHaveURL(new RegExp(`/issues/${seeded.targets.first}$`));
    await expect(page.getByRole("heading", { name: "Target 1" }).first()).toBeVisible();

    // A #id opens the document at that question, which starts below the fold.
    await page.goBack();
    await turn.getByRole("link", { exact: true, name: seeded.q2.question }).click();
    await expect(page).toHaveURL(new RegExp(`/issues/${host.key}/spec#b-q2$`));
    await expect(documentEditor(page).locator('[data-dispatch-ask-block="q2"]')).toBeInViewport();
  } finally {
    await alice.close();
  }
});

test("a collapsed thread card links the bare keys and #ids, and the Replies page shows their titles with no link", async ({
  browser,
}, testInfo) => {
  const seeded = await seedBareReferences();
  const { host } = seeded;
  const anchored = await createComment(
    host.key,
    { anchor: { artifact: "spec", quote: ANCHOR }, body: seeded.line },
    referenceAgent
  );
  expect(anchored.anchor?.quote).toBe(ANCHOR);
  const root = await createComment(host.key, { body: "Where do the references stand?" });
  await createComment(host.key, { body: seeded.line, reply_to: root.id }, referenceAgent);
  // The fixture puts exactly this thread on alice's Replies page, its first line the whole line.
  const replies = await listMyReplies();
  expect(replies.rows.map((row) => row.thread_id)).toEqual([root.id]);
  expect(replies.rows[0]?.reply.first_line).toBe(seeded.line);
  const alice = await signedIn(browser);
  try {
    const { page } = alice;
    await openSpec(page, host.key, ANCHOR);
    // The margin's collapsed card keeps its preview's links live: a click there is the way out.
    const collapsed = await collapsedMarginThread(page, testInfo.project.name, anchored.id);
    await expectCaseLinks(collapsed, seeded);

    // The Replies page's preview is inside a row that is one control: each reference reads as
    // its target's title in link-styled text, and nothing in it is a link.
    await page.goto("/replies");
    const row = page.locator(`[data-replies-row="${root.id}"]`);
    const preview = row.locator("[data-markdown-preview]").first();
    await expect
      .poll(() => referencesIn(preview, "[data-markdown-link][data-dispatch-ref]"))
      .toEqual(seeded.expected.map(({ ref }) => ref));
    await expect(preview.locator("[data-markdown-link][data-dispatch-ref]")).toHaveText(
      seeded.expected.map(({ title }) => title)
    );
    await expect(preview.locator("a")).toHaveCount(0);
    await expect(preview).toContainText("CORE-999999");
    await expect(preview.locator("code")).toHaveText([seeded.targets.first, "#q2"]);
  } finally {
    await alice.close();
  }
});

test("the live editor draws each bare key and #id the table links as a link, follows it, and leaves the stored text as written", async ({
  browser,
}) => {
  const seeded = await seedBareReferences();
  const { host } = seeded;
  const written = (await getIssueArtifactText(host.key, "spec")).markdown;
  // The server keeps the cases' line as it was written: the first line of the stored text.
  expect(written.split("\n")[0]).toBe(seeded.line);
  const alice = await signedIn(browser);
  try {
    const { page } = alice;
    await openSpec(page, host.key, ANCHOR);
    const editor = documentEditor(page);
    const paragraph = editor.locator("p").filter({ hasText: "legion/" });
    await expect(paragraph).toHaveCount(1);
    await expect
      .poll(() => editorReferencesIn(paragraph))
      .toEqual(seeded.expected.map(({ ref }) => ref));
    const titles = paragraph.locator("a.dispatch-reference");
    await expect(titles).toHaveText(seeded.expected.map(({ title }) => title));
    for (const title of await titles.all()) {
      await expect(title).toBeVisible();
    }
    // Code is never drawn as a reference, and the cases that may not link keep their text.
    await expect(paragraph.locator("code")).toHaveText([seeded.targets.first, "#q2"]);
    await expect(paragraph.getByText(/CORE-999999/)).toBeVisible();

    // A key opens its issue.
    await titles.nth(1).click();
    await expect(page).toHaveURL(new RegExp(`/issues/${seeded.targets.second}$`));
    await expect(page.getByRole("heading", { name: "Target 2" }).first()).toBeVisible();
    expect((await getIssueArtifactText(host.key, "spec")).markdown).toBe(written);

    // A #id opens the document at that question, which starts below the fold.
    await openSpec(page, host.key, ANCHOR);
    const question = paragraph.locator("a.dispatch-reference", { hasText: seeded.q2.question });
    await expect(question).toHaveAttribute(
      "data-dispatch-href",
      `dispatch://${host.key}/ask/${seeded.q2.id}`
    );
    await expect(editor.locator('[data-dispatch-ask-block="q2"]')).not.toBeInViewport();
    await question.click();
    await expect(page).toHaveURL(new RegExp(`/issues/${host.key}/spec#b-q2$`));
    await expect(editor.locator('[data-dispatch-ask-block="q2"]')).toBeInViewport();
    expect((await getIssueArtifactText(host.key, "spec")).markdown).toBe(written);
  } finally {
    await alice.close();
  }
});
