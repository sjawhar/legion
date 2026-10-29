import { setTimeout as sleep } from "node:timers/promises";

import { expect, type Locator, type Page, test } from "@playwright/test";

import { RELATED_DEBOUNCE_MS } from "../web/src/features/issue/related-timing";
import { createIssue, createProject } from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

/** The no-write check the dialog asks, for any project. A glob would read `?` as a wildcard. */
const relatedRoute = /\/api\/v1\/projects\/[^/]+\/related(\?|$)/u;

const astrolabeTitle = "Astrolabe drift spoils the mooring calibration";
const zeppelinTitle = "Zeppelin ballast valve sticks in the cold";

/** The check the dialog asks for a title holding `word`, awaited where the row needs its answer
 *  to have reached the page. */
function checkFor(page: Page, word: string): Promise<unknown> {
  return page.waitForResponse(
    (response) =>
      relatedRoute.test(response.url()) &&
      (new URL(response.url()).searchParams.get("title") ?? "").includes(word)
  );
}

/** Hold the page's clock still, so a check about the debounce cannot race it. `install()` alone
 * leaves the clock running at real speed; `pauseAt` is what stops it. It takes a time on the
 * page's own installed clock, and the Node process's `Date.now()` can already be behind that
 * clock, which would ask it to move backwards, so the lead of a second is past any skew. It runs
 * every page timer that falls due inside the jump; at both call sites the title box is still
 * empty, so no check is armed and the jump arms none. */
async function pauseClock(page: Page): Promise<void> {
  await page.clock.pauseAt(Date.now() + 1000);
}

async function seedProject(): Promise<{ astrolabe: string; zeppelin: string }> {
  await createProject({ key: "CORE", name: "Core" });
  const astrolabe = await createIssue({
    project: "CORE",
    spec: "The astrolabe drifts while the mooring mast is calibrated.",
    title: astrolabeTitle,
  });
  const zeppelin = await createIssue({
    project: "CORE",
    spec: "The zeppelin's ballast valve sticks below freezing.",
    title: zeppelinTitle,
  });
  return { astrolabe: astrolabe.key, zeppelin: zeppelin.key };
}

/** The create dialog, opened with `c` from the shell, and its title box and related section. */
async function openCreateDialog(
  page: Page
): Promise<{ dialog: Locator; related: Locator; title: Locator }> {
  await page.locator("body").focus();
  await page.keyboard.press("c");
  const dialog = page.getByRole("dialog", { name: "Create issue" });
  await expect(dialog).toBeVisible();
  const title = dialog.getByRole("textbox", { name: "Title" });
  await expect(title).toBeFocused();
  return { dialog, related: dialog.getByRole("region", { name: "Related items" }), title };
}

test.beforeEach(async () => {
  await resetDatabase();
});

test("a title whose rare words match an existing issue lists it with its link", async ({
  browser,
}, testInfo) => {
  const seeded = await seedProject();
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.goto("/");
    await expect(page.getByRole("heading", { name: "Inbox" })).toBeVisible();
    const { related, title } = await openCreateDialog(page);

    await title.fill("Astrolabe drift during mooring");
    const hit = related.getByRole("link", { name: `${seeded.astrolabe} ${astrolabeTitle}` });
    await expect(hit).toBeVisible();
    await expect(hit).toHaveAttribute("href", `/issues/${seeded.astrolabe}`);
    // The draft must survive the click, so the link leaves this tab alone.
    await expect(hit).toHaveAttribute("target", "_blank");
    await expect(related.getByRole("link", { name: new RegExp(seeded.zeppelin) })).toHaveCount(0);
    await page.screenshot({
      path: testInfo.outputPath(`create-issue-related-${testInfo.project.name}.png`),
    });

    // The control: a title sharing no rare word with anything in the project says so, in words
    // that cannot be read as a check that never ran.
    await title.fill("Repaint the tender's bollards");
    await expect(related.getByText(/searched .*and found nothing\.$/u)).toBeVisible();
    await expect(related.getByRole("link")).toHaveCount(0);
  } finally {
    await context.close();
  }
});

