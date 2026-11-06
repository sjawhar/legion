import { expect, type Page, test } from "@playwright/test";

import {
  createArtifactAsk,
  createComment,
  createProjectDocument,
  editAsk,
  getArtifactBlocks,
  getArtifactText,
} from "./api";
import { connectedDot, documentEditor } from "./editor";
import { sql } from "./psql";
import {
  ANCHOR,
  conversationTurn,
  type ReferenceWorld,
  referenceAgent,
  seedReferenceWorld,
} from "./reference-surfaces";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

/**
 * An id written short on a page's own address: eight hex characters or more of an ask's, a
 * comment's, a message's or a block's id open the item they name, and the address the reader is
 * left holding carries the whole id, so what they copy is unambiguous. A prefix two items carry
 * is refused with the list of both; one no item carries is the not-found view a whole id that
 * names nothing already shows. A reference in a text written the same way reads as its target's
 * title and opens it.
 */

// Each row seeds the shared world — three projects, a host whose spec declares five question
// blocks, and the records a short id names — and reads up to three pages.
test.setTimeout(180_000);

test.beforeEach(async () => {
  await resetDatabase();
});

/** How many characters of an id an address writes short here: the fewest the server resolves. */
const SHORT = 8;

function short(id: string): string {
  return id.slice(0, SHORT);
}

/**
 * A second comment of the host whose id shares its first eight characters with the comment
 * `id` names, so one short id names both. Ids are minted, so two that a prefix both names
 * cannot be made through the API; the row is written straight into Postgres as a copy of that
 * comment with an id and a body of its own.
 */
async function twinComment(id: string, body: string): Promise<string> {
  const twin = `${short(id)}-1111-4111-8111-111111111111`;
  await sql(
    `create temporary table twin_comment as select * from comments where id = '${id}'`,
    `update twin_comment set id = '${twin}', body = '${body}', created_at = now()`,
    "insert into comments select * from twin_comment",
    "drop table twin_comment"
  );
  return twin;
}

/** The id of the last filler paragraph of the host's spec, which stands below the fold. */
async function blockBelowTheFold(world: ReferenceWorld): Promise<string> {
  const { markdown } = await getArtifactText(world.host.primary_artifact_id);
  const blocks = await getArtifactBlocks(world.host.primary_artifact_id);
  const filler = blocks.filter((block) =>
    markdown.slice(block.from, block.to).startsWith("Filler paragraph ")
  );
  const last = filler.at(-1);
  if (last === undefined) {
    throw new Error("setup: the host's spec holds no filler paragraph");
  }
  return last.id;
}

/** The margin card the page opened: the item the address named, selected and on screen. */
async function expectOpenedMarginItem(page: Page, id: string): Promise<void> {
  const item = page.getByTestId("margin-sheet").locator(`[data-margin-item="${id}"]`);
  await expect(item).toHaveAttribute("aria-current", "true");
  await expect(item).toBeInViewport();
}

test("an item address that writes its id short opens the item and is replaced by the whole id", async ({
  browser,
}) => {
  const world = await seedReferenceWorld();
  const rows = [
    { kind: "ask" as const, segment: "asks", shown: "Which cache backs the rollout?" },
    { kind: "comment" as const, segment: "comments", shown: "The rollout waits on the cache." },
    { kind: "message" as const, segment: "messages", shown: "The cache is warm now." },
  ];

  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    for (const { kind, segment, shown } of rows) {
      const whole = world.items[kind].id;
      const path = (id: string) => `/issues/${world.host.key}/${segment}/${id}`;
      await page.goto(path(short(whole)));
      await expect(page, kind).toHaveURL(path(whole));
      await expect(conversationTurn(page, kind, whole), kind).toContainText(shown);
    }
  } finally {
    await context.close();
  }
});

test("a spec address that writes an anchored comment's id short opens it and is replaced by the whole id", async ({
  browser,
}) => {
  const world = await seedReferenceWorld();
  const anchored = await createComment(
    world.host.key,
    { anchor: { artifact: "spec", quote: ANCHOR }, body: "Anchored on the first line." },
    referenceAgent
  );
  expect(anchored.anchor?.quote).toBe(ANCHOR);

  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    const address = `/issues/${world.host.key}/spec?comment=`;
    await page.goto(`${address}${short(anchored.id)}`);
    await expectOpenedMarginItem(page, anchored.id);
    await expect(page).toHaveURL(`${address}${anchored.id}`);
  } finally {
    await context.close();
  }
});

