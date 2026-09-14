import { expect, test } from "@playwright/test";

import {
  answerAsk,
  createIssue,
  createIssueArtifact,
  createProject,
  editArtifact,
  getIssue,
  patchIssue,
  resolveAsk,
} from "./api";
import { documentEditor } from "./editor";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

test.beforeEach(async () => {
  await resetDatabase();
});

test("agentWrittenAskBlockAppearsAndAnswersInPlace", async ({ browser }, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "## Specification\n\nContext\n",
    title: "Ask block",
  });
  await patchIssue(issue.key, { labels: ["dispatch", "decision", "priority"] });
  await editArtifact(
    issue.primary_artifact_id,
    {
      ops: [
        {
          after: "end",
          markdown:
            ':::ask{#ship-decision urgency="high" multiple="false" state="open"}\nShould we ship?\n\n- Ship: Release it\n- Hold: Wait for review\n:::\n',
          op: "insert",
        },
      ],
    },
    { as: "agent" }
  );
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.setViewportSize({ width: 1770, height: 900 });
    await page.goto(`/issues/${issue.key}`);
    const ask = documentEditor(page).locator('[data-dispatch-ask-block="ship-decision"]');
    await expect(ask).toContainText("Should we ship?");
    const decisions = page.getByRole("navigation", { name: "Open decisions" });
    await expect(decisions.getByText("1 open decision", { exact: true })).toBeVisible();
    const decisionLink = decisions.getByRole("link", { name: "Should we ship?" });
    await expect(decisionLink).toHaveAttribute("title", "Should we ship?");
    await expect(decisions).toHaveCSS("height", "28px");
    const header = page.getByTestId("issue-header");
    const specification = documentEditor(page).getByRole("heading", { name: "Specification" });
    const desktopMetrics = await Promise.all([
      header.evaluate((element) => element.getBoundingClientRect().height),
      specification.evaluate((element) => element.getBoundingClientRect().y),
    ]);
    await testInfo.attach("specchrome-1770-metrics", {
      body: JSON.stringify({ headerHeight: desktopMetrics[0], specificationY: desktopMetrics[1] }),
      contentType: "application/json",
    });
    await page.screenshot({ path: testInfo.outputPath("specchrome-1770.png") });
    expect(desktopMetrics[0]).toBeLessThanOrEqual(96);

    await page.setViewportSize({ width: 1280, height: 800 });
    const tabletMetrics = await Promise.all([
      header.evaluate((element) => element.getBoundingClientRect().height),
      specification.evaluate((element) => element.getBoundingClientRect().y),
    ]);
    expect(tabletMetrics[0]).toBeLessThanOrEqual(140);
    await testInfo.attach("specchrome-1280-metrics", {
      body: JSON.stringify({ headerHeight: tabletMetrics[0], specificationY: tabletMetrics[1] }),
      contentType: "application/json",
    });

    await page.setViewportSize({ width: 390, height: 844 });
    await page.screenshot({ path: testInfo.outputPath("specchrome-390.png") });

    const form = ask.locator("form");
    await expect(form).toBeVisible({ timeout: 10_000 });
    await expect(ask.locator("[data-dispatch-ask-pill]")).toHaveText("High");
    await expect(ask).toHaveAttribute("data-dispatch-ask-urgency", "high");
    // The server indexes the block into an ask; the block then names when it was asked (and by
    // whom, once the server records the editing actor) and its form comes alive.
    await expect(ask.locator("[data-dispatch-ask-asked] time")).toBeVisible({ timeout: 10_000 });
    await expect(form.getByRole("radio").first()).toBeEnabled();
    const submit = form.getByRole("button", { name: "Answer" });
    await expect(submit).toBeDisabled();
    const answer = form.locator('textarea[name="answer"]');
    await answer.fill("Yes.");
    await answer.press("Enter");
    await expect(answer).toHaveValue("Yes.\n");
    // A note alone is not an answer while the decision has options.
    await expect(submit).toBeDisabled();
    await answer.press("Control+Enter");
    await expect(ask).not.toContainText("Answered by");
    await form.getByRole("radio", { name: "Ship Release it" }).check();
    await expect(submit).toBeEnabled();
    await answer.press("Control+Enter");
    await expect(ask).toContainText("Answered by alice");
    await expect(ask.locator('ul[aria-label="Options"] li[data-selected="true"]')).toContainText(
      "Ship"
    );
    await expect(ask.locator("[data-dispatch-ask-answer]")).toHaveText("Yes.");
    await expect(ask.locator("form")).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

