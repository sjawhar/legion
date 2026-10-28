import { afterEach, expect, spyOn, test } from "bun:test";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";

import { REQUEST_DEADLINE_MS } from "../../api/client";
import * as markdownEngine from "../refs/markdown-engine";
import {
  DeploymentResilience,
  importWhenOnline,
  installChunkFailureRecovery,
  spendChunkReload,
} from "./DeploymentResilience";

function withOnLine<T>(onLine: boolean, run: () => Promise<T>): Promise<T> {
  const descriptor = Object.getOwnPropertyDescriptor(Navigator.prototype, "onLine");
  Object.defineProperty(navigator, "onLine", { configurable: true, value: onLine });
  return run().finally(() => {
    Reflect.deleteProperty(navigator, "onLine");
    if (descriptor !== undefined) {
      Object.defineProperty(Navigator.prototype, "onLine", descriptor);
    }
  });
}

/** A stand-in for the Navigation API's `navigate` event: whether it stays in this document, the
 *  file name a link with `download` asks for, empty when the attribute names none (null for every
 *  other navigation), and where it goes. */
function navigateEvent(
  sameDocument: boolean,
  downloadRequest: string | null = null,
  url = "http://localhost/elsewhere"
): Event {
  return Object.assign(new Event("navigate"), {
    destination: { sameDocument, url },
    downloadRequest,
  });
}

const recoveryUninstalls = new Set<() => void>();

function installChunkFailureRecoveryForTest(): void {
  recoveryUninstalls.add(installChunkFailureRecovery());
}

afterEach(() => {
  for (const uninstall of recoveryUninstalls) uninstall();
  recoveryUninstalls.clear();
});

/** The tab's recorded recovery reloads, under the key `web/index.html`'s inline script shares. */
function storedChunkReloads(): unknown {
  const value = window.sessionStorage.getItem("dispatch.chunk-reloads");
  return value === null ? null : JSON.parse(value);
}

const minute = 60_000;

test("warms the block schema and headless Markdown renderer once after the first paint", async () => {
  const warm = spyOn(markdownEngine, "warmMarkdownRenderer").mockResolvedValue(undefined);
  const view = render(<DeploymentResilience />);

  try {
    await waitFor(() => expect(warm).toHaveBeenCalledTimes(1));
  } finally {
    warm.mockRestore();
    view.unmount();
  }
});

test("Vite preload errors reload the page three times in five minutes, then not, and always reach their importer", () => {
  window.sessionStorage.clear();
  installChunkFailureRecoveryForTest();
  const start = 1_800_000_000_000;
  let now = start;
  const dateNow = spyOn(Date, "now").mockImplementation(() => now);
  const reload = spyOn(window.location, "reload").mockImplementation(() => undefined);
  const failAt = (time: number) => {
    now = time;
    const failure = new Event("vite:preloadError", { cancelable: true });
    window.dispatchEvent(failure);
    // The page goes (`pagehide`), which spends the reload the failure asked for.
    window.dispatchEvent(new Event("pagehide"));
    return failure.defaultPrevented;
  };

  try {
    const prevented = [
      failAt(start),
      failAt(start + minute),
      failAt(start + 2 * minute),
      failAt(start + 4 * minute),
    ];

    expect(prevented).toEqual([false, false, false, false]);
    expect(reload).toHaveBeenCalledTimes(3);
    expect(storedChunkReloads()).toEqual([start, start + minute, start + 2 * minute]);
  } finally {
    reload.mockRestore();
    dateNow.mockRestore();
    window.sessionStorage.clear();
  }
});

test("a failure after the oldest recovery reload has left the five minutes reloads again", () => {
  window.sessionStorage.clear();
  const start = 1_800_000_000_000;

  try {
    expect([
      spendChunkReload(start),
      spendChunkReload(start + minute),
      spendChunkReload(start + 2 * minute),
      spendChunkReload(start + 5 * minute - 1),
    ]).toEqual([true, true, true, false]);
    expect(storedChunkReloads()).toEqual([start, start + minute, start + 2 * minute]);

    // Five minutes after the first reload it has left the window, and only the other two count.
    expect(spendChunkReload(start + 5 * minute)).toBe(true);
    expect(storedChunkReloads()).toEqual([start + minute, start + 2 * minute, start + 5 * minute]);
    expect(spendChunkReload(start + 5 * minute + 1)).toBe(false);
  } finally {
    window.sessionStorage.clear();
  }
});

