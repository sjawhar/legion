import { expect, spyOn, test } from "bun:test";
import { act, fireEvent, waitFor, within } from "@testing-library/react";
import { type Node as ProseMirrorNode, Schema } from "prosemirror-model";

import { commentDeliveryFields } from "../../__tests__/comment-fixture";
import {
  answerTextReadsWithTheSeededText,
  createQueryClient,
  renderProofDocument,
} from "../../__tests__/proof-document";
import { api } from "../../api/client";
import type { Ask } from "../../api/types";

answerTextReadsWithTheSeededText();

// ---------------------------------------------------------------------------------------------
// Decision (`:::ask`) blocks: the wiring from editor node views to the React card and back to
// the answer route. The card itself is covered in AskBlockCard.test.tsx.
// ---------------------------------------------------------------------------------------------

const askSchema = new Schema({
  nodes: {
    doc: { content: "block+" },
    paragraph: { content: "inline*", group: "block" },
    text: { group: "inline" },
    bullet_list: { content: "list_item+", group: "block" },
    list_item: { content: "paragraph+" },
    ask: {
      attrs: {
        answer: { default: undefined },
        answered_at: { default: undefined },
        answered_by: { default: undefined },
        blockId: { default: null },
        invalid: { default: undefined },
        multiple: { default: false },
        selected: { default: undefined },
        state: { default: "open" },
        urgency: { default: "med" },
      },
      content: "paragraph+ bullet_list?",
      group: "block",
    },
  },
});

function askNode(attrs: Record<string, unknown>): ProseMirrorNode {
  return askSchema.node("ask", { blockId: "b-1", ...attrs }, [
    askSchema.node("paragraph", undefined, [askSchema.text("Should we ship?")]),
    askSchema.node("bullet_list", undefined, [
      askSchema.node("list_item", undefined, [
        askSchema.node("paragraph", undefined, [askSchema.text("Ship: Release it")]),
      ]),
      askSchema.node("list_item", undefined, [
        askSchema.node("paragraph", undefined, [askSchema.text("Hold")]),
      ]),
    ]),
  ]);
}

const blockAsk: Ask = {
  anchor: null,
  answer: null,
  author: { id: "01a086ad", kind: "session", origin: { session_title: "Architect for CORE-1" } },
  block_artifact: { id: "artifact-1", primary: true, slug: "spec" },
  block_id: "b-1",
  created_at: new Date(Date.now() - 3 * 60_000).toISOString(),
  edited_at: null,
  id: "ask-1",
  issue_key: "CORE-1",
  kind: "question",
  multiple: false,
  opened_event_id: 1,
  options: [{ description: "Release it", label: "Ship" }, { label: "Hold" }],
  question: "Should we ship?",
  state: "open",
  urgency: "high",
};

test("ProofDocument hands its decision blocks the indexed ask; the hosted card answers through the ask route and the document's reads refresh", async () => {
  const queryClient = createQueryClient();
  queryClient.setQueryData(["asks", "CORE-1"], [blockAsk]);
  queryClient.setQueryData(["artifact", "artifact-1", "text"], { markdown: "stale" });
  const getAsk = spyOn(api, "getAsk").mockResolvedValue({
    ask: blockAsk,
    edits: [],
    followers: [],
    replies: [],
  });
  const answerAsk = spyOn(api, "answerAsk").mockResolvedValue({
    ...blockAsk,
    answer: { at: new Date().toISOString(), selected: ["Ship"], text: "Go.", user: "alice" },
    state: "answered",
  });
  const { editors, sync, view } = renderProofDocument({ queryClient });

  try {
    await sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    const editor = editors[0];
    if (editor === undefined) throw new Error("editor missing");
    // The fake editor records the node views the host installs; constructing the `ask` one is
    // what the real editor does for every ask node in the document.
    const nodeViews = editor.viewProps.nodeViews as Record<
      string,
      (node: ProseMirrorNode) => { dom: HTMLElement; destroy(): void }
    >;
    const construct = nodeViews.ask;
    if (construct === undefined) throw new Error("the ask node view was not installed");
    let askView: { dom: HTMLElement; destroy(): void } | undefined;
    act(() => {
      askView = construct(askNode({ urgency: "high" }));
      if (askView !== undefined) editor.root.append(askView.dom);
    });
    const section = editor.root.querySelector("section[data-dispatch-ask-block]");
    if (section === null) throw new Error("the ask block shell was not mounted");
    const block = within(section as HTMLElement);
    // The block hosts the shared ask card for its indexed row: it names the asker and reads
    // the thread the Inbox card would.
    const hosted = await block.findByRole("article", { name: "Urgency: High" });
    expect(within(hosted).getByText("Architect for CORE-1")).toBeDefined();
    await waitFor(() => expect(getAsk).toHaveBeenCalledWith("ask-1"));
    fireEvent.click(await block.findByRole("radio", { name: "Ship Release it" }));
    fireEvent.click(block.getByRole("button", { name: "Add a note or answer in your own words" }));
    fireEvent.change(block.getByLabelText("Your answer"), { target: { value: "Go." } });
    const getArtifactText = spyOn(api, "getArtifactText");
    const textReadsBeforeAnswer = getArtifactText.mock.calls.length;
    fireEvent.click(block.getByRole("button", { name: "Answer" }));
    await waitFor(() => expect(answerAsk).toHaveBeenCalledTimes(1));
    expect(answerAsk.mock.calls[0]).toEqual([
      "ask-1",
      { expected_edited_at: null, selected: ["Ship"], text: "Go." },
    ]);
    // The server writes the outcome into the block, so the document's own reads go stale and the
    // text is read again.
    await waitFor(() =>
      expect(getArtifactText.mock.calls.length).toBeGreaterThan(textReadsBeforeAnswer)
    );
    act(() => askView?.destroy());
    await waitFor(() => expect(section.querySelector("[data-dispatch-ask-pill]")).toBeNull());
  } finally {
    answerAsk.mockRestore();
    getAsk.mockRestore();
    view.unmount();
  }
});

