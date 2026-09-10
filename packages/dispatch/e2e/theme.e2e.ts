import { expect, type Locator, type Page, type TestInfo, test } from "@playwright/test";

import * as C from "../web/src/theme/classes";
import { oklchToSrgb8 } from "../web/src/theme/contrast";
import type { Swatch } from "../web/src/theme/palette";
import { createAsk, createComment, createIssue, createProject } from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

const session = {
  actor: { kind: "session" as const, id: "e2e-theme" },
  as: "agent" as const,
};

// Chromium serializes `getComputedStyle` colors in the color space they were declared in —
// Tailwind v4's default palette is `oklch()`, so `backgroundColor`/`color`/`borderBottomColor`
// come back as e.g. `oklch(0.208 0.042 265.755)`, not `rgb(...)`. Rather than pin that literal
// string (brittle against browser/Tailwind version formatting changes) or fall back to a
// lightness-threshold check (accepts any two colors on the right side of the light/dark line,
// not the *specific* token the component is supposed to render), this converts the actual
// computed color through the same OKLCH→sRGB math `palette.test.ts` uses to verify contrast,
// then compares it against the exact expected `classes.ts` `ColorPair` swatch for the
// element's role and color scheme — an 8-bit RGB triple, exact.
const OKLCH_PATTERN = /^oklch\(([\d.]+)\s+([\d.]+)\s+([\d.]+)/;
const RGB_PATTERN = /^rgba?\((\d+),\s*(\d+),\s*(\d+)/;

function toRgbString([r, g, b]: readonly [number, number, number]): string {
  return `rgb(${r}, ${g}, ${b})`;
}

/** Normalizes a computed color string (`oklch(...)` or `rgb(...)`/`rgba(...)`) to a canonical
 * `rgb(r, g, b)` 8-bit triple, dropping alpha — none of this suite's assertions target a
 * translucent element. */
function normalizeComputedColor(value: string): string {
  const oklchMatch = OKLCH_PATTERN.exec(value);
  if (oklchMatch !== null) {
    const [, l, c, h] = oklchMatch;
    return toRgbString(oklchToSrgb8({ c: Number(c), h: Number(h), l: Number(l) }));
  }
  const rgbMatch = RGB_PATTERN.exec(value);
  if (rgbMatch !== null) {
    const [, r, g, b] = rgbMatch;
    return toRgbString([Number(r), Number(g), Number(b)]);
  }
  throw new Error(`could not parse a css color from "${value}"`);
}

/** The exact 8-bit RGB `classes.ts` expects a `Swatch` to render as. */
function expectedRgb(swatch: Swatch): string {
  return toRgbString(oklchToSrgb8(swatch.oklch));
}

async function expectBackgroundColor(locator: Locator, swatch: Swatch): Promise<void> {
  const actual = await locator.first().evaluate((el) => getComputedStyle(el).backgroundColor);
  expect(normalizeComputedColor(actual)).toBe(expectedRgb(swatch));
}

async function expectTextColor(locator: Locator, swatch: Swatch): Promise<void> {
  const actual = await locator.first().evaluate((el) => getComputedStyle(el).color);
  expect(normalizeComputedColor(actual)).toBe(expectedRgb(swatch));
}

async function expectBorderColor(locator: Locator, swatch: Swatch): Promise<void> {
  const actual = await locator.first().evaluate((el) => getComputedStyle(el).borderBottomColor);
  expect(normalizeComputedColor(actual)).toBe(expectedRgb(swatch));
}

async function attachScreenshot(page: Page, testInfo: TestInfo, name: string): Promise<void> {
  const path = testInfo.outputPath(`${name}.png`);
  await page.screenshot({ path, fullPage: true });
  await testInfo.attach(name, { contentType: "image/png", path });
}

test.beforeEach(async () => {
  await resetDatabase();
});

test("the inbox renders the dark palette in dark mode and the light palette in light mode", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "Decide the rollout plan.",
    title: "Needs a decision",
  });
  const ask = await createAsk(
    issue.key,
    { anchor: { artifact: "spec", from: 0, to: 6 }, question: "Ship this week?" },
    session
  );
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    const card = page.getByTestId(`ask-${ask.id}`);

    await page.emulateMedia({ colorScheme: "light" });
    await page.goto("/");
    await expect(page.getByRole("heading", { name: "Inbox" })).toBeVisible();
    await expect(card).toBeVisible();
    await expectBackgroundColor(page.getByTestId("app-shell"), C.CANVAS.light);
    await expectBackgroundColor(card, C.SURFACE.light);
    await attachScreenshot(page, testInfo, "inbox-light");

    await page.emulateMedia({ colorScheme: "dark" });
    await expectBackgroundColor(page.getByTestId("app-shell"), C.CANVAS.dark);
    await expectTextColor(page.getByTestId("app-shell"), C.TEXT_PRIMARY.dark);
    await expectBackgroundColor(card, C.SURFACE.dark);
    await attachScreenshot(page, testInfo, "inbox-dark");
  } finally {
    await alice.close();
  }
});

