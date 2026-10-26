import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { installFakeLiveSockets } from "./__tests__/live-socket-harness";
import { ApiError, api, NetworkError } from "./api/client";
import { liveSocketFor } from "./api/live-socket";
import { AuthGate } from "./app";
import { plainWrite, writesOf } from "./features/writes/write-store";

/** The app at `/`, its whoami answered by `whoAmI` and its sign-out by `logout`, with window's
 *  confirm answered by `confirm` and every question it asked recorded. */
function renderApp({
  confirm = () => true,
  logout = async () => ({ ok: true as const }),
  whoAmI,
}: {
  confirm?: () => boolean;
  logout?: () => Promise<{ ok: true }>;
  whoAmI: () => Promise<{ event_head: string; kind: "user"; login: string }>;
}) {
  const sockets = installFakeLiveSockets();
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
  window.confirm = (message?: string) => {
    asked.push(message ?? "");
    return confirm();
  };
  const spies = [
    spyOn(api, "whoAmI").mockImplementation(whoAmI),
    spyOn(api, "listIssues").mockResolvedValue([]),
    spyOn(api, "getMyState").mockResolvedValue({}),
    spyOn(api, "getInbox").mockResolvedValue([]),
  ];
  const logoutSpy = spyOn(api, "logout").mockImplementation(logout);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter initialEntries={["/"]}>
      <QueryClientProvider client={queryClient}>
        <AuthGate />
      </QueryClientProvider>
    </MemoryRouter>
  );
  return {
    asked,
    logout: logoutSpy,
    queryClient,
    teardown: () => {
      view.unmount();
      liveSocketFor(queryClient).close();
      sockets.restore();
      for (const spy of spies) spy.mockRestore();
      logoutSpy.mockRestore();
      window.matchMedia = originalMatchMedia;
      window.confirm = originalConfirm;
    },
  };
}

/** A write alice pressed: one whose attempts keep failing at the connection, so it is tried
 *  again and again, or - `refuse` - one Dispatch refused, waiting for her Retry. Each attempt is
 *  recorded. */