test("a stored value that does not parse as a list of times reads as no reloads and is overwritten", () => {
  const now = 1_800_000_000_000;
  const stored = ["true", "not json", "{}", '"3"', `[null, "x", ${now}]`].map((value) => {
    window.sessionStorage.clear();
    window.sessionStorage.setItem("dispatch.chunk-reloads", value);
    return { reloads: spendChunkReload(now), value, after: storedChunkReloads() };
  });
  window.sessionStorage.clear();

  expect(stored).toEqual([
    { reloads: true, value: "true", after: [now] },
    { reloads: true, value: "not json", after: [now] },
    { reloads: true, value: "{}", after: [now] },
    { reloads: true, value: '"3"', after: [now] },
    // The one time the list holds counts; the entries that are not times are dropped.
    { reloads: true, value: `[null, "x", ${now}]`, after: [now, now] },
  ]);
});

test("the once-only key of earlier builds is neither read nor written", () => {
  window.sessionStorage.clear();
  // A page of an earlier build in this tab already spent its one reload.
  window.sessionStorage.setItem("dispatch.reloaded-for-chunk", "true");
  installChunkFailureRecoveryForTest();
  const reload = spyOn(window.location, "reload").mockImplementation(() => undefined);

  try {
    window.dispatchEvent(new Event("vite:preloadError", { cancelable: true }));
    window.dispatchEvent(new Event("pagehide"));

    expect(reload).toHaveBeenCalledTimes(1);
    expect(window.sessionStorage.getItem("dispatch.reloaded-for-chunk")).toBe("true");
    // Nor written: a reload with the old key absent leaves it absent.
    window.sessionStorage.removeItem("dispatch.reloaded-for-chunk");
    window.dispatchEvent(new Event("vite:preloadError", { cancelable: true }));
    window.dispatchEvent(new Event("pagehide"));
    expect(reload).toHaveBeenCalledTimes(2);
    expect(window.sessionStorage.getItem("dispatch.reloaded-for-chunk")).toBeNull();
  } finally {
    reload.mockRestore();
    window.sessionStorage.clear();
  }
});

test("a chunk that fails while the page is being left does not reload over the navigation", () => {
  window.sessionStorage.clear();
  installChunkFailureRecoveryForTest();
  const reload = spyOn(window.location, "reload").mockImplementation(() => undefined);
  const whileLeaving = new Event("vite:preloadError", { cancelable: true });
  const afterReturning = new Event("vite:preloadError", { cancelable: true });

  try {
    window.dispatchEvent(new Event("beforeunload"));
    window.dispatchEvent(whileLeaving);

    expect(whileLeaving.defaultPrevented).toBe(false);
    expect(reload).not.toHaveBeenCalled();
    expect(storedChunkReloads()).toBeNull();

    // A page the back/forward cache restores is shown again, and its own failures reload it.
    window.dispatchEvent(new Event("pageshow"));
    window.dispatchEvent(afterReturning);

    expect(afterReturning.defaultPrevented).toBe(false);
    expect(reload).toHaveBeenCalledTimes(1);
    // The page goes, and spends the reload the failure asked for.
    window.dispatchEvent(new Event("pagehide"));
    expect(storedChunkReloads()).toHaveLength(1);
  } finally {
    window.dispatchEvent(new Event("pageshow"));
    reload.mockRestore();
    window.sessionStorage.clear();
  }
});

