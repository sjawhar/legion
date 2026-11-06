// scripts/e2e/lib/app-installation.ts: whether each Legion role App a legion.yaml configures is
// installed on a repository, as GitHub answers `GET /repos/<owner>/<repo>/installation` under a JWT
// the App's own private key signs. A live proof that works on a repository through the Apps checks
// this before it relies on one there.
//
//   bun scripts/e2e/lib/app-installation.ts --roles <role>[,<role>] <legion.yaml> <owner/repo> [<GitHub API URL>]
//     prints {"<role>":{"installed":true,"installation":<id>,"account":"<login>"}} or, for an App
//     the repository lacks, {"<role>":{"installed":false,"status":<HTTP status>}}, and exits 0; it
//     names what it could not read on stderr and exits 1, and exits 2 with its usage.
//
// Each key comes from the role's `github_apps.<role>.private_key_command`, run as lib/app-logins.ts
// runs it (runKeyCommand), and stays in this process: never printed, written or handed on.
import { readFileSync } from "node:fs";
import { type AppRole, appJWT, appKeySources, runKeyCommand } from "./app-logins";

const usage =
  "usage: bun scripts/e2e/lib/app-installation.ts --roles <role>[,<role>] <legion.yaml> <owner/repo> [<GitHub API URL>]";

export type Installation =
  | { installed: true; installation: number; account: string }
  | { installed: false; status: number };

/** Whether the App `appId`, keyed by `key`, is installed on `repo`: GitHub's 200 names the
 * installation and its account, a 404 is no installation; any other answer is a throw naming it. */
export async function installationOn(
  appId: string,
  key: string,
  repo: string,
  api: string
): Promise<Installation> {
  const response = await fetch(`${api}/repos/${repo}/installation`, {
    headers: {
      authorization: `Bearer ${appJWT(appId, key, new Date())}`,
      accept: "application/vnd.github+json",
      "x-github-api-version": "2022-11-28",
    },
    signal: AbortSignal.timeout(30_000),
  });
  if (response.status === 404) return { installed: false, status: 404 };
  if (!response.ok) {
    throw new Error(`GET /repos/${repo}/installation answered ${response.status} for App ${appId}`);
  }
  const body = (await response.json()) as { id?: unknown; account?: { login?: unknown } };
  if (typeof body.id !== "number" || typeof body.account?.login !== "string") {
    throw new Error(`GET /repos/${repo}/installation for App ${appId} named no installation`);
  }
  return { installed: true, installation: body.id, account: body.account.login };
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
    const sources = appKeySources(readFileSync(config, "utf8"), roles as AppRole[]);
    const answer: Record<string, Installation> = {};
    for (const role of roles as AppRole[]) {
      const source = sources[role];
      if (source === undefined) throw new Error(`github_apps.${role} is not configured`);
      answer[role] = await installationOn(
        source.appId,
        runKeyCommand(role, source.privateKeyCommand),
        repo,
        api
      );
    }
    console.log(JSON.stringify(answer));
  } catch (error) {
    console.error(`app-installation: ${error instanceof Error ? error.message : String(error)}`);
    process.exit(1);
  }
}
