import { DISPATCH_TEXT_REFERENCES } from "@legion/contracts";
import { type Browser, expect, type Locator, type Page, test } from "@playwright/test";

import {
  baseUrl,
  createComment,
  createIssue,
  createIssueArtifact,
  createMessage,
  createProject,
  dashboardOrigin,
  getArtifactText,
  getIssueArtifactText,
  listIssueAsks,
  listMyReplies,
  patchIssue,
  userHeaders,
} from "./api";
import { connectedDot, documentEditor, openSpec } from "./editor";
import {
  editorReferencesIn,
  openMarginThread,
  referenceAgent,
  referenceTargetsIn,
  settledBlockAsk,
} from "./reference-surfaces";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

test.beforeEach(async () => {
  await resetDatabase();
});

type Row = (typeof DISPATCH_TEXT_REFERENCES)[number];

/** The origin the table's dashboard addresses are written against, unless a row names its own. */
const TABLE_ORIGIN = "https://dispatch.test";
/** The issue every row's `issues` and `questions` name: the host of the spec and the document
 *  the rows are written in, so a row's `#id` resolves against that document's questions. */
const HOST = { key: "CORE-1", title: "Bare reference host" } as const;
const OTHER = { key: "XCORE-1", title: "Cross-project target" } as const;
const ANCHOR = "The margin anchor sits here.";
const DOCUMENT = { name: "bare-references.md", slug: "bare-references-md" } as const;

/** Every table row is read in a world of its own; this test reads them all in one, where CORE-1
 *  exists. The one row that says CORE-1 names no issue cannot hold here, so it reads with a key of
 *  the same shape that no issue holds: ACME is no project. */
const NO_ISSUE_ROW = "Blocked on CORE-1, which no issue holds.";
const NO_ISSUE_KEY = "ACME-1";

/** Rows the live editor cannot read as a line of a document, each with its reason. They still
 *  run on the renderer and the preview. */
const EDITOR_EXCLUDED: Readonly<Record<string, string>> = {
  ":::ask{#q2}\nWhich region first?\n:::":
    "written into a document it is the q2 question block's own declaration, not text in it",
};
/** A row the editor holds as an author's link mark: no reference candidate, so it is checked on
 *  its own to carry no reference decoration rather than against the row's `links`. */
const AUTHOR_LINK_ROW = "See [the blocker](CORE-1).";

/** The question block ids any row names, in first-named order. */
const QUESTION_IDS = [
  ...new Set(DISPATCH_TEXT_REFERENCES.flatMap((row) => Object.keys(row.questions ?? {}))),
];

/** One table row as this world holds it. */
interface Case {
  /** The row's place in the table, which names it in a failure. */
  readonly index: number;
  readonly row: Row;
  /** The row's body with its origin rewritten to the test server's. */
  readonly body: string;
  /** The keys of existing issues the body names (`row.issues`, rewritten). */
  readonly issues: readonly string[];
  /** What a renderer links, each once, in order, a question's ask rewritten to the settled one. */
  readonly links: readonly string[];
}

function rowName(item: Case): string {
  return `row ${item.index} ${JSON.stringify(item.body)}`;
}

/** The case of the table row whose body is `body`, which a check after the table's reaches for. */
function caseOf(cases: readonly Case[], body: string): Case {
  const found = cases.find((item) => item.row.body === body);
  if (found === undefined) {
    throw new Error(`setup: the table no longer holds ${JSON.stringify(body)}`);
  }
  return found;
}

/** The table's rows in this world, each `#id` reading as the ask `asks` maps its block id to. */
function casesFor(asks: Readonly<Record<string, string>>): Case[] {
  for (const body of [NO_ISSUE_ROW, AUTHOR_LINK_ROW, ...Object.keys(EDITOR_EXCLUDED)]) {
    if (DISPATCH_TEXT_REFERENCES.filter((row) => row.body === body).length !== 1) {
      throw new Error(`setup: the table no longer holds exactly one ${JSON.stringify(body)}`);
    }
  }
  return DISPATCH_TEXT_REFERENCES.map((row, index) => {
    let body = row.body.replaceAll(row.origin ?? TABLE_ORIGIN, dashboardOrigin);
    if (row.body === NO_ISSUE_ROW) {
      body = body.replace(HOST.key, NO_ISSUE_KEY);
    }
    const name = `row ${index} ${JSON.stringify(row.body)}`;
    const links = (row.links ?? row.refs).map((link) => {
      const ids = Object.entries(row.questions ?? {})
        .filter(([, ask]) => ask === link)
        .map(([id]) => id);
      if (ids.length > 1) {
        throw new Error(`setup: ${name} maps ${link} to two questions`);
      }
      const [id] = ids;
      if (id === undefined) {
        return link;
      }
      const ask = asks[id];
      if (ask === undefined) {
        throw new Error(`setup: ${name} names question ${id}, which never settled`);
      }
      return `dispatch://${HOST.key}/ask/${ask}`;
    });
    return { body, index, issues: row.issues ?? [], links: [...new Set(links)], row };
  });
}