test("a resolved decision block is read-only", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", spec: "Context\n", title: "Resolved ask" });
  await editArtifact(
    issue.primary_artifact_id,
    {
      ops: [
        {
          after: "end",
          markdown:
            ':::ask{#resolved-decision urgency="med" multiple="false" state="open"}\nShould we ship?\n\n- Ship\n- Hold\n:::\n',
          op: "insert",
        },
      ],
    },
    { as: "agent" }
  );
  await expect
    .poll(
      async () =>
        (await getIssue(issue.key)).open_asks.some(
          (candidate) => candidate.block_id === "resolved-decision"
        ),
      { timeout: 10_000 }
    )
    .toBe(true);
  const blockAsk = (await getIssue(issue.key)).open_asks.find(
    (candidate) => candidate.block_id === "resolved-decision"
  );
  if (blockAsk === undefined) {
    throw new Error("resolved decision ask was not created");
  }
  await resolveAsk(
    blockAsk.id,
    { kind: "resolved", reason: "The decision is no longer needed." },
    { as: "agent" }
  );
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}`);
    const ask = documentEditor(page).locator('[data-dispatch-ask-block="resolved-decision"]');
    await expect(ask.getByText("Resolved", { exact: true })).toBeVisible();
    await expect(ask.locator("form")).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

test("a failed in-document decision answer identifies the failed block", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", spec: "Context\n", title: "Failing ask" });
  await editArtifact(
    issue.primary_artifact_id,
    {
      ops: [
        {
          after: "end",
          markdown:
            ':::ask{#failing-decision urgency="med" multiple="false" state="open"}\nShould we ship?\n\n- Ship\n- Hold\n:::\n',
          op: "insert",
        },
      ],
    },
    { as: "agent" }
  );
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.route("**/api/v1/asks/*/answer", async (route) => {
      await route.fulfill({
        body: JSON.stringify({ error: "simulated failure" }),
        contentType: "application/json",
        status: 500,
      });
    });
    await page.goto(`/issues/${issue.key}`);
    await expect(page.getByRole("navigation", { name: "Open decisions" })).toBeVisible();
    const ask = documentEditor(page).locator('[data-dispatch-ask-block="failing-decision"]');
    const form = ask.locator("form");
    await expect(form).toBeVisible({ timeout: 10_000 });
    const error = page.getByRole("article", { name: "Document" }).getByRole("alert");
    await expect(error).toHaveCount(0);
    await form.getByRole("radio", { name: "Other" }).check();
    await form.locator('textarea[name="answer"]').fill("Ship after review.");
    await form.getByRole("button", { name: "Answer" }).click();
    await expect(error).toHaveAttribute("data-dispatch-ask-error", "failing-decision");
    await expect(error).toHaveText("Could not save your answer.");
  } finally {
    await alice.close();
  }
});

test("malformed decision with a blank option label disables the answer form", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", spec: "Context\n", title: "Malformed ask" });
  const malformed = await createIssueArtifact(
    issue.key,
    {
      content:
        ':::ask{#malformed-decision urgency="high" multiple="false" state="open"}\nWhich path?\n\n- Ship: Release it\n- : No label\n:::\n',
      name: "malformed.md",
    },
    { as: "agent" }
  );
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.setViewportSize({ width: 1280, height: 800 });
    await page.goto(`/issues/${issue.key}/artifacts/${malformed.artifact.slug}`);
    const ask = documentEditor(page).locator('[data-dispatch-ask-block="malformed-decision"]');
    await expect(ask.getByText("Malformed", { exact: true })).toBeVisible();
    await expect(
      ask.getByText(/ask block "malformed-decision" has an option without a label/)
    ).toBeVisible({ timeout: 10_000 });
    await expect(
      ask.getByText("Fix the block text; the decision re-activates once it parses.")
    ).toBeVisible();
    await expect(ask.locator("form")).toHaveCount(0);
    await expect(ask.locator("li:empty")).toHaveCount(0);
    await expect(ask.getByRole("listitem")).toHaveCount(2);
    await page.screenshot({ path: testInfo.outputPath("specchrome-malformed-1280.png") });
  } finally {
    await alice.close();
  }
});

test("open decision navigation cycles through the available decisions", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", spec: "Context\n", title: "Two decisions" });
  await editArtifact(
    issue.primary_artifact_id,
    {
      ops: [
        {
          after: "end",
          markdown:
            ':::ask{#first-decision urgency="med" multiple="false" state="open"}\nUse Postgres?\n\n- Yes\n:::\n\n:::ask{#second-decision urgency="med" multiple="false" state="open"}\nUse Redis?\n\n- Yes\n:::\n',
          op: "insert",
        },
      ],
    },
    { as: "agent" }
  );
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}`);
    const decisions = page.getByRole("navigation", { name: "Open decisions" });
    await expect(decisions.getByText("2 open decisions", { exact: true })).toBeVisible();
    const firstDecision = await decisions.getByRole("link").textContent();
    expect(firstDecision).not.toBeNull();
    await decisions.getByRole("button", { name: "Next open decision" }).click();
    await expect(decisions.getByRole("link")).not.toHaveText(firstDecision ?? "");
    await decisions.getByRole("button", { name: "Next open decision" }).click();
    await expect(decisions.getByRole("link")).toHaveText(firstDecision ?? "");
  } finally {
    await alice.close();
  }
});

