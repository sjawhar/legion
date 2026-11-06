import { expect, type Page, test } from "@playwright/test";

import {
  createIssue,
  createIssueArtifact,
  createProject,
  editArtifact,
  getArtifact,
  getArtifactText,
} from "./api";
import { documentEditor, openSpec } from "./editor";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

// The clicks a link inside a document answers, which this spec is its own file for so that
// they run in Firefox too (e2e/playwright.config.ts): each engine decides them its own way.
// A picture line the editor cannot draw gives way to a link to the artifact's page
// (features/doc/picture-view.ts). The link sits far enough down a real spec that the document's
// start is off screen, and a plain click on it opens what it names. The row carries `@firefox`,
// the tag the `firefox` project selects by (e2e/playwright.config.ts): Firefox does not focus a
// link on mousedown, the editing host takes the focus instead, and the editor then restores its
// own selection, which a reader who has not clicked into the text yet has at the document's
// start. Then a document's own link mark, whose address the serializer emptied, which the page
// itself must open in a new tab on the middle click and the modified click the browser cannot
// answer for it, and whose row carries that same tag.

const agentSession = {
  actor: { kind: "session" as const, id: "e2e-picture-link-click" },
  as: "agent" as const,
};

test.beforeEach(async () => {
  await resetDatabase();
});

// @firefox: the engine's focus rule for a click on a link inside the editor.
test("a fallback link far down the document opens the artifact's page on a plain click @firefox", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  // Forty paragraphs: the document's start is off screen at every project's viewport, whatever a
  // paragraph's height.
  const filler = Array.from({ length: 40 }, (_, i) => `Filler paragraph ${i + 1}.`).join("\n\n");
  const issue = await createIssue({
    project: "CORE",
    spec: `# Pictures\n\n${filler}`,
    title: "A link far down the spec",
  });
  // A markdown document named without a version is a link at once, with no picture load to wait
  // for.
  await createIssueArtifact(issue.key, { content: "# Notes\n", name: "notes.md" }, agentSession);
  const source = `dispatch://${issue.key}/artifact/notes-md`;
  await editArtifact(
    issue.primary_artifact_id,
    {
      ops: [{ after: "end", markdown: `![notes.md](${source})\n\nBelow the link.`, op: "insert" }],
    },
    agentSession
  );
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await openSpec(page, issue.key, "Below the link.");
    const picture = documentEditor(page).locator(`[data-dispatch-doc-picture="${source}"]`);
    await picture.scrollIntoViewIfNeeded();
    const link = picture.getByRole("link", { name: `${issue.key} notes-md` });
    await expect(link).toBeInViewport();
    // The reader has not clicked into the text: the first thing they do is click the link.
    await link.click();
    await expect(page).toHaveURL(`/issues/${issue.key}/artifacts/notes-md`);
  } finally {
    await alice.close();
  }
});

// INFRA-760 k7: the same link in a past version. There the editor library's own link plugin
// would take the click and open the href its serializer emptied, reloading the version the
// reader is on; the article's shared handler (`features/doc/reference-click.ts`) opens the
// artifact inside the app instead. A middle click is nobody's but the browser's, and the link
// carries the real path for it.

/** Writes markdown at the end of the issue's spec and answers the version number the server
 *  gave that write, so a row can read what it wrote as a past version. */
async function appendToSpec(artifactId: string, markdown: string): Promise<number> {
  await editArtifact(artifactId, { ops: [{ after: "end", markdown, op: "insert" }] }, agentSession);
  const newest = async () =>
    Math.max(...(await getArtifact(artifactId)).versions.map(({ number }) => number));
  await expect.poll(newest).toBeGreaterThan(1);
  return newest();
}

