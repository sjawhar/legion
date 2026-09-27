import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";

import { ApiError, api } from "../../api/client";
import type { AuthenticatedUser, CredentialGrant, CredentialKey } from "../../api/types";
import { b64urlToBuf, bufToB64url } from "../../lib/webauthn";
import { KeysPage } from "./KeysPage";

function bufFrom(text: string): ArrayBuffer {
  return new TextEncoder().encode(text).buffer as ArrayBuffer;
}

function stubCredentials(fake: {
  create?: (options: unknown) => Promise<unknown>;
  get?: (options: unknown) => Promise<unknown>;
}): void {
  Object.defineProperty(navigator, "credentials", { configurable: true, value: fake });
}

const viewer: AuthenticatedUser = { kind: "user", login: "Sami" };

function credentialKey(overrides: Partial<CredentialKey> = {}): CredentialKey {
  return {
    aaguid: "ee882879-721c-4913-9775-3dfcce97072a",
    credential_id: "existing-key",
    endorsed_by: null,
    last_used_at: null,
    registered_at: "2026-09-01T00:00:00Z",
    seeded: true,
    state: "active",
    ...overrides,
  };
}

function credentialGrant(overrides: Partial<CredentialGrant> = {}): CredentialGrant {
  return {
    created_at: "2026-09-20T00:00:00Z",
    enrollment: { kind: "devbox", operator: "sami", runtime_id: "runtime-1" },
    expires_at: "2026-09-27T00:00:00Z",
    grant_id: "grant-1",
    names: ["ANTHROPIC_API_KEY"],
    record_id: "req-1",
    ...overrides,
  };
}

/** Finds the copyable YAML block's `<code>` text by the heading above it (`YamlBlock`'s own
 *  layout), rather than a text-matcher query — `<pre><code>` preserves the YAML's real
 *  newlines/indentation, which Testing Library's default text matcher would otherwise collapse
 *  before comparing against a literal multi-line fixture string. */
async function yamlBlockText(heading: string): Promise<string | null | undefined> {
  const headingElement = await screen.findByText(heading);
  return headingElement.closest("div")?.querySelector("code")?.textContent;
}

function renderPage() {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={queryClient}>
      <KeysPage />
    </QueryClientProvider>
  );
}

test("keys render with state chips", async () => {
  const whoAmI = spyOn(api, "whoAmI").mockResolvedValue(viewer);
  const getCredentialKeys = spyOn(api, "getCredentialKeys").mockResolvedValue({
    keys: [
      credentialKey({ aaguid: "aaguid-active", credential_id: "active-key", state: "active" }),
      credentialKey({
        aaguid: "aaguid-tombstoned",
        credential_id: "tombstoned-key",
        state: "tombstoned",
      }),
      credentialKey({
        aaguid: "aaguid-revoked",
        credential_id: "used-key",
        last_used_at: "2026-09-26T12:00:00Z",
        state: "revoked",
      }),
    ],
  });
  const getCredentialGrants = spyOn(api, "getCredentialGrants").mockResolvedValue({ grants: [] });

  try {
    renderPage();

    await screen.findByText("aaguid-active");
    expect(getCredentialKeys).toHaveBeenCalledWith("sami");
    expect(screen.getByText("aaguid-tombstoned")).toBeTruthy();
    expect(screen.getByText("aaguid-revoked")).toBeTruthy();
    expect(screen.getByText("Active")).toBeTruthy();
    expect(screen.getByText("Tombstoned")).toBeTruthy();
    expect(screen.getByText("Revoked")).toBeTruthy();
    expect(screen.getAllByText("Never used")).toHaveLength(2);
  } finally {
    cleanup();
    whoAmI.mockRestore();
    getCredentialKeys.mockRestore();
    getCredentialGrants.mockRestore();
  }
});

