import { expect, test } from "bun:test";

import { b64urlToBuf, bufToB64url, createCredential, getAssertion } from "./webauthn";

// RFC 4648 §10's test vectors, re-encoded as unpadded base64url (`+`/`/` never appear in these
// vectors, so standard base64 and base64url agree byte-for-byte on them).
const RFC_4648_VECTORS: ReadonlyArray<{ bytes: string; b64url: string }> = [
  { bytes: "", b64url: "" },
  { bytes: "f", b64url: "Zg" },
  { bytes: "fo", b64url: "Zm8" },
  { bytes: "foo", b64url: "Zm9v" },
  { bytes: "foob", b64url: "Zm9vYg" },
  { bytes: "fooba", b64url: "Zm9vYmE" },
  { bytes: "foobar", b64url: "Zm9vYmFy" },
];

function bufFrom(text: string): ArrayBuffer {
  return new TextEncoder().encode(text).buffer as ArrayBuffer;
}

function bufToText(buf: ArrayBuffer): string {
  return new TextDecoder().decode(buf);
}

test("b64urlToBuf decodes the RFC 4648 test vectors byte-exact", () => {
  for (const { bytes, b64url } of RFC_4648_VECTORS) {
    expect(bufToText(b64urlToBuf(b64url))).toBe(bytes);
  }
});

test("bufToB64url encodes the RFC 4648 test vectors byte-exact", () => {
  for (const { bytes, b64url } of RFC_4648_VECTORS) {
    expect(bufToB64url(bufFrom(bytes))).toBe(b64url);
  }
});

test("b64urlToBuf/bufToB64url round-trip a 32-byte all-zero digest", () => {
  const zeroes = new Uint8Array(32);
  const encoded = bufToB64url(zeroes.buffer as ArrayBuffer);
  expect(encoded).not.toContain("+");
  expect(encoded).not.toContain("/");
  expect(encoded).not.toContain("=");
  expect(new Uint8Array(b64urlToBuf(encoded))).toEqual(zeroes);
});

test("b64urlToBuf/bufToB64url round-trip a 32-byte random digest", () => {
  const random = new Uint8Array(32);
  for (let i = 0; i < random.length; i += 1) {
    random[i] = (i * 37 + 11) % 256;
  }
  const encoded = bufToB64url(random.buffer as ArrayBuffer);
  expect(new Uint8Array(b64urlToBuf(encoded))).toEqual(random);
});

test("bufToB64url never emits standard-base64 characters or padding", () => {
  // A buffer whose standard base64 encoding is known to contain `+`, `/`, and `=`.
  const bytes = Uint8Array.from([0xfb, 0xff, 0xbf]);
  const encoded = bufToB64url(bytes.buffer as ArrayBuffer);
  expect(encoded).toBe("-_-_");
});

function stubCredentials(fake: {
  get?: (options: unknown) => Promise<unknown>;
  create?: (options: unknown) => Promise<unknown>;
}): void {
  Object.defineProperty(navigator, "credentials", { configurable: true, value: fake });
}