// W20. A reload asked for is not a reload taken: an unconfirmed keyed send raises a leave prompt,
// and a reader who answers "stay" keeps the page they are on. A recovery reload is spent from the
// tab's budget as the page actually goes (`pagehide`), so the budget is as it was and the next
// failed chunk can still reload; the reader's press says the page is plainly still theirs.
test("a reload the reader cancels at the leave prompt spends nothing of the tab's budget", () => {
  window.sessionStorage.clear();
  installChunkFailureRecoveryForTest();
  const reload = spyOn(window.location, "reload").mockImplementation(() => undefined);

  try {
    window.dispatchEvent(new Event("vite:preloadError", { cancelable: true }));
    expect(reload).toHaveBeenCalledTimes(1);
    expect(storedChunkReloads()).toBeNull();
    // A second failure before the page goes asks for no second reload.
    window.dispatchEvent(new Event("vite:preloadError", { cancelable: true }));
    expect(reload).toHaveBeenCalledTimes(1);

    // The prompt is answered "stay": no `pagehide`, and the reader goes on using the page.
    window.dispatchEvent(new KeyboardEvent("keydown", { key: "a" }));
    window.dispatchEvent(new Event("pagehide"));
    expect(storedChunkReloads()).toBeNull();

    // So the next failure still has the whole budget to spend, and spends one reload when the
    // page does go.
    window.dispatchEvent(new Event("vite:preloadError", { cancelable: true }));
    expect(reload).toHaveBeenCalledTimes(2);
    window.dispatchEvent(new Event("pagehide"));
    expect(storedChunkReloads()).toHaveLength(1);
  } finally {
    reload.mockRestore();
    window.sessionStorage.clear();
  }
});

test("a navigation to another document marks the page as left where the browser has the Navigation API", () => {
  window.sessionStorage.clear();
  // iOS Safari fires no `beforeunload`; the Navigation API's `navigate` event is all it says.
  const navigation = new EventTarget();
  Object.defineProperty(window, "navigation", { configurable: true, value: navigation });
  const reload = spyOn(window.location, "reload").mockImplementation(() => undefined);
  const navigate = (sameDocument: boolean) => navigation.dispatchEvent(navigateEvent(sameDocument));
  const failChunk = () =>
    window.dispatchEvent(new Event("vite:preloadError", { cancelable: true }));

  try {
    installChunkFailureRecoveryForTest();
    navigate(false);
    failChunk();
    expect(reload).not.toHaveBeenCalled();
    expect(storedChunkReloads()).toBeNull();

    // A route change inside the app stays in this document, so the page's failures reload it.
    window.dispatchEvent(new Event("pageshow"));
    navigate(true);
    failChunk();
    expect(reload).toHaveBeenCalledTimes(1);
  } finally {
    window.dispatchEvent(new Event("pageshow"));
    Reflect.deleteProperty(window, "navigation");
    reload.mockRestore();
    window.sessionStorage.clear();
  }
});

test("a link answered with a download does not mark the page as left", () => {
  window.sessionStorage.clear();
  // A link with `download` fires `navigate` to another document and the page stays where it is.
  // Dispatch's Download version links carry an empty `download`, which Chromium reports as an
  // empty `downloadRequest`: a download all the same.
  const navigation = new EventTarget();
  Object.defineProperty(window, "navigation", { configurable: true, value: navigation });
  const reload = spyOn(window.location, "reload").mockImplementation(() => undefined);

  try {
    installChunkFailureRecoveryForTest();
    navigation.dispatchEvent(navigateEvent(false, ""));
    window.dispatchEvent(new Event("vite:preloadError", { cancelable: true }));
    expect(reload).toHaveBeenCalledTimes(1);
    // The page goes, and spends the reload the failure asked for.
    window.dispatchEvent(new Event("pagehide"));
    expect(storedChunkReloads()).toHaveLength(1);
  } finally {
    window.dispatchEvent(new Event("pageshow"));
    Reflect.deleteProperty(window, "navigation");
    reload.mockRestore();
    window.sessionStorage.clear();
  }
});