test("typing sends one check per pause, never one per keystroke", async ({ browser }) => {
  await seedProject();
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    // The debounce is the subject, so the page's timers are this test's to drive: wall-clock
    // typing would race the very timer being measured.
    await page.clock.install();
    await page.goto("/");
    await expect(page.getByRole("heading", { name: "Inbox" })).toBeVisible();

    let checks = 0;
    await page.route(relatedRoute, async (route) => {
      checks += 1;
      await route.continue();
    });

    const { title } = await openCreateDialog(page);
    await pauseClock(page);
    await title.pressSequentially("Astrolabe drift", { delay: 0 });
    await expect(title).toHaveValue("Astrolabe drift");
    expect(checks).toBe(0);

    // Fifteen keystrokes and all but the last millisecond of the rest have asked nothing.
    await page.clock.runFor(RELATED_DEBOUNCE_MS - 1);
    expect(checks).toBe(0);
    await page.clock.runFor(1);
    await expect.poll(() => checks).toBe(1);

    // The pause is over, so nothing more is asked while the title stands.
    await page.clock.runFor(RELATED_DEBOUNCE_MS * 3);
    expect(checks).toBe(1);
  } finally {
    await context.close();
  }
});

test("an answer that arrives after the title changed is not shown for the new title", async ({
  browser,
}) => {
  const seeded = await seedProject();
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.clock.install();
    await page.goto("/");
    await expect(page.getByRole("heading", { name: "Inbox" })).toBeVisible();

    // The first title's check is held, then released while the box already holds the second
    // one - inside its debounce window, so the first check is still the one in flight and its
    // answer really does land under the dialog's eyes. Released any later, react-query would
    // have aborted it and the gate would never be asked.
    const { promise: held, resolve: release } = Promise.withResolvers<void>();
    const { promise: firstAsked, resolve: asked } = Promise.withResolvers<string>();
    await page.route(relatedRoute, async (route) => {
      const title = new URL(route.request().url()).searchParams.get("title") ?? "";
      if (title.toLowerCase().includes("astrolabe")) {
        asked(title);
        await held;
      }
      await route.continue();
    });

    const { related, title } = await openCreateDialog(page);
    await pauseClock(page);
    await title.fill("Astrolabe drift during mooring");
    await page.clock.runFor(RELATED_DEBOUNCE_MS);
    expect(await firstAsked).toBe("Astrolabe drift during mooring");

    await title.fill("Zeppelin ballast valve");
    // Nothing on the page says a dropped answer was seen, so the row waits for the held answer
    // to reach the page, lets react-query deliver it, and then gives the render a margin. The
    // delivery needs a tick because react-query batches its notifications on a zero-delay
    // timer, and this page's timers are the test's; one millisecond is nowhere near the second
    // title's own 400 ms pause, which must not fire yet. The margin is Node's clock, since a
    // page-side wait would never return. It is proven by the mutation the pull request
    // records: with the display gate removed, this assertion fails, three runs of three.
    const answered = checkFor(page, "Astrolabe");
    release();
    await answered;
    await page.clock.runFor(1);
    await sleep(250);

    // The answer that landed is the first title's, so the dialog still says it is checking and
    // shows no link to the issue that answer named.
    await expect(related.getByText("Checking for related items…")).toBeVisible();
    await expect(related.getByRole("link", { name: new RegExp(seeded.astrolabe) })).toHaveCount(0);

    // The second title's own pause then brings its own answer, and the dropped one never appears.
    const second = checkFor(page, "Zeppelin");
    await page.clock.runFor(RELATED_DEBOUNCE_MS);
    await second;
    await page.clock.runFor(1);
    await expect(
      related.getByRole("link", { name: `${seeded.zeppelin} ${zeppelinTitle}` })
    ).toBeVisible();
    await expect(related.getByRole("link", { name: new RegExp(seeded.astrolabe) })).toHaveCount(0);
  } finally {
    await context.close();
  }
});
