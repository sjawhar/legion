import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { api } from "../../api/client";
import type { CredentialRecord } from "../../api/types";
import { bufToB64url } from "../../lib/webauthn";
import { CredentialRecordPage } from "./CredentialRecordPage";

function bufFrom(text: string): ArrayBuffer {
  return new TextEncoder().encode(text).buffer as ArrayBuffer;
}

function stubCredentials(fake: { get?: (options: unknown) => Promise<unknown> }): void {
  Object.defineProperty(navigator, "credentials", { configurable: true, value: fake });
}

function secretRecord(overrides: Partial<CredentialRecord> = {}): CredentialRecord {
  return {
    approver: "sami",
    challenges: {
      approve: bufToB64url(bufFrom("approve-challenge")),
      deny: bufToB64url(bufFrom("deny-challenge")),
    },
    decided: null,
    enrollment: { kind: "devbox", operator: "sami", runtime_id: "runtime-1" },
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

test("Approve runs the WebAuthn ceremony against the record's own challenge and posts the assertion", async () => {
  const record = secretRecord();
  const getCredentialRecord = spyOn(api, "getCredentialRecord").mockResolvedValue(record);
  const approveCredentialRecord = spyOn(api, "approveCredentialRecord").mockResolvedValue({
    credential_id: null,
    grant_id: "grant-1",
    state: "approved",
  });
  let capturedChallenge: ArrayBuffer | undefined;
  stubCredentials({
    get: async (options) => {
      const requestOptions = options as { publicKey: { challenge: ArrayBuffer } };
      capturedChallenge = requestOptions.publicKey.challenge;
      return {
        id: "credential-id",
        rawId: bufFrom("raw-id-bytes"),
        response: {
          authenticatorData: bufFrom("authenticator-data"),
          clientDataJSON: bufFrom("client-data-json"),
          signature: bufFrom("signature-bytes"),
        },
        type: "public-key",
      };
    },
  });

  try {
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "Approve" }));

    await waitFor(() => expect(approveCredentialRecord).toHaveBeenCalledTimes(1));
    if (record.challenges === null) {
      throw new Error("expected the fixture to carry challenges");
    }
    expect(bufToB64url(capturedChallenge as ArrayBuffer)).toBe(record.challenges.approve);
    expect(approveCredentialRecord).toHaveBeenCalledWith("req-1", {
      assertion: {
        id: "credential-id",
        rawId: bufToB64url(bufFrom("raw-id-bytes")),
        response: {
          authenticatorData: bufToB64url(bufFrom("authenticator-data")),
          clientDataJSON: bufToB64url(bufFrom("client-data-json")),
          signature: bufToB64url(bufFrom("signature-bytes")),
        },
        type: "public-key",
      },
    });
  } finally {
    cleanup();
    getCredentialRecord.mockRestore();
    approveCredentialRecord.mockRestore();
  }
});

test("a machine-kind record renders the machine sentence, links to the machine page, and shows no Approve button", async () => {
  const record = secretRecord({
    challenges: null,
    identifiers: ["worker-7.example.com"],
    kind: "launcher_credential",
    reason: "",
  });
  const getCredentialRecord = spyOn(api, "getCredentialRecord").mockResolvedValue(record);

  try {
    renderPage();

    expect(
      await screen.findByText("Approving lets worker-7.example.com start agent sessions as you.")
    ).toBeDefined();
    expect(screen.queryByRole("button", { name: "Approve" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Deny" })).toBeNull();
    expect(
      screen.getByRole("link", { name: "Enter the code shown on the machine" }).getAttribute("href")
    ).toBe("/credentials/machine");
  } finally {
    cleanup();
    getCredentialRecord.mockRestore();
  }
});

test("a decided record renders the decision and no buttons", async () => {
  const record = secretRecord({
    challenges: null,
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
