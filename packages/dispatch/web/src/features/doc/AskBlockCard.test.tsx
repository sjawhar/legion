import { afterEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, waitFor, within } from "@testing-library/react";
import { type Node as ProseMirrorNode, Schema } from "prosemirror-model";
import { MemoryRouter } from "react-router-dom";

import { commentDeliveryFields } from "../../__tests__/comment-fixture";
import { api } from "../../api/client";
import type { Ask, AskRead, Comment } from "../../api/types";
import { AskBlockCard } from "./AskBlockCard";
import type { AskBlockHost } from "./ask-block";

const schema = new Schema({
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

function askNode(
  attrs: Record<string, unknown>,
  options: string[] = ["Ship: Release it", "Hold"]
): ProseMirrorNode {
  return schema.node("ask", { blockId: "b-1", ...attrs }, [
    schema.node("paragraph", undefined, [schema.text("Should we ship?")]),
    ...(options.length === 0
      ? []
      : [
          schema.node(
            "bullet_list",
            undefined,
            options.map((option) =>
              schema.node("list_item", undefined, [
                schema.node("paragraph", undefined, [schema.text(option)]),
              ])
            )
          ),
        ]),
  ]);
}

const ask: Ask = {
  anchor: null,
  answer: null,
  author: {
    id: "01a086ad",
    kind: "session",
    origin: { session_title: "Architect for CORE-1" },
    owner: "sjawhar",
  },
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

const answeredAsk: Ask = {
  ...ask,
  answer: {
    at: new Date(Date.now() - 2 * 60 * 60_000).toISOString(),
    selected: ["Ship"],
    text: "Ship it after the fix lands.",
    user: "bob",
  },
  state: "answered",
};

function reply(body: string): Comment {
  return {
    anchor: null,
    ask_id: ask.id,
    author: { id: "alice", kind: "user" },
    body,
    created_at: new Date().toISOString(),
    edited_at: null,
    id: `comment-${body.length}`,
    issue_key: ask.issue_key,
    reply_to: null,
    resolved: false,
    resolved_at: null,
    resolved_by: null,
    suggestion: null,
    turn: "agent",
    ...commentDeliveryFields(),
  };
}

function threadRead(of: Ask, replies: Comment[] = []): AskRead {
  return { ask: of, edits: [], followers: [], replies };
}

// The block reaches the API only through the shared card; each test says what the server answers.
const answerAsk = spyOn(api, "answerAsk");
const getAsk = spyOn(api, "getAsk");
const createComment = spyOn(api, "createComment");
const createArtifactComment = spyOn(api, "createArtifactComment");

afterEach(() => {
  answerAsk.mockReset();
  getAsk.mockReset();
  createComment.mockReset();
  createArtifactComment.mockReset();
});

function renderCard({
  ask: indexed = ask,
  indexed: hasIndexedAsk = true,
  node,
  onAnswered = () => {},
  owner,
  readOnly = false,
  thread = threadRead(indexed),
}: {
  ask?: Ask;
  /** Render as the server would before it has indexed the block: no ask row at all. */
  indexed?: boolean;
  node: ProseMirrorNode;
  onAnswered?: (id: string) => void;
  owner?: { project: string; slug: string };
  readOnly?: boolean;
  /** What `GET /asks/{id}` answers for the block's ask. */
  thread?: AskRead;
}) {
  getAsk.mockResolvedValue(thread);
  const section = document.createElement("section");
  const header = document.createElement("div");
  const footer = document.createElement("div");
  section.append(header, footer);
  document.body.append(section);
  const host: AskBlockHost = { footer, header, key: 1, node };
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <AskBlockCard
          ask={hasIndexedAsk ? indexed : undefined}
          host={host}
          onAnswered={onAnswered}
          owner={owner}
          readOnly={readOnly}
        />
      </QueryClientProvider>
    </MemoryRouter>
  );
  const scoped = within(footer);
  return {
    header: within(header),
    footer: scoped,
    section,
    /** The compact card's free-text disclosure: opens the composer and returns its field. */
    openComposer() {
      fireEvent.click(
        scoped.getByRole("button", { name: "Add a note or answer in your own words" })
      );
      return scoped.getByLabelText("Your answer") as HTMLTextAreaElement;
    },
    recordRows() {
      return scoped
        .getAllByRole("listitem")
        .map((row) => [(row as HTMLElement).dataset.selected, row.textContent]);
    },
    unmount() {
      view.unmount();
      section.remove();
    },
  };
}

test.each([
  ["blocking", "Blocking"],
  ["high", "High"],
  ["med", "Medium"],
  ["low", "Low"],
] as const)("an open %s decision names its urgency in the block header and hosts the shared ask card, which names who asked", async (urgency, label) => {
  const card = renderCard({ ask: { ...ask, urgency }, node: askNode({ urgency }) });
  try {
    const pill = card.section.querySelector("[data-dispatch-ask-pill]");
    expect(pill?.textContent).toBe(label);
    expect(pill?.getAttribute("data-dispatch-ask-pill")).toBe(urgency);
    expect(card.header.getByText("Decision")).toBeDefined();
    // The card, not the header, says who asked and when — once, not twice.
    expect(card.section.querySelector("[data-dispatch-ask-asked]")).toBeNull();
    const hosted = card.footer.getByRole("article", { name: `Urgency: ${label}` });
    expect(hosted.dataset.testid).toBe("ask-ask-1");
    expect(within(hosted).getByText("Architect for CORE-1 (for sjawhar)")).toBeDefined();
    await waitFor(() =>
      expect(within(hosted).getByText("3 minutes ago", { exact: false })).toBeDefined()
    );
    // The editor's content is the question; the card does not repeat it.
    expect(within(hosted).queryByText("Should we ship?")).toBeNull();
    // The reference copy control lives in the card's metadata line, once.
    expect(card.section.querySelectorAll('[aria-label^="Copy reference"]')).toHaveLength(1);
    expect(within(hosted).getByRole("button", { name: /^Copy reference/ })).toBeDefined();
  } finally {
    card.unmount();
  }
});

test("the hosted card offers the options as the shared choice rows, plus Other, and answers through the ask route", async () => {
  answerAsk.mockImplementation(async (_id, input) => ({
    ...ask,
    answer: {
      at: new Date().toISOString(),
      selected: input.selected,
      text: input.text ?? "",
      user: "alice",
    },
    state: "answered",
  }));
  const answered: string[] = [];
  const card = renderCard({
    node: askNode({ urgency: "med" }),
    onAnswered: (id) => answered.push(id),
  });
  try {
    // Option labels render through MarkdownBody, asynchronously.
    const ship = (await card.footer.findByRole("radio", {
      name: "Ship Release it",
    })) as HTMLInputElement;
    expect(card.footer.getByRole("radio", { name: "Hold" })).toBeDefined();
    expect(card.footer.getByRole("radio", { name: "Other" })).toBeDefined();
    // Nothing chosen: nothing to submit yet.
    expect(card.footer.queryByRole("button", { name: "Answer" })).toBeNull();
    fireEvent.click(ship);
    expect(ship.checked).toBe(true);
    const submit = card.footer.getByRole("button", { name: "Answer" });
    expect((submit as HTMLButtonElement).disabled).toBe(false);
    fireEvent.click(submit);
    await waitFor(() => expect(answerAsk).toHaveBeenCalledTimes(1));
    expect(answerAsk.mock.calls[0]).toEqual([
      "ask-1",
      { expected_edited_at: null, selected: ["Ship"] },
    ]);
    await waitFor(() => expect(answered).toEqual(["ask-1"]));
    // The recorded choice shows at once, before the document round-trips the answer.
    await waitFor(() => expect(card.section.querySelector("form")).toBeNull());
    await waitFor(() =>
      expect(card.recordRows()).toEqual([
        ["true", "✓ShipRelease it"],
        ["false", "Hold"],
      ])
    );
    expect(card.footer.getByText("Answered by", { exact: false })).toBeDefined();
  } finally {
    card.unmount();
  }
});

test("Other opens the composer; the words are the answer and no option is recorded", async () => {
  answerAsk.mockResolvedValue({ ...ask, state: "answered" });
  const card = renderCard({ node: askNode({ urgency: "med" }) });
  try {
    fireEvent.click(await card.footer.findByRole("radio", { name: "Other" }));
    const field = card.footer.getByLabelText("Your answer");
    const submit = card.footer.getByRole("button", { name: "Answer" });
    expect((submit as HTMLButtonElement).disabled).toBe(true);
    fireEvent.change(field, { target: { value: "Neither, actually." } });
    expect((submit as HTMLButtonElement).disabled).toBe(false);
    fireEvent.click(submit);
    await waitFor(() => expect(answerAsk).toHaveBeenCalledTimes(1));
    expect(answerAsk.mock.calls[0]).toEqual([
      "ask-1",
      { expected_edited_at: null, selected: [], text: "Neither, actually." },
    ]);
  } finally {
    card.unmount();
  }
});

test("with multiple choice, Other sits beside the options and both are sent", async () => {
  answerAsk.mockResolvedValue({ ...ask, multiple: true, state: "answered" });
  const card = renderCard({
    ask: { ...ask, multiple: true },
    node: askNode({ multiple: true, urgency: "high" }),
  });
  try {
    const ship = (await card.footer.findByRole("checkbox", {
      name: "Ship Release it",
    })) as HTMLInputElement;
    fireEvent.click(ship);
    fireEvent.click(card.footer.getByRole("checkbox", { name: "Other" }));
    expect(ship.checked).toBe(true);
    const submit = card.footer.getByRole("button", { name: "Answer" });
    // Other still needs words, even with an option ticked beside it.
    expect((submit as HTMLButtonElement).disabled).toBe(true);
    fireEvent.change(card.footer.getByLabelText("Your answer"), {
      target: { value: "And a canary first." },
    });
    expect((submit as HTMLButtonElement).disabled).toBe(false);
    fireEvent.click(submit);
    await waitFor(() => expect(answerAsk).toHaveBeenCalledTimes(1));
    expect(answerAsk.mock.calls[0]).toEqual([
      "ask-1",
      { expected_edited_at: null, selected: ["Ship"], text: "And a canary first." },
    ]);
  } finally {
    card.unmount();
  }
});

test("a decision with no options answers in words alone", async () => {
  answerAsk.mockResolvedValue({ ...ask, options: [], state: "answered" });
  const card = renderCard({
    ask: { ...ask, options: [] },
    node: askNode({ urgency: "low" }, []),
  });
  try {
    expect(card.footer.queryByRole("radio")).toBeNull();
    const field = card.openComposer();
    const submit = card.footer.getByRole("button", { name: "Answer" });
    expect((submit as HTMLButtonElement).disabled).toBe(true);
    fireEvent.change(field, { target: { value: " Go. " } });
    expect((submit as HTMLButtonElement).disabled).toBe(false);
    fireEvent.submit(card.section.querySelector("form") as HTMLFormElement);
    await waitFor(() => expect(answerAsk).toHaveBeenCalledTimes(1));
    expect(answerAsk.mock.calls[0]).toEqual([
      "ask-1",
      { expected_edited_at: null, selected: [], text: "Go." },
    ]);
  } finally {
    card.unmount();
  }
});

test("until the server has indexed the block into an ask, the block shows its options with nothing to answer", async () => {
  const card = renderCard({ indexed: false, node: askNode({ urgency: "med" }) });
  try {
    expect(card.section.querySelector("[data-dispatch-ask-asked]")).toBeNull();
    expect(card.section.querySelector("form")).toBeNull();
    expect(card.footer.queryAllByRole("button")).toHaveLength(0);
    await waitFor(() =>
      expect(card.recordRows().map(([selected]) => selected)).toEqual(["false", "false"])
    );
    expect(getAsk).not.toHaveBeenCalled();
  } finally {
    card.unmount();
  }
});

test("Ask back on an issue document's decision posts a clarification on the issue, shows it under the block, and leaves the decision open", async () => {
  const posted = reply("Ship to which environment?");
  createComment.mockResolvedValue(posted);
  const card = renderCard({ node: askNode({ urgency: "high" }) });
  try {
    // No exchange yet: nothing to disclose.
    expect(card.footer.queryByRole("button", { name: /repl/ })).toBeNull();
    const field = card.openComposer();
    const askBack = card.footer.getByRole("button", { name: "Ask back" });
    // Nothing to send yet.
    expect((askBack as HTMLButtonElement).disabled).toBe(true);
    fireEvent.change(field, { target: { value: "Ship to which environment?" } });
    expect((askBack as HTMLButtonElement).disabled).toBe(false);
    getAsk.mockResolvedValue(threadRead(ask, [posted]));
    fireEvent.click(askBack);
    await waitFor(() => expect(createComment).toHaveBeenCalledTimes(1));
    expect(createComment.mock.calls[0]).toEqual([
      "CORE-1",
      { ask_id: "ask-1", body: "Ship to which environment?" },
    ]);
    expect(answerAsk).not.toHaveBeenCalled();
    // The composer clears for the next turn and the exchange shows, folded, under the block.
    await waitFor(() => expect(field.value).toBe(""));
    const disclosure = await card.footer.findByRole("button", { name: "1 reply" });
    expect(disclosure.getAttribute("aria-expanded")).toBe("false");
    fireEvent.click(disclosure);
    await waitFor(() => expect(card.footer.getByText("Ship to which environment?")).toBeDefined());
    // A clarification is not an answer: the composer is still here.
    expect(card.footer.getByRole("button", { name: "Answer" })).toBeDefined();
    expect(card.section.querySelector("form")).not.toBeNull();
  } finally {
    card.unmount();
  }
});

test("Ask back on a project document's decision posts the clarification on the document", async () => {
  const documentAsk: Ask = {
    ...ask,
    artifact_id: "doc-1",
    block_artifact: { id: "doc-1", primary: false, slug: "design" },
    issue_key: null,
  };
  createArtifactComment.mockResolvedValue({
    ...reply("Before or after the freeze?"),
    artifact_id: "doc-1",
    issue_key: null,
  });
  const card = renderCard({
    ask: documentAsk,
    node: askNode({ urgency: "med" }),
    owner: { project: "CORE", slug: "design" },
  });
  try {
    fireEvent.change(card.openComposer(), { target: { value: "Before or after the freeze?" } });
    fireEvent.click(card.footer.getByRole("button", { name: "Ask back" }));
    await waitFor(() => expect(createArtifactComment).toHaveBeenCalledTimes(1));
    expect(createArtifactComment.mock.calls[0]).toEqual([
      "doc-1",
      { ask_id: "ask-1", body: "Before or after the freeze?" },
    ]);
    expect(createComment).not.toHaveBeenCalled();
  } finally {
    card.unmount();
  }
});

test("a question-shaped answer with no option chosen offers Ask back instead, as the Inbox card does", async () => {
  createComment.mockResolvedValue(reply("Which release train?"));
  const card = renderCard({ node: askNode({ urgency: "med" }) });
  try {
    fireEvent.change(card.openComposer(), { target: { value: "Which release train?" } });
    const submit = card.footer.getByRole("button", { name: "Answer" });
    expect((submit as HTMLButtonElement).disabled).toBe(false);
    fireEvent.click(submit);
    const instead = card.footer.getByRole("button", { name: "Ask back instead" });
    expect(document.activeElement).toBe(instead);
    expect(card.footer.getByRole("button", { name: "Answer with it anyway" })).toBeDefined();
    expect(answerAsk).not.toHaveBeenCalled();
    fireEvent.click(instead);
    await waitFor(() => expect(createComment).toHaveBeenCalledTimes(1));
    expect(createComment.mock.calls[0]).toEqual([
      "CORE-1",
      { ask_id: "ask-1", body: "Which release train?" },
    ]);
    expect(answerAsk).not.toHaveBeenCalled();
  } finally {
    card.unmount();
  }
});

test("an existing exchange shows folded under an open decision with its count", async () => {
  const card = renderCard({
    node: askNode({ urgency: "med" }),
    thread: threadRead(ask, [reply("Ship where?"), reply("To staging first.")]),
  });
  try {
    const disclosure = await card.footer.findByRole("button", { name: "2 replies" });
    expect(card.footer.queryByText("Ship where?")).toBeNull();
    fireEvent.click(disclosure);
    expect(card.footer.getByText("Ship where?")).toBeDefined();
    expect(card.footer.getByText("To staging first.")).toBeDefined();
  } finally {
    card.unmount();
  }
});

test("an answered decision hosts the shared record: who answered and when, the chosen option, the note, and its exchange", async () => {
  const card = renderCard({
    ask: answeredAsk,
    node: askNode({ answered_by: "bob", selected: ["Ship"], state: "answered", urgency: "low" }),
    thread: threadRead(answeredAsk, [reply("Ship where?")]),
  });
  try {
    expect(card.section.querySelector("form")).toBeNull();
    expect(card.section.querySelector("[data-dispatch-ask-answered]")).toBeNull();
    const record = card.footer.getByTestId("ask-ask-1");
    expect(record.textContent).toContain("Answered by bob");
    await waitFor(() =>
      expect(card.recordRows()).toEqual([
        ["true", "✓ShipRelease it"],
        ["false", "Hold"],
      ])
    );
    await waitFor(() =>
      expect(within(record).getByText("Ship it after the fix lands.")).toBeDefined()
    );
    expect(await card.footer.findByRole("button", { name: "1 reply" })).toBeDefined();
    // The record has no metadata line, so the block header keeps the reference copy control.
    expect(card.header.getByRole("button", { name: /^Copy reference/ })).toBeDefined();
  } finally {
    card.unmount();
  }
});

test("a hosted block whose ask has no recorded author says when it was asked, without a dangling separator", async () => {
  const card = renderCard({
    ask: { ...ask, author: { id: "", kind: "user" } },
    node: askNode({ urgency: "high" }),
  });
  try {
    const hosted = card.footer.getByRole("article", { name: "Urgency: High" });
    const meta = within(hosted).getByText("asked").parentElement as HTMLElement;
    await waitFor(() => expect(meta.textContent).toContain("3 minutes ago"));
    expect(meta.textContent?.startsWith("asked")).toBe(true);
  } finally {
    card.unmount();
  }
});

test("a reply written in Markdown keeps its paragraphs and list inside the block", async () => {
  const card = renderCard({
    node: askNode({ urgency: "med" }),
    thread: threadRead(ask, [reply("Two things first:\n\n- the branch\n- the tag\n\nThen ship.")]),
  });
  try {
    fireEvent.click(await card.footer.findByRole("button", { name: "1 reply" }));
    const body = await waitFor(() => {
      const found = card.footer.getByText("Then ship.").closest(".dispatch-markdown");
      if (found === null) throw new Error("the reply body is not a MarkdownBody");
      return found;
    });
    expect(Array.from(body.querySelectorAll("li"), (item) => item.textContent)).toEqual([
      "the branch",
      "the tag",
    ]);
    expect(body.querySelectorAll("p")).toHaveLength(2);
  } finally {
    card.unmount();
  }
});

test("a resolved decision hosts the shared resolution record and no composer", () => {
  const resolved: Ask = {
    ...ask,
    resolution: {
      actor: { id: "01a086ad", kind: "session" },
      at: new Date().toISOString(),
      kind: "resolved",
      reason: "No longer needed.",
    },
    state: "resolved",
  };
  const card = renderCard({ ask: resolved, node: askNode({ state: "resolved" }) });
  try {
    expect(card.section.querySelector("form")).toBeNull();
    expect(card.footer.getByTestId("ask-resolution-badge").textContent).toBe("Resolved");
  } finally {
    card.unmount();
  }
});

// Read-only surfaces (a version view, a closed issue) render the block's own attributes: no
// card, so nothing to answer or ask back, and the header carries what the card would have said.

test("a read-only surface shows an open decision's options with nothing to answer or ask back, and names who asked", () => {
  const card = renderCard({ node: askNode({ urgency: "med" }), readOnly: true });
  try {
    expect(card.section.querySelector("form")).toBeNull();
    expect(card.footer.queryByRole("button", { name: "Ask back" })).toBeNull();
    expect(card.footer.queryByRole("button")).toBeNull();
    expect(card.recordRows().map(([selected]) => selected)).toEqual(["false", "false"]);
    expect(card.section.querySelector("[data-dispatch-ask-asked]")?.textContent).toBe(
      "·asked by Architect for CORE-1 (for sjawhar)·3 minutes ago"
    );
    expect(getAsk).not.toHaveBeenCalled();
  } finally {
    card.unmount();
  }
});

test("a read-only block whose ask has no recorded author still says when it was asked", () => {
  const card = renderCard({
    ask: { ...ask, author: { id: "", kind: "user" } },
    node: askNode({ urgency: "med" }),
    readOnly: true,
  });
  try {
    expect(card.section.querySelector("[data-dispatch-ask-asked]")?.textContent).toBe(
      "·asked3 minutes ago"
    );
  } finally {
    card.unmount();
  }
});

test("a read-only answered decision names who answered and when, ticks the chosen option, and shows the note", async () => {
  const card = renderCard({
    node: askNode({
      answer: "Ship it after the fix lands.",
      answered_at: new Date(Date.now() - 2 * 60 * 60_000).toISOString(),
      answered_by: "bob",
      selected: ["Ship"],
      state: "answered",
      urgency: "low",
    }),
    readOnly: true,
  });
  try {
    expect(card.section.querySelector("form")).toBeNull();
    const answered = card.section.querySelector("[data-dispatch-ask-answered]");
    expect(answered?.textContent).toContain("Answered by bob");
    await waitFor(() => expect(answered?.querySelector("time")?.textContent).toBe("2 hours ago"));
    await waitFor(() =>
      expect(card.recordRows()).toEqual([
        ["true", "✓ShipRelease it"],
        ["false", "Hold"],
      ])
    );
    expect(card.section.querySelector("[data-dispatch-ask-answer]")?.textContent).toBe(
      "Ship it after the fix lands."
    );
  } finally {
    card.unmount();
  }
});

test("a read-only Other answer is recorded under an Other label", () => {
  const card = renderCard({
    node: askNode({ answer: "Neither.", answered_by: "bob", selected: [], state: "answered" }),
    readOnly: true,
  });
  try {
    expect(card.footer.getByText("Other")).toBeDefined();
    expect(card.section.querySelector("[data-dispatch-ask-answer]")?.textContent).toBe("Neither.");
  } finally {
    card.unmount();
  }
});

test("a read-only resolved decision shows its state and no form", () => {
  const card = renderCard({ node: askNode({ state: "resolved" }), readOnly: true });
  try {
    expect(card.section.querySelector("form")).toBeNull();
    expect(card.header.getByText("Resolved")).toBeDefined();
  } finally {
    card.unmount();
  }
});

test("a malformed decision says what is wrong and offers no card", () => {
  const card = renderCard({ node: askNode({ urgency: "high" }, ["Ship", ": no label"]) });
  try {
    expect(card.header.getByText("Malformed")).toBeDefined();
    expect(card.header.getByText("Option 2 has no label")).toBeDefined();
    expect(card.section.querySelector("form")).toBeNull();
    expect(card.footer.queryByRole("article")).toBeNull();
    expect(
      card.footer.getByText("Fix the block text; the decision re-activates once it parses.")
    ).toBeDefined();
    expect(getAsk).not.toHaveBeenCalled();
  } finally {
    card.unmount();
  }
});
