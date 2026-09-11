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
  const originalMatchMedia = window.matchMedia;
  window.matchMedia = (() =>
    ({
      addEventListener: () => {},
      addListener: () => {},
      dispatchEvent: () => true,
      matches: false,
      media: "",
      onchange: null,
      removeEventListener: () => {},
      removeListener: () => {},
    }) as MediaQueryList) as typeof window.matchMedia;

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
    window.matchMedia = originalMatchMedia;
    whoAmI.mockRestore();
    logout.mockRestore();
    listIssues.mockRestore();
    getMyState.mockRestore();
    getInbox.mockRestore();
    eventSourceClose.mockRestore();
    fetchSpy.mockRestore();
  }
});

test("project routes render the project page while the future document route is not found", async () => {
  const originalMatchMedia = window.matchMedia;
  window.matchMedia = (() =>
    ({
      addEventListener: () => {},
      addListener: () => {},
      dispatchEvent: () => true,
      matches: false,
      media: "",
      onchange: null,
      removeEventListener: () => {},
      removeListener: () => {},
    }) as MediaQueryList) as typeof window.matchMedia;
  const whoAmI = spyOn(api, "whoAmI").mockResolvedValue({ kind: "user", login: "alice" });
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([]);
  const getMyState = spyOn(api, "getMyState").mockResolvedValue({});
  const listIssues = spyOn(api, "listIssues").mockResolvedValue([]);
  const listProjects = spyOn(api, "listProjects").mockResolvedValue([
    { created_at: "2026-09-10T00:00:00Z", key: "CORE", name: "Core", open_asks: 0 },
  ]);
  const listProjectArtifacts = spyOn(api, "listProjectArtifacts").mockResolvedValue([]);
  function streamFetch(_input: RequestInfo | URL, _init?: RequestInit): Promise<Response> {
    return new Promise<Response>(() => {});
  }
  streamFetch.preconnect = () => {};
  const fetchSpy = spyOn(globalThis, "fetch").mockImplementation(streamFetch);

  const renderRoute = (path: string) => {
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    return render(
      <MemoryRouter initialEntries={[path]}>
        <QueryClientProvider client={queryClient}>
          <AuthGate />
        </QueryClientProvider>
      </MemoryRouter>
    );
  };

  try {
    const issues = renderRoute("/projects/CORE");
    await screen.findByRole("heading", { name: "Core" });
    expect(screen.getByRole("tab", { name: "Issues" }).getAttribute("aria-selected")).toBe("true");
    issues.unmount();

    const documents = renderRoute("/projects/CORE/documents");
    await screen.findByRole("heading", { name: "Core" });
    expect(screen.getByRole("tab", { name: "Documents" }).getAttribute("aria-selected")).toBe(
      "true"
    );
    documents.unmount();

    const unavailableDocument = renderRoute("/projects/CORE/documents/design-notes");
    await screen.findByRole("heading", { name: "Page not found" });
    unavailableDocument.unmount();
  } finally {
    fetchSpy.mockRestore();
    getInbox.mockRestore();
    getMyState.mockRestore();
    listIssues.mockRestore();
    listProjectArtifacts.mockRestore();
    listProjects.mockRestore();
    whoAmI.mockRestore();
    window.matchMedia = originalMatchMedia;
  }
});
