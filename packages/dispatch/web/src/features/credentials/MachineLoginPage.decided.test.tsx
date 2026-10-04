import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type { CredentialRecord } from "../../api/types";
import { MachineLoginPage } from "./MachineLoginPage";

function machineRecord(decided: CredentialRecord["decided"] = null): CredentialRecord {
  return {
    approver: "sami",
    decided,
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
    state: decided === null ? "pending" : (decided.event as CredentialRecord["state"]),
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
      expect(screen.queryByText(/^Approving lets/)).toBeNull();
    } finally {
      cleanup();
      lookup.mockRestore();
      approve.mockRestore();
      deny.mockRestore();
    }
  });
}

// A code looked up again after its login was decided shows what was decided, as the page does
// right after the click, and no longer says what approving would do.
for (const [decided, word] of [
  [{ at: "2026-09-27T00:05:00Z", credential_id: "cred-9", event: "approved" }, /^approved/i],
  [{ at: "2026-09-27T00:05:00Z", credential_id: null, event: "denied" }, /^denied/i],
] as const) {
  test(`a looked-up login already ${decided.event} shows its decision and no decision buttons`, async () => {
    const lookup = spyOn(api, "lookupMachineCredential").mockResolvedValue(
      machineRecord({ ...decided })
    );
    try {
      await lookUp();
      expect(screen.getByText(word)).toBeDefined();
      expect(screen.queryByText(/^Approving lets/)).toBeNull();
      expect(screen.queryByRole("button", { name: "Approve" })).toBeNull();
      expect(screen.queryByRole("button", { name: "Deny" })).toBeNull();
      if (decided.credential_id === null) {
        expect(screen.queryByText(/^Credential/)).toBeNull();
      } else {
        expect(screen.getByText(`Credential ${decided.credential_id}`)).toBeDefined();
      }
    } finally {
      cleanup();
      lookup.mockRestore();
    }
  });
}

// A service's login (the Legion daemon's) starts worker pods as the service, so its approval never
// says the machine acts as the person who approved it.
test("approving a service's login says it starts worker pods as the service, not as you", async () => {
  const lookup = spyOn(api, "lookupMachineCredential").mockResolvedValue({
    ...machineRecord(),
    service: "legion-daemon",
  });
  const approve = spyOn(api, "approveCredentialRecord").mockResolvedValue({
    credential_id: "cred-9",
    grant_id: null,
    state: "approved",
  });
  try {
    await lookUp();
    fireEvent.click(screen.getByRole("button", { name: "Approve" }));
    expect(
      await screen.findByText(
        "Approved. legion-daemon on worker-7.example.com can start worker pods as legion-daemon."
      )
    ).toBeDefined();
    expect(screen.queryByText(/can start agent sessions as you/)).toBeNull();
  } finally {
    cleanup();
    lookup.mockRestore();
    approve.mockRestore();
  }
});
