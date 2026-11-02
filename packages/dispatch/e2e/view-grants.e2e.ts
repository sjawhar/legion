/**
 * The data grants a `view` block rests on (dispatch://INFRA-682, A10, R9): Dispatch gives a page
 * a data file only where a person granted that file to that page file, in the panel on the page
 * file's own view. A block that names a file no grant covers runs its page all the same, gives
 * it a `not-granted` entry and shows the reader one line with a link to that panel.
 *
 * Every row keeps the person who changes grants in one browser context and the reader in
 * another, as the two really are: a grant is a person's write, and what it changes is what
 * another reader's open block is given.
 *
 * The rows run in `chromium`, `firefox`, `webkit` and `webkit-iphone`: a grant reaching an open
 * block is the live socket, the refresh and a new frame, each engine's own.
 */
import { expect, type Locator, type Page, test } from "@playwright/test";

import {
  createArtifactGrant,
  createIssue,
  createProject,
  createProjectDocument,
  editArtifact,
  uploadIssueFile,
  uploadProjectFile,
} from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

/** The session whose edit of a block's `data` attribute one row makes. */
const session = {
  actor: { id: "e2e-view-grants", kind: "session" as const },
  as: "agent" as const,
};

/**
 * The page every row here shows: it reports the entries of the newest `dispatch-view:data`
 * message, each as its reference and its text or its error, and an id of its own load. A new
 * frame is a new load, so a changed id is the proof that the block started one, and an unchanged
 * id the proof that it did not.
 */
const REPORTING_PAGE = `<!doctype html>
<html lang="en"><body>
<p id="load"></p>
<pre id="entries">no data message</pre>
<script>
document.getElementById("load").textContent = Math.random().toString(36).slice(2);
window.addEventListener("message", (event) => {
  if (event.source !== window.parent) { return; }
  const message = event.data;
  if (message === null || typeof message !== "object" || message.type !== "dispatch-view:data") {
    return;
  }
  document.getElementById("entries").textContent = JSON.stringify(
    message.files.map((file) => [file.ref, file.error || file.text])
  );
});
parent.postMessage({ type: "dispatch-view:ready" }, "*");
</script>
</body></html>
`;

function viewDirective(block: { caption: string; data: string; page: string }): string {
  return `:::view{page="${block.page}" data="${block.data}"}\n${block.caption}\n:::\n`;
}

function viewBlock(page: Page, caption: string): Locator {
  return page.locator('section[data-proof-block-type="view"]').filter({ hasText: caption });
}

/** The entries the page was last given, as `[reference, text or error]` pairs. */
async function entriesOf(block: Locator): Promise<[string, string][]> {
  const text = await block.frameLocator("iframe").locator("#entries").innerText();
  return text === "no data message" ? [] : (JSON.parse(text) as [string, string][]);
}

/** The id of the load the frame is showing now, which changes with every new frame. */
function loadId(block: Locator): Promise<string> {
  return block.frameLocator("iframe").locator("#load").innerText();
}

/** The data one row grants, as text the page reports back. */
const attempts = (value: number) => JSON.stringify([{ label: "attempts", value }]);

test.beforeEach(async () => {
  await resetDatabase();
});

/** An issue holding the reporting page and one data file, with no grant between them. */
async function seedIssuePage(title: string): Promise<{
  dataRef: string;
  issue: string;
  pageId: string;
  pageRef: string;
}> {
  const issue = await createIssue({ project: "CORE", title });
  const pageFile = await uploadIssueFile(issue.key, {
    content: REPORTING_PAGE,
    name: "chart.html",
    type: "text/html; charset=utf-8",
  });
  await uploadIssueFile(issue.key, {
    content: attempts(7),
    name: "loop.json",
    type: "application/json",
  });
  return {
    dataRef: `dispatch://${issue.key}/artifact/loop-json`,
    issue: issue.key,
    pageId: pageFile.artifact.id,
    pageRef: `dispatch://${issue.key}/artifact/chart-html`,
  };
}

