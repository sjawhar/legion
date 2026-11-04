import { afterEach, beforeEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import {
  errorFrame,
  type FakeLiveSockets,
  installFakeLiveSockets,
  snapshotFrame,
} from "../../__tests__/live-socket-harness";
import { ApiError, api } from "../../api/client";
import type {
  AuthenticatedUser,
  LegionBoardCard,
  LegionBoardColumnID,
  LegionBoardResponse,
} from "../../api/types";
import { LegionView } from "./LegionView";

let sockets: FakeLiveSockets;
/** Every spy a test installed, put back when it ends, so one test's clock or identity is never
 *  the next one's. */
const spies: { mockRestore(): void }[] = [];
beforeEach(() => {
  sockets = installFakeLiveSockets();
});
afterEach(() => {
  cleanup();
  sockets.restore();
  for (const spy of spies.splice(0)) spy.mockRestore();
});

/** The ten columns every answer carries, in the order the server sends them. */
const COLUMNS: readonly { readonly id: LegionBoardColumnID; readonly label: string }[] = [
  { id: "queue", label: "Queue" },
  { id: "design", label: "Design" },
  { id: "planning", label: "Planning" },
  { id: "implementing", label: "Implementing" },
  { id: "testing", label: "Testing" },
  { id: "reviewing", label: "Reviewing" },
  { id: "accepting", label: "Accepting" },
  { id: "merging", label: "Merging" },
  { id: "awaiting_person", label: "Waiting on a person" },
  { id: "held", label: "Held" },
];

const NOW = Date.parse("2026-11-03T05:04:00Z");

function card(overrides: Partial<LegionBoardCard> = {}): LegionBoardCard {
  return {
    blocked_by: [],
    checks_verdict: null,
    children: 0,
    key: "ACME-1",
    phase: "implementing",
    phase_since: "2026-11-03T03:04:00Z",
    priority: 1,
    roles: [],
    slot: null,
    title: "Ship the widgets",
    tree: "ACME-1",
    waiting_on: null,
    ...overrides,
  };
}

function board(
  cards: Partial<Record<LegionBoardColumnID, readonly LegionBoardCard[]>> = {},
  overrides: Partial<LegionBoardResponse> = {}
): LegionBoardResponse {
  return {
    columns: COLUMNS.map((column) => ({ ...column, cards: cards[column.id] ?? [] })),
    computed_at: "2026-11-03T05:04:00Z",
    header: {
      queue_length: 0,
      read_at: "2026-11-03T05:03:30Z",
      read_error: null,
      read_ok: true,
      slot_cap: 4,
      slots_in_use: 2,
      trees: 2,
    },
    project: "ACME",
    setting_sources: {},
    settings: {},
    ...overrides,
  };
}

/** Renders the view, opens its socket and answers its subscription with `body`. `human` is what
 *  `/auth/whoami` says: a person is offered the settings control, a bearer is not. A bearer's
 *  answer names a kind the browser's own contract does not — the SPA types the route by the one
 *  answer it is meant to get — so this writes the answer a bearer really receives. */
async function renderView(body: LegionBoardResponse | null, human = true) {
  const whoAmI = spyOn(api, "whoAmI").mockResolvedValue(
    human
      ? { event_head: "0", kind: "user", login: "alice" }
      : ({ event_head: "0", kind: "agent", login: "alice" } as unknown as AuthenticatedUser)
  );
  spies.push(whoAmI);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <LegionView project="ACME" />
      </QueryClientProvider>
    </MemoryRouter>
  );
  await waitFor(() => expect(sockets.sockets.length).toBeGreaterThan(0));
  const socket = sockets.latest();
  act(() => socket.open());
  const frame = await waitFor(() => socket.subscribed("legion"));
  if (body !== null) {
    act(() => socket.receive(snapshotFrame(frame.sub, body)));
    await screen.findByText("Queue");
  }
  return { frame, socket, whoAmI };
}

