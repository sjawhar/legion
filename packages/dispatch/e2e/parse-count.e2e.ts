/**
 * Every page that draws stored text draws the reading Go sent with it, and parses no markdown in
 * either of the browser's engines.
 *
 * Each row here opens one page of the dashboard, waits for a stored text to be on it - a body
 * formatted, so the row is never a count of a page that drew nothing - and reads the counters
 * `e2e/parse-counting.ts` installs, which say how that count reaches a built page. The last row
 * is the control: the same issue page, served with every tree stripped from its answers, parses
 * in the browser and the counter sees it.
 */
import { type BrowserContext, expect, type Locator, type Page, test } from "@playwright/test";

import { plannerSession, reviewerSession, setLiveSessions } from "./agents";
import {
  answerAsk,
  createAgentMessage,
  createAsk,
  createBroadcast,
  createComment,
  createIssue,
  createMessage,
  createProject,
  createProjectDocument,
  editArtifact,
  putArchitectureSource,
  syncArchitectureSource,
} from "./api";
import { seedFakeGithub } from "./fake-github-helpers";
import { countParses, readParseCounts, rewrittenParseCallSites } from "./parse-counting";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

const session = {
  actor: { id: plannerSession.session_id, kind: "session" as const },
  as: "agent" as const,
};

/** The architecture model the fake GitHub serves: one component, whose prose is markdown. */
const architecture = {
  "web.md": "---\ntitle: Web\npaths: [packages/web]\n---\nThe **SPA** every reader opens.\n",
};

/** A word no seeded text but the document's holds, so the palette's hits are its own. */
const searchTerm = "astrolabe";

/** What the pages this spec opens were seeded with. */
interface Seeded {
  readonly documentSlug: string;
  readonly issueKey: string;
  readonly targetKey: string;
}

async function seedStoredTexts(): Promise<Seeded> {
  await createProject({ key: "CORE", name: "Core" });
  await seedFakeGithub({
    "legion/arch": { contents: "read", files: architecture, installation_id: 101 },
  });
  await putArchitectureSource("CORE", { branch: "main", repo: "legion/arch" });
  const source = await syncArchitectureSource("CORE", "legion/arch");
  expect(source.last_error).toBeNull();

  const target = await createIssue({
    project: "CORE",
    spec: "The **target** issue, which a reference previews.\n",
    title: "Reference target",
  });
  const issue = await createIssue({
    project: "CORE",
    spec: "# Plan\n\nThe **spec** of the issue page.\n",
    title: "Stored texts",
  });
  await createComment(
    issue.key,
    { body: `A **comment** body, beside dispatch://${target.key}.` },
    session
  );
  await createMessage(issue.key, { body: "A **message** body." }, session);
  // An open ask, which the Inbox draws, and an answered one, whose answer the Answers page
  // draws.
  await createAsk(issue.key, { question: "An **open** question?" }, session);
  const ask = await createAsk(
    issue.key,
    { options: [{ label: "Ship" }, { label: "Hold" }], question: "An **ask** question?" },
    session
  );
  await answerAsk(ask.id, {
    expected_edited_at: null,
    selected: ["Ship"],
    text: "An **answer** of mine.",
  });
  // A thread alice wrote in that a session replied to: the Replies page draws both bodies.
  const thread = await createComment(issue.key, { body: "A **thread** alice opened." });
  await createComment(
    issue.key,
    { body: "A **reply** of the planner's.", reply_to: thread.id },
    session
  );
  // A message to an agent, which the Agents page draws, and a broadcast for the Broadcasts page.
  await setLiveSessions([plannerSession, reviewerSession]);
  await createAgentMessage(plannerSession.session_id, {
    body: "A **direct** message.",
    delivery: "aside",
  });
  await createBroadcast({
    body: "A **broadcast** body.",
    delivery: "aside",
    session_ids: [plannerSession.session_id],
  });

  // Two versions of a document, so version 1 is a past one, and the word the palette searches.
  const document = await createProjectDocument("CORE", {
    content: `# Instruments\n\nThe ${searchTerm} measures altitude.\n`,
    name: "instruments.md",
  });
  await editArtifact(
    document.artifact.id,
    { ops: [{ after: "end", markdown: "A **second** version.\n", op: "insert" }] },
    { as: "agent" }
  );

  return { documentSlug: document.artifact.slug, issueKey: issue.key, targetKey: target.key };
}

/** One page this spec counts the parses of: its name, and what it does to put a stored text on
 *  the screen. */
interface Surface {
  readonly name: string;
  readonly show: (page: Page) => Promise<void>;
}

/** A word a stored text holds in bold: drawn, it is a `strong` of its own, whether the page read
 *  it from a tree or parsed it; undrawn, there is no such element at all. */
function bold(page: Page, word: string): Locator {
  return page
    .locator("strong")
    .filter({ hasText: new RegExp(`^${word}$`) })
    .first();
}

