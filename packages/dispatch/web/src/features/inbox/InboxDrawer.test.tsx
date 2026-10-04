import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { useState } from "react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type { InboxRow } from "../../api/types";
import { KeymapProvider } from "../shell/KeymapProvider";
import { userPreferenceStorageKey } from "../shell/userPreference";
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

function renderDrawer() {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  return render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <KeymapProvider>
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

test("toggling Mine/Everyone inside the drawer never writes the Inbox page's remembered view", async () => {
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
  const view = renderDrawer();
  try {
    fireEvent.click(screen.getByRole("button", { name: "Open inbox" }));
    const dialog = await screen.findByRole("dialog", { name: "Inbox" });
    await within(dialog).findByText("Which approach?");
    expect(within(dialog).queryByText("Bob's question?")).toBeNull();

    fireEvent.click(within(dialog).getByRole("button", { name: "Everyone" }));
    await within(dialog).findByText("Bob's question?");
    expect(window.localStorage.getItem(userPreferenceStorageKey("alice", "inbox.view"))).toBeNull();
  } finally {
    view.unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
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
