/**
 * The NATS server a test stack runs when its listeners must hold the grant a hub carried *before*
 * session consumers: `infra/core.ListenerPermissions` (packages/envoy/infra/core/permissions.go)
 * without the session-consumer entries and without the shared role-lane queue, which is the same
 * grant `packages/envoy/cmd/listener/session_delivery_test.go`'s
 * `listenerGrantBeforeSessionConsumers()` holds a listener to. A listener on it answers every
 * registration that asks for a consumer `{"mode": "core", "reason": "no_grant"}` and keeps the
 * core path, which is what every hub runs until its NATS user is replaced.
 *
 * The entries left out, in `ListenerPermissions`' order:
 *
 *   $JS.API.CONSUMER.CREATE.ENVOY_NOTIFICATIONS.*     the session consumers' create, with and
 *   $JS.API.CONSUMER.CREATE.ENVOY_NOTIFICATIONS.*.>   without a filter subject in the subject,
 *   $JS.API.CONSUMER.INFO.ENVOY_NOTIFICATIONS.*       their info, delete, pull and
 *   $JS.API.CONSUMER.DELETE.ENVOY_NOTIFICATIONS.*     acknowledgements, and the stream's
 *   $JS.API.CONSUMER.MSG.NEXT.ENVOY_NOTIFICATIONS.*   consumer list
 *   $JS.API.CONSUMER.LIST.ENVOY_NOTIFICATIONS
 *   $JS.ACK.ENVOY_NOTIFICATIONS.*.>
 *   notifications.role.> envoy-roles                  (subscribe) the shared role-lane queue
 *
 * The listener's own durable keeps its create, info and acknowledgements, so the hub keeps its
 * durable and its sessions keep core delivery, and the role lane falls back to the machine groups.
 *
 * A grant that drifted from the real one is caught by the tests that use it: a missing entry the
 * listener needs keeps the listener from starting, and a session-consumer entry left in has it
 * answer `consumer`, which the core-path scenario refuses.
 *
 * As a command, for a shell stack (scripts/e2e/envoy-session-delivery.sh):
 *
 *   bun packages/envoy-client/test/listener-grant.ts --config <path> --seed-dir <dir>
 *       --machine <id> [--machine <id>...]
 *
 * which writes the server's configuration to `--config` (public keys only) and each machine's
 * listener seed to `<dir>/<machine>.seed`, readable by its owner alone. The seed is never printed.
 */
import { chmodSync, mkdirSync, writeFileSync } from "node:fs";
import * as path from "node:path";
import { nkeys } from "nats";

/** The stream, buckets and names of `packages/envoy/internal/bus` the grant is built from. */
const STREAM = "ENVOY_NOTIFICATIONS";
const INTEREST_BUCKET = "envoy_interests";
const BUCKETS = [INTEREST_BUCKET, "envoy_roles", "envoy_sessions", "envoy_ci_state"];

/** One NATS user's rules, as the server's authorization block takes them. */
export interface NatsGrant {
  readonly publish: readonly string[];
  readonly subscribe: readonly string[];
}

/** The calls nats.go's KV makes on one bucket, in `bucketPermissions`' order. */
function bucketPermissions(bucket: string): string[] {
  const stream = `KV_${bucket}`;
  return [
    `$JS.API.STREAM.INFO.${stream}`,
    `$JS.API.STREAM.CREATE.${stream}`,
    `$JS.API.STREAM.UPDATE.${stream}`,
    `$JS.API.DIRECT.GET.${stream}.>`,
    `$JS.API.STREAM.MSG.GET.${stream}`,
    `$KV.${bucket}.>`,
    `$JS.API.CONSUMER.CREATE.${stream}.>`,
    `$JS.API.CONSUMER.DELETE.${stream}.>`,
    `$JS.FC.${stream}.>`,
  ];
}

/** The listener's grant as a hub carries it before session consumers, for machine `machineID`. */
export function listenerGrantBeforeSessionConsumers(machineID: string): NatsGrant {
  const durable = `listener-${machineID}`;
  const publish = [
    "notifications.>",
    `$JS.API.STREAM.INFO.${STREAM}`,
    `$JS.API.STREAM.CREATE.${STREAM}`,
    `$JS.API.STREAM.UPDATE.${STREAM}`,
    `$JS.API.STREAM.MSG.GET.${STREAM}`,
    `$JS.API.CONSUMER.INFO.${STREAM}.${durable}`,
    `$JS.API.CONSUMER.CREATE.${STREAM}.${durable}`,
    `$JS.ACK.${STREAM}.${durable}.>`,
    "$JS.API.INFO",
  ];
  for (const bucket of BUCKETS) publish.push(...bucketPermissions(bucket));
  publish.push(`$JS.API.STREAM.PURGE.KV_${INTEREST_BUCKET}`);
  return {
    publish,
    subscribe: ["_INBOX.>", `notifications.role.> envoy-listener-${machineID}`],
  };
}

