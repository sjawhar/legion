import { DISPATCH_TEXT_REFERENCES, isIdPrefix } from "@legion/contracts";
import { expect, type Locator, type Page, test } from "@playwright/test";

import {
  answerAsk,
  baseUrl,
  createAsk,
  createComment,
  createIssueArtifact,
  dashboardOrigin,
  getArtifactText,
  listMyAnswers,
  userHeaders,
} from "./api";
import { connectedDot, documentEditor, openSpec } from "./editor";
import {
  ANCHOR,
  answeredQuestionPreview,
  HOST_KEY,
  INERT_REFERENCE,
  OTHER_KEY,
  openMarginThread,
  QUESTION_IDS,
  questionBlock,
  questionText,
  RENDERED_BODY,
  RENDERED_REFERENCE,
  type ReferenceWorld,
  referenceAgent,
  referenceTargetsIn,
  type SettledQuestion,
  seedReferenceWorld,
  settledQuestions,
} from "./reference-surfaces";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

/**
 * The browser's half of the shared text table (`@legion/contracts`'s DISPATCH_TEXT_REFERENCES,
 * which the Go reader and the dashboard's reader are both held to): every row written into a
 * real Dispatch, read on the three surfaces a person reads text on — the rendered markdown of a
 * Conversation turn, the inert one-line preview of an Answers row, and the live document editor —
 * each linking exactly the references its row names and nothing else, inside code included,
 * against a server that resolves them for real.
 *
 * The table is written against `https://dispatch.test` and against ids of its own; this world
 * is a real one, so each row is rewritten into it (`casesFor`): the origin becomes this run's,
 * the ask a `#id` names becomes the question block's settled ask, and each id a row writes short
 * becomes the same number of characters of the real record's id, so an eight-character prefix
 * stays an eight-character prefix.
 */

test.beforeEach(async () => {
  await resetDatabase();
});

type Row = (typeof DISPATCH_TEXT_REFERENCES)[number];

/** The origin the table's dashboard addresses are written against, unless a row names its own. */
const TABLE_ORIGIN = "https://dispatch.test";

/** The one row that needs CORE-1 to name no issue, which this world's host holds: it reads with
 *  CORE-404 instead, the key the table's own rows use for an issue that does not exist. */
const NO_ISSUE_ROW = "Blocked on CORE-1, which no issue holds.";
const NO_ISSUE_KEY = "CORE-404";

/** Rows no document can hold as a line of its text, each with the reason. They are read on the
 *  rendered and preview surfaces like any other.
 *
 *  A row's `refs` is a property of the body string, which a comment, an ask and a message store
 *  verbatim. A document stores a tree, and the parse that makes that tree reads both spellings
 *  of an escaped `#` as a plain `#` (`pmdoc.Parse`), so the escape is gone before anything
 *  renders the document or reads it: `GET /api/v1/artifacts/{id}/text` for a document uploaded
 *  with `See &#35;q2 here.` answers `See #q2 here.`, the line the table itself reads as a
 *  citation of q2 (the row `See #q2 for the rollout.`). No writer can put the escape back,
 *  since a text `#q2` the author wrote bare, which cites q2, and one they escaped are the same
 *  characters by then. The two rows below are true of a body stored as written, untestable on a
 *  document, and proved on the rendered and preview surfaces; their exclusion is no gap in the
 *  grammar. */
const EDITOR_EXCLUDED: Readonly<Record<string, string>> = {
  ':::callout{#d1 affects="dispatch://CORE-1/spec#b-a dispatch://CORE-1/spec#b-b"}':
    "an unclosed directive, which in a document opens a block that swallows the text after it",
  ":::ask{#q2}\nWhich region first?\n:::":
    "written into a document it is the q2 question block's own declaration, not text in it",
  "See &#35;q2 here.":
    "a document stores no numeric character reference: the parse reads it as the text `See #q2 here.`",
  "See \\#q2 and #q3 here.":
    "a document stores no backslash escape: the parse reads it as the text `See #q2 and #q3 here.`",
};

