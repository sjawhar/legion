import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { installFakeLiveSockets } from "./__tests__/live-socket-harness";
import { ApiError, api, NetworkError } from "./api/client";
import { liveSocketFor } from "./api/live-socket";
import { AuthGate } from "./app";
import type { UnsentChange } from "./features/shell/sign-in-channel";
import { plainWrite, writesOf } from "./features/writes/write-store";

/** A message the tabs of this browser post to each other, naming `login`, from whichever tab. */
const about = (field: string, login: string) => expect.objectContaining({ [field]: login });

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

/** Another tab of this browser on the sign-in channel, as a test drives it. */
interface OtherTab {
  readonly close: () => void;
  readonly say: (message: object) => void;
  readonly told: unknown[];
}

/** Another tab of this browser on the sign-in channel, named `id`: it says it opened, records
 *  everything it hears, and answers a sign-out's question with `unsent` - or, `silent`, not at
 *  all. `say` posts a message of its own, naming it; `close` says it went. */
function otherTab(id: string, unsent: readonly UnsentChange[] | "silent" = []): OtherTab {
  const channel = new BroadcastChannel("dispatch-sign-in");
  const told: unknown[] = [];
  channel.addEventListener("message", (event: MessageEvent) => {
    told.push(event.data);
    const data: unknown = event.data;
    if (typeof data !== "object" || data === null || unsent === "silent") return;
    const ask: unknown = Reflect.get(data, "ask");
    if (typeof ask === "string") channel.postMessage({ answer: ask, tab: id, unsent });
  });
  channel.postMessage({ hello: true, tab: id });
  return {
    close: () => {
      channel.postMessage({ gone: true, tab: id });
      channel.close();
    },
    say: (message: object) => channel.postMessage({ ...message, tab: id }),
    told,
  };
}

/** A change a sign-out would discard, as a tab names it: by its key and label. */
const change = (label: string, key = `key of ${label}`): UnsentChange => ({ key, label });

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
    await waitFor(() => expect(tab.told).toContainEqual(about("signingOut", "alice")));
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
    await waitFor(() => expect(tab.told).toContainEqual(about("stillSignedIn", "alice")));
    expect(tab.told).not.toContainEqual(about("signedOut", "alice"));
    await waitFor(() => expect(attempts.length).toBeGreaterThan(made));

    fireEvent.click(within(alert).getByRole("button", { name: "Retry" }));
    await screen.findByRole("link", { name: "Sign in with Google" });
    expect(writesOf(app.queryClient).rows()).toEqual([]);
    expect(keptWrites()).toEqual([]);
    await waitFor(() => expect(tab.told).toContainEqual(about("signedOut", "alice")));
  } finally {
    tab.close();
    app.teardown();
  }
}, 15_000);

// W50, W60. A sign-out Dispatch answers `401 NO_IDENTITY` has landed: the session it would end is
// already gone - its answer was lost and the retry found the session ended, or a sign-out in
// another browser revoked it. The changes the person chose to discard go, here and in every other
// tab: the sign-in page keeps none of them, and none is sent once they sign in again.
test("a sign-out answered 401 discards what it said it would, and nothing goes out at the next sign-in", async () => {
  let signedIn = true;
  const app = renderApp({
    logout: async () => {
      signedIn = false;
      throw new ApiError(401, { code: "NO_IDENTITY", error: "unauthorized" });
    },
    whoAmI: async () => {
      if (!signedIn) throw new ApiError(401, { code: "NO_IDENTITY", error: "unauthorized" });
      return { event_head: "0", kind: "user", login: "alice" };
    },
  });
  const tab = otherTab("tab-2");
  try {
    await screen.findByText("Signed in as alice");
    await waitFor(() => expect(tab.told).toContainEqual(expect.objectContaining({ here: true })));
    const attempts: string[] = [];
    void aliceWrite(app.queryClient, attempts, "CORE-1 set to done");
    await waitFor(() => expect(writesOf(app.queryClient).rows()).toHaveLength(1));

    fireEvent.click(screen.getByRole("button", { name: "Sign out" }));
    await waitFor(() => expect(app.asked).toHaveLength(1));
    await screen.findByRole("link", { name: "Sign in with Google" });
    await waitFor(() => expect(tab.told).toContainEqual(about("signedOut", "alice")));
    expect(tab.told).not.toContainEqual(about("stillSignedIn", "alice"));
    expect(writesOf(app.queryClient).rows()).toEqual([]);
    expect(keptWrites()).toEqual([]);
    expect(screen.queryByRole("region", { name: "Kept changes" })).toBeNull();

    signedIn = true;
    await act(() => app.queryClient.invalidateQueries({ queryKey: ["whoami"] }));
    await screen.findByText("Signed in as alice");
    const made = attempts.length;
    await sleep(2_500);
    expect(attempts).toHaveLength(made);
    expect(writesOf(app.queryClient).rows()).toEqual([]);
  } finally {
    tab.close();
    app.teardown();
  }
}, 15_000);

