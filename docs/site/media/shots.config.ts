// The screenshots `shots.ts` takes against the docs harness: Dispatch's own, from the seeded
// example workspace, and the Legion section's, from one example issue's journey. Each set writes
// `docs/site/public/media/<set>/<id>.png`, which a page embeds as
// `![<alt>](/legion/media/<set>/<id>.png)`.
import type { Page } from "@playwright/test";

import {
  type DispatchWorkspace,
  dispatchApi,
  expect,
  fakeEnvoy,
  seedDispatchWorkspace,
} from "./harness";
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

// One issue's journey through Legion, as Dispatch shows it, written to
// `docs/site/public/media/legion/<id>.png`. The seed hands an example issue to Legion; each shot's
// `prepare` then makes the next move the Legion daemon, its agents or the human makes, through the
// same API they write through, so the shots follow one issue in order.

const DAEMON = { actor: { id: "legion-daemon:SHOP", kind: "session" }, as: "agent" } as const;
const agent = (role: "architect" | "implementer") =>
  ({
    actor: {
      id: `shop-1-${role}`,
      kind: "session",
      origin: { session_title: `SHOP-1 ${role}` },
    },
    as: "agent",
  }) as const;

const CART_SPEC = `# Save a cart for later

A signed-in shopper can save the cart they are building and pick it up again later, on any device.

## Acceptance criteria

- A shopper with items in their cart can save it from the cart page.
- A saved cart opens from their account on another device, with the same items and quantities.
- A saved cart untouched for 30 days is removed, and the shopper is emailed a week before.
`;

const CART_DESIGN = `## Design

Saving copies the cart's lines into a saved cart the account owns. Opening a saved cart replaces the
current cart, after asking first when the current cart has items.

:::ask{#cart-storage urgency="high" multiple="false"}
Where should a saved cart live?

I recommend Server: the second criterion needs the cart on another device, which browser storage cannot give.

- Server: one saved cart per account in the database, so any device can open it
- Browser: local storage on the device that saved it, with no schema change
:::
`;

const PULL_REQUEST = "https://github.com/acme/storefront/pull/42";

interface LegionJourney {
  readonly key: string;
  readonly spec: string;
  /** The approval ask the architect opens, once it has. */
  approval?: string;
}

async function seedLegionJourney(): Promise<LegionJourney> {
  const api = await dispatchApi();
  const { setLiveSessions } = await fakeEnvoy();
  await setLiveSessions(
    (["architect", "implementer"] as const).map((role) => ({
      capabilities: ["aside", "btw", "steer"],
      dir: `/workspaces/shop-1-${role}`,
      machine_id: "example-host-legion",
      roles: [`legion-shop-shop-1-${role}`],
      session_id: `shop-1-${role}`,
      title: `SHOP-1 ${role}`,
    }))
  );
  await api.createProject({ key: "SHOP", name: "Storefront" });
  const issue = await api.createIssue({
    project: "SHOP",
    spec: CART_SPEC,
    title: "Let shoppers save their cart for later",
  });
  // The handover: a human labels the issue `legion` and sets it to todo.
  await api.patchIssue(issue.key, { labels: ["legion"], priority: 1, status: "todo" });
  return { key: issue.key, spec: issue.primary_artifact_id };
}

const conversation = (journey: LegionJourney) => `/issues/${journey.key}/conversation`;
const turns = (page: Page) => page.getByRole("list", { name: "Conversation turns" });

