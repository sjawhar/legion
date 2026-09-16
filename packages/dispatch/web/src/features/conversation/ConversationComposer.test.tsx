import { expect, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";
import { ApiError, api } from "../../api/client";
import type { Agent, CreateMessageInput, Message } from "../../api/types";
import { ConversationComposer, type ReplyTarget } from "./ConversationComposer";

const createdMessage: Message = {
  author: { id: "alice", kind: "user" },
  body: "sent",
  created_at: "2026-09-10T00:00:00Z",
  deliveries: [],
  id: "message-1",
  in_reply_to: null,
  issue_key: "CORE-1",
  target: null,
};

function renderComposer(recipientSlot?: ReactNode) {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={queryClient}>
      <ConversationComposer issueKey="CORE-1" onSent={() => {}} recipientSlot={recipientSlot} />
    </QueryClientProvider>
  );
}

test("Enter keeps a multiline message in the composer while Ctrl+Enter sends it", async () => {
  const originalCreateMessage = api.createMessage;
  const sent: string[] = [];
  let sentCount = 0;
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const view = render(
    <QueryClientProvider client={queryClient}>
      <ConversationComposer
        issueKey="CORE-1"
        onSent={() => {
          sentCount += 1;
        }}
      />
    </QueryClientProvider>
  );

  try {
    api.createMessage = async (_issueKey, input) => {
      sent.push(input.body);
      return { ...createdMessage, body: input.body };
    };
    const field = screen.getByLabelText("Message") as HTMLTextAreaElement;
    fireEvent.change(field, { target: { value: "line one" } });
    expect(fireEvent.keyDown(field, { key: "Enter" })).toBe(true);
    expect(sent).toEqual([]);

    fireEvent.change(field, { target: { value: "line one\nline two" } });
    expect(field.value).toBe("line one\nline two");
    fireEvent.keyDown(field, { ctrlKey: true, key: "Enter" });

    await waitFor(() => expect(sent).toEqual(["line one\nline two"]));
    await waitFor(() => expect(field.value).toBe(""));
    expect(sentCount).toBe(1);
  } finally {
    view.unmount();
    api.createMessage = originalCreateMessage;
  }
});

test("a failed message keeps the draft and can be retried", async () => {
  const originalCreateMessage = api.createMessage;
  let attempts = 0;
  const view = renderComposer();

  try {
    api.createMessage = async (_issueKey, input) => {
      attempts += 1;
      if (attempts === 1) {
        throw new ApiError(500, { error: "boom" });
      }
      return { ...createdMessage, body: input.body };
    };
    const field = screen.getByLabelText("Message") as HTMLTextAreaElement;
    fireEvent.change(field, { target: { value: "keep me" } });
    fireEvent.keyDown(field, { ctrlKey: true, key: "Enter" });

    expect((await screen.findByRole("alert")).textContent).toContain("Couldn't send — boom");
    expect(field.value).toBe("keep me");
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));

    await waitFor(() => expect(screen.queryByRole("alert")).toBeNull());
    expect(field.value).toBe("");
    expect(attempts).toBe(2);
  } finally {
    view.unmount();
    api.createMessage = originalCreateMessage;
  }
});

test("whitespace-only input and IME composition do not send a message", () => {
  const originalCreateMessage = api.createMessage;
  const sent: string[] = [];
  const view = renderComposer();

  try {
    api.createMessage = async (_issueKey, input) => {
      sent.push(input.body);
      return { ...createdMessage, body: input.body };
    };
    const field = screen.getByLabelText("Message") as HTMLTextAreaElement;
    fireEvent.change(field, { target: { value: "   " } });
    fireEvent.keyDown(field, { key: "Enter" });
    fireEvent.change(field, { target: { value: "still composing" } });
    fireEvent.keyDown(field, { isComposing: true, key: "Enter" });

    expect(sent).toEqual([]);
  } finally {
    view.unmount();
    api.createMessage = originalCreateMessage;
  }
});

test("updates the default delivery before sending to a selected recipient", async () => {
  const originalCreateMessage = api.createMessage;
  const sent: CreateMessageInput[] = [];
  const agent: Agent = {
    capabilities: ["btw"],
    dir: "/workspaces/planner",
    last_activity: null,
    last_seen: Date.now(),
    machine_id: "host-a",
    open_asks: 0,
    roles: [],
    session_id: "planner-session",
    title: "Planner",
  };
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const view = render(
    <QueryClientProvider client={queryClient}>
      <ConversationComposer
        agents={[agent]}
        defaultDelivery="btw"
        issueKey="CORE-1"
        onSent={() => {}}
        recipientSlot={<span>To: Planner</span>}
        route="session:planner-session"
      />
    </QueryClientProvider>
  );

  try {
    api.createMessage = async (_issueKey, input) => {
      sent.push(input);
      return { ...createdMessage, body: input.body };
    };
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "BTW" }).getAttribute("aria-pressed")).toBe("true")
    );
    view.rerender(
      <QueryClientProvider client={queryClient}>
        <ConversationComposer
          agents={[agent]}
          defaultDelivery="steer"
          issueKey="CORE-1"
          onSent={() => {}}
          recipientSlot={<span>To: Planner</span>}
          route="session:planner-session"
        />
      </QueryClientProvider>
    );
    const field = screen.getByLabelText("Message");
    fireEvent.change(field, { target: { value: "Switch to steer" } });
    fireEvent.submit(screen.getByRole("form", { name: "Message composer" }));

    await waitFor(() =>
      expect(sent).toEqual([
        { body: "Switch to steer", delivery: "steer", target: "session:planner-session" },
      ])
    );
  } finally {
    view.unmount();
    api.createMessage = originalCreateMessage;
  }
});

