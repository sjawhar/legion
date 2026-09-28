import { expect, test } from "@playwright/test";

import { type FakeSession, getSentMessages, setLiveSessions } from "./agents";
import {
  createAgentMessage,
  publishAgentStreamFrame,
  replyToMessageDelivery,
  setAgentStreamResponder,
} from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

/**
 * The live agent conversation view. The harness runs Dispatch with NATS disabled, so the frames
 * here come from the test hook rather than from a session; what the test proves is everything on
 * this side of the relay — replay, live snapshots applied last-writer-wins, a tool call finding
 * its result, and a human's message going out through the existing delivery.
 */

const planner: FakeSession = {
  capabilities: ["aside", "btw", "steer"],
  dir: "/workspaces/planner",
  last_seen: Date.now() - 5_000,
  machine_id: "planner-host",
  roles: [],
  session_id: "01a0e090-4848-7473-acc5-fc96e6a646d3",
  title: "Planner",
};

const TOOL_CALL_ID = "call-1";

/** The session's replay: a human's prompt, and the assistant turn that answered it. */
const replay = {
  frames: [
    {
      kind: "message",
      message: {
        at: 1_000,
        id: "u1000",
        parts: [{ text: "Read the release notes", type: "text" }],
        role: "user",
        streaming: false,
      },
      seq: 1,
      v: 1,
    },
    {
      kind: "message",
      message: {
        at: 2_000,
        id: "a2000",
        parts: [
          { text: "Checking the notes.", type: "text" },
          {
            argsText: '{\n  "path": "CHANGELOG.md"\n}',
            toolCallId: TOOL_CALL_ID,
            toolName: "read",
            type: "tool-call",
          },
        ],
        role: "assistant",
        streaming: false,
      },
      seq: 2,
      v: 1,
    },
    {
      kind: "tool-result",
      result: {
        at: 2_500,
        isError: false,
        output: "## Unreleased\n- the relay",
        toolCallId: TOOL_CALL_ID,
        toolName: "read",
      },
      seq: 3,
      v: 1,
    },
  ],
  session_id: planner.session_id,
  v: 1,
};

function assistantFrame(seq: number, text: string, streaming: boolean): object {
  return {
    kind: "message",
    message: {
      at: 3_000,
      id: "a3000",
      parts: [{ text, type: "text" }],
      role: "assistant",
      streaming,
    },
    seq,
    v: 1,
  };
}

test.beforeEach(async () => {
  await Promise.all([resetDatabase(), setLiveSessions([planner])]);
});

test("the conversation view replays what the session held, streams its next turn, and sends an aside through the existing delivery", async ({
  browser,
}) => {
  await publishAgentStreamFrame(planner.session_id, replay, "replay");
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.goto("/agents");
    // A session Dispatch has never heard from sits in a collapsed fold.
    await page.getByRole("button", { name: /No Dispatch activity/ }).click();
    await page.getByRole("link", { name: "Open" }).first().click();
    await expect(page).toHaveURL(new RegExp(`/agents/${planner.session_id}/live$`));

    // What the session replayed: the human's prompt, the reply, and the tool call with its
    // result folded away until asked for.
    await expect(page.getByTestId("agent-message-user")).toContainText("Read the release notes");
    await expect(page.getByTestId("agent-message-assistant")).toContainText("Checking the notes.");
    const tool = page.getByTestId("agent-tool-call");
    await expect(tool).toContainText("read");
    await expect(page.getByTestId("agent-tool-result")).toHaveCount(0);
    await tool.getByRole("button").click();
    await expect(page.getByTestId("agent-tool-result")).toContainText("- the relay");

    // The next turn arrives as snapshots of one message. The host delivers a message's last few
    // updates AFTER its end, so the late streaming snapshot carries the HIGHER sequence number;
    // it must not rewind the settled text or leave the turn looking like it is still running,
    // which is what disables the composer.
    await publishAgentStreamFrame(planner.session_id, assistantFrame(4, "Ther", true));
    await expect(page.getByTestId("agent-message-assistant").last()).toContainText("Ther");
    await publishAgentStreamFrame(planner.session_id, assistantFrame(5, "There are two.", false));
    await expect(page.getByTestId("agent-message-assistant").last()).toContainText(
      "There are two."
    );
    await publishAgentStreamFrame(planner.session_id, assistantFrame(6, "There are", true));
    await expect(page.getByTestId("agent-message-assistant")).toHaveCount(2);
    await expect(page.getByTestId("agent-message-assistant").last()).toContainText(
      "There are two."
    );

    // Talking to the agent is the delivery the Agents page already uses, not a new write path.
    // Send being usable here is also what the late-snapshot rule above buys: a thread left
    // looking like it is still running disables the composer, and a viewer who opens a finished
    // session cannot talk to it.
    await page.getByTestId("agent-composer").locator("textarea").fill("try the other branch");
    await expect(page.getByRole("button", { name: "Send" })).toBeEnabled();
    await page.getByRole("button", { name: "Send" }).click();
    await expect
      .poll(async () => (await getSentMessages()).map((sent) => sent.message))
      .toContain("try the other branch");
  } finally {
    await context.close();
  }
});