test("an issue's spec and log tabs render the dark palette in dark mode and the light palette in light mode", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "Decide the rollout plan.",
    title: "Roll out the new pipeline",
  });
  await createComment(
    issue.key,
    { anchor: { artifact: "spec", from: 0, to: 6 }, body: "left a note" },
    session
  );
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    const title = page.getByRole("heading", { level: 1, name: issue.title });
    const header = page.locator("header").first();

    await page.emulateMedia({ colorScheme: "light" });
    await page.goto(`/issues/${issue.key}/spec`);
    await expect(title).toBeVisible();
    await expectTextColor(title, C.TEXT_PRIMARY.light);
    await attachScreenshot(page, testInfo, "issue-spec-light");

    await page.emulateMedia({ colorScheme: "dark" });
    await expectTextColor(title, C.TEXT_PRIMARY.dark);
    await expectBorderColor(header, C.BORDER_DEFAULT.dark);
    await attachScreenshot(page, testInfo, "issue-spec-dark");

    const logCard = page.getByRole("tabpanel", { name: "Log" }).locator("article").first();

    await page.emulateMedia({ colorScheme: "light" });
    await page.getByRole("tab", { name: "Log" }).click();
    await expect(logCard).toBeVisible();
    await expectBackgroundColor(logCard, C.SURFACE.light);
    await attachScreenshot(page, testInfo, "issue-log-light");

    await page.emulateMedia({ colorScheme: "dark" });
    await expectBackgroundColor(logCard, C.SURFACE.dark);
    await attachScreenshot(page, testInfo, "issue-log-dark");
  } finally {
    await alice.close();
  }
});

test("the margin review sheet renders the dark palette in dark mode on an iPhone viewport", async ({
  browser,
}, testInfo) => {
  test.skip(
    testInfo.project.name !== "iphone",
    "the margin sheet is a bottom sheet only on the iphone project"
  );
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "Decide the rollout plan.",
    title: "Roll out the new pipeline",
  });
  await createComment(
    issue.key,
    { anchor: { artifact: "spec", from: 0, to: 6 }, body: "left a note" },
    session
  );
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    const sheet = page.getByTestId("margin-sheet");

    await page.emulateMedia({ colorScheme: "light" });
    await page.goto(`/issues/${issue.key}/spec`);
    await page.getByRole("button", { name: "Open review panel" }).click();
    await expect(sheet).toBeVisible();
    await expectBackgroundColor(sheet, C.SURFACE.light);
    await attachScreenshot(page, testInfo, "margin-sheet-iphone-light");

    await page.emulateMedia({ colorScheme: "dark" });
    await expectBackgroundColor(sheet, C.SURFACE.dark);
    await attachScreenshot(page, testInfo, "margin-sheet-iphone-dark");
  } finally {
    await alice.close();
  }
});

test("Settings renders the dark palette in dark mode and the light palette in light mode", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    // The mappings table sits in a bordered card `<div>` that is its nearest ancestor `<div>` —
    // a structural lookup that survives class-name churn in SettingsPage.tsx.
    const card = page.locator("table").locator("xpath=ancestor::div[1]");
    const heading = page.getByRole("heading", { name: "Settings" });

    await page.emulateMedia({ colorScheme: "light" });
    await page.goto("/settings");
    await expect(heading).toBeVisible();
    await expectBackgroundColor(card, C.SURFACE.light);
    await expectTextColor(heading, C.TEXT_PRIMARY.light);
    await attachScreenshot(page, testInfo, "settings-light");

    await page.emulateMedia({ colorScheme: "dark" });
    await expectBackgroundColor(card, C.SURFACE.dark);
    await expectTextColor(heading, C.TEXT_PRIMARY.dark);
    await attachScreenshot(page, testInfo, "settings-dark");
  } finally {
    await alice.close();
  }
});
