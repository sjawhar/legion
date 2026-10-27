import { readFile } from "node:fs/promises";
import { fileURLToPath } from "node:url";

import {
  type Browser,
  type BrowserContext,
  expect,
  type Locator,
  type Page,
  test,
} from "@playwright/test";

import {
  baseUrl,
  createComment,
  createIssue,
  createNamedVersion,
  createProject,
  editArtifact,
  getIssueArtifactText,
  userHeaders,
} from "./api";
import { documentEditor, openSpec, paste, placeCaret } from "./editor";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

declare global {
  interface Window {
    /** Every address the page passed to `window.open` since it loaded (`signedIn`). */
    __e2eOpened?: string[];
    /** Set before a click; a full page load drops it, an in-app route change keeps it. */
    __e2eNavMarker?: boolean;
    /** What the `javascript:` link of the unsafe case would set, were it ever run. */
    __pwned?: unknown;
  }
}

// A linked image - a README badge, `[![build](badge.svg)](https://ci.example)` - keeps its link
// through the editor, its store, the documents API and the server, and every view opens it as it
// opens a text link (INFRA-190): the live editor on a Ctrl/Cmd-click (a plain click selects the
// picture), a past version on a plain click, a comment as the browser follows a link, and a
// `dispatch://` link in the app. A link no browser should follow (`javascript:`) draws no link.
//
// Every address on `ci.example` is answered here, by the context's route, so a picture loads and
// a popup lands on a page whatever the network: `.svg` is a badge, anything else a page.

const agentSession = {
  actor: { kind: "session" as const, id: "e2e-linked-images" },
  as: "agent" as const,
};

const build = "https://ci.example/build";
const badge = "https://ci.example/badge.svg";
const badgeSvg =
  '<svg xmlns="http://www.w3.org/2000/svg" width="96" height="24"><rect width="96" height="24" fill="#2e7d32"/></svg>';
const diagramPath = fileURLToPath(new URL("./fixtures/diagram.png", import.meta.url));

/** The linked badge every external case writes: a link with a title around a picture. */
const linkedBadge = `[![badge](${badge})](${build} "Build")`;

test.beforeEach(async () => {
  await resetDatabase();
});

/** Alice's context: `ci.example` answered, and every `window.open` the page makes recorded in
 *  `window.__e2eOpened` before it runs, so a check that a click opened nothing reads the record
 *  rather than waiting for a popup that may still come. */
async function signedIn(browser: Browser): Promise<BrowserContext> {
  const context = await asUser(browser, "alice");
  await context.route("https://ci.example/**", (route) => {
    const { pathname } = new URL(route.request().url());
    return pathname.endsWith(".svg")
      ? route.fulfill({ body: badgeSvg, contentType: "image/svg+xml" })
      : route.fulfill({
          body: `<!doctype html><title>ci.example${pathname}</title>`,
          contentType: "text/html",
        });
  });
  await context.addInitScript(() => {
    const opened: string[] = [];
    const open = window.open.bind(window);
    window.__e2eOpened = opened;
    window.open = (url?: string | URL, target?: string, features?: string) => {
      opened.push(String(url));
      return open(url, target, features);
    };
  });
  return context;
}

/** Every address the page has passed to `window.open` since it loaded. */
function openedAddresses(page: Page): Promise<string[]> {
  return page.evaluate(() => window.__e2eOpened ?? []);
}

/** Runs `gesture` and expects it to open one popup, at `url`, and leave the page where it was.
 *  The page's own record of `window.open` is read first, so a gesture that opens nothing fails
 *  naming what was opened rather than at the test's timeout. */
async function expectPopupAt(page: Page, url: string, gesture: () => Promise<void>): Promise<void> {
  const before = page.url();
  const [opened] = await Promise.all([
    page.waitForEvent("popup"),
    (async () => {
      await gesture();
      await expect.poll(() => openedAddresses(page), `the gesture opens ${url}`).toContain(url);
    })(),
  ]);
  await expect(opened).toHaveURL(url);
  await opened.close();
  expect(page.url()).toBe(before);
}

/** Waits for `picture` to have loaded real pixels, so a click lands on it. */
async function expectDrawn(picture: Locator): Promise<void> {
  await expect(picture).toBeVisible();
  await picture.scrollIntoViewIfNeeded();
  await expect
    .poll(() => picture.evaluate((image: HTMLImageElement) => image.naturalWidth))
    .toBeGreaterThan(0);
}

