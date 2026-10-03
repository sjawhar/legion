import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { ApiError, api } from "../../api/client";
import type { CredentialPendingRow } from "../../api/types";
import { CredentialRequestsSection } from "./CredentialRequestsSection";

function pendingSecretRow(overrides: Partial<CredentialPendingRow> = {}): CredentialPendingRow {
  return {
    identifiers: ["ANTHROPIC_API_KEY"],
    kind: "agent_secret",
    record_id: "req-1",
    requested_at: "2026-09-27T00:00:00Z",
    ...overrides,
  };
}

function renderSection(queryClient: QueryClient) {
  return render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <CredentialRequestsSection />
      </QueryClientProvider>
    </MemoryRouter>
  );
}

test("renders every pending credential request, linking a machine row to /credentials/machine", async () => {
  const secretRow = pendingSecretRow();
  const machineRow: CredentialPendingRow = {
    identifiers: ["worker-7.example.com"],
    kind: "launcher_credential",
    record_id: "req-2",
    requested_at: "2026-09-27T01:00:00Z",
  };
  const getCredentialPending = spyOn(api, "getCredentialPending").mockResolvedValue({
    pending: [secretRow, machineRow],
  });
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });

  try {
    renderSection(queryClient);

    expect(await screen.findByText("Secret request")).toBeDefined();
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
  } finally {
    cleanup();
    queryClient.clear();
    getCredentialPending.mockRestore();
  }
});

test("hides silently on a Dispatch with no secrets broker", async () => {
  const getCredentialPending = spyOn(api, "getCredentialPending").mockResolvedValue(null);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });

  try {
    const view = renderSection(queryClient);

    await waitFor(() => expect(getCredentialPending).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(view.container.firstChild).toBeNull());
  } finally {
    cleanup();
    queryClient.clear();
    getCredentialPending.mockRestore();
  }
});

test("surfaces a failure to load the list instead of hiding the section", async () => {
  const getCredentialPending = spyOn(api, "getCredentialPending").mockRejectedValue(
    new ApiError(500, { error: "broker unreachable" })
  );
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });

  try {
    renderSection(queryClient);

    expect(await screen.findByText("Couldn't load credential requests.")).toBeDefined();
  } finally {
    cleanup();
    queryClient.clear();
    getCredentialPending.mockRestore();
  }
});
