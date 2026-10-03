import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { api } from "../../api/client";
import type { CredentialRecord } from "../../api/types";
import { CredentialRecordPage } from "./CredentialRecordPage";

function decidedSecretRecord(): CredentialRecord {
  return {
    approver: "sami",
    decided: { at: "2026-09-27T00:05:00Z", credential_id: null, event: "approved" },
    enrollment: { kind: "box", operator: "sami", runtime_id: "box-1", slot: null },
    expires_at: "2026-09-27T12:00:00Z",
    identifiers: ["DEEL_API_KEY"],
    kind: "agent_secret",
    lifetime_seconds: 3600,
    reason: "",
    record_id: "rec-1",
    requested_at: "2026-09-27T00:00:00Z",
    rules_version: "v3",
    service: null,
    state: "approved",
  };
}

// The broker answers a decided agent_secret record with credential_id null, since its approval
// minted no launcher credential, so the page must not print a bare "Credential" line under it.
test("a decided secret record with no credential shows no Credential line", async () => {
  const getRecord = spyOn(api, "getCredentialRecord").mockResolvedValue(decidedSecretRecord());
  try {
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <MemoryRouter initialEntries={["/credentials/rec-1"]}>
        <QueryClientProvider client={queryClient}>
          <Routes>
            <Route element={<CredentialRecordPage />} path="/credentials/:recordId" />
          </Routes>
        </QueryClientProvider>
      </MemoryRouter>
    );
    expect(await screen.findByText(/^approved/i)).toBeDefined();
    expect(screen.queryByText("Credential")).toBeNull();
  } finally {
    cleanup();
    getRecord.mockRestore();
  }
});