// A session answers a human's direct message through Dispatch, which the relayed stream shows
// only as a tool call. The replies are news the human did not go looking for, so they are
// counted unread in the navigation, and the live view shows them - which is reading them.
test("a session's replies to a direct message are unread until the live view shows them", async ({
  browser,
}) => {
  const asked = await createAgentMessage(planner.session_id, {
    body: "Where is the dashboard?",
    delivery: "aside",
  });
  const session = { id: planner.session_id, kind: "session" as const };
  await replyToMessageDelivery(asked.id, { attempt: 1, body: "Switching to it now." }, session);
  // A follow-up, as dispatch_message sends it.
  await replyToMessageDelivery(asked.id, { attempt: 1, body: "Done: it is at /dash." }, session, {
    followUp: true,
  });
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.goto("/agents");
    await expect(page.getByText("New replies 2").first()).toBeVisible();

    await page.goto(`/agents/${planner.session_id}/live`);
    await expect(page.getByTestId("agent-thread")).toContainText("Where is the dashboard?");
    const replies = page.getByTestId("agent-dispatch-reply");
    await expect(replies).toHaveCount(2);
    await expect(replies.first()).toContainText("Switching to it now.");
    await expect(replies.last()).toContainText("Done: it is at /dash.");
    await expect(page.getByText(/^New repl/)).toHaveCount(0);
    await page.reload();
    await expect(page.getByTestId("agent-dispatch-reply")).toHaveCount(2);
    await expect(page.getByText(/^New repl/)).toHaveCount(0);
  } finally {
    await context.close();
  }
});

test("a session that has published nothing renders as empty, not as a conversation", async ({
  browser,
}) => {
  // A session of its own: the relay's frames are process-global, so a session another test
  // seeded would carry that test's replay into this one. It answers the relay with no history,
  // which is what a live session that has not spoken yet is.
  const sessionID = "01a0e0c1-0000-7000-8000-00000000beef";
  await setAgentStreamResponder(sessionID, true);
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.goto(`/agents/${sessionID}/live`);
    await expect(page.getByTestId("agent-conversation")).toBeVisible();
    await expect(page.getByTestId("agent-thread-empty")).toHaveText(
      "Nothing yet. This session's next turn appears here as it happens."
    );
    await expect(page.getByText(/relayed while this page is open/)).toBeVisible();
  } finally {
    await context.close();
  }
});

test("a session nobody answers for is told so, and recovers when a responder appears", async ({
  browser,
}) => {
  // Nobody on the session's control subject: a plugin that predates the live view, or a session
  // that is no longer running. The relay cannot tell those apart and does not claim to.
  const sessionID = "01a0e0c1-0000-7000-8000-00000000cafe";
  await setAgentStreamResponder(sessionID, false);

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.goto(`/agents/${sessionID}/live`);
    const empty = page.getByTestId("agent-thread-empty");
    await expect(empty).toHaveText(/not answering the live view/);
    await expect(empty).toContainText("no longer running");
    // Delivery never went through the stream, so the composer is live either way.
    await expect(page.getByTestId("agent-composer")).toBeVisible();

    // The session restarts onto a plugin that streams. The relay re-asks on every watch tick
    // (AGENT_STREAM_WATCH_INTERVAL_MS), so this page recovers with no reload and no reconnect.
    await publishAgentStreamFrame(
      sessionID,
      {
        frames: [
          {
            kind: "message",
            message: {
              at: 5_000,
              id: "u5000",
              parts: [{ text: "Back on a newer plugin", type: "text" }],
              role: "user",
              streaming: false,
            },
            seq: 1,
            v: 1,
          },
        ],
        session_id: sessionID,
        v: 1,
      },
      "replay"
    );
    await expect(page.getByText("Back on a newer plugin")).toBeVisible({ timeout: 20_000 });
    await expect(empty).toHaveCount(0);
  } finally {
    await context.close();
  }
});
