import { expect, type Locator, type Page, test } from "@playwright/test";

import {
  createComment,
  createIssue,
  createIssueArtifact,
  createProject,
  editArtifact,
  editComment,
  getArtifact,
  getArtifactText,
  getIssue,
} from "./api";
import { blockId } from "./editor";
import { resetDatabase } from "./seed";
import { signIn } from "./users";

// Every row seeds through the real API and reads the real server: a block's id comes from
// `GET /api/v1/artifacts/<id>/blocks`, never from a guess. Its helpers live in this file, in
// `editor.ts` (`blockId`) and in `api.ts` (`getArtifactBlocks`, which `blockId` reads), so a run at
// the base, where none of this exists, copies those three files.

test.beforeEach(async ({ context }) => {
  await resetDatabase();
  await signIn(context, "alice");
  await createProject({ key: "CORE", name: "Core" });
});

async function latestVersion(artifactId: string): Promise<number> {
  const artifact = await getArtifact(artifactId);
  return Math.max(...artifact.versions.map((version) => version.number));
}

/** Opens the hover card of `link` from the keyboard, which every project can do (the phone
 *  project has no hover), and answers the card. */
async function openCard(page: Page, link: Locator): Promise<Locator> {
  await page.keyboard.press("Escape");
  await page.mouse.move(0, 0);
  await page.keyboard.press("Tab");
  await link.focus();
  const card = page.getByRole("tooltip");
  await expect(card).toBeVisible();
  return card;
}

test("a block of another document citing a block shows as that document's row, linked to the citing block", async ({
  page,
}) => {
  const issue = await createIssue({ project: "CORE", title: "Block references" });
  const plan = await createIssueArtifact(issue.key, {
    content: "Intro.\n\nShip the block graph first.\n",
    name: "plan.md",
  });
  expect(plan.artifact.slug).toBe("plan-md");
  const cited = await blockId(plan.artifact.id, "Ship the block graph first.");
  const paragraph = `Decided: dispatch://${issue.key}/artifact/plan-md#b-${cited} holds.`;
  const decisions = await createIssueArtifact(issue.key, {
    content: `Context.\n\n${paragraph}\n`,
    name: "decisions.md",
  });
  expect(decisions.artifact.slug).toBe("decisions-md");
  const citing = await blockId(decisions.artifact.id, paragraph);

  await page.goto(`/issues/${issue.key}/artifacts/plan-md`);
  const references = page.getByRole("region", { name: "Referenced by" });
  // One row: the citing paragraph, shown as the document holding it and linked to that block.
  await expect(references.getByRole("link", { name: `Artifact · ${issue.key}` })).toHaveAttribute(
    "href",
    `/issues/${issue.key}/artifacts/decisions-md#b-${citing}`
  );
  await expect(references.getByText(paragraph, { exact: true })).toBeVisible();
  await expect(references.getByRole("heading")).toHaveText("Referenced by (1)");
});

test("two paragraphs of one document version citing an issue are two rows under that document, each linked to its block", async ({
  page,
}) => {
  const target = await createIssue({ project: "CORE", title: "Cited twice" });
  const first = `First, dispatch://${target.key} decides it.`;
  const second = `Then dispatch://${target.key} ships it.`;
  const source = await createIssue({
    project: "CORE",
    spec: `${first}\n\n${second}\n`,
    title: "Citing source",
  });
  const firstId = await blockId(source.primary_artifact_id, first);
  const secondId = await blockId(source.primary_artifact_id, second);
  expect(await latestVersion(source.primary_artifact_id)).toBe(1);

  await page.goto(`/issues/${target.key}`);
  // The target's own spec is attached to it, and each citing paragraph counts once.
  const toggle = page.getByRole("button", { name: /^Referenced by \(3\)$/ });
  await expect(toggle).toBeVisible();
  await toggle.click();
  const references = page.getByRole("region", { name: "Referenced by" });
  await expect(references.getByRole("link", { name: `Artifact · ${source.key}` })).toHaveAttribute(
    "href",
    `/issues/${source.key}/spec`
  );
  for (const [text, other, id] of [
    [first, second, firstId],
    [second, first, secondId],
  ] as const) {
    const row = references
      .getByRole("listitem")
      .filter({ hasText: text })
      .filter({ hasNotText: other });
    await expect(row).toHaveCount(1);
    await expect(row.getByText(text, { exact: true })).toBeVisible();
    await expect(row.locator(`a[href="/issues/${source.key}/spec#b-${id}"]`)).toHaveCount(1);
  }
});

