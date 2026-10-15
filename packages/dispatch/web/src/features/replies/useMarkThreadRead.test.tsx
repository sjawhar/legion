import { afterEach, expect, jest, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render } from "@testing-library/react";
import type { ReactNode } from "react";

import {
  type IntersectionControl,
  installIntersectionObserver,
} from "../../__tests__/intersection-observer";
import { api } from "../../api/client";
import { myRepliesOwnerQuery, whoAmIQuery } from "../../api/queries";
import type { MyRepliesResponse, MyReplyRow } from "../../api/types";
import { replyRow as row } from "./replies-fixture";
import { useMarkThreadRead } from "./useMarkThreadRead";

afterEach(() => {
  jest.useRealTimers();
});

function unreadSet(rows: MyReplyRow[], total = rows.length): MyRepliesResponse {
  return { limit: 200, offset: 0, rows, total };
}

function Thread({ at, threadId }: { at: string | undefined; threadId: string }): ReactNode {
  const ref = useMarkThreadRead({
    enabled: true,
    newestShownAt: at,
    owner: { issue: "CORE-1" },
    threadId,
  });
  return (
    <p data-testid="newest" ref={ref}>
      Newest reply
    </p>
  );
}

/** The unread set and the viewer are already cached, so the hook decides at once. */
function renderThread(set: MyRepliesResponse, props: { at: string | undefined; threadId: string }) {
  const queryClient = new QueryClient({
    defaultOptions: {
      mutations: { retry: false },
      queries: { retry: false, staleTime: Number.POSITIVE_INFINITY },
    },
  });
  queryClient.setQueryData(whoAmIQuery().queryKey, { kind: "user", login: "alice" });
  queryClient.setQueryData(myRepliesOwnerQuery({ issue: "CORE-1" }).queryKey, set);
  const wrap = (node: ReactNode) => (
    <QueryClientProvider client={queryClient}>{node}</QueryClientProvider>
  );
  const view = render(wrap(<Thread {...props} />));
  return {
    rerender: (next: { at: string | undefined; threadId: string }) =>
      view.rerender(wrap(<Thread {...next} />)),
    view,
  };
}

async function dwell(screen: IntersectionControl, element: Element) {
  act(() => screen.show(element));
  act(() => {
    jest.advanceTimersByTime(1_000);
  });
  for (let flush = 0; flush < 5; flush += 1) await act(async () => {});
}

test("a thread outside the viewer's unread set is never watched or marked", async () => {
  jest.useFakeTimers();
  const screen = installIntersectionObserver();
  const mark = spyOn(api, "markThreadRead").mockResolvedValue(undefined);
  const { view } = renderThread(unreadSet([row("other-thread")]), {
    at: "2026-10-12T00:00:00Z",
    threadId: "thread-1",
  });
  try {
    const newest = view.getByTestId("newest");
    expect(screen.observed()).not.toContain(newest);
    await dwell(screen, newest);
    expect(mark).not.toHaveBeenCalled();
  } finally {
    view.unmount();
    mark.mockRestore();
    screen.restore();
  }
});

test("an unread thread is marked through its newest reply once that reply dwells, once per reply", async () => {
  jest.useFakeTimers();
  const screen = installIntersectionObserver();
  const mark = spyOn(api, "markThreadRead").mockResolvedValue(undefined);
  const first = "2026-10-12T00:00:00.123456Z";
  const second = "2026-10-12T00:05:00Z";
  const { rerender, view } = renderThread(unreadSet([row("thread-1")]), {
    at: first,
    threadId: "thread-1",
  });
  try {
    const newest = view.getByTestId("newest");
    expect(screen.observed()).toContain(newest);
    act(() => screen.show(newest));
    act(() => {
      jest.advanceTimersByTime(999);
    });
    expect(mark).not.toHaveBeenCalled();
    act(() => {
      jest.advanceTimersByTime(1);
    });
    for (let flush = 0; flush < 5; flush += 1) await act(async () => {});
    expect(mark.mock.calls).toEqual([["thread-1", first]]);

    // The same reply seen again sends nothing more.
    act(() => screen.hide(newest));
    await dwell(screen, newest);
    expect(mark).toHaveBeenCalledTimes(1);

    // A newer reply is a new mark, through its own timestamp.
    rerender({ at: second, threadId: "thread-1" });
    await dwell(screen, view.getByTestId("newest"));
    expect(mark.mock.calls).toEqual([
      ["thread-1", first],
      ["thread-1", second],
    ]);
  } finally {
    view.unmount();
    mark.mockRestore();
    screen.restore();
  }
});

// The set holds the owner's first 200 unread threads; past them a thread may be unread without
// being listed, so every thread is watched and the server decides.
test("an owner with more unread threads than its set holds watches every thread", async () => {
  jest.useFakeTimers();
  const screen = installIntersectionObserver();
  const mark = spyOn(api, "markThreadRead").mockResolvedValue(undefined);
  const { view } = renderThread(unreadSet([row("other-thread")], 201), {
    at: "2026-10-12T00:00:00Z",
    threadId: "thread-1",
  });
  try {
    await dwell(screen, view.getByTestId("newest"));
    expect(mark.mock.calls).toEqual([["thread-1", "2026-10-12T00:00:00Z"]]);
  } finally {
    view.unmount();
    mark.mockRestore();
    screen.restore();
  }
});

test("a thread with no reply shown is never watched", () => {
  const screen = installIntersectionObserver();
  const { view } = renderThread(unreadSet([row("thread-1")]), {
    at: undefined,
    threadId: "thread-1",
  });
  try {
    expect(screen.observed()).not.toContain(view.getByTestId("newest"));
  } finally {
    view.unmount();
    screen.restore();
  }
});
