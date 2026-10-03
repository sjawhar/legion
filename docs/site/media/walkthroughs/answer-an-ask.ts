// Answering an ask from the Inbox, and where the answer lands. Rebuild it with the command in
// docs/site/media/README.md. Each section's narration was written to its measured clip.
import type { Page } from "@playwright/test";

import { type DispatchWorkspace, expect, seedDispatchWorkspace } from "../harness";
import { linger, pointTo, type Walkthrough } from "../recording";

/** The seeded spec's decision, as its Inbox row. */
const signInRow = (page: Page) =>
  page.locator("[data-inbox-row]").filter({ hasText: "Which sign-in path?" });

async function openInbox(page: Page): Promise<void> {
  await page.goto("/");
  await expect(page.getByRole("article", { name: "Urgency: Blocking" })).toBeVisible();
  await expect(signInRow(page)).toBeVisible();
}

const answerAnAsk: Walkthrough<DispatchWorkspace> = {
  title: "Answer an ask",
  seed: seedDispatchWorkspace,
  sections: [
    {
      id: "waiting",
      narration:
        "Your Inbox holds every question agents are waiting on you to answer. This one asks which " +
        "sign-in path to use.",
      open: openInbox,
      act: async (page) => {
        await pointTo(page, page.getByRole("article", { name: "Urgency: Blocking" }));
        await linger(page, 3);
        const row = signInRow(page);
        await pointTo(page, row.getByText("Which sign-in path?"));
        await linger(page, 2.5);
        await pointTo(page, row.getByRole("radio", { name: /^Cookie/ }));
        await linger(page, 1.5);
        await pointTo(page, row.getByRole("radio", { name: /^Header/ }));
        await linger(page, 2);
      },
    },
    {
      id: "answer",
      narration: "Pick an option, add a note, and answer. The answered ask leaves your Inbox.",
      open: async (page) => {
        await openInbox(page);
        await signInRow(page).scrollIntoViewIfNeeded();
      },
      act: async (page) => {
        const row = signInRow(page);
        const cookie = row.getByRole("radio", { name: /^Cookie/ });
        await pointTo(page, cookie);
        await cookie.check();
        await linger(page, 0.6);
        const note = row.getByLabel("Your answer");
        await pointTo(page, note);
        await note.click();
        await note.pressSequentially("Cookie: it is the identity production uses.", { delay: 45 });
        await linger(page, 0.6);
        const answer = row.getByRole("button", { exact: true, name: "Answer" });
        await pointTo(page, answer);
        await answer.click();
        // The Inbox holds the row the pointer rests on; moving off it lets it leave.
        await page.mouse.move(980, 600, { steps: 8 });
        await expect(signInRow(page)).toHaveCount(0);
        await linger(page, 2);
      },
    },
    {
      id: "recorded",
      narration:
        "The answer is written into the issue's spec, inside the decision block: who answered, " +
        "the option they chose, and their note.",
      open: async (page, seeded) => {
        await page.goto(`/issues/${seeded.issues.workflow}/spec`);
        const block = page.locator("[data-dispatch-ask-block]");
        await expect(block).toContainText("Answered by alice");
        await block.scrollIntoViewIfNeeded();
      },
      act: async (page) => {
        const block = page.locator("[data-dispatch-ask-block]");
        await pointTo(page, block.getByText("Answered by alice"));
        await linger(page, 2.5);
        // The block's source list is folded once it is answered; the record shows the choice.
        await pointTo(page, block.locator('ul[aria-label="Options"] li[data-selected="true"]'));
        await linger(page, 2.5);
        await pointTo(
          page,
          block.getByText("it is the identity production uses").filter({ visible: true })
        );
        await linger(page, 3);
      },
    },
  ],
};

export default answerAnAsk;
