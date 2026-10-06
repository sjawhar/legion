import { afterEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen } from "@testing-library/react";
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