function surfaces(seeded: Seeded): Surface[] {
  return [
    {
      name: "the issue page",
      show: async (page) => {
        await page.goto(`/issues/${seeded.issueKey}`);
        await expect(bold(page, "spec")).toBeVisible();
      },
    },
    {
      name: "the Inbox",
      show: async (page) => {
        await page.goto("/");
        await expect(bold(page, "open")).toBeVisible();
      },
    },
    {
      name: "the activity feed",
      show: async (page) => {
        await page.goto(`/issues/${seeded.issueKey}/activity`);
        // The feed draws each event as a line of its own rather than as a body, so its own
        // rows are what says the stored history is on the page.
        await expect(
          page.getByRole("list", { name: "Issue activity" }).getByRole("listitem").first()
        ).toBeVisible();
      },
    },
    {
      name: "a past version",
      show: async (page) => {
        await page.goto(`/projects/CORE/documents/${seeded.documentSlug}?version=1`);
        // The version's own section, not the document around it: the view that draws a past
        // version is what this row counts.
        await expect(
          page.getByRole("region", { name: "Document version 1" }).getByText("Instruments")
        ).toBeVisible();
      },
    },
    {
      name: "the search palette",
      show: async (page) => {
        await page.goto("/");
        await page.locator("body").focus();
        await page.keyboard.press("Control+k");
        const search = page.getByRole("combobox", { name: "Search" });
        await search.fill(searchTerm);
        await expect(
          page.getByRole("dialog", { name: "Search" }).getByRole("option").first()
        ).toBeVisible();
      },
    },
    {
      name: "a reference preview",
      show: async (page) => {
        await page.goto(`/issues/${seeded.issueKey}/conversation`);
        await page.getByRole("link", { name: "Reference target" }).first().hover();
        await expect(page.getByRole("tooltip")).toContainText(seeded.targetKey);
      },
    },
    {
      name: "the Answers page",
      show: async (page) => {
        await page.goto("/answers");
        await expect(bold(page, "answer")).toBeVisible();
      },
    },
    {
      name: "the Replies page",
      show: async (page) => {
        await page.goto("/replies");
        await expect(bold(page, "reply")).toBeVisible();
      },
    },
    {
      name: "the Agents page",
      show: async (page) => {
        await page.goto("/agents");
        // A session's row opens its conversation with Dispatch; the direct message sits behind
        // the broadcast that followed it.
        await page.getByRole("button", { exact: true, name: plannerSession.title }).click();
        await page.getByRole("button", { name: /^Show \d+ older$/ }).click();
        await expect(bold(page, "direct")).toBeVisible();
      },
    },
    {
      name: "the Broadcasts page",
      show: async (page) => {
        await page.goto("/agents/broadcasts");
        await expect(bold(page, "broadcast")).toBeVisible();
      },
    },
    {
      name: "a component's page",
      show: async (page) => {
        await page.goto("/projects/CORE/architecture?component=web");
        await expect(bold(page, "SPA")).toBeVisible();
      },
    },
  ];
}

/** Opens a page of `context`, shows `surface` on it and answers what it parsed. */
async function parsesOf(context: BrowserContext, surface: Surface): Promise<unknown> {
  const page = await context.newPage();
  try {
    await surface.show(page);
    return { ...(await readParseCounts(page)), surface: surface.name };
  } finally {
    await page.close();
  }
}

test.beforeEach(async () => {
  await resetDatabase();
});

test("no page that draws stored text parses markdown in either browser engine", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "chromium", "one engine proves what the pages parse");
  // Eleven pages, each loaded in a page of its own, on a two-core box: the default deadline is
  // one page's.
  test.setTimeout(240_000);
  const seeded = await seedStoredTexts();
  const alice = await asUser(browser, "alice");
  await countParses(alice);
  try {
    const pages = surfaces(seeded);
    const counted = [];
    for (const surface of pages) counted.push(await parsesOf(alice, surface));
    // One comparison over all eleven, so a page that parses is named beside the ten that do
    // not rather than stopping the walk at the first.
    expect(counted).toEqual(
      pages.map((surface) => ({ display: 0, editor: 0, surface: surface.name, workerRequests: 0 }))
    );
    // The counter is in the build these pages ran: a rename that left it counting nothing would
    // pass every row above.
    expect(rewrittenParseCallSites(alice)).toBeGreaterThan(0);
  } finally {
    await alice.close();
  }
});

test("the counter sees the parse a text served with no reading costs", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "chromium", "the control runs where the rows run");
  const seeded = await seedStoredTexts();
  const alice = await asUser(browser, "alice");
  await countParses(alice);
  // Every tree out of the answer, as a server that sends none would: the page then has the
  // markdown alone, which it parses itself.
  await alice.route(/\/api\/v1\//, async (route) => {
    const response = await route.fetch();
    const body = (await response.text()).replaceAll(/"[a-z_]+_tree":/g, '"dropped_tree":');
    const headers = { ...response.headers() };
    delete headers["content-encoding"];
    delete headers["content-length"];
    await route.fulfill({ body, headers, status: response.status() });
  });
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${seeded.issueKey}`);
    await expect(bold(page, "spec")).toBeVisible();
    expect((await readParseCounts(page)).display).toBeGreaterThan(0);
  } finally {
    await alice.close();
  }
});
