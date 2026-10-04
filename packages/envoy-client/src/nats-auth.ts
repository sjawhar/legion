import { type Authenticator, nkeyAuthenticator, nkeys } from "nats";
import { messageFor } from "./errors";
import { readSecretFile } from "./secret-file";

/** The part of nats.js's `nkeys` export this module uses; nats.js types the export `any`. */
const seedKeys: { fromSeed(seed: Uint8Array): { getPublicKey(): string } } = nkeys;

/** The `connect` options naming the NATS user a client connects as, from its environment: the
 * trimmed contents of the file `NATS_NKEY_SEED_FILE` names, else `NATS_NKEY_SEED`. A set variable
 * is authoritative, and the file pointer wins over the seed: an empty pointer, or a missing,
 * unreadable or blank file, throws naming the variable and the path (`readSecretFile`), never a
 * fallback to `NATS_NKEY_SEED` or to no credential; so does a blank `NATS_NKEY_SEED`, and a seed
 * that is not a user nkey seed. Neither set is `{}`: the connection carries no credential, as
 * every connection did before servers required one. No error carries the seed. The Go clients
 * read the same two variables (`packages/envoy/internal/bus/nkey.go`,
 * `packages/daemon/internal/natsauth`). */
export function natsAuthOptions(env: NodeJS.ProcessEnv): {
  readonly authenticator?: Authenticator;
} {
  const { NATS_NKEY_SEED_FILE: file, NATS_NKEY_SEED: plain } = env;
  let seed: string;
  let source: string;
  if (file !== undefined) {
    if (file === "") throw new Error("NATS_NKEY_SEED_FILE is set but empty");
    seed = readSecretFile("NATS_NKEY_SEED_FILE", file);
    source = `NATS_NKEY_SEED_FILE (${file})`;
  } else if (plain !== undefined) {
    seed = plain.trim();
    if (seed.length === 0) throw new Error("NATS_NKEY_SEED is set but empty");
    source = "NATS_NKEY_SEED";
  } else {
    return {};
  }
  const bytes = new TextEncoder().encode(seed);
  let publicKey: string;
  try {
    publicKey = seedKeys.fromSeed(bytes).getPublicKey();
  } catch (error) {
    throw new Error(`${source} does not hold a valid nkey seed: ${messageFor(error)}`);
  }
  if (!publicKey.startsWith("U")) {
    throw new Error(`${source} holds an nkey seed that is not a user's (public key ${publicKey})`);
  }
  return { authenticator: nkeyAuthenticator(bytes) };
}
