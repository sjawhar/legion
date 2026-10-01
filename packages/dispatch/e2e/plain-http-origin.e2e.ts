import { expect, test } from "@playwright/test";

import { createComment, createIssue, createProject, getArtifactText } from "./api";
import { connectedDot, documentEditor, typeAtEnd } from "./editor";
import { plainHttpHost } from "./plain-http-origin";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

// Runs only in the `chromium-plain-http` project (e2e/playwright.config.ts, e2e/plain-http-origin.ts):
// the page's origin is a plain-HTTP host name that is not loopback, so it is no secure context and
// `crypto.randomUUID` is undefined there. Block ids and Markdown bodies must mint without it.
const agent = {
  actor: { kind: "session" as const, id: "e2e-plain-http", origin: { tmux: "dispatch:1.9" } },
  as: "agent" as const,
};
const typed = "Typed on a plain-HTTP origin.";

test.skip(
  process.env.PLAYWRIGHT_BASE_URL?.startsWith("https:") === true,
  "a deployed https server has no plain-HTTP origin to map"
);

test.beforeEach(async () => {
  await resetDatabase();
});

test("the page's origin is a plain-HTTP host name and not a secure context", async ({
  browser,
}) => {
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    const url = new URL(page.url());
    expect(url.protocol).toBe("http:");
    expect(url.hostname).toBe(plainHttpHost);
    expect(await page.evaluate(() => window.isSecureContext)).toBe(false);
    expect(await page.evaluate(() => typeof crypto.randomUUID)).toBe("undefined");
    expect(await page.evaluate(() => typeof crypto.getRandomValues)).toBe("function");
  } finally {
    await alice.close();
  }
});

test("a document opens and takes a new paragraph with no page error", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue(
    { project: "CORE", spec: "## Plan\n\nShip it.\n", title: "Plain-HTTP document" },
    agent
  );
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const errors: string[] = [];
    page.on("pageerror", (error) => errors.push(`${error.name}: ${error.message}`));
    await page.goto(`/issues/${issue.key}/spec`);
    await expect(documentEditor(page)).toContainText("Ship it.");
    await expect(connectedDot(page)).toHaveText("connected");

    await typeAtEnd(page, typed);
    // Its own paragraph: a failed Enter leaves the text appended to "Ship it." instead.
    await expect(documentEditor(page).locator("p", { hasText: typed })).toHaveText(typed);
    await expect
      .poll(async () => (await getArtifactText(issue.primary_artifact_id)).markdown)
      .toContain(typed);
    expect(errors).toEqual([]);
  } finally {
    await alice.close();
  }
});

test("a comment body renders formatted rather than as literal Markdown", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Plain-HTTP comment" });
  await createComment(issue.key, { body: "A **formatted** comment." }, agent);
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    const turns = page.getByRole("list", { name: "Conversation turns" });
    await expect(turns.locator("strong", { hasText: "formatted" })).toBeVisible();
    await expect(turns.locator("[data-markdown-fallback]")).toHaveCount(0);
  } finally {
    await alice.close();
  }
});