test("a view block whose page has no grant runs its page and names the file it may not read", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const fixture = await seedIssuePage("Ungranted");
  const document = await createProjectDocument("CORE", {
    content: viewDirective({
      caption: "Attempts, ungranted.",
      data: fixture.dataRef,
      page: fixture.pageRef,
    }),
    name: "Ungranted",
  });
  const reader = await asUser(browser, "bob");
  try {
    const page = await reader.newPage();
    await page.goto(`/projects/CORE/documents/${document.artifact.slug}`);
    const block = viewBlock(page, "Attempts, ungranted.");
    await expect(
      block.frameLocator("iframe").locator("#entries"),
      "A10: the page runs and is given an entry for the file"
    ).not.toHaveText("no data message", { timeout: 30_000 });
    expect(await entriesOf(block), "A10: the entry says the file is not granted").toEqual([
      [fixture.dataRef, "not-granted"],
    ]);
    await expect(
      block.getByText(`${fixture.dataRef} — not granted to this page`),
      "A10: and the block tells the reader so"
    ).toBeVisible();
    await expect(
      block.getByRole("link", { name: "Open the page file" }),
      "A10: with a link to the panel on the page file"
    ).toHaveAttribute("href", `/issues/${fixture.issue}/artifacts/chart-html`);
  } finally {
    await reader.close();
  }
});

test("a view block follows a grant a person adds and removes in an issue page file's panel", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const fixture = await seedIssuePage("Panel");
  const document = await createProjectDocument("CORE", {
    content: viewDirective({
      caption: "Attempts, as granted.",
      data: fixture.dataRef,
      page: fixture.pageRef,
    }),
    name: "Panel",
  });
  const person = await asUser(browser, "alice");
  const reader = await asUser(browser, "bob");
  try {
    const watching = await reader.newPage();
    await watching.goto(`/projects/CORE/documents/${document.artifact.slug}`);
    const block = viewBlock(watching, "Attempts, as granted.");
    await expect(block.frameLocator("iframe").locator("#entries")).not.toHaveText(
      "no data message",
      { timeout: 30_000 }
    );
    const before = await loadId(block);

    const panel = await person.newPage();
    await panel.goto(`/issues/${fixture.issue}/artifacts/chart-html`);
    const grants = panel.getByRole("region", { name: "Data this page may read" });
    await expect(grants.getByText("No data file is granted to this page")).toBeVisible();
    await grants.getByLabel("Data file").fill(fixture.dataRef);
    await grants.getByRole("button", { name: "Grant" }).click();
    await expect(grants.getByText("loop.json")).toBeVisible();
    const granted = Date.now();

    await expect
      .poll(() => entriesOf(block), {
        message: "A10: within 5 seconds the reader's page is given the file's text",
        timeout: 5_000,
      })
      .toEqual([[fixture.dataRef, attempts(7)]]);
    expect(Date.now() - granted, "A10: within 5 seconds of the grant").toBeLessThanOrEqual(5_000);
    const afterGrant = await loadId(block);
    expect(afterGrant, "A10: the block started a new frame for the new grant").not.toBe(before);
    await expect(
      block.getByText(`${fixture.dataRef} — not granted to this page`),
      "A10: and the line is gone"
    ).toHaveCount(0);

    await grants.getByRole("button", { name: `Remove the grant for loop.json` }).click();
    await expect(grants.getByText("No data file is granted to this page")).toBeVisible();
    const removed = Date.now();
    await expect
      .poll(() => entriesOf(block), {
        message: "A10: within 5 seconds of the removal every entry is not-granted again",
        timeout: 5_000,
      })
      .toEqual([[fixture.dataRef, "not-granted"]]);
    expect(Date.now() - removed, "A10: within 5 seconds of the removal").toBeLessThanOrEqual(5_000);
    expect(await loadId(block), "A10: a frame of its own, so the page lost what it held").not.toBe(
      afterGrant
    );
  } finally {
    await person.close();
    await reader.close();
  }
});

