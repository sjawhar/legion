import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { seedDeliveryReads } from "../../__tests__/live-socket-harness";
import { ApiError, api } from "../../api/client";
import type { Agent, DeliverySessionGroup } from "../../api/types";
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

function renderEditor(
  groups: readonly DeliverySessionGroup[] = [],
  values: {
    agents?: readonly Agent[];
    titles?: Readonly<Record<string, string>>;
    counts?: Readonly<Record<string, number>>;
  } = {}
) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  // The agent registry, as the shell's poller last read it: none listed unless a test says so.
  queryClient.setQueryData(["agents"], values.agents ?? []);
  render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <KeymapProvider>
          <SessionGroupsEditor
            counts={values.counts ?? counts}
            groups={groups}
            onClose={() => {}}
            titles={values.titles ?? titles}
          />
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  return queryClient;
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

// Each session is an agent: the checkbox's label keeps its name, and its live link sits beside
// the label, outside it.
test("each session's checkbox is named by its agent, with its live link beside the label", () => {
  renderEditor([]);
  try {
    const checkbox = screen.getByRole("checkbox", { name: "Planner one" });
    const label = checkbox.closest("label");
    expect(label?.getAttribute("title")).toBe("s1");
    const live = screen.getByRole("link", { name: "Open live conversation with Planner one" });
    expect(live.getAttribute("href")).toBe("/agents/s1/live");
    expect(label?.contains(live)).toBe(false);
    expect(live.closest("[data-agent-name]")?.getAttribute("data-agent-name")).toBe("s1");
  } finally {
    cleanup();
  }
});

// A session the registry lists reads by its current title, not the one the delivery read stored;
// a session whose stored title is its own id has none, and reads by its short id.
test("a session reads by its registry title, else its stored title, else its short id", () => {
  renderEditor([], {
    agents: [
      {
        capabilities: [],
        dir: "/home/alice/legion",
        host: "devbox-alice",
        last_activity: null,
        last_seen: 1,
        machine_id: "devbox-alice",
        open_asks: 0,
        roles: [],
        session_id: "s1",
        title: "Planner renamed",
      },
    ],
    titles: { s1: "Planner one", s2: "Planner two", s3: "s3" },
  });
  try {
    expect(screen.getByRole("checkbox", { name: "Planner renamed" })).toBeDefined();
    expect(screen.getByRole("checkbox", { name: "Planner two" })).toBeDefined();
    expect(screen.getByRole("checkbox", { name: "session:s3…" })).toBeDefined();
  } finally {
    cleanup();
  }
});