test("a project document's address that writes an ask's id short opens it and is replaced by the whole id", async ({
  browser,
}) => {
  const world = await seedReferenceWorld();
  const document = await createProjectDocument(
    world.host.project,
    { content: "# Handbook\n\nHow the cache is warmed.\n", name: "handbook.md" },
    referenceAgent
  );
  const ask = await createArtifactAsk(
    document.artifact.id,
    { question: "Who warms the cache on a Sunday?" },
    referenceAgent
  );

  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    const address = `/projects/${world.host.project}/documents/${document.artifact.slug}?ask=`;
    await page.goto(`${address}${short(ask.id)}`);
    await expectOpenedMarginItem(page, ask.id);
    await expect(page).toHaveURL(`${address}${ask.id}`);
  } finally {
    await context.close();
  }
});

test("a document address whose block fragment writes the block's id short opens the document at that block", async ({
  browser,
}) => {
  const world = await seedReferenceWorld();
  const block = await blockBelowTheFold(world);

  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    const address = `/issues/${world.host.key}/spec#b-`;
    await page.goto(`${address}${short(block)}`);
    await expect(connectedDot(page)).toHaveText("connected");
    await expect(documentEditor(page).locator(`[data-block-id="${block}"]`)).toBeInViewport();
    await expect(page).toHaveURL(`${address}${block}`);
  } finally {
    await context.close();
  }
});

test("an id prefix two comments carry lists both, each opening its own, and one no item carries shows the not-found view", async ({
  browser,
}) => {
  const world = await seedReferenceWorld();
  const first = world.items.comment;
  const twin = await twinComment(first.id, "The second comment the prefix names.");
  const prefix = short(first.id);
  const matches = `/issues/${world.host.key}/comments/${prefix}`;
  // An eight-character prefix of no comment at all: the first one's with its last character
  // moved on, which the world's own setup has already shown names nothing else.
  const nothing = `${prefix.slice(0, 7)}${prefix.slice(7) === "f" ? "0" : "f"}`;
  const bodies: Record<string, string> = {
    [first.id]: "The rollout waits on the cache.",
    [twin]: "The second comment the prefix names.",
  };

  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.goto(matches);
    await expect(
      page.getByRole("heading", { name: "Several items match that link" })
    ).toBeVisible();
    await expect(page).toHaveURL(matches);
    // Each match is listed by its own first line, under its own whole id; the server answers
    // them in id order, which two minted ids do not fix, so the pair is read as a mapping.
    const listed = page.locator(`a[href^="/issues/${world.host.key}/comments/"]`);
    await expect(listed).toHaveCount(2);
    const shown = await listed.evaluateAll((links) =>
      links.map((link) => [link.getAttribute("href") ?? "", link.textContent ?? ""])
    );
    expect(
      Object.fromEntries(shown.map(([href, text]) => [(href ?? "").split("/").pop(), text]))
    ).toEqual(bodies);

    for (const id of Object.keys(bodies)) {
      await page.goto(matches);
      await page.locator(`a[href="/issues/${world.host.key}/comments/${id}"]`).click();
      await expect(page).toHaveURL(`/issues/${world.host.key}/comments/${id}`);
      await expect(conversationTurn(page, "comment", id)).toBeVisible();
    }

    await page.goto(`/issues/${world.host.key}/comments/${nothing}`);
    await expect(page.getByRole("heading", { name: "Comment not found" })).toBeVisible();
  } finally {
    await context.close();
  }
});

test("a comment that writes an ask's id short reads as the ask's title, opens it, and follows an edit of the question with no reload", async ({
  browser,
}) => {
  const world = await seedReferenceWorld();
  const ask = world.items.ask;
  const reference = `dispatch://${world.host.key}/ask/${short(ask.id)}`;
  const comment = await createComment(
    world.host.key,
    { body: `Settled in ${reference} already.` },
    referenceAgent
  );

  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.goto(`/issues/${world.host.key}/conversation`);
    const link = conversationTurn(page, "comment", comment.id).locator(
      `a[data-dispatch-ref="${world.items.ask.reference}"]`
    );
    await expect(link).toHaveText("Which cache backs the rollout?");
    // A reload would drop this mark.
    await page.evaluate(() => {
      document.documentElement.dataset.e2eNoReload = "kept";
    });
    await editAsk(ask.id, { question: "Which cache backs the rollout now?" }, referenceAgent);
    await expect(link).toHaveText("Which cache backs the rollout now?");
    await expect(page.locator("html")).toHaveAttribute("data-e2e-no-reload", "kept");

    await link.click();
    await expect(page).toHaveURL(`/issues/${world.host.key}/asks/${ask.id}`);
    await expect(conversationTurn(page, "ask", ask.id)).toContainText(
      "Which cache backs the rollout now?"
    );
  } finally {
    await context.close();
  }
});