/** The question blocks every row's `#id` resolves against, in the document `slug` names. Each
 *  document asks its own words: a block asking what another document of the issue asks under the
 *  same id is a copy of that document's ask and opens none of its own. */
function questionBlocks(slug: string): string[] {
  return QUESTION_IDS.map((id) => questionBlock(id, slug));
}

function questionBlock(id: string, slug: string): string {
  return `:::ask{#${id} urgency="med" multiple="false"}\n${questionText(id, slug)}\n\n- Yes\n- No\n:::`;
}

function questionText(id: string, slug: string): string {
  return `Which way does question ${id} of ${slug} go?`;
}

/** Waits until every question block of `slug`'s document under the host has settled into its
 *  ask, and answers each block id's ask id (`GET /api/v1/issues/{key}/asks`, every state). */
async function settledAsks(slug: string): Promise<Record<string, string>> {
  const read = async () => {
    const asks: Record<string, string> = {};
    for (const ask of await listIssueAsks(HOST.key)) {
      if (ask.block_id && ask.block_artifact?.slug === slug) {
        asks[ask.block_id] = ask.id;
      }
    }
    return asks;
  };
  await expect
    .poll(async () => Object.keys(await read()).sort(), { timeout: 30_000 })
    .toEqual([...QUESTION_IDS].sort());
  return read();
}

/** Seeds the world every row's `issues` names: CORE-1, the host, whose spec holds the margin
 *  anchor, filler that puts its questions below the fold, and every question block a row names;
 *  and XCORE-1 in a project of its own. Answers the spec's settled asks by block id. */
async function seedWorld(): Promise<Record<string, string>> {
  await createProject({ key: "CORE", name: "Core" });
  await createProject({ key: "XCORE", name: "Cross core" });
  const filler = Array.from(
    { length: 40 },
    (_, index) => `Filler paragraph ${index + 1} keeps the questions below the fold.`
  );
  const spec = [ANCHOR, ...filler, ...questionBlocks("spec"), ""].join("\n\n");
  const host = await createIssue({ project: "CORE", spec, title: HOST.title });
  const other = await createIssue({ project: "XCORE", title: OTHER.title });
  const named = new Set(DISPATCH_TEXT_REFERENCES.flatMap((row) => row.issues ?? []));
  if (host.key !== HOST.key || other.key !== OTHER.key) {
    throw new Error(`setup: the world made ${host.key} and ${other.key}`);
  }
  if ([...named].sort().join() !== [HOST.key, OTHER.key].sort().join()) {
    throw new Error(`setup: the table's rows name issues ${[...named].join(", ")}`);
  }
  return settledAsks("spec");
}

/** The keys a row writes where the bare-key grammar could read them: each issue-key-shaped token
 *  at the start of the text or after whitespace, `;` (a numeric reference to whitespace) or one
 *  of ( [ " ' * _ > ` ~ |, outside an address. A superset of what the grammar reads. */
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

/** The world's precondition, a setup error when it fails: each row's `issues` exist, and every
 *  other key the row writes where the grammar could read it answers 404. */
async function expectWorldHolds(cases: readonly Case[]): Promise<void> {
  const statuses = new Map<string, Promise<number>>();
  const status = (key: string) => {
    let answered = statuses.get(key);
    if (answered === undefined) {
      answered = issueStatus(key);
      statuses.set(key, answered);
    }
    return answered;
  };
  for (const item of cases) {
    for (const key of item.issues) {
      if ((await status(key)) !== 200) {
        throw new Error(`setup: ${rowName(item)} needs ${key}, which is missing`);
      }
    }
    for (const key of standaloneKeys(item.body)) {
      if (!item.issues.includes(key) && (await status(key)) !== 404) {
        throw new Error(
          `setup: ${rowName(item)} needs ${key} to name no issue, and it answers ${await status(key)}`
        );
      }
    }
  }
}

