import { expect, jest, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { installFakeLiveSockets } from "./__tests__/live-socket-harness";
import { ApiError, api } from "./api/client";
import { liveSocketFor } from "./api/live-socket";
import type { AuthenticatedUser } from "./api/types";
import { AuthGate } from "./app";

test("signing out shows the sign-in page without a reload and ends the event feed's socket", async () => {
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

  const sockets = installFakeLiveSockets();

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
    const socket = sockets.latest();
    act(() => socket.open());
    const { sub } = socket.subscribed("events");
    expect(socket.closedWith).toBeUndefined();

    fireEvent.click(screen.getByRole("button", { name: "Sign out" }));

    await waitFor(() => expect(logout).toHaveBeenCalled());
    await screen.findByRole("link", { name: "Sign in with Google" });

    // The feed left with the signed-in shell, and with it the socket's last subscription.
    expect(socket.unsubscribes()).toEqual([{ type: "unsubscribe", sub }]);
    await waitFor(() => expect(socket.closedWith).toBe(1000));
  } finally {
    view.unmount();
    window.matchMedia = originalMatchMedia;
    whoAmI.mockRestore();
    logout.mockRestore();
    listIssues.mockRestore();
    getMyState.mockRestore();
    getInbox.mockRestore();
    liveSocketFor(queryClient).close();
    sockets.restore();
  }
});

// INFRA-292's precondition. The event feed's first subscription resumes from the sign-in check's
// `event_head`, which is sound only while every read the page makes, and its socket, wait for that
// answer: a read made before it could miss an event committed between the read and the head, and
// the replay from the head would not bring that event back.
test("nothing is read and no socket opens until whoami answers, and the feed resumes from its event_head", async () => {
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
  const signedIn = Promise.withResolvers<AuthenticatedUser>();
  const whoAmI = spyOn(api, "whoAmI").mockReturnValue(signedIn.promise);
  // Every other method of the client, held unanswered: one called before whoami answers is the
  // failure under test, and the reads after it never answer, which nothing here waits on.
  const client = Object.getPrototypeOf(api) as Record<string, unknown>;
  const methods = Object.getOwnPropertyNames(client).filter(
    (name) => name !== "constructor" && name !== "whoAmI" && typeof client[name] === "function"
  );
  const held = methods.map((name) =>
    spyOn(api as unknown as Record<string, () => Promise<unknown>>, name).mockImplementation(
      () => Promise.withResolvers<never>().promise
    )
  );
  const called = () => methods.filter((_, index) => (held[index]?.mock.calls.length ?? 0) > 0);
  const sockets = installFakeLiveSockets();
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter initialEntries={["/"]}>
      <QueryClientProvider client={queryClient}>
        <AuthGate />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    await waitFor(() => expect(whoAmI).toHaveBeenCalledTimes(1));
    expect(called()).toEqual([]);
    expect(sockets.sockets).toHaveLength(0);

    await act(async () => signedIn.resolve({ event_head: "41", kind: "user", login: "alice" }));
    await waitFor(() => expect(sockets.sockets).toHaveLength(1));
    const socket = sockets.latest();
    act(() => socket.open());
    const [first] = socket.subscribes().filter((frame) => frame.view.kind === "events");
    expect(first?.resume).toEqual({ since: "41" });
  } finally {
    view.unmount();
    window.matchMedia = originalMatchMedia;
    whoAmI.mockRestore();
    for (const spy of held) spy.mockRestore();
    liveSocketFor(queryClient).close();
    sockets.restore();
  }
});

