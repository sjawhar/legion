import { type Authenticator, nkeyAuthenticator, nkeys } from "nats";
import type { NatsSeed } from "./envoy-settings";
import { messageFor } from "./errors";

/** The part of nats.js's `nkeys` export this module uses; nats.js types the export `any`. */
const seedKeys: { fromSeed(seed: Uint8Array): { getPublicKey(): string } } = nkeys;

/** The `connect` options naming the NATS user a client connects as, from its resolved seed
 * (`envoySettings`, or `natsSeedFromEnvironment`). No seed is `{}`: the connection carries no
 * credential, as every connection did before servers required one. A seed that is not a user nkey
 * seed throws naming its source; no error carries the seed. */
export function natsAuthOptions(seed: NatsSeed | undefined): {
  readonly authenticator?: Authenticator;
} {
  if (seed === undefined) return {};
  const bytes = new TextEncoder().encode(seed.value);
  let publicKey: string;
  try {
    publicKey = seedKeys.fromSeed(bytes).getPublicKey();
  } catch (error) {
    throw new Error(`${seed.source} does not hold a valid nkey seed: ${messageFor(error)}`);
  }
  if (!publicKey.startsWith("U")) {
    throw new Error(
      `${seed.source} holds an nkey seed that is not a user's (public key ${publicKey})`
    );
  }
  return { authenticator: nkeyAuthenticator(bytes) };
}
