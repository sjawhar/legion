import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import { SettingsPage } from "./SettingsPage";

function renderPage() {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <SettingsPage />
      </QueryClientProvider>
    </MemoryRouter>
  );
}

/** Every query `SettingsPage` and its child sections issue besides the credential ones,
 *  resolved with empty fixtures so the page settles past its own loading states. */
function stubUnrelatedQueries() {
  return {
    listAgentTokens: spyOn(api, "listAgentTokens").mockResolvedValue([]),
    listArchitectureSources: spyOn(api, "listArchitectureSources").mockResolvedValue([]),
    listProjects: spyOn(api, "listProjects").mockResolvedValue([]),
    listRepoProjects: spyOn(api, "listRepoProjects").mockResolvedValue([]),
  };
}

test("hides the Live grants section on a Dispatch with no secrets broker", async () => {
  const unrelated = stubUnrelatedQueries();
  const getCredentialPending = spyOn(api, "getCredentialPending").mockResolvedValue(null);

  try {
    renderPage();

    await waitFor(() => expect(getCredentialPending).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(screen.queryByText("Live grants")).toBeNull());
  } finally {
    cleanup();
    getCredentialPending.mockRestore();
    for (const spy of Object.values(unrelated)) {
      spy.mockRestore();
    }
  }
});

test("lists live grants once credential requests are configured, and Revoke is one plain POST", async () => {
  const unrelated = stubUnrelatedQueries();
  const getCredentialPending = spyOn(api, "getCredentialPending").mockResolvedValue({
    pending: [],
  });
  const getCredentialGrants = spyOn(api, "getCredentialGrants").mockResolvedValue({
    grants: [
      {
        approver: "sami@example.com",
        created_at: "2026-09-30T00:00:00Z",
        enrollment: { kind: "box", operator: "sami@example.com", runtime_id: "box-1", slot: null },
        expires_at: "2026-09-30T01:00:00Z",
        grant_id: "grant-1",
        granted: "approval",
        names: ["DEEL_API_KEY"],
        record_id: "rec-1",
      },
    ],
  });
  const revokeCredentialGrant = spyOn(api, "revokeCredentialGrant").mockResolvedValue(undefined);

  try {
    renderPage();

    expect(await screen.findByText("Live grants")).toBeDefined();
    expect(await screen.findByText("DEEL_API_KEY")).toBeDefined();
    fireEvent.click(screen.getByRole("button", { name: "Revoke" }));
    await waitFor(() => expect(revokeCredentialGrant).toHaveBeenCalledTimes(1));
    expect(revokeCredentialGrant).toHaveBeenCalledWith("grant-1");
  } finally {
    cleanup();
    getCredentialPending.mockRestore();
    getCredentialGrants.mockRestore();
    revokeCredentialGrant.mockRestore();
    for (const spy of Object.values(unrelated)) {
      spy.mockRestore();
    }
  }
});

// The list holds every grant of the viewer's sessions, so each row says how it was granted: by the
// policy without asking, or on the approval of a login it names, which can be another person's.
test("names how each grant was granted: automatically, or approved by whom", async () => {
  const unrelated = stubUnrelatedQueries();
  const getCredentialPending = spyOn(api, "getCredentialPending").mockResolvedValue({
    pending: [],
  });
  const enrollment = {
    kind: "host",
    operator: "sami@example.com",
    runtime_id: "example-host-devbox:4242:1",
    slot: null,
  };
  const getCredentialGrants = spyOn(api, "getCredentialGrants").mockResolvedValue({
    grants: [
      {
        approver: "mallory@example.com",
        created_at: "2026-09-30T00:00:00Z",
        enrollment,
        expires_at: "2026-09-30T01:00:00Z",
        grant_id: "grant-1",
        granted: "approval",
        names: ["DEEL_API_KEY"],
        record_id: "rec-1",
      },
      {
        approver: null,
        created_at: "2026-09-30T00:00:00Z",
        enrollment,
        expires_at: "2026-09-30T01:00:00Z",
        grant_id: "grant-2",
        granted: "automatic",
        names: ["GWS_READ_TOKEN"],
        record_id: null,
      },
    ],
  });

  try {
    renderPage();

    expect(await screen.findByRole("columnheader", { name: "Granted" })).toBeDefined();
    expect(screen.getByRole("cell", { name: "Approved by mallory@example.com" })).toBeDefined();
    expect(screen.getByRole("cell", { name: "Automatically" })).toBeDefined();
    expect(screen.getAllByRole("button", { name: "Revoke" })).toHaveLength(2);
  } finally {
    cleanup();
    getCredentialPending.mockRestore();
    getCredentialGrants.mockRestore();
    for (const spy of Object.values(unrelated)) {
      spy.mockRestore();
    }
  }
});

// Two roles of one pod hold grants on one runtime id, so each row names its enrollment's slot; a
// grant on an enrollment with no slot names none.
test("two grants on one pod read apart by their slots", async () => {
  const unrelated = stubUnrelatedQueries();
  const getCredentialPending = spyOn(api, "getCredentialPending").mockResolvedValue({
    pending: [],
  });
  const grant = (grantId: string, slot: string | null) => ({
    approver: "sami@example.com",
    created_at: "2026-09-30T00:00:00Z",
    enrollment: { kind: "pod", operator: "", runtime_id: "3f9c-pod-uid", slot },
    expires_at: "2026-09-30T01:00:00Z",
    grant_id: grantId,
    granted: "approval" as const,
    names: ["DEEL_API_KEY"],
    record_id: `rec-${grantId}`,
  });
  const getCredentialGrants = spyOn(api, "getCredentialGrants").mockResolvedValue({
    grants: [
      grant("grant-1", "implementer-g1"),
      grant("grant-2", "reviewer-g1"),
      grant("grant-3", null),
    ],
  });

  try {
    renderPage();

    expect(await screen.findByRole("cell", { name: /slot implementer-g1/ })).toBeDefined();
    expect(screen.getByRole("cell", { name: /slot reviewer-g1/ })).toBeDefined();
    const unslotted = screen.getAllByRole("cell", { name: "pod · 3f9c-pod-uid · —" });
    expect(unslotted).toHaveLength(1);
  } finally {
    cleanup();
    getCredentialPending.mockRestore();
    getCredentialGrants.mockRestore();
    for (const spy of Object.values(unrelated)) {
      spy.mockRestore();
    }
  }
});