// @firefox: the engine's own rule for the tab a middle click or a modified click may open.
test("a fallback link in a past version opens the artifact inside the app, and a middle click opens it in a new tab @firefox", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "# Pictures\n\nIntro.",
    title: "A link in a past version",
  });
  await createIssueArtifact(issue.key, { content: "# Notes\n", name: "notes.md" }, agentSession);
  const source = `dispatch://${issue.key}/artifact/notes-md`;
  const version = await appendToSpec(
    issue.primary_artifact_id,
    `![notes.md](${source})\n\nBelow the link.`
  );
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/artifact/spec?v=${version}`);
    const shown = page.getByRole("region", { name: `Document version ${version}` });
    const picture = shown.locator(`[data-dispatch-doc-picture="${source}"]`);
    await picture.scrollIntoViewIfNeeded();
    const link = picture.getByRole("link", { name: `${issue.key} notes-md` });
    await expect(link).toHaveAttribute("href", `/issues/${issue.key}/artifacts/notes-md`);

    // A middle click is the browser's own: it opens the link's real address in a new tab and
    // leaves the reader on the version.
    const [opened] = await Promise.all([
      alice.waitForEvent("page"),
      link.click({ button: "middle" }),
    ]);
    await expect(opened).toHaveURL(new RegExp(`/issues/${issue.key}/artifacts/notes-md$`));
    await opened.close();
    await expect(page).toHaveURL(`/issues/${issue.key}/artifact/spec?v=${version}`);

    // A plain click opens the artifact in the app: no page load, and the artifact's own page.
    let loads = 0;
    page.on("load", () => {
      loads += 1;
    });
    await link.click();
    await expect(page).toHaveURL(`/issues/${issue.key}/artifacts/notes-md`);
    await expect(page.getByTestId("artifact-header")).toContainText("notes.md");
    expect(loads, "the click reloaded the page instead of navigating in the app").toBe(0);
  } finally {
    await alice.close();
  }
});

// INFRA-760 k7: a document's own link mark, which is not the picture's fallback link above and
// carries no address at all - `@legion/proof-editor`'s serializer sanitizes a `dispatch://`
// href to `""` and keeps the target in `data-dispatch-href` (`dispatch-links.ts`). So the
// clicks the browser would answer itself on any other link, a middle click and a Ctrl click,
// would open the page the reader is on; the article's shared handler opens the reference's own
// path in the new tab instead (`features/doc/reference-click.ts`). Both engines, because each
// decides for itself whether a page may open a tab from a click of the middle button.

/** The address of the tab `open` opened beside `page`, which is then closed. */
async function tabOpenedBy(page: Page, open: Promise<unknown>): Promise<string> {
  const [opened] = await Promise.all([page.context().waitForEvent("page"), open]);
  await opened.waitForLoadState("domcontentloaded");
  const address = new URL(opened.url()).pathname;
  await opened.close();
  return address;
}

// @firefox: the engine's own rule for the tab a middle click or a modified click may open.
test("a middle click and a Ctrl click on a document's own link mark open its target in a new tab @firefox", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const target = await createIssue({ project: "CORE", title: "The issue linked to" });
  const issue = await createIssue({
    project: "CORE",
    spec: `# Links\n\nSee [the other issue](dispatch://${target.key}) for context.`,
    title: "A link mark opened in a new tab",
  });
  // A second version, so the live editor and a past version both hold that same link.
  const version = await appendToSpec(issue.primary_artifact_id, "Below the link.");
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await openSpec(page, issue.key, "Below the link.");
    const live = documentEditor(page).getByRole("link", { name: "the other issue" });
    // The mark carries no address of its own: following it would open this very page.
    await expect(live).toHaveAttribute("href", "");

    const inLiveEditor = `/issues/${issue.key}/spec`;
    expect(await tabOpenedBy(page, live.click({ button: "middle" }))).toBe(`/issues/${target.key}`);
    await expect(page).toHaveURL(inLiveEditor);
    expect(await tabOpenedBy(page, live.click({ modifiers: ["ControlOrMeta"] }))).toBe(
      `/issues/${target.key}`
    );
    await expect(page).toHaveURL(inLiveEditor);
    // Neither click wrote the opened path into the document: the editor's own DOM is where the
    // emptied href lives, and a click that rewrote it would reach the author's markdown.
    expect((await getArtifactText(issue.primary_artifact_id)).markdown).toContain(
      `[the other issue](dispatch://${target.key})`
    );

    const atVersion = `/issues/${issue.key}/artifact/spec?v=${version}`;
    await page.goto(atVersion);
    const shown = page.getByRole("region", { name: `Document version ${version}` });
    const past = shown.getByRole("link", { name: "the other issue" });
    await expect(past).toHaveAttribute("href", "");
    expect(await tabOpenedBy(page, past.click({ button: "middle" }))).toBe(`/issues/${target.key}`);
    await expect(page).toHaveURL(atVersion);
    expect(await tabOpenedBy(page, past.click({ modifiers: ["ControlOrMeta"] }))).toBe(
      `/issues/${target.key}`
    );
    await expect(page).toHaveURL(atVersion);
  } finally {
    await alice.close();
  }
});
