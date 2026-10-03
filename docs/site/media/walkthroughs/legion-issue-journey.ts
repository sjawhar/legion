// Handing an issue to Legion and getting it back merged: a finished issue first, then the next one
// handed over, its questions and spec approval answered in the Inbox, its status following the
// phases to READY, and the operator's terminal. Rebuild it with the command in
// docs/site/media/README.md; the two casts beside it in `legion-issue-journey/` are recorded by
// `legion-issue-journey/record-casts.sh`. Each narration line was written to its measured clip and
// starts at the cue it names, once that moment is on screen.
import type { Page } from "@playwright/test";

import { expect } from "../harness";
import {
  admit,
  DELIVERY_DATE,
  file,
  finish,
  linkPullRequest,
  PHASE_STATUSES,
  postReady,
  readyLine,
  requestSpecApproval,
  SAVED_CART,
  seedProject,
  setStatus,
  specVersionAfter,
  specVersions,
  type Tracked,
  writeDesign,
} from "../legion/journey";
import { highlight, linger, pointTo, ring, type Walkthrough } from "../recording";

interface Seeded {
  /** The issue Legion has already taken to done. */
  readonly done: Tracked;
  /** The issue handed over on camera. */
  readonly next: Tracked;
}

/** A finished issue for the video to open on, and the next one, filed and not yet handed over. */
async function seed(): Promise<Seeded> {
  await seedProject(["SHOP-1", "SHOP-2"]);
  const done = await file(SAVED_CART);
  await finish(done, SAVED_CART);
  const next = await file(DELIVERY_DATE);
  if (done.key !== "SHOP-1" || next.key !== "SHOP-2") {
    throw new Error(`legion-issue-journey: the seed made ${done.key} and ${next.key}`);
  }
  return { done, next };
}

const header = (page: Page) => page.getByTestId("issue-header");
const turns = (page: Page) => page.getByRole("list", { name: "Conversation turns" });
const row = (page: Page, text: string) =>
  page.locator("[data-inbox-row]").filter({ hasText: text });
const DECISION = "Where should the delivery dates come from?";

/** Opens an issue's Conversation with its header, and so its status, at the top of the screen:
 *  the tab opens scrolled to what is new since the viewer last read it. */
async function openConversation(page: Page, issue: Tracked): Promise<void> {
  await page.goto(`/issues/${issue.key}/conversation`);
  await expect(turns(page)).toBeVisible();
  await header(page).evaluate((element) => element.scrollIntoView());
  await expect(header(page)).toBeInViewport({ ratio: 1 });
}

