import { expect, jest, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { installFakeLiveSockets } from "./__tests__/live-socket-harness";
import { ApiError, api, NetworkError } from "./api/client";
import { liveSocketFor } from "./api/live-socket";
import type { AuthenticatedUser } from "./api/types";
import { AuthGate } from "./app";
import { SendStoppedError } from "./features/writes/keyed-write";
import { plainWrite, writesOf } from "./features/writes/write-store";

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
    return { event_head: "0", kind: "user", login: "alice" };
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

/** A write alice pressed, made under the app's own store: each attempt is recorded with the login
 *  it went under, and fails as a dropped connection does, so the store keeps trying it; one with
 *  `refuse` is refused by Dispatch instead and stays for Retry. */
function aliceWrite(
  queryClient: QueryClient,
  attempts: string[],
  refuse = false
): Promise<unknown> {
  return writesOf(queryClient)
    .run({
      detail: null,
      kind: plainWrite,
      label: refuse ? "CORE-2 set to done" : "CORE-1 set to done",
      presentation: "indicator",
      write: (bounds) => {
        attempts.push(bounds.login ?? "nobody");
        bounds.sent?.({ body: { status: "done" }, method: "PATCH", path: "/api/v1/issues/CORE-1" });
        return Promise.reject(
          refuse
            ? new ApiError(422, { code: "ISSUE_INPUT", error: "refused" })
            : new NetworkError("/api/v1/issues/CORE-1", new TypeError("Failed to fetch"))
        );
      },
    })
    .catch((error: unknown) => error);
}

/** The names of the writes this tab keeps across a reload. */
function keptWrites(): string[] {
  return Array.from({ length: window.sessionStorage.length }, (_, index) =>
    window.sessionStorage.key(index)
  ).filter((name): name is string => name?.startsWith("dispatch.write:") === true);
}

// W26. Someone else signs in from another tab of this browser: the session cookie is the browser's,
// so this tab's writes would go out as bob. The other tab tells this one (`dispatch-sign-in`), it
// reads who is signed in again, and every write alice pressed - one still being tried, one refused
// and waiting for her Retry - is dropped, in memory and in the tab's storage, its trying stopped,
// and nothing more goes out at all, as alice or as bob.
test("a sign-in from another tab drops every write of the person before, stops it and sends nothing", async () => {
  const sockets = installFakeLiveSockets();
  let login = "alice";
  const whoAmI = spyOn(api, "whoAmI").mockImplementation(async () => ({
    event_head: "0",
    kind: "user",
    login,
  }));
  const spies = [
    spyOn(api, "listIssues").mockResolvedValue([]),
    spyOn(api, "getMyState").mockResolvedValue({}),
    spyOn(api, "getInbox").mockResolvedValue([]),
  ];
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
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter initialEntries={["/"]}>
      <QueryClientProvider client={queryClient}>
        <AuthGate />
      </QueryClientProvider>
    </MemoryRouter>
  );
  const otherTab = new BroadcastChannel("dispatch-sign-in");
  try {
    await screen.findByText("Signed in as alice");
    const attempts: string[] = [];
    const out = aliceWrite(queryClient, attempts);
    const refused = aliceWrite(queryClient, attempts, true);
    await waitFor(() =>
      expect(
        writesOf(queryClient)
          .rows()
          .map((row) => row.status)
      ).toEqual(["sending", "refused"])
    );
    expect(keptWrites()).toHaveLength(2);
    expect(await refused).toBeInstanceOf(ApiError);

    login = "bob";
    act(() => otherTab.postMessage("bob"));
    await screen.findByText("Signed in as bob");
    await waitFor(() => expect(writesOf(queryClient).rows()).toEqual([]));
    expect(await out).toBeInstanceOf(SendStoppedError);
    expect(keptWrites()).toEqual([]);
    const made = attempts.length;
    // Past the first retry's backoff: nothing more went out, under anyone's name.
    await new Promise((resolve) => setTimeout(resolve, 2_500));
    expect(attempts).toHaveLength(made);
    expect(attempts.every((attempted) => attempted === "alice")).toBe(true);
    expect(whoAmI.mock.calls.length).toBeGreaterThan(1);
  } finally {
    otherTab.close();
    view.unmount();
    liveSocketFor(queryClient).close();
    sockets.restore();
    whoAmI.mockRestore();
    for (const spy of spies) spy.mockRestore();
    window.matchMedia = originalMatchMedia;
  }
}, 15_000);