test("a comment citing two blocks of the shown document is two rows, each linked to the block it cites", async ({
  page,
}) => {
  const issue = await createIssue({ project: "CORE", title: "Two blocks cited" });
  const callout = (id: string, text: string) =>
    `:::callout{#${id} kind="note" title=""}\n${text}\n:::`;
  const plan = await createIssueArtifact(issue.key, {
    content: `${callout("ship_it", "Ship the block graph.")}\n\n${callout("hold_line", "Hold the line.")}\n`,
    name: "plan.md",
  });
  expect(plan.artifact.slug).toBe("plan-md");
  // `encodeBlockId` writes `_` as %5F, the form the comment cites and each row's link carries.
  const ship = "#b-ship%5Fit";
  const hold = "#b-hold%5Fline";
  const doc = `dispatch://${issue.key}/artifact/plan-md`;
  await createComment(issue.key, { body: `Ship per ${doc}${ship}, hold per ${doc}${hold}.` });

  await page.goto(`/issues/${issue.key}/artifacts/plan-md`);
  const references = page.getByRole("region", { name: "Referenced by" });
  await expect(references.getByRole("heading")).toHaveText("Referenced by (2)");
  // One source, the comment, holding one row for each block of this document it cites.
  const sources = references.getByRole("list").first().locator(":scope > li");
  await expect(sources).toHaveCount(1);
  await expect(sources.getByRole("link", { name: `Comment · ${issue.key}` })).toBeVisible();
  const rows = sources.locator(":scope > ul > li");
  await expect(rows).toHaveCount(2);
  for (const [id, other, fragment] of [
    ["ship_it", "hold_line", ship],
    ["hold_line", "ship_it", hold],
  ] as const) {
    const row = rows
      .filter({ has: page.getByRole("link", { name: `block ${id}`, exact: true }) })
      .filter({ hasNot: page.getByRole("link", { name: `block ${other}`, exact: true }) });
    await expect(row).toHaveCount(1);
    await expect(row.getByRole("link", { name: `block ${id}`, exact: true })).toHaveAttribute(
      "href",
      `/issues/${issue.key}/artifacts/plan-md${fragment}`
    );
  }
});

test("a pinned link keeps the text of the version it pins, says how the block stands since, and opens that version", async ({
  page,
}) => {
  const issue = await createIssue({
    project: "CORE",
    spec: "Intro paragraph.\n\nP ships on Friday.\n",
    title: "Pinned block",
  });
  const specId = issue.primary_artifact_id;
  const paragraph = await blockId(specId, "P ships on Friday.");
  const pinned = await latestVersion(specId);
  const slug = (await getIssue(issue.key)).artifacts.find(
    (artifact) => artifact.id === specId
  )?.slug;
  expect(slug).toBeDefined();
  const block = `dispatch://${issue.key}/spec#b-${paragraph}`;
  await createComment(issue.key, { body: `Now ${block} and then ${block}@v${pinned}.` });

  await editArtifact(specId, {
    ops: [{ find: "Friday", op: "replace", with: "Monday" }],
    summary: "Move the day",
  });
  await expect.poll(() => latestVersion(specId)).toBe(pinned + 1);

  await page.goto(`/issues/${issue.key}/conversation`);
  const turns = page.getByRole("list", { name: "Conversation turns" });
  await expect(turns.getByRole("link", { name: "P ships on Monday." })).toHaveAttribute(
    "href",
    `/issues/${issue.key}/spec#b-${paragraph}`
  );
  const old = turns.getByRole("link", { name: "P ships on Friday." });
  await expect(old).toBeVisible();
  const card = await openCard(page, old);
  await expect(card).toContainText(`Version ${pinned}`);
  await expect(card).toContainText(`Changed in version ${pinned + 1}`);
  await expect(card).toContainText("P ships on Friday.");
  await page.keyboard.press("Escape");
  await expect(old).toHaveAttribute("href", `/issues/${issue.key}/artifacts/${slug}?v=${pinned}`);
  await old.click();
  await expect(page).toHaveURL(`/issues/${issue.key}/artifacts/${slug}?v=${pinned}`);

  await editArtifact(specId, {
    ops: [{ block: paragraph, op: "delete" }],
    summary: "Drop the day",
  });
  await expect.poll(() => latestVersion(specId)).toBe(pinned + 2);
  await page.goto(`/issues/${issue.key}/conversation`);
  const removed = page
    .getByRole("list", { name: "Conversation turns" })
    .getByRole("link", { name: "P ships on Friday." });
  await expect(removed).toBeVisible();
  const removedCard = await openCard(page, removed);
  await expect(removedCard).toContainText(`Removed in version ${pinned + 2}`);
  await expect(removedCard).toContainText("P ships on Friday.");
});