const legion: ShotSet<LegionJourney> = {
  set: "legion",
  seed: seedLegionJourney,
  // The issue has no comment threads on its spec, so its margin says so.
  allowEmpty: ["Margin review empty state"],
  shots: [
    {
      id: "handover",
      alt: "An issue handed to Legion: the label picker open on its header with legion selected, and the status Todo.",
      route: (journey) => `/issues/${journey.key}`,
      viewport: "desktop",
      theme: "light",
      ready: async (page) => {
        await expect(page.getByRole("heading", { level: 1 })).toContainText(
          "Let shoppers save their cart for later"
        );
      },
      steps: async (page) => {
        await page.getByRole("button", { name: "Edit labels" }).click();
        await expect(page.getByRole("option", { exact: true, name: "legion" })).toHaveAttribute(
          "aria-selected",
          "true"
        );
      },
    },
    {
      id: "admitted",
      alt: "The issue after admission: status In progress, claimed by the issue's architect, with the claim in its Conversation.",
      route: conversation,
      viewport: "desktop",
      theme: "light",
      prepare: async (journey) => {
        const api = await dispatchApi();
        await api.patchIssue(journey.key, { status: "in_progress" }, DAEMON);
        await api.claimIssue(journey.key, agent("architect"));
      },
      ready: async (page) => {
        await expect(page.getByTestId("issue-header")).toContainText("SHOP-1 architect");
        await expect(turns(page)).toContainText("claimed the issue");
      },
    },
    {
      id: "decision-block",
      alt: "The spec's design with an open decision block: the architect's question, its recommendation, and two options to answer.",
      route: (journey) => `/issues/${journey.key}/spec`,
      viewport: "desktop",
      theme: "light",
      prepare: async (journey) => {
        const api = await dispatchApi();
        await api.editArtifact(
          journey.spec,
          { ops: [{ after: "end", markdown: CART_DESIGN, op: "insert" }] },
          agent("architect")
        );
        await expect.poll(async () => (await api.getIssue(journey.key)).open_asks.length).toBe(1);
      },
      ready: async (page) => {
        const block = page.locator('[data-dispatch-ask-block="cart-storage"]');
        await expect(block.locator("article[data-testid^=ask-]")).toBeVisible();
        await expect(page.getByRole("status", { name: "connected" })).toBeVisible();
      },
      steps: async (page) => {
        await page
          .locator('[data-dispatch-ask-block="cart-storage"]')
          .evaluate((block) => block.scrollIntoView({ block: "center" }));
      },
    },
    {
      id: "spec-approval",
      alt: "The architect's request to approve the spec in the Inbox, with its summary and the Approve and Request changes options.",
      route: "/",
      viewport: "desktop",
      theme: "light",
      prepare: async (journey) => {
        const api = await dispatchApi();
        const [decision] = (await api.getIssue(journey.key)).open_asks;
        const versions = (await api.getArtifact(journey.spec)).versions.length;
        await api.answerAsk(decision.id, { expected_edited_at: null, selected: ["Server"] });
        // The answer is written into the decision block as a new spec version. An approval asked
        // for before that version lands would move to it and wait on the architect to hand it back.
        await expect
          .poll(async () => (await api.getArtifact(journey.spec)).versions.length)
          .toBeGreaterThan(versions);
        const requested = await api.requestApproval(
          journey.spec,
          {
            summary:
              "Saved carts live on the server, one per account, and open on any device; a nightly job removes carts untouched for 30 days, a week after emailing the shopper.",
          },
          agent("architect")
        );
        journey.approval = requested.ask.id;
      },
      ready: async (page, journey) => {
        await expect(page.getByTestId(`ask-${journey.approval}`)).toContainText(
          "Approval requested"
        );
      },
    },
    {
      id: "status-events",
      alt: "The issue's Conversation as the phases run: the daemon's status changes, newest first, with the issue now in Retro and its pull request linked.",
      route: conversation,
      viewport: "desktop",
      theme: "light",
      prepare: async (journey) => {
        const api = await dispatchApi();
        if (journey.approval === undefined) throw new Error("the spec approval was not requested");
        await api.answerAsk(journey.approval, { expected_edited_at: null, selected: ["Approve"] });
        await api.patchIssue(
          journey.key,
          { external_links: [{ url: PULL_REQUEST }] },
          agent("implementer")
        );
        for (const status of ["testing", "needs_review", "retro"]) {
          await api.patchIssue(journey.key, { status }, DAEMON);
        }
      },
      ready: async (page) => {
        await expect(page.getByTestId("issue-header").getByText("#42")).toBeVisible();
        await expect(turns(page)).toContainText("updated the issue");
      },
    },
    {
      id: "pull-request",
      alt: "The issue header with the pull request Legion opened, linked as #42.",
      route: (journey) => `/issues/${journey.key}`,
      viewport: "desktop",
      theme: "light",
      ready: async (page) => {
        await expect(page.getByTestId("issue-header").getByText("#42")).toBeVisible();
      },
      element: (page) => page.getByTestId("issue-header"),
    },
    {
      id: "ready",
      alt: "The READY message on the issue: the pull request, its head and the approved commit, then the outcome and the risk.",
      route: conversation,
      viewport: "desktop",
      theme: "light",
      prepare: async (journey) => {
        const api = await dispatchApi();
        await api.createMessage(
          journey.key,
          {
            body: [
              `READY #42 at 9c41e07 (approved at 9c41e07) for ${journey.key} (${PULL_REQUEST})`,
              "Outcome: a signed-in shopper saves their cart and reopens it from their account on another device.",
              "Not proven / risk: the expiry email was checked in the mail provider's sandbox, not a real inbox.",
            ].join("\n\n"),
          },
          DAEMON
        );
      },
      ready: async (page) => {
        await expect(turns(page)).toContainText("READY #42 at 9c41e07");
      },
    },
    {
      id: "signed-off",
      alt: "The issue closed as Done, with the implementer's production record and the architect's sign-off.",
      route: conversation,
      viewport: "desktop",
      theme: "light",
      prepare: async (journey) => {
        const api = await dispatchApi();
        await api.createComment(
          journey.key,
          {
            body: "Production: on the live storefront, saved a two-item cart as a test shopper, opened it from the account page in a second browser, and saw both items at their quantities. Merge commit 4b7d2a1.",
          },
          agent("implementer")
        );
        await api.createComment(
          journey.key,
          {
            body: "Signed off. All three acceptance criteria are met; review approved 9c41e07 with no open threads; retro is done; merged as 4b7d2a1; the production record above shows the saved cart opening on a second device.",
          },
          agent("architect")
        );
        await api.patchIssue(journey.key, { status: "done" }, DAEMON);
      },
      ready: async (page) => {
        await expect(turns(page)).toContainText("Signed off.");
        await expect(page.getByTestId("issue-header")).toContainText("Done");
      },
    },
  ],
};

export default [dispatch, legion];
