import { afterEach, beforeEach, expect, type Mock, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, type RenderResult, render, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { commentDeliveryFields } from "../../__tests__/comment-fixture";
import { api } from "../../api/client";
import type { Agent, Ask, AskRead, Comment, CreateCommentInput } from "../../api/types";
import { ANSWER_MENTION_REFUSAL, EMPTY_DRAFT_REFUSAL } from "./AskAnswerArea";
import { AskCard } from "./AskCard";
import { AskReplyComposer } from "./AskThread";
import { ANSWER_REFUSAL } from "./useAskAnswerForm";

/** INFRA-341 (R2, R3, R4): the one answer area under an ask - the open card's box and the reply
 *  box under an answered ask - mounts the Conversation tab's field and `@` picker, stores a
 *  picked session on the comment it posts, and refuses an answer that holds one. */

const planner: Agent = {
  capabilities: ["steer"],
  dir: "/workspaces/planner",
  host: "host-a",
  last_activity: null,
  last_seen: 1_700_000_000_000,
  machine_id: "host-a",
  open_asks: 0,
  roles: [],
  session_id: "A",
  title: "Planner",
};

let whoAmI: Mock<typeof api.whoAmI>;
let getReferences: Mock<typeof api.getReferences>;
let listAgents: Mock<typeof api.listAgents>;

beforeEach(() => {
  whoAmI = spyOn(api, "whoAmI").mockResolvedValue({
    event_head: "0",
    kind: "user",
    login: "alice",
  });
  getReferences = spyOn(api, "getReferences").mockResolvedValue({
    edges: [],
    node: { id: "ask-1", kind: "ask" },
  });
  listAgents = spyOn(api, "listAgents").mockResolvedValue([planner]);
});

afterEach(() => {
  whoAmI.mockRestore();
  getReferences.mockRestore();
  listAgents.mockRestore();
});

function ask(overrides: Partial<Ask> = {}): Ask {
  return {
    anchor: null,
    answer: null,
    author: { id: "session-1", kind: "session" },
    created_at: "2026-10-08T00:00:00Z",
    edited_at: null,
    id: "ask-1",
    issue_key: "CORE-1",
    kind: "question",
    multiple: false,
    opened_event_id: 1,
    options: [],
    question: "Which release path?",
    state: "open",
    urgency: "med",
    waiting_on: "human",
    ...overrides,
  };
}

function thread(input: Ask, replies: Comment[] = []): AskRead {
  return {
    answers: input.answer === null ? [] : [input.answer],
    ask: input,
    claimant: null,
    edits: [],
    followers: [],
    replies,
    route: null,
    route_holder: null,
    route_status: null,
  };
}

function comment(overrides: Partial<Comment> = {}): Comment {
  return {
    anchor: null,
    ask_id: "ask-1",
    author: { id: "alice", kind: "user" },
    body: "A reply",
    created_at: "2026-10-08T00:01:00Z",
    edited_at: null,
    id: "comment-1",
    issue_key: "CORE-1",
    reply_to: null,
    resolved: false,
    resolved_at: null,
    resolved_by: null,
    suggestion: null,
    turn: "agent",
    ...commentDeliveryFields(),
    ...overrides,
  };
}

function renderWithClient(node: ReactNode) {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  return render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>{node}</QueryClientProvider>
    </MemoryRouter>
  );
}

/** Opens the `@` picker in `field` over `text` and picks the one option named `option`, the way
 *  a reader does: the text ends in the `@` the picker stands over. */
async function pickMention(
  view: RenderResult,
  field: HTMLTextAreaElement,
  text: string,
  option: string
): Promise<void> {
  fireEvent.change(field, { target: { value: text } });
  const listbox = await view.findByRole("listbox", { name: "Mention suggestions" });
  fireEvent.click(within(listbox).getByRole("option", { name: option }));
  await waitFor(() => expect(field.value).toBe(`${text}${option}`));
}

/** The line on screen that a refused button is described by. */
function shownReason(button: HTMLElement): string | null {
  const id = (button.getAttribute("aria-describedby") ?? "").split(" ")[0];
  return id === undefined || id === "" ? null : (document.getElementById(id)?.textContent ?? null);
}

