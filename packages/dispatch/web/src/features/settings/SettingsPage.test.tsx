import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
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

/** Every query `SettingsPage` and its child sections issue besides `getCredentialPending`,
 *  resolved with empty fixtures so the page settles past its own loading states. */
function stubUnrelatedQueries() {
  return {
    listAgentTokens: spyOn(api, "listAgentTokens").mockResolvedValue([]),
    listArchitectureSources: spyOn(api, "listArchitectureSources").mockResolvedValue([]),
    listProjects: spyOn(api, "listProjects").mockResolvedValue([]),
    listRepoProjects: spyOn(api, "listRepoProjects").mockResolvedValue([]),
  };
}

test("hides the Approver keys section when the broker answers 404 FEATURE_OFF", async () => {
  const unrelated = stubUnrelatedQueries();
  const getCredentialPending = spyOn(api, "getCredentialPending").mockRejectedValue(
    new ApiError(404, { code: "FEATURE_OFF" })
  );

  try {
    renderPage();

    await waitFor(() => expect(getCredentialPending).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(screen.queryByText("Approver keys")).toBeNull());
  } finally {
    cleanup();
    getCredentialPending.mockRestore();
    for (const spy of Object.values(unrelated)) {
      spy.mockRestore();
    }
  }
});

test("shows the Approver keys section once credential requests are configured", async () => {
  const unrelated = stubUnrelatedQueries();
  const getCredentialPending = spyOn(api, "getCredentialPending").mockResolvedValue({
    pending: [],
  });

  try {
    renderPage();

    expect(await screen.findByText("Approver keys")).toBeDefined();
  } finally {
    cleanup();
    getCredentialPending.mockRestore();
    for (const spy of Object.values(unrelated)) {
      spy.mockRestore();
    }
  }
});