test("register flow calls navigator.credentials.create with the mocked options and POSTs finish with the encoded response, rendering the YAML text", async () => {
  const whoAmI = spyOn(api, "whoAmI").mockResolvedValue(viewer);
  const getCredentialKeys = spyOn(api, "getCredentialKeys").mockResolvedValue({ keys: [] });
  const getCredentialGrants = spyOn(api, "getCredentialGrants").mockResolvedValue({ grants: [] });
  const beginCredentialKeyRegistration = spyOn(
    api,
    "beginCredentialKeyRegistration"
  ).mockResolvedValue({
    ceremony_id: "ceremony-register",
    publicKey: {
      attestation: "direct",
      challenge: bufToB64url(bufFrom("register-challenge")),
      pubKeyCredParams: [{ alg: -7, type: "public-key" }],
      rp: { name: "Dispatch" },
      user: { displayName: "sami", id: bufToB64url(bufFrom("user-id")), name: "sami" },
    },
  });
  const registerYaml = "credential_id: new-key\nregistration:\n  nonce: abc\n";
  const finishCredentialKeyRegistration = spyOn(
    api,
    "finishCredentialKeyRegistration"
  ).mockResolvedValue({ yaml: registerYaml });
  let capturedChallenge: ArrayBuffer | undefined;
  stubCredentials({
    create: async (options) => {
      const creationOptions = options as { publicKey: { challenge: ArrayBuffer } };
      capturedChallenge = creationOptions.publicKey.challenge;
      return {
        id: "new-key",
        rawId: bufFrom("new-key-raw-id"),
        response: {
          attestationObject: bufFrom("attestation-object"),
          clientDataJSON: bufFrom("client-data-json"),
        },
        type: "public-key",
      };
    },
  });

  try {
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "Register key" }));

    await waitFor(() => expect(finishCredentialKeyRegistration).toHaveBeenCalledTimes(1));
    expect(bufToB64url(capturedChallenge as ArrayBuffer)).toBe(
      bufToB64url(bufFrom("register-challenge"))
    );
    expect(finishCredentialKeyRegistration).toHaveBeenCalledWith("sami", {
      ceremony_id: "ceremony-register",
      response: {
        id: "new-key",
        rawId: bufToB64url(bufFrom("new-key-raw-id")),
        response: {
          attestationObject: bufToB64url(bufFrom("attestation-object")),
          clientDataJSON: bufToB64url(bufFrom("client-data-json")),
        },
        type: "public-key",
      },
    });
    expect(await yamlBlockText("New key registered")).toBe(registerYaml);
    expect(
      screen.getByText(/Add this entry to meta\/infra\/config\/agent-secret-rules\.yaml/)
    ).toBeTruthy();
  } finally {
    cleanup();
    whoAmI.mockRestore();
    getCredentialKeys.mockRestore();
    getCredentialGrants.mockRestore();
    beginCredentialKeyRegistration.mockRestore();
    finishCredentialKeyRegistration.mockRestore();
  }
});