test("Ask back posts the picked session as a mention, which the answer refuses to carry", async () => {
  const input = ask();
  const posted: CreateCommentInput[] = [];
  const answered: unknown[] = [];
  const view = renderWithClient(
    <AskCard
      answerAsk={async (_id, answerInput) => {
        answered.push(answerInput);
        return input;
      }}
      ask={input}
      createReply={async (_issueKey, commentInput) => {
        posted.push(commentInput);
        return comment({ body: commentInput.body });
      }}
      getAskThread={async () => thread(input)}
    />
  );

  try {
    const field = (await view.findByLabelText("Your answer")) as HTMLTextAreaElement;
    await pickMention(view, field, "Ask @", "Planner");
    expect(view.getByRole("list", { name: "Mention targets" }).textContent).toContain("session:A");

    // An answer carries no mentions, so Answer refuses this draft - in the tab order, with its
    // reason on screen and as its accessible description.
    const answer = view.getByRole("button", { name: "Answer" });
    expect(answer.getAttribute("aria-disabled")).toBe("true");
    expect(answer.hasAttribute("disabled")).toBe(false);
    expect(answer.getAttribute("title")).toBe(ANSWER_MENTION_REFUSAL);
    expect(shownReason(answer)).toBe(ANSWER_MENTION_REFUSAL);
    fireEvent.click(answer);
    // Ctrl/Cmd+Enter submits the form without passing through the button, and is refused too.
    fireEvent.keyDown(field, { ctrlKey: true, key: "Enter" });
    fireEvent.keyDown(field, { key: "Enter", metaKey: true });
    expect(answered).toEqual([]);

    fireEvent.click(view.getByRole("button", { name: "Ask back" }));
    await waitFor(() => expect(posted).toHaveLength(1));
    expect(posted[0]).toEqual({
      ask_id: "ask-1",
      body: "Ask @Planner",
      mentions: [{ target: "session:A" }],
    });
  } finally {
    view.unmount();
  }
});

test("a draft whose box closes and opens again still posts its picked mention", async () => {
  const input = ask();
  const posted: CreateCommentInput[] = [];
  const card = (
    <AskCard
      ask={input}
      createReply={async (_issueKey, commentInput) => {
        posted.push(commentInput);
        return comment({ body: commentInput.body });
      }}
      getAskThread={async () => thread(input)}
    />
  );
  const first = renderWithClient(card);
  const field = (await first.findByLabelText("Your answer")) as HTMLTextAreaElement;
  await pickMention(first, field, "Ask @", "Planner");
  first.unmount();

  const second = renderWithClient(card);
  try {
    const reopened = (await second.findByLabelText("Your answer")) as HTMLTextAreaElement;
    expect(reopened.value).toBe("Ask @Planner");
    expect(second.getByRole("list", { name: "Mention targets" }).textContent).toContain(
      "session:A"
    );
    fireEvent.click(second.getByRole("button", { name: "Ask back" }));
    await waitFor(() => expect(posted).toHaveLength(1));
    expect(posted[0]?.mentions).toEqual([{ target: "session:A" }]);
  } finally {
    second.unmount();
  }
});

test("the reply box under an answered ask posts the picked session as a mention", async () => {
  const input = ask({
    answer: { at: "2026-10-08T00:02:00Z", selected: [], text: "Ship it.", user: "alice" },
    state: "answered",
  });
  const posted: CreateCommentInput[] = [];
  const view = renderWithClient(
    <AskReplyComposer
      ask={input}
      createReply={async (_issueKey, commentInput) => {
        posted.push(commentInput);
        return comment({ body: commentInput.body });
      }}
    />
  );

  try {
    const field = view.getByLabelText("Reply") as HTMLTextAreaElement;
    const send = view.getByRole("button", { name: "Reply" });
    // An empty box refuses, keeps its place in the tab order, and says why on screen.
    expect(send.getAttribute("aria-disabled")).toBe("true");
    expect(send.hasAttribute("disabled")).toBe(false);
    expect(send.getAttribute("title")).toBe(EMPTY_DRAFT_REFUSAL);
    expect(shownReason(send)).toBe(EMPTY_DRAFT_REFUSAL);
    fireEvent.click(send);
    fireEvent.keyDown(field, { ctrlKey: true, key: "Enter" });
    expect(posted).toEqual([]);

    await pickMention(view, field, "Over to @", "Planner");
    fireEvent.click(send);
    await waitFor(() => expect(posted).toHaveLength(1));
    expect(posted[0]).toEqual({
      ask_id: "ask-1",
      body: "Over to @Planner",
      mentions: [{ target: "session:A" }],
    });
  } finally {
    view.unmount();
  }
});

