import { afterEach, expect, jest, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type { CredentialPendingRow } from "../../api/types";
import { runningAgent } from "./agent-fixture";
import { CredentialRequestsSection } from "./CredentialRequestsSection";
import type { CredentialRequests } from "./pending";

afterEach(cleanup);

function pendingSecretRow(overrides: Partial<CredentialPendingRow> = {}): CredentialPendingRow {
  return {
    identifiers: ["ANTHROPIC_API_KEY"],
    kind: "agent_secret",
    record_id: "req-1",
    requested_at: "2026-09-27T00:00:00Z",
    ...overrides,
  };
}

function renderSection(credentials: CredentialRequests) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <CredentialRequestsSection credentials={credentials} />
      </QueryClientProvider>
    </MemoryRouter>
  );
}

test("renders every pending credential request, linking a machine row to /credentials/machine", () => {
  const machineRow: CredentialPendingRow = {
    identifiers: ["worker-7.example.com"],
    kind: "launcher_credential",
    record_id: "req-2",
    requested_at: "2026-09-27T01:00:00Z",
  };
  renderSection({ requests: [pendingSecretRow(), machineRow], status: "listed" });

  expect(screen.getByText("Secret request")).toBeDefined();
  expect(screen.getByText("Machine login")).toBeDefined();
  expect(screen.getByText("ANTHROPIC_API_KEY")).toBeDefined();
  expect(screen.getByText("worker-7.example.com")).toBeDefined();
  expect(
    screen.getByRole("link", { name: /Secret request.*ANTHROPIC_API_KEY/s }).getAttribute("href")
  ).toBe("/credentials/req-1");
  expect(
    screen
      .getByRole("link", { name: /Machine login.*worker-7\.example\.com/s })
      .getAttribute("href")
  ).toBe("/credentials/machine");
  expect(screen.getAllByText("No session named.")).toHaveLength(2);
});

test("renders nothing while the list loads or when it lists none, a broker-less Dispatch's answer", () => {
  for (const status of ["loading", "listed"] as const) {
    const view = renderSection({ requests: [], status });
    expect(view.container.firstChild).toBeNull();
    view.unmount();
  }
});

test("surfaces a failure to load the list instead of hiding the section", () => {
  renderSection({ requests: [], status: "failed" });
  expect(screen.getByText("Couldn't load credential requests.")).toBeDefined();
});

// LEGION-587: a row whose session names a live agent links to it; one whose session names an id
// the agents list doesn't carry reads as not running, with neither mistaken for the other.
test("a row's session reads as a running session's title, linked to its live conversation", async () => {
  const agents = spyOn(api, "listAgents").mockResolvedValue([runningAgent()]);
  try {
    renderSection({
      requests: [pendingSecretRow({ session: { enrollment: "sess-1", request: null } })],
      status: "listed",
    });
    const link = await screen.findByRole("link", { name: "Reviewing LEGION-587" });
    expect(link.getAttribute("href")).toBe("/agents/sess-1/live");
    expect(screen.getByText(/devbox-alice/)).toBeDefined();
  } finally {
    agents.mockRestore();
  }
});

// LEGION-587's review (round 3): `generatePath` does not escape its params, so a running
// session's own id - an unsigned claim nothing here can constrain the shape of - must be
// percent-encoded before it reaches the live link, or an id crafted with `/` and `..` segments
// would navigate somewhere other than `/agents/:sessionId/live`.
test("encodes a running session's id before building its live link", async () => {
  const agents = spyOn(api, "listAgents").mockResolvedValue([
    runningAgent({ session_id: "../../settings" }),
  ]);
  try {
    renderSection({
      requests: [pendingSecretRow({ session: { enrollment: "../../settings", request: null } })],
      status: "listed",
    });
    const link = await screen.findByRole("link", { name: "Reviewing LEGION-587" });
    expect(link.getAttribute("href")).toBe("/agents/..%2F..%2Fsettings/live");
  } finally {
    agents.mockRestore();
  }
});

