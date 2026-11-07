// scripts/e2e/lib/app-installation.ts: whether each Legion role App a legion.yaml configures is
// installed on a repository, and the installation tokens a live proof's own GitHub writes act
// under. A live proof that works on a repository through the Apps checks the installation before
// it relies on one there.
//
//   bun scripts/e2e/lib/app-installation.ts --roles <role>[,<role>] <legion.yaml> <owner/repo> [<GitHub API URL>]
//     prints {"<role>":{"installed":true,"installation":<id>,"account":"<login>"}} or, for an App
//     the repository lacks, {"<role>":{"installed":false,"reason":"<why>"}}, and exits 0; it names
//     what it could not read on stderr and exits 1, and exits 2 with its usage.
//
// Every credential comes from the role's `github_apps.<role>.token_command`, the one source
// lib/app-logins.ts reads too, run as the daemon runs it (internal/appauth, runTokenCommand):
// `sh -c`, stdin closed, the environment less the NATS seed variables, with LEGION_GITHUB_OWNER
// the owner the token is for and LEGION_GITHUB_READ_ONLY set for a read-only one. The release's
// token stays in this process: it is never printed, written or handed on, and a command's stderr
// reaches this process's stderr with every GitHub token form replaced (lib/app-logins.ts,
// redactTokens). There is no key and no JWT: an installation's own token answers
// `GET /installation/repositories`, which names exactly the repositories the installation is on.
import { readFileSync } from "node:fs";
import {
  type AppRole,
  type AppTokenSource,
  appTokenSources,
  firstInstallationToken,
  redactTokens,
} from "./app-logins";

const usage =
  "usage: bun scripts/e2e/lib/app-installation.ts --roles <role>[,<role>] <legion.yaml> <owner/repo> [<GitHub API URL>]";

/** The most a token command may print on either stream, and how long it may run: the bounds
 * lib/app-logins.ts holds its own run of the same command to. */
const outputLimit = 1 << 20;
const commandLimitMs = 35 * 60 * 1000;
/** The variables holding a NATS nkey seed's value, which no command the daemon starts sees
 * (internal/runtime, natsSeedVariables). */
const natsSeedVariables = ["NATS_NKEY_SEED", "NATS_DAEMON_NKEY_SEED"];

/** `value[key]` when `value` is an object that has `key`, else undefined: the one read of the
 * release's and GitHub's parsed shapes, which are outside data. */
function member(value: unknown, key: string): unknown {
  if (value === null || typeof value !== "object" || !(key in value)) return undefined;
  return Object.getOwnPropertyDescriptor(value, key)?.value;
}

/** Runs one App's token_command for `owner` and returns its stdout, the secrets broker's release.
 * A failure names the field and the command's exit code, then its stderr when it wrote any; the
 * stdout, which holds tokens, is never quoted. */
async function runTokenCommand(
  field: string,
  command: string,
  owner: string,
  readOnly: boolean
): Promise<string> {
  const env: Record<string, string | undefined> = { ...process.env };
  for (const name of natsSeedVariables) delete env[name];
  env.LEGION_GITHUB_OWNER = owner;
  env.LEGION_GITHUB_READ_ONLY = readOnly ? "1" : "";
  // detached: the command leads a process group of its own, which a kill ends whole.
  const child = Bun.spawn(["sh", "-c", command], {
    env,
    stdin: "ignore",
    stdout: "pipe",
    stderr: "pipe",
    detached: true,
  });
  let ended = "";
  const end = (why: string) => {
    if (ended !== "") return;
    ended = why;
    try {
      process.kill(-child.pid, "SIGKILL");
    } catch {
      child.kill("SIGKILL");
    }
  };
  const deadline = setTimeout(
    () => end(`did not finish within ${commandLimitMs / 60000} minutes`),
    commandLimitMs
  );
  const read = async (stream: ReadableStream<Uint8Array>, overflow: string): Promise<string> => {
    const chunks: Uint8Array[] = [];
    let size = 0;
    for await (const chunk of stream) {
      size += chunk.length;
      if (size > outputLimit) {
        end(overflow);
        continue;
      }
      chunks.push(chunk);
    }
    return Buffer.concat(chunks).toString("utf8");
  };
  const [out, err, status] = await Promise.all([
    read(child.stdout, `printed more than ${outputLimit} bytes, which no release is`),
    read(child.stderr, `wrote more than ${outputLimit} bytes to stderr`),
    child.exited,
  ]);
  clearTimeout(deadline);
  const relayed = redactTokens(err);
  if (relayed !== "") process.stderr.write(relayed.endsWith("\n") ? relayed : `${relayed}\n`);
  if (ended !== "") throw new Error(`${field} ${ended}`);
  if (status !== 0) {
    const failed = `${field} failed (exit ${status})`;
    throw new Error(relayed.trim() === "" ? failed : `${failed}: ${relayed.trim()}`);
  }
  return out;
}

