// scripts/e2e/lib/app-logins.ts: the bot login of each Legion role App a legion.yaml configures,
// as GitHub names it today. GitHub's `GET /app`, under a JWT the App's own private key signs,
// answers the App's slug, and its bot account is `<slug>[bot]`: the login the daemon reads at boot
// for the identity each role commits, pushes and reviews as (packages/daemon/internal/appauth,
// fetchIdentity). A stage proof compares GitHub's records with these instead of with a name of its
// own, so an App renamed on GitHub changes what this prints and no script, and a name another App
// takes over later is never mistaken for Legion's. lib/workflow.sh's read_app_logins runs it.
//
//   bun scripts/e2e/lib/app-logins.ts <legion.yaml> [<GitHub API URL>]
//     prints {"implement":"<slug>[bot]","review":"<slug>[bot]"} and exits 0, or names what it
//     could not read on stderr and exits 1; without <legion.yaml> it exits 2 with its usage. The
//     API URL defaults to https://api.github.com, the one the daemon calls.
//
// Each App's key comes from its `github_apps.<role>.private_key_command`, run as the daemon runs it
// (internal/config, executePrivateKeyCommand): `sh -c`, stdin closed, the environment less the NATS
// seed variables (internal/runtime, WithoutNATSSeeds), stdout trimmed as the key, and stderr kept
// for a failure's message alone. The key stays in this process's memory: it is never printed,
// written to a file, or handed to another process, and neither is the JWT. An App configured with
// any other key source is refused, since every stage proof's file names a command.
import { spawnSync } from "node:child_process";
import { createSign } from "node:crypto";
import { readFileSync } from "node:fs";

/** The two App roles `github_apps` names (internal/appauth, Implement and Review). */
export type AppRole = "implement" | "review";
export type AppLogins = Record<AppRole, string>;

/** One App of `github_apps`: its id and the command that prints its PEM private key. */
export interface AppKeySource {
  readonly appId: string;
  readonly privateKeyCommand: string;
}

/** `value[key]` when `value` is an object that has `key`, else undefined: the one read of the
 * config's and GitHub's parsed shapes, which are outside data. */
function member(value: unknown, key: string): unknown {
  if (value === null || typeof value !== "object" || !(key in value)) return undefined;
  return Object.getOwnPropertyDescriptor(value, key)?.value;
}

/** The two Apps `github_apps` configures in the legion.yaml text `text`, or a throw naming the key
 * that is missing or not a command. */
export function appKeySources(text: string): Record<AppRole, AppKeySource> {
  const apps = member(Bun.YAML.parse(text), "github_apps");
  if (apps === undefined || apps === null || typeof apps !== "object") {
    throw new Error("the config has no github_apps");
  }
  const source = (role: AppRole): AppKeySource => {
    const app = member(apps, role);
    const appId = member(app, "app_id");
    const command = member(app, "private_key_command");
    if (typeof appId !== "string" && typeof appId !== "number") {
      throw new Error(`github_apps.${role}.app_id is not set`);
    }
    if (typeof command !== "string" || command === "") {
      throw new Error(
        `github_apps.${role} names its key without a private_key_command, the one source this reads`
      );
    }
    return { appId: String(appId), privateKeyCommand: command };
  };
  return { implement: source("implement"), review: source("review") };
}

const base64url = (data: string | Buffer): string => Buffer.from(data).toString("base64url");

/** The RS256 App JWT the daemon mints (internal/appauth, GenerateJWT): issued a minute back for
 * clock skew, ten minutes of life, the App id as its issuer. */
export function appJWT(appId: string, privateKeyPEM: string, now: Date): string {
  const seconds = Math.floor(now.getTime() / 1000);
  const input = `${base64url(JSON.stringify({ alg: "RS256", typ: "JWT" }))}.${base64url(
    JSON.stringify({ iss: appId, iat: seconds - 60, exp: seconds + 600 })
  )}`;
  const signature = createSign("RSA-SHA256").update(input).sign(privateKeyPEM);
  return `${input}.${base64url(signature)}`;
}

