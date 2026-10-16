/**
 * The `slack_post` tool's one call: a reply in a Slack thread, posted with Slack's
 * `chat.postMessage` as the Legion Slack app. The bot token is the secrets broker's agent secret
 * `LEGION_SLACK_BOT_TOKEN`, which the broker's own client releases through its exec form,
 * `agent-secrets LEGION_SLACK_BOT_TOKEN --reason … --wait 0s -- bun -e <poster>`: the client asks the
 * broker as this session, and only once the grant is live replaces itself with the poster, the
 * token in the poster's environment alone. The session's process never holds the token. The reply
 * travels on the poster's stdin, and the poster prints one line of JSON saying what Slack answered.
 */

/** The broker's agent secret holding the Legion Slack app's bot token (`xoxb-…`). */
export const SLACK_BOT_TOKEN_SECRET = "LEGION_SLACK_BOT_TOKEN";

/** Moves Slack's Web API (`https://slack.com/api`) for a stand-in; read from the session's
 * environment, held to https or a loopback host, and handed to the poster. */
export const SLACK_POST_API_URL_VARIABLE = "SLACK_POST_API_URL";

/** How long the broker request and the post may take together before the tool stops waiting. The
 * poster gives Slack 10 s of its own. */
const POST_TIMEOUT_MS = 30_000;

/** The poster, run by `bun -e` in the process the broker's client becomes. It reads the reply from
 * stdin and LEGION_SLACK_BOT_TOKEN from its environment, and prints one JSON line: `{ts, channel}` for a
 * posted reply, `{refused}` with Slack's error code, `{unanswered}` when Slack did not answer
 * (the reply may have been posted), `{invalid}` for an answer that is not Slack's. */
const POSTER = `
const out = (value) => process.stdout.write(JSON.stringify(value) + "\\n");
const input = JSON.parse(await Bun.stdin.text());
const token = process.env.LEGION_SLACK_BOT_TOKEN;
if (!token) {
  out({ invalid: "the poster's environment carries no LEGION_SLACK_BOT_TOKEN" });
  process.exit(0);
}
const base = (process.env.SLACK_POST_API_URL || "https://slack.com/api").replace(/\\/+$/, "");
let status = 0;
let raw = "";
try {
  const response = await fetch(base + "/chat.postMessage", {
    method: "POST",
    headers: { Authorization: "Bearer " + token, "Content-Type": "application/json; charset=utf-8" },
    body: JSON.stringify({ channel: input.channel, thread_ts: input.thread_ts, text: input.text }),
    signal: AbortSignal.timeout(10000),
  });
  status = response.status;
  raw = await response.text();
} catch (error) {
  out({ unanswered: String((error && error.message) || error) });
  process.exit(0);
}
let answer;
try {
  answer = JSON.parse(raw);
} catch {}
if (!answer || typeof answer.ok !== "boolean") {
  out({ invalid: "HTTP " + status + " with a body that is not Slack's JSON: " + raw.slice(0, 200) });
} else if (!answer.ok) {
  out({ refused: String(answer.error) });
} else if (typeof answer.ts !== "string") {
  out({ invalid: "ok with no ts" });
} else {
  out({ ts: answer.ts, channel: typeof answer.channel === "string" ? answer.channel : input.channel });
}
`;

/** One reply into a Slack thread. */
export interface SlackReply {
  readonly channel: string;
  readonly threadTs: string;
  readonly text: string;
}

/** Where the reply was posted, and its ts. */
export interface SlackPosted {
  readonly channel: string;
  readonly threadTs: string;
  readonly ts: string;
}

interface PosterLine {
  readonly ts?: string;
  readonly channel?: string;
  readonly refused?: string;
  readonly unanswered?: string;
  readonly invalid?: string;
}

const MAY_HAVE_POSTED =
  "the reply may have been posted, and posting it again may put it in the thread twice";

/**
 * Posts reply once. Nothing retries it: a post whose answer was lost may already be in the
 * thread. A broker that has not granted LEGION_SLACK_BOT_TOKEN (pending approval, denied, or any other
 * refusal) posts nothing, and the error names the state and quotes the client.
 */