const DECISION_SPEC = [
  ':::ask{#storage urgency="blocking" multiple="false" state="open"}\nWhich storage engine backs the event log?\n\n- Postgres: Reuse the existing cluster\n- SQLite: One file per team, no ops\n:::\n',
  "Some prose between the decisions so the margins around each block are visible.\n",
  ':::ask{#retention urgency="high" multiple="true" state="open"}\nWhich retention windows do we support at launch?\n\n- 7 days\n- 30 days\n- Forever\n:::\n',
  ':::ask{#naming urgency="low" multiple="false" state="open"}\nShould the CLI be called `legion` or `lg`?\n\n- legion\n- lg\n:::\n',
  "Closing paragraph after the last decision.\n",
].join("\n");

test("decision blocks read as urgency-accented cards in the document and its version view", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "## Specification\n\nContext paragraph before the first decision.\n",
    title: "Decision rendering",
  });
  await editArtifact(
    issue.primary_artifact_id,
    { ops: [{ after: "end", markdown: DECISION_SPEC, op: "insert" }] },
    { as: "agent" }
  );
  await expect
    .poll(async () => (await getIssue(issue.key)).open_asks.length, { timeout: 10_000 })
    .toBe(3);
  const naming = (await getIssue(issue.key)).open_asks.find((ask) => ask.block_id === "naming");
  if (naming === undefined) {
    throw new Error("the naming decision was not indexed");
  }
  await answerAsk(
    naming.id,
    { expected_edited_at: null, selected: ["legion"], text: "Short names are for shells." },
    { login: "bob" }
  );
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const editor = documentEditor(page);
    for (const [width, height] of [
      [1280, 900],
      [390, 844],
    ] as const) {
      for (const scheme of ["light", "dark"] as const) {
        await page.emulateMedia({ colorScheme: scheme });
        await page.setViewportSize({ width, height });
        await page.goto(`/issues/${issue.key}`);
        await expect(editor.locator('[data-dispatch-ask-block="naming"]')).toContainText(
          "Answered by bob"
        );
        await expect(
          editor.locator('[data-dispatch-ask-block="storage"] [data-dispatch-ask-asked] time')
        ).toBeVisible({ timeout: 10_000 });
        await page.screenshot({
          fullPage: width > 400,
          path: testInfo.outputPath(`decisions-${width}-${scheme}.png`),
        });
      }
    }

    for (const [blockId, urgency, label] of [
      ["storage", "blocking", "Blocking"],
      ["retention", "high", "High"],
      ["naming", "low", "Low"],
    ] as const) {
      const block = editor.locator(`[data-dispatch-ask-block="${blockId}"]`);
      await expect(block.locator("[data-dispatch-ask-pill]")).toHaveText(label);
      await expect(block).toHaveAttribute("data-dispatch-ask-urgency", urgency);
      await expect(block.getByText("Decision", { exact: true })).toBeVisible();
    }
    const storage = editor.locator('[data-dispatch-ask-block="storage"]');
    const context = editor.getByText("Context paragraph before the first decision.");
    // The rows are the options; the source "- Option: description" list stays folded until the
    // caret is in the block, and folds again when the caret leaves.
    const source = storage.locator("[data-proof-block-content] ul");
    await expect(source).toBeHidden();
    await storage.getByText("Which storage engine backs the event log?").click();
    await expect(storage).toHaveAttribute("data-dispatch-ask-editing", "true");
    await expect(source).toBeVisible();
    await expect(source.getByText("Postgres: Reuse the existing cluster")).toBeVisible();
    await context.click();
    await expect(storage).not.toHaveAttribute("data-dispatch-ask-editing", "true");
    await expect(source).toBeHidden();
    await expect(storage.getByRole("radio")).toHaveCount(3);
    await expect(storage.locator("form").getByText("Reuse the existing cluster")).toBeVisible();
    const submit = storage.getByRole("button", { name: "Answer" });
    await expect(submit).toBeDisabled();
    await storage.getByRole("radio", { name: "Postgres Reuse the existing cluster" }).check();
    await expect(submit).toBeEnabled();

    // The card sits in the prose with the same gap above and below it.
    const gaps = await storage.evaluate((section) => {
      const previous = section.previousElementSibling?.getBoundingClientRect();
      const next = section.nextElementSibling?.getBoundingClientRect();
      const own = section.getBoundingClientRect();
      return previous === undefined || next === undefined
        ? undefined
        : { above: own.top - previous.bottom, below: next.top - own.bottom };
    });
    expect(gaps).toBeDefined();
    expect(Math.abs((gaps?.above ?? 0) - (gaps?.below ?? 1))).toBeLessThan(1);
    expect(gaps?.above).toBeGreaterThan(12);

    const answered = editor.locator('[data-dispatch-ask-block="naming"]');
    await expect(answered.locator('li[data-selected="true"]')).toContainText("legion");
    await expect(answered.locator('li[data-selected="false"]')).toContainText("lg");
    await expect(answered.locator("[data-dispatch-ask-answer]")).toHaveText(
      "Short names are for shells."
    );
    await expect(answered.locator("form")).toHaveCount(0);
    // Once answered, the rows are the record and the source list never shows, caret or not.
    const answeredSource = answered.locator("[data-proof-block-content] ul");
    await expect(answeredSource).toBeHidden();
    await answered.getByText("Should the CLI be called").click();
    await expect(answered).toHaveAttribute("data-dispatch-ask-editing", "true");
    await expect(answeredSource).toBeHidden();
    await context.click();

    // The historical version keeps the same card, read-only: options shown, nothing to answer.
    await page.emulateMedia({ colorScheme: "light" });
    await page.setViewportSize({ width: 1280, height: 900 });
    await page.getByRole("combobox", { name: "Version" }).selectOption("2");
    const versionView = page.getByTestId("version-view");
    const historical = versionView.locator('[data-dispatch-ask-block="storage"]');
    await expect(historical.locator("[data-dispatch-ask-pill]")).toHaveText("Blocking");
    await expect(historical.locator("form")).toHaveCount(0);
    const historicalSource = historical.locator("[data-proof-block-content] ul");
    await expect(historicalSource).toBeHidden();
    await historical.getByText("Which storage engine backs the event log?").click();
    await expect(historicalSource).toBeHidden();
    await expect(historical.locator('ul[aria-label="Options"] li')).toHaveCount(2);
    await expect(
      historical.locator('ul[aria-label="Options"]').getByText("Reuse the existing cluster")
    ).toBeVisible();
    await expect(historical.getByRole("button", { name: "Answer" })).toHaveCount(0);
    await page.screenshot({ path: testInfo.outputPath("decisions-version-1280-light.png") });
  } finally {
    await alice.close();
  }
});
