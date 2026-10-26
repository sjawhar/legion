import { expect, type Locator, type Page, test } from "@playwright/test";

import type { ArtifactBlock } from "../web/src/api/types";
import {
  baseUrl,
  createComment,
  createIssue,
  createIssueArtifact,
  createProject,
  editArtifact,
  getArtifact,
  getArtifactText,
  getIssue,
  userHeaders,
} from "./api";
import { resetDatabase } from "./seed";
import { signIn } from "./users";

// Every row seeds through the real API and reads the real server: a block's id comes from
// `GET /api/v1/artifacts/<id>/blocks`, never from a guess. Each helper lives in this file, so a
// run at the base, where none of this exists, copies this one file.

test.beforeEach(async ({ context }) => {
  await resetDatabase();
  await signIn(context, "alice");
  await createProject({ key: "CORE", name: "Core" });
});

/** The id of the block of `artifactId` whose markdown is exactly `text`. A paragraph's id is a
 *  minted UUID, which `encodeBlockId` writes unchanged, so the id is also its `#b-` fragment. */
async function blockId(artifactId: string, text: string): Promise<string> {
  const { markdown } = await getArtifactText(artifactId);
  const response = await fetch(
    new URL(`/api/v1/artifacts/${encodeURIComponent(artifactId)}/blocks`, baseUrl),
    { headers: await userHeaders() }
  );
  if (!response.ok) {
    throw new Error(
      `GET blocks of ${artifactId} failed: ${response.status} ${await response.text()}`
    );
  }
  const blocks = (await response.json()) as ArtifactBlock[];
  const block = blocks.find((candidate) => markdown.slice(candidate.from, candidate.to) === text);
  if (block === undefined) {
    throw new Error(`no block of ${artifactId} reads ${JSON.stringify(text)}: ${markdown}`);
  }
  expect(block.id).toMatch(/^[A-Za-z0-9-]+$/);
  return block.id;
}

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