const legionIssueJourney: Walkthrough<Seeded> = {
  title: "Hand an issue to Legion",
  seed,
  pronounce: { READY: "Ready", Todo: "to-do", tmux: "tee-mux" },
  sections: [
    {
      id: "done",
      narration: [
        { at: 0.3, text: "Legion took this issue to done:" },
        { at: "production", text: "checked in production," },
        { at: "signoff", text: "and signed off." },
      ],
      open: async (page, seeded) => {
        await openConversation(page, seeded.done);
        await expect(page.getByRole("combobox", { name: "Status" })).toHaveValue("done");
        await expect(turns(page)).toContainText("Signed off.");
      },
      act: async (page, _seeded, cue) => {
        await linger(page, 0.6);
        await highlight(page, page.getByRole("combobox", { name: "Status" }));
        cue("status");
        await linger(page, 1.2);
        await highlight(page, turns(page).getByText(/^Production:/));
        cue("production");
        await linger(page, 1.2);
        await highlight(page, turns(page).getByText(/^Signed off\./));
        cue("signoff");
        await linger(page, 1.8);
      },
    },
    {
      id: "handover",
      narration: [
        { at: 0.3, text: "To hand Legion an issue," },
        { at: "label", text: "add the legion label" },
        { at: "todo", text: "and set it to Todo." },
        { at: "admitted", text: "Legion admits it and claims it." },
      ],
      open: async (page, seeded) => {
        await page.goto(`/issues/${seeded.next.key}`);
        await expect(page.getByRole("heading", { level: 1 })).toContainText(DELIVERY_DATE.title);
      },
      act: async (page, seeded, cue) => {
        await linger(page, 0.5);
        const labels = page.getByRole("button", { name: "Edit labels" });
        await pointTo(page, labels);
        await labels.click();
        const legion = page.getByRole("option", { exact: true, name: "legion" });
        await pointTo(page, legion);
        cue("label");
        await legion.click();
        await expect(legion).toHaveAttribute("aria-selected", "true");
        await linger(page, 0.4);
        await pointTo(page, labels);
        await labels.click();
        await expect(header(page).getByText("legion", { exact: true })).toBeVisible();
        const status = page.getByRole("combobox", { name: "Status" });
        await pointTo(page, status);
        await status.selectOption("todo");
        cue("todo");
        await expect(status).toHaveValue("todo");
        await linger(page, 1.2);
        // The daemon admits it, and its architect claims it: the open page hears both.
        await admit(seeded.next);
        await expect(status).toHaveValue("in_progress");
        const claim = header(page).getByText(`${seeded.next.key} architect`);
        await expect(claim).toBeVisible();
        cue("admitted");
        await ring(page, claim);
        await linger(page, 2);
      },
    },
    {
      id: "inbox",
      narration: [
        { at: 0.3, text: "The architect's questions come to your Inbox." },
        { at: "pick", text: "Pick an answer," },
        { at: "answered", text: "and once every one is answered," },
        { at: "approval", text: "it asks you to approve the spec." },
        { at: "approve", text: "Approve it," },
        { at: "clear", text: "and it's clear." },
      ],
      // Approving the spec answers the last ask waiting on the viewer.
      allowEmpty: ["Inbox empty state"],
      open: async (page, seeded) => {
        await writeDesign(seeded.next, DELIVERY_DATE);
        await page.goto("/");
        await expect(page.getByRole("heading", { level: 1, name: "Inbox" })).toBeVisible();
        await expect(
          row(page, DECISION).getByRole("button", { exact: true, name: "Answer" })
        ).toBeInViewport({ ratio: 1 });
      },
      act: async (page, seeded, cue) => {
        const before = await specVersions(seeded.next);
        await linger(page, 0.5);
        const decision = row(page, DECISION);
        await highlight(page, decision.getByText(DECISION));
        cue("question");
        await linger(page, 1.2);
        const carrier = decision.getByRole("radio", { name: /^Carrier/ });
        await highlight(page, carrier);
        await carrier.check();
        cue("pick");
        const answer = decision.getByRole("button", { exact: true, name: "Answer" });
        await pointTo(page, answer);
        await answer.click();
        await expect(row(page, DECISION)).toHaveCount(0);
        await ring(page, page.getByText("Nothing needs you"));
        cue("answered");
        // The answer lands in the spec as a new version; the architect then asks for approval.
        await specVersionAfter(seeded.next, before);
        const approval = await requestSpecApproval(seeded.next, DELIVERY_DATE);
        const card = page.getByTestId(`ask-${approval}`);
        await expect(card).toContainText("Approval requested");
        cue("approval");
        await highlight(page, card.getByText(DELIVERY_DATE.approvalSummary, { exact: false }));
        await linger(page, 1.5);
        const approve = card.getByRole("radio", { name: /^Approve/ });
        await pointTo(page, approve);
        await approve.check();
        const send = card.getByRole("button", { exact: true, name: "Answer" });
        await pointTo(page, send);
        cue("approve");
        await send.click();
        await expect(card).toHaveCount(0);
        await highlight(page, page.getByText("Nothing needs you"));
        cue("clear");
        await linger(page, 1.6);
      },
    },
    {
      id: "phases",
      narration: [{ at: "phases", text: "The status follows each phase." }],
      open: async (page, seeded) => {
        await linkPullRequest(seeded.next, DELIVERY_DATE);
        await page.goto(`/issues/${seeded.next.key}`);
        await expect(header(page).getByText(`#${DELIVERY_DATE.pullRequest.number}`)).toBeVisible();
      },
      act: async (page, seeded, cue) => {
        await linger(page, 0.4);
        const pill = page.getByRole("combobox", { name: "Status" });
        await highlight(page, pill);
        // The daemon's status writes as the phases run; the open page hears each one.
        for (const [index, status] of PHASE_STATUSES.entries()) {
          await setStatus(seeded.next, status);
          await expect(pill).toHaveValue(status);
          if (index === 0) cue("phases");
          await linger(page, 1.1);
        }
        await linger(page, 0.4);
      },
    },
    {
      id: "ready",
      narration: [
        { at: "ready", text: "READY names the pull request," },
        { at: "outcome", text: "and what it changes, for your merge." },
      ],
      open: async (page, seeded) => {
        await postReady(seeded.next, DELIVERY_DATE);
        await openConversation(page, seeded.next);
        await expect(turns(page).getByText(readyLine(seeded.next, DELIVERY_DATE))).toBeVisible();
      },
      act: async (page, seeded, cue) => {
        await linger(page, 0.4);
        await highlight(page, turns(page).getByText(readyLine(seeded.next, DELIVERY_DATE)));
        cue("ready");
        await linger(page, 1.2);
        await highlight(page, turns(page).getByText(/^Outcome:/));
        cue("outcome");
        await linger(page, 2.4);
      },
    },
    {
      id: "state",
      cast: "legion-issue-journey/state.cast",
      // From `legion state` to the claims list's answer; `legion status` and the detach are cut.
      window: [3.9, 19.6],
      narration: [
        { at: 2.3, text: "From the terminal, legion state reads the daemon;" },
        { at: 8.3, text: "its JSON shows the architect, ready;" },
        { at: 11.9, text: "legion claims list shows each claim." },
      ],
    },
    {
      id: "controller",
      cast: "legion-issue-journey/controller.cast",
      // Up to the registered controller's locator; the detach is cut.
      window: [0.9, 16],
      narration: [
        { at: 0.4, text: "Then start the controller." },
        { at: 3.2, text: "It checks the controller's Oh My Pi," },
        { at: 5.75, text: "then starts it." },
        { at: 7.6, text: "The controller opens on its start procedure," },
        { at: 12.6, text: "and registers with the daemon." },
      ],
    },
  ],
};

export default legionIssueJourney;