test("endorse flow drives get with the endorse challenge and renders the endorsement YAML", async () => {
  const whoAmI = spyOn(api, "whoAmI").mockResolvedValue(viewer);
  const getCredentialKeys = spyOn(api, "getCredentialKeys").mockResolvedValue({
    keys: [credentialKey({ credential_id: "existing-active-credential", state: "active" })],
  });
  const getCredentialGrants = spyOn(api, "getCredentialGrants").mockResolvedValue({ grants: [] });
  const beginCredentialKeyRegistration = spyOn(
    api,
    "beginCredentialKeyRegistration"
  ).mockResolvedValue({
    ceremony_id: "ceremony-register",
    publicKey: {
      attestation: "direct",
      challenge: bufToB64url(bufFrom("register-challenge")),
      pubKeyCredParams: [{ alg: -7, type: "public-key" }],
      rp: { name: "Dispatch" },
      user: { displayName: "sami", id: bufToB64url(bufFrom("user-id")), name: "sami" },
    },
  });
  const finishCredentialKeyRegistration = spyOn(
    api,
    "finishCredentialKeyRegistration"
  ).mockResolvedValue({ yaml: "credential_id: new-key\nregistration:\n  nonce: abc\n" });
  const beginCredentialKeyEndorsement = spyOn(
    api,
    "beginCredentialKeyEndorsement"
  ).mockResolvedValue({
    ceremony_id: "ceremony-endorse",
    publicKey: {
      allowCredentials: [{ id: bufToB64url(bufFrom("existing-key-raw-id")), type: "public-key" }],
      challenge: bufToB64url(bufFrom("endorse-challenge")),
    },
  });
  const endorsementYaml = "endorsement:\n  by: existing-key\n";
  const finishCredentialKeyEndorsement = spyOn(
    api,
    "finishCredentialKeyEndorsement"
  ).mockResolvedValue({ yaml: endorsementYaml });
  let capturedChallenge: ArrayBuffer | undefined;
  let capturedAllowCredentials: { id: ArrayBuffer; type: string }[] | undefined;
  stubCredentials({
    create: async () => ({
      id: "new-key",
      rawId: bufFrom("new-key-raw-id"),
      response: {
        attestationObject: bufFrom("attestation-object"),
        clientDataJSON: bufFrom("client-data-json"),
      },
      type: "public-key",
    }),
    get: async (options) => {
      const requestOptions = options as {
        publicKey: {
          challenge: ArrayBuffer;
          allowCredentials?: { id: ArrayBuffer; type: string }[];
        };
      };
      capturedChallenge = requestOptions.publicKey.challenge;
      capturedAllowCredentials = requestOptions.publicKey.allowCredentials;
      return {
        id: "existing-key",
        rawId: bufFrom("existing-key-raw-id"),
        response: {
          authenticatorData: bufFrom("authenticator-data"),
          clientDataJSON: bufFrom("endorse-client-data-json"),
          signature: bufFrom("endorse-signature"),
        },
        type: "public-key",
      };
    },
  });

  try {
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "Register key" }));
    expect(screen.queryByRole("combobox")).toBeNull();
    fireEvent.click(await screen.findByRole("button", { name: "Endorse with another key" }));

    await waitFor(() => expect(finishCredentialKeyEndorsement).toHaveBeenCalledTimes(1));
    expect(bufToB64url(capturedChallenge as ArrayBuffer)).toBe(
      bufToB64url(bufFrom("endorse-challenge"))
    );
    expect(capturedAllowCredentials).toHaveLength(1);
    expect(capturedAllowCredentials?.[0].type).toBe("public-key");
    expect(new Uint8Array(capturedAllowCredentials?.[0].id as ArrayBuffer)).toEqual(
      new Uint8Array(bufFrom("existing-key-raw-id"))
    );
    expect(beginCredentialKeyEndorsement).toHaveBeenCalledTimes(1);
    const [loginArg, bodyArg] = beginCredentialKeyEndorsement.mock.calls[0] as [string, unknown];
    expect(loginArg).toBe("sami");
    expect((bodyArg as { credential_id: string }).credential_id).toBe("existing-active-credential");
    const keyHashBytes = new Uint8Array(
      await crypto.subtle.digest("SHA-256", b64urlToBuf("new-key"))
    );
    expect((bodyArg as { key_hash: string }).key_hash).toBe(
      Array.from(keyHashBytes, (byte) => byte.toString(16).padStart(2, "0")).join("")
    );
    expect(finishCredentialKeyEndorsement).toHaveBeenCalledWith("sami", {
      ceremony_id: "ceremony-endorse",
      response: {
        id: "existing-key",
        rawId: bufToB64url(bufFrom("existing-key-raw-id")),
        response: {
          authenticatorData: bufToB64url(bufFrom("authenticator-data")),
          clientDataJSON: bufToB64url(bufFrom("endorse-client-data-json")),
          signature: bufToB64url(bufFrom("endorse-signature")),
        },
        type: "public-key",
      },
    });
    expect(await yamlBlockText("Endorsement")).toBe(endorsementYaml);
  } finally {
    cleanup();
    whoAmI.mockRestore();
    getCredentialKeys.mockRestore();
    getCredentialGrants.mockRestore();
    beginCredentialKeyRegistration.mockRestore();
    finishCredentialKeyRegistration.mockRestore();
    beginCredentialKeyEndorsement.mockRestore();
    finishCredentialKeyEndorsement.mockRestore();
  }
});