// W50. A session that ends while its sign-out is still out - another browser's sign-out revoked
// it, and this page's sign-in check says so first - has ended as the sign-out asked: it discards,
// rather than keeping every write for a sign-in the person chose to discard them before.
test("a session that ends under its sign-out discards, and keeps nothing for the next sign-in", async () => {
  const held = Promise.withResolvers<{ ok: true }>();
  let signedIn = true;
  const app = renderApp({
    logout: () => held.promise,
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
    await waitFor(() => expect(writesOf(app.queryClient).rows()).toHaveLength(1));

    fireEvent.click(screen.getByRole("button", { name: "Sign out" }));
    await waitFor(() => expect(app.logout).toHaveBeenCalledTimes(1));
    signedIn = false;
    await act(() => app.queryClient.invalidateQueries({ queryKey: ["whoami"] }));
    await screen.findByRole("link", { name: "Sign in with Google" });
    await waitFor(() => expect(tab.told).toContainEqual(about("signedOut", "alice")));
    expect(tab.told).not.toContainEqual(about("stillSignedIn", "alice"));
    expect(writesOf(app.queryClient).rows()).toEqual([]);
    expect(keptWrites()).toEqual([]);
    expect(screen.queryByRole("region", { name: "Kept changes" })).toBeNull();

    signedIn = true;
    await act(() => app.queryClient.invalidateQueries({ queryKey: ["whoami"] }));
    await screen.findByText("Signed in as alice");
    const made = attempts.length;
    await sleep(2_500);
    expect(attempts).toHaveLength(made);
  } finally {
    held.resolve({ ok: true });
    tab.close();
    app.teardown();
  }
}, 15_000);

// W50, W60. Dispatch's own 401 is the verdict: the sign-out lands even while the sign-in check
// cannot be read to say the session lapsed, rather than asking again for good with every write
// held.
test("a sign-out answered 401 lands while the sign-in check cannot be read", async () => {
  let checkable = true;
  const app = renderApp({
    logout: async () => {
      checkable = false;
      throw new ApiError(401, { code: "NO_IDENTITY", error: "unauthorized" });
    },
    whoAmI: async () => {
      if (!checkable) throw new NetworkError("/auth/whoami", new TypeError("Failed to fetch"));
      return { event_head: "0", kind: "user", login: "alice" };
    },
  });
  const tab = otherTab("tab-2");
  try {
    await screen.findByText("Signed in as alice");
    await waitFor(() => expect(tab.told).toContainEqual(expect.objectContaining({ here: true })));
    const attempts: string[] = [];
    void aliceWrite(app.queryClient, attempts, "CORE-1 set to done");
    await waitFor(() => expect(writesOf(app.queryClient).rows()).toHaveLength(1));

    fireEvent.click(screen.getByRole("button", { name: "Sign out" }));
    await waitFor(() => expect(tab.told).toContainEqual(about("signedOut", "alice")));
    expect(app.logout).toHaveBeenCalledTimes(1);
    expect(writesOf(app.queryClient).rows()).toEqual([]);
    expect(keptWrites()).toEqual([]);
  } finally {
    tab.close();
    app.teardown();
  }
}, 15_000);

// W60. A 401 that is not Dispatch's own - an authenticating proxy in front of it refusing the
// request - says nothing about the session: the sign-out never reached Dispatch, so it did not
// go through. The person stays signed in with every change, as they do after any other refusal,
// rather than having their changes discarded by a 401 Dispatch never gave.
test("a 401 that is not Dispatch's own is a sign-out that did not go through", async () => {
  const app = renderApp({
    logout: async () => {
      throw new ApiError(401, { error: "Unauthorized" });
    },
    whoAmI: async () => ({ event_head: "0", kind: "user", login: "alice" }),
  });
  const tab = otherTab("tab-2");
  try {
    await screen.findByText("Signed in as alice");
    await waitFor(() => expect(tab.told).toContainEqual(expect.objectContaining({ here: true })));
    const attempts: string[] = [];
    void aliceWrite(app.queryClient, attempts, "CORE-1 set to done");
    await waitFor(() => expect(writesOf(app.queryClient).rows()).toHaveLength(1));

    fireEvent.click(screen.getByRole("button", { name: "Sign out" }));
    // The first 401 may be a session that rotated between the two reads, so the sign-out is
    // asked once more, a second after; the second, with the check still naming alice, is the
    // refusal.
    const alert = await screen.findByRole("alert", {}, { timeout: 5_000 });
    expect(alert.textContent).toContain("The sign-out did not go through.");
    expect(app.logout).toHaveBeenCalledTimes(2);
    expect(screen.getByText("Signed in as alice")).toBeTruthy();
    expect(
      writesOf(app.queryClient)
        .rows()
        .map((row) => row.label)
    ).toEqual(["CORE-1 set to done"]);
    expect(keptWrites()).toHaveLength(1);
    await waitFor(() => expect(tab.told).toContainEqual(about("stillSignedIn", "alice")));
    expect(tab.told).not.toContainEqual(about("signedOut", "alice"));
  } finally {
    tab.close();
    app.teardown();
  }
}, 15_000);

// W57. A tab whose sign-out held this tab's writes went without saying how it ended: it closed,
// crashed or was discarded, so only Dispatch knows whether the sign-out landed. This tab reads
// who is signed in before it decides. Dispatch's `401 NO_IDENTITY` says the sign-out landed, so
// the changes the person chose to discard go here too - keeping them would send, at their next
// sign-in, the very writes they discarded.
test("the writes held for a tab that went are discarded once whoami says the sign-out landed", async () => {
  let signedIn = true;
  const app = renderApp({
    whoAmI: async () => {
      if (!signedIn) throw new ApiError(401, { code: "NO_IDENTITY", error: "unauthorized" });
      return { event_head: "0", kind: "user", login: "alice" };
    },
  });
  const signer = otherTab("tab-2");
  try {
    await screen.findByText("Signed in as alice");
    await waitFor(() =>
      expect(signer.told).toContainEqual(expect.objectContaining({ here: true }))
    );
    const attempts: string[] = [];
    void aliceWrite(app.queryClient, attempts, "CORE-1 set to done");
    await waitFor(() => expect(attempts.length).toBeGreaterThan(0));

    signer.say({ signingOut: "alice" });
    await screen.findByText("CORE-1 set to done — Waiting for a sign-out in another tab");
    // Dispatch answers that tab's sign-out, and it goes before it can say so.
    signedIn = false;
    signer.close();

    await screen.findByRole("link", { name: "Sign in with Google" });
    expect(writesOf(app.queryClient).rows()).toEqual([]);
    expect(keptWrites()).toEqual([]);
    expect(screen.queryByRole("region", { name: "Kept changes" })).toBeNull();
    const made = attempts.length;
    await sleep(2_500);
    expect(attempts).toHaveLength(made);
  } finally {
    app.teardown();
  }
}, 15_000);

// W57. The same tab going while the person is still signed in - its sign-out was refused, or
// never answered - leaves their writes exactly where they stood: whoami names them, so the hold
// lifts and the writes go on.
test("the writes held for a tab that went go on once whoami still names the person", async () => {
  const app = renderApp({
    whoAmI: async () => ({ event_head: "0", kind: "user", login: "alice" }),
  });
  const signer = otherTab("tab-2");
  try {
    await screen.findByText("Signed in as alice");
    await waitFor(() =>
      expect(signer.told).toContainEqual(expect.objectContaining({ here: true }))
    );
    const attempts: string[] = [];
    void aliceWrite(app.queryClient, attempts, "CORE-1 set to done");
    await waitFor(() => expect(attempts.length).toBeGreaterThan(0));

    signer.say({ signingOut: "alice" });
    await screen.findByText("CORE-1 set to done — Waiting for a sign-out in another tab");
    const held = attempts.length;
    signer.close();

    await waitFor(() => expect(attempts.length).toBeGreaterThan(held));
    expect(screen.queryByText(/Waiting for a sign-out in another tab/)).toBeNull();
    expect(
      writesOf(app.queryClient)
        .rows()
        .map((row) => row.label)
    ).toEqual(["CORE-1 set to done"]);
    expect(screen.getByText("Signed in as alice")).toBeTruthy();
  } finally {
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
  const second = otherTab("tab-2", [change("CORE-4 set to backlog"), change("Labels of CORE-4")]);
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

// W53. The sign-out question names each change once, by key: a tab duplicated from this one, or
// from another, copied its kept writes and resumed them under the same keys, so its copies are the
// same changes. A tab that did not answer the last question is taken for gone, so the next one
// neither waits for it nor says the other tabs may hold more, until it speaks again.
test("the sign-out question names a duplicated tab's copies once, and forgets a tab that missed it", async () => {
  const app = renderApp({
    confirm: () => false,
    whoAmI: async () => ({ event_head: "0", kind: "user", login: "alice" }),
  });
  const tabs: OtherTab[] = [];
  try {
    await screen.findByText("Signed in as alice");
    const attempts: string[] = [];
    void aliceWrite(app.queryClient, attempts, "CORE-1 set to done");
    await waitFor(() => expect(writesOf(app.queryClient).rows()).toHaveLength(1));
    const own = writesOf(app.queryClient).rows()[0]?.key ?? "";
    const copies = [change("CORE-1 set to done", own), change("CORE-4 set to backlog", "k-4")];
    tabs.push(otherTab("tab-2", copies), otherTab("tab-3", copies), otherTab("tab-4", "silent"));
    for (const other of tabs) {
      await waitFor(() =>
        expect(other.told).toContainEqual(expect.objectContaining({ here: true }))
      );
    }
    const named = "2 unsent changes";
    const listed = "\n\n• CORE-1 set to done\n• CORE-4 set to backlog";
    const elsewhere = ", and any unsent changes in your other Dispatch tabs";

    fireEvent.click(screen.getByRole("button", { name: "Sign out" }));
    await waitFor(() => expect(app.asked).toHaveLength(1));
    expect(app.asked[0]).toBe(`Sign out and discard ${named}${elsewhere}?${listed}`);

    const started = Date.now();
    fireEvent.click(screen.getByRole("button", { name: "Sign out" }));
    await waitFor(() => expect(app.asked).toHaveLength(2));
    expect(Date.now() - started).toBeLessThan(250);
    expect(app.asked[1]).toBe(`Sign out and discard ${named}?${listed}`);

    tabs[2]?.say({ hello: true });
    await waitFor(() =>
      expect(tabs[2]?.told).toContainEqual(expect.objectContaining({ here: true }))
    );
    fireEvent.click(screen.getByRole("button", { name: "Sign out" }));
    await waitFor(() => expect(app.asked).toHaveLength(3));
    expect(app.asked[2]).toBe(`Sign out and discard ${named}${elsewhere}?${listed}`);
    expect(app.logout).not.toHaveBeenCalled();
  } finally {
    for (const other of tabs) other.close();
    app.teardown();
  }
}, 15_000);

// W51. Another tab's sign-out holds the person's writes here as a lease, and each held write says
// what it waits for. A signing-out tab that dies without a word - crashed, killed, frozen - stops
// saying its sign-out is out, and within 6 s of the last time it did this tab takes it for gone
// and its writes go on.
test("a tab holding writes for another tab's sign-out resumes them within 6 s once that tab falls silent", async () => {
  const app = renderApp({
    whoAmI: async () => ({ event_head: "0", kind: "user", login: "alice" }),
  });
  const signer = otherTab("tab-2");
  try {
    await screen.findByText("Signed in as alice");
    await waitFor(() =>
      expect(signer.told).toContainEqual(expect.objectContaining({ here: true }))
    );
    const attempts: string[] = [];
    void aliceWrite(app.queryClient, attempts, "CORE-1 set to done");
    await waitFor(() => expect(attempts.length).toBeGreaterThan(0));

    signer.say({ signingOut: "alice" });
    const said = Date.now();
    await screen.findByText("CORE-1 set to done — Waiting for a sign-out in another tab");
    const made = attempts.length;
    await sleep(4_000);
    expect(attempts).toHaveLength(made);

    await waitFor(() => expect(attempts.length).toBeGreaterThan(made), { timeout: 3_000 });
    const resumed = Date.now() - said;
    expect(resumed).toBeGreaterThanOrEqual(5_500);
    expect(resumed).toBeLessThanOrEqual(6_500);
    expect(screen.queryByText(/Waiting for a sign-out in another tab/)).toBeNull();
  } finally {
    signer.close();
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
