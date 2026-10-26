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

// The typed code goes to the broker with the approval it opens: the broker checks it against the
// record there and then, and the browser goes to the broker's page for the sign-in that approves.
test("code -> lookup -> Approve opens an approval with the looked-up code and goes to the broker", async () => {
  const record = machineRecord();
  const lookupMachineCredential = spyOn(api, "lookupMachineCredential").mockResolvedValue(record);
  const approveCredentialRecord = spyOn(api, "approveCredentialRecord").mockResolvedValue({
    expires_at: "2026-09-27T00:10:00Z",
    url: "https://broker.example/approve/challenge-2",
  });
  const assign = spyOn(window.location, "assign").mockImplementation(() => {});

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
    await waitFor(() =>
      expect(assign).toHaveBeenCalledWith("https://broker.example/approve/challenge-2")
    );
  } finally {
    cleanup();
    lookupMachineCredential.mockRestore();
    approveCredentialRecord.mockRestore();
    assign.mockRestore();
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

test("a pending service login from before token logins offers Deny alone, which the broker allows", async () => {
  const record = machineRecord({ approver: "anyone", service: "legion-daemon" });
  const lookupMachineCredential = spyOn(api, "lookupMachineCredential").mockResolvedValue(record);
  const denyCredentialRecord = spyOn(api, "denyCredentialRecord").mockResolvedValue({
    state: "denied",
  });

  try {
    renderPage();

    fireEvent.change(screen.getByLabelText("Code shown on the machine"), {
      target: { value: "abcd1234" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Look up" }));

    await waitFor(() => expect(lookupMachineCredential).toHaveBeenCalledWith("ABCD-1234"));
    expect(await screen.findByRole("button", { name: "Deny" })).toBeDefined();
    expect(screen.queryByRole("button", { name: "Approve" })).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "Deny" }));
    await waitFor(() =>
      expect(denyCredentialRecord).toHaveBeenCalledWith("req-2", { code: "ABCD-1234" })
    );
  } finally {
    cleanup();
    lookupMachineCredential.mockRestore();
    denyCredentialRecord.mockRestore();
  }
});
