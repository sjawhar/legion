import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen } from "@testing-library/react";
import { type ReactNode, useState } from "react";

import { api } from "../../api/client";
import type { DispatchUser } from "../../api/types";
import { AssigneeFilter } from "./AssigneeFilter";
import { UNASSIGNED_FILTER_VALUE } from "./sections";

const viewer = "alice@example.com";
const bob = "bob@example.com";
/** Someone the people list never carries, as a value read from `?assignee=` can be. */
const carol = "carol@example.com";
const people: DispatchUser[] = [{ login: viewer }, { login: bob }];

/** The host the Inbox is: it owns the popover's open state, the filter only reports it. */
function Host({
  onChange,
  selected,
}: {
  onChange: (selected: readonly string[]) => void;
  selected: readonly string[];
}): ReactNode {
  const [open, setOpen] = useState(false);
  return (
    <AssigneeFilter
      onChange={onChange}
      onOpenChange={setOpen}
      open={open}
      selected={selected}
      viewer={viewer}
    />
  );
}

function renderFilter({
  onChange = () => {},
  selected = [viewer],
  /** An Error rejects `GET /users`, as an unreachable server does. */
  users = people,
}: {
  onChange?: (selected: readonly string[]) => void;
  selected?: readonly string[];
  users?: DispatchUser[] | Error;
} = {}) {
  const listUsers = spyOn(api, "listUsers");
  if (users instanceof Error) {
    listUsers.mockRejectedValue(users);
  } else {
    listUsers.mockResolvedValue(users);
  }
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <QueryClientProvider client={queryClient}>
      <Host onChange={onChange} selected={selected} />
    </QueryClientProvider>
  );
  return {
    listUsers,
    unmount: () => {
      view.unmount();
      listUsers.mockRestore();
    },
  };
}

function trigger(): HTMLElement {
  return screen.getByRole("button", { name: "Filter by assignee" });
}

/** Each row's whole text: its label, then the muted detail the reader's own row carries. */
function optionTexts(): string[] {
  return screen.getAllByRole("option").map((option) => option.textContent ?? "");
}

test("the trigger names the selection in the reader's own words", () => {
  const cases: readonly (readonly [readonly string[], string])[] = [
    [[viewer], "Assignee · you"],
    [[], "Assignee · everyone"],
    [[UNASSIGNED_FILTER_VALUE], "Assignee · Unassigned"],
    [[bob], `Assignee · ${bob}`],
    [[viewer, bob], "Assignee · 2"],
  ];
  for (const [selected, text] of cases) {
    const { unmount } = renderFilter({ selected });
    try {
      expect(trigger().textContent).toBe(text);
    } finally {
      unmount();
    }
  }
});

test("the people are read on the first open and then kept, never on a render", async () => {
  const { listUsers, unmount } = renderFilter();
  try {
    // An Inbox nobody filters costs no read of who has signed in.
    expect(listUsers).not.toHaveBeenCalled();

    fireEvent.click(trigger());
    await screen.findByRole("option", { name: new RegExp(bob) });
    expect(listUsers).toHaveBeenCalledTimes(1);

    fireEvent.click(trigger());
    fireEvent.click(trigger());
    await screen.findByRole("option", { name: new RegExp(bob) });
    expect(listUsers).toHaveBeenCalledTimes(1);
  } finally {
    unmount();
  }
});

test("the options list Unassigned first, then the logins, and detail the reader's own row", async () => {
  const { unmount } = renderFilter({ selected: [] });
  try {
    fireEvent.click(trigger());
    await screen.findByRole("option", { name: new RegExp(bob) });
    // The detail runs into the label in `textContent`: `you` sits at the end of the reader's row.
    expect(optionTexts()).toEqual(["Unassigned", `${viewer}you`, bob]);
  } finally {
    unmount();
  }
});

test("the search narrows the people and says so when it matches nobody", async () => {
  const { unmount } = renderFilter({ selected: [] });
  try {
    fireEvent.click(trigger());
    await screen.findByRole("option", { name: new RegExp(bob) });
    const search = screen.getByRole("combobox", { name: "Search people" });

    fireEvent.change(search, { target: { value: "bob" } });
    expect(optionTexts()).toEqual([bob]);

    fireEvent.change(search, { target: { value: "unas" } });
    expect(optionTexts()).toEqual(["Unassigned"]);

    fireEvent.change(search, { target: { value: "nobody" } });
    expect(screen.queryAllByRole("option")).toEqual([]);
    expect(screen.getByText("No matching person.")).toBeDefined();
  } finally {
    unmount();
  }
});

test("a failed read says so and leaves the filter working from the address", async () => {
  const changes: (readonly string[])[] = [];
  const { unmount } = renderFilter({
    onChange: (selected) => changes.push(selected),
    selected: [carol],
    users: new Error("users unreachable"),
  });
  try {
    fireEvent.click(trigger());
    await screen.findByText("Could not load the people list.");
    // What the address filters by is still there to see and to switch off.
    expect(optionTexts()).toEqual(["Unassigned", `✓${carol}`]);

    fireEvent.click(screen.getByRole("option", { name: new RegExp(carol) }));
    fireEvent.click(screen.getByRole("option", { name: /Unassigned/ }));
    expect(changes).toEqual([[], [carol, UNASSIGNED_FILTER_VALUE]]);
  } finally {
    unmount();
  }
});

test("a selected person the people list does not hold is still a row", async () => {
  const { unmount } = renderFilter({ selected: [carol] });
  try {
    fireEvent.click(trigger());
    await screen.findByRole("option", { name: new RegExp(bob) });
    expect(optionTexts()).toEqual(["Unassigned", `${viewer}you`, bob, `✓${carol}`]);
  } finally {
    unmount();
  }
});
