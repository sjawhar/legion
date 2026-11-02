/**
 * What a `view` block costs the reader (dispatch://INFRA-682, A5, R6): the JavaScript a document
 * holding one makes the browser fetch, and the four rules that keep a page of blocks from
 * running them all - a frame starts only after the text around it shows and only near the
 * screen, at most four run at once, and a frame more than three screen heights away is removed.
 *
 * This file runs in the `chromium` project alone, and in no other project's `testMatch` or
 * `grep`. The byte row is a measure of one build against another, so a second engine would
 * measure that engine's own fetching rather than this change's bytes; and the three frame rows
 * rest on `IntersectionObserver` margins and on scroll positions the test sets, which the
 * `view-block`, `view-sandbox` and `view-grants` rows already exercise per engine. Running the
 * measure once, where the number it pins was measured, is what makes it a measure at all.
 */
import { expect, type Locator, type Page, type Request, test } from "@playwright/test";

import {
  createArtifactGrant,
  createIssue,
  createProject,
  createProjectDocument,
  uploadIssueFile,
} from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

/**
 * The bytes of JavaScript a build of the base commit - the commit before the `view` block, which
 * can store no such block - fetched to open this row's document with the block written as a
 * plain paragraph.
 *
 * Measured at `b284275e`, `main` at the merge base, on 2026-11-02: 1,780,431 bytes, from three
 * runs that agreed. The base moves with every forward merge, so re-measure it after one: a pin
 * left behind charges `main`'s own growth to this change, which it did twice here, by 1,839
 * bytes and then by 51.
 * To measure it again, in a checkout of that commit with this file copied in and cut to the byte
 * row alone, `e2e/api.ts`'s `uploadIssueFile`, `uploadProjectFile` and `createArtifactGrant`
 * added there (the base answers the grants route `404`, which that copy swallows) and that row
 * opening `BASE_DOCUMENT` in place of the directive and waiting for its caption's paragraph in
 * place of the block's frame, run:
 *
 *   (cd packages/contracts && bun run build) && cd packages/dispatch && bun run build:web && \
 *     DATABASE_URL='postgres://…' bun e2e/run.ts -- bunx playwright test \
 *     --config e2e/playwright.config.ts --project=chromium \
 *     view-budget.e2e.ts -g 'a view block costs the reader at most 15,000 more bytes'
 *
 * The row prints `view budget: … bytes of JavaScript` for the document it opened, at the base
 * commit and at this one alike, so the two runs the pull request quotes are those two lines.
 */
const BASE_JS_BYTES: number | undefined = 1_780_431;

/**
 * What R6 allows the change to add to that number. It read 10,000 until a person raised it to
 * 15,000 (dispatch://INFRA-682/ask/78430e75-b453-402d-924c-9506a00c6073, the spec approved at
 * version 11), having been shown what 10,000 would have cost: the four-frame budget and the
 * repaint splice both removed, which together bought 1,434 bytes and still left the change
 * 2,646 over, and which made a streamed agent turn rebuild a block's frame on every repaint.
 */
const BUDGET_BYTES = 15_000;

const CAPTION = "Attempts of the kernel boot loop.";

function viewDirective(block: { caption: string; data: string; page: string }): string {
  return `:::view{page="${block.page}" data="${block.data}"}\n${block.caption}\n:::\n`;
}

/** The same document as the row's, with the block written as the plain paragraph a base build
 *  can store. The base measurement opens this one. */
const BASE_DOCUMENT = `# Loop\n\n${CAPTION}\n`;

function viewBlock(page: Page, caption: string): Locator {
  return page.locator('section[data-proof-block-type="view"]').filter({ hasText: caption });
}

/** A paragraph per line, enough of them to put a block several screen heights down. */
function filler(lines: number): string {
  return Array.from(
    { length: lines },
    (_, index) => `Line ${index + 1}: the ledger and the mirror each keep a clock of their own.`
  ).join("\n\n");
}

interface Fixture {
  readonly dataRef: string;
  readonly issue: string;
  readonly pageRef: string;
}

