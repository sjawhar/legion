import { nkeys } from "nats";

/** The part of nats.js's `nkeys` export this module uses; nats.js types the export `any`. */
const seedKeys: { fromSeed(seed: Uint8Array): { getPublicKey(): string } } = nkeys;

/** Refuses a seed that is not an nkey user seed, naming `source` (the key or variable it came
 * from, and the file's path when it came from one) and never the seed. */
export function validateNatsUserSeed(seed: string, source: string): void {
  let publicKey: string;
  try {
    publicKey = seedKeys.fromSeed(new TextEncoder().encode(seed)).getPublicKey();
  } catch (error) {
    throw new Error(
      `${source} does not hold a valid nkey seed: ${error instanceof Error ? error.message : String(error)}`
    );
  }
  if (!publicKey.startsWith("U")) {
    throw new Error(`${source} holds an nkey seed that is not a user's (public key ${publicKey})`);
  }
}
