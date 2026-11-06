// scripts/e2e/lib/smoke-pull-request.ts: the GitHub writes a live proof makes on the smoke
// repository as Legion's role Apps, and their undoing: a pull request an implementer would have
// opened (a branch with one file, and the pull request from it), then its close, its branch's
// deletion and the deletion of a comment by id, as the implement App; and an approval of a pull
// request's head as the review App, with a body of the caller's. Each call mints the App's
// installation token on the repository from its own JWT; the key and the token stay in this
// process.
//
//   bun scripts/e2e/lib/smoke-pull-request.ts open <legion.yaml> <owner/repo> <branch> <title>
//     prints {"number":N,"branch":"<branch>","url":"<html url>","head":"<sha>"}
//   bun scripts/e2e/lib/smoke-pull-request.ts close <legion.yaml> <owner/repo> <number> <branch>
//   bun scripts/e2e/lib/smoke-pull-request.ts delete-comment <legion.yaml> <owner/repo> <comment id>
//     each prints what it did; a pull request already closed, a branch or comment already gone is
//     done already and says so
//   bun scripts/e2e/lib/smoke-pull-request.ts approve <legion.yaml> <owner/repo> <number> <head sha> <body>
//     prints {"id":N,"state":"APPROVED","user":"<login>","commit_id":"<sha>"}
//
// The App is `github_apps.implement` of <legion.yaml> (`github_apps.review` for approve), its key
// from its private_key_command (lib/app-logins.ts, runKeyCommand). Failures name the call and
// GitHub's status, never a key.
import { readFileSync } from "node:fs";
import { appJWT, appKeySources, runKeyCommand } from "./app-logins";

const api = "https://api.github.com";
const usage = `usage:
  bun scripts/e2e/lib/smoke-pull-request.ts open <legion.yaml> <owner/repo> <branch> <title>
  bun scripts/e2e/lib/smoke-pull-request.ts close <legion.yaml> <owner/repo> <number> <branch>
  bun scripts/e2e/lib/smoke-pull-request.ts delete-comment <legion.yaml> <owner/repo> <comment id>
  bun scripts/e2e/lib/smoke-pull-request.ts approve <legion.yaml> <owner/repo> <number> <head sha> <body>`;

type Call = (method: string, path: string, body?: unknown) => Promise<Response>;

/** The string or number at `keys` inside GitHub's parsed answer `value`, or a throw naming `what`. */
function field(value: unknown, what: string, ...keys: string[]): string {
  let at = value;
  for (const key of keys) {
    at =
      at !== null && typeof at === "object" && key in at
        ? Object.getOwnPropertyDescriptor(at, key)?.value
        : undefined;
  }
  if (typeof at !== "string" && typeof at !== "number")
    throw new Error(`${what} named no ${keys.join(".")}`);
  return String(at);
}

async function installationCall(
  config: string,
  repo: string,
  role: "implement" | "review"
): Promise<Call> {
  const source = appKeySources(readFileSync(config, "utf8"), [role])[role];
  if (source === undefined) throw new Error(`github_apps.${role} is not configured`);
  const jwt = appJWT(source.appId, runKeyCommand(role, source.privateKeyCommand), new Date());
  const headers = (token: string) => ({
    authorization: `Bearer ${token}`,
    accept: "application/vnd.github+json",
    "x-github-api-version": "2022-11-28",
  });
  const installation = await fetch(`${api}/repos/${repo}/installation`, { headers: headers(jwt) });
  if (!installation.ok) {
    throw new Error(`GET /repos/${repo}/installation answered ${installation.status}`);
  }
  const id = field(await installation.json(), `GET /repos/${repo}/installation`, "id");
  const minted = await fetch(`${api}/app/installations/${id}/access_tokens`, {
    method: "POST",
    headers: headers(jwt),
  });
  if (!minted.ok)
    throw new Error(`POST /app/installations/${id}/access_tokens answered ${minted.status}`);
  const token = field(await minted.json(), "the installation token answer", "token");
  return (method, path, body) =>
    fetch(`${api}${path}`, {
      method,
      headers: { ...headers(token), "content-type": "application/json" },
      body: body === undefined ? undefined : JSON.stringify(body),
      signal: AbortSignal.timeout(30_000),
    });
}

