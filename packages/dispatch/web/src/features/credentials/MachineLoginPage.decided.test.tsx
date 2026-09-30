import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type { CredentialRecord } from "../../api/types";
import { MachineLoginPage } from "./MachineLoginPage";

function machineRecord(): CredentialRecord {
  return {
    approver: "sami",
    decided: null,
    enrollment: null,
    expires_at: "2026-09-27T01:00:00Z",
    identifiers: ["worker-7.example.com"],
    kind: "launcher_credential",
    lifetime_seconds: 900,
    reason: "",
    record_id: "req-2",
    requested_at: "2026-09-27T00:00:00Z",
    rules_version: "v3",
    service: null,
    state: "pending",
  };
}

async function lookUp() {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <MemoryRouter initialEntries={["/credentials/machine"]}>
      <QueryClientProvider client={queryClient}>
        <MachineLoginPage />
      </QueryClientProvider>
    </MemoryRouter>
  );
  fireEvent.change(screen.getByLabelText("Code shown on the machine"), {
    target: { value: "abcd1234" },
  });
  fireEvent.click(screen.getByRole("button", { name: "Look up" }));
  expect(await screen.findByText("worker-7.example.com")).toBeDefined();
}

// Once the broker records the decision the page must say so and stop offering it: the looked-up
// record is still the pending one, so the page showed the same Approve and Deny buttons after a
// successful click, and a second Approve reached the broker's already-decided path.
for (const [button, word] of [
  ["Approve", /approved/i],
  ["Deny", /denied/i],
] as const) {
  test(`after ${button} succeeds the machine-login page shows the decision and no decision buttons`, async () => {
    const lookup = spyOn(api, "lookupMachineCredential").mockResolvedValue(machineRecord());
    const approve = spyOn(api, "approveCredentialRecord").mockResolvedValue({
      credential_id: "cred-9",
      grant_id: null,
      state: "approved",
    });
    const deny = spyOn(api, "denyCredentialRecord").mockResolvedValue({ state: "denied" });
    try {
      await lookUp();
      fireEvent.click(screen.getByRole("button", { name: button }));
      await waitFor(() =>
        expect((button === "Approve" ? approve : deny).mock.calls.length).toBe(1)
      );
      await waitFor(() => expect(screen.queryByRole("button", { name: "Approve" })).toBeNull());
      expect(screen.queryByRole("button", { name: "Deny" })).toBeNull();
      expect(screen.getByText(word)).toBeDefined();
    } finally {
      cleanup();
      lookup.mockRestore();
      approve.mockRestore();
      deny.mockRestore();
    }
  });
}
