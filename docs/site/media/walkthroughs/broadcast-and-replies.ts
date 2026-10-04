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

/** Where the pointer rests as the replies section opens. */
const RESTING = { x: 760, y: 300 } as const;

const broadcastAndReplies: Walkthrough<Seeded> = {
  title: "Broadcast and replies",
  seed,
  sections: [
    {
      id: "result",
      narration: [{ at: 0.2, text: "One message out, every reply back." }],
      open: async (page, seeded) => {
        await page.goto(`/agents/broadcasts/${seeded.broadcast.id}`);
        await expect(answered(page, 3)).toBeVisible();
        await expect(card(page, "Tester")).toContainText(EARLIER_REPLIES.Tester);
      },
      act: async (page) => {
        // The pointer goes to the broadcast as the line says one message went out, and to its
        // count of replies as it says they came back; the clip ends just after the line.
        await linger(page, 0.1);
        await pointTo(page, page.getByRole("heading", { name: "Broadcast to 3 agents" }));
        await linger(page, 0.6);
        await pointTo(page, answered(page, 3));
        await linger(page, 1.4);
      },
    },
    {
      id: "send",
      narration: [
        { at: 0.2, text: "On Agents, filter to the ones you mean," },
        { at: "select", text: "select all three," },
        { at: "mode", text: "set the mode to Send," },
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
      act: async (page, _seeded, cue) => {
        // The line names the page as it opens, and the filter as the pointer sets it.
        await linger(page, 0.3);
        const machine = page.getByRole("combobox", { name: "Machine" });
        await pointTo(page, machine);
        await machine.selectOption("build-host");
        const matching = page.getByText("3 matching", { exact: true });
        await expect(matching).toBeVisible();
        // The pointer goes to the filter's count of agents, then to the select-all box as the
        // line ends.
        await linger(page, 0.6);
        await pointTo(page, matching);
        await linger(page, 0.7);
        const all = page.getByRole("checkbox", { name: "Select all matching agents" });
        await pointTo(page, all);
        await all.check();
        cue("select");
        await expect(page.getByText("3 of 3 matching selected", { exact: true })).toBeVisible();
        const composer = page.getByRole("region", { name: "Broadcast" });
        const mode = composer.getByRole("combobox", { name: "Delivery mode" });
        await linger(page, 1.65);
        await pointTo(page, mode);
        await mode.selectOption("steer");
        cue("mode");
        await linger(page, 1.6);
        const message = composer.getByRole("textbox", { name: "Broadcast message" });
        await pointTo(page, message);
        await message.click();
        cue("write");
        await message.pressSequentially(MESSAGE, { delay: 35 });
        cue("send");
        const send = composer.getByRole("button", { name: "Send to 3" });
        await pointTo(page, send);
        // The click lands as the line ends, and ends the clip: the next section opens on the
        // broadcast's page, loaded, rather than on its loading skeleton.
        await linger(page, 1.25);
        await send.click();
      },
      finish: async (page, seeded) => {
        await page.waitForURL(/\/agents\/broadcasts\/[0-9a-f-]+$/);
        seeded.sent = page.url().split("/").at(-1);
        await expect(page.getByRole("heading", { name: "Broadcast to 3 agents" })).toBeVisible();
      },
    },
    {
      id: "replies",
      narration: [
        { at: 0.1, text: "Each card shows it was sent," },
        { at: "first", text: "As each agent answers, its reply appears," },
        { at: "all", text: "until all three are in." },
      ],
      open: async (page, seeded) => {
        if (seeded.sent === undefined) throw new Error("the send section sent no broadcast");
        await page.goto(`/agents/broadcasts/${seeded.sent}`);
        await expect(answered(page, 0)).toBeVisible();
        await expect(card(page, "Tester").getByText("Sent to Tester (Send)")).toBeVisible();
        await page.mouse.move(RESTING.x, RESTING.y);
        await linger(page, 0.6);
      },
      act: async (page, seeded, cue) => {
        if (seeded.sent === undefined) throw new Error("the send section sent no broadcast");
        const api = await dispatchApi();
        const { recipients } = await api.getBroadcast(seeded.sent);
        await pointTo(page, card(page, "Tester").getByText("Sent to Tester (Send)"));
        // The first reply is sent as the line ends.
        await linger(page, 1.35);
        for (const [index, { reply, title }] of RECIPIENTS.entries()) {
          const recipient = recipients.find((one) => one.session_id === sessionOf(title).id);
          if (recipient === undefined) throw new Error(`${title} received no copy`);
          // The line saying the replies appear starts as the first one is sent.
          if (index === 0) cue("first");
          await api.replyToMessageDelivery(
            recipient.message.id,
            { attempt: 1, body: reply },
            sessionOf(title)
          );
          // The open page hears the reply on its event stream; nothing reloads it.
          await expect(card(page, title)).toContainText(reply);
          await expect(answered(page, index + 1)).toBeVisible();
          // The pointer follows each reply but the last, which lands as the line ends.
          if (index < RECIPIENTS.length - 1) {
            await pointTo(page, card(page, title).getByText(reply));
            await linger(page, 0.8);
          }
        }
        // The last reply completes the count the pointer moves to.
        await pointTo(page, answered(page, 3));
        cue("all");
        // The clip ends just after the line.
        await linger(page, 1.85);
      },
    },
  ],
};

export default broadcastAndReplies;