export async function postSlackReply(
  reply: SlackReply,
  options: { readonly env?: Record<string, string | undefined> } = {}
): Promise<SlackPosted> {
  const env = options.env ?? process.env;
  const override = env[SLACK_POST_API_URL_VARIABLE];
  if (override !== undefined && override !== "" && !allowedAPIURL(override)) {
    throw new Error(
      `${SLACK_POST_API_URL_VARIABLE} must be an https URL, or http on a loopback host (got ${override}); nothing was posted`
    );
  }
  const { PATH: searchPath = "" } = env;
  const client = Bun.which("agent-secrets", { PATH: searchPath });
  if (client === null) {
    throw new Error(
      "agent-secrets is not on PATH: slack_post takes the bot token from the secrets broker, which this session is not enrolled with; nothing was posted"
    );
  }
  const reason = `slack_post: reply in Slack channel ${reply.channel}, thread ${reply.threadTs}`;
  const child = Bun.spawn(
    [client, SLACK_BOT_TOKEN_SECRET, "--reason", reason, "--wait", "0s", "--", "bun", "-e", POSTER],
    { env, stdin: "pipe", stdout: "pipe", stderr: "pipe" }
  );
  child.stdin.write(
    JSON.stringify({ channel: reply.channel, thread_ts: reply.threadTs, text: reply.text })
  );
  child.stdin.end();
  const timer = setTimeout(() => child.kill("SIGKILL"), POST_TIMEOUT_MS);
  const [stdout, stderr, code] = await Promise.all([
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
    child.exited,
  ]).finally(() => clearTimeout(timer));
  const said = stderr.trim().split("\n").filter(Boolean).join(" ");
  const line = posterLine(stdout);
  if (line === undefined) {
    // The broker's client writes every line of its own prefixed `agent-secrets:`, and exits before
    // it runs the poster; anything else is the poster's, which may have reached Slack.
    if (child.signalCode === "SIGKILL") {
      throw new Error(
        `slack_post got no answer within ${POST_TIMEOUT_MS / 1000} s: ${MAY_HAVE_POSTED}`
      );
    }
    if (said.startsWith("agent-secrets:")) {
      if (code === 75) {
        throw new Error(
          `the secrets broker has not granted ${SLACK_BOT_TOKEN_SECRET}: its request is waiting for approval (${said}); nothing was posted`
        );
      }
      if (code === 77) {
        throw new Error(
          `the secrets broker denied ${SLACK_BOT_TOKEN_SECRET} (${said}); nothing was posted`
        );
      }
      throw new Error(
        `the secrets broker did not release ${SLACK_BOT_TOKEN_SECRET} (${said}); nothing was posted`
      );
    }
    throw new Error(
      `slack_post's poster exited ${code} without an answer${said === "" ? "" : ` (${said})`}: ${MAY_HAVE_POSTED}`
    );
  }
  if (line.refused !== undefined)
    throw new Error(`Slack refused chat.postMessage: ${line.refused}`);
  if (line.unanswered !== undefined) {
    throw new Error(
      `Slack did not answer chat.postMessage (${line.unanswered}): ${MAY_HAVE_POSTED}`
    );
  }
  if (line.ts === undefined) {
    throw new Error(`chat.postMessage answered ${line.invalid ?? "nothing"}: ${MAY_HAVE_POSTED}`);
  }
  return { channel: line.channel ?? reply.channel, threadTs: reply.threadTs, ts: line.ts };
}

/** The poster's last line of stdout, read as its answer; undefined when it printed none. */
function posterLine(stdout: string): PosterLine | undefined {
  const last = stdout.trim().split("\n").at(-1);
  if (last === undefined || last === "") return undefined;
  try {
    const parsed: unknown = JSON.parse(last);
    return typeof parsed === "object" && parsed !== null ? (parsed as PosterLine) : undefined;
  } catch {
    return undefined;
  }
}

/** https anywhere, or http on a loopback host, which a stand-in on this machine is. */
function allowedAPIURL(value: string): boolean {
  let url: URL;
  try {
    url = new URL(value);
  } catch {
    return false;
  }
  if (url.protocol === "https:") return true;
  return url.protocol === "http:" && ["127.0.0.1", "localhost", "[::1]"].includes(url.hostname);
}
