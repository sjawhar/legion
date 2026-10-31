import { type Browser, type BrowserContext, expect, type Page, test } from "@playwright/test";

import { baseUrl, createIssue, createProject } from "./api";
import {
  connectedDot,
  countDocumentSockets,
  cursorLabel,
  documentEditor,
  placeCaret,
  typeAtEnd,
} from "./editor";
import { type DispatchProcess, processPinCookie } from "./harness-processes";
import type { ProxiedSocket } from "./round-robin-proxy";
import { resetDatabase } from "./seed";
import { signIn } from "./users";

/**
 * Two people on one document, each served by a different Dispatch process on one Postgres, as two
 * pods behind a load balancer serve them: alice's page is pinned to Dispatch A and bob's to B.
 * Each sees what the other types and the other's cursor within one second, and neither page's
 * document socket is closed or replaced on the way: one `/ws/doc/` socket per page, open from
 * start to end, and a connection dot that never leaves "connected".
 *
 * It runs in two-process mode (`bun run e2e:multiprocess`, DISPATCH_E2E_PROCESSES=2) alone, as
 * every `*.multiprocess.e2e.ts` does (e2e/playwright.config.ts): it reads the round-robin proxy's
 * socket log (`GET /_proxy/sockets`) and pins each page with the proxy's cookie.
 */

/** What a page must show within this long of the keystroke that caused it. */
const liveWithin = 1000;

/** How often each page reads its own connection dot. */
const samplingInterval = 100;

// No heading: the editor gives each heading an id of its own as it opens a document, an update
// that would land on one process while the other loads the room.
const spec = "Use SQLite for the cache.\n\nEnd of spec.\n";

test.beforeEach(async () => {
  await resetDatabase();
  const reset = await fetch(new URL("/_proxy/reset", baseUrl), { method: "POST" });
  expect(reset.status, "POST /_proxy/reset").toBe(204);
});

/** A context signed in as `login` whose every request, the document socket's included, goes to
 *  `process`. The pin is set before the sign-in, so the sign-in is that process's too. Viewport
 *  and device are the project's (Desktop Chrome, 1280 px wide), as doc.e2e.ts's cursor test runs
 *  in: below a 1280 px editor the document has no awareness (ProofDocument.tsx). */
async function pinnedUser(
  browser: Browser,
  login: string,
  process: DispatchProcess
): Promise<BrowserContext> {
  const context = await browser.newContext();
  await context.addCookies([{ name: processPinCookie, url: baseUrl, value: process }]);
  await signIn(context, login);
  return context;
}

async function documentSockets(): Promise<ProxiedSocket[]> {
  const response = await fetch(new URL("/_proxy/sockets", baseUrl));
  expect(response.status, "GET /_proxy/sockets").toBe(200);
  // The proxy's own answer, the rows round-robin-proxy.ts writes.
  const sockets = (await response.json()) as ProxiedSocket[];
  return sockets.filter(({ path }) => path.startsWith("/ws/doc/"));
}

interface ConnectionReading {
  /** Milliseconds since the page started reading. */
  readonly at: number;
  /** The dot's title (ConnectionDot.tsx: the state's label, then any pending-edit notice), or
   *  "absent" when the page shows no dot. */
  readonly label: string;
}

interface ConnectionReadings {
  readonly samples: number;
  /** Every reading whose state is not "connected", sampled or seen changing. */
  readonly outages: ConnectionReading[];
}

/** What a page keeps while it watches its connection dot. */
interface ConnectionWatch {
  samples: number;
  outages: ConnectionReading[];
  stop(): void;
}

/** Has `page` read its own connection dot every `samplingInterval`, and on every change to it in
 *  between, until `stop`. The page reads it, not this process: a Playwright round trip per reading
 *  is slower than the interval on a busy machine. The dot is the document's `role="status"`
 *  element whose title starts with one of `connectionLabel`'s states (features/doc/connection.ts). */
async function watchConnection(page: Page): Promise<{ stop(): Promise<ConnectionReadings> }> {
  const watch = await page.evaluateHandle((interval): ConnectionWatch => {
    const states = [
      "connected",
      "offline",
      "Connecting to the document…",
      "The document could not load",
    ];
    const started = performance.now();
    const label = () => {
      for (const element of document.querySelectorAll<HTMLElement>('[role="status"][title]')) {
        if (states.includes(element.title.split(" · ")[0] ?? "")) return element.title;
      }
      return "absent";
    };
    const watching: ConnectionWatch = {
      outages: [],
      samples: 0,
      stop: () => {
        window.clearInterval(timer);
        observer.disconnect();
      },
    };
    const read = (sampled: boolean) => {
      if (sampled) watching.samples += 1;
      const current = label();
      if (current.split(" · ")[0] !== "connected") {
        watching.outages.push({ at: Math.round(performance.now() - started), label: current });
      }
    };
    const timer = window.setInterval(() => read(true), interval);
    const observer = new MutationObserver(() => read(false));
    observer.observe(document.body, {
      attributeFilter: ["title"],
      attributes: true,
      childList: true,
      subtree: true,
    });
    read(true);
    return watching;
  }, samplingInterval);
  return {
    stop: () =>
      watch.evaluate((watching) => {
        watching.stop();
        return { outages: watching.outages, samples: watching.samples };
      }),
  };
}

