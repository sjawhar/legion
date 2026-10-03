import { expect, jest, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { type ReactNode, useState } from "react";
import { MemoryRouter } from "react-router-dom";

import { ApiError, api } from "../../api/client";
import type { Agent, Comment, Message } from "../../api/types";
import type { ComposerOwner } from "./composer-model";
import { MentionComposer, reconcileMentions } from "./MentionComposer";
import { SEND_DEADLINE_MS } from "./send-request";

const planner: Agent = {
  capabilities: ["btw"],
  dir: "/workspaces/planner",
  last_activity: null,
  last_seen: 1_700_000_000_000,
  machine_id: "host-a",
  open_asks: 0,
  roles: ["legion-planner"],
  session_id: "A",
  title: "Planner",
};

const worker: Agent = {
  capabilities: [],
  dir: "/workspaces/worker",
  last_activity: null,
  last_seen: 1_700_000_000_001,
  machine_id: "host-b",
  open_asks: 0,
  roles: [],
  session_id: "B",
  title: "Worker",
};

const createdComment: Comment = {
  anchor: null,
  ask_id: null,
  author: { id: "alice", kind: "user" },
  body: "sent",
  created_at: "2026-09-18T00:00:00Z",
  deliveries: [],
  edited_at: null,
  id: "comment-1",
  issue_key: "CORE-1",
  mentions: [],
  reply_to: null,
  resolved: false,
  resolved_at: null,
  resolved_by: null,
  suggestion: null,
  turn: null,
};

interface ComposerOptions {
  agents?: readonly Agent[];
  anchor?: { artifact: string; mark_id: string; quote: string };
  closed?: boolean;
  edit?: { body: string; id: string };
  kind?: "ask" | "comment" | "suggestion";
  seedMentions?: readonly { target: string; title: string }[];
  onCancelReply?: () => void;
  onKindChange?: (kind: "ask" | "comment" | "suggestion") => string | undefined;
  onSent?: () => void;
  owner?: ComposerOwner;
  replyTo?: {
    author: string;
    excerpt: string;
    id: string;
    parentKind?: "comment" | "message";
    thread?: { delivery: "btw" | "steer"; target: string; title: string };
    to?: string;
  } | null;
}

/** Renders the composer, and hands back `rerender` for a host that changes its props on the live
 *  instance - an Agents row's owner and seed when the reader picks an issue. */
function renderComposer(options: ComposerOptions = {}) {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const composer = (props: ComposerOptions) => (
    <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
      <QueryClientProvider client={queryClient}>
        <MentionComposer
          agents={props.agents}
          anchor={props.anchor}
          closed={props.closed}
          edit={props.edit}
          kind={props.kind}
          seedMentions={props.seedMentions}
          onCancelReply={props.onCancelReply}
          onClose={() => {}}
          onKindChange={props.onKindChange}
          onSent={props.onSent ?? (() => {})}
          owner={props.owner ?? { issueKey: "CORE-1", kind: "issue" }}
          replyTo={
            props.replyTo === null
              ? null
              : props.replyTo === undefined
                ? undefined
                : { ...props.replyTo, parentKind: props.replyTo.parentKind ?? "comment" }
          }
        />
      </QueryClientProvider>
    </MemoryRouter>
  );
  const view = render(composer(options));
  return { queryClient, rerender: (next: ComposerOptions) => view.rerender(composer(next)), view };
}

function holdControls(): HTMLFieldSetElement {
  const controls = screen.getByRole("form", { name: "Comment composer" }).querySelector("fieldset");
  if (!(controls instanceof HTMLFieldSetElement))
    throw new Error("expected composer control fieldset");
  return controls;
}

test("a comment without a surviving mention preserves /btw and omits delivery", async () => {
  const createComment = spyOn(api, "createComment").mockResolvedValue(createdComment);
  const { view } = renderComposer();

  try {
    fireEvent.change(screen.getByLabelText("Comment"), { target: { value: "/btw hello" } });
    fireEvent.click(screen.getByRole("button", { name: "Send" }));

    await waitFor(() =>
      expect(createComment).toHaveBeenCalledWith("CORE-1", { body: "/btw hello" })
    );
  } finally {
    view.unmount();
    createComment.mockRestore();
  }
});

test("a selected mention with /btw posts the canonical target and stripped body", async () => {
  const createComment = spyOn(api, "createComment").mockResolvedValue(createdComment);
  const { view } = renderComposer({ agents: [planner] });

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    fireEvent.change(field, { target: { value: "/btw @" } });
    await screen.findByRole("listbox", { name: "Mention suggestions" });
    fireEvent.click(screen.getByRole("option", { name: "Planner" }));
    await waitFor(() => expect(field.value).toBe("/btw @Planner"));
    expect(screen.getByRole("list", { name: "Mention targets" }).textContent).toContain(
      "session:A"
    );
    fireEvent.click(screen.getByRole("button", { name: "Send" }));

    await waitFor(() =>
      expect(createComment).toHaveBeenCalledWith("CORE-1", {
        body: "@Planner",
        delivery: "btw",
        mentions: [{ target: "session:A" }],
      })
    );
  } finally {
    view.unmount();
    createComment.mockRestore();
  }
});

test("deleting a selected mention restores verbatim body and omits delivery", async () => {
  const createComment = spyOn(api, "createComment").mockResolvedValue(createdComment);
  const { view } = renderComposer({ agents: [planner] });

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    fireEvent.change(field, { target: { value: "/btw @" } });
    await screen.findByRole("listbox", { name: "Mention suggestions" });
    fireEvent.click(screen.getByRole("option", { name: "Planner" }));
    await waitFor(() => expect(field.value).toBe("/btw @Planner"));
    fireEvent.change(field, { target: { value: "/btw " } });
    fireEvent.click(screen.getByRole("button", { name: "Send" }));

    await waitFor(() => expect(createComment).toHaveBeenCalledWith("CORE-1", { body: "/btw " }));
  } finally {
    view.unmount();
    createComment.mockRestore();
  }
});

test("deleting an accepted duplicate mention never delivers the indistinguishable survivor", async () => {
  const createComment = spyOn(api, "createComment").mockResolvedValue(createdComment);
  const { view } = renderComposer({ agents: [planner] });

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    fireEvent.change(field, { target: { value: "/btw @" } });
    await screen.findByRole("option", { name: "Planner" });
    fireEvent.click(screen.getByRole("option", { name: "Planner" }));
    fireEvent.change(field, { target: { value: "/btw @Planner @Planner" } });
    fireEvent.change(field, { target: { value: "/btw @Planner" } });
    fireEvent.click(screen.getByRole("button", { name: "Send" }));

    await waitFor(() =>
      expect(createComment).toHaveBeenCalledWith("CORE-1", { body: "/btw @Planner" })
    );
  } finally {
    view.unmount();
    createComment.mockRestore();
  }
});

