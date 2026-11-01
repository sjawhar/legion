import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { useState } from "react";
import { MemoryRouter, useLocation } from "react-router-dom";

import { api } from "../../api/client";
import type { InboxRow } from "../../api/types";
import { KeymapProvider } from "../shell/KeymapProvider";
import { InboxDrawer } from "./InboxDrawer";
import { installInboxApiMocks, issueAsk, mockAskReads } from "./inbox-fixture";

installInboxApiMocks();

/** A harness matching how `app.tsx` mounts the drawer: a trigger the reader's focus starts on,
 *  the drawer itself as a sibling, state living above both. */
function Harness() {
  const [open, setOpen] = useState(false);
  return (
    <>
      <button onClick={() => setOpen(true)} type="button">
        Open inbox
      </button>
      <InboxDrawer onClose={() => setOpen(false)} open={open} />
    </>
  );
}

/** What the drawer must never write to: the address of the page it is a peek over. */
function LocationProbe() {
  return <output data-testid="location">{useLocation().search}</output>;
}

function renderDrawer() {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  return render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <KeymapProvider>
          <LocationProbe />
          <Harness />
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
}

test("opens over the triggering control, moves initial focus to Close, and returns it on close", async () => {
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([issueAsk()]);
  const getAsk = mockAskReads([issueAsk()]);
  const view = renderDrawer();
  try {
    const opener = screen.getByRole("button", { name: "Open inbox" });
    opener.focus();
    fireEvent.click(opener);

    const dialog = await screen.findByRole("dialog", { name: "Inbox" });
    const closeButton = within(dialog).getByRole("button", { name: "Close" });
    await waitFor(() => expect(document.activeElement).toBe(closeButton));

    fireEvent.click(closeButton);
    await waitFor(() => expect(screen.queryByRole("dialog", { name: "Inbox" })).toBeNull());
    expect(document.activeElement).toBe(opener);
  } finally {
    view.unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("picking an assignee inside the drawer lists that person there, writes nothing to the address, and leaves the drawer open", async () => {
  const mine = issueAsk();
  const bobs = issueAsk({
    id: "ask-bob",
    issue: { assignee: "bob", key: "CORE-2", title: "Bob's issue" },
    issue_key: "CORE-2",
    question: "Bob's question?",
  });
  const rows = [mine, bobs];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const listUsers = spyOn(api, "listUsers").mockResolvedValue([
    { login: "alice" },
    { login: "bob" },
  ]);
  const view = renderDrawer();
  try {
    fireEvent.click(screen.getByRole("button", { name: "Open inbox" }));
    const dialog = await screen.findByRole("dialog", { name: "Inbox" });
    await within(dialog).findByText("Which approach?");
    expect(within(dialog).queryByText("Bob's question?")).toBeNull();

    fireEvent.click(within(dialog).getByRole("button", { name: "Filter by assignee" }));
    // The popover is portalled onto the body, so it is reached from the screen, not the drawer.
    fireEvent.click(await screen.findByRole("option", { name: /bob/ }));

    await within(dialog).findByText("Bob's question?");
    expect(screen.getByTestId("location").textContent).toBe("");
    expect(screen.getByRole("dialog", { name: "Inbox" })).toBe(dialog);
  } finally {
    view.unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
    listUsers.mockRestore();
  }
});

test("Snooze inside the drawer folds the row into Later, same as the Inbox page", async () => {
  let served: InboxRow[] = [issueAsk()];
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const snoozeAsk = spyOn(api, "snoozeAsk").mockImplementation(async (id, until) => {
    served = served.map((row) => (row.id === id ? { ...row, snoozed_until: until } : row));
    return { snoozed_until: until };
  });
  const getAsk = mockAskReads([issueAsk()]);
  const view = renderDrawer();
  try {
    fireEvent.click(screen.getByRole("button", { name: "Open inbox" }));
    const dialog = await screen.findByRole("dialog", { name: "Inbox" });
    await within(dialog).findByText("Which approach?");

    const row = within(dialog).getByTestId("ask-ask-a").closest<HTMLElement>("[data-inbox-row]");
    if (row === null) throw new Error("ask-a row missing");
    fireEvent.change(within(row).getByLabelText("Snooze CORE-1: Which approach?"), {
      target: { value: "tomorrow" },
    });

    await waitFor(() => expect(within(dialog).queryByText("Which approach?")).toBeNull());
    expect(snoozeAsk).toHaveBeenCalledTimes(1);
  } finally {
    view.unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
    snoozeAsk.mockRestore();
  }
});

test("Answering an ask inside the drawer submits and removes it from the list, same as the Inbox page", async () => {
  const withOptions = issueAsk({ options: [{ label: "Ship" }, { label: "Hold" }] });
  let served = [withOptions];
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const getAsk = mockAskReads([withOptions]);
  const answerAsk = spyOn(api, "answerAsk").mockImplementation(async () => {
    served = [];
    return {
      ...withOptions,
      answer: { at: "2026-09-11T03:00:00Z", selected: ["Ship"], text: "", user: "alice" },
      state: "answered",
    };
  });
  const view = renderDrawer();
  try {
    fireEvent.click(screen.getByRole("button", { name: "Open inbox" }));
    const dialog = await screen.findByRole("dialog", { name: "Inbox" });
    const card = await within(dialog).findByTestId("ask-ask-a");
    fireEvent.click(within(card).getByRole("radio", { name: "Ship" }));
    fireEvent.click(within(card).getByRole("button", { name: "Answer" }));

    await waitFor(() => expect(within(dialog).queryByTestId("ask-ask-a")).toBeNull());
    expect(answerAsk).toHaveBeenCalledTimes(1);
  } finally {
    view.unmount();
    answerAsk.mockRestore();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("Escape with a row focused backs out of the row first; only a second Escape closes the drawer", async () => {
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([issueAsk()]);
  const getAsk = mockAskReads([issueAsk()]);
  const view = renderDrawer();
  try {
    fireEvent.click(screen.getByRole("button", { name: "Open inbox" }));
    const dialog = await screen.findByRole("dialog", { name: "Inbox" });
    const row = within(dialog).getByTestId("ask-ask-a").closest<HTMLElement>("[data-inbox-row]");
    if (row === null) throw new Error("ask-a row missing");
    row.focus();
    expect(document.activeElement).toBe(row);

    fireEvent.keyDown(row, { key: "Escape" });
    expect(screen.queryByRole("dialog", { name: "Inbox" })).not.toBeNull();
    expect(document.activeElement).not.toBe(row);

    fireEvent.keyDown(document.body, { key: "Escape" });
    await waitFor(() => expect(screen.queryByRole("dialog", { name: "Inbox" })).toBeNull());
  } finally {
    view.unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});

test("at 1440 px the drawer is still today's Inbox: the card in the row, no reading pane", async () => {
  // A peek is a strip over the page it covers, so `Inbox pane={false}` holds at every width;
  // happy-dom answers `matchMedia` from the window, which is how a width is set here.
  const window_ = window as unknown as {
    happyDOM: { setViewport(size: { width: number }): void };
    innerWidth: number;
  };
  const was = window_.innerWidth;
  window_.happyDOM.setViewport({ width: 1440 });
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([issueAsk()]);
  const getAsk = mockAskReads([issueAsk()]);
  const view = renderDrawer();
  try {
    fireEvent.click(screen.getByRole("button", { name: "Open inbox" }));
    const dialog = await screen.findByRole("dialog", { name: "Inbox" });
    const card = await within(dialog).findByTestId("ask-ask-a");

    expect(card.closest("[data-inbox-row]")?.getAttribute("data-inbox-row")).toBe("ask-a");
    expect(screen.queryByTestId("inbox-pane")).toBeNull();
    expect(document.querySelector("[data-inbox-selected]")).toBeNull();
    // The address under the drawer is still the page's own.
    expect(screen.getByTestId("location").textContent).toBe("");
  } finally {
    view.unmount();
    window_.happyDOM.setViewport({ width: was });
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});