test("a second register cycle clears the prior cycle's stale endorsement and re-offers Endorse for the new key", async () => {
  const whoAmI = spyOn(api, "whoAmI").mockResolvedValue(viewer);
  const getCredentialKeys = spyOn(api, "getCredentialKeys").mockResolvedValue({
    keys: [credentialKey({ credential_id: "existing-active-credential", state: "active" })],
  });
  const getCredentialGrants = spyOn(api, "getCredentialGrants").mockResolvedValue({ grants: [] });
  const beginCredentialKeyRegistration = spyOn(api, "beginCredentialKeyRegistration")
    .mockResolvedValueOnce({
      ceremony_id: "ceremony-register-a",
      publicKey: {
        attestation: "direct",
        challenge: bufToB64url(bufFrom("register-challenge-a")),
        pubKeyCredParams: [{ alg: -7, type: "public-key" }],
        rp: { name: "Dispatch" },
        user: { displayName: "sami", id: bufToB64url(bufFrom("user-id")), name: "sami" },
      },
    })
    .mockResolvedValueOnce({
      ceremony_id: "ceremony-register-b",
      publicKey: {
        attestation: "direct",
        challenge: bufToB64url(bufFrom("register-challenge-b")),
        pubKeyCredParams: [{ alg: -7, type: "public-key" }],
        rp: { name: "Dispatch" },
        user: { displayName: "sami", id: bufToB64url(bufFrom("user-id")), name: "sami" },
      },
    });
  const registerYamlA = "credential_id: key-a\nregistration:\n  nonce: aaa\n";
  const registerYamlB = "credential_id: key-b\nregistration:\n  nonce: bbb\n";
  const finishCredentialKeyRegistration = spyOn(api, "finishCredentialKeyRegistration")
    .mockResolvedValueOnce({ yaml: registerYamlA })
    .mockResolvedValueOnce({ yaml: registerYamlB });
  const beginCredentialKeyEndorsement = spyOn(
    api,
    "beginCredentialKeyEndorsement"
  ).mockResolvedValue({
    ceremony_id: "ceremony-endorse-a",
    publicKey: { challenge: bufToB64url(bufFrom("endorse-challenge-a")) },
  });
  const endorsementYamlA = "endorsement:\n  by: existing-key\n";
  const finishCredentialKeyEndorsement = spyOn(
    api,
    "finishCredentialKeyEndorsement"
  ).mockResolvedValue({ yaml: endorsementYamlA });
  let createCallCount = 0;
  stubCredentials({
    create: async () => {
      createCallCount += 1;
      return createCallCount === 1
        ? {
            id: "key-a",
            rawId: bufFrom("key-a-raw-id"),
            response: {
              attestationObject: bufFrom("attestation-a"),
              clientDataJSON: bufFrom("client-data-a"),
            },
            type: "public-key",
          }
        : {
            id: "key-b",
            rawId: bufFrom("key-b-raw-id"),
            response: {
              attestationObject: bufFrom("attestation-b"),
              clientDataJSON: bufFrom("client-data-b"),
            },
            type: "public-key",
          };
    },
    get: async () => ({
      id: "existing-key",
      rawId: bufFrom("existing-key-raw-id"),
      response: {
        authenticatorData: bufFrom("authenticator-data"),
        clientDataJSON: bufFrom("endorse-client-data-json"),
        signature: bufFrom("endorse-signature"),
      },
      type: "public-key",
    }),
  });

  try {
    renderPage();

    // Cycle 1: register key A, then endorse it.
    fireEvent.click(await screen.findByRole("button", { name: "Register key" }));
    await waitFor(() => expect(finishCredentialKeyRegistration).toHaveBeenCalledTimes(1));
    expect(await yamlBlockText("New key registered")).toBe(registerYamlA);
    fireEvent.click(await screen.findByRole("button", { name: "Endorse with another key" }));
    await waitFor(() => expect(finishCredentialKeyEndorsement).toHaveBeenCalledTimes(1));
    expect(await yamlBlockText("Endorsement")).toBe(endorsementYamlA);

    // Cycle 2 (design v4's routine rotation - "endorse-new, then remove-old"): register key B.
    // The stale endorsement from key A must not linger next to B's fresh registration YAML,
    // and the Endorse button must reappear (now wired to key B, not the old key A state).
    fireEvent.click(await screen.findByRole("button", { name: "Register key" }));
    await waitFor(() => expect(finishCredentialKeyRegistration).toHaveBeenCalledTimes(2));
    expect(await yamlBlockText("New key registered")).toBe(registerYamlB);
    expect(screen.queryByText("Endorsement")).toBeNull();

    fireEvent.click(await screen.findByRole("button", { name: "Endorse with another key" }));
    await waitFor(() => expect(beginCredentialKeyEndorsement).toHaveBeenCalledTimes(2));
    const [, secondEndorseBody] = beginCredentialKeyEndorsement.mock.calls[1] as [string, unknown];
    expect((secondEndorseBody as { credential_id: string }).credential_id).toBe(
      "existing-active-credential"
    );
  } finally {
    cleanup();
    whoAmI.mockRestore();
    getCredentialKeys.mockRestore();
    getCredentialGrants.mockRestore();
    beginCredentialKeyRegistration.mockRestore();
    finishCredentialKeyRegistration.mockRestore();
    beginCredentialKeyEndorsement.mockRestore();
    finishCredentialKeyEndorsement.mockRestore();
  }
});

