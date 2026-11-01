// The production check of Dispatch's keyed writes (packages/envoy/cmd/dispatch/README.md,
// "Checking a deploy"): a comment whose first answer is lost lands once, and the repeat under its
// key is Dispatch's stored answer (`Idempotency-Replayed: ?1`).
//
//   bun scripts/e2e/lib/dispatch-keyed-comment.ts --url <dispatch> --bearer-file <file> --issue <KEY> [--session <id>] [--cli]
//
// Without --cli it posts one comment on <KEY> under one `Idempotency-Key` twice, as a page whose
// first answer was lost does. With --cli the `dispatch` command of this checkout
// (packages/envoy-client/bin/dispatch.ts) posts it, through a proxy on 127.0.0.1 that forwards
// everything to <dispatch> and answers the command's first comment POST with a gateway's 502 page
// once Dispatch has answered it, so the command asks again under its own key. Either way it then
// counts the issue's comments carrying the check's body, which no other comment carries. It prints
// one JSON line per step, then {"ok":true} and exits 0; a check that fails prints
// {"failed":"<why>"} and exits 1, and a usage error exits 2. Name a scratch issue: the check leaves
// its comment there.
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { parseArgs } from "node:util";
import {
  IDEMPOTENCY_KEY_HEADER,
  IDEMPOTENCY_REPLAYED_HEADER,
} from "../../../packages/contracts/src/dispatch-api";

const USAGE =
  "usage: bun scripts/e2e/lib/dispatch-keyed-comment.ts --url <dispatch> --bearer-file <file> --issue <KEY> [--session <id>] [--cli]";
const CLI = join(import.meta.dir, "../../../packages/envoy-client/bin/dispatch.ts");

class UsageError extends Error {}
class CheckFailed extends Error {}

interface Options {
  readonly bearer: string;
  readonly cli: boolean;
  readonly issue: string;
  readonly session: string;
  readonly url: URL;
}

/** One answer to a comment POST, as the check prints it. */
interface Answer {
  readonly echo: string | null;
  readonly id: string | null;
  readonly replayed: string | null;
  readonly status: number;
}

function write(line: object): void {
  process.stdout.write(`${JSON.stringify(line)}\n`);
}

async function optionsFrom(argv: readonly string[]): Promise<Options> {
  let values: {
    "bearer-file"?: string;
    cli?: boolean;
    issue?: string;
    session?: string;
    url?: string;
  };
  try {
    values = parseArgs({
      args: [...argv],
      allowPositionals: false,
      options: {
        "bearer-file": { type: "string" },
        cli: { type: "boolean" },
        issue: { type: "string" },
        session: { type: "string" },
        url: { type: "string" },
      },
      strict: true,
    }).values;
  } catch (error) {
    throw new UsageError(error instanceof Error ? error.message : String(error));
  }
  const { url: rawURL, issue, "bearer-file": bearerFile } = values;
  if (rawURL === undefined || rawURL === "") throw new UsageError("--url is required");
  if (issue === undefined || issue === "") throw new UsageError("--issue is required");
  if (bearerFile === undefined || bearerFile === "") {
    throw new UsageError("--bearer-file is required");
  }
  let url: URL;
  try {
    url = new URL(rawURL);
  } catch {
    throw new UsageError(`--url is not a URL: ${rawURL}`);
  }
  if (url.protocol !== "http:" && url.protocol !== "https:") {
    throw new UsageError(`--url is Dispatch's http or https address, not ${rawURL}`);
  }
  let bearer: string;
  try {
    bearer = (await Bun.file(bearerFile).text()).trim();
  } catch (error) {
    const reason = error instanceof Error ? error.message : String(error);
    throw new UsageError(`cannot read --bearer-file ${bearerFile}: ${reason}`);
  }
  if (bearer === "") throw new UsageError(`--bearer-file ${bearerFile} holds no token`);
  return {
    bearer,
    cli: values.cli === true,
    issue,
    session: values.session ?? `deploy-check-${crypto.randomUUID()}`,
    url,
  };
}

/** Dispatch's `/healthz` says it judges the key (`idempotency_keys: true`), whatever its status:
 *  a Dispatch that does not would make a repeated write a second time. */