/** A page file that answers `ready` and says nothing else, and a data file granted to it. */
async function seedView(): Promise<Fixture> {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Kernel boot loop" });
  const pageFile = await uploadIssueFile(issue.key, {
    content:
      '<!doctype html>\n<html lang="en"><body><p id="words">drawn</p>' +
      '<script>parent.postMessage({ type: "dispatch-view:ready" }, "*");</script></body></html>\n',
    name: "chart.html",
    type: "text/html; charset=utf-8",
  });
  await uploadIssueFile(issue.key, {
    content: '[{"label":"attempts","value":7}]',
    name: "loop.json",
    type: "application/json",
  });
  const dataRef = `dispatch://${issue.key}/artifact/loop-json`;
  await createArtifactGrant(pageFile.artifact.id, dataRef);
  return { dataRef, issue: issue.key, pageRef: `dispatch://${issue.key}/artifact/chart-html` };
}

async function openDocument(page: Page, content: string, name: string): Promise<void> {
  const document = await createProjectDocument("CORE", { content, name });
  await page.goto(`/projects/CORE/documents/${document.artifact.slug}`);
}

/** Every `.js` response's body size, and whether the page has been quiet. The quiet condition is
 *  `e2e/refetch-budget.e2e.ts`'s: no request in flight for 500 ms. */
function watchJavaScript(page: Page): {
  bytes(): Promise<number>;
  quietFor(ms: number): boolean;
} {
  const sizes: Promise<number>[] = [];
  const inFlight = new Set<Request>();
  let lastSettledAt = Date.now();
  page.on("request", (request) => inFlight.add(request));
  const settle = (request: Request) => {
    if (inFlight.delete(request)) {
      lastSettledAt = Date.now();
    }
  };
  page.on("requestfailed", settle);
  page.on("requestfinished", (request) => {
    settle(request);
    if (new URL(request.url()).pathname.endsWith(".js")) {
      sizes.push(request.sizes().then((measured) => measured.responseBodySize));
    }
  });
  return {
    bytes: async () => (await Promise.all(sizes)).reduce((total, size) => total + size, 0),
    quietFor: (ms) => inFlight.size === 0 && Date.now() - lastSettledAt >= ms,
  };
}

test.beforeEach(async () => {
  await resetDatabase();
});

test("a view block costs the reader at most 15,000 more bytes of JavaScript", async ({
  browser,
}) => {
  test.skip(
    BASE_JS_BYTES === undefined,
    "BASE_JS_BYTES is not measured yet. The constant's comment names the run that measures it, " +
      "in a worktree of the base commit; writing the number is all this row needs."
  );
  const fixture = await seedView();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const traffic = watchJavaScript(page);
    await openDocument(
      page,
      viewDirective({ caption: CAPTION, data: fixture.dataRef, page: fixture.pageRef }),
      "Budget"
    );
    await expect(viewBlock(page, CAPTION).frameLocator("iframe").locator("#words")).toHaveText(
      "drawn"
    );
    await expect
      .poll(() => traffic.quietFor(500), { intervals: [100], timeout: 30_000 })
      .toBe(true);

    const bytes = await traffic.bytes();
    // The two runs the pull request quotes are this line, here and at the base commit.
    console.log(`view budget: ${bytes} bytes of JavaScript`);
    expect(
      bytes,
      `A5: the document's JavaScript is ${bytes} bytes against the base's ${String(BASE_JS_BYTES)}`
    ).toBeLessThanOrEqual((BASE_JS_BYTES ?? 0) + BUDGET_BYTES);
    expect(
      BASE_DOCUMENT,
      "A5: the base build opens the same document as a plain paragraph"
    ).toContain(CAPTION);
  } finally {
    await alice.close();
  }
});

test("a view block far down the page reads nothing until the reader scrolls to it", async ({
  browser,
}) => {
  const fixture = await seedView();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const reads: string[] = [];
    page.on("request", (request) => {
      const path = new URL(request.url()).pathname;
      if (path.includes("/api/v1/artifacts/") && path.includes("/versions/")) {
        reads.push(path);
      }
    });
    await openDocument(
      page,
      `${filler(220)}\n\n${viewDirective({
        caption: CAPTION,
        data: fixture.dataRef,
        page: fixture.pageRef,
      })}`,
      "Far below"
    );
    const block = viewBlock(page, CAPTION);
    await expect(page.getByText("Line 1:", { exact: false }).first()).toBeVisible();
    const away = async () =>
      block.evaluate((element) => element.getBoundingClientRect().top / window.innerHeight);
    expect(
      await away(),
      "A5: the block starts more than one screen height below the screen"
    ).toBeGreaterThan(1);

    await page.waitForTimeout(2_000);
    expect(reads, "A5: nothing of the page file or the data file was read").toEqual([]);
    await expect(block.locator("iframe"), "A5: and no frame was inserted").toHaveCount(0);

    await block.scrollIntoViewIfNeeded();
    await expect(
      block.frameLocator("iframe").locator("#words"),
      "A5: scrolling to the block starts its frame"
    ).toHaveText("drawn");
    expect(reads.length, "A5: and only then is anything read").toBeGreaterThan(0);
  } finally {
    await alice.close();
  }
});