test("a view block follows a grant a person adds in a project page file's panel", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const pageFile = await uploadProjectFile("CORE", {
    content: REPORTING_PAGE,
    name: "shared.html",
    type: "text/html; charset=utf-8",
  });
  await uploadProjectFile("CORE", {
    content: attempts(4),
    name: "shared.json",
    type: "application/json",
  });
  const dataRef = "dispatch://CORE/artifact/shared-json";
  const pageRef = "dispatch://CORE/artifact/shared-html";
  const document = await createProjectDocument("CORE", {
    content: viewDirective({ caption: "Attempts, project-wide.", data: dataRef, page: pageRef }),
    name: "Project panel",
  });
  const person = await asUser(browser, "alice");
  const reader = await asUser(browser, "bob");
  try {
    const watching = await reader.newPage();
    await watching.goto(`/projects/CORE/documents/${document.artifact.slug}`);
    const block = viewBlock(watching, "Attempts, project-wide.");
    await expect(block.frameLocator("iframe").locator("#entries")).not.toHaveText(
      "no data message",
      { timeout: 30_000 }
    );
    expect(await entriesOf(block)).toEqual([[dataRef, "not-granted"]]);
    await expect(
      block.getByRole("link", { name: "Open the page file" }),
      "A10: a project page file's line links to its own view"
    ).toHaveAttribute("href", "/projects/CORE/documents/shared-html");

    const panel = await person.newPage();
    await panel.goto(`/projects/CORE/documents/${pageFile.artifact.slug}`);
    const grants = panel.getByRole("region", { name: "Data this page may read" });
    await grants.getByLabel("Data file").fill(dataRef);
    await grants.getByRole("button", { name: "Grant" }).click();
    await expect(grants.getByText("shared.json")).toBeVisible();
    const granted = Date.now();

    await expect
      .poll(() => entriesOf(block), {
        message: "A10: the project page file's grant reaches the reader within 5 seconds",
        timeout: 5_000,
      })
      .toEqual([[dataRef, attempts(4)]]);
    expect(Date.now() - granted, "A10: within 5 seconds").toBeLessThanOrEqual(5_000);
  } finally {
    await person.close();
    await reader.close();
  }
});

test("a view block naming two files under one grant gets the one text and one not-granted", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const fixture = await seedIssuePage("Two files");
  await uploadIssueFile(fixture.issue, {
    content: attempts(2),
    name: "other.json",
    type: "application/json",
  });
  const otherRef = `dispatch://${fixture.issue}/artifact/other-json`;
  await createArtifactGrant(fixture.pageId, fixture.dataRef);
  const document = await createProjectDocument("CORE", {
    content: viewDirective({
      caption: "One granted, one not.",
      data: `${fixture.dataRef} ${otherRef}`,
      page: fixture.pageRef,
    }),
    name: "Two files",
  });
  const reader = await asUser(browser, "bob");
  try {
    const page = await reader.newPage();
    await page.goto(`/projects/CORE/documents/${document.artifact.slug}`);
    const block = viewBlock(page, "One granted, one not.");
    await expect
      .poll(() => entriesOf(block), {
        message: "A10: the granted file's text and the other's refusal, in the order named",
        timeout: 30_000,
      })
      .toEqual([
        [fixture.dataRef, attempts(7)],
        [otherRef, "not-granted"],
      ]);
    await expect(
      block.getByText(`${otherRef} — not granted to this page`),
      "A10: one line, for the file that is not granted"
    ).toBeVisible();
  } finally {
    await reader.close();
  }
});

