import { expect, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { ApiError, api } from "../../api/client";
import type { Message } from "../../api/types";
import { ConversationComposer } from "./ConversationComposer";

const createdMessage: Message = {
  author: { id: "alice", kind: "user" },
  body: "sent",
  created_at: "2026-09-10T00:00:00Z",
  id: "message-1",
  issue_key: "CORE-1",
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

test("Enter sends a message while Shift+Enter keeps it in the composer", async () => {
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
    fireEvent.keyDown(field, { key: "Enter", shiftKey: true });
    expect(sent).toEqual([]);

    fireEvent.keyDown(field, { key: "Enter" });

    await waitFor(() => expect(sent).toEqual(["line one"]));
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
    fireEvent.keyDown(field, { key: "Enter" });

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