test("a session title cannot make its mention text impersonate a different session", async () => {
  const createComment = spyOn(api, "createComment").mockResolvedValue(createdComment);
  const { view } = renderComposer({ agents: [{ ...planner, title: "session:Victim" }] });

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    fireEvent.change(field, { target: { value: "@" } });
    await screen.findByRole("option", { name: "session:Victim" });
    fireEvent.click(screen.getByRole("option", { name: "session:Victim" }));
    await waitFor(() => expect(field.value).toBe("@session A"));
    expect(screen.getByRole("list", { name: "Mention targets" }).textContent).toContain(
      "session:A"
    );
    fireEvent.click(screen.getByRole("button", { name: "Send" }));

    await waitFor(() =>
      expect(createComment).toHaveBeenCalledWith("CORE-1", {
        body: "@session A",
        delivery: "steer",
        mentions: [{ target: "session:A" }],
      })
    );
  } finally {
    view.unmount();
    createComment.mockRestore();
  }
});

test("reopening the same reply restores its canonical prefills after cancel", async () => {
  const ReplyHarness = () => {
    const [replyTo, setReplyTo] = useState<{
      author: string;
      excerpt: string;
      id: string;
      mentions: { target: string; title: string }[];
      parentKind: "comment";
    } | null>(null);
    const target = {
      author: "Planner",
      excerpt: "Question",
      id: "comment-1",
      mentions: [{ target: "role:Planner", title: "Planner" }],
      parentKind: "comment" as const,
    };
    return (
      <>
        <button onClick={() => setReplyTo(target)} type="button">
          Open reply
        </button>
        <MentionComposer
          onCancelReply={() => setReplyTo(null)}
          onClose={() => {}}
          onSent={() => {}}
          owner={{ issueKey: "CORE-1", kind: "issue" }}
          replyTo={replyTo}
        />
      </>
    );
  };
  const view = render(
    <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
      <QueryClientProvider
        client={
          new QueryClient({
            defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
          })
        }
      >
        <ReplyHarness />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    fireEvent.click(screen.getByRole("button", { name: "Open reply" }));
    await waitFor(() => expect(field.value).toBe("@Planner"));
    fireEvent.click(screen.getByRole("button", { name: "Cancel reply" }));
    await waitFor(() => expect(screen.queryByRole("button", { name: "Cancel reply" })).toBeNull());
    fireEvent.change(field, { target: { value: "Draft after cancellation" } });
    fireEvent.click(screen.getByRole("button", { name: "Open reply" }));
    await waitFor(() => expect(field.value).toBe("@Planner"));
  } finally {
    view.unmount();
  }
});

/** Send's refusal as a reader meets it: `aria-disabled`, and the reason on `title` and in the
 *  element `aria-describedby` names. */
interface SendRefusal {
  readonly description: string | null;
  readonly disabled: string | null;
  readonly title: string | null;
}

function sendRefusal(): SendRefusal {
  const send = screen.getByRole("button", { name: "Send" });
  const describedBy = send.getAttribute("aria-describedby");
  return {
    description:
      describedBy === null ? null : (document.getElementById(describedBy)?.textContent ?? null),
    disabled: send.getAttribute("aria-disabled"),
    title: send.getAttribute("title"),
  };
}

/** Presses Send both ways a reader can - the button, and Ctrl+Enter in the box - then waits a
 *  task. TanStack awaits `onMutate` before it calls the mutation function, so an API spy read
 *  straight after the press shows no call whether or not Send refused. */
async function pressSend(field: HTMLElement): Promise<void> {
  fireEvent.click(screen.getByRole("button", { name: "Send" }));
  fireEvent.keyDown(field, { ctrlKey: true, key: "Enter" });
  const { promise, resolve } = Promise.withResolvers<void>();
  setTimeout(resolve, 20);
  await promise;
}

test("a token-only direct-session draft cannot submit an empty message, and Send says why", async () => {
  const createAgentMessage = spyOn(api, "createAgentMessage").mockResolvedValue({} as never);
  const { view } = renderComposer({ owner: { kind: "session", sessionId: "A" } });

  try {
    const field = screen.getByLabelText("Comment");
    for (const command of ["/btw", "/aside"]) {
      fireEvent.change(field, { target: { value: `${command} ` } });
      const reason = `Type the message after ${command}.`;
      expect(sendRefusal()).toEqual({ description: reason, disabled: "true", title: reason });
      await pressSend(field);
      expect(createAgentMessage).not.toHaveBeenCalled();
      expect(screen.queryByRole("button", { name: "Sending…" })).toBeNull();
    }
    fireEvent.change(field, { target: { value: "/btw status?" } });
    expect(sendRefusal()).toEqual({ description: null, disabled: null, title: null });
  } finally {
    view.unmount();
    createAgentMessage.mockRestore();
  }
});

test("a direct-session reply inherits its thread delivery", async () => {
  const createAgentMessage = spyOn(api, "createAgentMessage").mockResolvedValue({} as never);
  const { view } = renderComposer({
    owner: { kind: "session", sessionId: "A" },
    replyTo: {
      author: "Planner",
      excerpt: "Question",
      id: "message-9",
      thread: { delivery: "btw", target: "session:A", title: "Planner" },
    },
  });

  try {
    fireEvent.change(screen.getByLabelText("Comment"), { target: { value: "Follow up" } });
    fireEvent.click(screen.getByRole("button", { name: "Send" }));
    await waitFor(() =>
      expect(createAgentMessage).toHaveBeenCalledWith("A", {
        body: "Follow up",
        delivery: "btw",
        in_reply_to: "message-9",
      })
    );
  } finally {
    view.unmount();
    createAgentMessage.mockRestore();
  }
});

test("a legacy targeted-message reply keeps the message endpoint while a comment reply stays a comment", async () => {
  const createComment = spyOn(api, "createComment").mockResolvedValue(createdComment);
  const createMessage = spyOn(api, "createMessage").mockResolvedValue({} as never);
  const { view } = renderComposer({
    replyTo: {
      author: "Planner",
      excerpt: "Question",
      id: "message-9",
      parentKind: "message",
      thread: { delivery: "btw", target: "session:A", title: "Planner" },
    } as never,
  });

  try {
    fireEvent.change(screen.getByLabelText("Comment"), { target: { value: "Legacy reply" } });
    fireEvent.click(screen.getByRole("button", { name: "Send" }));
    await waitFor(() =>
      expect(createMessage).toHaveBeenCalledWith("CORE-1", {
        body: "Legacy reply",
        delivery: "btw",
        in_reply_to: "message-9",
        target: "session:A",
      })
    );
    expect(createComment).not.toHaveBeenCalled();
  } finally {
    view.unmount();
    createComment.mockRestore();
    createMessage.mockRestore();
  }
});

test("a command overrides a targeted legacy reply's inherited delivery", async () => {
  const createMessage = spyOn(api, "createMessage").mockResolvedValue({} as never);
  const { view } = renderComposer({
    replyTo: {
      author: "Planner",
      excerpt: "Question",
      id: "message-9",
      parentKind: "message",
      thread: { delivery: "btw", target: "session:A", title: "Planner" },
    },
  });

  try {
    fireEvent.change(screen.getByLabelText("Comment"), { target: { value: "/aside hello" } });
    fireEvent.click(screen.getByRole("button", { name: "Send" }));
    await waitFor(() =>
      expect(createMessage).toHaveBeenCalledWith("CORE-1", {
        body: "hello",
        delivery: "aside",
        in_reply_to: "message-9",
        target: "session:A",
      })
    );
  } finally {
    view.unmount();
    createMessage.mockRestore();
  }
});

