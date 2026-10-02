import { expect, spyOn, test } from "bun:test";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";

import * as MarkdownBody from "../refs/MarkdownBody";
import {
  DeploymentResilience,
  importWhenOnline,
  installChunkFailureRecovery,
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

test("warms the block schema and headless Markdown renderer once after the first paint", async () => {
  const warm = spyOn(MarkdownBody, "warmMarkdownRenderer").mockResolvedValue(undefined);
  const view = render(<DeploymentResilience />);

  try {
    await waitFor(() => expect(warm).toHaveBeenCalledTimes(1));
  } finally {
    warm.mockRestore();
    view.unmount();
  }
});

test("a Vite preload error reloads once per session and always reaches its importer", async () => {
  window.sessionStorage.clear();
  installChunkFailureRecovery();
  const reload = spyOn(window.location, "reload").mockImplementation(() => undefined);
  const view = render(<DeploymentResilience />);
  const first = new Event("vite:preloadError", { cancelable: true });
  const second = new Event("vite:preloadError", { cancelable: true });

  try {
    window.dispatchEvent(first);
    window.dispatchEvent(second);

    expect(first.defaultPrevented).toBe(false);
    expect(second.defaultPrevented).toBe(false);
    expect(reload).toHaveBeenCalledTimes(1);
    expect(window.sessionStorage.getItem("dispatch.reloaded-for-chunk")).toBe("true");
  } finally {
    reload.mockRestore();
    view.unmount();
    window.sessionStorage.clear();
  }
});

test("a chunk that fails while the page is being left does not reload over the navigation", () => {
  window.sessionStorage.clear();
  installChunkFailureRecovery();
  const reload = spyOn(window.location, "reload").mockImplementation(() => undefined);
  const whileLeaving = new Event("vite:preloadError", { cancelable: true });
  const afterReturning = new Event("vite:preloadError", { cancelable: true });

  try {
    window.dispatchEvent(new Event("beforeunload"));
    window.dispatchEvent(whileLeaving);

    expect(whileLeaving.defaultPrevented).toBe(false);
    expect(reload).not.toHaveBeenCalled();
    expect(window.sessionStorage.getItem("dispatch.reloaded-for-chunk")).toBeNull();

    // A page the back/forward cache restores is shown again, and its own failures reload it.
    window.dispatchEvent(new Event("pageshow"));
    window.dispatchEvent(afterReturning);

    expect(afterReturning.defaultPrevented).toBe(false);
    expect(reload).toHaveBeenCalledTimes(1);
    expect(window.sessionStorage.getItem("dispatch.reloaded-for-chunk")).toBe("true");
  } finally {
    window.dispatchEvent(new Event("pageshow"));
    reload.mockRestore();
    window.sessionStorage.clear();
  }
});

test("a page still in use after a navigation began reloads for its next chunk failure", () => {
  window.sessionStorage.clear();
  installChunkFailureRecovery();
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
  installChunkFailureRecovery();
  const reload = spyOn(window.location, "reload").mockImplementation(() => undefined);
  const onLine = Object.getOwnPropertyDescriptor(Navigator.prototype, "onLine");
  Object.defineProperty(navigator, "onLine", { configurable: true, value: false });
  const failure = new Event("vite:preloadError", { cancelable: true });

  try {
    window.dispatchEvent(failure);

    expect(failure.defaultPrevented).toBe(false);
    expect(reload).not.toHaveBeenCalled();
    expect(window.sessionStorage.getItem("dispatch.reloaded-for-chunk")).toBeNull();
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

test("shows a reloadable update notice when focused after the running index chunk changes", async () => {
  const currentScript = document.createElement("script");
  currentScript.type = "application/json";
  currentScript.src = "/assets/index-current.js";
  document.head.append(currentScript);
  function responseForUpdate(input: RequestInfo | URL): Promise<Response> {
    if (input === "/healthz") {
      return Promise.resolve(new Response("{}", { status: 200 }));
    }
    return Promise.resolve(
      new Response('<script src="/a.js"></script><script src="/assets/index-new.js"></script>', {
        status: 200,
      })
    );
  }
  responseForUpdate.preconnect = () => {};
  const fetch = spyOn(globalThis, "fetch").mockImplementation(responseForUpdate);
  const reload = spyOn(window.location, "reload").mockImplementation(() => undefined);
  const view = render(<DeploymentResilience />);

  try {
    fireEvent.focus(window);

    const notice = await screen.findByRole("status", { name: "A new version is available" });
    expect(notice.textContent).toContain("A new version is available - Reload");
    expect(fetch).toHaveBeenCalledWith("/healthz", { cache: "no-store" });
    expect(fetch).toHaveBeenCalledWith("/", { cache: "no-store" });

    fireEvent.click(screen.getByRole("button", { name: "Reload" }));
    expect(reload).toHaveBeenCalledTimes(1);
  } finally {
    reload.mockRestore();
    fetch.mockRestore();
    currentScript.remove();
    view.unmount();
  }
});

test("waits one minute before checking for another deployment on focus", async () => {
  const currentScript = document.createElement("script");
  currentScript.type = "application/json";
  currentScript.src = "/assets/index-current.js";
  document.head.append(currentScript);
  let now = 100_000;
  const dateNow = spyOn(Date, "now").mockImplementation(() => now);
  function responseForThrottle(input: RequestInfo | URL): Promise<Response> {
    if (input === "/healthz") {
      return Promise.resolve(new Response("{}", { status: 200 }));
    }
    return Promise.resolve(
      new Response('<script type="module" src="/assets/index-current.js"></script>', {
        status: 200,
      })
    );
  }
  responseForThrottle.preconnect = () => {};
  const fetch = spyOn(globalThis, "fetch").mockImplementation(responseForThrottle);
  const view = render(<DeploymentResilience />);

  try {
    fireEvent.focus(window);
    await waitFor(() => expect(fetch).toHaveBeenCalledTimes(2));

    fireEvent.focus(window);
    expect(fetch).toHaveBeenCalledTimes(2);

    now += 60_000;
    fireEvent.focus(window);
    await waitFor(() => expect(fetch).toHaveBeenCalledTimes(4));
  } finally {
    fetch.mockRestore();
    dateNow.mockRestore();
    currentScript.remove();
    view.unmount();
  }
});
