import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { ApiError, api } from "../../api/client";
import type { DeliverySessionGroup } from "../../api/types";
import { KeymapProvider } from "../shell/KeymapProvider";
import { SessionGroupsEditor } from "./SessionGroupsEditor";

const crew: DeliverySessionGroup = {
  created_at: "2026-10-11T00:00:00Z",
  created_by: { id: "alice@example.com", kind: "user" },
  id: "11111111-1111-1111-1111-111111111111",
  name: "Release crew",
  sessions: ["s2", "s9"],
  updated_at: "2026-10-11T00:00:00Z",
  updated_by: { id: "alice@example.com", kind: "user" },
};

const titles = { s1: "Planner one", s2: "Planner two", s3: "Reviewer" };
const counts = { s1: 3, s2: 2, s3: 1 };

function renderEditor(groups: readonly DeliverySessionGroup[] = []) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const invalidate = spyOn(queryClient, "invalidateQueries");
  render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <KeymapProvider>
          <SessionGroupsEditor counts={counts} groups={groups} onClose={() => {}} titles={titles} />
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  return invalidate;
}

test("it lists the window's sessions busiest first, each with its title, count and group", () => {
  renderEditor([crew]);
  try {
    const rows = within(screen.getByRole("list", { name: "Sessions in this window" })).getAllByRole(
      "listitem"
    );
    expect(rows.map((row) => row.textContent)).toEqual([
      expect.stringContaining("Planner one"),
      expect.stringContaining("Planner two"),
      expect.stringContaining("Reviewer"),
    ]);
    expect(rows[0]?.textContent).toContain("3 PRs");
    expect(rows[1]?.textContent).toContain("Release crew");
  } finally {
    cleanup();
  }
});

test("create: two selected sessions and a name become a group, and the page reads again", async () => {
  const create = spyOn(api, "createDeliverySessionGroup").mockResolvedValue(crew);
  const invalidate = renderEditor();
  try {
    fireEvent.click(screen.getByRole("checkbox", { name: "Planner one" }));
    fireEvent.click(screen.getByRole("checkbox", { name: "Reviewer" }));
    fireEvent.change(screen.getByLabelText("New group name"), {
      target: { value: "Release crew" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Create group" }));
    await waitFor(() =>
      expect(create).toHaveBeenCalledWith({ name: "Release crew", sessions: ["s1", "s3"] })
    );
    await waitFor(() => expect(invalidate).toHaveBeenCalledWith({ queryKey: ["delivery"] }));
  } finally {
    cleanup();
    create.mockRestore();
  }
});

test("rename, add, remove and delete each write their one change", async () => {
  const update = spyOn(api, "updateDeliverySessionGroup").mockResolvedValue(crew);
  const remove = spyOn(api, "deleteDeliverySessionGroup").mockResolvedValue(undefined);
  renderEditor([crew]);
  try {
    const group = screen.getByRole("group", { name: "Release crew" });
    fireEvent.change(within(group).getByLabelText("Name"), { target: { value: "Ship crew" } });
    fireEvent.click(within(group).getByRole("button", { name: "Rename" }));
    await waitFor(() => expect(update).toHaveBeenLastCalledWith(crew.id, { name: "Ship crew" }));

    fireEvent.click(screen.getByRole("checkbox", { name: "Reviewer" }));
    fireEvent.click(within(group).getByRole("button", { name: "Add selected" }));
    await waitFor(() => expect(update).toHaveBeenLastCalledWith(crew.id, { add: ["s3"] }));

    fireEvent.click(within(group).getByRole("button", { name: "Remove Planner two" }));
    await waitFor(() => expect(update).toHaveBeenLastCalledWith(crew.id, { remove: ["s2"] }));

    fireEvent.click(within(group).getByRole("button", { name: "Delete group" }));
    await waitFor(() => expect(remove).toHaveBeenCalledWith(crew.id));
  } finally {
    cleanup();
    update.mockRestore();
    remove.mockRestore();
  }
});

test("a member no pull request of the window names shows by its id", () => {
  renderEditor([crew]);
  try {
    const group = screen.getByRole("group", { name: "Release crew" });
    expect(within(group).getByRole("button", { name: "Remove s9" })).toBeDefined();
  } finally {
    cleanup();
  }
});

test("a refusal shows as the server wrote it", async () => {
  const create = spyOn(api, "createDeliverySessionGroup").mockRejectedValue(
    new ApiError(409, {
      code: "SESSION_IN_GROUP",
      error: 'session s2 is already in the group "Release crew"; remove it there first',
    })
  );
  renderEditor();
  try {
    fireEvent.click(screen.getByRole("checkbox", { name: "Planner two" }));
    fireEvent.change(screen.getByLabelText("New group name"), { target: { value: "Other" } });
    fireEvent.click(screen.getByRole("button", { name: "Create group" }));
    expect(
      await screen.findByText(
        'session s2 is already in the group "Release crew"; remove it there first'
      )
    ).toBeDefined();
  } finally {
    cleanup();
    create.mockRestore();
  }
});
