// Answering an agent's ask from the Inbox, and where the answer lands. Rebuild it with the command
// in docs/site/media/README.md. Each narration line was written to its measured clip and starts
// at the cue it names, once that moment is on screen.
import type { Page } from "@playwright/test";

import { type DispatchWorkspace, dispatchApi, expect, seedDispatchWorkspace } from "../harness";
import { linger, pointTo, scrollBy, type Walkthrough } from "../recording";

const ISSUE_TITLE = "Retry failed webhook deliveries";
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

/** The top bar's count of asks waiting on the viewer. */
async function needsYouCount(page: Page): Promise<number> {
  return Number((await needsYou(page).textContent())?.replace("Needs you ", ""));
}

/** Where the pointer rests at the end of the Inbox section, and where the next section's
 *  pointer starts, so the cut between them changes nothing on screen. */
const RESTING = { x: 900, y: 360 } as const;

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
        { at: "ask-back", text: "Unclear? Ask back instead." },
        { at: "gone", text: "Answered, it leaves your Inbox." },
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
        await pointTo(page, card.getByText("Planner", { exact: true }));
        await linger(page, 2.6);
        cue("asker");
        await linger(page, 2);
        const three = card.getByRole("radio", { name: /^Three times/ });
        await pointTo(page, three);
        cue("pick");
        await three.check();
        const note = card.getByLabel("Your answer");
        await pointTo(page, note);
        await note.click();
        await note.pressSequentially(NOTE, { delay: 40 });
        await linger(page, 0.4);
        await pointTo(page, card.getByRole("button", { exact: true, name: "Ask back" }));
        cue("ask-back");
        await linger(page, 3.4);
        const answer = card.getByRole("button", { exact: true, name: "Answer" });
        await pointTo(page, answer);
        await answer.click();
        // The Inbox holds the row the pointer rests on; moving off it lets it leave.
        await page.mouse.move(RESTING.x, RESTING.y, { steps: 8 });
        await expect(retryRow(page)).toHaveCount(0);
        await expect(needsYou(page)).toHaveText(`Needs you ${countBefore - 1}`);
        // A take's drawn pointer stopped one step short of this move, whose path empties the row
        // under it; one more move at the resting point puts it where the next section starts it.
        await page.mouse.move(RESTING.x, RESTING.y);
        cue("gone");
        await linger(page, 3);
      },
    },
    {
      id: "spec",
      narration: [
        { at: 0.4, text: "The Planner's next question is on the same spec. Open it there." },
        { at: "choice", text: "The decision block keeps the option you picked," },
        { at: "note", text: "your reason," },
        { at: "who", text: "and who answered." },
      ],
      open: async (page) => {
        await page.goto("/");
        await expect(alertRow(page)).toBeVisible();
        await expect(retryRow(page)).toHaveCount(0);
        await page.mouse.move(RESTING.x, RESTING.y);
        await linger(page, 0.6);
      },
      act: async (page, seeded, cue) => {
        const open = alertRow(page).getByRole("link", { name: "Open in document" });
        await pointTo(page, open);
        await linger(page, 1.4);
        await open.click();
        await page.waitForURL(new RegExp(`/issues/${seeded.webhooks}`));
        const block = page.locator('[data-dispatch-ask-block="retry-limit"]');
        await expect(block).toContainText("Answered by alice");
        await linger(page, 1);
        // One visible scroll brings the whole answered block above the review sheet's handle.
        const box = await block.boundingBox();
        const viewport = page.viewportSize();
        if (box === null || viewport === null) throw new Error("the decision is not rendered");
        const below = box.y + box.height - (viewport.height - 100);
        if (below > 0) await scrollBy(page, below);
        const chosen = block.locator('ul[aria-label="Options"] li[data-selected="true"]');
        await pointTo(page, chosen.getByText("Three times", { exact: true }));
        cue("choice");
        await linger(page, 3);
        await pointTo(page, block.getByText(NOTE, { exact: true }));
        cue("note");
        await linger(page, 1.4);
        await pointTo(page, block.getByText("alice", { exact: true }));
        cue("who");
        await linger(page, 2.6);
      },
    },
  ],
};

export default answerAnAsk;