test("a block reference a person made keeps resolving after an upload that leaves its block alone", async ({
  page,
}) => {
  const issue = await createIssue({
    project: "CORE",
    spec:
      "The retry policy is this issue's subject.\n\n" +
      "The retry cap holds at three attempts.\n\n" +
      "The rollout waits on review.\n",
    title: "Uploaded spec",
  });
  const specId = issue.primary_artifact_id;
  const cited = "The retry cap holds at three attempts.";
  const citedId = await blockId(specId, cited);
  const spec = (await getIssue(issue.key)).artifacts.find((artifact) => artifact.id === specId);
  if (spec === undefined) {
    throw new Error(`${issue.key} has no primary document`);
  }
  await createComment(issue.key, {
    body: `We cap it per dispatch://${issue.key}/spec#b-${citedId}.`,
  });
  const before = await latestVersion(specId);

  // A whole document posted under the document's own name is a new version of it through
  // `docs.ReplaceText`, the path `dispatch artifact` takes. This one holds the document's own
  // text with one other paragraph changed, so the cited paragraph is left exactly as it was.
  const { markdown } = await getArtifactText(specId);
  const uploaded = markdown.replace(
    "The rollout waits on review.",
    "The rollout waits on the staging run."
  );
  expect(uploaded).not.toBe(markdown);
  expect(uploaded).toContain(cited);
  await createIssueArtifact(issue.key, { content: uploaded, name: spec.name });
  await expect.poll(() => latestVersion(specId)).toBe(before + 1);

  // The reference still names a block of the live document, so it reads that paragraph's text and
  // opens the document at it.
  await page.goto(`/issues/${issue.key}/conversation`);
  const link = page
    .getByRole("list", { name: "Conversation turns" })
    .getByRole("link", { name: cited });
  await expect(link).toHaveAttribute("href", `/issues/${issue.key}/spec#b-${citedId}`);
  // The id the comment names is still the id `GET /api/v1/artifacts/{id}/blocks` gives that
  // paragraph, not one the upload re-minted.
  expect(await blockId(specId, cited)).toBe(citedId);

  await link.click();
  await expect(page).toHaveURL(`/issues/${issue.key}/spec#b-${citedId}`);
  await expect(page.getByRole("textbox", { name: "Document editor" })).toContainText(cited);
});

// A link to a block of a comment reads the block's graph read, which is fetched again only by an
// event that can change it (`blockQuery`, staleTime Infinity). The comment's own edit is one: the
// open card, and the link's own title, show the reworded paragraph with no reload.
test("a link to a block of a comment shows the block's new text once its author edits it, with no reload", async ({
  page,
}) => {
  const issue = await createIssue({ project: "CORE", title: "Comment block text" });
  const cited = await createComment(issue.key, { body: "Alpha first.\n\nBeta as first written." });
  await createComment(issue.key, {
    body: `The detail is in dispatch://${issue.key}/comment/${cited.id}#b-b2 today.`,
  });

  // The first events subscription resumes from the sign-in check's head, so once the live socket
  // opens, every write after this reaches the page.
  const stream = page.waitForEvent(
    "websocket",
    (socket) => new URL(socket.url()).pathname === "/ws/live"
  );
  await page.goto(`/issues/${issue.key}/conversation`);
  await stream;
  const turns = page.getByRole("list", { name: "Conversation turns" });
  const link = turns.getByRole("link", { name: "Beta as first written." });
  await expect(link).toHaveAttribute("href", `/issues/${issue.key}/comments/${cited.id}#b-b2`);
  const card = await openCard(page, link);
  await expect(card).toContainText("Comment · alice");
  await expect(card).toContainText("Beta as first written.");

  // The author rewords the cited paragraph; b1 stays, so the reworded paragraph keeps b2.
  await editComment(cited.id, { body: "Alpha first.\n\nBeta as reworded." });
  await expect(card).toContainText("Beta as reworded.", { timeout: 5_000 });
  await expect(card).not.toContainText("as first written");
  await expect(turns.getByRole("link", { name: "Beta as reworded." })).toHaveAttribute(
    "href",
    `/issues/${issue.key}/comments/${cited.id}#b-b2`
  );
});

