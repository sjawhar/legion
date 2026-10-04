import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type { CredentialGrant, MachineLogin } from "../../api/types";
import { GrantsSection } from "./GrantsSection";
import { MachineLoginPage } from "./MachineLoginPage";
import { MachineLoginsSection } from "./MachineLoginsSection";

const devbox: MachineLogin = {
  credential_id: "cred-devbox",
  expired: false,
  expires_at: "2026-10-10T12:00:00Z",
  host: "devbox.example.com",
  issued_at: "2026-10-03T12:00:00Z",
  service: null,
};
const laptop: MachineLogin = {
  credential_id: "cred-laptop",
  expired: false,
  expires_at: "2026-10-09T08:00:00Z",
  host: "laptop.example.com",
  issued_at: "2026-10-02T08:00:00Z",
  service: null,
};
const daemon: MachineLogin = {
  credential_id: "cred-daemon",
  expired: false,
  expires_at: "2026-10-10T09:00:00Z",
  host: "cluster.example.com",
  issued_at: "2026-10-03T09:00:00Z",
  service: "legion-daemon",
};
/** The devbox's previous login: expired, but a box it started still renews with its own key. */
const stale: MachineLogin = {
  credential_id: "cred-stale",
  expired: true,
  expires_at: "2026-10-03T11:00:00Z",
  host: "devbox.example.com",
  issued_at: "2026-09-26T11:00:00Z",
  service: null,
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

test("an expired login whose sessions still run is listed and marked so, and Revoke ends it", async () => {
  let live = [devbox, stale];
  const getMachineLogins = spyOn(api, "getMachineLogins").mockImplementation(async () => ({
    credentials: live,
  }));
  const revokeMachineLogin = spyOn(api, "revokeMachineLogin").mockImplementation(async (id) => {
    live = live.filter((login) => login.credential_id !== id);
  });
  const confirm = spyOn(window, "confirm").mockReturnValue(true);

  try {
    renderPage();
    const section = within(await screen.findByRole("region", { name: "Your machine logins" }));
    const marked = await section.findByText("expired, sessions still running");
    const staleRow = marked.closest("tr") as HTMLElement;
    expect(within(staleRow).getByText("devbox.example.com")).toBeDefined();
    expect(section.getAllByText("expired, sessions still running")).toHaveLength(1);

    fireEvent.click(within(staleRow).getByRole("button", { name: "Revoke" }));
    await waitFor(() => expect(revokeMachineLogin).toHaveBeenCalledWith("cred-stale"));
    await waitFor(() => expect(section.queryByText("expired, sessions still running")).toBeNull());
    expect(section.getByText("devbox.example.com")).toBeDefined();
  } finally {
    cleanup();
    confirm.mockRestore();
    getMachineLogins.mockRestore();
    revokeMachineLogin.mockRestore();
  }
});

// A machine login's revoke ends every session it enrolled and with them their grants, so a page
// showing Live grants beside the machine logins drops those grants without a reload.
test("revoking a machine login refreshes Live grants", async () => {
  let logins = [devbox];
  const grant: CredentialGrant = {
    approver: null,
    created_at: "2026-10-03T12:05:00Z",
    enrollment: { kind: "box", operator: "ada@example.com", runtime_id: "box-1", slot: null },
    expires_at: "2026-10-03T13:05:00Z",
    grant_id: "grant-1",
    granted: "automatic",
    names: ["WORKER_TOKEN"],
    record_id: null,
  };
  let grants = [grant];
  const getMachineLogins = spyOn(api, "getMachineLogins").mockImplementation(async () => ({
    credentials: logins,
  }));
  const getCredentialGrants = spyOn(api, "getCredentialGrants").mockImplementation(async () => ({
    grants,
  }));
  const revokeMachineLogin = spyOn(api, "revokeMachineLogin").mockImplementation(async () => {
    logins = [];
    grants = [];
  });
  const confirm = spyOn(window, "confirm").mockReturnValue(true);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });

  try {
    render(
      <QueryClientProvider client={queryClient}>
        <MachineLoginsSection />
        <GrantsSection />
      </QueryClientProvider>
    );
    const liveGrants = within(await screen.findByRole("region", { name: "Live grants" }));
    expect(await liveGrants.findByText("WORKER_TOKEN")).toBeDefined();
    const section = within(screen.getByRole("region", { name: "Your machine logins" }));

    fireEvent.click(await section.findByRole("button", { name: "Revoke" }));
    await waitFor(() => expect(revokeMachineLogin).toHaveBeenCalledWith("cred-devbox"));
    expect(await liveGrants.findByText("No live grants.")).toBeDefined();
    expect(liveGrants.queryByText("WORKER_TOKEN")).toBeNull();
  } finally {
    cleanup();
    confirm.mockRestore();
    getMachineLogins.mockRestore();
    getCredentialGrants.mockRestore();
    revokeMachineLogin.mockRestore();
  }
});

