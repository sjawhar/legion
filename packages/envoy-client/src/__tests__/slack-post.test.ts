import { afterEach, describe, expect, test } from "bun:test";
import { chmodSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { postSlackReply, SLACK_BOT_TOKEN_SECRET } from "../slack-post";

// A stand-in for the secrets broker's client: it records its arguments and, as the exec form does
// once the broker grants the secret, runs the command after `--` with the secret in its
// environment; FAKE_BROKER=pending or denied answers as the real client does for those states.
const FAKE_AGENT_SECRETS = `#!/bin/sh
printf '%s\\n' "$@" > "$FAKE_ARGS"
case "$FAKE_BROKER" in
  pending)
    echo "agent-secrets: request req-1 is waiting for approval; waiting up to 0s" >&2
    echo "agent-secrets: request req-1 is still waiting for approval; nothing was run. Check it with: agent-secrets status req-1" >&2
    exit 75 ;;
  denied)
    echo "agent-secrets: request req-1 was denied" >&2
    exit 77 ;;
  unenrolled)
    echo "agent-secrets: AGENT_SECRETS_URL is required" >&2
    exit 2 ;;
esac
while [ "$1" != "--" ]; do shift; done
shift
LEGION_SLACK_BOT_TOKEN=xoxb-granted exec "$@"
`;

interface SlackCall {
  readonly path: string;
  readonly authorization: string | null;
  readonly body: unknown;
}

const cleanups: (() => void)[] = [];
afterEach(() => {
  for (const cleanup of cleanups.splice(0)) cleanup();
});

/** A fake Slack Web API whose chat.postMessage answers `answer`, recording every call. */
function fakeSlack(answer: unknown): { readonly url: string; readonly calls: SlackCall[] } {
  const calls: SlackCall[] = [];
  const server = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    async fetch(request) {
      calls.push({
        path: new URL(request.url).pathname,
        authorization: request.headers.get("authorization"),
        body: await request.json(),
      });
      return Response.json(answer);
    },
  });
  cleanups.push(() => server.stop(true));
  return { url: `http://127.0.0.1:${server.port}/api`, calls };
}

/** An environment whose PATH finds the fake broker client first, and the file it writes its
 * arguments to. */
function brokerEnvironment(mode: string, slackURL: string) {
  const dir = mkdtempSync(path.join(tmpdir(), "slack-post-"));
  cleanups.push(() => rmSync(dir, { recursive: true, force: true }));
  const client = path.join(dir, "agent-secrets");
  writeFileSync(client, FAKE_AGENT_SECRETS);
  chmodSync(client, 0o755);
  const args = path.join(dir, "args");
  return {
    args,
    env: {
      PATH: `${dir}${path.delimiter}${process.env.PATH ?? ""}`,
      FAKE_ARGS: args,
      FAKE_BROKER: mode,
      SLACK_POST_API_URL: slackURL,
    } as Record<string, string>,
  };
}

const reply = { channel: "C00000000", threadTs: "1700000000.000100", text: "Filed as ACME-12." };

describe("postSlackReply", () => {
  test("posts through the broker's exec form with the granted token and answers Slack's ts", async () => {
    const slack = fakeSlack({ ok: true, channel: "C00000000", ts: "1700000000.000200" });
    const { env, args } = brokerEnvironment("granted", slack.url);

    const posted = await postSlackReply(reply, { env });

    expect(posted).toEqual({
      channel: "C00000000",
      threadTs: "1700000000.000100",
      ts: "1700000000.000200",
    });
    expect(slack.calls).toEqual([
      {
        path: "/api/chat.postMessage",
        authorization: "Bearer xoxb-granted",
        body: { channel: "C00000000", thread_ts: "1700000000.000100", text: "Filed as ACME-12." },
      },
    ]);
    const argv = readFileSync(args, "utf-8").trimEnd().split("\n");
    expect(argv.slice(0, argv.indexOf("--"))).toEqual([
      SLACK_BOT_TOKEN_SECRET,
      "--reason",
      "slack_post: reply in Slack channel C00000000, thread 1700000000.000100",
      "--wait",
      "0s",
    ]);
    expect(argv.slice(argv.indexOf("--") + 1, argv.indexOf("--") + 3)).toEqual(["bun", "-e"]);
  });

  test("a pending grant posts nothing and names the broker's state", async () => {
    const slack = fakeSlack({ ok: true, ts: "1.2" });
    const { env } = brokerEnvironment("pending", slack.url);

    const failure = postSlackReply(reply, { env });

    await expect(failure).rejects.toThrow(
      "the secrets broker has not granted LEGION_SLACK_BOT_TOKEN: its request is waiting for approval"
    );
    await expect(failure).rejects.toThrow("nothing was posted");
    await expect(failure).rejects.toThrow("agent-secrets: request req-1 is still waiting");
    expect(slack.calls).toEqual([]);
  });

  test("a denied grant posts nothing and names the broker's state", async () => {
    const slack = fakeSlack({ ok: true, ts: "1.2" });
    const { env } = brokerEnvironment("denied", slack.url);

    await expect(postSlackReply(reply, { env })).rejects.toThrow(
      "the secrets broker denied LEGION_SLACK_BOT_TOKEN (agent-secrets: request req-1 was denied); nothing was posted"
    );
    expect(slack.calls).toEqual([]);
  });

  test("any other broker failure posts nothing and quotes the client", async () => {
    const slack = fakeSlack({ ok: true, ts: "1.2" });
    const { env } = brokerEnvironment("unenrolled", slack.url);

    await expect(postSlackReply(reply, { env })).rejects.toThrow(
      "the secrets broker did not release LEGION_SLACK_BOT_TOKEN (agent-secrets: AGENT_SECRETS_URL is required); nothing was posted"
    );
    expect(slack.calls).toEqual([]);
  });

  test("Slack's refusal comes back verbatim", async () => {
    const slack = fakeSlack({ ok: false, error: "not_in_channel" });
    const { env } = brokerEnvironment("granted", slack.url);

    await expect(postSlackReply(reply, { env })).rejects.toThrow(
      "Slack refused chat.postMessage: not_in_channel"
    );
    expect(slack.calls).toHaveLength(1);
  });

  test("a Slack that does not answer is a failure that says the reply may have been posted", async () => {
    // A port nothing listens on: the poster's request fails after the grant.
    const closed = Bun.serve({ port: 0, hostname: "127.0.0.1", fetch: () => new Response() });
    const url = `http://127.0.0.1:${closed.port}/api`;
    closed.stop(true);
    const { env } = brokerEnvironment("granted", url);

    await expect(postSlackReply(reply, { env })).rejects.toThrow(
      "the reply may have been posted, and posting it again may put it in the thread twice"
    );
  });

  test("a Slack API override that is not https or a loopback host is refused before the broker is asked", async () => {
    const { env, args } = brokerEnvironment("granted", "http://slack.example/api");

    await expect(postSlackReply(reply, { env })).rejects.toThrow(
      "SLACK_POST_API_URL must be an https URL, or http on a loopback host"
    );
    expect(() => readFileSync(args)).toThrow();
  });

  test("a session without the broker's client says so and posts nothing", async () => {
    const env = { PATH: mkdtempSync(path.join(tmpdir(), "slack-post-empty-")) };
    cleanups.push(() => rmSync(env.PATH, { recursive: true, force: true }));

    await expect(postSlackReply(reply, { env })).rejects.toThrow(
      "agent-secrets is not on PATH: slack_post takes the bot token from the secrets broker, which this session is not enrolled with; nothing was posted"
    );
  });
});