test("project routes render the project page and a project document route", async () => {
  // React.lazy loads each page module on its first render (app.tsx). Loading the two pages this
  // test routes to first keeps their cold compile, hundreds of milliseconds on a loaded host, out
  // of findBy's one-second window, which is for the routing under test.
  await Promise.all([
    import("./features/project/ProjectPage"),
    import("./features/document/DocumentPage"),
  ]);
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
  // No architecture source: the bare project path opens on Issues.
  const getArchitectureSource = spyOn(api, "getArchitectureSource").mockResolvedValue(null);
  const getProjectArtifact = spyOn(api, "getProjectArtifact").mockResolvedValue({
    created_at: "2026-09-10T00:00:00Z",
    created_by: { id: "alice", kind: "user" },
    id: "artifact-1",
    issue_key: null,
    kind: "doc" as const,
    name: "Design notes",
    primary: false,
    project: "CORE",
    slug: "design-notes",
    versions: [],
  });
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

    const document = renderRoute("/projects/CORE/documents/design-notes");
    await screen.findByTestId("artifact-header");
    expect(screen.getByRole("link", { name: "Project CORE" }).getAttribute("href")).toBe(
      "/projects/CORE/documents"
    );
    document.unmount();
  } finally {
    getArchitectureSource.mockRestore();
    getInbox.mockRestore();
    getProjectArtifact.mockRestore();
    getMyState.mockRestore();
    listIssues.mockRestore();
    listProjectArtifacts.mockRestore();
    listProjects.mockRestore();
    whoAmI.mockRestore();
    window.matchMedia = originalMatchMedia;
  }
});

test("the Inbox page links to the answers page, which renders at /answers", async () => {
  // React.lazy loads the page module on its first render; loading it first keeps its cold
  // compile out of findBy's window, as the project-routes test above does.
  await import("./features/answers/AnswersPage");
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
  const listProjects = spyOn(api, "listProjects").mockResolvedValue([]);
  const listMyAnswers = spyOn(api, "listMyAnswers").mockResolvedValue({
    limit: 50,
    offset: 0,
    rows: [],
    total: 0,
  });
  const renderRoute = (path: string) =>
    render(
      <MemoryRouter initialEntries={[path]}>
        <QueryClientProvider
          client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}
        >
          <AuthGate />
        </QueryClientProvider>
      </MemoryRouter>
    );

  try {
    const inbox = renderRoute("/");
    await screen.findByRole("heading", { name: "Inbox" });
    expect(screen.getByRole("link", { name: "Answered by you" }).getAttribute("href")).toBe(
      "/answers"
    );
    inbox.unmount();

    const answers = renderRoute("/answers");
    expect(await screen.findByRole("heading", { name: "Answered by you" })).toBeTruthy();
    expect(await screen.findByText("You have not answered or replied on an ask yet.")).toBeTruthy();
    answers.unmount();
  } finally {
    getInbox.mockRestore();
    getMyState.mockRestore();
    listIssues.mockRestore();
    listMyAnswers.mockRestore();
    listProjects.mockRestore();
    whoAmI.mockRestore();
    window.matchMedia = originalMatchMedia;
  }
});

