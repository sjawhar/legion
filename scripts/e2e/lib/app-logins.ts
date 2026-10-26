// scripts/e2e/lib/app-logins.ts: the bot login of each Legion role App a legion.yaml configures,
// as GitHub names it today, and the proof that each App's key is the key of the App id configured
// for it. GitHub's `GET /app`, under a JWT the App's own private key signs, answers the App's id
// and slug, and its bot account is `<slug>[bot]`: the login the daemon reads at boot for the
// identity each role commits, pushes and reviews as (packages/daemon/internal/appauth,
// fetchIdentity). A stage proof compares GitHub's records with these instead of with a name of its
// own, so an App renamed on GitHub changes what this prints and no script, and a name another App
// takes over later is never mistaken for Legion's. lib/github-apps.sh's check_github_apps runs it.
//
//   bun scripts/e2e/lib/app-logins.ts [--roles <role>[,<role>]] <legion.yaml> [<GitHub API URL>]
//     prints {"implement":"<slug>[bot]","review":"<slug>[bot]"} (the roles --roles names, both by
//     default) and exits 0, or names what it could not read or prove on stderr and exits 1; without
//     <legion.yaml>, or with a --roles it does not know, it exits 2 with its usage. The API URL
//     defaults to https://api.github.com, the one the daemon calls. An answer whose App id is not
//     the configured app_id is refused, naming both ids, and so is an App without the write
//     permission its role's work needs (roleWrites), which also refuses the two Apps swapped whole.
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
export type AppLogins = Partial<Record<AppRole, string>>;
const appRoles: readonly AppRole[] = ["implement", "review"];
/** The App permission each role's work cannot run without: the implement App pushes the issue's
 * branch, and the review App posts the tester's and architect's check runs. The implement App
 * holds `checks: read` alone, so the review role configured with it, the two swapped whole, is
 * refused here rather than hours later at the first check run. `GET /app` answers the App's
 * declared permissions, not one installation's granted set, so this catches a wrong App and not
 * an installation that has not accepted a permission the App has since asked for. */
const roleWrites: Record<AppRole, string> = { implement: "contents", review: "checks" };

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

/** The Apps of `roles` that `github_apps` configures in the legion.yaml text `text`, or a throw
 * naming the key that is missing or not a command. */
export function appKeySources(
  text: string,
  roles: readonly AppRole[] = appRoles
): Partial<Record<AppRole, AppKeySource>> {
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
  return Object.fromEntries(roles.map((role) => [role, source(role)]));
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
export function runKeyCommand(role: AppRole, command: string): string {
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
 * `GET <api>/app` under the App's own JWT. Every failure names the App by its role and id: a
 * GitHub that never answered, an answer that is not 2xx, one whose App id is not `source.appId`
 * (naming both ids), one without its role's write permission (roleWrites), and one with no slug. */
export async function appLogin(
  role: AppRole,
  source: AppKeySource,
  api: string,
  now: () => Date = () => new Date()
): Promise<string> {
  const read = `GET /app as the ${role} App (app_id ${source.appId})`;
  const jwt = appJWT(source.appId, runKeyCommand(role, source.privateKeyCommand), now());
  let response: Response;
  let body: string;
  try {
    response = await fetch(`${api.replace(/\/+$/, "")}/app`, {
      headers: {
        Authorization: `Bearer ${jwt}`,
        Accept: "application/vnd.github+json",
        "User-Agent": "legion-e2e",
      },
      signal: AbortSignal.timeout(30_000),
    });
    body = await response.text();
  } catch (error) {
    throw new Error(`${read} failed: ${error instanceof Error ? error.message : String(error)}`);
  }
  if (!response.ok) {
    throw new Error(`${read} answered ${response.status}: ${body.slice(0, 300)}`);
  }
  let answer: unknown;
  try {
    answer = JSON.parse(body);
  } catch {
    answer = undefined;
  }
  const id = member(answer, "id");
  if (typeof id !== "number" && typeof id !== "string") {
    throw new Error(`${read} answered no App id: ${body.slice(0, 300)}`);
  }
  if (String(id) !== source.appId) {
    throw new Error(
      `${read} answered App id ${id}, want ${source.appId}: the key is another App's`
    );
  }
  const permission = roleWrites[role];
  const granted = member(member(answer, "permissions"), permission);
  if (granted !== "write" && granted !== "admin") {
    throw new Error(
      `${read} answered permissions.${permission} ${JSON.stringify(granted ?? null)}, want write: the ${role} role's work needs it, so the key is not the ${role} App's`
    );
  }
  const slug = member(answer, "slug");
  if (typeof slug !== "string" || slug === "") {
    throw new Error(`${read} answered no slug: ${body.slice(0, 300)}`);
  }
  return `${slug}[bot]`;
}

/** The bot logins of the Apps of `roles`, refusing a config whose two roles resolve to one App,
 * which the daemon refuses at boot too (internal/daemon, mintAtBoot). */
export async function appLogins(
  configPath: string,
  api: string,
  roles: readonly AppRole[] = appRoles
): Promise<AppLogins> {
  const sources = appKeySources(readFileSync(configPath, "utf8"), roles);
  const logins: AppLogins = {};
  for (const role of roles) {
    const source = sources[role];
    if (source !== undefined) logins[role] = await appLogin(role, source, api);
  }
  if (logins.implement !== undefined && logins.implement === logins.review) {
    throw new Error(`both github_apps roles are the App ${logins.review}: the workflow needs two`);
  }
  return logins;
}

/** The roles a `--roles` value names, comma-separated, each once; undefined for any other value. */
function parseRoles(value: string): AppRole[] | undefined {
  const roles = value.split(",");
  const known = (role: string): role is AppRole => (appRoles as readonly string[]).includes(role);
  if (!roles.every(known) || new Set(roles).size !== roles.length) return undefined;
  return roles.filter(known);
}

if (import.meta.main) {
  const args = process.argv.slice(2);
  let roles: AppRole[] | undefined = [...appRoles];
  if (args[0] === "--roles") {
    roles = parseRoles(args[1] ?? "");
    args.splice(0, 2);
  }
  const [configPath, api = "https://api.github.com"] = args;
  if (roles === undefined || configPath === undefined || configPath === "") {
    process.stderr.write(
      "usage: bun scripts/e2e/lib/app-logins.ts [--roles <role>[,<role>]] <legion.yaml> [<GitHub API URL>]\n"
    );
    process.exit(2);
  }
  try {
    process.stdout.write(`${JSON.stringify(await appLogins(configPath, api, roles))}\n`);
  } catch (error) {
    process.stderr.write(`app-logins: ${error instanceof Error ? error.message : String(error)}\n`);
    process.exitCode = 1;
  }
}