test("a command on a plain legacy reply stays verbatim and sends no delivery", async () => {
  const createMessage = spyOn(api, "createMessage").mockResolvedValue({} as never);
  const { view } = renderComposer({
    replyTo: {
      author: "Planner",
      excerpt: "Question",
      id: "message-9",
      parentKind: "message",
    },
  });

  try {
    fireEvent.change(screen.getByLabelText("Comment"), { target: { value: "/btw hello" } });
    fireEvent.click(screen.getByRole("button", { name: "Send" }));
    await waitFor(() =>
      expect(createMessage).toHaveBeenCalledWith("CORE-1", {
        body: "/btw hello",
        in_reply_to: "message-9",
      })
    );
  } finally {
    view.unmount();
    createMessage.mockRestore();
  }
});

test("a token-only targeted legacy reply cannot submit, and Send says why", async () => {
  const createMessage = spyOn(api, "createMessage").mockResolvedValue({} as never);
  const { view } = renderComposer({
    replyTo: {
      author: "Planner",
      excerpt: "Question",
      id: "message-9",
      parentKind: "message",
      thread: { delivery: "btw", target: "session:A", title: "Planner" },
    },
  });

  try {
    const field = screen.getByLabelText("Comment");
    fireEvent.change(field, { target: { value: "/btw " } });
    const reason = "Type the message after /btw.";
    expect(sendRefusal()).toEqual({ description: reason, disabled: "true", title: reason });
    await pressSend(field);
    expect(createMessage).not.toHaveBeenCalled();
    expect(screen.queryByRole("button", { name: "Sending…" })).toBeNull();
  } finally {
    view.unmount();
    createMessage.mockRestore();
  }
});

test("a programmatic reference append reuses its current body as the next edit baseline", () => {
  expect(
    reconcileMentions(
      "/btw @Planner x @Worker dispatch://CORE-1",
      "/btw @Planner @Worker dispatch://CORE-1",
      [
        { end: 13, start: 5, target: "session:A", text: "Planner" },
        { end: 23, start: 16, target: "session:B", text: "Worker" },
      ]
    )
  ).toEqual([
    { end: 13, start: 5, target: "session:A", text: "Planner" },
    { end: 21, start: 14, target: "session:B", text: "Worker" },
  ]);
});

test("a reference append retains every visible accepted mention through the next edit", async () => {
  const createComment = spyOn(api, "createComment").mockResolvedValue(createdComment);
  const getIssue = spyOn(api, "getIssue").mockResolvedValue({ project: "CORE" } as never);
  const listIssues = spyOn(api, "listIssues").mockResolvedValue([
    { key: "CORE-1", title: "Core issue" },
  ] as never);
  const { view } = renderComposer({ agents: [planner, worker] });

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    fireEvent.change(field, { target: { value: "/btw @" } });
    await screen.findByRole("option", { name: "Planner" });
    fireEvent.click(screen.getByRole("option", { name: "Planner" }));
    await waitFor(() => expect(field.value).toBe("/btw @Planner"));
    const secondMention = "/btw @Planner x @";
    field.setSelectionRange(secondMention.length, secondMention.length);
    fireEvent.change(field, {
      target: { value: secondMention, selectionStart: secondMention.length },
    });
    await screen.findByRole("option", { name: "Worker" });
    fireEvent.click(screen.getByRole("option", { name: "Worker" }));
    await waitFor(() => expect(field.value).toBe("/btw @Planner x @Worker"));
    fireEvent.keyDown(field, { ctrlKey: true, key: "k" });
    await screen.findByRole("dialog", { name: "Reference picker" });
    fireEvent.click(screen.getByRole("button", { name: "CORE-1: Core issue" }));
    await waitFor(() => expect(field.value).toBe("/btw @Planner x @Worker dispatch://CORE-1"));
    fireEvent.change(field, {
      target: { value: "/btw @Planner @Worker dispatch://CORE-1" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Send" }));

    await waitFor(() =>
      expect(createComment).toHaveBeenCalledWith("CORE-1", {
        body: "@Planner @Worker dispatch://CORE-1",
        delivery: "btw",
        mentions: [{ target: "session:A" }, { target: "session:B" }],
      })
    );
  } finally {
    view.unmount();
    createComment.mockRestore();
    getIssue.mockRestore();
    listIssues.mockRestore();
  }
});

test("opening autocomplete refetches live agents before selecting a canonical target", async () => {
  const listAgents = spyOn(api, "listAgents").mockResolvedValue([planner, worker]);
  const createComment = spyOn(api, "createComment").mockResolvedValue(createdComment);
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  queryClient.setQueryData(["agents"], [planner]);
  const view = render(
    <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
      <QueryClientProvider client={queryClient}>
        <MentionComposer
          onClose={() => {}}
          onSent={() => {}}
          owner={{ issueKey: "CORE-1", kind: "issue" }}
        />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    fireEvent.change(field, { target: { value: "@" } });
    await waitFor(() => expect(listAgents).toHaveBeenCalledTimes(1));
    await screen.findByRole("option", { name: "Worker" });
    fireEvent.click(screen.getByRole("option", { name: "Worker" }));
    act(() =>
      queryClient.setQueryData(["agents"], [planner, { ...worker, title: "Renamed worker" }])
    );
    fireEvent.click(screen.getByRole("button", { name: "Send" }));

    await waitFor(() =>
      expect(createComment).toHaveBeenCalledWith("CORE-1", {
        body: "@Worker",
        delivery: "steer",
        mentions: [{ target: "session:B" }],
      })
    );
  } finally {
    view.unmount();
    listAgents.mockRestore();
    createComment.mockRestore();
  }
});

test("an accepted mention survives its target going offline before Send", async () => {
  // Typing @ refetches the live agents (the test above). That refetch answers what the cache
  // already holds, so Planner goes offline only when this test takes it out of the list.
  const listAgents = spyOn(api, "listAgents").mockResolvedValue([planner]);
  const createComment = spyOn(api, "createComment").mockResolvedValue(createdComment);
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  queryClient.setQueryData(["agents"], [planner]);
  const view = render(
    <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
      <QueryClientProvider client={queryClient}>
        <MentionComposer
          onClose={() => {}}
          onSent={() => {}}
          owner={{ issueKey: "CORE-1", kind: "issue" }}
        />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    fireEvent.change(field, { target: { value: "@" } });
    await waitFor(() => expect(listAgents).toHaveBeenCalledTimes(1));
    await screen.findByRole("option", { name: "Planner" });
    fireEvent.click(screen.getByRole("option", { name: "Planner" }));
    act(() => queryClient.setQueryData(["agents"], []));
    fireEvent.click(screen.getByRole("button", { name: "Send" }));
    await waitFor(() =>
      expect(createComment).toHaveBeenCalledWith("CORE-1", {
        body: "@Planner",
        delivery: "steer",
        mentions: [{ target: "session:A" }],
      })
    );
  } finally {
    view.unmount();
    listAgents.mockRestore();
    createComment.mockRestore();
  }
});

test("two accepted mentions receive one comment-level delivery mode", async () => {
  const createComment = spyOn(api, "createComment").mockResolvedValue(createdComment);
  const { view } = renderComposer({ agents: [planner] });

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    fireEvent.change(field, { target: { value: "/aside @" } });
    await screen.findByRole("option", { name: "Planner" });
    fireEvent.click(screen.getByRole("option", { name: "Planner" }));
    await waitFor(() => expect(field.value).toBe("/aside @Planner"));
    fireEvent.change(field, { target: { value: "/aside @Planner @" } });
    await screen.findByRole("option", { name: "legion-planner" });
    fireEvent.click(screen.getByRole("option", { name: "legion-planner" }));
    fireEvent.click(screen.getByRole("button", { name: "Send" }));

    await waitFor(() =>
      expect(createComment).toHaveBeenCalledWith("CORE-1", {
        body: "@Planner @legion-planner",
        delivery: "aside",
        mentions: [{ target: "session:A" }, { target: "role:legion-planner" }],
      })
    );
  } finally {
    view.unmount();
    createComment.mockRestore();
  }
});

test("/btw in the body and /btwx stay ordinary text despite a mention", async () => {
  const createComment = spyOn(api, "createComment").mockResolvedValue(createdComment);
  const { view } = renderComposer({ agents: [planner] });

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    fireEvent.change(field, { target: { value: "hello /btw @" } });
    await screen.findByRole("option", { name: "Planner" });
    fireEvent.click(screen.getByRole("option", { name: "Planner" }));
    fireEvent.click(screen.getByRole("button", { name: "Send" }));
    await waitFor(() =>
      expect(createComment).toHaveBeenLastCalledWith("CORE-1", {
        body: "hello /btw @Planner",
        delivery: "steer",
        mentions: [{ target: "session:A" }],
      })
    );
    view.unmount();

    const second = renderComposer({ agents: [planner] });
    const secondField = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    fireEvent.change(secondField, { target: { value: "/btwx @" } });
    await screen.findByRole("option", { name: "Planner" });
    fireEvent.click(screen.getByRole("option", { name: "Planner" }));
    fireEvent.click(screen.getByRole("button", { name: "Send" }));
    await waitFor(() =>
      expect(createComment).toHaveBeenLastCalledWith("CORE-1", {
        body: "/btwx @Planner",
        delivery: "steer",
        mentions: [{ target: "session:A" }],
      })
    );
    second.view.unmount();
  } finally {
    createComment.mockRestore();
  }
});

