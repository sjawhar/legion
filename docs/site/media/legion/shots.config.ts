// The Legion section's screenshots: one example issue's journey through Legion, as Dispatch shows
// it, written to `docs/site/public/media/legion/<id>.png` and embedded as
// `![<alt>](/legion/media/legion/<id>.png)`. The seed files the issue and hands it to Legion; each
// shot's `prepare` then makes the next move the Legion daemon, its agents or the human makes
// (`journey.ts`), so the shots follow the issue in order, then the controller's daily report.
import type { Page } from "@playwright/test";

import { expect } from "../harness";
import type { ShotSet } from "../shot-runner";
import {
  admit,
  answerDecision,
  approve,
  file,
  handOver,
  linkPullRequest,
  PHASE_STATUSES,
  postDailyReport,
  postReady,
  readyLine,
  requestSpecApproval,
  SAVED_CART,
  seedProject,
  setStatus,
  signOff,
  type Tracked,
  writeDesign,
} from "./journey";

interface LegionJourney extends Tracked {
  /** The approval ask the architect opens, once it has. */
  approval?: string;
  /** The project's daily report issue, once the controller has posted to it. */
  report?: string;
}

async function seed(): Promise<LegionJourney> {
  await seedProject(["SHOP-1"]);
  const issue = await file(SAVED_CART);
  await handOver(issue);
  return { ...issue };
}

const conversation = (journey: LegionJourney) => `/issues/${journey.key}/conversation`;
const turns = (page: Page) => page.getByRole("list", { name: "Conversation turns" });
/** The Conversation opens scrolled to what is new since the viewer's last read; this brings the
 *  issue's header, with its status, back to the top of the screen above the newest turns. */
async function headerOnTop(page: Page): Promise<void> {
  await page.getByTestId("issue-header").evaluate((header) => header.scrollIntoView());
  await expect(page.getByTestId("issue-header")).toBeInViewport({ ratio: 1 });
}

const legion: ShotSet<LegionJourney> = {
  set: "legion",
  seed,
  // The issues have no comment threads on their specs, so the margin says so.
  allowEmpty: ["Margin review empty state"],
  shots: [
    {
      id: "handover",
      alt: "An issue handed to Legion: the label picker open on its header with legion selected, and the status Todo.",
      route: (journey) => `/issues/${journey.key}`,
      viewport: "desktop",
      theme: "light",
      ready: async (page) => {
        await expect(page.getByRole("heading", { level: 1 })).toContainText(SAVED_CART.title);
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
      prepare: admit,
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
      prepare: (journey) => writeDesign(journey, SAVED_CART),
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
        await answerDecision(journey, SAVED_CART);
        journey.approval = await requestSpecApproval(journey, SAVED_CART);
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
        if (journey.approval === undefined) throw new Error("the spec approval was not requested");
        await approve(journey.approval);
        await linkPullRequest(journey, SAVED_CART);
        for (const status of PHASE_STATUSES) await setStatus(journey, status);
      },
      ready: async (page) => {
        await expect(page.getByTestId("issue-header").getByText("#42")).toBeVisible();
        await expect(turns(page)).toContainText("updated the issue");
      },
      steps: headerOnTop,
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
      prepare: (journey) => postReady(journey, SAVED_CART),
      ready: async (page, journey) => {
        await expect(turns(page)).toContainText(readyLine(journey, SAVED_CART));
      },
    },
    {
      id: "signed-off",
      alt: "The issue closed as Done, with the implementer's production record and the architect's sign-off.",
      route: conversation,
      viewport: "desktop",
      theme: "light",
      prepare: (journey) => signOff(journey, SAVED_CART),
      ready: async (page) => {
        await expect(turns(page)).toContainText("Signed off.");
        await expect(page.getByRole("combobox", { name: "Status" })).toHaveValue("done");
      },
      steps: headerOnTop,
    },
    {
      id: "daily-report",
      alt: "The project's Legion daily report issue, parked in Icebox, with the controller's report for the day: the issue Legion finished and its pull request, nothing running, and the free slots.",
      route: (journey) => `/issues/${journey.report}/conversation`,
      viewport: "desktop",
      theme: "light",
      prepare: async (journey) => {
        journey.report = (await postDailyReport([{ example: SAVED_CART, issue: journey }])).key;
      },
      ready: async (page) => {
        await expect(page.getByRole("heading", { level: 1 })).toContainText("Legion daily report");
        await expect(page.getByRole("combobox", { name: "Status" })).toHaveValue("icebox");
        await expect(turns(page)).toContainText("Finished:");
      },
    },
  ],
};

export default legion;