test("a row's session naming an id the agents list doesn't carry reads as not running", async () => {
  const agents = spyOn(api, "listAgents").mockResolvedValue([]);
  try {
    renderSection({
      requests: [pendingSecretRow({ session: { enrollment: "sess-gone", request: null } })],
      status: "listed",
    });
    expect(await screen.findByText(/sess-gone isn't running\./)).toBeDefined();
  } finally {
    agents.mockRestore();
  }
});

// LEGION-587's review (round 2): a request-only session id - the common case for a host Oh My Pi
// session, which has no enrollment session_id of its own - must still say where it came from, so
// a reader never mistakes an unsigned claim for the broker's own verified enrollment fact.
test("a row's request-only session id is labeled, even with no enrollment id to disambiguate against", async () => {
  const agents = spyOn(api, "listAgents").mockResolvedValue([runningAgent()]);
  try {
    renderSection({
      requests: [pendingSecretRow({ session: { enrollment: null, request: "sess-1" } })],
      status: "listed",
    });
    expect(await screen.findByText(/The session the request says it came from:/)).toBeDefined();
    const link = screen.getByRole("link", { name: "Reviewing LEGION-587" });
    expect(link.getAttribute("href")).toBe("/agents/sess-1/live");
  } finally {
    agents.mockRestore();
  }
});

// LEGION-587's review (round 3): the whole pending-requests list must share one `useAgents`
// subscriber to the Agents list (`CredentialRequestsSection`'s own call, now that
// `CredentialSessionLines` takes `agents`/`isError`/`isPending` as props instead of calling the
// hook itself) - not one per row. `getObserversCount()` on the shared `["agents"]` query is the
// direct signal: the old per-row shape left one `useQuery` subscriber per
// `CredentialSessionLines` instance, so it read 3 once three rows had mounted; this shape reads
// 1 no matter how many rows are showing. A second signal catches the same regression from the
// request-volume side: each row mounts more than the query's 10s `staleTime` after the last, so
// a per-row subscriber (mounting with already-stale cached data) would fire its own refetch -
// three rows would then cost three fetches instead of the one the initial mount already made.
test("mounts one agents poll for the whole list, not one per pending row", async () => {
  const listAgents = spyOn(api, "listAgents").mockResolvedValue([runningAgent()]);
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const tree = (requests: CredentialPendingRow[]) => (
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <CredentialRequestsSection credentials={{ requests, status: "listed" }} />
      </QueryClientProvider>
    </MemoryRouter>
  );
  const observerCount = () =>
    client
      .getQueryCache()
      .find({ queryKey: ["agents"] })
      ?.getObserversCount() ?? 0;
  jest.useFakeTimers();
  try {
    const row1 = pendingSecretRow({ session: { enrollment: "sess-1", request: null } });
    const { rerender } = render(tree([row1]));
    await act(async () => {});
    expect(listAgents).toHaveBeenCalledTimes(1);
    expect(observerCount()).toBe(1);

    // A second row mounts 11s later - past the 10s staleTime.
    await act(async () => {
      jest.advanceTimersByTime(11_000);
    });
    const row2 = pendingSecretRow({
      record_id: "req-2",
      session: { enrollment: "sess-2", request: null },
    });
    rerender(tree([row1, row2]));
    await act(async () => {});

    // A third row mounts another 11s later.
    await act(async () => {
      jest.advanceTimersByTime(11_000);
    });
    const row3 = pendingSecretRow({
      record_id: "req-3",
      session: { enrollment: "sess-3", request: null },
    });
    rerender(tree([row1, row2, row3]));
    await act(async () => {});

    // Still the one observer from the first mount, and still its one fetch: adding rows past the
    // staleTime window never starts a second poll of the shared Agents list.
    expect(observerCount()).toBe(1);
    expect(listAgents).toHaveBeenCalledTimes(1);
  } finally {
    jest.useRealTimers();
    listAgents.mockRestore();
  }
});
