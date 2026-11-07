import { afterEach, expect, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { type ReactNode, useRef } from "react";

import { userPreferenceStorageKey } from "../shell/userPreference";
import { DEFAULT_PANE_WIDTH, PaneDivider, usePaneWidth } from "./PaneDivider";

afterEach(() => {
  cleanup();
  window.localStorage.clear();
});

const STORAGE_KEY = userPreferenceStorageKey("alice", "inbox.pane-width");

/** The divider as the Inbox mounts it: the share it carries is the stored preference, and what
 *  it writes goes back to that preference. */
function Columns(): ReactNode {
  const columns = useRef<HTMLDivElement>(null);
  const [width, setWidth] = usePaneWidth();
  return (
    <div ref={columns}>
      <PaneDivider columns={columns} onWidthChange={setWidth} width={width} />
      <span data-testid="share">{width}</span>
    </div>
  );
}

function renderDivider(): void {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  render(
    <QueryClientProvider client={client}>
      <Columns />
    </QueryClientProvider>
  );
}

/** What the divider says it stands at, as a percentage of the two columns. */
function standsAt(): number {
  return Number(screen.getByTestId("inbox-pane-divider").getAttribute("aria-valuenow"));
}

test("the reading pane starts at 60% of the two columns", () => {
  renderDivider();
  expect(DEFAULT_PANE_WIDTH).toBe(0.6);
  expect(screen.getByTestId("share").textContent).toBe("0.6");
  expect(standsAt()).toBe(60);
  // Nothing is written until the reader moves it: a fresh profile holds no width at all.
  expect(window.localStorage.getItem(STORAGE_KEY)).toBeNull();
});

test("the arrow keys move the line, left widening the pane, and keep the width in the browser", async () => {
  const user = userEvent.setup();
  renderDivider();
  const divider = screen.getByTestId("inbox-pane-divider");
  // The keys are the line's own, so the reader is on it: a press on it would start a drag.
  divider.focus();
  await user.keyboard("{ArrowLeft}");
  expect(standsAt()).toBe(62);
  await user.keyboard("{ArrowRight}{ArrowRight}");
  expect(standsAt()).toBe(58);
  expect(Number(window.localStorage.getItem(STORAGE_KEY))).toBeCloseTo(0.58, 5);
  // A key that is not an arrow leaves the line where it stands.
  await user.keyboard("{Enter}");
  expect(standsAt()).toBe(58);
});

test("a double-click puts the line back where it started", async () => {
  const user = userEvent.setup();
  renderDivider();
  const divider = screen.getByTestId("inbox-pane-divider");
  divider.focus();
  await user.keyboard("{ArrowLeft}{ArrowLeft}");
  expect(standsAt()).toBe(64);
  await user.dblClick(divider);
  expect(standsAt()).toBe(60);
  expect(Number(window.localStorage.getItem(STORAGE_KEY))).toBeCloseTo(DEFAULT_PANE_WIDTH, 5);
});

test("the width the reader set is read back on the next load, clamped to what the line allows", () => {
  window.localStorage.setItem(STORAGE_KEY, "0.45");
  renderDivider();
  expect(standsAt()).toBe(45);
  cleanup();

  // A value from another build, or one a person edited, never opens a pane that cannot be read:
  // it is clamped, and nonsense falls back to the default share.
  window.localStorage.setItem(STORAGE_KEY, "0.95");
  renderDivider();
  expect(standsAt()).toBe(80);
  cleanup();

  window.localStorage.setItem(STORAGE_KEY, "not a width");
  renderDivider();
  expect(standsAt()).toBe(60);
});