test("a view block under a grant pinned to version 1 gets version 1, and @v3 is not granted", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const fixture = await seedIssuePage("Pinned");
  for (const value of [8, 9]) {
    await uploadIssueFile(fixture.issue, {
      content: attempts(value),
      name: "loop.json",
      type: "application/json",
    });
  }
  await createArtifactGrant(fixture.pageId, `${fixture.dataRef}@v1`);
  const document = await createProjectDocument("CORE", {
    content: [
      viewDirective({
        caption: "The newest version it may read.",
        data: fixture.dataRef,
        page: fixture.pageRef,
      }),
      viewDirective({
        caption: "The version it may not read.",
        data: `${fixture.dataRef}@v3`,
        page: fixture.pageRef,
      }),
    ].join("\n"),
    name: "Pinned",
  });
  const reader = await asUser(browser, "bob");
  try {
    const page = await reader.newPage();
    await page.goto(`/projects/CORE/documents/${document.artifact.slug}`);
    await expect
      .poll(() => entriesOf(viewBlock(page, "The newest version it may read.")), {
        message: "A10: a grant pinned to version 1 gives version 1, later versions and all",
        timeout: 30_000,
      })
      .toEqual([[fixture.dataRef, attempts(7)]]);
    await expect
      .poll(() => entriesOf(viewBlock(page, "The version it may not read.")), {
        message: "A10: and a block that names @v3 under that grant is given nothing",
        timeout: 30_000,
      })
      .toEqual([[`${fixture.dataRef}@v3`, "not-granted"]]);
  } finally {
    await reader.close();
  }
});

test("a view block whose grant pins a version over 16 MiB sends no data and says so", async ({
  browser,
}) => {
  // The upload of 17 MiB, and the block's own read of the version's size, take longer than the
  // default a row of this file is given.
  test.setTimeout(60_000);
  await createProject({ key: "CORE", name: "Core" });
  const fixture = await seedIssuePage("Pinned over the limit");
  // Version 1 is over the limit on its own and version 2 is a few bytes. The reference pins
  // nothing, so the grant's pin alone decides that version 1 is what the page would be given,
  // and version 1's bytes are what the block weighs (R6).
  await uploadIssueFile(fixture.issue, {
    content: "x".repeat(17 * 1024 * 1024),
    name: "huge.json",
    type: "application/json",
  });
  await uploadIssueFile(fixture.issue, {
    content: attempts(3),
    name: "huge.json",
    type: "application/json",
  });
  const hugeRef = `dispatch://${fixture.issue}/artifact/huge-json`;
  await createArtifactGrant(fixture.pageId, `${hugeRef}@v1`);
  const document = await createProjectDocument("CORE", {
    content: viewDirective({
      caption: "A huge version, pinned by the grant.",
      data: hugeRef,
      page: fixture.pageRef,
    }),
    name: "Pinned over the limit",
  });
  const reader = await asUser(browser, "bob");
  try {
    const page = await reader.newPage();
    await page.goto(`/projects/CORE/documents/${document.artifact.slug}`);
    const block = viewBlock(page, "A huge version, pinned by the grant.");
    await expect(
      block.getByText(
        "data — the data files together are larger than 16 MiB, so Dispatch sends no data"
      ),
      "R6: the grant's pin is the version the block weighs, so version 1 is over the limit"
    ).toBeVisible();
    await expect(
      block.frameLocator("iframe").locator("#entries"),
      "R6: and the page is given no data at all"
    ).toHaveText("no data message");
  } finally {
    await reader.close();
  }
});