/** The title each target the world made reads as, by its reference. */
function madeTitles(asks: Readonly<Record<string, string>>, slug: string): Map<string, string> {
  return new Map([
    [`dispatch://${HOST.key}`, `${HOST.key} ${HOST.title}`],
    [`dispatch://${OTHER.key}`, `${OTHER.key} ${OTHER.title}`],
    ...Object.entries(asks).map(
      ([id, ask]) => [`dispatch://${HOST.key}/ask/${ask}`, questionText(id, slug)] as const
    ),
  ]);
}

/** Checks that every reference in scope to a target the world made reads as its title. */
async function expectMadeTitles(
  scope: Locator,
  selector: (reference: string) => string,
  titles: ReadonlyMap<string, string>,
  item: Case
): Promise<void> {
  for (const link of item.links) {
    const title = titles.get(link);
    if (title !== undefined) {
      const found = scope.locator(selector(link));
      for (const each of await found.all()) {
        await expect(each, rowName(item)).toHaveText(title);
      }
    }
  }
}

/** What the renderer links: its reference anchors and Dispatch pictures. */
const RENDERED = "a[data-dispatch-ref], a[data-dispatch-picture]";
/** What an inert preview shows as a reference: its link-styled spans and pictures. */
const INERT = "[data-dispatch-ref], [data-dispatch-picture]";

async function signedIn(browser: Browser): Promise<{ close(): Promise<void>; page: Page }> {
  const context = await asUser(browser, "alice");
  return { close: () => context.close(), page: await context.newPage() };
}

/** The phone layout opens a margin thread in the Thread dialog over the sheet; Back closes it. */
async function closeThread(page: Page, project: string): Promise<void> {
  if (project !== "iphone") {
    return;
  }
  const thread = page.getByRole("dialog", { name: "Thread" });
  await thread.getByRole("button", { name: "Back" }).click();
  await expect(thread).toHaveCount(0);
}

test("a margin comment links each reference the shared table links and nothing else, and a link opens its target", async ({
  browser,
}, testInfo) => {
  test.setTimeout(60_000 + DISPATCH_TEXT_REFERENCES.length * 3_000);
  const asks = await seedWorld();
  const cases = casesFor(asks);
  await expectWorldHolds(cases);
  const titles = madeTitles(asks, "spec");
  // One comment per row, each anchored to the spec, whose questions a `#id` resolves against.
  const comments = new Map<Case, string>();
  for (const item of cases) {
    const comment = await createComment(
      HOST.key,
      { anchor: { artifact: "spec", quote: ANCHOR }, body: item.body },
      referenceAgent
    );
    expect(comment.anchor?.quote).toBe(ANCHOR);
    comments.set(item, comment.id);
  }
  const alice = await signedIn(browser);
  try {
    const { page } = alice;
    const project = testInfo.project.name;
    const open = (item: Case) => openMarginThread(page, project, comments.get(item) ?? "");
    await openSpec(page, HOST.key, ANCHOR);
    for (const item of cases) {
      const card = await open(item);
      await expect
        .poll(() => referenceTargetsIn(card, RENDERED), { message: rowName(item) })
        .toEqual(item.links);
      await expectMadeTitles(card, (link) => `a[data-dispatch-ref="${link}"]`, titles, item);
      await closeThread(page, project);
    }

    // A key opens its issue.
    const keyCard = await open(caseOf(cases, "See XCORE-1 here."));
    await keyCard.getByRole("link", { exact: true, name: `${OTHER.key} ${OTHER.title}` }).click();
    await expect(page).toHaveURL(new RegExp(`/issues/${OTHER.key}$`));
    await expect(page.getByRole("heading", { name: OTHER.title }).first()).toBeVisible();

    // A #id opens the document at that question, which starts below the fold.
    await openSpec(page, HOST.key, ANCHOR);
    const askCard = await open(caseOf(cases, "See #q2 for the rollout."));
    const block = documentEditor(page).locator('[data-dispatch-ask-block="q2"]');
    await expect(block).not.toBeInViewport();
    await askCard.getByRole("link", { exact: true, name: questionText("q2", "spec") }).click();
    await expect(page).toHaveURL(new RegExp(`/issues/${HOST.key}/spec#b-q2$`));
    await expect(block).toBeInViewport();
  } finally {
    await alice.close();
  }
});

