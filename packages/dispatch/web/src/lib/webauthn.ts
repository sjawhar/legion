import type {
  AuthenticationResponseJSON,
  PublicKeyCredentialCreationOptionsJSON,
  PublicKeyCredentialRequestOptionsJSON,
  RegistrationResponseJSON,
} from "../api/types";

/** Standard base64url (RFC 4648 §5), unpadded: the `-`/`_` alphabet the broker's go-webauthn
 *  JSON uses for every challenge, id, and response field. */
export function b64urlToBuf(s: string): ArrayBuffer {
  const standard = s.replaceAll("-", "+").replaceAll("_", "/");
  const padded = standard.padEnd(standard.length + ((4 - (standard.length % 4)) % 4), "=");
  const binary = atob(padded);
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i += 1) {
    bytes[i] = binary.charCodeAt(i);
  }
  return bytes.buffer as ArrayBuffer;
}

export function bufToB64url(b: ArrayBuffer): string {
  const bytes = new Uint8Array(b);
  let binary = "";
  for (const byte of bytes) {
    binary += String.fromCharCode(byte);
  }
  return btoa(binary).replaceAll("+", "-").replaceAll("/", "_").replaceAll("=", "");
}

/** Runs a WebAuthn assertion ceremony against a broker-issued challenge and maps the browser's
 *  `PublicKeyCredential` into the exact `AuthenticationResponseJSON` wire shape the broker's
 *  go-webauthn parser expects. `allowCredentials` is required whenever the broker's ceremony
 *  names one (contract v9's endorse/begin route, whose `publicKey` is a full
 *  `PublicKeyCredentialRequestOptionsJSON`): registration explicitly requests non-resident
 *  (`residentKey: "discouraged"`) keys, so without it a real hardware authenticator has no
 *  credential id to locate and the ceremony fails. The approve/deny/revoke routes' bare-string
 *  challenges carry no such list - those ceremonies rely on the browser enumerating whichever
 *  discoverable credential the caller injected, and omit the field entirely, exactly as before. */
export async function getAssertion(
  challengeB64url: string,
  rpId: string,
  allowCredentials?: PublicKeyCredentialRequestOptionsJSON["allowCredentials"]
): Promise<AuthenticationResponseJSON> {
  const credential = (await navigator.credentials.get({
    publicKey: {
      challenge: b64urlToBuf(challengeB64url),
      rpId,
      userVerification: "required",
      ...(allowCredentials === undefined
        ? {}
        : {
            allowCredentials: allowCredentials.map((entry) => ({
              id: b64urlToBuf(entry.id),
              type: entry.type as "public-key",
            })),
          }),
    },
  })) as PublicKeyCredential | null;

  if (credential === null) {
    throw new Error("No credential returned for the assertion ceremony");
  }

  const response = credential.response as AuthenticatorAssertionResponse;
  return {
    id: credential.id,
    rawId: bufToB64url(credential.rawId),
    response: {
      authenticatorData: bufToB64url(response.authenticatorData),
      clientDataJSON: bufToB64url(response.clientDataJSON),
      signature: bufToB64url(response.signature),
    },
    type: "public-key",
  };
}

/** Runs a WebAuthn registration ceremony from broker-issued creation options and maps the
 *  browser's `PublicKeyCredential` into the exact `RegistrationResponseJSON` wire shape the
 *  broker's go-webauthn parser expects. */
export async function createCredential(
  options: PublicKeyCredentialCreationOptionsJSON
): Promise<RegistrationResponseJSON> {
  // The wire JSON keeps `attestation`/`authenticatorSelection.userVerification` as plain
  // strings (api/types.ts is deliberately permissive rather than mirroring every DOM union
  // literal); the browser itself rejects a malformed value at the `create` call below.
  const publicKey = {
    ...options,
    challenge: b64urlToBuf(options.challenge),
    excludeCredentials: options.excludeCredentials?.map((credential) => ({
      ...credential,
      id: b64urlToBuf(credential.id),
    })),
    user: {
      ...options.user,
      id: b64urlToBuf(options.user.id),
    },
  } as PublicKeyCredentialCreationOptions;

  const credential = (await navigator.credentials.create({
    publicKey,
  })) as PublicKeyCredential | null;

  if (credential === null) {
    throw new Error("No credential returned for the registration ceremony");
  }

  const response = credential.response as AuthenticatorAttestationResponse;
  return {
    id: credential.id,
    rawId: bufToB64url(credential.rawId),
    response: {
      attestationObject: bufToB64url(response.attestationObject),
      clientDataJSON: bufToB64url(response.clientDataJSON),
    },
    type: "public-key",
  };
}