/** A past version of the issue's spec, in the read-only version view. */
async function openVersion(page: Page, issueKey: string, version: number): Promise<Locator> {
  await page.goto(`/issues/${issueKey}/artifact/spec?v=${version}`);
  const view = page.getByRole("region", { name: `Document version ${version}` });
  await expect(view.getByRole("textbox", { name: "Document editor" })).toHaveAttribute(
    "contenteditable",
    "false"
  );
  return view;
}

/** The issue's Conversation, where a comment's body is rendered. */
async function openConversation(page: Page, issueKey: string): Promise<Locator> {
  await page.goto(`/issues/${issueKey}/conversation`);
  return page.getByRole("list", { name: "Conversation turns" });
}

/** Uploads the diagram fixture to `issueKey` as alice, as the Artifacts tab's upload does. */
async function uploadDiagram(issueKey: string): Promise<void> {
  const form = new FormData();
  form.set("name", "diagram.png");
  form.set("file", new Blob([await readFile(diagramPath)], { type: "image/png" }), "diagram.png");
  const response = await fetch(
    new URL(`/api/v1/issues/${encodeURIComponent(issueKey)}/artifacts`, baseUrl),
    { body: form, headers: await userHeaders("alice"), method: "POST" }
  );
  expect(response.status, await response.text()).toBeLessThan(300);
}

// D1: the editor's reader and its store. A badge pasted as text keeps its link in the stored
// markdown and after a reload; the live editor opens it on Ctrl/Cmd-click only, a past version on
// a plain click.
test("a linked badge pasted into the live editor keeps its link through a reload and the store, and opens as a text link does", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "# Badges\n\nIntro text.",
    title: "A pasted badge",
  });
  const alice = await signedIn(browser);
  try {
    const page = await alice.newPage();
    await openSpec(page, issue.key, "Intro text.");
    await placeCaret(page, "after", "Intro text.");
    await page.keyboard.press("Enter");
    await paste(page, { html: "", text: linkedBadge });

    const editor = documentEditor(page);
    await expectDrawn(editor.getByRole("img", { name: "badge" }));
    // The paste reached the server: the picture is in the stored markdown, linked or not.
    const stored = async () => (await getIssueArtifactText(issue.key, "spec")).markdown;
    await expect.poll(stored).toContain(badge);

    await page.reload();
    await expect(editor).toContainText("Intro text.");
    const picture = editor.getByRole("img", { name: "badge" });
    await expectDrawn(picture);
    const linked = editor
      .locator(`a[href="${build}"][title="Build"]`)
      .getByRole("img", { name: "badge" });
    await expect(linked, "the reloaded picture sits in its link").toBeVisible();
    expect((await stored()).split("\n")).toContain(linkedBadge);

    // A plain click selects the picture and opens nothing; Ctrl/Cmd-click opens the link.
    const spec = page.url();
    await linked.click();
    expect(await openedAddresses(page)).toEqual([]);
    await expect(page).toHaveURL(spec);
    await expectPopupAt(page, build, () => linked.click({ modifiers: ["ControlOrMeta"] }));
    // The one address opened is the Ctrl/Cmd-click's: the plain click before it opened none.
    expect(await openedAddresses(page)).toEqual([build]);

    // A past version holding the pasted line opens the link on a plain click.
    const version = await createNamedVersion(issue.primary_artifact_id, "pasted badge");
    const view = await openVersion(page, issue.key, version.number);
    const pastLinked = view
      .locator(`a[href="${build}"][title="Build"]`)
      .getByRole("img", { name: "badge" });
    await expectDrawn(pastLinked);
    await expectPopupAt(page, build, () => pastLinked.click());
  } finally {
    await alice.close();
  }
});

// D2: the documents API and the server's reader. The same line written through the API keeps its
// link, and the live editor opens it on Ctrl/Cmd-click after a reload.
test("a linked badge written through the API opens its link on a Ctrl/Cmd-click in the live editor", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue(
    {
      project: "CORE",
      spec: `# Badges\n\n${linkedBadge}\n\nBelow the badge.`,
      title: "A written badge",
    },
    agentSession
  );
  const alice = await signedIn(browser);
  try {
    const page = await alice.newPage();
    await openSpec(page, issue.key, "Below the badge.");
    await page.reload();
    const editor = documentEditor(page);
    await expect(editor).toContainText("Below the badge.");
    await expectDrawn(editor.getByRole("img", { name: "badge" }));
    const linked = editor
      .locator(`a[href="${build}"][title="Build"]`)
      .getByRole("img", { name: "badge" });
    await expect(linked, "the written picture sits in its link").toBeVisible();
    await expectPopupAt(page, build, () => linked.click({ modifiers: ["ControlOrMeta"] }));
    expect((await getIssueArtifactText(issue.key, "spec")).markdown.split("\n")).toContain(
      linkedBadge
    );
  } finally {
    await alice.close();
  }
});

