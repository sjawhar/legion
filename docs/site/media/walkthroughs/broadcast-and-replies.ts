// Broadcasting one message to several agents from the Agents page, and watching their replies
// arrive on the broadcast's page. Rebuild it with the command in docs/site/media/README.md. Each
// narration line was written to its measured clip and starts at the cue it names.
import type { Page } from "@playwright/test";

import { type DispatchWorkspace, dispatchApi, expect, seedDispatchWorkspace } from "../harness";
import { linger, pointTo, type Walkthrough } from "../recording";

const MESSAGE = "Rebase on main before the release cut, and reply when you are done.";
/** The three agents on the build machine, in the order the page lists them and they are ticked,
 *  and how each answers. All three advertise Send. */
const RECIPIENTS = [
  { reply: "Rebased, and fixed two conflicts in the cart spec.", title: "Tester" },
  { reply: "Rebased; the checkout tests still pass.", title: "Planner" },
  { reply: "Nothing to rebase: I only read the logs.", title: "Observer" },
] as const;
/** The workspace's own broadcast, which the Planner has answered, and the other two replies. */
const EARLIER_REPLIES = {
  Observer: "Stood down; nothing was running.",
  Tester: "Stopped after the checkout suite: 41 passed.",
} as const;

interface Seeded extends DispatchWorkspace {
  /** The broadcast this walkthrough sends, once its send section has sent it. */
  sent?: string;
}

const sessionOf = (title: string) => ({
  id: `${title.toLowerCase()}-session`,
  kind: "session" as const,
  origin: { session_title: title },
});

/** The e2e workspace, with every recipient of its broadcast answered, so the video can open on a
 *  finished broadcast. */
async function seed(): Promise<Seeded> {
  const workspace = await seedDispatchWorkspace();
  const api = await dispatchApi();
  const earlier = await api.getBroadcast(workspace.broadcast.id);
  for (const [title, reply] of Object.entries(EARLIER_REPLIES)) {
    const recipient = earlier.recipients.find((one) => one.session_id === sessionOf(title).id);
    if (recipient === undefined) throw new Error(`the workspace's broadcast skipped ${title}`);
    await api.replyToMessageDelivery(
      recipient.message.id,
      { attempt: 1, body: reply },
      sessionOf(title)
    );
  }
  return workspace;
}

const card = (page: Page, title: string) =>
  page.getByRole("region", { name: "Broadcast" }).getByRole("article", { name: title });
const answered = (page: Page, count: number) =>
  page.getByRole("region", { name: "Broadcast" }).getByText(`${count} of 3 answered`);

/** Where the pointer rests at the end of the send section and the start of the replies
 *  section, so the cut between them changes nothing on screen. */
const RESTING = { x: 760, y: 300 } as const;