/** Whether `row` writes a prefix two of its own items carry: the table proves an ambiguous
 *  short id with two ids that differ past their eighth character, and ids are minted, so no
 *  world made through the API holds two that one prefix names. The ambiguous prefix is proved
 *  on its own (e2e/prefix-links.e2e.ts, which writes its twin straight into Postgres); here the
 *  row is left out, since rewriting both of its ids into this world's one item of that kind
 *  would make the prefix name exactly one. */
function writesAnAmbiguousPrefix(row: Row): boolean {
  const ids = Object.keys(row.items ?? {});
  return ids.some((id, at) =>
    ids.some((other, index) => index > at && other.slice(0, 8) === id.slice(0, 8))
  );
}

/** One table row as this world holds it. */
interface Case {
  /** The row's place in the table, which names it in a failure. */
  readonly index: number;
  readonly row: Row;
  /** The row's body, rewritten into this world. */
  readonly body: string;
  /** What a renderer links for it, in order, rewritten into this world. */
  readonly links: readonly string[];
  /** What the live editor draws a title for, in order: the links whose target this world holds
   *  and whose row writes them where the editor reads them. */
  readonly drawn: readonly string[];
}

function rowName(item: Case): string {
  return `row ${item.index} ${JSON.stringify(item.row.body)}`;
}

/** The case of the row whose body is `body`, which a check after the table's reaches for. */
function caseOf(cases: readonly Case[], body: string): Case {
  const found = cases.find((item) => item.row.body === body);
  if (found === undefined) {
    throw new Error(`setup: the table no longer holds ${JSON.stringify(body)}`);
  }
  return found;
}

/**
 * `text` with each id of `ids` replaced by the real record's: every prefix of the written id
 * eight characters or longer becomes that many characters of the real one, in the case it was
 * written in, so `2a7f1c90` and `2A7F1C90-1f4e` stay a prefix of the same length and shape and
 * the seven-character value the table writes to prove it is no prefix stays one.
 */
function substituteIds(text: string, ids: ReadonlyMap<string, string>): string {
  let written = text;
  for (const [table, real] of ids) {
    for (let length = table.length; length >= 8; length -= 1) {
      const short = table.slice(0, length);
      written = written.replaceAll(short, real.slice(0, length));
      written = written.replaceAll(short.toUpperCase(), real.slice(0, length).toUpperCase());
    }
  }
  return written;
}

/** The kind of record the table's `items` entry names, read from the reference it maps to. */
function itemKindOf(reference: string): "ask" | "comment" | "message" | "block" {
  if (reference.includes("#b-")) return "block";
  if (reference.includes("/ask/")) return "ask";
  if (reference.includes("/comment/")) return "comment";
  if (reference.includes("/message/")) return "message";
  throw new Error(`setup: the table names an item of no kind this world makes: ${reference}`);
}

/**
 * The table's rows in this world. `questions` is the question map the texts' own document
 * declares — the host's spec for a comment or a message of the host, the uploaded document for
 * its own text — so a `#id` reads as the ask that document's block settled into.
 */
function casesFor(
  world: ReferenceWorld,
  questions: Readonly<Record<string, SettledQuestion>>
): Case[] {
  for (const body of [NO_ISSUE_ROW, ...Object.keys(EDITOR_EXCLUDED)]) {
    if (DISPATCH_TEXT_REFERENCES.filter((row) => row.body === body).length !== 1) {
      throw new Error(`setup: the table no longer holds exactly one ${JSON.stringify(body)}`);
    }
  }

  // Every reference the live editor draws a title for: the issues this world holds, the records
  // a short id names, each question block's ask, and any document of an issue it holds - a
  // document's title is the name its own reference carries, so the editor draws one whether or
  // not that document exists, as it does for a block of one.
  const issues = [world.host.key, world.target.key, world.other.key, world.acme.key];
  const named = new Set<string>([
    ...issues.map((key) => `dispatch://${key}`),
    ...Object.values(world.items).map((item) => item.reference),
    ...QUESTION_IDS.map((id) => askReference(world, questions, id)),
  ]);
  const document = new RegExp(`^dispatch://(${issues.join("|")})/(spec|artifact/)`);
  // A document's title is the name its own reference carries, so the editor draws one whether
  // or not that document exists; a block's title is the block's own text, so it draws only for
  // a block this world holds.
  const made = (link: string) => named.has(link) || (document.test(link) && !link.includes("#b-"));
  return DISPATCH_TEXT_REFERENCES.map((row, index) => {
    const ids = new Map(
      Object.entries(row.items ?? {}).map(([id, reference]) => [
        id,
        world.items[itemKindOf(reference)].id,
      ])
    );
    let body = substituteIds(row.body.replaceAll(row.origin ?? TABLE_ORIGIN, dashboardOrigin), ids);
    if (row.body === NO_ISSUE_ROW) {
      body = body.replace(HOST_KEY, NO_ISSUE_KEY);
    }
    const asks = new Map(
      Object.entries(row.questions ?? {}).map(([block, reference]) => [
        reference,
        askReference(world, questions, block),
      ])
    );
    const links = (row.links ?? row.refs).map((link) => asks.get(link) ?? substituteIds(link, ids));
    return { body, drawn: links.filter(made), index, links, row };
  });
}