test("an empty answer area refuses Answer and Ask back, sends nothing, and keeps the focus", async () => {
  const input = ask();
  const answered: unknown[] = [];
  const posted: unknown[] = [];
  const view = renderWithClient(
    <AskCard
      answerAsk={async (_id, answerInput) => {
        answered.push(answerInput);
        return input;
      }}
      ask={input}
      createReply={async (_issueKey, commentInput) => {
        posted.push(commentInput);
        return comment({ body: commentInput.body });
      }}
      getAskThread={async () => thread(input)}
    />
  );

  try {
    await view.findByLabelText("Your answer");
    const answer = view.getByRole("button", { name: "Answer" });
    const askBack = view.getByRole("button", { name: "Ask back" });
    expect(answer.getAttribute("title")).toBe(ANSWER_REFUSAL);
    expect(shownReason(answer)).toBe(ANSWER_REFUSAL);
    expect(askBack.getAttribute("title")).toBe(EMPTY_DRAFT_REFUSAL);
    expect(shownReason(askBack)).toBe(EMPTY_DRAFT_REFUSAL);
    for (const button of [answer, askBack]) {
      button.focus();
      fireEvent.click(button);
      expect(document.activeElement).toBe(button);
    }
    expect(answered).toEqual([]);
    expect(posted).toEqual([]);
  } finally {
    view.unmount();
  }
});

// The draft belongs to the ask, not to the card showing it, so a mention picked in one card
// stands in the other's draft - the one way "Answer with it anyway" is offered over a draft that
// holds a mention, since picking one in the card that offered the choice takes the choice away.
test("Answer with it anyway refuses a draft another card put a mention in", async () => {
  const input = ask();
  const answered: unknown[] = [];
  const view = renderWithClient(
    <>
      <AskCard
        answerAsk={async (_id, answerInput) => {
          answered.push(answerInput);
          return input;
        }}
        ask={input}
        getAskThread={async () => thread(input)}
      />
      <AskCard ask={input} getAskThread={async () => thread(input)} />
    </>
  );

  try {
    const fields = (await view.findAllByLabelText("Your answer")) as HTMLTextAreaElement[];
    const [asking, other] = fields;
    if (asking === undefined || other === undefined) throw new Error("expected two answer boxes");
    fireEvent.change(asking, { target: { value: "How does this fit the release plan?" } });
    fireEvent.click(view.getAllByRole("button", { name: "Answer" })[0] as HTMLElement);
    const choice = await view.findByRole("group", { name: "Question-shaped answer" });
    const anyway = within(choice).getByRole("button", { name: "Answer with it anyway" });
    expect(anyway.hasAttribute("aria-disabled")).toBe(false);

    await pickMention(view, other, "How does this fit the release plan? @", "Planner");
    expect(anyway.getAttribute("aria-disabled")).toBe("true");
    expect(anyway.getAttribute("title")).toBe(ANSWER_MENTION_REFUSAL);
    expect(shownReason(anyway)).toBe(ANSWER_MENTION_REFUSAL);
    fireEvent.click(anyway);
    expect(answered).toEqual([]);
  } finally {
    view.unmount();
  }
});

test("an ask reply carries the copy control for its own reference", async () => {
  const input = ask({
    answer: { at: "2026-10-08T00:02:00Z", selected: [], text: "Ship it.", user: "alice" },
    state: "answered",
  });
  const view = renderWithClient(
    <AskCard ask={input} getAskThread={async () => thread(input, [comment()])} />
  );

  try {
    const replies = await view.findByTestId(`thread-${input.id}`);
    const copy = within(replies).getByRole("button", {
      name: "Copy reference dispatch://CORE-1/comment/comment-1",
    });
    expect(copy.closest("[data-copy-ref]")).not.toBeNull();
  } finally {
    view.unmount();
  }
});
