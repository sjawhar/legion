import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { api } from "../../api/client";
import type { CredentialRecord } from "../../api/types";
import { CredentialRecordPage } from "./CredentialRecordPage";

function secretRecord(overrides: Partial<CredentialRecord> = {}): CredentialRecord {
  return {
    approver: "sami",
    decided: null,
    enrollment: { kind: "devbox", operator: "sami", runtime_id: "runtime-1", slot: null },
    expires_at: "2026-09-27T01:00:00Z",
    identifiers: ["ANTHROPIC_API_KEY"],
    kind: "agent_secret",
    lifetime_seconds: 900,
    reason: "**bold** [x](y)",
    record_id: "req-1",
    requested_at: "2026-09-27T00:00:00Z",
    rules_version: "v3",
    service: null,
    state: "pending",
    ...overrides,
  };
}

function renderPage(recordId = "req-1") {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter initialEntries={[`/credentials/${recordId}`]}>
      <QueryClientProvider client={queryClient}>
        <Routes>
          <Route element={<CredentialRecordPage />} path="/credentials/:recordId" />
        </Routes>
      </QueryClientProvider>
    </MemoryRouter>
  );
  return { queryClient, view };
}

test("renders the broker's facts before the agent's reason, rendered as plain text with no linkification", async () => {
  const getCredentialRecord = spyOn(api, "getCredentialRecord").mockResolvedValue(secretRecord());

  try {
    renderPage();

    const identifiers = await screen.findByText("ANTHROPIC_API_KEY");
    const quote = screen.getByText("**bold** [x](y)");
    expect(quote.tagName).toBe("BLOCKQUOTE");
    expect(quote.querySelector("a")).toBeNull();
    expect(
      Boolean(identifiers.compareDocumentPosition(quote) & Node.DOCUMENT_POSITION_FOLLOWING)
    ).toBe(true);
    expect(screen.getByText("15 minutes")).toBeDefined();
  } finally {
    cleanup();
    getCredentialRecord.mockRestore();
  }
});

test("Approve is one plain POST for the record, with no body", async () => {
  const record = secretRecord();
  const getCredentialRecord = spyOn(api, "getCredentialRecord").mockResolvedValue(record);
  const approveCredentialRecord = spyOn(api, "approveCredentialRecord").mockResolvedValue({
    credential_id: null,
    grant_id: "grant-1",
    state: "approved",
  });

  try {
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "Approve" }));

    await waitFor(() => expect(approveCredentialRecord).toHaveBeenCalledTimes(1));
    expect(approveCredentialRecord).toHaveBeenCalledWith("req-1");
  } finally {
    cleanup();
    getCredentialRecord.mockRestore();
    approveCredentialRecord.mockRestore();
  }
});

// A shared secret's request waits on anyone signed in, which the broker names with the word
// `anyone`; the page says so in words, and the viewer, whoever they are, gets the decision buttons.
test("a request any signed-in person may decide says so and offers its buttons", async () => {
  const getCredentialRecord = spyOn(api, "getCredentialRecord").mockResolvedValue(
    secretRecord({ approver: "anyone" })
  );

  try {
    renderPage();

    expect(await screen.findByText("Anyone signed in to Dispatch")).toBeDefined();
    expect(screen.queryByText("anyone")).toBeNull();
    expect(screen.getByRole("button", { name: "Approve" })).toBeDefined();
  } finally {
    cleanup();
    getCredentialRecord.mockRestore();
  }
});

test("a machine-kind record renders the machine sentence, links to the machine page, and shows no Approve button", async () => {
  for (const { sentence, service } of [
    {
      sentence: "Approving lets worker-7.example.com start agent sessions as you.",
      service: null,
    },
    {
      sentence:
        "Approving lets legion-daemon on worker-7.example.com start worker pods as legion-daemon, not as you: no secret of yours reaches its pods unless you approve the request for it.",
      service: "legion-daemon",
    },
  ]) {
    const record = secretRecord({
      identifiers: ["worker-7.example.com"],
      kind: "launcher_credential",
      reason: "",
      service,
    });
    const getCredentialRecord = spyOn(api, "getCredentialRecord").mockResolvedValue(record);

    try {
      renderPage();

      expect(await screen.findByText(sentence)).toBeDefined();
      expect(screen.queryByRole("button", { name: "Approve" })).toBeNull();
      expect(screen.queryByRole("button", { name: "Deny" })).toBeNull();
      expect(
        screen
          .getByRole("link", { name: "Enter the code shown on the machine" })
          .getAttribute("href")
      ).toBe("/credentials/machine");
    } finally {
      cleanup();
      getCredentialRecord.mockRestore();
    }
  }
});

test("a decided record renders the decision and no buttons", async () => {
  const record = secretRecord({
    decided: { at: "2026-09-27T00:30:00Z", credential_id: "cred-123", event: "approved" },
    state: "approved",
  });
  const getCredentialRecord = spyOn(api, "getCredentialRecord").mockResolvedValue(record);

  try {
    renderPage();

    expect(await screen.findByText(/approved/)).toBeDefined();
    expect(screen.getByText("Credential cred-123")).toBeDefined();
    expect(screen.queryByRole("button", { name: "Approve" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Deny" })).toBeNull();
  } finally {
    cleanup();
    getCredentialRecord.mockRestore();
  }
});

test("an empty-reason record renders no reason blockquote", async () => {
  const getCredentialRecord = spyOn(api, "getCredentialRecord").mockResolvedValue(
    secretRecord({ reason: "" })
  );

  try {
    renderPage();

    expect(await screen.findByText("ANTHROPIC_API_KEY")).toBeDefined();
    expect(screen.queryByText("The agent's stated reason")).toBeNull();
    expect(document.querySelector("blockquote")).toBeNull();
  } finally {
    cleanup();
    getCredentialRecord.mockRestore();
  }
});

// Two roles of one pod share its kind, runtime id and (absent) operator, so the slot is the fact
// that tells their requests apart; a record without one shows the facts it always did.
test("a pod record names its worker slot, and a record with no slot shows no slot fact", async () => {
  const podRecord = secretRecord({
    enrollment: { kind: "pod", operator: "", runtime_id: "3f9c-pod-uid", slot: "reviewer-g2" },
  });
  const getCredentialRecord = spyOn(api, "getCredentialRecord").mockResolvedValue(podRecord);

  try {
    renderPage();
    expect(await screen.findByText("Worker slot")).toBeDefined();
    expect(screen.getByText("reviewer-g2")).toBeDefined();
    expect(screen.getByText("pod · 3f9c-pod-uid · —")).toBeDefined();
    cleanup();

    getCredentialRecord.mockResolvedValue(secretRecord());
    renderPage();
    expect(await screen.findByText("devbox · runtime-1 · sami")).toBeDefined();
    expect(screen.queryByText("Worker slot")).toBeNull();
  } finally {
    cleanup();
    getCredentialRecord.mockRestore();
  }
});
