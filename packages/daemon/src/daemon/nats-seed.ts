import { nkeys } from "nats";

/** The part of nats.js's `nkeys` export this module uses; nats.js types the export `any`. */
const seedKeys: { fromSeed(seed: Uint8Array): { getPublicKey(): string } } = nkeys;

/** Refuses a seed that is not an nkey user seed, naming `source` (the key or variable it came
 * from, and the file's path when it came from one) and never the seed. */
export function validateNatsUserSeed(seed: string, source: string): void {
  let publicKey: string;
  try {
    publicKey = natsUserPublicKey(seed);
  } catch (error) {
    throw new Error(
      `${source} does not hold a valid nkey seed: ${error instanceof Error ? error.message : String(error)}`
    );
  }
  if (!publicKey.startsWith("U")) {
    throw new Error(`${source} holds an nkey seed that is not a user's (public key ${publicKey})`);
  }
}

/** The public key of the nkey user `seed` (a seed `validateNatsUserSeed` accepted) is the seed
 * of: what the daemon may say about a seed it holds without the seed leaving it. */
export function natsUserPublicKey(seed: string): string {
  return seedKeys.fromSeed(new TextEncoder().encode(seed)).getPublicKey();
}
