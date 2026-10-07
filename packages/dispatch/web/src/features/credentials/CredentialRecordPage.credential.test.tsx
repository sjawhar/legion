import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { api } from "../../api/client";
import type { CredentialRecord } from "../../api/types";
import { runningAgent } from "./agent-fixture";
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
  const { getRecord } = renderRecordPage(decidedSecretRecord());
  try {
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

test("a record whose two session ids differ shows both, the enrollment's labeled and the request's bare", async () => {
  const agents = spyOn(api, "listAgents").mockResolvedValue([
    runningAgent({
      dir: "/home/sami/legion",
      machine_id: "devbox-sami",
      session_id: "sess-enrollment",
    }),
  ]);
  const { getRecord } = renderRecordPage({
    ...decidedSecretRecord(),
    session: { enrollment: "sess-enrollment", request: "sess-request" },
  });
  try {
    const link = await screen.findByRole("link", { name: "Reviewing LEGION-587" });
    expect(link.getAttribute("href")).toBe("/agents/sess-enrollment/live");
    expect(screen.getByText(/The session that enrolled:/)).toBeDefined();
    expect(screen.getByText("sess-request isn't running.")).toBeDefined();
    expect(screen.queryByText(/says it came from/)).toBeNull();
  } finally {
    cleanup();
    getRecord.mockRestore();
    agents.mockRestore();
  }
});

// The request's session id is shown as written, with no preamble, whether or not an enrollment id
// is present - the record page shares this rule with the Inbox row through the same
// `CredentialSessionLines` component.
test("a record's request-only session id is shown with no preamble", async () => {
  const agents = spyOn(api, "listAgents").mockResolvedValue([runningAgent()]);
  const { getRecord } = renderRecordPage({
    ...decidedSecretRecord(),
    session: { enrollment: null, request: "sess-1" },
  });
  try {
    const link = await screen.findByRole("link", { name: "Reviewing LEGION-587" });
    expect(link.getAttribute("href")).toBe("/agents/sess-1/live");
    expect(link.parentElement?.textContent?.startsWith("Reviewing LEGION-587")).toBe(true);
    expect(screen.queryByText(/says it came from/)).toBeNull();
  } finally {
    cleanup();
    getRecord.mockRestore();
    agents.mockRestore();
  }
});