test("renders a recipient slot in the message toolbar", () => {
  const view = renderComposer(<span>To: planner</span>);

  try {
    expect(screen.getByRole("form", { name: "Message composer" }).textContent).toContain(
      "To: planner"
    );
  } finally {
    view.unmount();
  }
});

test("reply mode shows the quoted parent, sends in_reply_to, and cancels with the chip or Escape", async () => {
  const originalCreateMessage = api.createMessage;
  const sent: CreateMessageInput[] = [];
  let cancelled = 0;
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <ConversationComposer
          issueKey="CORE-1"
          onCancelReply={() => {
            cancelled += 1;
          }}
          onSent={() => {}}
          replyTo={{
            author: "Planner",
            excerpt: "Once the build is green.",
            id: "message-9",
            to: "/issues/CORE-1/messages/message-9",
          }}
        />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    api.createMessage = async (_issueKey, input) => {
      sent.push(input);
      return { ...createdMessage, body: input.body, in_reply_to: input.in_reply_to ?? null };
    };
    const chip = screen.getByRole("link", { name: /Replying to Planner/ });
    expect(chip.textContent).toBe("Replying to Planner — Once the build is green.");
    expect(chip.getAttribute("href")).toBe("/issues/CORE-1/messages/message-9");
    expect(document.activeElement).toBe(screen.getByLabelText("Message"));

    fireEvent.change(screen.getByLabelText("Message"), { target: { value: "Ship it." } });
    fireEvent.submit(screen.getByRole("form", { name: "Message composer" }));
    await waitFor(() => expect(sent).toEqual([{ body: "Ship it.", in_reply_to: "message-9" }]));

    fireEvent.click(screen.getByRole("button", { name: "Cancel reply" }));
    expect(cancelled).toBe(1);
    fireEvent.keyDown(screen.getByLabelText("Message"), { key: "Escape" });
    expect(cancelled).toBe(2);
  } finally {
    view.unmount();
    api.createMessage = originalCreateMessage;
  }
});

test("a reply on a targeted thread inherits the thread's recipient and delivery mode", async () => {
  const originalCreateMessage = api.createMessage;
  const sent: CreateMessageInput[] = [];
  const agent: Agent = {
    capabilities: ["btw"],
    dir: "/workspaces/planner",
    last_activity: null,
    last_seen: Date.now(),
    machine_id: "host-a",
    open_asks: 0,
    roles: [],
    session_id: "planner-session",
    title: "Planner",
  };
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const composer = (replyTo: ReplyTarget | null) => (
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <ConversationComposer
          agents={[agent]}
          issueKey="CORE-1"
          onCancelReply={() => {}}
          onSent={() => {}}
          replyTo={replyTo}
        />
      </QueryClientProvider>
    </MemoryRouter>
  );
  const view = render(composer(null));

  try {
    api.createMessage = async (_issueKey, input) => {
      sent.push(input);
      return { ...createdMessage, body: input.body };
    };
    expect(screen.getByRole("button", { name: "Choose recipient" }).textContent).toBe(
      "To: Choose recipient"
    );
    view.rerender(
      composer({
        author: "Planner",
        excerpt: "Once the build is green.",
        id: "message-9",
        thread: { delivery: "btw", target: "session:planner-session", title: "Planner" },
      })
    );
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Choose recipient" }).textContent).toBe(
        "To: Planner"
      )
    );
    expect(screen.getByRole("button", { name: "BTW" }).getAttribute("aria-pressed")).toBe("true");
    fireEvent.change(screen.getByLabelText("Message"), { target: { value: "It is green." } });
    fireEvent.submit(screen.getByRole("form", { name: "Message composer" }));
    await waitFor(() =>
      expect(sent).toEqual([
        {
          body: "It is green.",
          delivery: "btw",
          in_reply_to: "message-9",
          target: "session:planner-session",
        },
      ])
    );

    // Switching straight to a reply on a plain message drops the inherited recipient too.
    view.rerender(composer({ author: "bob", excerpt: "A plain note.", id: "message-10" }));
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Choose recipient" }).textContent).toBe(
        "To: Choose recipient"
      )
    );
    expect(screen.getByText("Replying to bob — A plain note.")).toBeTruthy();

    // Leaving reply mode returns the composer to its own default: here, no recipient.
    view.rerender(
      composer({
        author: "Planner",
        excerpt: "Once the build is green.",
        id: "message-9",
        thread: { delivery: "btw", target: "session:planner-session", title: "Planner" },
      })
    );
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Choose recipient" }).textContent).toBe(
        "To: Planner"
      )
    );
    view.rerender(composer(null));
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Choose recipient" }).textContent).toBe(
        "To: Choose recipient"
      )
    );
  } finally {
    view.unmount();
    api.createMessage = originalCreateMessage;
  }
});