test("two people on two processes see each other's typing and cursor within a second, on one socket each that stays connected", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", spec, title: "Cache decision" });
  const alice = await pinnedUser(browser, "alice", "a");
  const bob = await pinnedUser(browser, "bob", "b");

  try {
    // Each page's connection dot is read from the moment it first connects to the end.
    const alicePage = await alice.newPage();
    const aliceSockets = countDocumentSockets(alicePage);
    await alicePage.goto(`/issues/${issue.key}/spec`);
    const aliceEditor = documentEditor(alicePage);
    await expect(aliceEditor).toContainText("Use SQLite");
    await expect(connectedDot(alicePage)).toHaveText("connected");
    const aliceConnection = await watchConnection(alicePage);

    const bobPage = await bob.newPage();
    const bobSockets = countDocumentSockets(bobPage);
    await bobPage.goto(`/issues/${issue.key}/spec`);
    const bobEditor = documentEditor(bobPage);
    await expect(bobEditor).toContainText("Use SQLite");
    await expect(connectedDot(bobPage)).toHaveText("connected");
    const bobConnection = await watchConnection(bobPage);

    // Every behaviour check below is soft, so a run that breaks several names every one. Alice's
    // socket is A's and Bob's is B's: the only document sockets, one per page.
    await expect.poll(async () => (await documentSockets()).length).toBeGreaterThanOrEqual(2);
    const opened = await documentSockets();
    const onePerPage = [
      { open: true, upstream: "a" },
      { open: true, upstream: "b" },
    ];
    expect
      .soft(
        opened.map(({ open, upstream }) => ({ open, upstream })),
        "once both pages are connected: one open document socket on A (alice's page) and one on " +
          "B (bob's page)"
      )
      .toEqual(onePerPage);
    expect.soft(aliceSockets(), "document sockets alice's page opened").toBe(1);
    expect.soft(bobSockets(), "document sockets bob's page opened").toBe(1);

    // Each wait starts at the last keystroke, so what it allows is the time from that keystroke.
    await typeAtEnd(alicePage, "Alice types on A.");
    await expect
      .soft(bobEditor, "bob's page on B shows alice's typing within 1 s of her last keystroke")
      .toContainText("Alice types on A.", { timeout: liveWithin });
    await expect
      .soft(cursorLabel(bobPage, "alice"), "bob's page on B shows alice's cursor within 1 s")
      .toBeVisible({ timeout: liveWithin });

    // Somewhere other than Alice's caret: a peer's caret on the focused local caret is not drawn.
    // Each page's text is matched up to the other's caret, whose label the editor draws inline
    // right after what that person typed.
    await placeCaret(bobPage, "after", "Use SQLite");
    await bobPage.keyboard.type(" and Postgres");
    await expect
      .soft(aliceEditor, "alice's page on A shows bob's typing within 1 s of his last keystroke")
      .toContainText("Use SQLite and Postgres", { timeout: liveWithin });
    await expect
      .soft(cursorLabel(alicePage, "bob"), "alice's page on A shows bob's cursor within 1 s")
      .toBeVisible({ timeout: liveWithin });

    // A second turn each: the first edit from the other process left each room live, not stale.
    await alicePage.keyboard.type(" Then Bob answers.");
    await expect
      .soft(bobEditor, "bob's page on B shows alice's second edit within 1 s")
      .toContainText("Alice types on A. Then Bob answers.", { timeout: liveWithin });
    await bobPage.keyboard.type(" everywhere");
    await expect
      .soft(aliceEditor, "alice's page on A shows bob's second edit within 1 s")
      .toContainText("Use SQLite and Postgres everywhere", { timeout: liveWithin });
    await expect
      .soft(cursorLabel(bobPage, "alice"), "bob's page on B still shows alice's cursor")
      .toBeVisible({ timeout: liveWithin });
    await expect
      .soft(cursorLabel(alicePage, "bob"), "alice's page on A still shows bob's cursor")
      .toBeVisible({ timeout: liveWithin });

    for (const [who, connection] of [
      ["alice's page on A", aliceConnection],
      ["bob's page on B", bobConnection],
    ] as const) {
      const { outages, samples } = await connection.stop();
      expect.soft(samples, `${who}: connection dot samples`).toBeGreaterThan(1);
      expect
        .soft(
          outages,
          `${who}: the connection dot, read every ${samplingInterval} ms and on every change from ` +
            'its first connect, always reads "connected" (never offline, connecting or absent)'
        )
        .toEqual([]);
    }

    // The same two sockets, still open, and no other: neither page reconnected.
    const closing = await documentSockets();
    expect
      .soft(
        closing.map(({ open, upstream }) => ({ open, upstream })),
        "at the end: one open document socket on A (alice's page) and one on B (bob's page)"
      )
      .toEqual(onePerPage);
    expect
      .soft(closing, "the document sockets at the end are the ones open at the start")
      .toEqual(opened);
    expect.soft(aliceSockets(), "document sockets alice's page opened, start to end").toBe(1);
    expect.soft(bobSockets(), "document sockets bob's page opened, start to end").toBe(1);
  } finally {
    await bob.close();
    await alice.close();
  }
});
