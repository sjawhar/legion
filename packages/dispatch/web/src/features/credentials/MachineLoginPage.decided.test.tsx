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

// Once the broker records a denial the page must say so and stop offering it: the looked-up
// record is still the pending one, so the page showed the same Approve and Deny buttons after a
// successful click, and a second click reached the broker's already-decided path.
test("after Deny succeeds the machine-login page shows the decision and no decision buttons", async () => {
  const lookup = spyOn(api, "lookupMachineCredential").mockResolvedValue(machineRecord());
  const deny = spyOn(api, "denyCredentialRecord").mockResolvedValue({ state: "denied" });
  try {
    await lookUp();
    fireEvent.click(screen.getByRole("button", { name: "Deny" }));
    await waitFor(() => expect(deny.mock.calls.length).toBe(1));
    await waitFor(() => expect(screen.queryByRole("button", { name: "Approve" })).toBeNull());
    expect(screen.queryByRole("button", { name: "Deny" })).toBeNull();
    expect(screen.getByText(/denied/i)).toBeDefined();
    expect(screen.queryByText(/^Approving lets/)).toBeNull();
  } finally {
    cleanup();
    lookup.mockRestore();
    deny.mockRestore();
  }
});

// On a broker that takes the approver's own sign-in, Approve decides nothing here, so the page
// shows no decision: it hands the browser to the broker's own page, where the approver signs in
// and the broker mints the credential.
test("Approve sends the browser to the broker's page and records no decision here", async () => {
  const lookup = spyOn(api, "lookupMachineCredential").mockResolvedValue(machineRecord());
  const approval = spyOn(api, "approveCredentialRecord").mockResolvedValue({
    expires_at: "2026-09-27T00:10:00Z",
    url: "https://broker.example/approve/challenge-3",
  });
  const assign = spyOn(window.location, "assign").mockImplementation(() => {});
  try {
    await lookUp();
    fireEvent.click(screen.getByRole("button", { name: "Approve" }));
    await waitFor(() =>
      expect(assign).toHaveBeenCalledWith("https://broker.example/approve/challenge-3")
    );
    expect(screen.queryByText(/^approved/i)).toBeNull();
    expect(screen.getByRole("button", { name: "Approve" })).toBeDefined();
  } finally {
    cleanup();
    lookup.mockRestore();
    approval.mockRestore();
    assign.mockRestore();
  }
});

// On a broker whose BROKER_APPROVAL_* group is unset, Approve answers the decision itself: the
// page shows it, with the credential it minted, in place of the buttons, and goes nowhere.
test("on a broker that takes no sign-in, Approve shows its decision and no decision buttons", async () => {
  const lookup = spyOn(api, "lookupMachineCredential").mockResolvedValue(machineRecord());
  const approval = spyOn(api, "approveCredentialRecord").mockResolvedValue({
    credential_id: "cred-9",
    grant_id: null,
    state: "approved",
  });
  const assign = spyOn(window.location, "assign").mockImplementation(() => {});
  try {
    await lookUp();
    fireEvent.click(screen.getByRole("button", { name: "Approve" }));
    await waitFor(() => expect(screen.queryByRole("button", { name: "Approve" })).toBeNull());
    expect(screen.queryByRole("button", { name: "Deny" })).toBeNull();
    expect(screen.getByText(/approved/i)).toBeDefined();
    expect(screen.getByText("Credential cred-9")).toBeDefined();
    expect(assign).not.toHaveBeenCalled();
  } finally {
    cleanup();
    lookup.mockRestore();
    approval.mockRestore();
    assign.mockRestore();
  }
});

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

// A service's login (the Legion daemon's) an older broker opened and a person approved starts
// worker pods as the service, so a lookup of it never says the machine acts as the person who
// approved it.
test("a looked-up approved service's login says it starts worker pods as the service, not as you", async () => {
  const lookup = spyOn(api, "lookupMachineCredential").mockResolvedValue({
    ...machineRecord({ at: "2026-09-27T00:05:00Z", credential_id: "cred-9", event: "approved" }),
    service: "legion-daemon",
  });
  try {
    await lookUp();
    expect(
      await screen.findByText(
        "Approved. legion-daemon on worker-7.example.com can start worker pods as legion-daemon."
      )
    ).toBeDefined();
    expect(screen.queryByText(/can start agent sessions as you/)).toBeNull();
  } finally {
    cleanup();
    lookup.mockRestore();
  }
});