function columnIds(): string[] {
  return [...document.querySelectorAll("[data-legion-column]")].map(
    (column) => column.getAttribute("data-legion-column") ?? ""
  );
}

test("the view subscribes the board over the page's live socket, by project alone", async () => {
  const { frame, socket } = await renderView(board());

  expect(frame.view).toEqual({ kind: "legion", project: "ACME" });
  // One subscription: the view makes no socket call of its own beside the page's.
  expect(socket.subscribes()).toHaveLength(1);
});

test("the ten columns are drawn in the order the server sent them", async () => {
  await renderView(board());

  expect(columnIds()).toEqual(COLUMNS.map((column) => column.id));
  for (const column of COLUMNS) {
    expect(screen.getByRole("region", { name: column.label })).toBeDefined();
  }
});

test("the queue keeps the daemon's own order, which the view does not re-sort", async () => {
  // Out of every order of Dispatch's own - not by key, not by priority, not by phase time.
  const waiting = [
    card({ key: "ACME-9", priority: 3, title: "Answered, so first" }),
    card({ key: "ACME-2", priority: 0, title: "Next by rank" }),
    card({ key: "ACME-7", priority: 1, title: "Last by rank" }),
  ];
  await renderView(board({ queue: waiting }));

  const queue = screen.getByRole("region", { name: "Queue" });
  expect(
    [...queue.querySelectorAll("[data-legion-card]")].map((node) =>
      node.getAttribute("data-legion-card")
    )
  ).toEqual(["ACME-9", "ACME-2", "ACME-7"]);
});

test("a card shows its key, title, priority, time in phase, slot, the roles in a turn and the CI verdict", async () => {
  spies.push(spyOn(Date, "now").mockReturnValue(NOW));
  await renderView(
    board({
      implementing: [
        card({
          checks_verdict: "failing",
          key: "ACME-4",
          phase_since: "2026-11-03T02:34:00Z",
          priority: 0,
          roles: ["implementer", "reviewer"],
          slot: 2,
          title: "Hot reload the settings",
        }),
      ],
    })
  );

  const issue = screen.getByRole("article", { name: "ACME-4 Hot reload the settings" });
  expect(within(issue).getByText("ACME-4")).toBeDefined();
  expect(within(issue).getByText("Hot reload the settings")).toBeDefined();
  expect(within(issue).getByText("P0")).toBeDefined();
  // 02:34 to 05:04 is two and a half hours.
  expect(issue.querySelector('[data-legion-fact="phase-time"]')?.textContent).toContain("2 h 30 m");
  expect(issue.querySelector('[data-legion-fact="slot"]')?.textContent).toContain("2");
  expect(issue.querySelector('[data-legion-fact="roles"]')?.textContent).toContain(
    "implementer, reviewer"
  );
  expect(issue.querySelector('[data-legion-fact="checks"]')?.textContent).toContain("failing");
});

test("a card with no phase entry recorded shows no time in the phase rather than one nobody measured", async () => {
  await renderView(board({ held: [card({ key: "ACME-5", phase_since: null })] }));

  const issue = screen.getByRole("article", { name: "ACME-5 Ship the widgets" });
  expect(issue.querySelector('[data-legion-fact="phase-time"]')?.textContent).toContain("—");
});

test("the header counts the slots in use against the cap and the queue's length", async () => {
  await renderView(
    board(
      {},
      {
        header: {
          queue_length: 3,
          read_at: "2026-11-03T05:03:30Z",
          read_error: null,
          read_ok: true,
          slot_cap: 4,
          slots_in_use: 2,
          trees: 2,
        },
      }
    )
  );

  expect(document.querySelector("[data-legion-slots]")?.textContent).toBe("2 of 4 slots in use");
  expect(document.querySelector("[data-legion-queue-length]")?.textContent).toBe(
    "3 waiting in the queue"
  );
});