test("a failed identity refetch keeps the cached user's sidebar preference", async () => {
  const sidebarStorageKey = "dispatch.shell.sidebar:alice";
  const originalMatchMedia = window.matchMedia;
  const originalInnerWidth = window.innerWidth;
  const innerWidthDescriptor = Object.getOwnPropertyDescriptor(window, "innerWidth");
  Object.defineProperty(window, "innerWidth", { configurable: true, value: 1280 });
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
  window.localStorage.setItem(sidebarStorageKey, "hidden");
  const whoAmI = spyOn(api, "whoAmI").mockRejectedValue(new Error("temporary outage"));
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([]);
  const getMyState = spyOn(api, "getMyState").mockResolvedValue({});
  const listIssues = spyOn(api, "listIssues").mockResolvedValue([]);
  const listProjects = spyOn(api, "listProjects").mockResolvedValue([]);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  queryClient.setQueryData(["whoami"], { kind: "user", login: "alice" });
  const view = render(
    <MemoryRouter initialEntries={["/"]}>
      <QueryClientProvider client={queryClient}>
        <AuthGate />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    await waitFor(() => expect(queryClient.getQueryState(["whoami"])?.status).toBe("error"));
    expect(screen.getByRole("button", { name: "Show sidebar" })).toBeTruthy();
  } finally {
    view.unmount();
    getInbox.mockRestore();
    getMyState.mockRestore();
    listIssues.mockRestore();
    listProjects.mockRestore();
    whoAmI.mockRestore();
    window.localStorage.removeItem(sidebarStorageKey);
    window.matchMedia = originalMatchMedia;
    if (innerWidthDescriptor === undefined) {
      Object.defineProperty(window, "innerWidth", {
        configurable: true,
        value: originalInnerWidth,
      });
    } else {
      Object.defineProperty(window, "innerWidth", innerWidthDescriptor);
    }
  }
});
test("desktop shell persists collapsed sidebars and gives the main region the full layout", async () => {
  const sidebarStorageKey = "dispatch.shell.sidebar:alice";
  const marginStorageKey = "dispatch.shell.margin:alice";
  const marginWidthStorageKey = "dispatch.shell.margin-width:alice";
  const originalMatchMedia = window.matchMedia;
  const originalInnerWidth = window.innerWidth;
  const innerWidthDescriptor = Object.getOwnPropertyDescriptor(window, "innerWidth");
  Object.defineProperty(window, "innerWidth", { configurable: true, value: 1280 });
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
  window.localStorage.removeItem(sidebarStorageKey);
  window.localStorage.removeItem(marginStorageKey);
  window.localStorage.removeItem(marginWidthStorageKey);
  const whoAmI = spyOn(api, "whoAmI").mockResolvedValue({ kind: "user", login: "alice" });
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([]);
  const getMyState = spyOn(api, "getMyState").mockResolvedValue({});
  const listIssues = spyOn(api, "listIssues").mockResolvedValue([]);
  const listProjects = spyOn(api, "listProjects").mockResolvedValue([]);
  // An issue route: the shell's margin controls only exist where the route has a margin, and
  // this test is about the collapse preferences surviving a remount, not about the Inbox.
  const first = render(
    <MemoryRouter initialEntries={["/issues/CORE-1"]}>
      <QueryClientProvider
        client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}
      >
        <AuthGate />
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    await screen.findByRole("navigation", { name: "Navigation" });
    fireEvent.keyDown(screen.getByRole("separator", { name: "Resize margin" }), {
      key: "ArrowLeft",
    });
    await waitFor(() => expect(window.localStorage.getItem(marginWidthStorageKey)).toBe("408"));
    fireEvent.click(screen.getByRole("button", { name: "Hide sidebar" }));
    fireEvent.click(screen.getByRole("button", { name: "Hide margin" }));

    expect(screen.getByRole("button", { name: "Show sidebar" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Show margin" })).toBeTruthy();
    expect(screen.getByTestId("main-content").getAttribute("data-shell-layout")).toBe("full-width");
    const main = screen.getByTestId("main-content");
    expect(main.classList.contains("xl:w-full")).toBe(true);
    expect(main.classList.contains("xl:pl-20")).toBe(true);
    expect(main.classList.contains("xl:pr-20")).toBe(true);
    expect(window.localStorage.getItem(sidebarStorageKey)).toBe("hidden");
    expect(window.localStorage.getItem(marginStorageKey)).toBe("hidden");
  } finally {
    first.unmount();
  }

  const second = render(
    <MemoryRouter initialEntries={["/issues/CORE-1"]}>
      <QueryClientProvider
        client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}
      >
        <AuthGate />
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    await screen.findByRole("button", { name: "Show sidebar" });
    expect(screen.getByRole("button", { name: "Show margin" })).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "Show margin" }));
    expect(screen.getByTestId("desktop-margin-shell").getAttribute("style")).toContain(
      "width: 408px"
    );
    fireEvent.click(screen.getByRole("button", { name: "Show sidebar" }));

    await screen.findByRole("navigation", { name: "Navigation" });
    expect(screen.getByRole("button", { name: "Hide margin" })).toBeTruthy();
    expect(screen.getByTestId("main-content").getAttribute("data-shell-layout")).toBe("standard");
  } finally {
    second.unmount();
    getInbox.mockRestore();
    getMyState.mockRestore();
    listIssues.mockRestore();
    listProjects.mockRestore();
    whoAmI.mockRestore();
    window.localStorage.removeItem(sidebarStorageKey);
    window.localStorage.removeItem(marginStorageKey);
    window.localStorage.removeItem(marginWidthStorageKey);
    window.matchMedia = originalMatchMedia;
    if (innerWidthDescriptor === undefined) {
      Object.defineProperty(window, "innerWidth", {
        configurable: true,
        value: originalInnerWidth,
      });
    } else {
      Object.defineProperty(window, "innerWidth", innerWidthDescriptor);
    }
  }
});

