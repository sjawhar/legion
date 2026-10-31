import { afterEach, expect, jest, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, render, screen, waitFor } from "@testing-library/react";
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
// the agents list doesn't carry reads as not running, with neither mistaken for the other. Every
// line carries the full id: a request's id is an unsigned claim, and a short form is shared by
// any id with the same first eight characters.
test("a row's session reads as a running session's title, linked to its live conversation", async () => {
  const agents = spyOn(api, "listAgents").mockResolvedValue([runningAgent()]);
  try {
    renderSection({
      requests: [pendingSecretRow({ session: { enrollment: "sess-1", request: null } })],
      status: "listed",
    });
    const link = await screen.findByRole("link", { name: "Reviewing LEGION-587" });
    expect(link.getAttribute("href")).toBe("/agents/sess-1/live");
    await waitFor(() =>
      expect(link.closest("[data-agent-name]")?.getAttribute("data-agent-state")).toBe("running")
    );
    expect(screen.getByRole("img", { name: "Running" })).toBeDefined();
    expect(link.closest("[data-agent-name]")?.parentElement?.textContent).toBe(
      "Reviewing LEGION-587 · sess-1 · /home/alice/legion · devbox-alice"
    );
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

// An id the agents list does not carry is the agent's not-running state: its short id, marked
// not running, still linked, then its full id and "isn't running" in words, with no directory or
// machine.
test("a row's session naming an id the agents list doesn't carry reads as not running", async () => {
  const agents = spyOn(api, "listAgents").mockResolvedValue([]);
  try {
    renderSection({
      requests: [pendingSecretRow({ session: { enrollment: "sess-gone", request: null } })],
      status: "listed",
    });
    expect(await screen.findByRole("img", { name: "Not running" })).toBeDefined();
    const name = screen.getByRole("link", { name: "session:sess-gon…" });
    expect(name.getAttribute("href")).toBe("/agents/sess-gone/live");
    const agent = name.closest("[data-agent-name]");
    expect(agent?.getAttribute("data-agent-state")).toBe("not-running");
    expect(agent?.parentElement?.textContent).toBe("session:sess-gon… · sess-gone isn't running.");
  } finally {
    agents.mockRestore();
  }
});

// Envoy unavailable or not configured: the agents list never answers, so an id is the agent's
// unknown state, never not running, and the line says in words, with the full id, that it
// couldn't check.
test("a row's session reads as unknown while the agents list cannot be read", async () => {
  const agents = spyOn(api, "listAgents").mockRejectedValue(new Error("envoy unavailable"));
  try {
    renderSection({
      requests: [pendingSecretRow({ session: { enrollment: "sess-gone", request: null } })],
      status: "listed",
    });
    await waitFor(() => expect(agents).toHaveBeenCalled());
    expect(await screen.findByRole("img", { name: "Running state unknown" })).toBeDefined();
    expect(
      screen.getByRole("link", { name: "session:sess-gon…" }).closest("[data-agent-name]")
        ?.parentElement?.textContent
    ).toBe("session:sess-gon… · couldn't check whether sess-gone is running.");
    expect(screen.queryByRole("img", { name: "Not running" })).toBeNull();
    expect(screen.queryByText(/isn't running/)).toBeNull();
  } finally {
    agents.mockRestore();
  }
});

// A request-only session id - the common case for a host Oh My Pi session, which has no enrollment
// session_id of its own - is shown as written, with no preamble.
test("a row's request-only session id is shown with no preamble", async () => {
  const agents = spyOn(api, "listAgents").mockResolvedValue([runningAgent()]);
  try {
    renderSection({
      requests: [pendingSecretRow({ session: { enrollment: null, request: "sess-1" } })],
      status: "listed",
    });
    const link = await screen.findByRole("link", { name: "Reviewing LEGION-587" });
    expect(link.getAttribute("href")).toBe("/agents/sess-1/live");
    expect(
      link
        .closest("[data-agent-name]")
        ?.parentElement?.textContent?.startsWith("Reviewing LEGION-587")
    ).toBe(true);
    expect(screen.queryByText(/says it came from/)).toBeNull();
  } finally {
    agents.mockRestore();
  }
});

// LEGION-587's review (round 3): the whole pending-requests list reads the Agents list once,
// never once per row. Each row mounts more than the query's 10s `staleTime` after the last, so a
// per-row reader that refetched on mount (with already-stale cached data) would fire its own
// request - three rows would then cost three fetches instead of the one the initial mount
// already made. Refreshing it is the shell's one poller's job (`AgentRegistryPoller`).
test("reads the agents list once for the whole list, not once per pending row", async () => {
  const listAgents = spyOn(api, "listAgents").mockResolvedValue([runningAgent()]);
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const tree = (requests: CredentialPendingRow[]) => (
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <CredentialRequestsSection credentials={{ requests, status: "listed" }} />
      </QueryClientProvider>
    </MemoryRouter>
  );
  jest.useFakeTimers();
  try {
    const row1 = pendingSecretRow({ session: { enrollment: "sess-1", request: null } });
    const { rerender } = render(tree([row1]));
    await act(async () => {});
    expect(listAgents).toHaveBeenCalledTimes(1);

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

    // Still the one fetch from the first mount: adding rows past the staleTime window never
    // starts a second read of the shared Agents list.
    expect(listAgents).toHaveBeenCalledTimes(1);
  } finally {
    jest.useRealTimers();
    listAgents.mockRestore();
  }
});
