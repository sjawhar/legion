import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
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
    enrollment: { kind: "session", operator: "sami", runtime_id: "box-1", slot: null },
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

// A request that asked for no lifetime gets a grant that lasts until its session ends, which the
// broker answers as a null lifetime; the page says so rather than "0 seconds".
test("a record with no lifetime says its grant lasts until the session ends", async () => {
  const { getRecord } = renderRecordPage({ ...decidedSecretRecord(), lifetime_seconds: null });
  try {
    const lifetime = await screen.findByText("Until the session ends");
    expect(lifetime.previousElementSibling?.textContent).toBe("Lifetime");
    expect(screen.queryByText(/second/)).toBeNull();
  } finally {
    cleanup();
    getRecord.mockRestore();
  }
});

test("a record with a lifetime names it in whole units", async () => {
  const { getRecord } = renderRecordPage({ ...decidedSecretRecord(), lifetime_seconds: 120 });
  try {
    expect(await screen.findByText("2 minutes")).toBeDefined();
    expect(screen.queryByText("Until the session ends")).toBeNull();
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
    expect(link.closest("[data-agent-name]")?.parentElement?.textContent).toBe(
      "The session that enrolled: Reviewing LEGION-587 · sess-enrollment · /home/sami/legion · devbox-alice"
    );
    const request = await screen.findByRole("link", { name: "session:sess-req…" });
    expect(request.getAttribute("href")).toBe("/agents/sess-request/live");
    const requestAgent = request.closest("[data-agent-name]");
    expect(requestAgent?.getAttribute("data-agent-state")).toBe("not-running");
    expect(requestAgent?.parentElement?.textContent).toBe(
      "session:sess-req… · sess-request isn't running."
    );
    expect(screen.queryByText(/says it came from/)).toBeNull();
  } finally {
    cleanup();
    getRecord.mockRestore();
    agents.mockRestore();
  }
});

// A request's session id is an unsigned claim, and a short form `session:<8>…` is shared by every
// id with the same first eight characters: an unlisted id that copies a real untitled session's
// prefix must still read apart from it, by its full id and "isn't running" in words.
test("a request id that shares a running session's short form reads apart by its full id", async () => {
  const agents = spyOn(api, "listAgents").mockResolvedValue([
    runningAgent({ session_id: "sess-real-0001", title: "" }),
  ]);
  const { getRecord } = renderRecordPage({
    ...decidedSecretRecord(),
    session: { enrollment: "sess-real-0001", request: "sess-real-0002" },
  });
  try {
    const [enrolled, claimed] = await screen.findAllByRole("link", { name: "session:sess-rea…" });
    expect(enrolled?.closest("[data-agent-name]")?.parentElement?.textContent).toBe(
      "The session that enrolled: session:sess-rea… · sess-real-0001 · /home/alice/legion · devbox-alice"
    );
    expect(claimed?.getAttribute("href")).toBe("/agents/sess-real-0002/live");
    expect(claimed?.closest("[data-agent-name]")?.parentElement?.textContent).toBe(
      "session:sess-rea… · sess-real-0002 isn't running."
    );
  } finally {
    cleanup();
    getRecord.mockRestore();
    agents.mockRestore();
  }
});

// Envoy unavailable or not configured: the record page says, with the full id, that it couldn't
// check, never that the session isn't running.
test("a record's session reads as couldn't-check, with its full id, while the agents list cannot be read", async () => {
  const agents = spyOn(api, "listAgents").mockRejectedValue(new Error("envoy unavailable"));
  const { getRecord } = renderRecordPage({
    ...decidedSecretRecord(),
    session: { enrollment: "sess-gone", request: null },
  });
  try {
    const name = await screen.findByRole("link", { name: "session:sess-gon…" });
    await waitFor(() =>
      expect(name.closest("[data-agent-name]")?.parentElement?.textContent).toBe(
        "session:sess-gon… · couldn't check whether sess-gone is running."
      )
    );
    expect(screen.queryByText(/isn't running/)).toBeNull();
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
    expect(link.closest("[data-agent-name]")?.parentElement?.textContent).toBe(
      "Reviewing LEGION-587 · sess-1 · /home/alice/legion · devbox-alice"
    );
    expect(screen.queryByText(/says it came from/)).toBeNull();
  } finally {
    cleanup();
    getRecord.mockRestore();
    agents.mockRestore();
  }
});