test("a page shown after a download marks a later navigation to that URL as leaving", () => {
  window.sessionStorage.clear();
  const navigation = new EventTarget();
  Object.defineProperty(window, "navigation", { configurable: true, value: navigation });
  const reload = spyOn(window.location, "reload").mockImplementation(() => undefined);
  const url = "http://localhost/api/v1/artifacts/spec/versions/1";

  try {
    installChunkFailureRecoveryForTest();
    navigation.dispatchEvent(navigateEvent(false, "", url));
    // This is no longer Firefox's second event from the download click: the page stayed in use.
    window.dispatchEvent(new Event("pageshow"));
    navigation.dispatchEvent(navigateEvent(false, null, url));
    window.dispatchEvent(new Event("vite:preloadError", { cancelable: true }));

    expect(reload).not.toHaveBeenCalled();
    expect(storedChunkReloads()).toBeNull();
  } finally {
    window.dispatchEvent(new Event("pageshow"));
    Reflect.deleteProperty(window, "navigation");
    reload.mockRestore();
    window.sessionStorage.clear();
  }
});

test("the second navigate Firefox fires for one Download click does not mark the page as left", () => {
  window.sessionStorage.clear();
  // Firefox follows the download's `navigate` with another for the same click: to the same URL,
  // with no `downloadRequest`.
  const navigation = new EventTarget();
  Object.defineProperty(window, "navigation", { configurable: true, value: navigation });
  const reload = spyOn(window.location, "reload").mockImplementation(() => undefined);
  const file = "http://localhost/api/v1/artifacts/spec/versions/1";
  const navigate = (downloadRequest: string | null, url: string) =>
    navigation.dispatchEvent(navigateEvent(false, downloadRequest, url));
  const reloadsAfter = (...steps: (() => void)[]) => {
    window.dispatchEvent(new Event("pageshow"));
    window.sessionStorage.clear();
    reload.mockClear();
    for (const step of steps) step();
    window.dispatchEvent(new Event("vite:preloadError", { cancelable: true }));
    return reload.mock.calls.length;
  };

  try {
    installChunkFailureRecoveryForTest();
    expect({
      repeated: reloadsAfter(
        () => navigate("", file),
        () => navigate(null, file)
      ),
      // Only the one navigation after the download is skipped: the next to that URL leaves.
      thenAgain: reloadsAfter(() => navigate(null, file)),
      elsewhere: reloadsAfter(
        () => navigate("", file),
        () => navigate(null, "http://localhost/elsewhere")
      ),
    }).toEqual({ repeated: 1, thenAgain: 0, elsewhere: 0 });
  } finally {
    window.dispatchEvent(new Event("pageshow"));
    Reflect.deleteProperty(window, "navigation");
    reload.mockRestore();
    window.sessionStorage.clear();
  }
});

test("a page still in use after a navigation began reloads for its next chunk failure", () => {
  window.sessionStorage.clear();
  installChunkFailureRecoveryForTest();
  const reload = spyOn(window.location, "reload").mockImplementation(() => undefined);
  const failChunk = () =>
    window.dispatchEvent(new Event("vite:preloadError", { cancelable: true }));
  // The navigation never happens - cancelled at a leave prompt, stopped, or answered with a
  // download - so no pageshow and no unload follow it, and the reader goes on using the page.
  const press = (type: string) => () =>
    document.body.dispatchEvent(new Event(type, { bubbles: true }));
  const stillInUse = [
    ["a pointer press", press("pointerdown")],
    ["a key press", press("keydown")],
    ["the tab shown again", () => document.dispatchEvent(new Event("visibilitychange"))],
  ] as const;

  try {
    // A tab hidden while its navigation is under way is still being left.
    window.dispatchEvent(new Event("beforeunload"));
    Object.defineProperty(document, "visibilityState", { configurable: true, value: "hidden" });
    try {
      document.dispatchEvent(new Event("visibilitychange"));
    } finally {
      Reflect.deleteProperty(document, "visibilityState");
    }
    failChunk();
    expect(reload).not.toHaveBeenCalled();

    for (const [signal, use] of stillInUse) {
      window.sessionStorage.clear();
      reload.mockClear();
      window.dispatchEvent(new Event("beforeunload"));
      failChunk();
      const whileLeaving = reload.mock.calls.length;
      use();
      failChunk();
      expect({ signal, whileLeaving, afterUse: reload.mock.calls.length }).toEqual({
        signal,
        whileLeaving: 0,
        afterUse: 1,
      });
    }
  } finally {
    window.dispatchEvent(new Event("pageshow"));
    reload.mockRestore();
    window.sessionStorage.clear();
  }
});