async function checkHealthz(base: URL): Promise<void> {
  const response = await fetch(new URL("/healthz", base));
  const body: unknown = await response.json().catch(() => null);
  const keys =
    typeof body === "object" && body !== null && "idempotency_keys" in body
      ? body.idempotency_keys
      : undefined;
  write({ healthz: { idempotency_keys: keys ?? null, status: response.status } });
  if (keys !== true) {
    throw new CheckFailed(
      "/healthz does not answer idempotency_keys: true, so this Dispatch does not judge Idempotency-Key and would make a repeated write twice"
    );
  }
}

async function answerOf(response: Response): Promise<Answer> {
  const body: unknown = await response.json().catch(() => null);
  return {
    echo: response.headers.get(IDEMPOTENCY_KEY_HEADER),
    id:
      typeof body === "object" && body !== null && "id" in body && typeof body.id === "string"
        ? body.id
        : null,
    replayed: response.headers.get(IDEMPOTENCY_REPLAYED_HEADER),
    status: response.status,
  };
}

/** Posts the check's comment under `key` twice, as a page whose first answer was lost asks again,
 *  and checks the repeat is the first's stored answer. */
async function postTwice(options: Options, key: string, body: string): Promise<void> {
  const post = async (): Promise<Answer> =>
    answerOf(
      await fetch(
        new URL(`/api/v1/issues/${encodeURIComponent(options.issue)}/comments`, options.url),
        {
          body: JSON.stringify({ actor: { id: options.session, kind: "session" }, body }),
          headers: {
            Authorization: `Bearer ${options.bearer}`,
            "Content-Type": "application/json",
            [IDEMPOTENCY_KEY_HEADER]: key,
          },
          method: "POST",
        }
      )
    );
  const first = await post();
  write({ post: 1, ...first });
  if (first.status !== 201) {
    throw new CheckFailed(`the first POST was answered ${first.status}, not 201`);
  }
  const repeat = await post();
  write({ post: 2, ...repeat });
  if (repeat.status !== 201 || repeat.replayed !== "?1") {
    throw new CheckFailed(
      `the repeat under the same key was answered ${repeat.status} with ${IDEMPOTENCY_REPLAYED_HEADER}: ${repeat.replayed}, not Dispatch's stored 201 marked ?1`
    );
  }
  if (repeat.id !== first.id || repeat.echo !== key || first.echo !== key) {
    throw new CheckFailed(
      `the repeat named comment ${repeat.id} and key ${repeat.echo}, the first comment ${first.id} and key ${first.echo}, where both should name ${first.id} and ${key}`
    );
  }
}

/** Runs this checkout's `dispatch comment` through a proxy that loses the first comment POST's
 *  answer, and checks the command asked again under that POST's key and was answered the replay. */
