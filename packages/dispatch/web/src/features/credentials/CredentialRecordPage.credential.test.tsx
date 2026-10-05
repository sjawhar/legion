import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { api } from "../../api/client";
import type { Agent, CredentialRecord } from "../../api/types";
import { CredentialRecordPage } from "./CredentialRecordPage";

function renderRecordPage(record: CredentialRecord) {
  const getRecord = spyOn(api, "getCredentialRecord").mockResolvedValue(record);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <MemoryRouter initialEntries={[`/credentials/${record.record_id}`]}>
      <QueryClientProvider client={queryClient}>
        <Routes>
          <Route element={<CredentialRecordPage />} path="/credentials/:recordId" />
        </Routes>
      </QueryClientProvider>
    </MemoryRouter>
  );
  return { getRecord };
}

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

// LEGION-587: a record's own session, resolved the same way the Inbox row's is.
test("a record with no session set reads as no session named", async () => {
  const { getRecord } = renderRecordPage(decidedSecretRecord());
  try {
    expect(await screen.findByText("No session named.")).toBeDefined();
  } finally {
    cleanup();
    getRecord.mockRestore();
  }
});

test("a record whose two session ids differ shows both, naming where each came from", async () => {
  const running: Agent = {
    capabilities: [],
    dir: "/home/sami/legion",
    last_activity: null,
    last_seen: 1,
    machine_id: "devbox-sami",
    open_asks: 0,
    roles: [],
    session_id: "sess-enrollment",
    title: "Reviewing LEGION-587",
  };
  const agents = spyOn(api, "listAgents").mockResolvedValue([running]);
  const { getRecord } = renderRecordPage({
    ...decidedSecretRecord(),
    session: { enrollment: "sess-enrollment", request: "sess-request" },
  });
  try {
    const link = await screen.findByRole("link", { name: "Reviewing LEGION-587" });
    expect(link.getAttribute("href")).toBe("/agents/sess-enrollment/live");
    expect(screen.getByText(/The session that enrolled:/)).toBeDefined();
    expect(
      screen.getByText(/The session the request names: sess-request isn't running\./)
    ).toBeDefined();
  } finally {
    cleanup();
    getRecord.mockRestore();
    agents.mockRestore();
  }
});