test("editing a comment patches only its body and never creates a delivery", async () => {
  const editComment = spyOn(api, "editComment").mockResolvedValue(createdComment);
  const createComment = spyOn(api, "createComment").mockResolvedValue(createdComment);
  const { view } = renderComposer({ edit: { body: "Original", id: "comment-1" } });

  try {
    fireEvent.change(screen.getByLabelText("Edit comment"), { target: { value: "/btw @Planner" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() =>
      expect(editComment).toHaveBeenCalledWith("comment-1", { body: "/btw @Planner" })
    );
    expect(createComment).not.toHaveBeenCalled();
  } finally {
    view.unmount();
    editComment.mockRestore();
    createComment.mockRestore();
  }
});

test("a failed send keeps its draft and retries", async () => {
  const createComment = spyOn(api, "createComment")
    .mockRejectedValueOnce(new ApiError(500, { error: "boom" }))
    .mockResolvedValueOnce(createdComment);
  const { view } = renderComposer();

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    fireEvent.change(field, { target: { value: "keep me" } });
    fireEvent.keyDown(field, { ctrlKey: true, key: "Enter" });
    expect((await screen.findByRole("alert")).textContent).toContain("Couldn't send — boom");
    expect(field.value).toBe("keep me");
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(field.value).toBe(""));
    expect(createComment).toHaveBeenCalledTimes(2);
  } finally {
    view.unmount();
    createComment.mockRestore();
  }
});

test("Ctrl+Enter sends while Enter, whitespace, and IME composition leave the draft intact", async () => {
  const createComment = spyOn(api, "createComment").mockResolvedValue(createdComment);
  const { view } = renderComposer();

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    fireEvent.change(field, { target: { value: "   " } });
    fireEvent.keyDown(field, { ctrlKey: true, key: "Enter" });
    fireEvent.change(field, { target: { value: "line one" } });
    fireEvent.keyDown(field, { key: "Enter" });
    fireEvent.change(field, { target: { value: "still composing" } });
    fireEvent.keyDown(field, { isComposing: true, key: "Enter" });
    expect(createComment).not.toHaveBeenCalled();
    fireEvent.keyDown(field, { ctrlKey: true, key: "Enter" });
    await waitFor(() => expect(createComment).toHaveBeenCalledTimes(1));
  } finally {
    view.unmount();
    createComment.mockRestore();
  }
});

test("reply mode shows the quote, sends reply_to, and cancels by chip or Escape", async () => {
  const createComment = spyOn(api, "createComment").mockResolvedValue(createdComment);
  let cancelled = 0;
  const { view } = renderComposer({
    onCancelReply: () => {
      cancelled += 1;
    },
    replyTo: {
      author: "Planner",
      excerpt: "Once the build is green.",
      id: "comment-9",
      parentKind: "comment",
      to: "/issues/CORE-1/comments/comment-9",
    },
  });

  try {
    expect(screen.getByRole("link", { name: /Replying to Planner/ }).textContent).toContain(
      "Once the build is green."
    );
    fireEvent.change(screen.getByLabelText("Comment"), { target: { value: "Ship it." } });
    fireEvent.click(screen.getByRole("button", { name: "Send" }));
    await waitFor(() =>
      expect(createComment).toHaveBeenCalledWith("CORE-1", {
        body: "Ship it.",
        reply_to: "comment-9",
      })
    );
    fireEvent.click(screen.getByRole("button", { name: "Cancel reply" }));
    fireEvent.keyDown(screen.getByLabelText("Comment"), { key: "Escape" });
    expect(cancelled).toBe(2);
  } finally {
    view.unmount();
    createComment.mockRestore();
  }
});

test("a direct-session reply warns before sending when the target does not advertise the inherited delivery", () => {
  const { view } = renderComposer({ agents: [worker], owner: { kind: "session", sessionId: "B" } });

  try {
    fireEvent.change(screen.getByLabelText("Comment"), { target: { value: "Ship it." } });
    expect(screen.getByText(/Worker does not advertise Send/)).toBeTruthy();
  } finally {
    view.unmount();
  }
});

// The warning's way out names only a mode the session takes: a Claude Code session advertises
// Aside alone, so offering it /btw would suggest a send that fails the same way.
for (const [name, capabilities, suggestion] of [
  [
    "an aside-only session is offered /aside",
    ["aside"],
    "Prefix with /aside to send it as an Aside instead.",
  ],
  ["a BTW-only session is offered /btw", ["btw"], "Prefix with /btw to send it as a BTW instead."],
  [
    "a session taking both is offered both",
    ["aside", "btw"],
    "Prefix with /btw or /aside to send it as a BTW or an Aside instead.",
  ],
  ["a session taking neither is offered nothing", [], undefined],
] as const) {
  test(`when a session does not advertise Send, ${name}`, () => {
    const session: Agent = { ...worker, capabilities: [...capabilities] };
    const { view } = renderComposer({
      agents: [session],
      owner: { kind: "session", sessionId: "B" },
    });

    try {
      fireEvent.change(screen.getByLabelText("Comment"), { target: { value: "Ship it." } });
      const warning = screen.getByText(/Worker does not advertise Send, so sending it records/);
      if (suggestion === undefined) expect(warning.textContent).not.toContain("Prefix with");
      else expect(warning.textContent).toContain(suggestion);
    } finally {
      view.unmount();
    }
  });
}