/** The reference of the ask the question block `id` of that document settled into. */
function askReference(
  world: ReferenceWorld,
  questions: Readonly<Record<string, SettledQuestion>>,
  id: string
): string {
  const settled = questions[id];
  if (settled === undefined) {
    throw new Error(`setup: the question block ${id} never settled`);
  }
  return `dispatch://${world.host.key}/ask/${settled.id}`;
}

/** The keys a row writes where the bare-key grammar could read them: each issue-key-shaped
 *  token at the start of the text or after whitespace, `;` (a numeric reference to whitespace)
 *  or one of ( [ " ' * _ > ` ~ |, outside an address. A superset of what the grammar reads. */
function standaloneKeys(body: string): string[] {
  const keys: string[] = [];
  for (const chunk of body.split(/\s+/)) {
    if (chunk.includes("://")) {
      continue;
    }
    for (const match of chunk.matchAll(/[A-Z][A-Z0-9]*-\d+/g)) {
      const before = match.index === 0 ? "" : (chunk[match.index - 1] ?? "");
      if (before === "" || "([\"'*_>`~|;".includes(before)) {
        keys.push(match[0]);
      }
    }
  }
  return keys;
}

/** What `GET /api/v1/issues/{key}` answers alice. */
async function issueStatus(key: string): Promise<number> {
  const response = await fetch(new URL(`/api/v1/issues/${encodeURIComponent(key)}`, baseUrl), {
    headers: await userHeaders(),
  });
  return response.status;
}

/**
 * The world's precondition, a setup error when it fails: every key a row needs exists, every
 * other key it writes where the grammar could read it names no issue, and each short id the
 * table writes to name nothing still names nothing once it has been rewritten into this world
 * (the real ids are minted, so a value the table chose to miss by one character could in
 * principle hit).
 */