/** One installation of a release: the account it is on, its id, and its token. */
export interface AppInstallation {
  readonly token: string;
  readonly installation: number;
  readonly account: string;
}

/** The installation `owner` holds of the App `source` names, from the release its token command
 * gives for that owner, read-only when `readOnly`. The whole release is held to the shape the
 * daemon requires first (firstInstallationToken), so an expired or malformed one is refused here
 * and not at the first call it would fail; the entry taken is the owner's own, matched as GitHub
 * matches a login, not the release's first. A release that names no installation for the owner is
 * undefined, which is an App the owner has not installed. */
export async function installationFor(
  role: AppRole,
  source: AppTokenSource,
  owner: string,
  readOnly: boolean
): Promise<AppInstallation | undefined> {
  const field = `github_apps.${role}.token_command`;
  const output = await runTokenCommand(field, source.tokenCommand, owner, readOnly);
  firstInstallationToken(output, field, new Date());
  const installations = member(JSON.parse(output.trim()), "installations");
  const account = Object.keys(installations as object).find(
    (login) => login.toLowerCase() === owner.toLowerCase()
  );
  if (account === undefined) return undefined;
  const entry = member(installations, account);
  return {
    token: member(entry, "token") as string,
    installation: Number(member(entry, "installation_id")),
    account,
  };
}

export type Installation =
  | { installed: true; installation: number; account: string }
  | { installed: false; reason: string };

/** Whether the App `source` names is installed on `repo`: its owner's installation token lists
 * exactly the repositories the installation is on (`GET /installation/repositories`), which a
 * public repository outside the installation is not, and which the token's own
 * `GET /repos/<owner>/<repo>` answers 200 for. Any answer that is not 2xx is a throw naming the
 * read and GitHub's status, never a token. */
export async function installationOn(
  role: AppRole,
  source: AppTokenSource,
  repo: string,
  api: string
): Promise<Installation> {
  const owner = repo.split("/")[0] as string;
  const installation = await installationFor(role, source, owner, true);
  if (installation === undefined) {
    return {
      installed: false,
      reason: `the ${role} App's token command released no installation for ${owner}`,
    };
  }
  let page = 1;
  let seen = 0;
  for (;;) {
    const read = `GET /installation/repositories?page=${page}`;
    const response = await fetch(`${api}/installation/repositories?per_page=100&page=${page}`, {
      headers: {
        authorization: `Bearer ${installation.token}`,
        accept: "application/vnd.github+json",
        "x-github-api-version": "2022-11-28",
      },
      signal: AbortSignal.timeout(30_000),
    });
    if (!response.ok) {
      throw new Error(`${read} answered ${response.status} for the ${role} App on ${owner}`);
    }
    const body = await response.json();
    const total = member(body, "total_count");
    const listed = member(body, "repositories");
    if (typeof total !== "number" || !Array.isArray(listed)) {
      throw new Error(`${read} named no repositories for the ${role} App on ${owner}`);
    }
    for (const entry of listed) {
      if (String(member(entry, "full_name")).toLowerCase() === repo.toLowerCase()) {
        return {
          installed: true,
          installation: installation.installation,
          account: installation.account,
        };
      }
    }
    seen += listed.length;
    if (listed.length === 0 || seen >= total) {
      return {
        installed: false,
        reason: `the ${role} App's installation ${installation.installation} on ${installation.account} is on ${total} repositories, and ${repo} is not one of them`,
      };
    }
    page += 1;
  }
}

if (import.meta.main) {
  const args = process.argv.slice(2);
  const roles = args[0] === "--roles" ? args[1]?.split(",") : undefined;
  const [config, repo, api = "https://api.github.com"] = args.slice(2);
  if (
    roles === undefined ||
    roles.some((role) => role !== "implement" && role !== "review") ||
    config === undefined ||
    repo === undefined ||
    !/^[A-Za-z0-9-]+\/[A-Za-z0-9._-]+$/.test(repo)
  ) {
    console.error(usage);
    process.exit(2);
  }
  try {
    const sources = appTokenSources(readFileSync(config, "utf8"), roles as AppRole[]);
    const answer: Record<string, Installation> = {};
    for (const role of roles as AppRole[]) {
      const source = sources[role];
      if (source === undefined) throw new Error(`github_apps.${role} is not configured`);
      answer[role] = await installationOn(role, source, repo, api);
    }
    console.log(JSON.stringify(answer));
  } catch (error) {
    console.error(`app-installation: ${error instanceof Error ? error.message : String(error)}`);
    process.exit(1);
  }
}