test("a chunk that fails while the browser is offline is not a stale deployment: no reload, the importer sees the error", () => {
  window.sessionStorage.clear();
  installChunkFailureRecoveryForTest();
  const reload = spyOn(window.location, "reload").mockImplementation(() => undefined);
  const onLine = Object.getOwnPropertyDescriptor(Navigator.prototype, "onLine");
  Object.defineProperty(navigator, "onLine", { configurable: true, value: false });
  const failure = new Event("vite:preloadError", { cancelable: true });

  try {
    window.dispatchEvent(failure);

    expect(failure.defaultPrevented).toBe(false);
    expect(reload).not.toHaveBeenCalled();
    expect(storedChunkReloads()).toBeNull();
  } finally {
    reload.mockRestore();
    Reflect.deleteProperty(navigator, "onLine");
    if (onLine !== undefined) {
      Object.defineProperty(Navigator.prototype, "onLine", onLine);
    }
    window.sessionStorage.clear();
  }
});

test("a chunk that fails offline loads once the browser is back online", async () => {
  let attempts = 0;
  const load = () => {
    attempts += 1;
    return attempts === 1
      ? Promise.reject(new TypeError("Failed to fetch"))
      : Promise.resolve("ok");
  };

  await withOnLine(false, async () => {
    const loading = importWhenOnline(load);
    await Promise.resolve();
    expect(attempts).toBe(1);
    window.dispatchEvent(new Event("online"));
    expect(await loading).toBe("ok");
    expect(attempts).toBe(2);
  });
});

test("a chunk that fails while online is a real failure, not a network blip", async () => {
  await withOnLine(true, async () => {
    await expect(
      importWhenOnline(() => Promise.reject(new TypeError("Failed to fetch")))
    ).rejects.toThrow("Failed to fetch");
  });
});

/** The tab's running page: a script naming its entry chunk, as the built page's module script does. */
function runningPage(entry: string): HTMLScriptElement {
  const script = document.createElement("script");
  script.type = "application/json";
  script.src = entry;
  document.head.append(script);
  return script;
}

/** Answers GET /api/v1/dashboard/builds with `builds` (a status and a body, or a network failure),
 *  `/healthz` with 200, and `/` with a page naming `pageEntry`, so a check that still read `/`
 *  would see another build there. */
function serverAnswering(
  builds: { status: number; body: unknown } | "network failure",
  pageEntry = "/assets/index-new.js"
) {
  function respond(input: RequestInfo | URL): Promise<Response> {
    if (input === "/api/v1/dashboard/builds") {
      if (builds === "network failure") {
        return Promise.reject(new TypeError("Failed to fetch"));
      }
      return Promise.resolve(Response.json(builds.body, { status: builds.status }));
    }
    if (input === "/healthz") {
      return Promise.resolve(new Response("{}", { status: 200 }));
    }
    return Promise.resolve(
      new Response(`<script type="module" src="${pageEntry}"></script>`, { status: 200 })
    );
  }
  respond.preconnect = () => {};
  return respond;
}

/** Lets the check a focus started run to its end: its request, its body and its state update. */
async function settleCheck(): Promise<void> {
  for (let round = 0; round < 5; round++) {
    const { promise, resolve } = Promise.withResolvers<void>();
    setTimeout(resolve, 10);
    await promise;
  }
}

test("shows a reloadable update notice when one build other than the tab's is live", async () => {
  const currentScript = runningPage("/assets/index-current.js");
  const fetch = spyOn(globalThis, "fetch").mockImplementation(
    serverAnswering({
      status: 200,
      body: { serving: "/assets/index-new.js", live: ["/assets/index-new.js"] },
    })
  );
  const reload = spyOn(window.location, "reload").mockImplementation(() => undefined);
  const view = render(<DeploymentResilience />);

  try {
    fireEvent.focus(window);

    const notice = await screen.findByRole("status", { name: "A new version is available" });
    expect(notice.textContent).toContain("A new version is available - Reload");
    expect(fetch).toHaveBeenCalledTimes(1);
    expect(fetch).toHaveBeenCalledWith("/api/v1/dashboard/builds", {
      cache: "no-store",
      signal: expect.any(AbortSignal),
    });

    fireEvent.click(screen.getByRole("button", { name: "Reload" }));
    expect(reload).toHaveBeenCalledTimes(1);
  } finally {
    reload.mockRestore();
    fetch.mockRestore();
    currentScript.remove();
    view.unmount();
  }
});