// The author's edit that deletes the cited paragraph is an edit too: the open card and the link
// stop showing the paragraph's text without a reload, and read as a freshly loaded page reads the
// same link (no text of a paragraph the comment no longer holds).
test("a link to a block of a comment stops showing the block's text once its author deletes it, with no reload", async ({
  page,
}) => {
  const issue = await createIssue({ project: "CORE", title: "Deleted comment block" });
  const cited = await createComment(issue.key, { body: "Alpha first.\n\nBeta soon deleted." });
  await createComment(issue.key, {
    body: `The detail was in dispatch://${issue.key}/comment/${cited.id}#b-b2 today.`,
  });

  const stream = page.waitForEvent(
    "websocket",
    (socket) => new URL(socket.url()).pathname === "/ws/live"
  );
  await page.goto(`/issues/${issue.key}/conversation`);
  await stream;
  const turns = page.getByRole("list", { name: "Conversation turns" });
  const link = turns.getByRole("link", { name: "Beta soon deleted." });
  const card = await openCard(page, link);
  await expect(card).toContainText("Beta soon deleted.");

  await editComment(cited.id, { body: "Alpha first." });
  await expect(card).not.toContainText("Beta soon deleted.", { timeout: 5_000 });
  await expect(turns.getByRole("link", { name: "Beta soon deleted." })).toHaveCount(0);
});

// A link to a block of a document reads the same block read, which a version of the document
// fetches again: a document edit that deletes the cited block makes it answer 404, and the open
// card and the link stop showing the block's text without a reload, reading as a freshly loaded
// page reads the same link (no text of a block the document no longer holds).
test("a link to a block of a spec stops showing the block's text once a document edit deletes it, with no reload", async ({
  page,
}) => {
  const issue = await createIssue({
    project: "CORE",
    spec: "Intro paragraph.\n\nGamma soon deleted.\n",
    title: "Deleted spec block",
  });
  const specId = issue.primary_artifact_id;
  const paragraph = await blockId(specId, "Gamma soon deleted.");
  const before = await latestVersion(specId);
  await createComment(issue.key, {
    body: `The detail was in dispatch://${issue.key}/spec#b-${paragraph} today.`,
  });

  const stream = page.waitForEvent(
    "websocket",
    (socket) => new URL(socket.url()).pathname === "/ws/live"
  );
  await page.goto(`/issues/${issue.key}/conversation`);
  await stream;
  const turns = page.getByRole("list", { name: "Conversation turns" });
  const link = turns.getByRole("link", { name: "Gamma soon deleted." });
  await expect(link).toHaveAttribute("href", `/issues/${issue.key}/spec#b-${paragraph}`);
  const card = await openCard(page, link);
  await expect(card).toContainText("Gamma soon deleted.");

  await editArtifact(specId, {
    ops: [{ block: paragraph, op: "delete" }],
    summary: "Drop the paragraph",
  });
  await expect.poll(() => latestVersion(specId)).toBe(before + 1);
  await expect(card).not.toContainText("Gamma soon deleted.", { timeout: 5_000 });
  await expect(card).toHaveText(`${issue.key} spec block`);
  await expect(turns.getByRole("link", { name: "Gamma soon deleted." })).toHaveCount(0);
  await expect(turns.getByRole("link", { name: `${issue.key} spec block` })).toHaveAttribute(
    "href",
    `/issues/${issue.key}/spec#b-${paragraph}`
  );

  // A freshly loaded page reads the same link the same way.
  await page.reload();
  const fresh = page
    .getByRole("list", { name: "Conversation turns" })
    .getByRole("link", { name: `${issue.key} spec block` });
  await expect(fresh).toHaveAttribute("href", `/issues/${issue.key}/spec#b-${paragraph}`);
  await expect(await openCard(page, fresh)).toHaveText(`${issue.key} spec block`);
});