test("endorse mutation excludes a non-active key and picks the active one as the signer", async () => {
  const whoAmI = spyOn(api, "whoAmI").mockResolvedValue(viewer);
  const getCredentialKeys = spyOn(api, "getCredentialKeys").mockResolvedValue({
    keys: [
      credentialKey({ credential_id: "tombstoned-one", state: "tombstoned" }),
      credentialKey({ credential_id: "the-live-one", state: "active" }),
    ],
  });
  const getCredentialGrants = spyOn(api, "getCredentialGrants").mockResolvedValue({ grants: [] });
  const beginCredentialKeyRegistration = spyOn(
    api,
    "beginCredentialKeyRegistration"
  ).mockResolvedValue({
    ceremony_id: "ceremony-register",
    publicKey: {
      attestation: "direct",
      challenge: bufToB64url(bufFrom("register-challenge")),
      pubKeyCredParams: [{ alg: -7, type: "public-key" }],
      rp: { name: "Dispatch" },
      user: { displayName: "sami", id: bufToB64url(bufFrom("user-id")), name: "sami" },
    },
  });
  const finishCredentialKeyRegistration = spyOn(
    api,
    "finishCredentialKeyRegistration"
  ).mockResolvedValue({ yaml: "credential_id: new-key\nregistration:\n  nonce: abc\n" });
  const beginCredentialKeyEndorsement = spyOn(
    api,
    "beginCredentialKeyEndorsement"
  ).mockResolvedValue({
    ceremony_id: "ceremony-endorse",
    publicKey: {
      allowCredentials: [{ id: bufToB64url(bufFrom("the-live-one-raw-id")), type: "public-key" }],
      challenge: bufToB64url(bufFrom("endorse-challenge")),
    },
  });
  const finishCredentialKeyEndorsement = spyOn(
    api,
    "finishCredentialKeyEndorsement"
  ).mockResolvedValue({ yaml: "endorsement:\n  by: the-live-one\n" });
  stubCredentials({
    create: async () => ({
      id: "new-key",
      rawId: bufFrom("new-key-raw-id"),
      response: {
        attestationObject: bufFrom("attestation-object"),
        clientDataJSON: bufFrom("client-data-json"),
      },
      type: "public-key",
    }),
    get: async () => ({
      id: "the-live-one",
      rawId: bufFrom("the-live-one-raw-id"),
      response: {
        authenticatorData: bufFrom("authenticator-data"),
        clientDataJSON: bufFrom("endorse-client-data-json"),
        signature: bufFrom("endorse-signature"),
      },
      type: "public-key",
    }),
  });

  try {
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "Register key" }));
    fireEvent.click(await screen.findByRole("button", { name: "Endorse with another key" }));

    await waitFor(() => expect(beginCredentialKeyEndorsement).toHaveBeenCalledTimes(1));
    const [, bodyArg] = beginCredentialKeyEndorsement.mock.calls[0] as [string, unknown];
    expect((bodyArg as { credential_id: string }).credential_id).toBe("the-live-one");
  } finally {
    cleanup();
    whoAmI.mockRestore();
    getCredentialKeys.mockRestore();
    getCredentialGrants.mockRestore();
    beginCredentialKeyRegistration.mockRestore();
    finishCredentialKeyRegistration.mockRestore();
    beginCredentialKeyEndorsement.mockRestore();
    finishCredentialKeyEndorsement.mockRestore();
  }
});

test("endorse stays disabled when there is no active key to endorse with", async () => {
  const whoAmI = spyOn(api, "whoAmI").mockResolvedValue(viewer);
  const getCredentialKeys = spyOn(api, "getCredentialKeys").mockResolvedValue({ keys: [] });
  const getCredentialGrants = spyOn(api, "getCredentialGrants").mockResolvedValue({ grants: [] });
  const beginCredentialKeyRegistration = spyOn(
    api,
    "beginCredentialKeyRegistration"
  ).mockResolvedValue({
    ceremony_id: "ceremony-register",
    publicKey: {
      attestation: "direct",
      challenge: bufToB64url(bufFrom("register-challenge")),
      pubKeyCredParams: [{ alg: -7, type: "public-key" }],
      rp: { name: "Dispatch" },
      user: { displayName: "sami", id: bufToB64url(bufFrom("user-id")), name: "sami" },
    },
  });
  const finishCredentialKeyRegistration = spyOn(
    api,
    "finishCredentialKeyRegistration"
  ).mockResolvedValue({ yaml: "credential_id: new-key\nregistration:\n  nonce: abc\n" });
  const beginCredentialKeyEndorsement = spyOn(
    api,
    "beginCredentialKeyEndorsement"
  ).mockResolvedValue({
    ceremony_id: "ceremony-endorse",
    publicKey: { challenge: bufToB64url(bufFrom("endorse-challenge")) },
  });
  stubCredentials({
    create: async () => ({
      id: "new-key",
      rawId: bufFrom("new-key-raw-id"),
      response: {
        attestationObject: bufFrom("attestation-object"),
        clientDataJSON: bufFrom("client-data-json"),
      },
      type: "public-key",
    }),
  });

  try {
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "Register key" }));

    const endorseButton = (await screen.findByRole("button", {
      name: "Endorse with another key",
    })) as HTMLButtonElement;
    expect(endorseButton.disabled).toBe(true);

    fireEvent.click(endorseButton);
    expect(beginCredentialKeyEndorsement).not.toHaveBeenCalled();
    expect(screen.queryByText("Could not endorse this key.")).toBeNull();
  } finally {
    cleanup();
    whoAmI.mockRestore();
    getCredentialKeys.mockRestore();
    getCredentialGrants.mockRestore();
    beginCredentialKeyRegistration.mockRestore();
    finishCredentialKeyRegistration.mockRestore();
    beginCredentialKeyEndorsement.mockRestore();
  }
});

