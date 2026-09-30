import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { ApiError, api } from "../../api/client";
import type { CredentialRecord } from "../../api/types";
import { MachineLoginPage } from "./MachineLoginPage";

function machineRecord(overrides: Partial<CredentialRecord> = {}): CredentialRecord {
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
    ...overrides,
  };
}

function renderPage() {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <MemoryRouter initialEntries={["/credentials/machine"]}>
      <QueryClientProvider client={queryClient}>
        <MachineLoginPage />
      </QueryClientProvider>
    </MemoryRouter>
  );
}

test("code -> lookup -> Approve posts the looked-up code, with no key ceremony", async () => {
  const record = machineRecord();
  const lookupMachineCredential = spyOn(api, "lookupMachineCredential").mockResolvedValue(record);
  const approveCredentialRecord = spyOn(api, "approveCredentialRecord").mockResolvedValue({
    credential_id: "cred-9",
    grant_id: null,
    state: "approved",
  });

  try {
    renderPage();

    fireEvent.change(screen.getByLabelText("Code shown on the machine"), {
      target: { value: "abcd1234" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Look up" }));

    await waitFor(() => expect(lookupMachineCredential).toHaveBeenCalledWith("ABCD-1234"));
    expect(await screen.findByText("worker-7.example.com")).toBeDefined();

    fireEvent.click(screen.getByRole("button", { name: "Approve" }));

    await waitFor(() => expect(approveCredentialRecord).toHaveBeenCalledTimes(1));
    expect(approveCredentialRecord).toHaveBeenCalledWith("req-2", { code: "ABCD-1234" });
  } finally {
    cleanup();
    lookupMachineCredential.mockRestore();
    approveCredentialRecord.mockRestore();
  }
});

test("a NO_SUCH_CODE lookup failure renders inline under the input", async () => {
  const lookupMachineCredential = spyOn(api, "lookupMachineCredential").mockRejectedValue(
    new ApiError(404, { code: "NO_SUCH_CODE", error: "No pending request matches that code." })
  );

  try {
    renderPage();

    fireEvent.change(screen.getByLabelText("Code shown on the machine"), {
      target: { value: "zzzz9999" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Look up" }));

    expect(await screen.findByText("No pending request matches that code.")).toBeDefined();
    expect(screen.queryByText("worker-7.example.com")).toBeNull();
  } finally {
    cleanup();
    lookupMachineCredential.mockRestore();
  }
});

test("code -> lookup -> Deny posts the looked-up code too, which the broker requires", async () => {
  const record = machineRecord();
  const lookupMachineCredential = spyOn(api, "lookupMachineCredential").mockResolvedValue(record);
  const denyCredentialRecord = spyOn(api, "denyCredentialRecord").mockResolvedValue({
    credential_id: null,
    grant_id: null,
    state: "denied",
  });

  try {
    renderPage();

    fireEvent.change(screen.getByLabelText("Code shown on the machine"), {
      target: { value: "abcd1234" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Look up" }));

    await waitFor(() => expect(lookupMachineCredential).toHaveBeenCalledWith("ABCD-1234"));
    expect(await screen.findByText("worker-7.example.com")).toBeDefined();

    fireEvent.click(screen.getByRole("button", { name: "Deny" }));

    await waitFor(() => expect(denyCredentialRecord).toHaveBeenCalledTimes(1));
    expect(denyCredentialRecord).toHaveBeenCalledWith("req-2", { code: "ABCD-1234" });
  } finally {
    cleanup();
    lookupMachineCredential.mockRestore();
    denyCredentialRecord.mockRestore();
  }
});
