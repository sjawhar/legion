import { expect, spyOn, test } from "bun:test";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";

import * as MarkdownBody from "../refs/MarkdownBody";
import { DeploymentResilience, installChunkFailureRecovery } from "./DeploymentResilience";

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

test("a Vite preload error reloads once and prevents the browser's fallback", async () => {
  window.sessionStorage.clear();
  installChunkFailureRecovery();
  const reload = spyOn(window.location, "reload").mockImplementation(() => undefined);
  const view = render(<DeploymentResilience />);
  const first = new Event("vite:preloadError", { cancelable: true });
  const second = new Event("vite:preloadError", { cancelable: true });

  try {
    window.dispatchEvent(first);
    window.dispatchEvent(second);

    expect(first.defaultPrevented).toBe(true);
    expect(second.defaultPrevented).toBe(true);
    expect(reload).toHaveBeenCalledTimes(1);
    expect(window.sessionStorage.getItem("dispatch.reloaded-for-chunk")).toBe("true");
  } finally {
    reload.mockRestore();
    view.unmount();
    window.sessionStorage.clear();
  }
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