test("multiple active keys: Endorse stays disabled until a key is picked, and the ceremony uses the pick", async () => {
  const whoAmI = spyOn(api, "whoAmI").mockResolvedValue(viewer);
  const getCredentialKeys = spyOn(api, "getCredentialKeys").mockResolvedValue({
    keys: [
      credentialKey({
        aaguid: "aaaaaaaa-0000-0000-0000-000000000001",
        credential_id: "key-alpha",
        last_used_at: "2026-09-10T00:00:00Z",
        registered_at: "2026-08-01T00:00:00Z",
        state: "active",
      }),
      credentialKey({
        aaguid: "bbbbbbbb-0000-0000-0000-000000000002",
        credential_id: "key-beta",
        last_used_at: null,
        registered_at: "2026-08-15T00:00:00Z",
        state: "active",
      }),
    ],
  });
  const getCredentialGrants = spyOn(api, "getCredentialGrants").mockResolvedValue({ grants: [] });
  const beginCredentialKeyRegistration = spyOn(
    api,
    "beginCredentialKeyRegistration"
  ).mockResolvedValue({
    ceremony_id: "ceremony-register",
    publicKey: {
      attestation: "direct",
      challenge: bufToB64url(bufFrom("register-challenge")),
      pubKeyCredParams: [{ alg: -7, type: "public-key" }],
      rp: { name: "Dispatch" },
      user: { displayName: "sami", id: bufToB64url(bufFrom("user-id")), name: "sami" },
    },
  });
  const finishCredentialKeyRegistration = spyOn(
    api,
    "finishCredentialKeyRegistration"
  ).mockResolvedValue({ yaml: "credential_id: new-key\nregistration:\n  nonce: abc\n" });
  const beginCredentialKeyEndorsement = spyOn(
    api,
    "beginCredentialKeyEndorsement"
  ).mockResolvedValue({
    ceremony_id: "ceremony-endorse",
    publicKey: { challenge: bufToB64url(bufFrom("endorse-challenge")) },
  });
  const finishCredentialKeyEndorsement = spyOn(
    api,
    "finishCredentialKeyEndorsement"
  ).mockResolvedValue({ yaml: "endorsement:\n  by: key-beta\n" });
  stubCredentials({
    create: async () => ({
      id: "new-key",
      rawId: bufFrom("new-key-raw-id"),
      response: {
        attestationObject: bufFrom("attestation-object"),
        clientDataJSON: bufFrom("client-data-json"),
      },
      type: "public-key",
    }),
    get: async () => ({
      id: "key-beta",
      rawId: bufFrom("key-beta-raw-id"),
      response: {
        authenticatorData: bufFrom("authenticator-data"),
        clientDataJSON: bufFrom("endorse-client-data-json"),
        signature: bufFrom("endorse-signature"),
      },
      type: "public-key",
    }),
  });

  try {
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "Register key" }));
    await screen.findByText("New key registered");

    const endorseButton = (await screen.findByRole("button", {
      name: "Endorse with another key",
    })) as HTMLButtonElement;
    expect(endorseButton.disabled).toBe(true);

    const select = screen.getByRole("combobox") as HTMLSelectElement;
    const optionTexts = Array.from(select.options).map((option) => option.textContent ?? "");
    expect(optionTexts.some((text) => text.includes("aaaaaaaa-0000-0000-0000-000000000001"))).toBe(
      true
    );
    expect(optionTexts.some((text) => text.includes("bbbbbbbb-0000-0000-0000-000000000002"))).toBe(
      true
    );

    fireEvent.change(select, { target: { value: "key-beta" } });
    expect(endorseButton.disabled).toBe(false);

    fireEvent.click(endorseButton);

    await waitFor(() => expect(beginCredentialKeyEndorsement).toHaveBeenCalledTimes(1));
    const [, bodyArg] = beginCredentialKeyEndorsement.mock.calls[0] as [string, unknown];
    expect(bodyArg).toMatchObject({ credential_id: "key-beta" });
  } finally {
    cleanup();
    whoAmI.mockRestore();
    getCredentialKeys.mockRestore();
    getCredentialGrants.mockRestore();
    beginCredentialKeyRegistration.mockRestore();
    finishCredentialKeyRegistration.mockRestore();
    beginCredentialKeyEndorsement.mockRestore();
    finishCredentialKeyEndorsement.mockRestore();
  }
});