async function expectOk(response: Response, what: string): Promise<unknown> {
  if (!response.ok)
    throw new Error(`${what} answered ${response.status}: ${await response.text()}`);
  return response.status === 204 ? {} : await response.json();
}

export async function openPullRequest(call: Call, repo: string, branch: string, title: string) {
  const info = await expectOk(await call("GET", `/repos/${repo}`), `GET /repos/${repo}`);
  const base = field(info, `GET /repos/${repo}`, "default_branch");
  const ref = await expectOk(
    await call("GET", `/repos/${repo}/git/ref/heads/${base}`),
    `GET the ${base} ref`
  );
  const sha = field(ref, `the ${base} ref`, "object", "sha");
  await expectOk(
    await call("POST", `/repos/${repo}/git/refs`, { ref: `refs/heads/${branch}`, sha }),
    `create ${branch}`
  );
  const file = await expectOk(
    await call("PUT", `/repos/${repo}/contents/smoke/${branch.replaceAll("/", "-")}.md`, {
      message: title,
      content: Buffer.from(`${title}\n`).toString("base64"),
      branch,
    }),
    "commit the smoke file"
  );
  const pull = await expectOk(
    await call("POST", `/repos/${repo}/pulls`, { title, head: branch, base, body: `${title}\n` }),
    "open the pull request"
  );
  return {
    number: Number(field(pull, "the pull request", "number")),
    branch,
    url: field(pull, "the pull request", "html_url"),
    head: field(file, "the smoke file's commit", "commit", "sha"),
  };
}

if (import.meta.main) {
  const [command, config, repo, ...rest] = process.argv.slice(2);
  if (!command || !config || !repo || !/^[A-Za-z0-9-]+\/[A-Za-z0-9._-]+$/.test(repo)) {
    console.error(usage);
    process.exit(2);
  }
  try {
    const call = await installationCall(
      config,
      repo,
      command === "approve" ? "review" : "implement"
    );
    if (command === "approve" && rest.length === 3) {
      const [number, head, body] = rest;
      const review = await expectOk(
        await call("POST", `/repos/${repo}/pulls/${number}/reviews`, {
          event: "APPROVE",
          commit_id: head,
          body,
        }),
        `approve #${number}`
      );
      console.log(
        JSON.stringify({
          id: Number(field(review, "the review", "id")),
          state: field(review, "the review", "state"),
          user: field(review, "the review", "user", "login"),
          commit_id: field(review, "the review", "commit_id"),
        })
      );
    } else if (command === "open" && rest.length === 2) {
      const [branch = "", title = ""] = rest;
      console.log(JSON.stringify(await openPullRequest(call, repo, branch, title)));
    } else if (command === "close" && rest.length === 2) {
      const [number, branch] = rest;
      const closed = await call("PATCH", `/repos/${repo}/pulls/${number}`, { state: "closed" });
      console.log(closed.ok ? `closed #${number}` : `close #${number} answered ${closed.status}`);
      const deleted = await call("DELETE", `/repos/${repo}/git/refs/heads/${branch}`);
      console.log(
        deleted.ok || deleted.status === 422 || deleted.status === 404
          ? `deleted ${branch}`
          : `delete ${branch} answered ${deleted.status}`
      );
      if (!closed.ok || !(deleted.ok || deleted.status === 422 || deleted.status === 404))
        process.exit(1);
    } else if (command === "delete-comment" && rest.length === 1) {
      const deleted = await call("DELETE", `/repos/${repo}/issues/comments/${rest[0]}`);
      console.log(
        deleted.ok || deleted.status === 404
          ? `deleted comment ${rest[0]}`
          : `delete answered ${deleted.status}`
      );
      if (!(deleted.ok || deleted.status === 404)) process.exit(1);
    } else {
      console.error(usage);
      process.exit(2);
    }
  } catch (error) {
    console.error(`smoke-pull-request: ${error instanceof Error ? error.message : String(error)}`);
    process.exit(1);
  }
}
