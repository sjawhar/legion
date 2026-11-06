import { expect, type Page, test } from "@playwright/test";

import { createComment, createIssue, createProject, patchIssue } from "./api";
import { sql } from "./psql";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

// Check k18 of dispatch://INFRA-1042/artifact/verification-md, serving R16: the operator forms
// the query already reads are offered in the bar as a person types, each narrowing the list it
// is typed into, and a project, a status, an author and a date are chips the person adds, each
// narrowing that list and each reading back in the words they chose.

/** The titles the operator rows are asserted on. Each form of the query holds a different set
 *  of them: `brass astrolabe` all but the sextant one, the phrase all but `Astrolabe of brass`,
 *  the `OR` only the two naming a sextant or a theodolite, and `-steel` only the first. */
const operatorTitles = [
  "Brass astrolabe sextant case",
  "Brass astrolabe theodolite steel mount",
  "Brass astrolabe mirror",
  "Astrolabe of brass",
  "Brass sextant",
];

/** Opens the bar with `/`. The queries below name no action and no project, so every row the
 *  list holds for them is a hit. */
async function openSearch(page: Page): Promise<void> {
  await page.locator("body").focus();
  await page.keyboard.press("/");
  await expect(page.getByRole("dialog", { name: "Search" })).toBeVisible();
}

/** The day three days back: the value the date control takes, and the words the chip reads it
 *  back as, in the context's own locale and time zone. */
function threeDaysBack(): { text: string; value: string } {
  const day = new Date(Date.now() - 3 * 24 * 60 * 60 * 1000);
  const thisYear = day.getUTCFullYear() === new Date().getUTCFullYear();
  return {
    text: new Intl.DateTimeFormat("en-GB", {
      day: "numeric",
      month: "long",
      timeZone: "UTC",
      ...(thisYear ? {} : { year: "numeric" }),
    }).format(day),
    value: day.toISOString().slice(0, 10),
  };
}

test.beforeEach(async () => {
  await resetDatabase();
});

// SLOW(>=2s): five seeded issues and five searches of them, one per form the person adds to
// the words in the box.
test("the operator forms are offered as the words are typed, and each narrows the rows", async ({
  browser,
}) => {
  // Every check here carries the suite's own 15 s deadline, and a run where the bar shows none
  // of this reports each miss, which takes longer than one row's default budget.
  test.setTimeout(120_000);
  await createProject({ key: "INFRA", name: "INFRA work" });
  for (const title of operatorTitles) {
    // These titles share their words on purpose, which is what the related-item check refuses.
    await createIssue({ force: true, project: "INFRA", title });
  }
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    await page.goto("/");
    await openSearch(page);
    const dialog = page.getByRole("dialog", { name: "Search" });
    const search = page.getByRole("combobox", { name: "Search" });
    const options = dialog.getByRole("option");
    const forms = dialog.getByTestId("search-operators");
    const inUse = (name: string) => forms.locator(`[data-operator="${name}"][data-in-use="true"]`);

    // What the bar shows is checked with `expect.soft` and the rows with a plain `expect`, so
    // a run where the query reads an operator the bar never shows reports both halves.

    // All three are offered from the first keystroke, so a person who knows none of them is
    // told what the box can do while they are using it.
    await search.fill("brass");
    await expect
      .soft(forms.getByRole("listitem"), "the three forms are offered")
      .toContainText(['"two words"', "this OR that", "-word"]);
    await expect.soft(inUse("phrase")).toHaveCount(0);

    // The words as they stand: every issue holding both of them.
    await search.fill("brass astrolabe");
    await expect(options).toHaveCount(4);

    // The quotes narrow that list to the issues whose words are that phrase.
    await search.fill('"brass astrolabe"');
    await expect(options).toHaveCount(3);
    await expect.soft(inUse("phrase")).toHaveCount(1);

    // `OR` narrows it again: of those three, the two that also name a sextant or a theodolite.
    await search.fill('"brass astrolabe" sextant OR theodolite');
    await expect(options).toHaveCount(2);
    await expect.soft(inUse("or")).toHaveCount(1);
    await expect.soft(inUse("phrase")).toHaveCount(1);

    // `-word` narrows it to the one of those two that does not name steel.
    await search.fill('"brass astrolabe" sextant OR theodolite -steel');
    await expect(options).toHaveCount(1);
    await expect.soft(inUse("exclude")).toHaveCount(1);
    await expect(options.first()).toContainText("sextant case");

    // The harness runs no embedder, so every search here is the keyword half alone and the bar
    // says so above the hits it still shows (`e2e/run-server.sh`, DISPATCH_TEST_EMBEDDER_OFF).
    await expect.soft(dialog.getByTestId("search-degraded")).toContainText("Searching words only");
  } finally {
    await context.close();
  }
});