test("getAssertion maps a stubbed navigator.credentials.get result into AuthenticationResponseJSON", async () => {
  const challenge = bufToB64url(bufFrom("assertion-challenge"));
  let capturedOptions: PublicKeyCredentialRequestOptions | undefined;
  stubCredentials({
    get: async (options) => {
      // The stub's own input shape - the DOM lib type for a `credentials.get` call.
      const requestOptions = options as { publicKey: PublicKeyCredentialRequestOptions };
      capturedOptions = requestOptions.publicKey;
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

  const result = await getAssertion(challenge, "dispatch.example");

  expect(capturedOptions?.rpId).toBe("dispatch.example");
  expect(capturedOptions?.userVerification).toBe("required");
  const capturedChallenge = capturedOptions?.challenge as ArrayBuffer;
  expect(new Uint8Array(capturedChallenge)).toEqual(new Uint8Array(b64urlToBuf(challenge)));
  expect(result).toEqual({
    id: "credential-id",
    rawId: bufToB64url(bufFrom("raw-id-bytes")),
    response: {
      authenticatorData: bufToB64url(bufFrom("authenticator-data")),
      clientDataJSON: bufToB64url(bufFrom("client-data-json")),
      signature: bufToB64url(bufFrom("signature-bytes")),
    },
    type: "public-key",
  });
});

test("getAssertion throws when navigator.credentials.get resolves null", async () => {
  stubCredentials({ get: async () => null });

  await expect(getAssertion(bufToB64url(bufFrom("c")), "dispatch.example")).rejects.toThrow();
});

test("getAssertion decodes and forwards allowCredentials, when supplied, to navigator.credentials.get", async () => {
  const challenge = bufToB64url(bufFrom("assertion-challenge"));
  const existingKeyId = bufToB64url(bufFrom("existing-key-raw-id"));
  let capturedOptions: PublicKeyCredentialRequestOptions | undefined;
  stubCredentials({
    get: async (options) => {
      const requestOptions = options as { publicKey: PublicKeyCredentialRequestOptions };
      capturedOptions = requestOptions.publicKey;
      return {
        id: "existing-key",
        rawId: bufFrom("existing-key-raw-id"),
        response: {
          authenticatorData: bufFrom("authenticator-data"),
          clientDataJSON: bufFrom("client-data-json"),
          signature: bufFrom("signature-bytes"),
        },
        type: "public-key",
      };
    },
  });

  await getAssertion(challenge, "dispatch.example", [{ id: existingKeyId, type: "public-key" }]);

  expect(capturedOptions?.allowCredentials).toHaveLength(1);
  const capturedEntry = capturedOptions?.allowCredentials?.[0];
  expect(capturedEntry?.type).toBe("public-key");
  expect(new Uint8Array(capturedEntry?.id as ArrayBuffer)).toEqual(
    new Uint8Array(b64urlToBuf(existingKeyId))
  );
});

test("getAssertion omits allowCredentials entirely when none is supplied", async () => {
  let capturedOptions: PublicKeyCredentialRequestOptions | undefined;
  stubCredentials({
    get: async (options) => {
      const requestOptions = options as { publicKey: PublicKeyCredentialRequestOptions };
      capturedOptions = requestOptions.publicKey;
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

  await getAssertion(bufToB64url(bufFrom("c")), "dispatch.example");

  expect(capturedOptions?.allowCredentials).toBeUndefined();
});

test("createCredential maps a stubbed navigator.credentials.create result into RegistrationResponseJSON", async () => {
  const options = {
    attestation: "none",
    challenge: bufToB64url(bufFrom("registration-challenge")),
    pubKeyCredParams: [{ alg: -7, type: "public-key" as const }],
    rp: { name: "Dispatch" },
    user: {
      displayName: "Architect",
      id: bufToB64url(bufFrom("user-id-bytes")),
      name: "architect",
    },
  };
  let capturedOptions: PublicKeyCredentialCreationOptions | undefined;
  stubCredentials({
    create: async (input) => {
      // The stub's own input shape - the DOM lib type for a `credentials.create` call.
      const creationOptions = input as { publicKey: PublicKeyCredentialCreationOptions };
      capturedOptions = creationOptions.publicKey;
      return {
        id: "new-credential-id",
        rawId: bufFrom("new-raw-id"),
        response: {
          attestationObject: bufFrom("attestation-object"),
          clientDataJSON: bufFrom("registration-client-data"),
        },
        type: "public-key",
      };
    },
  });

  const result = await createCredential(options);

  const capturedChallenge = capturedOptions?.challenge as ArrayBuffer;
  expect(new Uint8Array(capturedChallenge)).toEqual(new Uint8Array(b64urlToBuf(options.challenge)));
  const capturedUserId = capturedOptions?.user.id as ArrayBuffer;
  expect(new Uint8Array(capturedUserId)).toEqual(new Uint8Array(b64urlToBuf(options.user.id)));
  expect(result).toEqual({
    id: "new-credential-id",
    rawId: bufToB64url(bufFrom("new-raw-id")),
    response: {
      attestationObject: bufToB64url(bufFrom("attestation-object")),
      clientDataJSON: bufToB64url(bufFrom("registration-client-data")),
    },
    type: "public-key",
  });
});
