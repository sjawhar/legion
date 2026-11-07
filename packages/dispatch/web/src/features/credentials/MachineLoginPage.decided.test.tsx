import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
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
  const user = userEvent.setup();
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <MemoryRouter initialEntries={["/credentials/machine"]}>
      <QueryClientProvider client={queryClient}>
        <MachineLoginPage />
      </QueryClientProvider>
    </MemoryRouter>
  );
  await user.click(screen.getByLabelText("Code shown on the machine"));
  await user.paste("abcd1234");
  await user.click(screen.getByRole("button", { name: "Look up" }));
  expect(await screen.findByText("worker-7.example.com")).toBeDefined();
}

// Once the broker records the decision the page must say so and stop offering it: the looked-up
// record is still the pending one, so the page showed the same Approve and Deny buttons after a
// successful click, and a second Approve reached the broker's already-decided path.
// The decision's own line, read past the machine logins the page lists below it: that table's
// "Approved by" and "Credential" headers answer a looser match for either word.
for (const [button, word] of [
  ["Approve", /^Approved\./],
  ["Deny", /^denied/i],
] as const) {
  test(`after ${button} succeeds the machine-login page shows the decision and no decision buttons`, async () => {
    const user = userEvent.setup();
    const lookup = spyOn(api, "lookupMachineCredential").mockResolvedValue(machineRecord());
    // The page lists the viewer's machine logins beside the code form; no test here reads them.
    const getMachineLogins = spyOn(api, "getMachineLogins").mockResolvedValue({ credentials: [] });
    const approve = spyOn(api, "approveCredentialRecord").mockResolvedValue({
      credential_id: "cred-9",
      state: "approved",
    });
    const deny = spyOn(api, "denyCredentialRecord").mockResolvedValue({ state: "denied" });
    try {
      await lookUp();
      await user.click(screen.getByRole("button", { name: button }));
      await waitFor(() =>
        expect((button === "Approve" ? approve : deny).mock.calls.length).toBe(1)
      );
      await waitFor(() =>
        expect(screen.queryByRole("button", { name: "Approve" })).not.toBeInTheDocument()
      );
      expect(screen.queryByRole("button", { name: "Deny" })).not.toBeInTheDocument();
      expect(screen.getByText(word)).toBeDefined();
      expect(screen.queryByText(/^Approving lets/)).not.toBeInTheDocument();
    } finally {
      cleanup();
      lookup.mockRestore();
      getMachineLogins.mockRestore();
      approve.mockRestore();
      deny.mockRestore();
    }
  });
}

// A code looked up again after its login was decided shows what was decided, as the page does
// right after the click, and no longer says what approving would do.
for (const [decided, word] of [
  [{ at: "2026-09-27T00:05:00Z", credential_id: "cred-9", event: "approved" }, /^Approved\./],
  [{ at: "2026-09-27T00:05:00Z", credential_id: null, event: "denied" }, /^denied/i],
] as const) {
  test(`a looked-up login already ${decided.event} shows its decision and no decision buttons`, async () => {
    const lookup = spyOn(api, "lookupMachineCredential").mockResolvedValue(
      machineRecord({ ...decided })
    );
    const getMachineLogins = spyOn(api, "getMachineLogins").mockResolvedValue({ credentials: [] });
    try {
      await lookUp();
      expect(screen.getByText(word)).toBeDefined();
      expect(screen.queryByText(/^Approving lets/)).not.toBeInTheDocument();
      expect(screen.queryByRole("button", { name: "Approve" })).not.toBeInTheDocument();
      expect(screen.queryByRole("button", { name: "Deny" })).not.toBeInTheDocument();
      if (decided.credential_id === null) {
        expect(screen.queryByText(/^Credential /)).not.toBeInTheDocument();
      } else {
        expect(screen.getByText(`Credential ${decided.credential_id}`)).toBeDefined();
      }
    } finally {
      cleanup();
      lookup.mockRestore();
      getMachineLogins.mockRestore();
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
  const getMachineLogins = spyOn(api, "getMachineLogins").mockResolvedValue({ credentials: [] });
  try {
    await lookUp();
    expect(
      await screen.findByText(
        "Approved. legion-daemon on worker-7.example.com can start worker pods as legion-daemon."
      )
    ).toBeDefined();
    expect(screen.queryByText(/can start agent sessions as you/)).not.toBeInTheDocument();
  } finally {
    cleanup();
    lookup.mockRestore();
    getMachineLogins.mockRestore();
  }
});
