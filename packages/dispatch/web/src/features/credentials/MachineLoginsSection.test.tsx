import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type { MachineLogin } from "../../api/types";
import { MachineLoginPage } from "./MachineLoginPage";

const devbox: MachineLogin = {
  credential_id: "cred-devbox",
  expires_at: "2026-10-10T12:00:00Z",
  host: "devbox.example.com",
  issued_at: "2026-10-03T12:00:00Z",
};
const laptop: MachineLogin = {
  credential_id: "cred-laptop",
  expires_at: "2026-10-09T08:00:00Z",
  host: "laptop.example.com",
  issued_at: "2026-10-02T08:00:00Z",
};

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

test("the machine-login page lists the viewer's machine logins, and Revoke ends the one confirmed", async () => {
  let live = [devbox, laptop];
  const getMachineLogins = spyOn(api, "getMachineLogins").mockImplementation(async () => ({
    credentials: live,
  }));
  const revokeMachineLogin = spyOn(api, "revokeMachineLogin").mockImplementation(async (id) => {
    live = live.filter((login) => login.credential_id !== id);
  });
  const originalConfirm = window.confirm;
  const asked: string[] = [];
  let answer = false;
  window.confirm = (message?: string) => {
    asked.push(message ?? "");
    return answer;
  };

  try {
    renderPage();
    const section = within(await screen.findByRole("region", { name: "Your machine logins" }));
    const devboxRow = (await section.findByText("devbox.example.com")).closest("tr");
    expect(devboxRow).not.toBeNull();
    expect(section.getByText("laptop.example.com")).toBeDefined();

    fireEvent.click(within(devboxRow as HTMLElement).getByRole("button", { name: "Revoke" }));
    expect(asked).toEqual([
      "Revoke the machine login for devbox.example.com? Every agent session it started loses its secrets at once, and the machine needs a new login.",
    ]);
    // A mutation calls its function a few microtasks after the click; a timer runs after them all.
    const settled = Promise.withResolvers<void>();
    setTimeout(settled.resolve, 0);
    await settled.promise;
    expect(revokeMachineLogin).not.toHaveBeenCalled();

    answer = true;
    fireEvent.click(within(devboxRow as HTMLElement).getByRole("button", { name: "Revoke" }));
    await waitFor(() => expect(revokeMachineLogin).toHaveBeenCalledWith("cred-devbox"));
    await waitFor(() => expect(section.queryByText("devbox.example.com")).toBeNull());
    expect(section.getByText("laptop.example.com")).toBeDefined();
    expect(revokeMachineLogin).toHaveBeenCalledTimes(1);
  } finally {
    cleanup();
    window.confirm = originalConfirm;
    getMachineLogins.mockRestore();
    revokeMachineLogin.mockRestore();
  }
});

test("a machine login approved on the page joins the list", async () => {
  let live: MachineLogin[] = [];
  const getMachineLogins = spyOn(api, "getMachineLogins").mockImplementation(async () => ({
    credentials: live,
  }));
  const lookupMachineCredential = spyOn(api, "lookupMachineCredential").mockResolvedValue({
    approver: "ada@example.com",
    decided: null,
    enrollment: null,
    expires_at: "2026-10-03T12:15:00Z",
    identifiers: ["devbox.example.com"],
    kind: "launcher_credential",
    lifetime_seconds: 604800,
    reason: "",
    record_id: "req-1",
    requested_at: "2026-10-03T12:00:00Z",
    rules_version: "v1",
    service: null,
    state: "pending",
  });
  const approveCredentialRecord = spyOn(api, "approveCredentialRecord").mockImplementation(
    async () => {
      live = [devbox];
      return { credential_id: "cred-devbox", grant_id: null, state: "approved" };
    }
  );

  try {
    renderPage();
    const section = within(await screen.findByRole("region", { name: "Your machine logins" }));
    expect(await section.findByText("No live machine logins.")).toBeDefined();

    fireEvent.change(screen.getByLabelText("Code shown on the machine"), {
      target: { value: "abcd1234" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Look up" }));
    fireEvent.click(await screen.findByRole("button", { name: "Approve" }));

    await waitFor(() => expect(approveCredentialRecord).toHaveBeenCalledTimes(1));
    expect(await section.findByText("devbox.example.com")).toBeDefined();
  } finally {
    cleanup();
    getMachineLogins.mockRestore();
    lookupMachineCredential.mockRestore();
    approveCredentialRecord.mockRestore();
  }
});