async function expectWorldHolds(world: ReferenceWorld, cases: readonly Case[]): Promise<void> {
  const statuses = new Map<string, Promise<number>>();
  const status = (key: string) => {
    let answered = statuses.get(key);
    if (answered === undefined) {
      answered = issueStatus(key);
      statuses.set(key, answered);
    }
    return answered;
  };
  const whole = new Map(
    Object.entries(world.items).map(([kind, item]) => [kind, item.id.toLowerCase()])
  );
  for (const item of cases) {
    for (const key of item.row.issues ?? []) {
      if ((await status(key)) !== 200) {
        throw new Error(`setup: ${rowName(item)} needs ${key}, which is missing`);
      }
    }
    for (const key of standaloneKeys(item.body)) {
      if (!(item.row.issues ?? []).includes(key) && (await status(key)) !== 404) {
        throw new Error(`setup: ${rowName(item)} needs ${key} to name no issue`);
      }
    }
    // A row whose reference names nothing must still name nothing: its written id is no prefix
    // of the real record of that kind. Only a value the grammar resolves as a prefix can hit
    // one - `#b-a` is a block id of one character, which names nothing whatever this world
    // minted - so a shorter value is no collision, however the ids came out.
    for (const [index, link] of (item.row.links ?? item.row.refs).entries()) {
      if (item.links[index] !== link) {
        continue;
      }
      const written = /\/(ask|comment|message)\/([^/?#]+)$|#b-(.+)$/.exec(link);
      const value = written?.[2] ?? written?.[3];
      const kind = written?.[3] === undefined ? written?.[1] : "block";
      if (value === undefined || kind === undefined || !isIdPrefix(value)) {
        continue;
      }
      if (whole.get(kind)?.startsWith(value.toLowerCase()) === true) {
        throw new Error(
          `setup: ${rowName(item)} writes ${value}, which this world's ${kind} id starts with`
        );
      }
    }
  }
}

/** The title each target this world made reads as, by its reference: the two kinds of target a
 *  bare key and a `#id` name. */
function madeTitles(
  world: ReferenceWorld,
  questions: Readonly<Record<string, SettledQuestion>>,
  scope: string
): Map<string, string> {
  return new Map([
    [`dispatch://${world.host.key}`, `${world.host.key} ${world.host.title}`],
    [`dispatch://${world.other.key}`, `${world.other.key} ${world.other.title}`],
    [`dispatch://${world.acme.key}`, `${world.acme.key} ${world.acme.title}`],
    ...QUESTION_IDS.map(
      (id) => [askReference(world, questions, id), questionText(id, scope)] as const
    ),
  ]);
}

/**
 * Checks that every reference in scope to a target this world made reads as its title. A
 * one-line preview cuts a title that does not fit and ends it with an ellipsis, so what is
 * shown is the title or its own opening.
 */
async function expectMadeTitles(
  scope: Locator,
  selector: (reference: string) => string,
  titles: ReadonlyMap<string, string>,
  item: Case
): Promise<void> {
  for (const link of new Set(item.links)) {
    const title = titles.get(link);
    if (title === undefined) {
      continue;
    }
    for (const each of await scope.locator(selector(link)).all()) {
      const shown = (await each.textContent()) ?? "";
      const opening = shown.endsWith("…") ? shown.slice(0, -1) : shown;
      expect(title.startsWith(opening) && (shown === title || shown.endsWith("…")), {
        message: `${rowName(item)}: ${link} reads ${JSON.stringify(shown)}, not ${JSON.stringify(title)}`,
      }).toBe(true);
    }
  }
}

test("a margin thread's replies link each reference the shared table links and nothing else, and a link opens its target", async ({
  browser,
}, testInfo) => {
  test.setTimeout(60_000 + DISPATCH_TEXT_REFERENCES.length * 1_500);
  const world = await seedReferenceWorld();
  const cases = casesFor(world, world.questions).filter(
    (item) => !writesAnAmbiguousPrefix(item.row)
  );
  await expectWorldHolds(world, cases);
  const titles = madeTitles(world, world.questions, "spec");
  // One thread on the host's spec, one reply per row: every row is drawn at once, each read
  // against the host's own questions (`source`, the margin's owner).
  const root = await createComment(
    world.host.key,
    { anchor: { artifact: "spec", quote: ANCHOR }, body: "Where does each row stand?" },
    referenceAgent
  );
  expect(root.anchor?.quote).toBe(ANCHOR);
  for (const item of cases) {
    await createComment(world.host.key, { body: item.body, reply_to: root.id }, referenceAgent);
  }

  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await openSpec(page, world.host.key, ANCHOR);
    const card = await openMarginThread(page, testInfo.project.name, root.id);
    const replies = card.locator("li[data-reply]");
    await expect(replies).toHaveCount(cases.length);
    const body = (item: Case) => replies.nth(cases.indexOf(item)).locator(RENDERED_BODY).first();
    for (const item of cases) {
      await expect
        .poll(() => referenceTargetsIn(body(item), RENDERED_REFERENCE), { message: rowName(item) })
        .toEqual([...item.links]);
      await expectMadeTitles(body(item), (link) => `a[data-dispatch-ref="${link}"]`, titles, item);
    }

    // A bare key opens its issue.
    await body(caseOf(cases, "See XCORE-1 here."))
      .getByRole("link", { exact: true, name: `${world.other.key} ${world.other.title}` })
      .click();
    await expect(page).toHaveURL(new RegExp(`/issues/${OTHER_KEY}$`));
    await expect(page.getByRole("heading", { name: world.other.title }).first()).toBeVisible();

    // A `#id` opens the document at that question, which starts below the fold.
    await openSpec(page, world.host.key, ANCHOR);
    const reopened = await openMarginThread(page, testInfo.project.name, root.id);
    await reopened
      .locator("li[data-reply]")
      .nth(cases.indexOf(caseOf(cases, "See #q2 for the rollout.")))
      .getByRole("link", { exact: true, name: questionText("q2", "spec") })
      .click();
    await expect(page).toHaveURL(new RegExp(`/issues/${HOST_KEY}/spec#b-q2$`));
    const block = documentEditor(page).locator('[data-dispatch-ask-block="q2"]');
    await expect(block).toBeInViewport();
  } finally {
    await context.close();
  }
});

test("the one-line preview shows each reference the shared table links as its target's title, with no link inside it", async ({
  browser,
}) => {
  test.setTimeout(60_000 + DISPATCH_TEXT_REFERENCES.length * 1_500);
  const world = await seedReferenceWorld();
  // The Replies page carried this one-line preview until INFRA-450 deleted that page, and the
  // Inbox that took its rows draws a thread whole with live links rather than a preview of it.
  // The inert one-line preview a reader still meets is an answered ask's question on the
  // Answers page, so each row of the table is written as one question there. A preview is one
  // line of a list, so the table's longer and multi-line bodies are read on the rendered and
  // editor surfaces alone.
  const cases = casesFor(world, world.questions).filter(
    (item) =>
      !writesAnAmbiguousPrefix(item.row) &&
      item.row.body.length <= 160 &&
      !item.row.body.includes("\n")
  );
  await expectWorldHolds(world, cases);
  const titles = madeTitles(world, world.questions, "spec");
  // One ask per row, asked by the agent session and answered by alice: that is what puts a row
  // on her Answers page, and the row's preview is the question the session wrote.
  const asks = new Map<Case, string>();
  for (const item of cases) {
    const ask = await createAsk(
      world.host.key,
      { options: [{ label: "Yes" }, { label: "No" }], question: item.body },
      referenceAgent
    );
    await answerAsk(ask.id, { expected_edited_at: null, selected: ["Yes"] });
    asks.set(item, ask.id);
  }
  const questions = new Map<string, string>();
  for (let offset = 0; ; ) {
    const listed = await listMyAnswers({ limit: 50, offset });
    const rows = listed.rows ?? [];
    for (const row of rows) {
      questions.set(row.ask_id, row.question);
    }
    offset += rows.length;
    if (rows.length === 0 || offset >= listed.total) {
      break;
    }
  }
  expect(questions.size).toBe(asks.size);
  for (const [item, ask] of asks) {
    expect(questions.get(ask), rowName(item)).toBe(item.body);
  }

  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.goto("/answers");
    const rows = page.locator("[data-answer-row]");
    const more = page.getByRole("button", { name: "Show more" });
    await expect(async () => {
      if (await more.isVisible()) {
        await more.click();
      }
      expect(await rows.count()).toBe(asks.size);
    }).toPass();
    for (const [item, ask] of asks) {
      const preview = answeredQuestionPreview(page, ask);
      await expect
        .poll(() => referenceTargetsIn(preview, INERT_REFERENCE), { message: rowName(item) })
        .toEqual([...item.links]);
      await expectMadeTitles(preview, (link) => `[data-dispatch-ref="${link}"]`, titles, item);
      // A preview is one control's text: what a reference reads as is link-styled text, never
      // a link.
      await expect(preview.locator("a"), rowName(item)).toHaveCount(0);
    }
  } finally {
    await context.close();
  }
});

