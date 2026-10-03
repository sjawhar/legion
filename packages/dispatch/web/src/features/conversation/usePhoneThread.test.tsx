import { expect, test } from "bun:test";
import { type MutationKey, QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, renderHook } from "@testing-library/react";
import type { ReactNode } from "react";

import { pastDeadlineKey } from "../../hooks/useSending";
import type { ReplyTarget } from "./composer-model";
import { threadReplySendKey, usePhoneThread } from "./usePhoneThread";

const sendKey: MutationKey = ["conversation-composer", "CORE-1"];

const reply: ReplyTarget = {
  author: "bob",
  excerpt: "Earlier comment",
  id: "comment-1",
  parentKind: "comment",
};

/** Starts a send named `mutationKey` that stays out until the returned call answers it. */
function startSend(queryClient: QueryClient, mutationKey: MutationKey): () => Promise<void> {
  const { promise, resolve } = Promise.withResolvers<void>();
  const mutation = queryClient
    .getMutationCache()
    .build(queryClient, { mutationFn: () => promise, mutationKey });
  let settled: Promise<unknown> = Promise.resolve();
  act(() => {
    settled = mutation.execute(undefined);
  });
  return () =>
    act(async () => {
      resolve();
      await settled;
    });
}

function renderPhoneThread() {
  const queryClient = new QueryClient({ defaultOptions: { mutations: { retry: false } } });
  const view = renderHook(
    ({ isPhoneViewport }: { isPhoneViewport: boolean }) =>
      usePhoneThread({
        isPhoneViewport,
        sendKey,
        sendingNow: () => queryClient.isMutating({ mutationKey: sendKey }) > 0,
      }),
    {
      initialProps: { isPhoneViewport: true },
      wrapper: ({ children }: { children: ReactNode }) => (
        <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
      ),
    }
  );
  return { queryClient, view };
}

test("a thread opened on a phone closes when the viewport widens with nothing out", () => {
  const { view } = renderPhoneThread();

  try {
    act(() => view.result.current.open("comment-1"));
    expect(view.result.current.id).toBe("comment-1");
    expect(view.result.current.holds("comment-1")).toBe(true);

    view.rerender({ isPhoneViewport: false });
    expect(view.result.current.id).toBeUndefined();
    expect(view.result.current.holds("comment-1")).toBe(false);
  } finally {
    view.unmount();
  }
});

// The card's reply composer lives in the view, so the view outlives a widening while that reply is
// out, and stays open after it lands, until Back. Back itself waits for the reply until its
// deadline, after which the send goes on beneath its name and Back lets the reader go.
test("the card's own reply keeps the thread through a widening, and holds Back until its deadline", async () => {
  const { queryClient, view } = renderPhoneThread();

  try {
    act(() => view.result.current.open("comment-1"));
    const cardReply = threadReplySendKey(sendKey, "comment-1");
    const answer = startSend(queryClient, cardReply);
    expect(view.result.current.cardReplySending).toBe(true);

    view.rerender({ isPhoneViewport: false });
    expect(view.result.current.id).toBe("comment-1");
    act(() => view.result.current.close());
    expect(view.result.current.id).toBe("comment-1");

    await answer();
    expect(view.result.current.cardReplySending).toBe(false);
    expect(view.result.current.id).toBe("comment-1");

    act(() => view.result.current.open("comment-1"));
    const lateAnswer = startSend(queryClient, pastDeadlineKey(cardReply));
    expect(view.result.current.cardReplySending).toBe(false);
    act(() => view.result.current.close());
    expect(view.result.current.id).toBeUndefined();
    await lateAnswer();
  } finally {
    view.unmount();
  }
});

// The thread composer is the view's, and nothing else shows its send's outcome on a wider screen:
// a widening while it is out reopens the thread it answers, even one the reader left with Back.
test("the thread composer's send out through a widening reopens the thread it answers", async () => {
  const { queryClient, view } = renderPhoneThread();

  try {
    act(() => {
      expect(view.result.current.beginReply(reply)).toBe(true);
    });
    expect(view.result.current.id).toBe("comment-1");
    expect(view.result.current.replyTo).toBe(reply);
    const answer = startSend(queryClient, view.result.current.composerKey);
    act(() => view.result.current.close());
    expect(view.result.current.id).toBeUndefined();
    expect(view.result.current.holds("comment-1")).toBe(true);

    view.rerender({ isPhoneViewport: false });
    expect(view.result.current.id).toBe("comment-1");

    await answer();
    act(() => view.result.current.endReply());
    expect(view.result.current.id).toBe("comment-1");
    act(() => view.result.current.close());
    expect(view.result.current.id).toBeUndefined();
    expect(view.result.current.holds("comment-1")).toBe(false);
  } finally {
    view.unmount();
  }
});

test("a Reply while the tab has a send out is refused and leaves the thread where it was", async () => {
  const { queryClient, view } = renderPhoneThread();

  try {
    const answer = startSend(queryClient, sendKey);
    act(() => {
      expect(view.result.current.beginReply(reply)).toBe(false);
    });
    expect(view.result.current.id).toBeUndefined();
    expect(view.result.current.replyTo).toBeNull();
    await answer();
  } finally {
    view.unmount();
  }
});
