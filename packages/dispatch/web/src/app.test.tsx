import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { ApiError, api } from "./api/client";
import { AuthGate } from "./app";

// happy-dom does not implement EventSource; useEventStream only needs a constructible
// class with addEventListener/close so its mount/unmount lifecycle can be exercised.
if (typeof globalThis.EventSource === "undefined") {
  class FakeEventSource {
    addEventListener(): void {}
    close(): void {}
  }
  Object.assign(globalThis, { EventSource: FakeEventSource });
}

test("signing out shows the sign-in page without a reload and tears down the event stream", async () => {
  let signedIn = true;
  const whoAmI = spyOn(api, "whoAmI").mockImplementation(async () => {
    if (!signedIn) {
      throw new ApiError(401, { error: "unauthorized" });
    }
    return { kind: "user", login: "alice" };
  });
  const logout = spyOn(api, "logout").mockImplementation(async () => {
    signedIn = false;
    return { ok: true };
  });
  const listIssues = spyOn(api, "listIssues").mockResolvedValue([]);
  const getMyState = spyOn(api, "getMyState").mockResolvedValue({});
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([]);
  const eventSourceClose = spyOn(EventSource.prototype, "close");

  // The event stream's transport is being migrated from EventSource to a fetch stream
  // torn down via AbortController (#837); this test asserts teardown happened by
  // whichever primitive the current tree uses, not one specific implementation. Any
  // fetch call the hook makes for the stream never resolves — only its signal matters.
  const fetchSignals: AbortSignal[] = [];
  function stubFetch(_input: RequestInfo | URL, init?: RequestInit): Promise<Response> {
    if (init?.signal !== undefined && init.signal !== null) {
      fetchSignals.push(init.signal);
    }
    return new Promise<Response>(() => {});
  }
  stubFetch.preconnect = () => {};
  const fetchSpy = spyOn(globalThis, "fetch").mockImplementation(stubFetch);

  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter initialEntries={["/"]}>
      <QueryClientProvider client={queryClient}>
        <AuthGate />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    await screen.findByText("Signed in as alice");
    expect(eventSourceClose).not.toHaveBeenCalled();
    expect(fetchSignals.some((signal) => signal.aborted)).toBe(false);

    fireEvent.click(screen.getByRole("button", { name: "Sign out" }));

    await waitFor(() => expect(logout).toHaveBeenCalled());
    await screen.findByRole("link", { name: "Sign in with GitHub" });

    const streamTornDown =
      eventSourceClose.mock.calls.length > 0 || fetchSignals.some((signal) => signal.aborted);
    expect(streamTornDown).toBe(true);
  } finally {
    view.unmount();
    whoAmI.mockRestore();
    logout.mockRestore();
    listIssues.mockRestore();
    getMyState.mockRestore();
    getInbox.mockRestore();
    eventSourceClose.mockRestore();
    fetchSpy.mockRestore();
  }
});
