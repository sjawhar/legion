// The Dispatch screenshots `shots.ts` takes against the docs harness, from the seeded example
// workspace. The set writes `docs/site/public/media/dispatch/<id>.png`, which a page embeds as
// `![<alt>](/legion/media/dispatch/<id>.png)`. A section's own set lives in
// `<section>/shots.config.ts` (the Legion section's: `legion/shots.config.ts`).
import type { Page } from "@playwright/test";

import { type DispatchWorkspace, expect, seedDispatchWorkspace } from "./harness";
import type { ShotSet } from "./shot-runner";

/** The Inbox has rendered its rows: the blocking ask leads, the snoozed one is folded. */
async function inboxReady(page: Page): Promise<void> {
  await expect(page.getByRole("heading", { name: "Inbox" })).toBeVisible();
  await expect(page.getByRole("article", { name: "Urgency: Blocking" })).toBeVisible();
  await expect(page.getByRole("button", { name: /^Later \(1\)/ })).toBeVisible();
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
      alt: "The Dispatch Inbox: asks waiting on you, the blocking one first, with a snoozed ask folded under Later.",
      route: "/",
      viewport: "desktop",
      theme: "light",
      ready: inboxReady,
    },
    {
      id: "ask-card",
      alt: "An ask card in the Inbox: the question, its two options and Other, a note field, and Answer.",
      route: "/",
      viewport: "desktop",
      theme: "light",
      ready: inboxReady,
      element: (page) =>
        page.locator("[data-inbox-row]").filter({ hasText: "Which sign-in path?" }),
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
        await expect
          .poll(async () => (await agents.locator("article h2").allTextContents()).sort())
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
      alt: "The keyboard shortcuts overlay, opened with ?, listing the keys for the page you are on.",
      route: "/",
      viewport: "desktop",
      theme: "light",
      ready: inboxReady,
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
  ],
};

export default dispatch;