/** The document the editor rows are written in, which declares question blocks of its own: a
 *  `#id` written in it names this document's question, not the spec's. */
const DOCUMENT = { name: "table-rows.md", slug: "table-rows-md" } as const;

/** Where each row's blocks start in that document: a line of its own, which names no reference
 *  and holds no candidate. */
function marker(index: number): string {
  return `Row ${index}.`;
}

/** The titles the editor draws for each row of the document, by the row's index: the blocks are
 *  walked in order and each marker line opens the row it names. */
function drawnTitlesByRow(page: Page): Promise<Record<string, string[]>> {
  return documentEditor(page).evaluate((root) => {
    const drawn: Record<string, string[]> = {};
    let current: string[] | undefined;
    for (const block of Array.from(root.children)) {
      const found = /^Row (\d+)\.$/.exec(block.textContent?.trim() ?? "");
      if (found !== null && found[1] !== undefined) {
        current = [];
        drawn[found[1]] = current;
        continue;
      }
      if (current === undefined) {
        continue;
      }
      for (const title of Array.from(block.querySelectorAll("a.dispatch-reference"))) {
        current.push(title.getAttribute("data-dispatch-href") ?? "");
      }
    }
    return drawn;
  });
}

test("the live editor draws each reference the shared table links whose target exists, follows it, and leaves the stored text as written", async ({
  browser,
}) => {
  test.setTimeout(60_000 + DISPATCH_TEXT_REFERENCES.length * 1_500);
  const world = await seedReferenceWorld();
  // The questions first, so each marker line opens exactly its row's blocks, then one marker and
  // one row each.
  const written = casesFor(world, world.questions).filter(
    (item) => EDITOR_EXCLUDED[item.row.body] === undefined && !writesAnAmbiguousPrefix(item.row)
  );
  const content = [
    ...QUESTION_IDS.map((id) => questionBlock(id, DOCUMENT.slug)),
    ...written.flatMap((item) => [marker(item.index), item.body]),
    "",
  ].join("\n\n");
  const uploaded = await createIssueArtifact(
    world.host.key,
    { content, name: DOCUMENT.name },
    referenceAgent
  );
  expect(uploaded.artifact.slug).toBe(DOCUMENT.slug);
  const questions = await settledQuestions(world.host.key, DOCUMENT.slug, QUESTION_IDS);
  const cases = casesFor(world, questions).filter(
    (item) => EDITOR_EXCLUDED[item.row.body] === undefined && !writesAnAmbiguousPrefix(item.row)
  );
  await expectWorldHolds(world, cases);
  const titles = madeTitles(world, questions, DOCUMENT.slug);
  const stored = (await getArtifactText(uploaded.artifact.id)).markdown;

  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    const open = async () => {
      await page.goto(`/issues/${world.host.key}/artifacts/${DOCUMENT.slug}`);
      await expect(documentEditor(page)).toContainText(questionText("q2", DOCUMENT.slug));
      await expect(connectedDot(page)).toHaveText("connected");
    };
    await open();
    const expected = Object.fromEntries(cases.map((item) => [String(item.index), [...item.drawn]]));
    await expect.poll(() => drawnTitlesByRow(page), { timeout: 60_000 }).toEqual(expected);
    for (const item of cases) {
      await expectMadeTitles(
        documentEditor(page),
        (link) => `a.dispatch-reference[data-dispatch-href="${link}"]`,
        titles,
        item
      );
    }
    for (const title of await documentEditor(page).locator("a.dispatch-reference").all()) {
      await expect(title).toBeVisible();
    }

    // A bare key opens its issue, and the document's stored text is what was written.
    await documentEditor(page)
      .locator(`a.dispatch-reference[data-dispatch-href="dispatch://${world.other.key}"]`)
      .first()
      .click();
    await expect(page).toHaveURL(new RegExp(`/issues/${OTHER_KEY}$`));
    await expect(page.getByRole("heading", { name: world.other.title }).first()).toBeVisible();
    expect((await getArtifactText(uploaded.artifact.id)).markdown).toBe(stored);

    // A `#id` opens this document at its own question, which stands above the fold it is
    // followed from.
    await open();
    // The line the `#id` row was written as. The editor draws a resolved reference's title in
    // front of the reference's own text, which it keeps and hides, so the line reads
    // `See <the question>#q2 for the rollout.`: the words around the reference name the line.
    const question = documentEditor(page)
      .locator("p")
      .filter({ hasText: /^See .*#q2 for the rollout\.$/ })
      .locator("a.dispatch-reference");
    await expect(question).toHaveAttribute(
      "data-dispatch-href",
      askReference(world, questions, "q2")
    );
    const block = documentEditor(page).locator('[data-dispatch-ask-block="q2"]');
    await expect(question).toBeVisible();
    await expect(block).not.toBeInViewport();
    await question.click();
    await expect(page).toHaveURL(
      new RegExp(`/issues/${HOST_KEY}/artifacts/${DOCUMENT.slug}#b-q2$`)
    );
    await expect(block).toBeInViewport();
    expect((await getArtifactText(uploaded.artifact.id)).markdown).toBe(stored);
  } finally {
    await context.close();
  }
});