// SLOW(>=2s): two projects, three issues and five comments, and five searches of the seeded
// content, one per chip added.
test("each chip narrows the same list and reads back in the words the person chose", async ({
  browser,
}) => {
  await createProject({ key: "INFRA", name: "INFRA work" });
  await createProject({ key: "LINUX", name: "LINUX work" });
  const kept = await createIssue({ project: "INFRA", title: "Instrument log" });
  const elsewhere = await createIssue({ project: "LINUX", title: "Kernel log" });
  const started = await createIssue({ project: "INFRA", title: "Retired notes" });
  await patchIssue(kept.key, { status: "todo" });
  await patchIssue(elsewhere.key, { status: "todo" });
  await patchIssue(started.key, { status: "in_progress" });
  await createComment(kept.key, { body: "The astrolabe case is ready." });
  await createComment(elsewhere.key, { body: "The kernel astrolabe demo runs." });
  await createComment(started.key, { body: "This astrolabe is retired." });
  await createComment(kept.key, { body: "I polished the astrolabe mirror." }, { login: "bob" });
  const older = await createComment(kept.key, { body: "The astrolabe arrived last week." });
  await sql(`update comments set created_at = now() - interval '10 days' where id = '${older.id}'`);
  // The chip's words are the reader's own: this context reads dates as `3 November`.
  const context = await asUser(browser, "alice", { locale: "en-GB", timezoneId: "UTC" });
  const since = threeDaysBack();

  try {
    const page = await context.newPage();
    await page.goto("/");
    await openSearch(page);
    const dialog = page.getByRole("dialog", { name: "Search" });
    const search = page.getByRole("combobox", { name: "Search" });
    const options = dialog.getByRole("option");
    const chips = dialog.getByTestId("search-filters");

    // The row the four chips are added from, before any of them is used: a bar without it
    // fails here rather than at a click on a control that does not exist.
    await expect(chips).toBeVisible();
    await search.fill("astrolabe");
    await expect(options).toHaveCount(5);

    await dialog.getByRole("button", { name: "Project" }).click();
    await dialog.getByRole("button", { name: "INFRA" }).click();
    await expect(options).toHaveCount(4);

    await dialog.getByRole("button", { name: "Status" }).click();
    await dialog.getByRole("button", { name: "Todo" }).click();
    await expect(options).toHaveCount(3);

    await dialog.getByLabel("Author").fill("alice");
    await dialog.getByLabel("Author").press("Enter");
    await expect(options).toHaveCount(2);

    await dialog.getByLabel("Updated since").fill(since.value);
    await expect(options).toHaveCount(1);
    await expect(options.first()).toContainText("case is ready");

    // Each chip says what it narrowed by in the words it was chosen with, and none of them was
    // written into the words the person typed.
    await expect(chips).toContainText("Project INFRA");
    await expect(chips).toContainText("Status Todo");
    await expect(chips).toContainText("Author alice");
    await expect(chips).toContainText(`Updated since ${since.text}`);
    await expect(chips).not.toContainText("project:INFRA");
    await expect(search).toHaveValue("astrolabe");

    // A chip comes off on its own, and the rows it was holding back come back.
    await dialog.getByRole("button", { name: "Remove Author alice" }).click();
    await expect(options).toHaveCount(2);
    await expect(dialog.getByLabel("Author")).toBeVisible();
  } finally {
    await context.close();
  }
});