/** The variables holding a NATS nkey seed's value, which no key command the daemon starts sees
 * (internal/runtime, natsSeedVariables). */
const natsSeedVariables = ["NATS_NKEY_SEED", "NATS_DAEMON_NKEY_SEED"];

/** Runs one App's private_key_command and returns the key it printed, trimmed, as the daemon does.
 * A failure is named as the daemon names it (internal/config, commandError): its field and exit
 * code, then the command's stderr when it wrote any; its stdout is never quoted. */
function runKeyCommand(role: AppRole, command: string): string {
  const field = `github_apps.${role}.private_key_command`;
  const env = { ...process.env };
  for (const name of natsSeedVariables) delete env[name];
  const run = spawnSync("sh", ["-c", command], {
    encoding: "utf8",
    env,
    stdio: ["ignore", "pipe", "pipe"],
  });
  if (run.error !== undefined) throw new Error(`${field} could not run: ${run.error.message}`);
  if (run.status !== 0) {
    const stderr = run.stderr.trim();
    const failed = `${field} failed (exit ${run.status ?? -1})`;
    throw new Error(stderr === "" ? failed : `${failed}: ${stderr}`);
  }
  const key = run.stdout.trim();
  if (key === "") throw new Error(`${field} produced empty output`);
  return key;
}

/** The bot login GitHub gives the App `source` names: `<slug>[bot]`, the slug read from
 * `GET <api>/app` under the App's own JWT. */
export async function appLogin(
  role: AppRole,
  source: AppKeySource,
  api: string,
  now: () => Date = () => new Date()
): Promise<string> {
  const jwt = appJWT(source.appId, runKeyCommand(role, source.privateKeyCommand), now());
  const response = await fetch(`${api.replace(/\/+$/, "")}/app`, {
    headers: {
      Authorization: `Bearer ${jwt}`,
      Accept: "application/vnd.github+json",
      "User-Agent": "legion-e2e",
    },
    signal: AbortSignal.timeout(30_000),
  });
  const body = await response.text();
  if (!response.ok) {
    throw new Error(
      `GET /app as the ${role} App (app_id ${source.appId}) answered ${response.status}: ${body.slice(0, 300)}`
    );
  }
  let slug: unknown;
  try {
    slug = member(JSON.parse(body), "slug");
  } catch {
    slug = undefined;
  }
  if (typeof slug !== "string" || slug === "") {
    throw new Error(
      `GET /app as the ${role} App (app_id ${source.appId}) answered no slug: ${body.slice(0, 300)}`
    );
  }
  return `${slug}[bot]`;
}

/** Both Apps' bot logins, refusing a config whose two roles resolve to one App, which the daemon
 * refuses at boot too (internal/daemon, mintAtBoot). */
export async function appLogins(configPath: string, api: string): Promise<AppLogins> {
  const sources = appKeySources(readFileSync(configPath, "utf8"));
  const logins: AppLogins = {
    implement: await appLogin("implement", sources.implement, api),
    review: await appLogin("review", sources.review, api),
  };
  if (logins.implement === logins.review) {
    throw new Error(`both github_apps roles are the App ${logins.review}: the workflow needs two`);
  }
  return logins;
}

if (import.meta.main) {
  const [configPath, api = "https://api.github.com"] = process.argv.slice(2);
  if (configPath === undefined || configPath === "") {
    process.stderr.write(
      "usage: bun scripts/e2e/lib/app-logins.ts <legion.yaml> [<GitHub API URL>]\n"
    );
    process.exit(2);
  }
  try {
    process.stdout.write(`${JSON.stringify(await appLogins(configPath, api))}\n`);
  } catch (error) {
    process.stderr.write(`app-logins: ${error instanceof Error ? error.message : String(error)}\n`);
    process.exitCode = 1;
  }
}