test("the machine-login page lists the viewer's machine logins, and Revoke ends the one confirmed", async () => {
  let live = [devbox, laptop];
  const getMachineLogins = spyOn(api, "getMachineLogins").mockImplementation(async () => ({
    credentials: live,
  }));
  const revokeMachineLogin = spyOn(api, "revokeMachineLogin").mockImplementation(async (id) => {
    live = live.filter((login) => login.credential_id !== id);
  });
  const confirm = spyOn(window, "confirm").mockReturnValue(false);

  try {
    renderPage();
    const section = within(await screen.findByRole("region", { name: "Your machine logins" }));
    const devboxRow = (await section.findByText("devbox.example.com")).closest("tr");
    expect(devboxRow).not.toBeNull();
    expect(section.getByText("laptop.example.com")).toBeDefined();

    fireEvent.click(within(devboxRow as HTMLElement).getByRole("button", { name: "Revoke" }));
    expect(confirm.mock.calls).toEqual([
      [
        "Revoke the machine login for devbox.example.com? Every agent session it started loses its secrets at once, and the machine needs a new login.",
      ],
    ]);
    // A mutation calls its function a few microtasks after the click; a timer runs after them all.
    const settled = Promise.withResolvers<void>();
    setTimeout(settled.resolve, 0);
    await settled.promise;
    expect(revokeMachineLogin).not.toHaveBeenCalled();

    confirm.mockReturnValue(true);
    fireEvent.click(within(devboxRow as HTMLElement).getByRole("button", { name: "Revoke" }));
    await waitFor(() => expect(revokeMachineLogin).toHaveBeenCalledWith("cred-devbox"));
    await waitFor(() => expect(section.queryByText("devbox.example.com")).toBeNull());
    expect(section.getByText("laptop.example.com")).toBeDefined();
    expect(revokeMachineLogin).toHaveBeenCalledTimes(1);
  } finally {
    cleanup();
    confirm.mockRestore();
    getMachineLogins.mockRestore();
    revokeMachineLogin.mockRestore();
  }
});

test("a service's login the viewer approved is listed by its service, and its Revoke says its pods end", async () => {
  let live = [daemon, devbox];
  const getMachineLogins = spyOn(api, "getMachineLogins").mockImplementation(async () => ({
    credentials: live,
  }));
  const revokeMachineLogin = spyOn(api, "revokeMachineLogin").mockImplementation(async (id) => {
    live = live.filter((login) => login.credential_id !== id);
  });
  const confirm = spyOn(window, "confirm").mockReturnValue(true);

  try {
    renderPage();
    const section = within(await screen.findByRole("region", { name: "Your machine logins" }));
    const daemonRow = (await section.findByText("legion-daemon on cluster.example.com")).closest(
      "tr"
    );
    expect(daemonRow).not.toBeNull();
    expect(section.getByText("devbox.example.com")).toBeDefined();

    fireEvent.click(within(daemonRow as HTMLElement).getByRole("button", { name: "Revoke" }));
    expect(confirm.mock.calls).toEqual([
      [
        "Revoke the machine login for legion-daemon on cluster.example.com? Every session it started, its worker pods included, ends at once, and legion-daemon needs a new login approval before it starts any more.",
      ],
    ]);
    await waitFor(() => expect(revokeMachineLogin).toHaveBeenCalledWith("cred-daemon"));
    await waitFor(() =>
      expect(section.queryByText("legion-daemon on cluster.example.com")).toBeNull()
    );
    expect(section.getByText("devbox.example.com")).toBeDefined();
  } finally {
    cleanup();
    confirm.mockRestore();
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
