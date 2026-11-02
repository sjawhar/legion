// The Dispatch screenshots `shots.ts` takes against the docs harness, from the seeded example
// workspace. The set writes `docs/site/public/media/dispatch/<id>.png`, which a page embeds as
// `![<alt>](/legion/media/dispatch/<id>.png)`. A section's own set lives in
// `<section>/shots.config.ts` (the Legion section's: `legion/shots.config.ts`).
import type { Locator, Page } from "@playwright/test";

import { type DispatchWorkspace, expect, seedDispatchWorkspace } from "./harness";
import type { ShotSet } from "./shot-runner";

/** The Inbox page from 1280 px: the two columns, the questions as short rows in the list, one
 *  row selected, and the snoozed one folded under Later. */
async function inboxPaneReady(page: Page): Promise<void> {
  await expect(page.getByRole("heading", { name: "Inbox" })).toBeVisible();
  await expect(page.getByTestId("inbox-list")).toBeVisible();
  await expect(page.getByTestId("inbox-pane")).toBeVisible();
  await expect(page.locator('[data-inbox-selected="true"]')).toHaveCount(1);
  await expect(page.getByRole("button", { name: /^Later \(1\)/ })).toBeVisible();
}

/** The Inbox below 1280 px: no reading column, so each question holds its own card in its row,
 *  the blocking one first, with the snoozed one folded under Later. */
async function inboxCardsReady(page: Page): Promise<void> {
  await expect(page.getByRole("heading", { name: "Inbox" })).toBeVisible();
  await expect(page.getByTestId("inbox-pane")).toHaveCount(0);
  await expect(page.getByRole("article", { name: "Urgency: Blocking" })).toBeVisible();
  await expect(page.getByRole("button", { name: /^Later \(1\)/ })).toBeVisible();
}

/** The row of the spec's decision block, which both Inbox shots are of. */
function signInRow(page: Page): Locator {
  return page.locator("[data-inbox-row]").filter({ hasText: "Which sign-in path?" });
}

/** The spec is live in the editor with its decision block hosted and the margin filled. */
async function specReady(page: Page, seeded: DispatchWorkspace): Promise<void> {
  const editor = page.getByRole("textbox", { name: "Document editor" });
  await expect(editor).toContainText("Local testing workflow");
  await expect(page.getByRole("status", { name: "connected" })).toBeVisible();
  await expect(
    editor.locator("[data-dispatch-ask-block] article[data-testid^=ask-]")
  ).toBeVisible();
  await expect(
    page.locator(`[data-margin-item="${seeded.marginThread.commentId}"]`).filter({ visible: true })
  ).toBeVisible();
}

const spec = (seeded: DispatchWorkspace) => `/issues/${seeded.issues.workflow}/spec`;