test("a background keys refetch that drops the picked key disables Endorse instead of keeping the stale id", async () => {
  const whoAmI = spyOn(api, "whoAmI").mockResolvedValue(viewer);
  const getCredentialKeys = spyOn(api, "getCredentialKeys")
    .mockResolvedValueOnce({
      keys: [
        credentialKey({
          aaguid: "aaaaaaaa-0000-0000-0000-000000000001",
          credential_id: "key-alpha",
          last_used_at: "2026-09-10T00:00:00Z",
          registered_at: "2026-08-01T00:00:00Z",
          state: "active",
        }),
        credentialKey({
          aaguid: "bbbbbbbb-0000-0000-0000-000000000002",
          credential_id: "key-beta",
          last_used_at: null,
          registered_at: "2026-08-15T00:00:00Z",
          state: "active",
        }),
      ],
    })
    .mockResolvedValueOnce({
      keys: [
        credentialKey({
          aaguid: "aaaaaaaa-0000-0000-0000-000000000001",
          credential_id: "key-alpha",
          last_used_at: "2026-09-10T00:00:00Z",
          registered_at: "2026-08-01T00:00:00Z",
          state: "active",
        }),
        credentialKey({
          aaguid: "bbbbbbbb-0000-0000-0000-000000000002",
          credential_id: "key-beta",
          last_used_at: null,
          registered_at: "2026-08-15T00:00:00Z",
          state: "revoked",
        }),
        credentialKey({
          aaguid: "cccccccc-0000-0000-0000-000000000003",
          credential_id: "key-gamma",
          last_used_at: null,
          registered_at: "2026-08-20T00:00:00Z",
          state: "active",
        }),
      ],
    });
  const getCredentialGrants = spyOn(api, "getCredentialGrants").mockResolvedValue({ grants: [] });
  const beginCredentialKeyRegistration = spyOn(
    api,
    "beginCredentialKeyRegistration"
  ).mockResolvedValue({
    ceremony_id: "ceremony-register",
    publicKey: {
      attestation: "direct",
      challenge: bufToB64url(bufFrom("register-challenge")),
      pubKeyCredParams: [{ alg: -7, type: "public-key" }],
      rp: { name: "Dispatch" },
      user: { displayName: "sami", id: bufToB64url(bufFrom("user-id")), name: "sami" },
    },
  });
  const finishCredentialKeyRegistration = spyOn(
    api,
    "finishCredentialKeyRegistration"
  ).mockResolvedValue({ yaml: "credential_id: new-key\nregistration:\n  nonce: abc\n" });
  const beginCredentialKeyEndorsement = spyOn(api, "beginCredentialKeyEndorsement");
  stubCredentials({
    create: async () => ({
      id: "new-key",
      rawId: bufFrom("new-key-raw-id"),
      response: {
        attestationObject: bufFrom("attestation-object"),
        clientDataJSON: bufFrom("client-data-json"),
      },
      type: "public-key",
    }),
  });

  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });

  try {
    render(
      <QueryClientProvider client={queryClient}>
        <KeysPage />
      </QueryClientProvider>
    );

    fireEvent.click(await screen.findByRole("button", { name: "Register key" }));
    await screen.findByText("New key registered");

    const endorseButton = (await screen.findByRole("button", {
      name: "Endorse with another key",
    })) as HTMLButtonElement;
    const select = screen.getByRole("combobox") as HTMLSelectElement;

    // Pick the second active key ("key-beta") — the picker mirrors the multi-active-keys test.
    fireEvent.change(select, { target: { value: "key-beta" } });
    expect(endorseButton.disabled).toBe(false);

    // A background refetch (e.g. React Query's refetchOnWindowFocus) lands while "key-beta" is
    // still selected: the second response revokes it and adds a third active key, so there are
    // still two active keys (never auto-picked), but the one the user chose is no longer among
    // them.
    await queryClient.invalidateQueries({ queryKey: ["credential-keys", "sami"] });
    await waitFor(() => expect(getCredentialKeys).toHaveBeenCalledTimes(2));

    // The stale selection must never resolve to a truthy endorser id: Endorse goes back to
    // disabled rather than staying enabled and sending a ceremony for a key that is no longer
    // active.
    await waitFor(() => expect(endorseButton.disabled).toBe(true));
    fireEvent.click(endorseButton);
    expect(beginCredentialKeyEndorsement).not.toHaveBeenCalled();
  } finally {
    cleanup();
    whoAmI.mockRestore();
    getCredentialKeys.mockRestore();
    getCredentialGrants.mockRestore();
    beginCredentialKeyRegistration.mockRestore();
    finishCredentialKeyRegistration.mockRestore();
    beginCredentialKeyEndorsement.mockRestore();
  }
});