test("create: two selected sessions and a name become a group; the live views hear of it from Dispatch, and only the reads that are not live read again", async () => {
  const create = spyOn(api, "createDeliverySessionGroup").mockResolvedValue(crew);
  const queryClient = renderEditor();
  const reads = seedDeliveryReads(queryClient);
  try {
    fireEvent.click(screen.getByRole("checkbox", { name: "Planner one" }));
    fireEvent.click(screen.getByRole("checkbox", { name: "Reviewer" }));
    fireEvent.change(screen.getByLabelText("New group name"), {
      target: { value: "Release crew" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Create group" }));
    await waitFor(() =>
      expect(create).toHaveBeenCalledWith(
        { name: "Release crew", sessions: ["s1", "s3"] },
        keyedAttempt
      )
    );
    await waitFor(() => expect(queryClient.getQueryState(reads.run)?.isInvalidated).toBe(true));
    expect(queryClient.getQueryState(reads.live)?.isInvalidated).toBe(false);
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
    await waitFor(() =>
      expect(update).toHaveBeenLastCalledWith(crew.id, { name: "Ship crew" }, keyedAttempt)
    );

    fireEvent.click(screen.getByRole("checkbox", { name: "Reviewer" }));
    fireEvent.click(within(group).getByRole("button", { name: "Add selected" }));
    await waitFor(() =>
      expect(update).toHaveBeenLastCalledWith(crew.id, { add: ["s3"] }, keyedAttempt)
    );

    fireEvent.click(within(group).getByRole("button", { name: "Remove Planner two" }));
    await waitFor(() =>
      expect(update).toHaveBeenLastCalledWith(crew.id, { remove: ["s2"] }, keyedAttempt)
    );

    fireEvent.click(within(group).getByRole("button", { name: "Delete group" }));
    await waitFor(() => expect(remove).toHaveBeenCalledWith(crew.id, keyedAttempt));
  } finally {
    cleanup();
    update.mockRestore();
    remove.mockRestore();
  }
});

test("group actions are disabled while a save is pending", async () => {
  let settle: (value: DeliverySessionGroup) => void = () => {};
  const pending = new Promise<DeliverySessionGroup>((resolve) => {
    settle = resolve;
  });
  const update = spyOn(api, "updateDeliverySessionGroup").mockImplementation(() => pending);
  renderEditor([crew]);
  try {
    const group = screen.getByRole("group", { name: "Release crew" });
    fireEvent.click(screen.getByRole("checkbox", { name: "Reviewer" }));
    fireEvent.change(within(group).getByLabelText("Name"), { target: { value: "Ship crew" } });
    fireEvent.click(within(group).getByRole("button", { name: "Rename" }));
    await waitFor(() =>
      expect(update).toHaveBeenCalledWith(crew.id, { name: "Ship crew" }, keyedAttempt)
    );

    for (const name of ["Rename", "Add selected", "Remove Planner two", "Delete group"]) {
      expect((within(group).getByRole("button", { name }) as HTMLButtonElement).disabled).toBe(
        true
      );
    }
    settle(crew);
  } finally {
    cleanup();
    update.mockRestore();
  }
});

// Each member is an agent linked to its live conversation; one no pull request of the window
// names has no stored title, so it reads by its short id.
test("group members link to their live conversations, an untitled one by its short id", () => {
  renderEditor([crew]);
  try {
    const group = screen.getByRole("group", { name: "Release crew" });
    expect(within(group).getByRole("link", { name: "Planner two" }).getAttribute("href")).toBe(
      "/agents/s2/live"
    );
    expect(within(group).getByRole("link", { name: "session:s9…" }).getAttribute("href")).toBe(
      "/agents/s9/live"
    );
    expect(within(group).getByRole("button", { name: "Remove session:s9…" })).toBeDefined();
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

// Dispatch made the group and lost its answer (`409 KEY_COMMITTED`): a landing, not a refusal. The
// name and the selection go, so a second press makes nothing, and the editor says what was made
// instead of showing a refusal.
test("a group made with its answer lost clears the form and says so, and a second press makes nothing", async () => {
  const create = spyOn(api, "createDeliverySessionGroup").mockRejectedValue(
    new ApiError(409, { code: "KEY_COMMITTED", error: "made; the answer was lost" })
  );
  const queryClient = renderEditor();
  const reads = seedDeliveryReads(queryClient);
  try {
    fireEvent.click(screen.getByRole("checkbox", { name: "Planner one" }));
    fireEvent.change(screen.getByLabelText("New group name"), { target: { value: "Crew" } });
    fireEvent.click(screen.getByRole("button", { name: "Create group" }));
    const made = await screen.findByTestId("landed-without-answer");
    expect(made.textContent).toBe(
      "The group “Crew” was made, but Dispatch lost its answer, so it may take a moment to appear."
    );
    expect(screen.queryByRole("alert")).toBeNull();
    expect((screen.getByLabelText("New group name") as HTMLInputElement).value).toBe("");
    expect(
      (screen.getByRole("checkbox", { name: "Planner one" }) as HTMLInputElement).checked
    ).toBe(false);
    expect(
      (screen.getByRole("button", { name: "Create group" }) as HTMLButtonElement).disabled
    ).toBe(true);
    await waitFor(() => expect(queryClient.getQueryState(reads.run)?.isInvalidated).toBe(true));
    expect(create).toHaveBeenCalledTimes(1);
  } finally {
    cleanup();
    create.mockRestore();
  }
});