// W32. A session that lapses keeps the person's own writes: whoami answers 401 and the page shows
// the sign-in page, every write stops trying and nothing more goes out, but both writes - one out,
// one refused and holding Retry - stay, in memory and in the tab's storage. Once the same person
// signs in again, the one out goes on under its key and lands, and the refusal is still there.
test("a lapsed session keeps every write, and the same person's sign-in resumes them", async () => {
  const sockets = installFakeLiveSockets();
  let signedIn = true;
  const whoAmI = spyOn(api, "whoAmI").mockImplementation(async () => {
    if (!signedIn) throw new ApiError(401, { error: "unauthorized" });
    return { event_head: "0", kind: "user", login: "alice" };
  });
  const spies = [
    spyOn(api, "listIssues").mockResolvedValue([]),
    spyOn(api, "getMyState").mockResolvedValue({}),
    spyOn(api, "getInbox").mockResolvedValue([]),
  ];
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
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter initialEntries={["/"]}>
      <QueryClientProvider client={queryClient}>
        <AuthGate />
      </QueryClientProvider>
    </MemoryRouter>
  );
  const rereadWhoAmI = () =>
    act(() => void queryClient.invalidateQueries({ queryKey: ["whoami"] }));
  try {
    await screen.findByText("Signed in as alice");
    const keys: string[] = [];
    let up = false;
    const out = writesOf(queryClient).run({
      detail: null,
      kind: plainWrite,
      label: "CORE-1 set to done",
      presentation: "indicator",
      write: (bounds) => {
        keys.push(bounds.key);
        bounds.sent?.({ body: { status: "done" }, method: "PATCH", path: "/api/v1/issues/CORE-1" });
        return up
          ? Promise.resolve({ key: "CORE-1" })
          : Promise.reject(new NetworkError("/api/v1/issues/CORE-1", new TypeError("Failed")));
      },
    });
    const attempts: string[] = [];
    void aliceWrite(queryClient, attempts, true);
    await waitFor(() =>
      expect(
        writesOf(queryClient)
          .rows()
          .map((row) => row.status)
      ).toEqual(["sending", "refused"])
    );

    signedIn = false;
    await rereadWhoAmI();
    await screen.findByRole("link", { name: "Sign in with Google" });
    await waitFor(() =>
      expect(
        writesOf(queryClient)
          .rows()
          .map((row) => row.status)
      ).toEqual(["queued", "refused"])
    );
    expect(keptWrites()).toHaveLength(2);
    const made = keys.length;
    const quiet = Promise.withResolvers<void>();
    setTimeout(quiet.resolve, 2_500);
    await quiet.promise;
    expect(keys).toHaveLength(made);

    signedIn = true;
    up = true;
    await rereadWhoAmI();
    await screen.findByText("Signed in as alice");
    expect(await out).toEqual({ key: "CORE-1" });
    expect(new Set(keys).size).toBe(1);
    expect(writesOf(queryClient).rows()).toMatchObject([
      { label: "CORE-2 set to done", status: "refused" },
    ]);
    expect(keptWrites()).toHaveLength(1);
  } finally {
    view.unmount();
    liveSocketFor(queryClient).close();
    sockets.restore();
    whoAmI.mockRestore();
    for (const spy of spies) spy.mockRestore();
    window.matchMedia = originalMatchMedia;
  }
}, 15_000);

