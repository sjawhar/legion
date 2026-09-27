import { expect, test } from "@playwright/test";

import { type FakeSession, getSentMessages, setLiveSessions } from "./agents";
import { publishAgentStreamFrame } from "./api";
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

test("a session that has published nothing renders as empty, not as a conversation", async ({
  browser,
}) => {
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    // A session of its own: the relay's frames are process-global, so a session another test
    // seeded would carry that test's replay into this one.
    await page.goto("/agents/01a0e0c1-0000-7000-8000-00000000beef/live");
    await expect(page.getByTestId("agent-conversation")).toBeVisible();
    await expect(page.getByText("Nothing yet.")).toBeVisible();
    await expect(page.getByText("Nothing here is stored")).toBeVisible();
  } finally {
    await context.close();
  }
});