test("a daemon that has never answered shows the view with empty columns and the failure in the header", async () => {
  await renderView(
    board(
      {},
      {
        header: {
          queue_length: 0,
          read_at: null,
          read_error: "dial tcp: connection refused",
          read_ok: false,
          slot_cap: null,
          slots_in_use: 0,
          trees: 0,
        },
      }
    )
  );

  // The view is there, with all ten columns and no card on any of them.
  expect(columnIds()).toEqual(COLUMNS.map((column) => column.id));
  expect(document.querySelectorAll("[data-legion-card]")).toHaveLength(0);
  const failure = document.querySelector("[data-legion-read-error]");
  expect(failure?.textContent).toContain("never answered");
  expect(failure?.textContent).toContain("dial tcp: connection refused");
  expect(document.querySelector("[data-legion-slots]")?.textContent).toBe(
    "0 of an unknown number of slots in use"
  );
});

test("a read that failed after a good one keeps the board and says why the last read failed", async () => {
  await renderView(
    board(
      { planning: [card({ key: "ACME-3" })] },
      {
        header: {
          queue_length: 0,
          read_at: "2026-11-03T05:03:30Z",
          read_error: "context deadline exceeded",
          read_ok: false,
          slot_cap: 4,
          slots_in_use: 1,
          trees: 1,
        },
      }
    )
  );

  expect(screen.getByRole("article", { name: "ACME-3 Ship the widgets" })).toBeDefined();
  expect(document.querySelector("[data-legion-read-error]")?.textContent).toContain(
    "context deadline exceeded"
  );
});

test("a project whose setting was removed while the view was open says so, with no retry", async () => {
  const { frame, socket } = await renderView(null);
  act(() =>
    socket.receive(errorFrame(frame.sub, "LEGION_NOT_CONFIGURED", "this project has no daemon"))
  );

  expect(await screen.findByText("ACME no longer names a Legion daemon.")).toBeDefined();
  expect(screen.queryByRole("alert")).toBeNull();
});

const SETTINGS = {
  settings: { admission_cap: 4, instructions: "work/instructions.md" },
  setting_sources: {
    admission_cap: { at: "2026-11-02T09:00:00Z", by: "bob", source: "route" as const },
    instructions: { at: "2026-10-20T00:00:00Z", by: null, source: "file" as const },
  },
};

test("the settings panel shows each setting's value and where it came from", async () => {
  await renderView(board({}, SETTINGS));

  const cap = document.querySelector('[data-legion-setting="admission_cap"]');
  expect(cap?.querySelector("[data-legion-setting-value]")?.textContent).toBe("4");
  expect(cap?.querySelector('[data-legion-setting-source="route"]')?.textContent).toContain(
    "Changed here by bob"
  );
  const instructions = document.querySelector('[data-legion-setting="instructions"]');
  expect(instructions?.querySelector("[data-legion-setting-value]")?.textContent).toBe(
    '"work/instructions.md"'
  );
  expect(instructions?.querySelector('[data-legion-setting-source="file"]')?.textContent).toContain(
    "configuration file"
  );
});

test("a daemon no read has recorded the settings of says so rather than showing an empty table", async () => {
  await renderView(board());

  expect(document.querySelector("[data-legion-settings-empty]")?.textContent).toContain(
    "No read of this daemon has recorded its settings yet"
  );
});

