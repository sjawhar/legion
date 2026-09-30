import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { useState } from "react";
import { MemoryRouter } from "react-router-dom";

import { ApiError, api } from "../../api/client";
import type { Agent, Comment, Message } from "../../api/types";
import {
  type CarriedDraft,
  type ComposerOwner,
  MentionComposer,
  reconcileMentions,
} from "./MentionComposer";

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

function renderComposer(
  options: {
    agents?: readonly Agent[];
    carried?: CarriedDraft;
    edit?: { body: string; id: string };
    initialMentions?: readonly { target: string; title: string }[];
    onCarry?: (draft: CarriedDraft) => void;
    onCancelReply?: () => void;
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
  } = {}
) {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const view = render(
    <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
      <QueryClientProvider client={queryClient}>
        <MentionComposer
          agents={options.agents}
          carried={options.carried}
          edit={options.edit}
          initialMentions={options.initialMentions}
          onCancelReply={options.onCancelReply}
          onCarry={options.onCarry}
          onClose={() => {}}
          onSent={options.onSent ?? (() => {})}
          owner={options.owner ?? { issueKey: "CORE-1", kind: "issue" }}
          replyTo={
            options.replyTo === null
              ? null
              : options.replyTo === undefined
                ? undefined
                : { ...options.replyTo, parentKind: options.replyTo.parentKind ?? "comment" }
          }
        />
      </QueryClientProvider>
    </MemoryRouter>
  );
  return { queryClient, view };
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

test("a token-only direct-session draft cannot submit an empty message, and Send says why", () => {
  const createAgentMessage = spyOn(api, "createAgentMessage").mockResolvedValue({} as never);
  const { view } = renderComposer({ owner: { kind: "session", sessionId: "A" } });

  try {
    for (const command of ["/btw", "/aside"]) {
      fireEvent.change(screen.getByLabelText("Comment"), { target: { value: `${command} ` } });
      const reason = `Type the message after ${command}.`;
      expect(sendRefusal()).toEqual({ description: reason, disabled: "true", title: reason });
      fireEvent.click(screen.getByRole("button", { name: "Send" }));
    }
    expect(createAgentMessage).not.toHaveBeenCalled();
    fireEvent.change(screen.getByLabelText("Comment"), { target: { value: "/btw status?" } });
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

test("a token-only targeted legacy reply cannot submit, and Send says why", () => {
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
    fireEvent.change(screen.getByLabelText("Comment"), { target: { value: "/btw " } });
    const reason = "Type the message after /btw.";
    expect(sendRefusal()).toEqual({ description: reason, disabled: "true", title: reason });
    fireEvent.click(screen.getByRole("button", { name: "Send" }));
    expect(createMessage).not.toHaveBeenCalled();
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
    expect(screen.getByText(/Worker does not advertise Steer/)).toBeTruthy();
  } finally {
    view.unmount();
  }
});

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
    expect(screen.getByText(/Planner and Worker do not advertise Steer/)).toBeTruthy();
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

// The carry is the one branch these seven call sites share, so its four cases live here rather
// than in one host's e2e: what a caller with no carry gets (every other surface), what a carry
// with prose gets, what a carry that already holds this owner's mention gets, and where a
// mention accepted afterwards lands. Each asserts the wire as well as the field: the body and
// the accepted records disagreeing is exactly the defect these cover.
test("no carry seeds the owner's mention and sends it", async () => {
  const createComment = spyOn(api, "createComment").mockResolvedValue(createdComment);
  const { view } = renderComposer({ initialMentions: [plannerMention] });

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

test("a carry without this owner's mention is seeded in front of it, records and all", async () => {
  const createComment = spyOn(api, "createComment").mockResolvedValue(createdComment);
  const { view } = renderComposer({
    carried: {
      body: "hello @Worker",
      mentions: [{ end: 13, start: 6, target: "session:B", text: "Worker" }],
    },
    initialMentions: [plannerMention],
  });

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    expect(field.value).toBe("@Planner hello @Worker");
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

test("a carry that already holds this owner's mention is not seeded twice, wherever it sits", async () => {
  const createComment = spyOn(api, "createComment").mockResolvedValue(createdComment);
  const { view } = renderComposer({
    carried: {
      body: "y@Planner x",
      mentions: [{ end: 9, start: 1, target: "session:A", text: "Planner" }],
    },
    initialMentions: [plannerMention],
  });

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    expect(field.value).toBe("y@Planner x");
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

test("a mention accepted after a carry lands at its own offset", async () => {
  const createComment = spyOn(api, "createComment").mockResolvedValue(createdComment);
  const { view } = renderComposer({
    agents: [planner, worker],
    carried: { body: "hello", mentions: [] },
    initialMentions: [plannerMention],
  });

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    expect(field.value).toBe("@Planner hello");
    fireEvent.change(field, { target: { value: "@Planner hello @" } });
    await screen.findByRole("listbox", { name: "Mention suggestions" });
    fireEvent.click(screen.getByRole("option", { name: "Worker" }));
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
test("the owner's own untouched mention does not travel into a message addressed to it", async () => {
  const createAgentMessage = spyOn(api, "createAgentMessage").mockResolvedValue(createdMessage);
  const { view } = renderComposer({
    carried: {
      body: "@Planner hello",
      mentions: [{ end: 8, start: 0, target: "session:A", text: "Planner" }],
    },
    owner: { kind: "session", sessionId: "A" },
  });

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    expect(field.value).toBe("hello");
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
  const { view } = renderComposer({
    carried: {
      body: "@Plannr hello",
      mentions: [{ end: 8, start: 0, target: "session:A", text: "Planner" }],
    },
    owner: { kind: "session", sessionId: "A" },
  });

  try {
    // The span no longer reads `@Planner`, so it is no longer a record: it is what they typed.
    expect(screen.getByLabelText<HTMLTextAreaElement>("Comment").value).toBe("@Plannr hello");
  } finally {
    view.unmount();
  }
});

// "Discard draft?" means the draft is gone, in every host. Where `onClose` unmounts the composer
// that was true by accident; on the Agents page it only moves focus, so the reset has to be the
// composer's own - the same one a successful send runs, carry included.
test("Discard clears the draft and its records, wherever the host takes focus", () => {
  const carriedDrafts: CarriedDraft[] = [];
  const { view } = renderComposer({
    initialMentions: [plannerMention],
    onCarry: (draft) => carriedDrafts.push(draft),
  });

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    fireEvent.change(field, { target: { value: "@Planner discard me" } });
    fireEvent.keyDown(field, { key: "Escape" });
    fireEvent.click(screen.getByRole("button", { name: "Discard" }));

    expect(field.value).toBe("");
    expect(carriedDrafts.at(-1)).toEqual({ body: "", mentions: [] });
  } finally {
    view.unmount();
  }
});