const noNotice: [string, { status: number; body: unknown } | "network failure"][] = [
  [
    "two builds are live, one newer, while / names the newer one",
    {
      status: 200,
      body: {
        serving: "/assets/index-new.js",
        live: ["/assets/index-current.js", "/assets/index-new.js"],
      },
    },
  ],
  [
    "the one live build is the tab's own",
    {
      status: 200,
      body: { serving: "/assets/index-current.js", live: ["/assets/index-current.js"] },
    },
  ],
  [
    "a process older than the route answers 404",
    { status: 404, body: { code: "NOT_FOUND", error: "no route" } },
  ],
  ["the caller is signed out (401)", { status: 401, body: { error: "unauthorized" } }],
  ["the server fails (500)", { status: 500, body: { error: "internal" } }],
  ["the request fails on the network", "network failure"],
];

for (const [when, builds] of noNotice) {
  test(`shows no update notice when ${when}, from one request`, async () => {
    const currentScript = runningPage("/assets/index-current.js");
    const fetch = spyOn(globalThis, "fetch").mockImplementation(serverAnswering(builds));
    const view = render(<DeploymentResilience />);

    try {
      fireEvent.focus(window);
      await waitFor(() => expect(fetch).toHaveBeenCalledTimes(1));
      await settleCheck();

      expect(screen.queryByRole("status", { name: "A new version is available" })).toBeNull();
      expect(fetch).toHaveBeenCalledTimes(1);
      expect(fetch).toHaveBeenCalledWith("/api/v1/dashboard/builds", {
        cache: "no-store",
        signal: expect.any(AbortSignal),
      });
    } finally {
      fetch.mockRestore();
      currentScript.remove();
      view.unmount();
    }
  });
}

test("shows no update notice on a page that names no entry chunk, as Vite's dev server's does", async () => {
  // Vite defines the build ID in its dev server as in a build; bun's test runner defines nothing.
  const globals = globalThis as { __DISPATCH_BUILD__?: string };
  globals.__DISPATCH_BUILD__ = "dev-build";
  const devScript = runningPage("/src/main.tsx");
  const fetch = spyOn(globalThis, "fetch").mockImplementation(
    serverAnswering({
      status: 200,
      body: { serving: "/assets/index-new.js", live: ["/assets/index-new.js"] },
    })
  );
  const view = render(<DeploymentResilience />);

  try {
    fireEvent.focus(window);
    await waitFor(() => expect(fetch).toHaveBeenCalledTimes(1));
    await settleCheck();

    expect(screen.queryByRole("status", { name: "A new version is available" })).toBeNull();
    expect(fetch).toHaveBeenCalledWith("/api/v1/dashboard/builds", {
      cache: "no-store",
      signal: expect.any(AbortSignal),
    });
  } finally {
    fetch.mockRestore();
    devScript.remove();
    view.unmount();
    Reflect.deleteProperty(globals, "__DISPATCH_BUILD__");
  }
});

test("waits one minute before checking for another deployment on focus", async () => {
  const currentScript = runningPage("/assets/index-current.js");
  let now = 100_000;
  const dateNow = spyOn(Date, "now").mockImplementation(() => now);
  const fetch = spyOn(globalThis, "fetch").mockImplementation(
    serverAnswering(
      {
        status: 200,
        body: { serving: "/assets/index-current.js", live: ["/assets/index-current.js"] },
      },
      "/assets/index-current.js"
    )
  );
  const view = render(<DeploymentResilience />);

  try {
    fireEvent.focus(window);
    await waitFor(() => expect(fetch).toHaveBeenCalledTimes(1));

    fireEvent.focus(window);
    expect(fetch).toHaveBeenCalledTimes(1);

    now += 60_000;
    fireEvent.focus(window);
    await waitFor(() => expect(fetch).toHaveBeenCalledTimes(2));
  } finally {
    fetch.mockRestore();
    dateNow.mockRestore();
    currentScript.remove();
    view.unmount();
  }
});