const broadcastAndReplies: Walkthrough<Seeded> = {
  title: "Broadcast and replies",
  seed,
  sections: [
    {
      id: "result",
      narration: [
        {
          at: 0.4,
          text: "A broadcast sends one message to many agents and gathers their replies.",
        },
      ],
      open: async (page, seeded) => {
        await page.goto(`/agents/broadcasts/${seeded.broadcast.id}`);
        await expect(answered(page, 3)).toBeVisible();
        await expect(card(page, "Tester")).toContainText(EARLIER_REPLIES.Tester);
      },
      act: async (page) => {
        await linger(page, 1.2);
        await pointTo(page, answered(page, 3));
        await linger(page, 1.4);
        await pointTo(page, card(page, "Tester").getByText(EARLIER_REPLIES.Tester));
        await linger(page, 2.6);
      },
    },
    {
      id: "send",
      narration: [
        { at: "filter", text: "Filter the Agents page to the agents you mean," },
        { at: "tick", text: "tick each one," },
        { at: "mode", text: "choose Send," },
        { at: "write", text: "write the message once," },
        { at: "send", text: "and send it to all three." },
      ],
      open: async (page) => {
        await page.goto("/agents");
        await expect(page.getByRole("heading", { level: 1, name: "Agents" })).toBeVisible();
        for (const { title } of RECIPIENTS) {
          await expect(
            page.getByRole("checkbox", { name: `Select ${title} for broadcast` })
          ).toBeVisible();
        }
      },
      act: async (page, seeded, cue) => {
        await linger(page, 0.6);
        const machine = page.getByRole("combobox", { name: "Machine" });
        await pointTo(page, machine);
        await machine.selectOption("build-host");
        cue("filter");
        await expect(page.getByRole("checkbox", { name: /^Select Reviewer/ })).toHaveCount(0);
        await linger(page, 3.2);
        for (const [index, { title }] of RECIPIENTS.entries()) {
          const tick = page.getByRole("checkbox", { name: `Select ${title} for broadcast` });
          await pointTo(page, tick);
          await tick.check();
          if (index === 0) cue("tick");
        }
        const composer = page.getByRole("region", { name: "Broadcast" });
        const mode = composer.getByRole("combobox", { name: "Delivery mode" });
        await pointTo(page, mode);
        await mode.selectOption("steer");
        cue("mode");
        await linger(page, 1.4);
        const message = composer.getByRole("textbox", { name: "Broadcast message" });
        await pointTo(page, message);
        await message.click();
        cue("write");
        await message.pressSequentially(MESSAGE, { delay: 35 });
        await linger(page, 0.4);
        const send = composer.getByRole("button", { name: "Send to 3" });
        await pointTo(page, send);
        await send.click();
        await page.waitForURL(/\/agents\/broadcasts\/[0-9a-f-]+$/);
        cue("send");
        seeded.sent = page.url().split("/").at(-1);
        await expect(page.getByRole("heading", { name: "Broadcast to 3 agents" })).toBeVisible();
        await expect(answered(page, 0)).toBeVisible();
        await page.mouse.move(RESTING.x, RESTING.y, { steps: 8 });
        await page.mouse.move(RESTING.x, RESTING.y);
        await linger(page, 1.6);
      },
    },
    {
      id: "replies",
      narration: [
        { at: 0.4, text: "Each card shows that its message was sent." },
        { at: "first", text: "As each agent answers, its reply appears," },
        { at: "all", text: "until all three are in." },
      ],
      open: async (page, seeded) => {
        if (seeded.sent === undefined) throw new Error("the send section sent no broadcast");
        await page.goto(`/agents/broadcasts/${seeded.sent}`);
        await expect(answered(page, 0)).toBeVisible();
        await page.mouse.move(RESTING.x, RESTING.y);
        await linger(page, 0.6);
      },
      act: async (page, seeded, cue) => {
        if (seeded.sent === undefined) throw new Error("the send section sent no broadcast");
        const api = await dispatchApi();
        const { recipients } = await api.getBroadcast(seeded.sent);
        await pointTo(page, card(page, "Tester").getByText("Sent to Tester (Send)"));
        await linger(page, 2.4);
        for (const [index, { reply, title }] of RECIPIENTS.entries()) {
          const recipient = recipients.find((one) => one.session_id === sessionOf(title).id);
          if (recipient === undefined) throw new Error(`${title} received no copy`);
          await api.replyToMessageDelivery(
            recipient.message.id,
            { attempt: 1, body: reply },
            sessionOf(title)
          );
          // The open page hears the reply on its event stream; nothing reloads it.
          await expect(card(page, title)).toContainText(reply);
          await expect(answered(page, index + 1)).toBeVisible();
          await pointTo(page, card(page, title).getByText(reply));
          if (index === 0) cue("first");
          await linger(page, index === 0 ? 2 : 1.2);
        }
        await pointTo(page, answered(page, 3));
        cue("all");
        await linger(page, 2.2);
      },
    },
  ],
};

export default broadcastAndReplies;