test("the one-line preview shows each reference the shared table links as its target's title, with no link inside it", async ({
  browser,
}) => {
  test.setTimeout(60_000 + DISPATCH_TEXT_REFERENCES.length * 1_500);
  const asks = await seedWorld();
  const cases = casesFor(asks);
  await expectWorldHolds(cases);
  const titles = madeTitles(asks, "spec");
  // The Replies page shows a thread's newest reply by its first line, at most 160 characters: a
  // row that is one line that long is a reply there, one thread each. A longer row, or one of
  // several lines, cannot be shown whole there, so it is the parent a Conversation reply quotes,
  // the other inert one-line preview, which shows the parent's whole text.
  const threads = new Map<Case, string>();
  const quotes = new Map<Case, string>();
  for (const item of cases) {
    if (item.body.length <= 160 && !item.body.includes("\n")) {
      const root = await createComment(HOST.key, { body: `Where does row ${item.index} stand?` });
      await createComment(HOST.key, { body: item.body, reply_to: root.id }, referenceAgent);
      threads.set(item, root.id);
    } else {
      const parent = await createMessage(HOST.key, { body: item.body }, referenceAgent);
      const reply = await createMessage(
        HOST.key,
        { body: `Quoting row ${item.index}.`, in_reply_to: parent.id },
        referenceAgent
      );
      quotes.set(item, reply.id);
    }
  }
  // Each row that fits is its thread's whole first line, the thread on alice's Replies page.
  const firstLines = new Map<string, string>();
  for (let offset = 0; ; ) {
    const page = await listMyReplies({ limit: 50, offset });
    for (const row of page.rows) {
      firstLines.set(row.thread_id, row.reply.first_line);
    }
    offset += page.rows.length;
    if (page.rows.length === 0 || offset >= page.total) {
      break;
    }
  }
  expect(firstLines.size).toBe(threads.size);
  for (const [item, root] of threads) {
    expect(firstLines.get(root), rowName(item)).toBe(item.body);
  }

  const alice = await signedIn(browser);
  try {
    const { page } = alice;
    const expectPreview = async (preview: Locator, item: Case) => {
      await expect
        .poll(() => referenceTargetsIn(preview, INERT), { message: rowName(item) })
        .toEqual(item.links);
      await expectMadeTitles(preview, (link) => `[data-dispatch-ref="${link}"]`, titles, item);
      await expect(preview.locator("a"), rowName(item)).toHaveCount(0);
    };

    await page.goto("/replies");
    const rows = page.locator("[data-replies-row]");
    const more = page.getByRole("button", { name: "Show more" });
    await expect(async () => {
      if (await more.isVisible()) {
        await more.click();
      }
      expect(await rows.count()).toBe(threads.size);
    }).toPass();
    for (const [item, root] of threads) {
      const row = page.locator(`[data-replies-row="${root}"]`);
      await expectPreview(row.locator("[data-markdown-preview]").first(), item);
    }

    await page.goto(`/issues/${HOST.key}/conversation`);
    for (const [item, reply] of quotes) {
      const turn = page.locator(`[data-turn="message:${reply}"]`);
      await expectPreview(turn.locator("[data-markdown-preview]").first(), item);
    }
  } finally {
    await alice.close();
  }
});

