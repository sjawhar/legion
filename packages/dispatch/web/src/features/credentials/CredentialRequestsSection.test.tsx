import { afterEach, expect, test } from "bun:test";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import type { CredentialPendingRow } from "../../api/types";
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
  return render(
    <MemoryRouter>
      <CredentialRequestsSection credentials={credentials} />
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