const dispatch: ShotSet<DispatchWorkspace> = {
  set: "dispatch",
  seed: seedDispatchWorkspace,
  shots: [
    {
      id: "inbox",
      alt: "The Dispatch Inbox in two columns: the open questions as short rows on the left, and the selected question, a decision block, open in its own document on the right.",
      route: "/",
      viewport: "desktop",
      theme: "light",
      ready: inboxPaneReady,
      steps: async (page) => {
        // The age is the one part of a short row that is neither the owner link nor a control,
        // so the click reaches the row itself (`e2e/inbox-pane.ts`'s `selectRow`).
        await signInRow(page).locator("time").first().click();
        await expect(signInRow(page)).toHaveAttribute("data-inbox-selected", "true");
        const pane = page.getByTestId("inbox-pane");
        await expect(pane).toContainText("Local testing workflow");
        await expect(
          pane.locator("[data-dispatch-ask-block] article[data-testid^=ask-]")
        ).toBeVisible();
      },
    },
    {
      id: "ask-card",
      alt: "An ask card in the Inbox: the question, its two options and Other, a note field, and Answer.",
      route: "/",
      viewport: "desktop",
      // Below 1280 px the Inbox has no reading column, so each question keeps its card in its
      // own row. This shot is of that card.
      size: { height: 900, width: 1024 },
      theme: "light",
      ready: async (page) => {
        await inboxCardsReady(page);
        await expect(signInRow(page).locator("[data-ask-answer]")).toBeVisible();
      },
      element: signInRow,
    },
    {
      id: "issue-spec",
      alt: "An issue's spec in the editor, with a decision block in the document and a comment thread in the margin.",
      route: spec,
      viewport: "desktop",
      theme: "light",
      ready: specReady,
    },
    {
      id: "margin-thread",
      alt: "A comment thread anchored on highlighted spec text, open in the margin beside the document with its reply.",
      route: (seeded) =>
        `/issues/${seeded.issues.workflow}/comments/${seeded.marginThread.commentId}`,
      viewport: "desktop",
      theme: "light",
      ready: specReady,
      steps: async (page) => {
        await expect(
          page.getByText("Yes: one of each, plus an ask on the Runbook document.")
        ).toBeVisible();
      },
    },
    {
      id: "board",
      alt: "A project's Board: one column per lifecycle status, with Icebox and Done folded into rails.",
      route: "/projects/CORE/issues",
      viewport: "desktop",
      theme: "light",
      ready: async (page) => {
        await expect(page.getByRole("button", { name: "Board" })).toBeVisible();
      },
      steps: async (page, seeded) => {
        await page.getByRole("button", { name: "Board" }).click();
        const board = page.getByRole("region", { name: "Project board" });
        await expect(
          board.getByRole("article").filter({ hasText: "Local testing workflow" })
        ).toBeVisible();
        await expect(page.getByRole("region", { name: "Done (collapsed)" })).toBeVisible();
        await expect(page.getByRole("region", { name: "In progress" })).toContainText(
          seeded.issues.workflow
        );
      },
    },
    {
      id: "agents",
      alt: "The Agents page: live sessions grouped by activity, one opened to show the conversation with it.",
      route: "/agents",
      viewport: "desktop",
      theme: "light",
      ready: async (page) => {
        const agents = page.getByRole("region", { name: "Agents" });
        // A folded section's rows stay mounted, hidden (packages/dispatch/AGENTS.md), so only the
        // rows on screen are counted.
        await expect
          .poll(async () =>
            (await agents.locator("article:not([hidden]) h2").allTextContents()).sort()
          )
          .toEqual(["Planner", "Reviewer", "Tester"]);
      },
      steps: async (page) => {
        const planner = page
          .getByRole("region", { name: "Agents" })
          .locator("article")
          .filter({ has: page.getByRole("heading", { level: 2, name: "Planner" }) });
        await planner.getByRole("button", { exact: true, name: "Planner" }).click();
        await expect(
          planner.getByRole("list", { name: "Conversation with Planner" })
        ).toContainText("Half-way; the seed is next.");
      },
    },
    {
      id: "broadcast",
      alt: "Broadcasting from the Agents page: two sessions ticked and a message in the composer, ready to send.",
      route: "/agents",
      viewport: "desktop",
      theme: "light",
      ready: async (page) => {
        await expect(
          page.getByRole("checkbox", { name: "Select Planner for broadcast" })
        ).toBeVisible();
      },
      steps: async (page) => {
        for (const title of ["Tester", "Planner"]) {
          await page.getByRole("checkbox", { name: `Select ${title} for broadcast` }).check();
        }
        const composer = page.getByRole("region", { name: "Broadcast" });
        await composer
          .getByRole("textbox", { name: "Broadcast message" })
          .fill("Pause and post your status on your issue.");
        await expect(composer.getByRole("button", { name: "Send to 2" })).toBeEnabled();
      },
    },
    {
      id: "broadcast-result",
      alt: "A sent broadcast: every recipient in the order it was sent, with the one reply so far.",
      route: (seeded) => `/agents/broadcasts/${seeded.broadcast.id}`,
      viewport: "desktop",
      theme: "light",
      ready: async (page) => {
        const broadcast = page.getByRole("region", { name: "Broadcast" });
        await expect(broadcast.getByText("1 of 3 answered")).toBeVisible();
        await expect(broadcast.getByText("Standing down; build is green.")).toBeVisible();
      },
    },
    {
      id: "palette",
      alt: "The command palette, opened with Ctrl+K, searching the workspace.",
      route: (seeded) => `/issues/${seeded.issues.workflow}/spec`,
      viewport: "desktop",
      theme: "light",
      ready: specReady,
      steps: async (page) => {
        await page.locator("body").focus();
        await page.keyboard.press("Control+k");
        const dialog = page.getByRole("dialog", { name: "Search" });
        await page.getByRole("combobox", { name: "Search" }).fill("runbook");
        await expect(dialog.getByRole("option", { name: /^doc / }).first()).toBeVisible();
        await expect(dialog.getByText("Searching…")).toHaveCount(0);
      },
    },
    {
      id: "shortcuts",
      alt: "The keyboard shortcuts overlay, opened with ?, over the Inbox's two columns, listing the keys for the page you are on.",
      route: "/",
      viewport: "desktop",
      theme: "light",
      ready: inboxPaneReady,
      steps: async (page) => {
        await page.locator("body").focus();
        await page.keyboard.press("?");
        const help = page.getByRole("dialog", { name: "Keyboard shortcuts" });
        await expect(help.getByRole("region", { name: "Inbox" })).toBeVisible();
      },
    },
    {
      id: "inbox-phone",
      alt: "The Inbox on a phone in dark mode.",
      route: "/",
      viewport: "phone",
      theme: "dark",
      ready: async (page) => {
        await expect(page.getByRole("article", { name: "Urgency: Blocking" })).toBeVisible();
      },
    },
    {
      id: "delivery-measures",
      alt: "The Delivery page's measures panel: six target cards over a strip of headline numbers.",
      route: "/delivery?mode=list&from=2026-08-30T00%3A00%3A00Z&to=2026-09-27T20%3A00%3A00Z",
      viewport: "desktop",
      theme: "light",
      ready: async (page) => {
        const kpis = page.getByLabel("KPI targets");
        await expect(kpis).toBeVisible();
        await expect(kpis.locator('[data-kpi="Deploys a day"]')).toContainText("5 deploys");
      },
      element: (page) => page.getByLabel("KPI targets").locator(".."),
    },
    {
      id: "pipeline",
      alt: "The Pipeline page: six timing cards over the slowest-jobs and failing-jobs tables.",
      // The e2e seed's pinned day (e2e/delivery-seed.ts PIPELINE_AT), so the fixture's runs are
      // the seven days shown.
      route: "/pipeline?at=2026-10-02T12%3A00%3A00Z",
      viewport: "desktop",
      theme: "light",
      ready: async (page) => {
        await expect(page.locator('[data-metric="staging_e2e"]')).toContainText("55m");
        await expect(page.locator('[data-failing-job="build / unit"]')).toBeVisible();
      },
    },
  ],
};

export default dispatch;