/** A NATS server's configuration and the seeds its listener users connect with. */
export interface PreConsumerNatsServer {
  /** The server's configuration text: the authorization block and `no_auth_user`. */
  readonly config: string;
  /** Each machine id's listener seed, the value of its `NATS_NKEY_SEED_FILE`. */
  readonly seeds: ReadonlyMap<string, string>;
}

/** One user's line of the authorization block: a comment naming it, then its JSON object. */
function userLines(name: string, publicKey: string, grant: NatsGrant): string[] {
  const user = {
    nkey: publicKey,
    permissions: { publish: { allow: grant.publish }, subscribe: { allow: grant.subscribe } },
  };
  return [`    # ${name}`, `    ${JSON.stringify(user)}`];
}

/**
 * The configuration of a NATS server (one global account, JetStream as natsd turns it on) whose
 * listener of each machine id holds `listenerGrantBeforeSessionConsumers` and whose every other
 * client — sessions, Dispatch, a test's own connection — presents no credential and becomes the
 * `everyone` user, which may publish and subscribe to anything.
 */
export function preConsumerNatsServer(machineIDs: readonly string[]): PreConsumerNatsServer {
  if (machineIDs.length === 0) throw new Error("a pre-consumer NATS server needs a machine id");
  const seeds = new Map<string, string>();
  const lines = [
    "# Rendered by packages/envoy-client/test/listener-grant.ts: each listener holds the grant a",
    "# hub carries before session consumers. Public keys only.",
    "authorization {",
    "  users: [",
  ];
  for (const machineID of machineIDs) {
    const user = nkeys.createUser();
    seeds.set(machineID, new TextDecoder().decode(user.getSeed()));
    lines.push(
      ...userLines(
        `listener-${machineID}`,
        user.getPublicKey(),
        listenerGrantBeforeSessionConsumers(machineID)
      )
    );
  }
  const everyone = nkeys.createUser();
  const everyoneKey: string = everyone.getPublicKey();
  lines.push(
    ...userLines("everyone", everyoneKey, { publish: [">"], subscribe: [">"] }),
    "  ]",
    "}",
    "# everyone: a client presenting no credential becomes this user",
    `no_auth_user: ${JSON.stringify(everyoneKey)}`,
    ""
  );
  return { config: lines.join("\n"), seeds };
}

/** `--config <path> --seed-dir <dir> --machine <id>...`, as the command takes them. */
function parseArguments(argv: readonly string[]): {
  config: string;
  seedDir: string;
  machines: string[];
} {
  let config = "";
  let seedDir = "";
  const machines: string[] = [];
  for (let i = 0; i < argv.length; i += 2) {
    const flag = argv[i];
    const value = argv[i + 1];
    if (value === undefined) throw new Error(`${flag} needs a value`);
    if (flag === "--config") config = value;
    else if (flag === "--seed-dir") seedDir = value;
    else if (flag === "--machine") machines.push(value);
    else throw new Error(`unknown argument ${flag}`);
  }
  if (config === "" || seedDir === "" || machines.length === 0) {
    throw new Error("--config, --seed-dir and at least one --machine are required");
  }
  return { config, seedDir, machines };
}

if (import.meta.main) {
  const { config, seedDir, machines } = parseArguments(process.argv.slice(2));
  const server = preConsumerNatsServer(machines);
  mkdirSync(path.dirname(config), { recursive: true });
  writeFileSync(config, server.config, { mode: 0o600 });
  mkdirSync(seedDir, { recursive: true });
  for (const [machineID, seed] of server.seeds) {
    const file = path.join(seedDir, `${machineID}.seed`);
    writeFileSync(file, `${seed}\n`, { mode: 0o600 });
    // `mode` applies to a file this call creates; a seed file of an earlier run keeps its own.
    chmodSync(file, 0o600);
  }
  console.log(config);
}