test("mentioning two targets that both lack the outbound mode names both in the warning", async () => {
  const { view } = renderComposer({ agents: [planner, worker] });

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    fireEvent.change(field, { target: { value: "@" } });
    await screen.findByRole("option", { name: "Planner" });
    fireEvent.click(screen.getByRole("option", { name: "Planner" }));
    await waitFor(() => expect(field.value).toBe("@Planner"));
    const secondMention = "@Planner x @";
    field.setSelectionRange(secondMention.length, secondMention.length);
    fireEvent.change(field, { target: { value: secondMention } });
    await screen.findByRole("option", { name: "Worker" });
    fireEvent.click(screen.getByRole("option", { name: "Worker" }));
    await waitFor(() => expect(field.value).toBe("@Planner x @Worker"));
    expect(screen.getByText(/Planner and Worker do not advertise Send/)).toBeTruthy();
  } finally {
    view.unmount();
  }
});

const plannerMention = { target: "session:A", title: "Planner" };
const createdMessage: Message = {
  author: { id: "alice", kind: "user" },
  body: "sent",
  created_at: "2026-09-18T00:00:00Z",
  deliveries: [],
  id: "message-1",
  in_reply_to: null,
  issue_key: null,
  target: null,
};

// The seed is the one branch these seven call sites share, so its cases live here rather than in
// one host's e2e: what a mount seeds, what a draft entering a seeded channel gets, what a draft
// that already holds the channel's mention gets, where a mention accepted afterwards lands, and
// what leaves with a channel. Each asserts the wire as well as the field: the body and the
// accepted records disagreeing is exactly the defect these cover.
test("a mount seeds the owner's mention and sends it", async () => {
  const createComment = spyOn(api, "createComment").mockResolvedValue(createdComment);
  const { view } = renderComposer({ seedMentions: [plannerMention] });

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    expect(field.value).toBe("@Planner");
    fireEvent.change(field, { target: { value: "@Planner hello" } });
    fireEvent.click(screen.getByRole("button", { name: "Send" }));

    await waitFor(() =>
      expect(createComment).toHaveBeenCalledWith("CORE-1", {
        body: "@Planner hello",
        delivery: "steer",
        mentions: [{ target: "session:A" }],
      })
    );
  } finally {
    view.unmount();
    createComment.mockRestore();
  }
});

/** Types `@` at the end of the field and accepts `title` from the suggestions, as a reader does. */
async function acceptMention(field: HTMLTextAreaElement, title: string): Promise<void> {
  fireEvent.change(field, { target: { value: `${field.value}@` } });
  await screen.findByRole("listbox", { name: "Mention suggestions" });
  fireEvent.click(screen.getByRole("option", { name: title }));
}

// An Agents row changes the composer's owner and seed when the reader picks an issue, and the
// live draft takes the new seed in place, by the rules a mount applies. The records these start
// from are accepted in an issue comment that seeds nothing, since a direct message offers none.
test("a draft entering a channel that owes a mention is seeded in front of it, records and all", async () => {
  const createComment = spyOn(api, "createComment").mockResolvedValue(createdComment);
  const { rerender, view } = renderComposer({ agents: [planner, worker] });

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    fireEvent.change(field, { target: { value: "hello " } });
    await acceptMention(field, "Worker");
    await waitFor(() => expect(field.value).toBe("hello @Worker"));
    rerender({ agents: [planner, worker], seedMentions: [plannerMention] });
    await waitFor(() => expect(field.value).toBe("@Planner hello @Worker"));
    fireEvent.click(screen.getByRole("button", { name: "Send" }));

    await waitFor(() =>
      expect(createComment).toHaveBeenCalledWith("CORE-1", {
        body: "@Planner hello @Worker",
        delivery: "steer",
        mentions: [{ target: "session:A" }, { target: "session:B" }],
      })
    );
  } finally {
    view.unmount();
    createComment.mockRestore();
  }
});

test("a draft that already holds the channel's mention is not seeded twice, wherever it sits", async () => {
  const createComment = spyOn(api, "createComment").mockResolvedValue(createdComment);
  const { rerender, view } = renderComposer({ agents: [planner, worker] });

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    fireEvent.change(field, { target: { value: "y" } });
    await acceptMention(field, "Planner");
    await waitFor(() => expect(field.value).toBe("y@Planner"));
    fireEvent.change(field, { target: { value: "y@Planner x" } });
    rerender({ agents: [planner, worker], seedMentions: [plannerMention] });
    await waitFor(() => expect(field.value).toBe("y@Planner x"));
    fireEvent.click(screen.getByRole("button", { name: "Send" }));

    await waitFor(() =>
      expect(createComment).toHaveBeenCalledWith("CORE-1", {
        body: "y@Planner x",
        delivery: "steer",
        mentions: [{ target: "session:A" }],
      })
    );
  } finally {
    view.unmount();
    createComment.mockRestore();
  }
});

test("a mention accepted after a reseed lands at its own offset", async () => {
  const createComment = spyOn(api, "createComment").mockResolvedValue(createdComment);
  const direct = { kind: "session" as const, sessionId: "A" };
  const { rerender, view } = renderComposer({ agents: [planner, worker], owner: direct });

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    fireEvent.change(field, { target: { value: "hello" } });
    rerender({ agents: [planner, worker], seedMentions: [plannerMention] });
    await waitFor(() => expect(field.value).toBe("@Planner hello"));
    fireEvent.change(field, { target: { value: "@Planner hello " } });
    await acceptMention(field, "Worker");
    await waitFor(() => expect(field.value).toBe("@Planner hello @Worker"));
    fireEvent.click(screen.getByRole("button", { name: "Send" }));

    await waitFor(() =>
      expect(createComment).toHaveBeenCalledWith("CORE-1", {
        body: "@Planner hello @Worker",
        delivery: "steer",
        mentions: [{ target: "session:A" }, { target: "session:B" }],
      })
    );
  } finally {
    view.unmount();
    createComment.mockRestore();
  }
});

// A message this channel already addresses needs no mention of its own recipient: the record
// proves the reader never touched the text the last channel seeded, so it goes with the channel.
test("the owner's own untouched mention leaves with the channel that seeded it", async () => {
  const createAgentMessage = spyOn(api, "createAgentMessage").mockResolvedValue(createdMessage);
  const { rerender, view } = renderComposer({ seedMentions: [plannerMention] });

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    fireEvent.change(field, { target: { value: "@Planner hello" } });
    rerender({ owner: { kind: "session", sessionId: "A" } });
    await waitFor(() => expect(field.value).toBe("hello"));
    fireEvent.click(screen.getByRole("button", { name: "Send" }));

    await waitFor(() =>
      expect(createAgentMessage).toHaveBeenCalledWith("A", { body: "hello", delivery: "steer" })
    );
  } finally {
    view.unmount();
    createAgentMessage.mockRestore();
  }
});

