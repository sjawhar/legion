import { expect, jest, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";

import { keptTab } from "../../__tests__/kept-sends-harness";
import { ApiError, api, type WriteBounds } from "../../api/client";
import { SEND_DEADLINE_MS } from "./keyed-write";
import { PendingWritesIndicator } from "./PendingWritesIndicator";
import { plainWrite, type Writes, writesOf } from "./write-store";

/** One write the indicator shows, labelled `label`, its attempt `write`. */
function indicatorWrite(
  writes: Writes,
  label: string,
  write: (bounds: WriteBounds) => Promise<unknown>
): void {
  void writes
    .run({ detail: null, kind: plainWrite, label, presentation: "indicator", write })
    .catch(() => undefined);
}

const refusal = new ApiError(503, { code: "UNAVAILABLE", error: "the server is down" });
const verdict = new ApiError(422, { code: "KEY_REUSED", error: "the key names another write" });

/** The shell's indicator over one app's write queue, which the test drives directly: the rows are
 *  the queue's, whichever surface made them. */
function mountIndicator(): { client: QueryClient; unmount: () => void } {
  const client = new QueryClient({ defaultOptions: { mutations: { retry: false } } });
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={client}>{children}</QueryClientProvider>
  );
  const view = render(<PendingWritesIndicator />, { wrapper });
  return { client, unmount: () => view.unmount() };
}

async function settle(): Promise<void> {
  await act(async () => {
    for (let turn = 0; turn < 20; turn += 1) await Promise.resolve();
  });
}

/** Moves the fake clock `ms` and lets what it released run. */
async function tick(ms: number): Promise<void> {
  await act(async () => {
    jest.advanceTimersByTime(ms);
    for (let turn = 0; turn < 20; turn += 1) await Promise.resolve();
  });
}

// Nothing pending, nothing on screen: the indicator is chrome, and an empty one would sit over the
// page for ever.
test("the indicator shows nothing while no write is out", () => {
  const { unmount } = mountIndicator();
  try {
    expect(screen.queryByTestId("pending-writes")).toBeNull();
  } finally {
    unmount();
  }
});

// What a write is, how many are out, and what the reader may do about each: the count while they
// are within their deadline, the "not confirmed" line and Stop trying once one is late, and -
// once the reader stops it - its name with Retry and Discard. A write that lands leaves.
test("the indicator counts the writes out, names a late one, and clears when it lands", async () => {
  jest.useFakeTimers();
  const tab = keptTab();
  const patchIssue = spyOn(api, "patchIssue").mockRejectedValue(refusal);
  const held = Promise.withResolvers<never>();
  const putIssueState = spyOn(api, "putIssueState").mockReturnValue(held.promise);
  const { client, unmount } = mountIndicator();
  const writes = writesOf(client);

  try {
    indicatorWrite(writes, "CORE-1 set to done", (bounds) =>
      api.patchIssue("CORE-1", { status: "done" }, bounds)
    );
    indicatorWrite(writes, "CORE-2 pinned", (bounds) =>
      api.putIssueState("CORE-2", { pinned: true }, bounds)
    );
    await settle();

    const indicator = await screen.findByTestId("pending-writes");
    expect(within(indicator).getByRole("status").textContent).toBe("Saving 2 changes…");
    expect(within(indicator).queryByRole("button", { name: "Stop trying" })).toBeNull();

    await tick(SEND_DEADLINE_MS);
    expect(within(indicator).getByRole("status").textContent).toBe(
      "Dispatch has not confirmed 2 changes — still trying"
    );
    expect(within(indicator).getByText("CORE-1 set to done")).toBeTruthy();
    expect(within(indicator).getByText("CORE-2 pinned")).toBeTruthy();
    expect(within(indicator).getAllByRole("button", { name: "Stop trying" })).toHaveLength(2);

    // Stop trying ends the first one: it names itself with why, and offers Retry and Discard.
    fireEvent.click(
      within(indicator).getAllByRole("button", { name: "Stop trying" })[0] as Element
    );
    await tick(30 * 60_000);
    expect(
      within(indicator).getByText(/^Not saved: CORE-1 set to done — Dispatch has not confirmed it/)
    ).toBeTruthy();
    expect(within(indicator).getByRole("button", { name: "Retry" })).toBeTruthy();
    expect(within(indicator).getByRole("button", { name: "Discard" })).toBeTruthy();
    expect(within(indicator).getByRole("status").textContent).toBe(
      "Dispatch has not confirmed 1 change — still trying"
    );

    // Discard drops the refusal; the pin write lands, and the indicator goes with it.
    fireEvent.click(within(indicator).getByRole("button", { name: "Discard" }));
    await settle();
    expect(screen.queryByText("CORE-1 set to done")).toBeNull();

    // The pin write is stopped too; its attempt in flight then fails, and its Retry lands, so the
    // indicator goes with the last write it held.
    fireEvent.click(within(indicator).getByRole("button", { name: "Stop trying" }));
    await settle();
    held.reject(refusal);
    await tick(30 * 60_000);
    expect(within(indicator).getByRole("button", { name: "Retry" })).toBeTruthy();

    putIssueState.mockResolvedValueOnce({ pinned: true } as never);
    fireEvent.click(within(indicator).getByRole("button", { name: "Retry" }));
    await settle();
    await waitFor(() => expect(screen.queryByTestId("pending-writes")).toBeNull());
  } finally {
    unmount();
    patchIssue.mockRestore();
    putIssueState.mockRestore();
    tab.close();
    jest.advanceTimersByTime(1);
    jest.useRealTimers();
  }
});

// A write Dispatch refused says so in its own words, and its Retry asks again - under a key of
// its own, since Dispatch replays the refusal it stored under the first.
test("a refused write names Dispatch's reason, and Retry sends it again", async () => {
  const tab = keptTab();
  const patchIssue = spyOn(api, "patchIssue").mockRejectedValueOnce(verdict);
  const { client, unmount } = mountIndicator();
  const writes = writesOf(client);

  try {
    indicatorWrite(writes, "CORE-1 set to done", (bounds) =>
      api.patchIssue("CORE-1", { status: "done" }, bounds)
    );
    await settle();

    const indicator = await screen.findByTestId("pending-writes");
    expect(
      within(indicator).getByText("Not saved: CORE-1 set to done — the key names another write")
    ).toBeTruthy();
    expect(within(indicator).queryByRole("status")).toBeNull();

    patchIssue.mockResolvedValueOnce({ key: "CORE-1" } as never);
    fireEvent.click(within(indicator).getByRole("button", { name: "Retry" }));
    await settle();
    expect(patchIssue).toHaveBeenCalledTimes(2);
    expect(patchIssue.mock.calls[1]?.[2]?.key).not.toBe(patchIssue.mock.calls[0]?.[2]?.key);
    await waitFor(() => expect(screen.queryByTestId("pending-writes")).toBeNull());
  } finally {
    unmount();
    patchIssue.mockRestore();
    tab.close();
  }
});