/** The shell at `/`, signed in as alice, its event feed subscribed on the page's live socket;
 *  returns the teardown. */
async function renderSignedIn(whoAmI: () => Promise<{ kind: "user"; login: string }>) {
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
  const spies = [
    spyOn(api, "whoAmI").mockImplementation(whoAmI),
    spyOn(api, "listIssues").mockResolvedValue([]),
    spyOn(api, "getMyState").mockResolvedValue({}),
    spyOn(api, "getInbox").mockResolvedValue([]),
  ];
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter initialEntries={["/"]}>
      <QueryClientProvider client={queryClient}>
        <AuthGate />
      </QueryClientProvider>
    </MemoryRouter>
  );
  await screen.findByText("Signed in as alice");
  return () => {
    view.unmount();
    liveSocketFor(queryClient).close();
    for (const spy of spies) spy.mockRestore();
    window.matchMedia = originalMatchMedia;
  };
}

test("the connection pill follows the live socket: none while it connects, Reconnecting… once lost, and a Reload once two minutes pass", async () => {
  const sockets = installFakeLiveSockets();
  const teardown = await renderSignedIn(async () => ({
    kind: "user",
    login: "alice",
  }));
  // Every timer the socket sets from here on runs on the test's clock.
  jest.useFakeTimers();
  const random = spyOn(Math, "random").mockReturnValue(0.5);
  try {
    expect(sockets.sockets).toHaveLength(1);
    expect(screen.queryByTestId("connection-pill")).toBeNull();
    act(() => sockets.latest().open());
    expect(screen.queryByTestId("connection-pill")).toBeNull();

    act(() => sockets.latest().drop(1006));
    expect(screen.getByTestId("connection-pill").textContent).toBe("Reconnecting…");

    // Every reconnect fails for two minutes: the page says so, and keeps trying underneath.
    act(() => {
      for (let ran = 0; ran < 120_000; ran += 500) {
        jest.advanceTimersByTime(500);
        const socket = sockets.latest();
        if (socket.readyState === 0) socket.drop(1006);
      }
    });
    const pill = screen.getByTestId("connection-pill");
    expect(pill.textContent).toContain("Live updates are not reaching this page");
    expect(within(pill).getByRole("button", { name: "Reload" })).toBeTruthy();

    // The next attempt, at 121 s, opens, and the pill goes.
    const attempts = sockets.sockets.length;
    act(() => jest.advanceTimersByTime(1_000));
    expect(sockets.sockets).toHaveLength(attempts + 1);
    act(() => sockets.latest().open());
    expect(screen.queryByTestId("connection-pill")).toBeNull();

    // A rotation (4001) reopens without one.
    act(() => sockets.latest().drop(4001));
    expect(screen.queryByTestId("connection-pill")).toBeNull();
  } finally {
    // A zero-delay timer scheduled on Bun's fake clock (here the event feed's refresh after the
    // reopen at 121 s) stalls every later `waitFor` in the process once real timers are back,
    // even when it was cleared; moving the clock on past it does not.
    act(() => jest.advanceTimersByTime(1));
    random.mockRestore();
    jest.useRealTimers();
    teardown();
    sockets.restore();
  }
});

test("a live socket refused once the person is signed out takes the page to sign-in", async () => {
  const sockets = installFakeLiveSockets();
  let signedIn = true;
  const teardown = await renderSignedIn(async () => {
    if (!signedIn) throw new ApiError(401, { error: "unauthorized" });
    return { kind: "user", login: "alice" };
  });
  try {
    act(() => sockets.latest().open());
    signedIn = false;
    // Dispatch ends the socket of a caller that no longer resolves; the next handshake is refused.
    act(() => sockets.latest().drop(1008));
    await screen.findByRole("link", { name: "Sign in with Google" });
  } finally {
    teardown();
    sockets.restore();
  }
});