test("the live editor draws each reference the shared table links whose target exists, follows it, and leaves the stored text as written", async ({
  browser,
}) => {
  const asks = await seedWorld();
  const written = casesFor(asks).filter((item) => EDITOR_EXCLUDED[item.row.body] === undefined);
  await expectWorldHolds(written);
  test.setTimeout(60_000 + written.length * 1_500);
  // A second document under the host, declaring the same question ids: there a `#id` resolves
  // against this document's own questions. One block per row.
  const content = [...written.map((item) => item.body), ...questionBlocks(DOCUMENT.slug), ""].join(
    "\n\n"
  );
  const uploaded = await createIssueArtifact(
    HOST.key,
    { content, name: DOCUMENT.name },
    referenceAgent
  );
  expect(uploaded.artifact.slug).toBe(DOCUMENT.slug);
  const documentAsks = await settledAsks(DOCUMENT.slug);
  const cases = casesFor(documentAsks).filter(
    (item) => EDITOR_EXCLUDED[item.row.body] === undefined
  );
  const titles = madeTitles(documentAsks, DOCUMENT.slug);
  // The editor draws a title only for a target that exists; a reference to any other shows as its
  // stored text. Of what the table links, this world makes the two issues, the host's spec and
  // the document's questions (no dashboard address or picture in the table names one of them,
  // and the editor reads neither).
  const made = new Set([...titles.keys(), `dispatch://${HOST.key}/spec`]);
  const stored = (await getArtifactText(uploaded.artifact.id)).markdown;

  const alice = await signedIn(browser);
  try {
    const { page } = alice;
    const editor = documentEditor(page);
    const open = async () => {
      await page.goto(`/issues/${HOST.key}/artifacts/${DOCUMENT.slug}`);
      await expect(editor).toContainText(questionText("q2", DOCUMENT.slug));
      await expect(connectedDot(page)).toHaveText("connected");
    };
    await open();
    const blocks = editor.locator(":scope > *");
    await expect(blocks).toHaveCount(cases.length + QUESTION_IDS.length);
    for (const [position, item] of cases.entries()) {
      const block = blocks.nth(position);
      const name = rowName(item);
      if (item.row.body === AUTHOR_LINK_ROW) {
        // An author's link mark: its words show, and nothing in it is a reference.
        await expect(block.getByRole("link", { exact: true, name: "the blocker" })).toBeVisible();
        await expect(block.locator("[data-dispatch-href]"), name).toHaveCount(0);
        continue;
      }
      await expect
        .poll(() => editorReferencesIn(block), { message: name })
        .toEqual(item.links.filter((link) => made.has(link)));
      await expectMadeTitles(
        block,
        (link) => `a.dispatch-reference[data-dispatch-href="${link}"]`,
        titles,
        item
      );
      for (const title of await block.locator("a.dispatch-reference").all()) {
        await expect(title, name).toBeVisible();
      }
      // A linked address written as text whose target is missing shows as written (a picture's
      // address is its destination, never text).
      for (const link of item.links) {
        if (!made.has(link) && item.body.replaceAll(`(${link})`, "").includes(link)) {
          await expect(block, name).toContainText(link, { useInnerText: true });
        }
      }
    }

    // A key opens its issue.
    const blockOf = (body: string) => blocks.nth(cases.indexOf(caseOf(cases, body)));
    await blockOf("See XCORE-1 here.").locator("a.dispatch-reference").click();
    await expect(page).toHaveURL(new RegExp(`/issues/${OTHER.key}$`));
    await expect(page.getByRole("heading", { name: OTHER.title }).first()).toBeVisible();
    expect((await getArtifactText(uploaded.artifact.id)).markdown).toBe(stored);

    // A #id opens the document at that question, which starts below the fold.
    await open();
    const question = blockOf("See #q2 for the rollout.").locator("a.dispatch-reference");
    await expect(question).toHaveAttribute(
      "data-dispatch-href",
      `dispatch://${HOST.key}/ask/${documentAsks.q2}`
    );
    const block = editor.locator('[data-dispatch-ask-block="q2"]');
    await expect(block).not.toBeInViewport();
    await question.click();
    await expect(page).toHaveURL(
      new RegExp(`/issues/${HOST.key}/artifacts/${DOCUMENT.slug}#b-q2$`)
    );
    await expect(block).toBeInViewport();
    expect((await getArtifactText(uploaded.artifact.id)).markdown).toBe(stored);
  } finally {
    await alice.close();
  }
});

