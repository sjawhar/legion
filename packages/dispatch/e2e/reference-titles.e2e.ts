import { expect, type Locator, type Page, test } from "@playwright/test";

import {
  answerAsk,
  createAsk,
  createComment,
  editArtifact,
  getArtifact,
  listMyAnswers,
} from "./api";
import { documentEditor, openSpec } from "./editor";
import {
  ANCHOR,
  answeredQuestionPreview,
  collapsedMarginThread,
  inboxReplyTurn,
  openInboxReplies,
  openMarginThread,
  type ReferenceWorld,
  referenceAgent,
  seedReferenceWorld,
  TARGET_KINDS,
  type Target,
  type TargetKind,
} from "./reference-surfaces";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

/**
 * What a `dispatch://` reference reads as, everywhere a person meets one: the title of what it
 * names. A bare address and a link whose text is its own address — the form a document stores an
 * autolink in — both read as the target's title in the live editor, in a past version of the
 * same document, in a rendered comment, in that comment's reply where the Inbox draws it, and
 * in an inert one-line preview; a link whose text its author wrote keeps those words; and an
 * address whose target does not exist reads as the address, since there is no title to read it
 * as.
 *
 * The inert one-line preview is read on the Answers page. The Replies page carried it until
 * INFRA-450 deleted that page, and the Inbox that took its rows draws a thread whole with live
 * links rather than a preview of it, so the preview a reader still meets is an answered ask's
 * question (`answeredQuestionPreview`).
 */

// Each row seeds the shared world, writes a second version of the host's spec, an anchored
// comment, ten threads the Inbox lists as Reply rows and ten answered asks, then reads six
// surfaces.
test.setTimeout(180_000);

test.beforeEach(async () => {
  await resetDatabase();
});

/** The kinds whose titles this spec reads: a message is a target of its own, proved beside the
 *  reply cards (`reference-cards.e2e.ts`). */
const KINDS = TARGET_KINDS.filter((kind) => kind !== "Message");

/** An ask id of the right shape that no ask holds. */
const MISSING_ASK = "00000000-0000-4000-8000-000000000000";

/** An address whose target does not exist, and the API path whose 404 says so: until that read
 *  answers, every surface shows what was written, so the checks wait for it. */
interface Missing {
  readonly label: string;
  readonly address: string;
  readonly path: string;
}

/** Where one reference text was written: a thread the Inbox lists as a Reply row, whose reply
 *  holds the text, and an answered ask whose question holds it. */
interface Written {
  /** The thread's root, which is also the Inbox row's thread id. */
  readonly thread: string;
  /** The reply holding the text, which is the turn the Inbox row draws it in. */
  readonly reply: string;
  /** The answered ask whose question holds the text, which is its Answers row's id. */
  readonly ask: string;
}

interface Seeded {
  readonly world: ReferenceWorld;
  /** The anchored comment on the host's spec, which writes every form. */
  readonly comment: string;
  readonly missing: readonly Missing[];
  /** Where each reference text was written, by the text itself. */
  readonly written: ReadonlyMap<string, Written>;
}

/** The three forms a document's text writes a reference in. */
function forms(kind: TargetKind, target: Target): readonly string[] {
  return [
    `${kind} bare: ${target.address}`,
    `${kind} linked: [${target.address}](${target.address})`,
    `${kind} worded: [the ${kind.toLowerCase()} in my words](${target.address})`,
  ];
}

/**
 * Seeds the world and, on its host, a spec whose text writes each target three ways (bare, as a
 * link whose text is its own address, and as a link its author worded) and the two addresses
 * whose targets are missing; then a second version of that spec, so version 1 is a past
 * version; then an anchored comment writing the bare and self-addressed forms and the missing
 * addresses; then, per form and per missing address, a thread whose reply is that text alone -
 * which the Inbox lists as a Reply row - and an ask whose question is that same text, answered
 * by the reader so the Answers page draws its inert preview.
 */