test("revoke POSTs the assertion to /api/v1/credential-grants/{id}/revoke and refreshes the grants query", async () => {
  const whoAmI = spyOn(api, "whoAmI").mockResolvedValue(viewer);
  const getCredentialKeys = spyOn(api, "getCredentialKeys").mockResolvedValue({ keys: [] });
  const grant = credentialGrant();
  const getCredentialGrants = spyOn(api, "getCredentialGrants")
    .mockResolvedValueOnce({ grants: [grant] })
    .mockResolvedValueOnce({ grants: [] });
  const revokeCredentialGrant = spyOn(api, "revokeCredentialGrant").mockResolvedValue();
  let capturedChallenge: ArrayBuffer | undefined;
  stubCredentials({
    get: async (options) => {
      const requestOptions = options as { publicKey: { challenge: ArrayBuffer } };
      capturedChallenge = requestOptions.publicKey.challenge;
      return {
        id: "existing-key",
        rawId: bufFrom("existing-key-raw-id"),
        response: {
          authenticatorData: bufFrom("authenticator-data"),
          clientDataJSON: bufFrom("revoke-client-data-json"),
          signature: bufFrom("revoke-signature"),
        },
        type: "public-key",
      };
    },
  });

  try {
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "Revoke" }));

    await waitFor(() => expect(revokeCredentialGrant).toHaveBeenCalledTimes(1));
    const expectedChallengeBytes = await crypto.subtle.digest(
      "SHA-256",
      new TextEncoder().encode(`agent-secrets/revoke/v1\n${grant.grant_id}`)
    );
    expect(bufToB64url(capturedChallenge as ArrayBuffer)).toBe(bufToB64url(expectedChallengeBytes));
    expect(revokeCredentialGrant).toHaveBeenCalledWith(grant.grant_id, {
      assertion: {
        id: "existing-key",
        rawId: bufToB64url(bufFrom("existing-key-raw-id")),
        response: {
          authenticatorData: bufToB64url(bufFrom("authenticator-data")),
          clientDataJSON: bufToB64url(bufFrom("revoke-client-data-json")),
          signature: bufToB64url(bufFrom("revoke-signature")),
        },
        type: "public-key",
      },
    });
    await waitFor(() => expect(getCredentialGrants).toHaveBeenCalledTimes(2));
    await screen.findByText("No live grants.");
  } finally {
    cleanup();
    whoAmI.mockRestore();
    getCredentialKeys.mockRestore();
    getCredentialGrants.mockRestore();
    revokeCredentialGrant.mockRestore();
  }
});

test("shows a configuration message and no controls when keys 404s FEATURE_OFF", async () => {
  const whoAmI = spyOn(api, "whoAmI").mockResolvedValue(viewer);
  const getCredentialKeys = spyOn(api, "getCredentialKeys").mockRejectedValue(
    new ApiError(404, { code: "FEATURE_OFF" })
  );

  try {
    renderPage();

    expect(
      await screen.findByText("Credential requests are not configured on this Dispatch deployment.")
    ).toBeDefined();
    expect(screen.queryByRole("button", { name: "Retry" })).toBeNull();
    expect(screen.queryByText("Register key")).toBeNull();
    expect(screen.queryByText("Live grants")).toBeNull();
  } finally {
    cleanup();
    whoAmI.mockRestore();
    getCredentialKeys.mockRestore();
  }
});