test("a view block under a grant without a version takes a new data version in the same frame", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const fixture = await seedIssuePage("Same frame");
  await createArtifactGrant(fixture.pageId, fixture.dataRef);
  const document = await createProjectDocument("CORE", {
    content: viewDirective({
      caption: "Attempts, live.",
      data: fixture.dataRef,
      page: fixture.pageRef,
    }),
    name: "Same frame",
  });
  const reader = await asUser(browser, "bob");
  try {
    const page = await reader.newPage();
    await page.goto(`/projects/CORE/documents/${document.artifact.slug}`);
    const block = viewBlock(page, "Attempts, live.");
    await expect
      .poll(() => entriesOf(block), { timeout: 30_000 })
      .toEqual([[fixture.dataRef, attempts(7)]]);
    const before = await loadId(block);

    await uploadIssueFile(fixture.issue, {
      content: attempts(11),
      name: "loop.json",
      type: "application/json",
    });
    await expect
      .poll(() => entriesOf(block), {
        message: "A10: the new version reaches the page",
        timeout: 10_000,
      })
      .toEqual([[fixture.dataRef, attempts(11)]]);
    expect(
      await loadId(block),
      "A10: a new data version changes no grant, so the frame is the same one"
    ).toBe(before);
  } finally {
    await reader.close();
  }
});

test("a view block keeps its grants through a new version of its page file", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const fixture = await seedIssuePage("New page version");
  await createArtifactGrant(fixture.pageId, fixture.dataRef);
  const document = await createProjectDocument("CORE", {
    content: viewDirective({
      caption: "Attempts, through a new page.",
      data: fixture.dataRef,
      page: fixture.pageRef,
    }),
    name: "New page version",
  });
  const reader = await asUser(browser, "bob");
  try {
    const page = await reader.newPage();
    await page.goto(`/projects/CORE/documents/${document.artifact.slug}`);
    const block = viewBlock(page, "Attempts, through a new page.");
    await expect
      .poll(() => entriesOf(block), { timeout: 30_000 })
      .toEqual([[fixture.dataRef, attempts(7)]]);

    await uploadIssueFile(fixture.issue, {
      content: REPORTING_PAGE.replace('<p id="load">', '<p id="second">second</p><p id="load">'),
      name: "chart.html",
      type: "text/html; charset=utf-8",
    });
    await expect(
      block.frameLocator("iframe").locator("#second"),
      "A10: the new version of the page file is the one running"
    ).toHaveText("second", { timeout: 10_000 });
    await expect
      .poll(() => entriesOf(block), {
        message: "A10: a new page version keeps the grants, so the data still arrives",
        timeout: 10_000,
      })
      .toEqual([[fixture.dataRef, attempts(7)]]);
  } finally {
    await reader.close();
  }
});

test("a view block whose data attribute an agent edits to an ungranted file says so", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const fixture = await seedIssuePage("Edited data");
  await uploadIssueFile(fixture.issue, {
    content: attempts(2),
    name: "other.json",
    type: "application/json",
  });
  const otherRef = `dispatch://${fixture.issue}/artifact/other-json`;
  await createArtifactGrant(fixture.pageId, fixture.dataRef);
  const document = await createProjectDocument("CORE", {
    content: `:::view{#chart page="${fixture.pageRef}" data="${fixture.dataRef}"}\nAttempts, until the block is edited.\n:::\n`,
    name: "Edited data",
  });
  const reader = await asUser(browser, "bob");
  try {
    const page = await reader.newPage();
    await page.goto(`/projects/CORE/documents/${document.artifact.slug}`);
    const block = viewBlock(page, "Attempts, until the block is edited.");
    await expect
      .poll(() => entriesOf(block), { timeout: 30_000 })
      .toEqual([[fixture.dataRef, attempts(7)]]);

    // A block's attributes are not its text, so the edit that writes them is `retype`, with the
    // block's own id, rather than a quote-anchored replace.
    await editArtifact(
      document.artifact.id,
      {
        ops: [
          {
            attributes: { data: otherRef, page: fixture.pageRef },
            block: "chart",
            op: "retype",
            type: "view",
          },
        ],
      },
      session
    );
    await expect
      .poll(() => entriesOf(block), {
        message: "A10: the file the edit named has no grant, so the page is given nothing for it",
        timeout: 15_000,
      })
      .toEqual([[otherRef, "not-granted"]]);
    await expect(
      block.getByText(`${otherRef} — not granted to this page`),
      "A10: and the reader is told which file"
    ).toBeVisible();
  } finally {
    await reader.close();
  }
});