async function seedTitles(): Promise<Seeded> {
  const world = await seedReferenceWorld();
  const missing: Missing[] = [
    {
      address: `dispatch://${world.host.project}-999999`,
      label: "Missing issue",
      path: `/api/v1/issues/${world.host.project}-999999/summary`,
    },
    {
      address: `dispatch://${world.target.key}/ask/${MISSING_ASK}`,
      label: "Missing question",
      path: `/api/v1/asks/${MISSING_ASK}`,
    },
  ];
  // The host's spec already opens with the anchor a margin thread quotes, so these lines are
  // appended to it rather than writing a second one.
  const lines = [
    ...KINDS.flatMap((kind) => forms(kind, world.targets[kind])),
    ...missing.map(({ address, label }) => `${label}: ${address}`),
    "",
  ];
  await editArtifact(
    world.host.primary_artifact_id,
    { ops: [{ after: "end", markdown: lines.join("\n\n"), op: "insert" }] },
    referenceAgent
  );
  await editArtifact(
    world.host.primary_artifact_id,
    { ops: [{ after: "end", markdown: "A second version.", op: "insert" }] },
    referenceAgent
  );
  await expect
    .poll(async () => (await getArtifact(world.host.primary_artifact_id)).versions.length)
    .toBeGreaterThan(2);

  const body = [
    ...KINDS.flatMap((kind) => {
      const { address } = world.targets[kind];
      return [`${kind} bare: ${address}`, `${kind} linked: [${address}](${address})`];
    }),
    ...missing.map(({ address, label }) => `${label}: ${address}`),
  ].join("\n\n");
  const anchored = await createComment(
    world.host.key,
    { anchor: { artifact: "spec", quote: ANCHOR }, body },
    referenceAgent
  );
  expect(anchored.anchor?.quote).toBe(ANCHOR);

  const written = new Map<string, Written>();
  const texts = [
    ...KINDS.flatMap((kind) => {
      const { address } = world.targets[kind];
      return [address, `[${address}](${address})`];
    }),
    ...missing.map(({ address }) => address),
  ];
  for (const text of texts) {
    const root = await createComment(world.host.key, { body: "Where does this reference lead?" });
    const reply = await createComment(
      world.host.key,
      { body: text, reply_to: root.id },
      referenceAgent
    );
    // The ask is the session's, the answer the reader's: that is what puts a row on their
    // Answers page, and the row's preview is the question the session wrote.
    const ask = await createAsk(
      world.host.key,
      { options: [{ label: "Yes" }, { label: "No" }], question: text },
      referenceAgent
    );
    await answerAsk(ask.id, { expected_edited_at: null, selected: ["Yes"] });
    written.set(text, { ask: ask.id, reply: reply.id, thread: root.id });
  }
  // Each answered ask is one row of alice's Answers page, under the question the session wrote.
  const answers = await listMyAnswers({ limit: 50 });
  expect(new Map(answers.rows.map((row) => [row.ask_id, row.question]))).toEqual(
    new Map([...written].map(([text, where]) => [where.ask, text]))
  );
  return { comment: anchored.id, missing, world, written };
}

/** The one paragraph of a document view that starts with `label`. */
async function paragraph(scope: Locator, label: string): Promise<Locator> {
  const found = scope.locator("p").filter({ hasText: `${label}:` });
  await expect(found).toHaveCount(1);
  return found;
}

/** Checks that a document view draws the bare and the self-addressed reference to `target` as
 *  its title: one title per paragraph, carrying the address, with the stored text hidden. */
async function expectDrawnTitles(scope: Locator, kind: TargetKind, target: Target): Promise<void> {
  for (const form of ["bare", "linked"]) {
    const line = await paragraph(scope, `${kind} ${form}`);
    const title = line.locator("a.dispatch-reference");
    await expect(title).toHaveText(target.title);
    await expect(title).toHaveAttribute("data-dispatch-href", target.address);
    await expect(title).toBeVisible();
    await expect(line.locator(".dispatch-reference-text")).toHaveCount(1);
    await expect(line.locator(".dispatch-reference-text")).toBeHidden();
  }
}

/** Resolves once every missing target's read has answered 404 on `page`. Registered before the
 *  navigation that makes the reads. */
function missingAnswered(page: Page, seeded: Seeded): Promise<unknown> {
  return Promise.all(
    seeded.missing.map(({ path }) =>
      page.waitForResponse(
        (response) => new URL(response.url()).pathname === path && response.status() === 404
      )
    )
  );
}

