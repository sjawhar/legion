import { afterEach, beforeEach, expect, type Mock, spyOn, test } from "bun:test";
import { focusManager, QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, screen, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { openStreamResponse } from "../../__tests__/sse-test-harness";
import { api, isRetryableQueryError } from "../../api/client";
import { refreshQueries } from "../../api/query-refresh";
import { useEventStream } from "../../api/sse";
import type { CredentialPendingResponse } from "../../api/types";
import { Inbox } from "./Inbox";

// A Dispatch with no secrets broker answers the credential-request list with `null`, an answer that
// holds until the page reloads or the event stream reconnects (the server reads its broker setting
// at boot). The QueryClient here is main.tsx's own, with no retry delay. Every credential-list call
// after the first is held open, so a refetch, had one started, would leave the list loading and the
// empty state off the screen.
const never = () => Promise.withResolvers<never>().promise;

let whoAmI: Mock<typeof api.whoAmI>;
let getInbox: Mock<typeof api.getInbox>;
let getCredentialPending: Mock<typeof api.getCredentialPending>;
beforeEach(() => {
  window.localStorage.clear();
  whoAmI = spyOn(api, "whoAmI").mockResolvedValue({ kind: "user", login: "alice" });
  getInbox = spyOn(api, "getInbox").mockResolvedValue([]);
  getCredentialPending = spyOn(api, "getCredentialPending")
    .mockResolvedValueOnce(null)
    .mockImplementation(never);
});
afterEach(() => {
  whoAmI.mockRestore();
  getInbox.mockRestore();
  getCredentialPending.mockRestore();
  focusManager.setFocused(undefined);
});

function productionQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: {
      queries: {
        refetchOnWindowFocus: true,
        retry: (failureCount, error) => failureCount < 2 && isRetryableQueryError(error),
        retryDelay: 0,
        staleTime: 30_000,
      },
    },
  });
}

function shell(queryClient: QueryClient, children: ReactNode) {
  return (
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
    </MemoryRouter>
  );
}

/** Records every DOM change after which "Nothing needs you" is no longer on the page. */
function watchEmptyState(): { blinks: number; stop: () => void } {
  const record = { blinks: 0, stop: () => observer.disconnect() };
  const observer = new MutationObserver(() => {
    if (screen.queryByText("Nothing needs you") === null) record.blinks += 1;
  });
  observer.observe(document.body, { characterData: true, childList: true, subtree: true });
  return record;
}

test("a Dispatch with no broker is asked once per load, even when that answer beats the inbox list", async () => {
  const inbox = Promise.withResolvers<[]>();
  getInbox.mockReturnValue(inbox.promise);
  const view = render(shell(productionQueryClient(), <Inbox />));
  try {
    await waitFor(() => expect(getCredentialPending).toHaveBeenCalledTimes(1));
    await act(async () => {
      await Promise.resolve();
    });
    inbox.resolve([]);
    expect(await screen.findByText("Nothing needs you")).toBeTruthy();
    expect(screen.queryByText("Couldn't load credential requests.")).toBeNull();
    expect(getCredentialPending).toHaveBeenCalledTimes(1);
  } finally {
    view.unmount();
  }
});

test("with no broker, a focus, a stream refresh and a return to the Inbox neither ask again nor hide the empty state", async () => {
  const queryClient = productionQueryClient();
  const view = render(shell(queryClient, <Inbox />));
  try {
    expect(await screen.findByText("Nothing needs you")).toBeTruthy();
    const watch = watchEmptyState();

    act(() => {
      focusManager.setFocused(false);
      focusManager.setFocused(true);
    });
    await act(async () => {
      await Promise.resolve();
    });
    act(() => refreshQueries(queryClient));
    // The refresh refetches the inbox list; once that has landed, any credential refetch it
    // started would already be on the wire.
    await waitFor(() => expect(getInbox).toHaveBeenCalledTimes(2));
    await act(async () => {
      await Promise.resolve();
    });
    watch.stop();
    expect(watch.blinks).toBe(0);

    // Leaving the Inbox and coming back mounts a new reader of the same answer.
    view.rerender(shell(queryClient, null));
    view.rerender(shell(queryClient, <Inbox />));
    expect(screen.getByText("Nothing needs you")).toBeTruthy();
    await act(async () => {
      await Promise.resolve();
    });
    expect(screen.getByText("Nothing needs you")).toBeTruthy();
    expect(getCredentialPending).toHaveBeenCalledTimes(1);
  } finally {
    view.unmount();
  }
});

function EventStream(): null {
  useEventStream();
  return null;
}

test("with no broker, a reconnect of the event stream asks the list again once, and a focus does not", async () => {
  const originalFetch = globalThis.fetch;
  const streams: string[] = [];
  globalThis.fetch = ((input: RequestInfo | URL, init?: RequestInit) => {
    if (!String(input).startsWith("/api/v1/events")) return originalFetch(input, init);
    streams.push(String(input));
    return Promise.resolve(openStreamResponse(init?.signal));
  }) as typeof fetch;
  // The server restarted with a broker: the second answer lists a request, and is held until the
  // whole-cache refresh the reconnect starts has run.
  const relisted = Promise.withResolvers<CredentialPendingResponse>();
  getCredentialPending.mockReturnValueOnce(relisted.promise);
  const queryClient = productionQueryClient();
  const view = render(
    shell(
      queryClient,
      <>
        <EventStream />
        <Inbox />
      </>
    )
  );
  try {
    expect(await screen.findByText("Nothing needs you")).toBeTruthy();
    await waitFor(() => expect(streams).toHaveLength(1));

    act(() => {
      focusManager.setFocused(false);
      focusManager.setFocused(true);
    });
    await act(async () => {
      await Promise.resolve();
    });
    expect(getCredentialPending).toHaveBeenCalledTimes(1);

    const watch = watchEmptyState();
    act(() => {
      window.dispatchEvent(new Event("online"));
    });
    await waitFor(() => expect(streams).toHaveLength(2));
    await waitFor(() => expect(getCredentialPending).toHaveBeenCalledTimes(2));
    // The reconnect's whole-cache refresh refetches the inbox list; the held answer is not asked a
    // third time, and nothing blinks while the second call is out.
    await waitFor(() => expect(getInbox).toHaveBeenCalledTimes(2));
    await act(async () => {
      await Promise.resolve();
    });
    watch.stop();
    expect(watch.blinks).toBe(0);
    expect(getCredentialPending).toHaveBeenCalledTimes(2);

    await act(async () => {
      relisted.resolve({
        pending: [
          {
            identifiers: ["DEMO_API_KEY"],
            kind: "agent_secret",
            record_id: "record-1",
            requested_at: new Date().toISOString(),
          },
        ],
      });
    });
    expect(await screen.findByRole("link", { name: /Secret request.*DEMO_API_KEY/s })).toBeTruthy();
    expect(screen.queryByText("Nothing needs you")).toBeNull();
  } finally {
    view.unmount();
    globalThis.fetch = originalFetch;
  }
}, 10_000);