test("a bare key that answered 404 is asked about again on issue.created only, not on another issue's update or close, and links once its issue is created with no reload", async ({
  browser,
}) => {
  test.setTimeout(60_000);
  await createProject({ key: "CORE", name: "Core" });
  const host = await createIssue({ project: "CORE", title: HOST.title });
  const renamed = await createIssue({ project: "CORE", title: "Rename the cache" });
  const closed = await createIssue({ project: "CORE", title: "Close the cache" });
  // Keys are numbered in order of creation, so the next issue of CORE takes CORE-4.
  const missing = "CORE-4";
  if ([host.key, renamed.key, closed.key].join() !== "CORE-1,CORE-2,CORE-3") {
    throw new Error(`setup: the world made ${host.key}, ${renamed.key} and ${closed.key}`);
  }
  for (const key of [missing, "UTF-8"]) {
    if ((await issueStatus(key)) !== 404) {
      throw new Error(`setup: ${key} must name no issue`);
    }
  }
  // The two issues the events are about are named too, so the page shows when it has read each.
  const comment = await createComment(
    host.key,
    { body: `Blocked on ${missing} and UTF-8, after ${renamed.key} and ${closed.key}.` },
    referenceAgent
  );

  const alice = await signedIn(browser);
  try {
    const { page } = alice;
    const issuePath = (key: string) => `/api/v1/issues/${key}`;
    // The page's issue reads by path: each request it sends, and each answer it gets.
    const asked = new Map<string, number>();
    const answered = new Map<string, number>();
    page.on("request", (request) => {
      if (request.method() === "GET") {
        const path = new URL(request.url()).pathname;
        asked.set(path, (asked.get(path) ?? 0) + 1);
      }
    });
    page.on("response", (response) => {
      if (response.request().method() === "GET") {
        const path = new URL(response.url()).pathname;
        answered.set(path, (answered.get(path) ?? 0) + 1);
      }
    });
    const askedAbout = (key: string) => asked.get(issuePath(key)) ?? 0;
    const answersAbout = (key: string) => answered.get(issuePath(key)) ?? 0;
    // The event stream carries only what commits after it opens, so the writes below wait for it.
    const streamOpen = page.waitForResponse(
      (response) => new URL(response.url()).pathname === "/api/v1/events"
    );
    await page.goto(`/issues/${host.key}/conversation`);
    const turn = page.locator(`[data-turn="comment:${comment.id}"]`);
    await expect(turn).toContainText(`Blocked on ${missing} and UTF-8, after`);
    // First load: each key that names no issue is asked about once, and its 404 has landed.
    for (const key of [missing, "UTF-8"]) {
      await expect.poll(() => answersAbout(key), `the first load's read of ${key}`).toBe(1);
      expect(askedAbout(key)).toBe(1);
    }
    const link = (key: string) => turn.locator(`a[data-dispatch-ref="dispatch://${key}"]`);
    await expect(link(renamed.key)).toHaveText(`${renamed.key} Rename the cache`);
    await expect(link(closed.key)).toHaveText(`${closed.key} Close the cache`);
    await expect(link(missing)).toHaveCount(0);
    await streamOpen;
    // A reload would drop this mark.
    await page.evaluate(() => {
      document.documentElement.dataset.e2eNoReload = "kept";
    });

    // issue.updated of another issue: the page reads the issues that exist again, so the renamed
    // title shows, and asks nothing about a key that answered 404.
    await patchIssue(renamed.key, { title: "Renamed cache" });
    await expect(link(renamed.key)).toHaveText(`${renamed.key} Renamed cache`);
    expect(askedAbout(missing), "an issue.updated asked about a 404 key").toBe(1);
    expect(askedAbout("UTF-8"), "an issue.updated asked about a 404 key").toBe(1);

    // issue.closed of another issue: the page reads the closed issue again, and asks nothing
    // about a key that answered 404. Every issue read one event queues is sent at once, so by
    // the closed issue's answer any read of a 404 key would have been sent too.
    const closedAnswers = answersAbout(closed.key);
    await patchIssue(closed.key, { status: "done" });
    await expect.poll(() => answersAbout(closed.key)).toBeGreaterThan(closedAnswers);
    expect(askedAbout(missing), "an issue.closed asked about a 404 key").toBe(1);
    expect(askedAbout("UTF-8"), "an issue.closed asked about a 404 key").toBe(1);
    await expect(link(missing)).toHaveCount(0);

    // issue.created: the key is asked about once more, and links with no reload or refocus.
    const created = await createIssue({ project: "CORE", title: "Unblock the release" });
    if (created.key !== missing) {
      throw new Error(`setup: the new issue is ${created.key}, not ${missing}`);
    }
    await expect(link(missing)).toHaveText(`${missing} Unblock the release`);
    expect(askedAbout(missing)).toBe(2);
    await expect(page.locator("html")).toHaveAttribute("data-e2e-no-reload", "kept");
  } finally {
    await alice.close();
  }
});