async function postThroughCli(options: Options, body: string): Promise<void> {
  const comments = `/api/v1/issues/${encodeURIComponent(options.issue)}/comments`;
  const posts: { key: string | null; replayed: string | null; status: number }[] = [];
  const proxy = Bun.serve({
    async fetch(request) {
      const incoming = new URL(request.url);
      const target = new URL(incoming.pathname + incoming.search, options.url);
      const headers = new Headers(request.headers);
      headers.delete("host");
      // Asked unencoded, so the answer passes on as it came, with no encoding to undo twice.
      headers.delete("accept-encoding");
      const response = await fetch(target, {
        body:
          request.method === "GET" || request.method === "HEAD"
            ? undefined
            : await request.arrayBuffer(),
        headers,
        method: request.method,
        redirect: "manual",
      });
      const line = { method: request.method, path: incoming.pathname, status: response.status };
      if (request.method !== "POST" || incoming.pathname !== comments) {
        write({ proxy: line });
        return response;
      }
      const post = {
        key: request.headers.get(IDEMPOTENCY_KEY_HEADER),
        replayed: response.headers.get(IDEMPOTENCY_REPLAYED_HEADER),
        status: response.status,
      };
      posts.push(post);
      if (posts.length > 1) {
        write({ proxy: { ...line, ...post } });
        return response;
      }
      // Dispatch made the comment; the command is handed a gateway's page in place of its
      // answer, with no key echo, as a load balancer that lost the answer hands one.
      await response.arrayBuffer();
      write({ proxy: { ...line, ...post, answered: 502 } });
      return new Response("<html><body><h1>502 Bad Gateway</h1></body></html>", {
        headers: { "Content-Type": "text/html" },
        status: 502,
      });
    },
    hostname: "127.0.0.1",
    port: 0,
  });
  const home = mkdtempSync(join(tmpdir(), "dispatch-keyed-comment-"));
  try {
    const child = Bun.spawn(
      [process.execPath, CLI, "comment", "--issue", options.issue, "--body", body],
      {
        env: {
          DISPATCH_HOST: "omp",
          DISPATCH_SESSION_ID: options.session,
          DISPATCH_TOKEN: options.bearer,
          DISPATCH_URL: `http://127.0.0.1:${proxy.port}`,
          HOME: home,
          PATH: process.env.PATH ?? "",
        },
        stderr: "pipe",
        stdout: "pipe",
      }
    );
    const [code, stdout, stderr] = await Promise.all([
      child.exited,
      new Response(child.stdout).text(),
      new Response(child.stderr).text(),
    ]);
    write({ cli: { code, stderr: stderr.trim(), stdout: stdout.trim() } });
    if (code !== 0) throw new CheckFailed(`dispatch comment exited ${code}`);
  } finally {
    proxy.stop(true);
    rmSync(home, { force: true, recursive: true });
  }
  const [first, ...repeats] = posts;
  if (first === undefined || first.status !== 201) {
    throw new CheckFailed(`Dispatch answered the command's first POST ${first?.status}, not 201`);
  }
  if (repeats.length === 0 || repeats.some((repeat) => repeat.key !== first.key)) {
    throw new CheckFailed(
      `the command asked ${repeats.length} more time(s), under ${JSON.stringify(repeats.map((repeat) => repeat.key))}, where it should ask again under its first key ${first.key}`
    );
  }
  const last = repeats.at(-1);
  if (last?.status !== 201 || last.replayed !== "?1") {
    throw new CheckFailed(
      `the command's last repeat was answered ${last?.status} with ${IDEMPOTENCY_REPLAYED_HEADER}: ${last?.replayed}, not Dispatch's stored 201 marked ?1`
    );
  }
}

/** How many of the issue's comments carry `body`: the check's, which names its key. */
async function countComments(options: Options, body: string): Promise<void> {
  const response = await fetch(
    new URL(`/api/v1/issues/${encodeURIComponent(options.issue)}/comments`, options.url),
    { headers: { Authorization: `Bearer ${options.bearer}` } }
  );
  if (!response.ok) {
    throw new CheckFailed(`listing ${options.issue}'s comments was answered ${response.status}`);
  }
  const listed: unknown = await response.json();
  const count = Array.isArray(listed)
    ? listed.filter(
        (comment: unknown) =>
          typeof comment === "object" &&
          comment !== null &&
          "body" in comment &&
          comment.body === body
      ).length
    : 0;
  write({ comments_with_body: count });
  if (count !== 1) {
    throw new CheckFailed(`${options.issue} holds ${count} comments with the check's body, not 1`);
  }
}

async function main(argv: readonly string[]): Promise<number> {
  try {
    const options = await optionsFrom(argv);
    await checkHealthz(options.url);
    const check = crypto.randomUUID();
    const body = `Dispatch deploy check ${check} at ${new Date().toISOString()}: a comment whose first answer was lost, made once.`;
    if (options.cli) await postThroughCli(options, body);
    else await postTwice(options, `deploy-check-${check}`, body);
    await countComments(options, body);
    write({ ok: true });
    return 0;
  } catch (error) {
    if (error instanceof UsageError) {
      process.stderr.write(`dispatch-keyed-comment: ${error.message}\n${USAGE}\n`);
      return 2;
    }
    write({ failed: error instanceof Error ? error.message : String(error) });
    return 1;
  }
}

process.exit(await main(Bun.argv.slice(2)));