test("after Ask back on a decision block, the hosted card's turn label follows the refetched issue asks to the agent's turn", async () => {
  const openAsk: Ask = { ...blockAsk, waiting_on: "human" };
  const handedOver: Ask = { ...blockAsk, waiting_on: "agent" };
  const queryClient = createQueryClient();
  queryClient.setQueryData(["asks", "CORE-1"], [openAsk]);
  // The owner's list is what the block reads; after the clarification the server says the
  // asker holds the turn.
  const listIssueAsks = spyOn(api, "listIssueAsks").mockResolvedValue([handedOver]);
  const getAsk = spyOn(api, "getAsk").mockResolvedValue({
    ask: openAsk,
    edits: [],
    followers: [],
    replies: [],
  });
  const createComment = spyOn(api, "createComment").mockResolvedValue({
    anchor: null,
    ask_id: "ask-1",
    author: { id: "alice", kind: "user" },
    body: "Ship where?",
    created_at: new Date().toISOString(),
    ...commentDeliveryFields(),
    edited_at: null,
    id: "comment-1",
    issue_key: "CORE-1",
    reply_to: null,
    resolved: false,
    resolved_at: null,
    resolved_by: null,
    suggestion: null,
    turn: "agent",
  });
  const { editors, sync, view } = renderProofDocument({ queryClient });

  try {
    await sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    const editor = editors[0];
    if (editor === undefined) throw new Error("editor missing");
    const nodeViews = editor.viewProps.nodeViews as Record<
      string,
      (node: ProseMirrorNode) => { dom: HTMLElement; destroy(): void }
    >;
    const construct = nodeViews.ask;
    if (construct === undefined) throw new Error("the ask node view was not installed");
    act(() => {
      const askView = construct(askNode({ urgency: "high" }));
      editor.root.append(askView.dom);
    });
    const section = editor.root.querySelector("section[data-dispatch-ask-block]");
    if (section === null) throw new Error("the ask block shell was not mounted");
    const block = within(section as HTMLElement);
    expect((await block.findByTestId("turn-ask-1")).textContent).toBe("Waiting on you");
    fireEvent.click(block.getByRole("button", { name: "Add a note or answer in your own words" }));
    fireEvent.change(block.getByLabelText("Your answer"), { target: { value: "Ship where?" } });
    fireEvent.click(block.getByRole("button", { name: "Ask back" }));
    await waitFor(() => expect(createComment).toHaveBeenCalledTimes(1));
    // The clarification invalidates the owner's asks; the refetch hands the block the new turn.
    await waitFor(() => expect(listIssueAsks).toHaveBeenCalled());
    await waitFor(() =>
      expect(block.getByTestId("turn-ask-1").textContent).toBe("Waiting on Architect for CORE-1")
    );
  } finally {
    createComment.mockRestore();
    getAsk.mockRestore();
    listIssueAsks.mockRestore();
    view.unmount();
  }
});