for (const kind of KINDS) {
  test(`a bare address and a link whose text is its own address to the ${kind.toLowerCase()} read as its title in the live editor, a past version, a comment, its reply on the Inbox and an inert one-line preview`, async ({
    browser,
  }, testInfo) => {
    const seeded = await seedTitles();
    const { world } = seeded;
    const target = world.targets[kind];
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openSpec(page, world.host.key, ANCHOR);
      await expectDrawnTitles(documentEditor(page), kind, target);

      // The comment's one-line preview, on its collapsed card, and the comment itself: both
      // read each of the two forms as the title.
      const collapsed = await collapsedMarginThread(page, testInfo.project.name, seeded.comment);
      await expect(collapsed.locator(`[data-dispatch-ref="${target.address}"]`)).toHaveText([
        target.title,
        target.title,
      ]);
      const card = await openMarginThread(page, testInfo.project.name, seeded.comment);
      await expect(card.locator(`a[data-dispatch-ref="${target.address}"]`)).toHaveText([
        target.title,
        target.title,
      ]);

      // The past version: the same document, read-only, before the second version was written.
      await page.goto(`/issues/${world.host.key}/artifacts/spec?v=2`);
      const past = page.getByTestId("version-view");
      await expect(past.getByRole("textbox", { name: "Document editor" })).toHaveAttribute(
        "contenteditable",
        "false"
      );
      await expect(past).not.toContainText("A second version.");
      await expectDrawnTitles(past, kind, target);

      // The reply itself, where a reader meets it on the Inbox: below the reading column's
      // width a Reply row is the card that draws its thread whole, with live links
      // (`refresh-freshness.e2e.ts` reads the same row), so each form is a link reading as the
      // title.
      await openInboxReplies(page);
      for (const text of [target.address, `[${target.address}](${target.address})`]) {
        const where = seeded.written.get(text);
        const turn = inboxReplyTurn(page, where?.thread ?? "", where?.reply ?? "");
        await expect(turn.locator(`a[data-dispatch-ref="${target.address}"]`)).toHaveText(
          target.title
        );
      }

      // The inert one-line preview, which the Answers page carries now that the Replies page
      // is gone: each form reads as the title, with no link inside it.
      await page.goto("/answers");
      for (const text of [target.address, `[${target.address}](${target.address})`]) {
        const preview = answeredQuestionPreview(page, seeded.written.get(text)?.ask ?? "");
        await expect(preview.locator(`[data-dispatch-ref="${target.address}"]`)).toHaveText(
          target.title
        );
        await expect(preview.locator("a")).toHaveCount(0);
      }
    } finally {
      await context.close();
    }
  });
}

/** Checks, once every target the document names has drawn its title (so each read has
 *  answered), that a worded link shows its author's words and no title, and that an address
 *  whose target is missing shows as written. */
async function expectWordsAndAddresses(scope: Locator, seeded: Seeded): Promise<void> {
  for (const kind of KINDS) {
    const bare = await paragraph(scope, `${kind} bare`);
    await expect(bare.locator("a.dispatch-reference")).toHaveText(seeded.world.targets[kind].title);
    const worded = await paragraph(scope, `${kind} worded`);
    await expect(
      worded.getByRole("link", { exact: true, name: `the ${kind.toLowerCase()} in my words` })
    ).toBeVisible();
    await expect(worded.locator("a.dispatch-reference")).toHaveCount(0);
  }
  for (const { address, label } of seeded.missing) {
    const line = await paragraph(scope, label);
    await expect(line.getByText(address)).toBeVisible();
    await expect(line.locator("a.dispatch-reference")).toHaveCount(0);
  }
}

test("a link its author worded keeps its words in the document, and an address whose target is missing reads as written", async ({
  browser,
}, testInfo) => {
  const seeded = await seedTitles();
  const { world } = seeded;
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    const answered = missingAnswered(page, seeded);
    await openSpec(page, world.host.key, ANCHOR);
    await answered;
    await expectWordsAndAddresses(documentEditor(page), seeded);

    // The comment's one-line preview, on its collapsed card, and the comment itself.
    const collapsed = await collapsedMarginThread(page, testInfo.project.name, seeded.comment);
    for (const { address } of seeded.missing) {
      await expect(collapsed.locator(`[data-dispatch-ref="${address}"]`)).toHaveText(address);
    }
    const card = await openMarginThread(page, testInfo.project.name, seeded.comment);
    for (const { address } of seeded.missing) {
      await expect(card.locator(`a[data-dispatch-ref="${address}"]`)).toHaveText(address);
    }

    const pastAnswered = missingAnswered(page, seeded);
    await page.goto(`/issues/${world.host.key}/artifacts/spec?v=2`);
    await pastAnswered;
    await expectWordsAndAddresses(page.getByTestId("version-view"), seeded);

    // The inert one-line preview shows each missing address as written, with no link in it.
    // The Answers page carries it now that the Replies page is gone.
    const answersAnswered = missingAnswered(page, seeded);
    await page.goto("/answers");
    await answersAnswered;
    for (const { address } of seeded.missing) {
      const preview = answeredQuestionPreview(page, seeded.written.get(address)?.ask ?? "");
      await expect(preview.locator(`[data-dispatch-ref="${address}"]`)).toHaveText(address);
      await expect(preview.locator("a")).toHaveCount(0);
    }
  } finally {
    await context.close();
  }
});
