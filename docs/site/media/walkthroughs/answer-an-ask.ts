// Answering an agent's ask from the Inbox, and where the answer lands. Rebuild it with the command
// in docs/site/media/README.md. Each narration line was written to its measured clip and starts
// at the cue it names, once that moment is on screen.
import type { Page } from "@playwright/test";

import { type DispatchWorkspace, dispatchApi, expect, seedDispatchWorkspace } from "../harness";
import { highlight, linger, pointTo, ring, scrollBy, type Walkthrough } from "../recording";

const ISSUE_TITLE = "Retry failed webhook deliveries";
/** The key the seed's issue gets, which the narration names as the Inbox shows it. */
const ISSUE_KEY = "CORE-8";
const SPEC =
  `# ${ISSUE_TITLE}\n\n` +
  "When a customer's endpoint answers with an error, the delivery is dropped. Retry it with " +
  "backoff, and tell the endpoint's owner when it keeps failing.\n";
/** The Planner's two decisions, written into the spec one after the other. The Inbox lists the
 *  newer first, so the retry limit is the one answered on camera, and the alert channel is the
 *  next question on the same spec, whose link opens it. */
const ALERT_CHANNEL =
  ':::ask{#alert-channel urgency="med" multiple="false"}\n' +
  "Where should the endpoint's owner hear about failures?\n\n" +
  "- Email: to the address on the account\n" +
  "- Dashboard: a banner on the endpoint's page\n:::\n";
const RETRY_LIMIT =
  ':::ask{#retry-limit urgency="high" multiple="false"}\n' +
  "How many times should a failed delivery be retried?\n\n" +
  "- Three times: over about an hour, then alert the owner\n" +
  "- Until it succeeds: backing off up to a day between tries\n:::\n";
const NOTE = "An hour rides out a deploy or a restart; past that, the owner should know.";

interface Seeded extends DispatchWorkspace {
  readonly webhooks: string;
}

/** The e2e workspace, plus an issue of alice's whose spec the Planner (a live agent session)
 *  writes its two decision blocks into, as an agent's document edit does. */
async function seed(): Promise<Seeded> {
  const workspace = await seedDispatchWorkspace();
  const api = await dispatchApi();
  const planner = {
    actor: { id: "planner-session", kind: "session", origin: { session_title: "Planner" } },
    as: "agent",
  } as const;
  const issue = await api.createIssue({ project: "CORE", spec: SPEC, title: ISSUE_TITLE });
  if (issue.key !== ISSUE_KEY) {
    throw new Error(`answer-an-ask: the narration names ${ISSUE_KEY}, the seed made ${issue.key}`);
  }
  await api.patchIssue(issue.key, { priority: 1, status: "in_progress" });
  for (const [count, block] of [ALERT_CHANNEL, RETRY_LIMIT].entries()) {
    await api.editArtifact(
      issue.primary_artifact_id,
      { ops: [{ after: "end", markdown: block, op: "insert" }] },
      planner
    );
    // Each block becomes an ask once the document service settles the edit; waiting for it
    // before the next edit gives the two asks the order the Inbox lists them in.
    await expect
      .poll(async () => (await api.getIssue(issue.key)).open_asks.length, { timeout: 15_000 })
      .toBe(count + 1);
  }
  const asks = (await api.getIssue(issue.key)).open_asks;
  if (asks.some((ask) => ask.author.kind !== "session" || ask.author.id !== "planner-session")) {
    throw new Error(`answer-an-ask: the decisions are not the Planner's: ${JSON.stringify(asks)}`);
  }
  return { ...workspace, webhooks: issue.key };
}

const row = (page: Page, question: string) =>
  page.locator("[data-inbox-row]").filter({ hasText: question });
const retryRow = (page: Page) => row(page, "How many times should a failed delivery be retried?");
const alertRow = (page: Page) =>
  row(page, "Where should the endpoint's owner hear about failures?");
const needsYou = (page: Page) => page.getByText(/^Needs you \d+$/);
const openInDocument = (page: Page) =>
  alertRow(page).getByRole("link", { name: "Open in document" });
const answeredBlock = (page: Page) => page.locator('[data-dispatch-ask-block="retry-limit"]');

/** The top bar's count of asks waiting on the viewer. */
async function needsYouCount(page: Page): Promise<number> {
  return Number((await needsYou(page).textContent())?.replace("Needs you ", ""));
}

/** Where the pointer moves once the answer is sent: off the row, which the Inbox holds while the
 *  pointer rests on it. The mouse jumps to each resting spot, so it hovers nothing on the way;
 *  the drawn pointer still glides there. */
const RESTING = { x: 900, y: 360 } as const;
/** The spec page's left margin, level with where the Inbox's click left the pointer: nothing
 *  there takes a hover as the page scrolls under it. */
const MARGIN = { x: 6, y: 380 } as const;