// D3: a Dispatch picture inside a link is drawn from its version's bytes route and opens the
// link, never the artifact's page.
test("a Dispatch picture inside a link opens the link on a Ctrl/Cmd-click, not the artifact's page", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "# Diagrams\n\nIntro.",
    title: "A linked diagram",
  });
  await uploadDiagram(issue.key);
  const line = `[![diagram](dispatch://${issue.key}/artifact/diagram-png@v1)](${build})`;
  await editArtifact(
    issue.primary_artifact_id,
    { ops: [{ after: "end", markdown: `${line}\n\nBelow the diagram.`, op: "insert" }] },
    agentSession
  );
  const alice = await signedIn(browser);
  try {
    const page = await alice.newPage();
    await openSpec(page, issue.key, "Below the diagram.");
    const editor = documentEditor(page);
    const picture = editor.getByRole("img", { name: "diagram" });
    await expectDrawn(picture);
    await expect(picture).toHaveAttribute(
      "src",
      `/api/v1/issues/${issue.key}/artifacts/diagram-png/versions/1`
    );
    const linked = editor.locator(`a[href="${build}"]`).getByRole("img", { name: "diagram" });
    await expect(linked, "the diagram sits in its link").toBeVisible();
    await expect(linked.locator("xpath=ancestor::a")).toHaveCount(1);
    await expectPopupAt(page, build, () => linked.click({ modifiers: ["ControlOrMeta"] }));
    expect(await openedAddresses(page)).toEqual([build]);
  } finally {
    await alice.close();
  }
});

// D4: a picture linked to another issue navigates there in the app, from the live editor and
// from a past version.
test("a picture linked to another issue navigates to it in the app from the live editor and a past version", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const other = await createIssue({ project: "CORE", title: "Linked target" });
  const issue = await createIssue(
    {
      project: "CORE",
      spec: `# Links\n\n[![x](${badge})](dispatch://${other.key})\n\nBelow the picture.`,
      title: "A picture linking an issue",
    },
    agentSession
  );
  const otherPath = new RegExp(`/issues/${other.key}(/|$)`);
  const anchor = `a[data-dispatch-href="dispatch://${other.key}"]`;
  const alice = await signedIn(browser);
  try {
    const page = await alice.newPage();
    await openSpec(page, issue.key, "Below the picture.");
    const editor = documentEditor(page);
    await expectDrawn(editor.getByRole("img", { name: "x" }));
    const live = editor.locator(anchor).getByRole("img", { name: "x" });
    await expect(live, "the picture sits in its link").toBeVisible();
    await page.evaluate(() => {
      window.__e2eNavMarker = true;
    });
    await live.click();
    await expect(page).toHaveURL(otherPath);
    expect(await page.evaluate(() => window.__e2eNavMarker)).toBe(true);

    const view = await openVersion(page, issue.key, 1);
    const past = view.locator(anchor).getByRole("img", { name: "x" });
    await expectDrawn(past);
    await page.evaluate(() => {
      window.__e2eNavMarker = true;
    });
    await past.click();
    await expect(page).toHaveURL(otherPath);
    expect(await page.evaluate(() => window.__e2eNavMarker)).toBe(true);
  } finally {
    await alice.close();
  }
});

// C1: a comment's linked badge is a link the browser follows, as a text link in a comment is.
test("a linked badge in a comment opens its link on a click", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "A badge in a comment" });
  await createComment(issue.key, { body: `The build:\n\n${linkedBadge}` }, agentSession);
  const alice = await signedIn(browser);
  try {
    const page = await alice.newPage();
    const turns = await openConversation(page, issue.key);
    const linked = turns.locator(`a[href="${build}"]`).getByRole("img", { name: "badge" });
    await expectDrawn(linked);
    await linked.click();
    await expect(page).toHaveURL(build);
  } finally {
    await alice.close();
  }
});

