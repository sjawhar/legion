import { expect, test } from "@playwright/test";

import {
  createAsk,
  createComment,
  createIssue,
  createMessage,
  createProject,
  createProjectDocument,
  editArtifact,
  editAsk,
  editComment,
  getIssue,
} from "./api";
import { documentEditor } from "./editor";
import { resetDatabase } from "./seed";

test.use({ extraHTTPHeaders: { "X-Dispatch-User": "alice" } });

test.beforeEach(async () => {
  await resetDatabase();
});

test("an issue and its ask show a cross-project message and structural backlinks", async ({
  page,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  await createProject({ key: "OPS", name: "Operations" });
  const target = await createIssue({ project: "CORE", title: "Target issue" });
  const decision = await createAsk(target.key, { question: "Should the target issue ship?" });
  const child = await createIssue({ parent: target.key, project: "CORE", title: "Child issue" });
  const source = await createIssue({ project: "OPS", title: "Source issue" });
  await createMessage(source.key, {
    body: `Depends on dispatch://${target.key} and dispatch://${target.key}/ask/${decision.id}.`,
  });

  await page.setViewportSize({ height: 900, width: 1440 });
  await page.goto(`/issues/${target.key}`);
  await page
    .getByTestId("issue-header")
    .getByRole("button", { name: /^Referenced by/ })
    .click();

  const issueReferences = page.getByTestId("issue-header").getByRole("region", {
    name: "Referenced by",
  });
  await expect(issueReferences).toContainText(`Message · ${source.key}`);
  await expect(issueReferences).toContainText(`Depends on dispatch://${target.key}`);
  await expect(issueReferences).toContainText(`Child issue · ${child.key}`);
  await expect(issueReferences).toContainText("Child issue");
  await expect(issueReferences).toContainText(`Attached document · ${target.key}`);
  await expect(issueReferences).toContainText("spec.md");

  const desktopHeader = testInfo.outputPath("referenced-by-issue-header-1440.png");
  await page.getByTestId("issue-header").screenshot({ path: desktopHeader });
  await testInfo.attach("Referenced by issue header (1440px)", {
    contentType: "image/png",
    path: desktopHeader,
  });

  await page.setViewportSize({ height: 844, width: 390 });
  const phoneHeader = testInfo.outputPath("referenced-by-issue-header-390.png");
  await page.getByTestId("issue-header").screenshot({ path: phoneHeader });
  await testInfo.attach("Referenced by issue header (390px)", {
    contentType: "image/png",
    path: phoneHeader,
  });
  await expect(
    page.evaluate(
      () => document.documentElement.scrollWidth === document.documentElement.clientWidth
    )
  ).resolves.toBe(true);

  await page.goto("/");
  const askCard = page.getByTestId(`ask-${decision.id}`);
  await askCard.getByRole("button", { name: /^Referenced by \(1\)$/ }).click();
  const askReferences = askCard.getByRole("region", { name: "Referenced by" });
  await expect(askReferences).toContainText(`Message · ${source.key}`);

  const phoneAsk = testInfo.outputPath("referenced-by-ask-card-390.png");
  await askCard.screenshot({ path: phoneAsk });
  await testInfo.attach("Referenced by ask card (390px)", {
    contentType: "image/png",
    path: phoneAsk,
  });

  await page.setViewportSize({ height: 900, width: 1440 });
  const desktopAsk = testInfo.outputPath("referenced-by-ask-card-1440.png");
  await askCard.screenshot({ path: desktopAsk });
  await testInfo.attach("Referenced by ask card (1440px)", {
    contentType: "image/png",
    path: desktopAsk,
  });
});

test("an open backlink panel follows a mention added, replied to, and removed", async ({
  page,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  await createProject({ key: "OPS", name: "Operations" });
  const target = await createIssue({ project: "CORE", title: "Target issue" });
  const source = await createIssue({ project: "OPS", title: "Source issue" });

  await page.goto(`/issues/${target.key}`);
  await page.getByRole("button", { name: /^Referenced by/ }).click();
  const references = page.getByTestId("issue-header").getByRole("region", {
    name: "Referenced by",
  });
  // Each step waits on a server write, the event stream, and the panel's refetch, so these are
  // deliberately generous on a loaded box.
  const live = { timeout: 30_000 };
  await expect(references).toContainText("Attached document", live);

  await createMessage(source.key, { body: `Blocked on dispatch://${target.key} today.` });
  await expect(references).toContainText("Blocked on dispatch://", live);

  const reply = await createComment(source.key, {
    body: `Still blocked on dispatch://${target.key} after the rollback.`,
  });
  await expect(references).toContainText("after the rollback", live);

  await editComment(reply.id, { body: "Unblocked; the dependency is gone." });
  await expect(references).not.toContainText("after the rollback", live);
});

// The Inbox row carries the ask's backlink count instead of the card reading the graph, so the
// count is only as live as the Inbox response — and a plain message or a comment replying to no
// ask is exactly the write whose Inbox refresh is otherwise skipped. A card at zero has no
// control at all, so without this the newly cited ask is invisible until a reload.
test("Inbox cards gain and move their backlink control as writes elsewhere cite their asks", async ({
  page,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  await createProject({ key: "OPS", name: "Operations" });
  const target = await createIssue({ project: "CORE", title: "Target issue" });
  const uncited = await createAsk(target.key, { question: "Nothing cites this yet?" });
  const cited = await createAsk(target.key, { question: "Should the target issue ship?" });
  const source = await createIssue({ project: "OPS", title: "Source issue" });
  await createMessage(source.key, {
    body: `Waiting on dispatch://${target.key}/ask/${cited.id}.`,
  });

  await page.goto("/");
  const uncitedCard = page.getByTestId(`ask-${uncited.id}`);
  const citedCard = page.getByTestId(`ask-${cited.id}`);
  const live = { timeout: 30_000 };
  await expect(citedCard.getByRole("button", { name: /^Referenced by \(1\)$/ })).toBeVisible(live);
  await expect(uncitedCard.getByRole("button", { name: /^Referenced by/ })).toHaveCount(0);

  await createMessage(source.key, {
    body: `Now also waiting on dispatch://${target.key}/ask/${uncited.id}.`,
  });
  await createMessage(source.key, {
    body: `Second cite of dispatch://${target.key}/ask/${cited.id}.`,
  });
  await expect(uncitedCard.getByRole("button", { name: /^Referenced by \(1\)$/ })).toBeVisible(
    live
  );
  await expect(citedCard.getByRole("button", { name: /^Referenced by \(2\)$/ })).toBeVisible(live);

  // A comment that replies to no ask is the other half of #1248's subtraction.
  const note = await createComment(source.key, {
    body: `Third citation of dispatch://${target.key}/ask/${cited.id}.`,
  });
  await expect(citedCard.getByRole("button", { name: /^Referenced by \(3\)$/ })).toBeVisible(live);

  await editComment(note.id, { body: "Withdrawn; nothing to wait on." });
  await expect(citedCard.getByRole("button", { name: /^Referenced by \(2\)$/ })).toBeVisible(live);
});

// Answering an ask moves no backlink count, and a citation written afterwards does. Both are the
// answered card's problem: it reads its ask from the thread, not from the answer response.
test("an answered decision keeps its backlink control and still moves with a later citation", async ({
  page,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  await createProject({ key: "OPS", name: "Operations" });
  const target = await createIssue({
    project: "CORE",
    spec: "## Specification\n\nContext\n",
    title: "Answered decision",
  });
  await editArtifact(
    target.primary_artifact_id,
    {
      ops: [
        {
          after: "end",
          markdown:
            ':::ask{#ship-decision urgency="med" multiple="false" state="open"}\nShould the answered decision ship?\n:::\n',
          op: "insert",
        },
      ],
    },
    { as: "agent" }
  );
  const live = { timeout: 30_000 };
  const decision = await expect
    .poll(async () => (await getIssue(target.key)).open_asks.at(0)?.id, live)
    .not.toBeUndefined()
    .then(async () => (await getIssue(target.key)).open_asks[0]);

  const source = await createIssue({ project: "OPS", title: "Citing source" });
  await createMessage(source.key, {
    body: `Waiting on dispatch://${target.key}/ask/${decision.id}.`,
  });

  await page.setViewportSize({ height: 900, width: 1440 });
  await page.goto(`/issues/${target.key}`);
  // The page hosts this ask twice — the decision block and the margin sheet; the block is the
  // one the reader answers in, and it keeps rendering the card once the ask is answered.
  const card = documentEditor(page).locator('[data-dispatch-ask-block="ship-decision"]');
  await expect(card.getByRole("button", { name: /^Referenced by \(1\)$/ })).toBeVisible(live);

  const note = card.getByRole("button", { name: "Add a note or answer in your own words" });
  if ((await note.count()) > 0) {
    await note.click();
  }
  await card.getByLabel("Your answer").fill("Ship it.");
  await card.getByRole("button", { exact: true, name: "Answer" }).click();
  await expect(card).toContainText("Ship it.", live);
  // The answer response is the one ask shape that carries no count: the control the reader was
  // shown must not vanish with it.
  await expect(card.getByRole("button", { name: /^Referenced by \(1\)$/ })).toBeVisible(live);

  await createMessage(source.key, {
    body: `Second citation of dispatch://${target.key}/ask/${decision.id}.`,
  });
  await expect(card.getByRole("button", { name: /^Referenced by \(2\)$/ })).toBeVisible(live);
});

// An ask's clarification thread is rendered on the card itself; counting those replies as
// backlinks would advertise a panel that only repeats it.
test("an ask's own clarification replies are not backlinks", async ({ page }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Reply-counted" });
  const decision = await createAsk(issue.key, { question: "Reply-counted?" });
  await createComment(issue.key, { ask_id: decision.id, body: "First clarification." });
  await createComment(issue.key, { ask_id: decision.id, body: "Second clarification." });

  await page.goto("/");
  const askCard = page.getByTestId(`ask-${decision.id}`);
  await expect(askCard).toContainText("Second clarification.");
  await expect(askCard.getByRole("button", { name: /^Referenced by/ })).toHaveCount(0);
});

// A project document's body cites the same nodes a message does, and its version event carries
// the same targets: an ask a document cites gains its control on an open Inbox.
test("an Inbox card gains its count when a project document cites the ask", async ({ page }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Document-cited" });
  const decision = await createAsk(issue.key, { question: "Should the document decide this?" });

  await page.goto("/");
  const askCard = page.getByTestId(`ask-${decision.id}`);
  await expect(askCard).toContainText("Should the document decide this?");
  await expect(askCard.getByRole("button", { name: /^Referenced by/ })).toHaveCount(0);

  await createProjectDocument("CORE", {
    content: `# Decision record\n\nWaiting on dispatch://${issue.key}/ask/${decision.id}.\n`,
    name: "Decision record",
  });
  await expect(askCard.getByRole("button", { name: /^Referenced by \(1\)$/ })).toBeVisible({
    timeout: 30_000,
  });
});

// `dispatch_doc_edit` writes through `POST /api/v1/artifacts/{id}/edits`, which records its own
// version and appends its own event: the path agents actually use has to name what it cited.
test("an Inbox card gains its count when a document edit cites the ask", async ({ page }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Edit-cited" });
  const decision = await createAsk(issue.key, { question: "Should the edit decide this?" });
  const document = await createProjectDocument("CORE", {
    content: "# Decision record\n\nPending.\n",
    name: "Decision record",
  });

  await page.goto("/");
  const askCard = page.getByTestId(`ask-${decision.id}`);
  await expect(askCard).toContainText("Should the edit decide this?");
  await expect(askCard.getByRole("button", { name: /^Referenced by/ })).toHaveCount(0);

  await editArtifact(document.artifact.id, {
    ops: [
      {
        op: "replace",
        find: "Pending.",
        with: `Waiting on dispatch://${issue.key}/ask/${decision.id}.`,
      },
    ],
  });
  await expect(askCard.getByRole("button", { name: /^Referenced by \(1\)$/ })).toBeVisible({
    timeout: 30_000,
  });
});

// An ask's question is indexed like any other body, so opening one that cites an issue moves
// that issue's own header count on the page already showing it.
test("an issue header's count follows an ask whose question cites it", async ({ page }) => {
  await createProject({ key: "CORE", name: "Core" });
  await createProject({ key: "OPS", name: "Operations" });
  const target = await createIssue({ project: "CORE", title: "Cited by a question" });
  const source = await createIssue({ project: "OPS", title: "Asking issue" });

  await page.goto(`/issues/${target.key}`);
  const header = page.getByTestId("issue-header");
  // The issue's own spec document is attached to it, so it starts at one.
  await expect(header.getByRole("button", { name: /^Referenced by \(1\)$/ })).toBeVisible();

  const decision = await createAsk(source.key, {
    question: `Does dispatch://${target.key} ship first?`,
  });
  await expect(header.getByRole("button", { name: /^Referenced by \(2\)$/ })).toBeVisible({
    timeout: 30_000,
  });

  await editAsk(decision.id, { question: "Does anything ship first?" });
  await expect(header.getByRole("button", { name: /^Referenced by \(1\)$/ })).toBeVisible({
    timeout: 30_000,
  });
});

test("an Inbox of twenty asks reads no backlinks until a card is opened", async ({ page }) => {
  await createProject({ key: "CORE", name: "Core" });
  await createProject({ key: "OPS", name: "Operations" });
  const issue = await createIssue({ project: "CORE", title: "Twenty decisions" });
  const source = await createIssue({ project: "OPS", title: "Citing issue" });
  const asks = [];
  for (let index = 0; index < 20; index += 1) {
    asks.push(await createAsk(issue.key, { question: `Decision ${index + 1}?` }));
  }
  const cited = asks[0];
  if (cited === undefined) {
    throw new Error("seeded no asks");
  }
  await createMessage(source.key, {
    body: `The rollout waits on dispatch://${issue.key}/ask/${cited.id}.`,
  });

  let referenceReads = 0;
  page.on("request", (request) => {
    if (new URL(request.url()).pathname === "/api/v1/references") {
      referenceReads += 1;
    }
  });

  await page.goto("/");
  await expect(page.getByTestId(/^ask-/)).toHaveCount(20);
  await expect(page.getByText("Decision 20?")).toBeVisible();
  expect(referenceReads).toBe(0);

  await page
    .getByTestId(`ask-${cited.id}`)
    .getByRole("button", { name: /^Referenced by \(1\)$/ })
    .click();
  await expect(
    page.getByTestId(`ask-${cited.id}`).getByRole("region", { name: "Referenced by" })
  ).toContainText(`Message · ${source.key}`);
  expect(referenceReads).toBe(1);
});