// (b) Signing out discards the person's own writes, so it asks first while any is unsent, naming
// each: Cancel keeps them signed in with every write as it was. Confirmed, and once Dispatch has
// answered the sign-out (W48), every write - one still being tried, a refusal waiting for Retry -
// stops and goes, nothing of either is left in the tab, the page stops asking to confirm leaving,
// and every other tab of the browser is told whose writes to drop.
test("signing out asks first, then drops every write here and tells every other tab", async () => {
  const sockets = installFakeLiveSockets();
  let signedIn = true;
  const whoAmI = spyOn(api, "whoAmI").mockImplementation(async () => {
    if (!signedIn) throw new ApiError(401, { error: "unauthorized" });
    return { event_head: "0", kind: "user", login: "alice" };
  });
  const logout = spyOn(api, "logout").mockImplementation(async () => {
    signedIn = false;
    return { ok: true };
  });
  const spies = [
    spyOn(api, "listIssues").mockResolvedValue([]),
    spyOn(api, "getMyState").mockResolvedValue({}),
    spyOn(api, "getInbox").mockResolvedValue([]),
  ];
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
  const originalConfirm = window.confirm;
  const asked: string[] = [];
  let answer = false;
  window.confirm = (message?: string) => {
    asked.push(message ?? "");
    return answer;
  };
  const otherTab = new BroadcastChannel("dispatch-sign-in");
  const told: unknown[] = [];
  otherTab.addEventListener("message", (event) => told.push(event.data));
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter initialEntries={["/"]}>
      <QueryClientProvider client={queryClient}>
        <AuthGate />
      </QueryClientProvider>
    </MemoryRouter>
  );
  const asksBeforeLeaving = () => {
    const leaving = new Event("beforeunload", { cancelable: true });
    window.dispatchEvent(leaving);
    return leaving.defaultPrevented;
  };
  try {
    await screen.findByText("Signed in as alice");
    const attempts: string[] = [];
    const out = aliceWrite(queryClient, attempts);
    void aliceWrite(queryClient, attempts, true);
    await waitFor(() => expect(writesOf(queryClient).rows()).toHaveLength(2));
    expect(keptWrites()).toHaveLength(2);
    expect(asksBeforeLeaving()).toBe(true);

    fireEvent.click(screen.getByRole("button", { name: "Sign out" }));
    await waitFor(() => expect(asked).toHaveLength(1));
    expect(asked).toEqual([
      "Sign out and discard 2 unsent changes?\n\n• CORE-1 set to done\n• CORE-2 set to done",
    ]);
    expect(logout).not.toHaveBeenCalled();
    expect(screen.getByText("Signed in as alice")).toBeTruthy();
    expect(writesOf(queryClient).rows()).toHaveLength(2);
    expect(keptWrites()).toHaveLength(2);

    answer = true;
    fireEvent.click(screen.getByRole("button", { name: "Sign out" }));
    await waitFor(() => expect(writesOf(queryClient).rows()).toEqual([]));
    await screen.findByRole("link", { name: "Sign in with Google" });
    expect(logout).toHaveBeenCalledTimes(1);
    expect(await out).toBeInstanceOf(SendStoppedError);
    expect(keptWrites()).toEqual([]);
    expect(asksBeforeLeaving()).toBe(false);
    await waitFor(() =>
      expect(told).toContainEqual(expect.objectContaining({ signedOut: "alice" }))
    );
    const made = attempts.length;
    const quiet = Promise.withResolvers<void>();
    setTimeout(quiet.resolve, 2_500);
    await quiet.promise;
    expect(attempts).toHaveLength(made);
  } finally {
    otherTab.close();
    window.confirm = originalConfirm;
    view.unmount();
    liveSocketFor(queryClient).close();
    sockets.restore();
    whoAmI.mockRestore();
    logout.mockRestore();
    for (const spy of spies) spy.mockRestore();
    window.matchMedia = originalMatchMedia;
  }
}, 15_000);