// A check Dispatch never answers would hold one of the browser's few connections to it for good,
// as an unbounded read did (INFRA-258): its request is given up at the read deadline, its body
// included, and the check then reports nothing.
test("a deployment check Dispatch does not answer is given up at the read deadline", async () => {
  const currentScript = runningPage("/assets/index-current.js");
  let now = 100_000;
  const dateNow = spyOn(Date, "now").mockImplementation(() => now);
  const deadlines: { controller: AbortController; ms: number }[] = [];
  const timeout = spyOn(AbortSignal, "timeout").mockImplementation((ms) => {
    const controller = new AbortController();
    deadlines.push({ controller, ms });
    return controller.signal;
  });
  const signals: (AbortSignal | null | undefined)[] = [];
  // The second check's body, for the test to finish once the check has been given up: a check
  // still waiting on it would then read another live build and show the notice (below).
  let buildsBody: ReadableStreamDefaultController<Uint8Array> | undefined;
  function stalled(_input: RequestInfo | URL, init?: RequestInit): Promise<Response> {
    signals.push(init?.signal);
    const signal = init?.signal;
    // The second check's answer has its headers at once, with a body that never ends.
    if (signals.length > 1) {
      return Promise.resolve(
        new Response(
          new ReadableStream<Uint8Array>({
            start(body) {
              buildsBody = body;
              body.enqueue(
                new TextEncoder().encode('{"serving":"/assets/index-new.js","live":["/')
              );
              signal?.addEventListener("abort", () => body.error(signal.reason), { once: true });
            },
          }),
          { status: 200 }
        )
      );
    }
    const { promise, reject } = Promise.withResolvers<Response>();
    signal?.addEventListener("abort", () => reject(signal.reason), { once: true });
    return promise;
  }
  stalled.preconnect = () => {};
  const fetch = spyOn(globalThis, "fetch").mockImplementation(stalled);
  const view = render(<DeploymentResilience />);

  try {
    // The first check gets no answer: given up at the deadline, it ends there.
    fireEvent.focus(window);
    await waitFor(() => expect(fetch).toHaveBeenCalledTimes(1));
    expect(deadlines.map(({ ms }) => ms)).toEqual([REQUEST_DEADLINE_MS]);
    expect(signals[0]).toBe(deadlines[0]?.controller.signal);
    deadlines[0]?.controller.abort(new DOMException("timed out", "TimeoutError"));
    await settleCheck();
    expect(fetch).toHaveBeenCalledTimes(1);

    // The next check's answer has its headers and stalls in its body: given up at its deadline.
    now += 60_000;
    fireEvent.focus(window);
    await waitFor(() => expect(fetch).toHaveBeenCalledTimes(2));
    expect(deadlines.map(({ ms }) => ms)).toEqual([REQUEST_DEADLINE_MS, REQUEST_DEADLINE_MS]);
    expect(signals[1]).toBe(deadlines[1]?.controller.signal);
    deadlines[1]?.controller.abort(new DOMException("timed out", "TimeoutError"));
    await settleCheck();
    // The body finishes, naming another live build, after the check was given up: a check still
    // waiting on it would read it and show the notice, so this assertion fails if the deadline
    // did not cut the read.
    try {
      buildsBody?.enqueue(new TextEncoder().encode('assets/index-new.js"]}'));
      buildsBody?.close();
    } catch {
      // The abort errored the stream, which is the behaviour under test.
    }
    await settleCheck();
    expect(screen.queryByRole("status", { name: "A new version is available" })).toBeNull();
  } finally {
    fetch.mockRestore();
    timeout.mockRestore();
    dateNow.mockRestore();
    currentScript.remove();
    view.unmount();
  }
});