test("an owner mention the reader edited is their prose and stays", async () => {
  const { rerender, view } = renderComposer({ seedMentions: [plannerMention] });

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    // The span no longer reads `@Planner`, so it is no longer a record: it is what they typed.
    fireEvent.change(field, { target: { value: "@Plannr hello" } });
    rerender({ owner: { kind: "session", sessionId: "A" } });
    await waitFor(() => expect(field.value).toBe("@Plannr hello"));
  } finally {
    view.unmount();
  }
});

// "Discard draft?" means the draft is gone, in every host. Where `onClose` unmounts the composer
// that was true by accident; on the Agents page it only moves focus, so the reset has to be the
// composer's own - the same one a successful send runs. What it leaves is what a mount shows: the
// channel's own mention, seeded again, or the next message would not reach it.
test("Discard clears the draft to what a mount shows, wherever the host takes focus", () => {
  const { view } = renderComposer({ seedMentions: [plannerMention] });

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    fireEvent.change(field, { target: { value: "@Planner discard me" } });
    fireEvent.keyDown(field, { key: "Escape" });
    fireEvent.click(screen.getByRole("button", { name: "Discard" }));

    expect(field.value).toBe("@Planner");
  } finally {
    view.unmount();
  }
});

// A refusal is about the draft it turned down: Retry sends the draft as it stands, so it offers
// only what Send would, and Discard, which drops the draft, drops the notice with it.
test("a refused send's Retry asks what Send asks, and Discard takes the notice with the draft", async () => {
  const createComment = spyOn(api, "createComment").mockRejectedValue(
    new ApiError(503, { code: "UNAVAILABLE", error: "the server is down" })
  );
  const { view } = renderComposer({ seedMentions: [plannerMention] });

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    fireEvent.change(field, { target: { value: "@Planner hello" } });
    fireEvent.click(screen.getByRole("button", { name: "Send" }));
    await screen.findByText("Couldn't send — the server is down");
    expect(field.value).toBe("@Planner hello");
    expect(screen.getByRole("button", { name: "Retry" })).toBeDefined();

    // Nothing Send would send, so nothing Retry may: the button goes, the notice stays.
    fireEvent.change(field, { target: { value: "" } });
    expect(screen.queryByRole("button", { name: "Retry" })).toBeNull();
    expect(screen.getByText("Couldn't send — the server is down")).toBeDefined();

    fireEvent.change(field, { target: { value: "@Planner hello" } });
    fireEvent.keyDown(field, { key: "Escape" });
    fireEvent.click(screen.getByRole("button", { name: "Discard" }));
    expect(field.value).toBe("@Planner");
    expect(screen.queryByText("Couldn't send — the server is down")).toBeNull();
    expect(screen.queryByRole("button", { name: "Retry" })).toBeNull();
    expect(createComment).toHaveBeenCalledTimes(1);
  } finally {
    view.unmount();
    createComment.mockRestore();
  }
});

// A message on its way is the server's until it answers, so its draft is not the reader's to
// discard: sending takes a `Discard draft?` already up with it, and Escape raises none while the
// send is out - from a control that stays enabled then, such as a margin composer's Replacement.
// The outcome then lands on the draft that was sent: a refusal hands it back with its notice.
test("a send takes the Discard prompt with it and Escape raises none until the server answers", async () => {
  const refused = Promise.withResolvers<Comment>();
  const createComment = spyOn(api, "createComment").mockReturnValueOnce(refused.promise);
  const { view } = renderComposer({ seedMentions: [plannerMention] });

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    fireEvent.change(field, { target: { value: "@Planner hello" } });
    fireEvent.keyDown(field, { key: "Escape" });
    expect(screen.getByRole("button", { name: "Discard" })).toBeDefined();
    fireEvent.click(screen.getByRole("button", { name: "Send" }));
    await waitFor(() => expect(holdControls().disabled).toBe(true));
    expect(screen.queryByRole("button", { name: "Discard" })).toBeNull();
    fireEvent.keyDown(screen.getByRole("form", { name: "Comment composer" }), { key: "Escape" });
    expect(screen.queryByRole("button", { name: "Discard" })).toBeNull();

    refused.reject(new ApiError(503, { code: "UNAVAILABLE", error: "the server is down" }));
    await screen.findByText("Couldn't send — the server is down");
    expect(field.value).toBe("@Planner hello");
    expect(screen.getByRole("button", { name: "Retry" })).toBeDefined();
    expect(screen.queryByRole("button", { name: "Discard" })).toBeNull();
  } finally {
    view.unmount();
    createComment.mockRestore();
  }
});

test("a send holds every suggestion control and a refusal restores the replacement it sent", async () => {
  const refused = Promise.withResolvers<Comment>();
  const createComment = spyOn(api, "createComment")
    .mockReturnValueOnce(refused.promise)
    .mockResolvedValueOnce(createdComment);
  const { view } = renderComposer({
    anchor: { artifact: "artifact-1", mark_id: "mark-1", quote: "brown" },
    kind: "suggestion",
  });

  try {
    const reason = screen.getByLabelText<HTMLTextAreaElement>("Reason");
    const replacement = screen.getByLabelText<HTMLTextAreaElement>("Replacement");
    fireEvent.change(reason, { target: { value: "why" } });
    fireEvent.change(replacement, { target: { value: "quick" } });
    fireEvent.click(screen.getByRole("button", { name: "Send" }));
    fireEvent.change(replacement, { target: { value: "quick edited while sending" } });

    await waitFor(() => expect(holdControls().disabled).toBe(true));

    refused.reject(new ApiError(503, { code: "UNAVAILABLE", error: "the server is down" }));
    await screen.findByText("Couldn't send — the server is down");
    expect(replacement.value).toBe("quick");
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(createComment).toHaveBeenCalledTimes(2));
    expect(createComment.mock.calls[1]).toEqual([
      "CORE-1",
      {
        anchor: { artifact: "artifact-1", mark_id: "mark-1" },
        body: "why",
        suggestion: { replace_with: "quick" },
      },
    ]);
  } finally {
    view.unmount();
    createComment.mockRestore();
  }
});

test("an Escape in Send's task cannot offer Discard", async () => {
  const refused = Promise.withResolvers<Comment>();
  const createComment = spyOn(api, "createComment").mockReturnValueOnce(refused.promise);
  const { view } = renderComposer({ seedMentions: [plannerMention] });

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    fireEvent.change(field, { target: { value: "@Planner hello" } });
    act(() => {
      fireEvent.click(screen.getByRole("button", { name: "Send" }));
      fireEvent.keyDown(screen.getByRole("form", { name: "Comment composer" }), { key: "Escape" });
    });

    expect(screen.queryByRole("button", { name: "Discard" })).toBeNull();
    refused.reject(new ApiError(503, { code: "UNAVAILABLE", error: "the server is down" }));
    await screen.findByText("Couldn't send — the server is down");
    expect(field.value).toBe("@Planner hello");
  } finally {
    view.unmount();
    createComment.mockRestore();
  }
});