const answerAnAsk: Walkthrough<Seeded> = {
  title: "Answer an ask",
  seed,
  sections: [
    {
      id: "inbox",
      narration: [
        { at: 0.4, text: "Your Inbox holds the questions agents are waiting on you to answer." },
        { at: "asker", text: "This one is the Planner's." },
        { at: "pick", text: "Pick an option, and give your reason in a note." },
        { at: "ask-back", text: "Unclear? Ask back." },
        { at: "answer", text: "Answer, and it leaves your Inbox." },
        { at: "open", text: `${ISSUE_KEY}'s spec keeps your answer.` },
        { at: "click", text: "Open it from here." },
      ],
      open: async (page) => {
        await page.goto("/");
        await expect(page.getByRole("heading", { level: 1, name: "Inbox" })).toBeVisible();
        // The payoff is the first frame: the count, the heading and the Planner's whole question.
        await expect(needsYou(page)).toBeInViewport({ ratio: 1 });
        await expect(
          retryRow(page).getByRole("button", { exact: true, name: "Answer" })
        ).toBeInViewport({ ratio: 1 });
      },
      act: async (page, _seeded, cue) => {
        const countBefore = await needsYouCount(page);
        const card = retryRow(page);
        await linger(page, 1.5);
        // The pointer moves to the question as the first line names it, and to its asker as the
        // second does.
        await pointTo(page, card.getByText("How many times should a failed delivery be retried?"));
        await linger(page, 1.7);
        await pointTo(page, card.getByText("Planner", { exact: true }));
        cue("asker");
        await linger(page, 1.45);
        const three = card.getByRole("radio", { name: /^Three times/ });
        await pointTo(page, three);
        cue("pick");
        await three.check();
        const note = card.getByLabel("Your answer");
        await pointTo(page, note);
        await note.click();
        await note.pressSequentially(NOTE, { delay: 40 });
        // The line starts as the typing ends; the pointer reaches Ask back as it is named, and
        // Answer as the line ends. The next line names Answer as it is clicked.
        cue("ask-back");
        await pointTo(page, card.getByRole("button", { exact: true, name: "Ask back" }));
        await linger(page, 1.25);
        const answer = card.getByRole("button", { exact: true, name: "Answer" });
        await pointTo(page, answer);
        cue("answer");
        await answer.click();
        await page.mouse.move(RESTING.x, RESTING.y);
        await expect(retryRow(page)).toHaveCount(0);
        await expect(needsYou(page)).toHaveText(`Needs you ${countBefore - 1}`);
        // The Inbox's own count, one lower, is ringed as the line says the ask left.
        await linger(page, 0.9);
        await ring(page, page.getByText(`Blocked on you: ${countBefore - 1} items`));
        await linger(page, 1.2);
        // The ring moves to the next question's link, which opens the same spec, as its line
        // names the spec; the pointer goes to the link as the next line says to open it, and the
        // click ends the clip, so the next section opens on the loaded spec rather than on its
        // loading skeleton.
        cue("open");
        const open = openInDocument(page);
        await ring(page, open);
        await linger(page, 2.5);
        cue("click");
        await pointTo(page, open);
        await linger(page, 0.6);
        await open.click();
      },
    },
    {
      id: "spec",
      narration: [
        { at: "block", text: "The decision block keeps the option you picked," },
        { at: "note", text: "your reason," },
        { at: "who", text: "and who answered." },
      ],
      open: async (page, seeded) => {
        // The click the Inbox section ended on, and the spec it opens, loaded.
        await page.goto("/");
        await openInDocument(page).click();
        await page.waitForURL(new RegExp(`/issues/${seeded.webhooks}`));
        await expect(answeredBlock(page)).toContainText("Answered by alice");
        await linger(page, 0.6);
      },
      act: async (page, _seeded, cue) => {
        const block = answeredBlock(page);
        // The pointer leaves the content for the margin before the page scrolls under it, so
        // nothing hovers.
        await page.mouse.move(MARGIN.x, MARGIN.y);
        await linger(page, 0.5);
        // One visible scroll brings the whole answered block above the review sheet's handle.
        const box = await block.boundingBox();
        const viewport = page.viewportSize();
        if (box === null || viewport === null) throw new Error("the decision is not rendered");
        const below = box.y + box.height - (viewport.height - 100);
        if (below > 0) await scrollBy(page, below);
        // The ring follows the line: the block, the option, the reason, then who answered. Each
        // next ring starts gliding as the words before it end.
        await highlight(page, block);
        cue("block");
        await linger(page, 0.8);
        await highlight(page, block.locator('ul[aria-label="Options"] li[data-selected="true"]'));
        await linger(page, 1.25);
        await highlight(page, block.getByText(NOTE, { exact: true }));
        cue("note");
        await linger(page, 0.4);
        await highlight(page, block.getByText(/Answered by alice/));
        cue("who");
        // The clip ends just after the line.
        await linger(page, 1);
      },
    },
  ],
};

export default answerAnAsk;