function aliceWrite(
  queryClient: QueryClient,
  attempts: string[],
  label: string,
  refuse = false
): Promise<unknown> {
  return writesOf(queryClient)
    .run({
      detail: null,
      kind: plainWrite,
      label,
      presentation: "indicator",
      write: (bounds) => {
        attempts.push(label);
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

/** Another tab of this browser on the sign-in channel, named `id`: it says it opened, records
 *  everything it hears, and answers a sign-out's question with `unsent` - or, `silent`, not at
 *  all. `close` says it went. */
function otherTab(id: string, unsent: readonly string[] | "silent" = []) {
  const channel = new BroadcastChannel("dispatch-sign-in");
  const told: unknown[] = [];
  channel.addEventListener("message", (event: MessageEvent) => {
    told.push(event.data);
    const data: unknown = event.data;
    if (typeof data !== "object" || data === null || unsent === "silent") return;
    const ask: unknown = Reflect.get(data, "ask");
    if (typeof ask === "string") channel.postMessage({ answer: ask, from: id, unsent });
  });
  channel.postMessage({ hello: true, tab: id });
  return {
    close: () => {
      channel.postMessage({ gone: true, tab: id });
      channel.close();
    },
    told,
  };
}

function sleep(ms: number): Promise<void> {
  const { promise, resolve } = Promise.withResolvers<void>();
  setTimeout(resolve, ms);
  return promise;
}

// W48. A sign-out discards only once Dispatch has answered it. While it is out nothing more goes
// out here - every write stops trying after the attempt in flight - and every other tab is told to
// hold the person's writes too. Refused, it leaves the person signed in with every write, here and
// in every other tab, and says the sign-out did not go through; its Retry, once it lands, drops
// every write here and tells every other tab whose to drop.
test("a sign-out discards only once Dispatch answers it, and one that fails keeps every write", async () => {
  const first = Promise.withResolvers<{ ok: true }>();
  let signedIn = true;
  let logouts = 0;
  const app = renderApp({
    logout: () => {
      logouts += 1;
      if (logouts === 1) return first.promise;
      signedIn = false;
      return Promise.resolve({ ok: true });
    },
    whoAmI: async () => {
      if (!signedIn) throw new ApiError(401, { error: "unauthorized" });
      return { event_head: "0", kind: "user", login: "alice" };
    },
  });
  const tab = otherTab("tab-2");
  try {
    await screen.findByText("Signed in as alice");
    await waitFor(() => expect(tab.told).toContainEqual(expect.objectContaining({ here: true })));
    const attempts: string[] = [];
    void aliceWrite(app.queryClient, attempts, "CORE-1 set to done");
    void aliceWrite(app.queryClient, attempts, "CORE-2 set to done", true);
    await waitFor(() => expect(writesOf(app.queryClient).rows()).toHaveLength(2));

    fireEvent.click(screen.getByRole("button", { name: "Sign out" }));
    await waitFor(() => expect(app.logout).toHaveBeenCalledTimes(1));
    expect(app.asked).toHaveLength(1);
    await waitFor(() => expect(tab.told).toContainEqual({ signingOut: "alice" }));
    const made = attempts.length;
    await sleep(2_500);
    expect(attempts).toHaveLength(made);
    expect(writesOf(app.queryClient).rows()).toHaveLength(3);
    expect(keptWrites()).toHaveLength(3);

    act(() => first.reject(new ApiError(403, { code: "FORBIDDEN", error: "not now" })));
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("The sign-out did not go through.");
    expect(screen.getByText("Signed in as alice")).toBeTruthy();
    expect(
      writesOf(app.queryClient)
        .rows()
        .map((row) => row.label)
    ).toEqual(["CORE-1 set to done", "CORE-2 set to done"]);
    expect(keptWrites()).toHaveLength(2);
    await waitFor(() => expect(tab.told).toContainEqual({ stillSignedIn: "alice" }));
    expect(tab.told).not.toContainEqual({ signedOut: "alice" });
    await waitFor(() => expect(attempts.length).toBeGreaterThan(made));

    fireEvent.click(within(alert).getByRole("button", { name: "Retry" }));
    await screen.findByRole("link", { name: "Sign in with Google" });
    expect(writesOf(app.queryClient).rows()).toEqual([]);
    expect(keptWrites()).toEqual([]);
    await waitFor(() => expect(tab.told).toContainEqual({ signedOut: "alice" }));
  } finally {
    tab.close();
    app.teardown();
  }
}, 15_000);

// W45. The sign-out question names every tab's unsent changes: it asks the other tabs of this
// browser first, over the sign-in channel, and lists what each of them would discard beside this
// tab's own. A tab it knows of that does not answer within 300 ms is named as such.
test("the sign-out question names every tab's unsent changes, and says when a tab did not answer", async () => {
  const answer = false;
  const app = renderApp({
    confirm: () => answer,
    whoAmI: async () => ({ event_head: "0", kind: "user", login: "alice" }),
  });
  const second = otherTab("tab-2", ["CORE-4 set to backlog", "Labels of CORE-4"]);
  try {
    await screen.findByText("Signed in as alice");
    await waitFor(() =>
      expect(second.told).toContainEqual(expect.objectContaining({ here: true }))
    );
    const attempts: string[] = [];
    void aliceWrite(app.queryClient, attempts, "CORE-1 set to done");
    await waitFor(() => expect(writesOf(app.queryClient).rows()).toHaveLength(1));

    fireEvent.click(screen.getByRole("button", { name: "Sign out" }));
    await waitFor(() => expect(app.asked).toHaveLength(1));
    expect(app.asked[0]).toBe(
      "Sign out and discard 3 unsent changes?\n\n• CORE-1 set to done\n• CORE-4 set to backlog\n• Labels of CORE-4"
    );
    expect(second.told).toContainEqual(expect.objectContaining({ unsentOf: "alice" }));
    expect(app.logout).not.toHaveBeenCalled();

    const silent = otherTab("tab-3", "silent");
    try {
      await waitFor(() =>
        expect(silent.told).toContainEqual(expect.objectContaining({ here: true }))
      );
      const started = Date.now();
      fireEvent.click(screen.getByRole("button", { name: "Sign out" }));
      await waitFor(() => expect(app.asked).toHaveLength(2));
      expect(Date.now() - started).toBeGreaterThanOrEqual(250);
      expect(app.asked[1]).toBe(
        "Sign out and discard 3 unsent changes, and any unsent changes in your other Dispatch tabs?\n\n• CORE-1 set to done\n• CORE-4 set to backlog\n• Labels of CORE-4"
      );
    } finally {
      silent.close();
    }
    expect(app.logout).not.toHaveBeenCalled();
    expect(writesOf(app.queryClient).rows()).toHaveLength(1);
  } finally {
    second.close();
    app.teardown();
  }
}, 15_000);

// W43. A lapsed tab shows the writes it keeps: the sign-in page lists the lapsed person's kept
// writes by name, so they see what waits for them, and a refused one offers Discard there.
test("a lapsed tab's sign-in page lists the writes it keeps, a refused one with Discard", async () => {
  let signedIn = true;
  const app = renderApp({
    whoAmI: async () => {
      if (!signedIn) throw new ApiError(401, { error: "unauthorized" });
      return { event_head: "0", kind: "user", login: "alice" };
    },
  });
  try {
    await screen.findByText("Signed in as alice");
    const attempts: string[] = [];
    void aliceWrite(app.queryClient, attempts, "CORE-7 set to todo");
    void aliceWrite(app.queryClient, attempts, "CORE-7 set to backlog", true);
    await waitFor(() => expect(writesOf(app.queryClient).rows()).toHaveLength(2));

    signedIn = false;
    await act(() => app.queryClient.invalidateQueries({ queryKey: ["whoami"] }));
    const kept = await screen.findByRole("region", { name: "Kept changes" });
    expect(kept.textContent).toContain("Kept until you sign in again as alice:");
    expect(kept.textContent).toContain("CORE-7 set to todo");
    expect(kept.textContent).toContain("CORE-7 set to backlog");
    const buttons = within(kept).getAllByRole("button", { name: "Discard" });
    expect(buttons).toHaveLength(1);
    fireEvent.click(buttons[0] as HTMLElement);
    await waitFor(() =>
      expect(
        writesOf(app.queryClient)
          .rows()
          .map((row) => row.label)
      ).toEqual(["CORE-7 set to todo"])
    );
    expect(kept.textContent).not.toContain("CORE-7 set to backlog");
    expect(keptWrites()).toHaveLength(1);
  } finally {
    app.teardown();
  }
}, 15_000);

// W44. The sign-in link of a lapsed tab raises no leave prompt: the writes it keeps resume in this
// tab once the person is back, so its own navigation is no leaving. Any other way out still asks.
test("the sign-in link of a lapsed tab raises no leave prompt", async () => {
  let signedIn = true;
  const app = renderApp({
    whoAmI: async () => {
      if (!signedIn) throw new ApiError(401, { error: "unauthorized" });
      return { event_head: "0", kind: "user", login: "alice" };
    },
  });
  const asksBeforeLeaving = () => {
    const leaving = new Event("beforeunload", { cancelable: true });
    window.dispatchEvent(leaving);
    return leaving.defaultPrevented;
  };
  // The test's page does not navigate: the link's own handler runs, and the click stops there.
  const stay = (event: Event) => event.preventDefault();
  window.addEventListener("click", stay);
  try {
    await screen.findByText("Signed in as alice");
    const attempts: string[] = [];
    void aliceWrite(app.queryClient, attempts, "CORE-7 set to todo");
    await waitFor(() => expect(writesOf(app.queryClient).rows()).toHaveLength(1));
    signedIn = false;
    await act(() => app.queryClient.invalidateQueries({ queryKey: ["whoami"] }));
    const link = await screen.findByRole("link", { name: "Sign in with Google" });
    expect(asksBeforeLeaving()).toBe(true);

    fireEvent.click(link);
    expect(asksBeforeLeaving()).toBe(false);
    // Back on the page - the navigation never happened - leaving asks again.
    fireEvent.pointerDown(document.body);
    expect(asksBeforeLeaving()).toBe(true);
  } finally {
    window.removeEventListener("click", stay);
    app.teardown();
  }
}, 15_000);