// A Ctrl+K that reaches the field before React disables it must not open a picker later: the send
// has already taken that draft. Once a refusal returns it, a new Ctrl+K opens the picker normally.
test("a reference picker request in Send's task is refused until the send answers", async () => {
  const refused = Promise.withResolvers<Comment>();
  const createComment = spyOn(api, "createComment").mockReturnValueOnce(refused.promise);
  const getIssue = spyOn(api, "getIssue").mockResolvedValue({ project: "CORE" } as never);
  const listIssues = spyOn(api, "listIssues").mockResolvedValue([
    { key: "CORE-1", title: "Core issue" },
  ] as never);
  const { view } = renderComposer({ seedMentions: [plannerMention] });

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    fireEvent.change(field, { target: { value: "@Planner see" } });
    // One act, so React renders nothing between Send and the key.
    act(() => {
      fireEvent.click(screen.getByRole("button", { name: "Send" }));
      expect(holdControls().disabled).toBe(false);
      fireEvent.keyDown(field, { ctrlKey: true, key: "k" });
    });
    await waitFor(() => expect(createComment).toHaveBeenCalledTimes(1));
    expect(holdControls().disabled).toBe(true);
    expect(screen.queryByRole("dialog", { name: "Reference picker" })).toBeNull();

    refused.reject(new ApiError(503, { code: "UNAVAILABLE", error: "the server is down" }));
    await screen.findByText("Couldn't send — the server is down");
    expect(field.value).toBe("@Planner see");
    expect(screen.queryByRole("dialog", { name: "Reference picker" })).toBeNull();
    fireEvent.keyDown(field, { ctrlKey: true, key: "k" });
    fireEvent.click(await screen.findByRole("button", { name: "CORE-1: Core issue" }));
    await waitFor(() => expect(field.value).toBe("@Planner see dispatch://CORE-1"));
  } finally {
    view.unmount();
    createComment.mockRestore();
    getIssue.mockRestore();
    listIssues.mockRestore();
  }
});

test("a Ctrl+K in Send's task cannot open a picker over a successful send", async () => {
  const sent = Promise.withResolvers<Comment>();
  const createComment = spyOn(api, "createComment").mockReturnValueOnce(sent.promise);
  const { view } = renderComposer({ seedMentions: [plannerMention] });

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    fireEvent.change(field, { target: { value: "@Planner see" } });
    act(() => {
      fireEvent.click(screen.getByRole("button", { name: "Send" }));
      fireEvent.keyDown(field, { ctrlKey: true, key: "k" });
    });

    await waitFor(() => expect(createComment).toHaveBeenCalledTimes(1));
    sent.resolve(createdComment);
    await waitFor(() => expect(field.value).toBe("@Planner"));
    expect(screen.queryByRole("dialog", { name: "Reference picker" })).toBeNull();
  } finally {
    view.unmount();
    createComment.mockRestore();
  }
});

// The mention suggestions write into the draft too: a suggestion open when the message goes
// would put a mention into a draft its refusal then replaces. Sending closes them.
test("a send closes the mention suggestions open over its draft", async () => {
  const refused = Promise.withResolvers<Comment>();
  const createComment = spyOn(api, "createComment").mockReturnValueOnce(refused.promise);
  const { view } = renderComposer({ agents: [planner, worker] });

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    fireEvent.change(field, { target: { selectionStart: 9, value: "hello @Pl" } });
    await screen.findByRole("option", { name: "Planner" });
    fireEvent.click(screen.getByRole("button", { name: "Send" }));
    await waitFor(() => expect(holdControls().disabled).toBe(true));
    expect(screen.queryByRole("listbox", { name: "Mention suggestions" })).toBeNull();

    refused.reject(new ApiError(503, { code: "UNAVAILABLE", error: "the server is down" }));
    await screen.findByText("Couldn't send — the server is down");
    expect(field.value).toBe("hello @Pl");
  } finally {
    view.unmount();
    createComment.mockRestore();
  }
});

// A reply is part of the message's address. The composer that started it holds that address until
// the answer, so Cancel reply cannot rewrite a send while it is on the wire.
test("a refused reply keeps the reply its send started with until the server answers", async () => {
  const refused = Promise.withResolvers<Comment>();
  const createComment = spyOn(api, "createComment")
    .mockReturnValueOnce(refused.promise)
    .mockResolvedValueOnce(createdComment);
  let cancelled = 0;
  const cancelReply = () => {
    cancelled += 1;
  };
  const { view } = renderComposer({
    onCancelReply: cancelReply,
    replyTo: { author: "Bob", excerpt: "Earlier", id: "comment-0" },
    seedMentions: [plannerMention],
  });

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    fireEvent.change(field, { target: { value: "Reply body" } });
    fireEvent.click(screen.getByRole("button", { name: "Send" }));
    await waitFor(() => expect(createComment).toHaveBeenCalledTimes(1));
    const cancel = screen.getByRole("button", { name: "Cancel reply" }) as HTMLButtonElement;
    expect(holdControls().disabled).toBe(true);
    fireEvent.click(cancel);
    expect(cancelled).toBe(0);

    refused.reject(new ApiError(503, { code: "UNAVAILABLE", error: "the server is down" }));
    await screen.findByText("Couldn't send — the server is down");
    expect(field.value).toBe("Reply body");
    fireEvent.click(cancel);
    expect(cancelled).toBe(1);

    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(createComment).toHaveBeenCalledTimes(2));
    expect(createComment.mock.calls[1]).toEqual([
      "CORE-1",
      { body: "Reply body", reply_to: "comment-0" },
    ]);
  } finally {
    view.unmount();
    createComment.mockRestore();
  }
});

// A send is the request it started as. TanStack calls `mutationFn` only once `onMutate` has
// resolved, with the options of the latest render, so a host that re-addresses the composer in
// Send's own task - before React re-renders, with no guard of its own - changes only what the
// composer shows next. Here the owner, the kind, the anchor and a reply into a message thread all
// change at once, and the comment still goes as it was sent, to where it was sent.
test("a host that re-addresses the composer in Send's task cannot re-address the send", async () => {
  const refused = Promise.withResolvers<Comment>();
  const createComment = spyOn(api, "createComment").mockReturnValueOnce(refused.promise);
  const createMessage = spyOn(api, "createMessage").mockResolvedValue(createdMessage);
  const createAgentMessage = spyOn(api, "createAgentMessage").mockResolvedValue(createdMessage);
  const createAsk = spyOn(api, "createAsk").mockResolvedValue({} as never);
  const { rerender, view } = renderComposer({
    anchor: { artifact: "artifact-1", mark_id: "mark-1", quote: "brown" },
  });

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    fireEvent.change(field, { target: { value: "Status please" } });
    act(() => {
      fireEvent.click(screen.getByRole("button", { name: "Send" }));
      rerender({
        anchor: { artifact: "artifact-2", mark_id: "mark-2", quote: "fox" },
        kind: "ask",
        owner: { kind: "session", sessionId: "A" },
        replyTo: {
          author: "Planner",
          excerpt: "Can this ship?",
          id: "message-0",
          parentKind: "message",
          thread: { delivery: "btw", target: "session:A", title: "Planner" },
        },
      });
    });

    await waitFor(() => expect(createComment).toHaveBeenCalledTimes(1));
    expect(createComment.mock.calls[0]).toEqual([
      "CORE-1",
      { anchor: { artifact: "artifact-1", mark_id: "mark-1" }, body: "Status please" },
    ]);
    expect(createMessage).not.toHaveBeenCalled();
    expect(createAgentMessage).not.toHaveBeenCalled();
    expect(createAsk).not.toHaveBeenCalled();

    refused.reject(new ApiError(503, { code: "UNAVAILABLE", error: "the server is down" }));
    await screen.findByText("Couldn't send — the server is down");
    expect(field.value).toBe("Status please");
  } finally {
    view.unmount();
    createComment.mockRestore();
    createMessage.mockRestore();
    createAgentMessage.mockRestore();
    createAsk.mockRestore();
  }
});

