import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { ApiError, api } from "../../api/client";
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

test("hides the Live grants section when the broker answers 404 FEATURE_OFF", async () => {
  const unrelated = stubUnrelatedQueries();
  const getCredentialPending = spyOn(api, "getCredentialPending").mockRejectedValue(
    new ApiError(404, { code: "FEATURE_OFF" })
  );

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
        created_at: "2026-09-30T00:00:00Z",
        enrollment: { kind: "box", operator: "sami", runtime_id: "box-1" },
        expires_at: "2026-09-30T01:00:00Z",
        grant_id: "grant-1",
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