test("a view block more than three screen heights away loses its frame, and takes it back", async ({
  browser,
}) => {
  const fixture = await seedView();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await openDocument(
      page,
      `${filler(400)}\n\n${viewDirective({
        caption: CAPTION,
        data: fixture.dataRef,
        page: fixture.pageRef,
      })}`,
      "Scrolled away"
    );
    const block = viewBlock(page, CAPTION);
    await block.scrollIntoViewIfNeeded();
    await expect(block.frameLocator("iframe").locator("#words")).toHaveText("drawn");

    await page.getByText("Line 1:", { exact: false }).first().scrollIntoViewIfNeeded();
    const away = await block.evaluate(
      (element) => element.getBoundingClientRect().top / window.innerHeight
    );
    expect(away, "A5: the reader is now more than three screen heights away").toBeGreaterThan(3);
    await expect(block.locator("iframe"), "A5: so the frame is removed").toHaveCount(0, {
      timeout: 10_000,
    });

    await block.scrollIntoViewIfNeeded();
    await expect(
      block.frameLocator("iframe").locator("#words"),
      "A5: and it starts again when the block comes back"
    ).toHaveText("drawn");
  } finally {
    await alice.close();
  }
});

test("a view block asks for its page only after its caption is on the page", async ({
  browser,
}) => {
  const fixture = await seedView();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    // The block's first read of its page is the `HEAD` of the page route, which goes through
    // `fetch`. What the document held at that moment is what this records.
    await page.addInitScript(() => {
      const seen: (string | null)[] = [];
      Object.assign(window, { __whenThePageWasAskedFor: seen });
      const original = window.fetch;
      // `Object.assign` keeps whatever else the engine hangs off `fetch`, which is what its own
      // type requires of a replacement.
      window.fetch = Object.assign(
        (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
          const address =
            input instanceof URL
              ? input.href
              : input instanceof Request
                ? input.url
                : String(input);
          if (address.endsWith("/page")) {
            seen.push(
              document.querySelector('[data-proof-block-type="view"] [data-proof-block-content]')
                ?.textContent ?? null
            );
          }
          return original.call(window, input, init);
        },
        original
      );
    });
    await openDocument(
      page,
      viewDirective({ caption: CAPTION, data: fixture.dataRef, page: fixture.pageRef }),
      "Caption first"
    );
    await expect(viewBlock(page, CAPTION).frameLocator("iframe").locator("#words")).toHaveText(
      "drawn"
    );
    const captions = await page.evaluate<(string | null)[]>(() =>
      Reflect.get(window, "__whenThePageWasAskedFor")
    );
    expect(captions.length, "A5: the page was asked for").toBeGreaterThan(0);
    expect(
      captions[0],
      "A5: the block's caption was already on the page when its frame was asked for"
    ).toContain(CAPTION);
  } finally {
    await alice.close();
  }
});

test("a view block waits its turn where six of them sit on one screen", async ({ browser }) => {
  const fixture = await seedView();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await openDocument(
      page,
      Array.from({ length: 6 }, (_, index) =>
        viewDirective({
          caption: `Attempts, block ${index + 1}.`,
          data: fixture.dataRef,
          page: fixture.pageRef,
        })
      ).join("\n"),
      "Six blocks"
    );
    await expect(
      page.locator('section[data-proof-block-type="view"]'),
      "A5: six blocks"
    ).toHaveCount(6);
    await expect(
      page.locator('section[data-proof-block-type="view"] iframe'),
      "A5: four frames run at once"
    ).toHaveCount(4, { timeout: 30_000 });
    await expect(
      page.getByText("waiting", { exact: true }),
      "A5: and the other two wait their turn"
    ).toHaveCount(2);
  } finally {
    await alice.close();
  }
});