// C2: a comment's picture linked to another issue stays a picture in its link and navigates
// there.
test("a picture linked to another issue in a comment stays a picture and navigates to the issue", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const other = await createIssue({ project: "CORE", title: "Linked target" });
  const issue = await createIssue({ project: "CORE", title: "A picture in a comment" });
  await createComment(
    issue.key,
    { body: `See:\n\n[![x](${badge})](dispatch://${other.key})` },
    agentSession
  );
  const alice = await signedIn(browser);
  try {
    const page = await alice.newPage();
    const turns = await openConversation(page, issue.key);
    await expect(turns).toContainText("See:");
    const linked = turns.locator(`a[href="/issues/${other.key}"]`).getByRole("img", { name: "x" });
    await expect(linked, "the picture is still shown inside its link").toBeVisible();
    await expectDrawn(linked);
    await linked.click();
    await expect(page).toHaveURL(new RegExp(`/issues/${other.key}(/|$)`));
  } finally {
    await alice.close();
  }
});

// N1: a picture with no link sits in no link, wherever it is drawn.
test("an unlinked picture sits in no link in the live editor, a past version and a comment", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const plain = "![plain](https://ci.example/plain.svg)";
  const issue = await createIssue(
    {
      project: "CORE",
      spec: `# Pictures\n\n${plain}\n\nBelow the picture.`,
      title: "An unlinked picture",
    },
    agentSession
  );
  await createComment(issue.key, { body: `A picture:\n\n${plain}` }, agentSession);
  const alice = await signedIn(browser);
  try {
    const page = await alice.newPage();
    const expectNoLink = async (picture: Locator) => {
      await expectDrawn(picture);
      await expect(picture.locator("xpath=ancestor::a")).toHaveCount(0);
      const before = page.url();
      await picture.click();
      expect(await openedAddresses(page)).toEqual([]);
      await expect(page).toHaveURL(before);
    };

    await openSpec(page, issue.key, "Below the picture.");
    await expectNoLink(documentEditor(page).getByRole("img", { name: "plain" }));

    const view = await openVersion(page, issue.key, 1);
    await expectNoLink(view.getByRole("img", { name: "plain" }));

    const turns = await openConversation(page, issue.key);
    await expectNoLink(turns.getByRole("img", { name: "plain" }));
  } finally {
    await alice.close();
  }
});

// N2: a picture linked to a `javascript:` address draws no link and runs nothing on any click,
// whether the address is spelled plainly or with a character reference, an escape or mixed case:
// every view checks the scheme of the address the markdown names, decoded.
test("a picture linked to a javascript: address draws no link and runs nothing in the live editor, a past version and a comment", async ({
  browser,
}) => {
  // Five spellings, each clicked twice in three views from a fresh load: thirty page loads.
  test.setTimeout(180_000);
  await createProject({ key: "CORE", name: "Core" });
  const addresses = [
    ["x", "javascript:window.__pwned=1"],
    ["x-entity", "javascript&#58;window.__pwned=1"],
    ["x-escape", "javascript\\:window.__pwned=1"],
    ["x-case", "JaVaScRiPt:window.__pwned=1"],
    ["x-hex", "&#x6A;avascript:window.__pwned=1"],
  ] as const;
  const unsafe = addresses
    .map(([name, address]) => `[![${name}](${badge})](${address})`)
    .join("\n\n");
  const issue = await createIssue(
    {
      project: "CORE",
      spec: `# Pictures\n\n${unsafe}\n\nBelow the picture.`,
      title: "An unsafe link",
    },
    agentSession
  );
  await createComment(issue.key, { body: `A picture:\n\n${unsafe}` }, agentSession);
  const alice = await signedIn(browser);
  try {
    const page = await alice.newPage();
    // Each gesture starts from a fresh load of the view: a plain click on a comment opens its
    // card (a sheet on a phone), which would cover the picture for the second gesture.
    const expectInert = async (open: () => Promise<Locator>) => {
      for (const modifiers of [[], ["ControlOrMeta" as const]]) {
        const picture = await open();
        await expectDrawn(picture);
        await expect(picture.locator("xpath=ancestor::a[@href]")).toHaveCount(0);
        const before = page.url();
        await picture.click({ modifiers });
        expect(await openedAddresses(page)).toEqual([]);
        expect(await page.evaluate(() => window.__pwned)).toBeUndefined();
        await expect(page).toHaveURL(before);
      }
    };

    for (const [name] of addresses) {
      await expectInert(async () => {
        await openSpec(page, issue.key, "Below the picture.");
        return documentEditor(page).getByRole("img", { exact: true, name });
      });
      await expectInert(async () =>
        (await openVersion(page, issue.key, 1)).getByRole("img", { exact: true, name })
      );
      await expectInert(async () =>
        (await openConversation(page, issue.key)).getByRole("img", { exact: true, name })
      );
    }
  } finally {
    await alice.close();
  }
});