// (b) The person signed out in another tab of this browser: this tab drops every write they pressed
// - out or refused - and stops trying it, so no tab holds a write whose kept file another tab let
// go of. A session that lapses with no sign-out keeps them instead (the W32 test above).
test("a sign-out in another tab drops that person's writes here", async () => {
  const sockets = installFakeLiveSockets();
  const whoAmI = spyOn(api, "whoAmI").mockResolvedValue({
    event_head: "0",
    kind: "user",
    login: "alice",
  });
  const spies = [
    spyOn(api, "listIssues").mockResolvedValue([]),
    spyOn(api, "getMyState").mockResolvedValue({}),
    spyOn(api, "getInbox").mockResolvedValue([]),
  ];
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
  const otherTab = new BroadcastChannel("dispatch-sign-in");
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
    const attempts: string[] = [];
    const out = aliceWrite(queryClient, attempts);
    void aliceWrite(queryClient, attempts, true);
    await waitFor(() => expect(writesOf(queryClient).rows()).toHaveLength(2));
    expect(keptWrites()).toHaveLength(2);

    act(() => otherTab.postMessage({ signedOut: "alice" }));
    await waitFor(() => expect(writesOf(queryClient).rows()).toEqual([]));
    expect(await out).toBeInstanceOf(SendStoppedError);
    expect(keptWrites()).toEqual([]);
    const made = attempts.length;
    const quiet = Promise.withResolvers<void>();
    setTimeout(quiet.resolve, 2_500);
    await quiet.promise;
    expect(attempts).toHaveLength(made);
  } finally {
    otherTab.close();
    view.unmount();
    liveSocketFor(queryClient).close();
    sockets.restore();
    whoAmI.mockRestore();
    for (const spy of spies) spy.mockRestore();
    window.matchMedia = originalMatchMedia;
  }
}, 15_000);

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
  const whoAmI = spyOn(api, "whoAmI").mockResolvedValue({
    event_head: "0",
    kind: "user",
    login: "alice",
  });
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([]);
  const getMyState = spyOn(api, "getMyState").mockResolvedValue({});
  const listIssues = spyOn(api, "listIssues").mockResolvedValue([]);
  const listProjects = spyOn(api, "listProjects").mockResolvedValue([
    { created_at: "2026-09-10T00:00:00Z", key: "CORE", name: "Core", open_asks: 0 },
  ]);
  const listProjectArtifacts = spyOn(api, "listProjectArtifacts").mockResolvedValue([]);
  // No architecture source: the bare project path opens on Issues.
  const listProjectArchitectureSources = spyOn(
    api,
    "listProjectArchitectureSources"
  ).mockResolvedValue([]);
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
    listProjectArchitectureSources.mockRestore();
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
  const whoAmI = spyOn(api, "whoAmI").mockResolvedValue({
    event_head: "0",
    kind: "user",
    login: "alice",
  });
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

// W16. A sign-in check that comes back 401 ends this page's session, so every read it holds goes,
// as a sign-out's do: the sign-in that follows answers with an `event_head` of its own, which the
// feed resumes from, and a row read before it would be older than that head with nothing to
// bring it up to date. A transient failure (above) is not that: it keeps what the page holds.
test("a whoami 401 drops every other read the page holds", async () => {
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
  const whoAmI = spyOn(api, "whoAmI").mockRejectedValue(new ApiError(401, { error: "no session" }));
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([]);
  const getMyState = spyOn(api, "getMyState").mockResolvedValue({});
  const listIssues = spyOn(api, "listIssues").mockResolvedValue([]);
  const listProjects = spyOn(api, "listProjects").mockResolvedValue([]);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  queryClient.setQueryData(["whoami"], { event_head: "41", kind: "user", login: "alice" });
  queryClient.setQueryData(["issue", "CORE-1"], { key: "CORE-1", title: "Read before the 401" });
  const view = render(
    <MemoryRouter initialEntries={["/"]}>
      <QueryClientProvider client={queryClient}>
        <AuthGate />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    await screen.findByRole("link", { name: "Sign in with Google" });
    await waitFor(() => expect(queryClient.getQueryData(["issue", "CORE-1"])).toBeUndefined());
    // The check itself stays, so the gate keeps showing sign-in rather than reading it again.
    expect(queryClient.getQueryState(["whoami"])?.status).toBe("error");
  } finally {
    view.unmount();
    getInbox.mockRestore();
    getMyState.mockRestore();
    listIssues.mockRestore();
    listProjects.mockRestore();
    whoAmI.mockRestore();
    window.matchMedia = originalMatchMedia;
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
  const whoAmI = spyOn(api, "whoAmI").mockResolvedValue({
    event_head: "0",
    kind: "user",
    login: "alice",
  });
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
async function renderSignedIn(whoAmI: () => Promise<AuthenticatedUser>) {
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
    event_head: "0",
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
    return { event_head: "0", kind: "user", login: "alice" };
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