// The relay answers the daemon's own body, which keys each verdict by path under `settings`
// (`LegionSettingsChangeResponse` in @legion/contracts/legion-api, the daemon's
// api.SettingsChangeResponse): a fake answering a flat map of paths is a fake no daemon matches,
// which is why the shape is a contract type both ends read.
test("a change goes to the relay route and shows what the daemon did with the key", async () => {
  const put = spyOn(api, "putLegionProjectSettings").mockResolvedValue({
    settings: { admission_cap: { applied: "at-once" } },
  });
  try {
    await renderView(board({}, SETTINGS));

    const input = screen.getByLabelText("New value for admission_cap") as HTMLInputElement;
    expect(input.value).toBe("4");
    fireEvent.change(input, { target: { value: "6" } });
    fireEvent.click(screen.getByRole("button", { name: "Change admission_cap" }));

    expect(await screen.findByText("Applied at once")).toBeDefined();
    expect(put).toHaveBeenCalledWith("ACME", { admission_cap: 6 }, expect.anything());
    // The row shows the new value, and that it came from here, by the person who changed it.
    const cap = document.querySelector('[data-legion-setting="admission_cap"]');
    await waitFor(() =>
      expect(cap?.querySelector("[data-legion-setting-value]")?.textContent).toBe("6")
    );
    expect(cap?.querySelector('[data-legion-setting-source="route"]')?.textContent).toContain(
      "Changed here by alice"
    );
  } finally {
    put.mockRestore();
  }
});

test("a refused key shows the daemon's reason and leaves the value as it was", async () => {
  const put = spyOn(api, "putLegionProjectSettings").mockResolvedValue({
    settings: { admission_cap: { refused: "a running issue holds a slot" } },
  });
  try {
    await renderView(board({}, SETTINGS));

    fireEvent.change(screen.getByLabelText("New value for admission_cap"), {
      target: { value: "1" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Change admission_cap" }));

    expect(await screen.findByText("Refused: a running issue holds a slot")).toBeDefined();
    const cap = document.querySelector('[data-legion-setting="admission_cap"]');
    await waitFor(() =>
      expect(cap?.querySelector("[data-legion-setting-value]")?.textContent).toBe("4")
    );
  } finally {
    put.mockRestore();
  }
});

// The backend's own refusal is the other half of D12's gate: the control is offered to any
// signed-in person, and a caller whose credential names one project is refused the write, which
// `scopeDeployment` answers `403 PROJECT_SCOPE_DEPLOYMENT`. A `503` is not this test's refusal:
// the keyed write retries every 5xx under its key until Dispatch decides it (`retriesUnderKey`),
// so a daemon that cannot be reached is tried again rather than shown.
test("a refusal Dispatch itself made lands on the row it was made from", async () => {
  const put = spyOn(api, "putLegionProjectSettings").mockRejectedValue(
    new ApiError(403, {
      code: "PROJECT_SCOPE_DEPLOYMENT",
      error: "this request's credential may write project ACME and nothing else",
    })
  );
  try {
    await renderView(board({}, SETTINGS));

    fireEvent.change(screen.getByLabelText("New value for admission_cap"), {
      target: { value: "6" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Change admission_cap" }));

    expect(
      await screen.findByText(/this request's credential may write project ACME/)
    ).toBeDefined();
    const cap = document.querySelector('[data-legion-setting="admission_cap"]');
    await waitFor(() =>
      expect(cap?.querySelector("[data-legion-setting-value]")?.textContent).toBe("4")
    );
  } finally {
    put.mockRestore();
  }
});

test("a value that is not JSON is refused here, and nothing is sent", async () => {
  const put = spyOn(api, "putLegionProjectSettings");
  try {
    await renderView(board({}, SETTINGS));

    fireEvent.change(screen.getByLabelText("New value for admission_cap"), {
      target: { value: "six" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Change admission_cap" }));

    expect(await screen.findByText(/That is not JSON/)).toBeDefined();
    expect(put).not.toHaveBeenCalled();
  } finally {
    put.mockRestore();
  }
});

test("a reader who is not a person is shown the settings and offered no control", async () => {
  await renderView(board({}, SETTINGS), false);

  expect(document.querySelector('[data-legion-setting="admission_cap"]')).not.toBeNull();
  expect(screen.queryByLabelText("New value for admission_cap")).toBeNull();
  expect(screen.queryByRole("button", { name: "Change admission_cap" })).toBeNull();
});