/** The line of `seedQuestionHost`'s spec that writes `#q2` followed by `-` and by `_`, as the
 *  server stores it: Markdown escapes the `_`, and the document reads `See #q2- and #q2_ now.` */
const TRAILING_LINE = "See #q2- and #q2\\_ now.";

/** Seeds CORE-1, whose spec holds the margin anchor, `TRAILING_LINE`, and the question blocks q2
 *  and q3, and answers each block's settled ask. */
async function seedQuestionHost(): Promise<
  Record<"q2" | "q3", { readonly id: string; readonly question: string }>
> {
  await createProject({ key: "CORE", name: "Core" });
  const spec = [
    ANCHOR,
    TRAILING_LINE,
    questionBlock("q2", "spec"),
    questionBlock("q3", "spec"),
    "",
  ];
  const host = await createIssue({ project: "CORE", spec: spec.join("\n\n"), title: HOST.title });
  if (host.key !== HOST.key) {
    throw new Error(`setup: the world made ${host.key}`);
  }
  return {
    q2: await settledBlockAsk(HOST.key, "q2"),
    q3: await settledBlockAsk(HOST.key, "q3"),
  };
}

test("the live editor draws #q2- and #q2_ as q2's title over #q2 only, keeps the - and the _ as text, and leaves the stored text as written", async ({
  browser,
}) => {
  test.setTimeout(60_000);
  const { q2 } = await seedQuestionHost();
  const stored = (await getIssueArtifactText(HOST.key, "spec")).markdown;
  if (!stored.includes(TRAILING_LINE)) {
    throw new Error(`setup: the spec holds no ${JSON.stringify(TRAILING_LINE)}: ${stored}`);
  }

  const alice = await signedIn(browser);
  try {
    const { page } = alice;
    await openSpec(page, HOST.key, ANCHOR);
    // The title is drawn before the text it hides, so the line is found by its end.
    const line = documentEditor(page).locator("p").filter({ hasText: "now." });
    await expect(line).toHaveCount(1);
    const titles = line.locator("a.dispatch-reference");
    await expect(titles).toHaveText([q2.question, q2.question]);
    for (const title of await titles.all()) {
      await expect(title).toHaveAttribute(
        "data-dispatch-href",
        `dispatch://${HOST.key}/ask/${q2.id}`
      );
      await expect(title).toBeVisible();
    }
    // Each title stands for the #q2 alone; the - and the _ after it stay drawn as text.
    await expect(line.locator(".dispatch-reference-text")).toHaveText(["#q2", "#q2"]);
    await expect(line).toHaveText(`See ${q2.question}- and ${q2.question}_ now.`, {
      useInnerText: true,
    });
    expect((await getIssueArtifactText(HOST.key, "spec")).markdown).toBe(stored);
  } finally {
    await alice.close();
  }
});

test("a margin comment that writes #q2 escaped (\\#q2, &#35;q2) beside #q3 links #q3 alone and shows #q2 as text", async ({
  browser,
}, testInfo) => {
  test.setTimeout(60_000);
  const { q3 } = await seedQuestionHost();
  const q3Reference = `dispatch://${HOST.key}/ask/${q3.id}`;
  const comments: string[] = [];
  for (const body of ["See \\#q2 and #q3.", "See &#35;q2 and #q3."]) {
    const comment = await createComment(
      HOST.key,
      { anchor: { artifact: "spec", quote: ANCHOR }, body },
      referenceAgent
    );
    expect(comment.anchor?.quote).toBe(ANCHOR);
    comments.push(comment.id);
  }

  const alice = await signedIn(browser);
  try {
    const { page } = alice;
    const project = testInfo.project.name;
    await openSpec(page, HOST.key, ANCHOR);
    for (const id of comments) {
      const card = await openMarginThread(page, project, id);
      // One render links every #id the document's question map names, so a reader that links
      // the escaped #q2 never shows #q3 linked alone.
      await expect
        .poll(() => referenceTargetsIn(card, RENDERED), { timeout: 15_000 })
        .toEqual([q3Reference]);
      await expect(card.locator(`a[data-dispatch-ref="${q3Reference}"]`)).toHaveText(q3.question);
      await expect(card.locator("p").filter({ hasText: "See #q2 and" })).toHaveText(
        `See #q2 and ${q3.question}.`
      );
      await closeThread(page, project);
    }
  } finally {
    await alice.close();
  }
});