test("the kind switch hands the pick to the host and shows why the host refused it", () => {
  const picks: string[] = [];
  const { view } = renderComposer({
    anchor: { artifact: "artifact-1", mark_id: "m-1", quote: "brown" },
    onKindChange: (next) => {
      picks.push(next);
      return next === "suggestion"
        ? "A suggestion needs whole words inside one table cell."
        : undefined;
    },
  });

  try {
    const kinds = screen.getByRole("group", { name: "Kind" });
    const pressed = (name: string) =>
      within(kinds).getByRole("button", { name }).getAttribute("aria-pressed");

    fireEvent.click(within(kinds).getByRole("button", { name: "Suggest" }));
    expect(picks).toEqual(["suggestion"]);
    expect(screen.getByRole("status").textContent).toBe(
      "A suggestion needs whole words inside one table cell."
    );
    // The kind is the host's: a refused pick leaves the pressed button where it was.
    expect(pressed("Comment")).toBe("true");
    expect(pressed("Suggest")).toBe("false");

    fireEvent.click(within(kinds).getByRole("button", { name: "Ask" }));
    expect(picks).toEqual(["suggestion", "ask"]);
    expect(screen.queryByRole("status")).toBeNull();
    // ...and an accepted pick shows only once the host answers through `kind`.
    expect(pressed("Comment")).toBe("true");
  } finally {
    view.unmount();
  }
});

test("a refusal belongs to the mark it answered: a newer anchor leaves it behind", () => {
  function Host(): ReactNode {
    const [markId, setMarkId] = useState("m-1");
    return (
      <>
        <button onClick={() => setMarkId("m-2")} type="button">
          Replace mark
        </button>
        <MentionComposer
          anchor={{ artifact: "artifact-1", mark_id: markId, quote: "brown" }}
          onClose={() => {}}
          onKindChange={() => "Not this one."}
          onSent={() => {}}
          owner={{ issueKey: "CORE-1", kind: "issue" }}
        />
      </>
    );
  }
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const view = render(
    <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
      <QueryClientProvider client={queryClient}>
        <Host />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    const kinds = screen.getByRole("group", { name: "Kind" });
    fireEvent.click(within(kinds).getByRole("button", { name: "Suggest" }));
    expect(screen.getByRole("status").textContent).toBe("Not this one.");
    fireEvent.click(screen.getByRole("button", { name: "Replace mark" }));
    expect(screen.queryByRole("status")).toBeNull();
  } finally {
    view.unmount();
  }
});

// A send's draft and every control a host holds for it are the server's until it answers, so a
// request the server never answers cannot keep them: past the deadline the send ends as refused,
// with the draft it sent handed back, and the composer takes the reader's next move.
test("a send the server never answers is refused at the deadline, with its draft", async () => {
  const createComment = spyOn(api, "createComment").mockImplementation(
    () => Promise.withResolvers<Comment>().promise
  );
  jest.useFakeTimers();
  const { view } = renderComposer();

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    fireEvent.change(field, { target: { value: "Hung send" } });
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Send" }));
    });
    expect(createComment).toHaveBeenCalledTimes(1);
    expect(holdControls().disabled).toBe(true);

    await act(async () => {
      jest.advanceTimersByTime(SEND_DEADLINE_MS - 1);
    });
    expect(holdControls().disabled).toBe(true);
    await act(async () => {
      jest.advanceTimersByTime(1);
    });
    expect(
      screen.getByText(
        "Couldn't send — the server did not answer within 30 seconds. It may still arrive, so look for it before you retry"
      )
    ).toBeTruthy();
    expect(holdControls().disabled).toBe(false);
    expect(field.value).toBe("Hung send");
    expect(screen.getByRole("button", { name: "Retry" })).toBeTruthy();
  } finally {
    jest.useRealTimers();
    view.unmount();
    createComment.mockRestore();
  }
});

// A closed owner takes no send. Its composer stays mounted for a send it still has out and for
// that send's refusal - Send refused, Discard draft in place of Retry - and shows nothing else.
test("a closed composer shows only a send of its own still out, then its refusal with Discard", async () => {
  const refused = Promise.withResolvers<Comment>();
  const createComment = spyOn(api, "createComment").mockReturnValueOnce(refused.promise);
  const { rerender, view } = renderComposer();

  try {
    rerender({ closed: true });
    expect(screen.queryByRole("form", { name: "Comment composer" })).toBeNull();
    rerender({});

    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    fireEvent.change(field, { target: { value: "Closing under me" } });
    fireEvent.click(screen.getByRole("button", { name: "Send" }));
    await waitFor(() => expect(createComment).toHaveBeenCalledTimes(1));
    rerender({ closed: true });
    expect(screen.getByRole("form", { name: "Comment composer" })).toBeTruthy();
    expect(holdControls().disabled).toBe(true);

    refused.reject(new ApiError(409, { code: "ISSUE_CLOSED", error: "issue is closed" }));
    await screen.findByText("Couldn't send — issue is closed");
    expect(screen.getByLabelText<HTMLTextAreaElement>("Comment").value).toBe("Closing under me");
    expect(screen.queryByRole("button", { name: "Retry" })).toBeNull();
    const send = screen.getByRole("button", { name: "Send" });
    expect(send.getAttribute("aria-disabled")).toBe("true");
    expect(send.getAttribute("title")).toBe("This issue is closed. Reopen it to send.");

    fireEvent.click(screen.getByRole("button", { name: "Discard draft" }));
    expect(screen.queryByRole("form", { name: "Comment composer" })).toBeNull();
  } finally {
    view.unmount();
    createComment.mockRestore();
  }
});

// An upload carries its target from the paste that starts it: a host that re-addresses the
// composer in the paste's own task - before React re-renders - moves where the next upload goes,
// never this one, and the reference it appends names where the file went.
test("a pick in the paste's own task cannot move the upload", async () => {
  const uploadArtifact = spyOn(api, "uploadArtifact").mockResolvedValue({
    artifact: { slug: "notes-md" } as never,
    version: {} as never,
  });
  const { rerender, view } = renderComposer();

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    const file = new File(["# Notes"], "notes.md", { type: "text/markdown" });
    act(() => {
      fireEvent.paste(field, { clipboardData: { files: [file] } });
      rerender({ owner: { issueKey: "CORE-2", kind: "issue" } });
    });

    await waitFor(() => expect(uploadArtifact).toHaveBeenCalledTimes(1));
    expect(uploadArtifact.mock.calls[0]?.[0]).toEqual({ issue: "CORE-1" });
    await waitFor(() => expect(field.value).toBe("dispatch://CORE-1/artifact/notes-md"));
  } finally {
    view.unmount();
    uploadArtifact.mockRestore();
  }
});
