import { describe, expect, test } from "bun:test";
import { SLACK_POST_TEXT_MAX, zodSchemaApi } from "@legion/contracts";
import { z } from "zod";
import {
  envoyToolSpecs,
  MessageMetadataSchema,
  messageMetadataShape,
  parseEnvoyToolArguments,
  sessionsOnMachine,
  toMessageMetadata,
} from "../tool-contract";
import { ToolInputError } from "../tool-input-errors";

const schemaApi = zodSchemaApi(z);

describe("envoyToolSpecs", () => {
  test("defines the ten canonical Envoy tool names and slack_post", () => {
    expect(envoyToolSpecs.map((spec) => spec.name)).toEqual([
      "envoy_subscribe",
      "envoy_unsubscribe",
      "envoy_list",
      "envoy_inbox",
      "envoy_send",
      "envoy_publish",
      "envoy_role_set",
      "envoy_role_get",
      "envoy_whoami",
      "envoy_sessions",
      "slack_post",
    ]);
  });

  test("maps every tool descriptor to its harness-neutral operation", () => {
    expect(envoyToolSpecs.map((spec) => spec.operation)).toEqual([
      "subscribe",
      "unsubscribe",
      "listInterests",
      "inbox",
      "send",
      "publish",
      "setRole",
      "getRole",
      "whoami",
      "listSessions",
      "slackPost",
    ]);
  });

  test("exposes additive message metadata and listener session filters", () => {
    const send = envoyToolSpecs.find((spec) => spec.name === "envoy_send");
    const publish = envoyToolSpecs.find((spec) => spec.name === "envoy_publish");
    const sessions = envoyToolSpecs.find((spec) => spec.name === "envoy_sessions");

    expect(Object.keys(send?.arguments(schemaApi) ?? {}).sort()).toEqual([
      "expects_reply",
      "expires_at",
      "in_reply_to",
      "message",
      "session_id",
      "supersedes",
      "urgency",
    ]);
    expect(Object.keys(publish?.arguments(schemaApi) ?? {}).sort()).toEqual([
      "expects_reply",
      "expires_at",
      "in_reply_to",
      "message",
      "supersedes",
      "topic",
      "urgency",
    ]);
    expect(Object.keys(sessions?.arguments(schemaApi) ?? {}).sort()).toEqual([
      "dir",
      "machine",
      "title",
    ]);
  });

  test("documents delivery guarantees and the full Envoy topic guide", () => {
    const subscribe = envoyToolSpecs.find((spec) => spec.name === "envoy_subscribe");
    const publish = envoyToolSpecs.find((spec) => spec.name === "envoy_publish");

    expect(subscribe?.description).toContain("agent.<session_id>");
    expect(subscribe?.description).toContain("role.<role>");
    expect(subscribe?.description).toContain(
      "Default PR subscription: github.<owner>.<repo>.pr.<n>.>"
    );
    expect(subscribe?.description).toContain(
      "> matches one or more trailing tokens and does not match the base subject"
    );
    expect(subscribe?.description).toContain(
      "Envoy registers the concrete base when you subscribe to <subject>.>"
    );
    expect(subscribe?.description).toContain(
      "pr.<n> (lifecycle: opened/synchronize/closed; closed carries merged, merge_commit_sha, merged_by, head_sha), pr.<n>.comment, pr.<n>.review, pr.<n>.mention, pr.<n>.checks"
    );
    expect(subscribe?.description).toContain(
      "workflow.<file>.<action> (only runs without an associated PR)"
    );
    expect(subscribe?.description).not.toContain("pr.<n>.check,");
    expect(subscribe?.description).not.toContain("pr.<n>.ci");
    expect(subscribe?.description).not.toContain("pr.<n>.checks.settled");
    expect(subscribe?.description).not.toContain("pr.<n>.merged");
    expect(subscribe?.description).not.toContain("pr.<n>.closed");
    expect(subscribe?.description).not.toContain("workflow.<file>.<action>.branch.");
    expect(subscribe?.description).toContain("slack.<team>.<channel>.thread.<ts>.message|mention");
    expect(subscribe?.description).toContain("ghostwispr.<session>.<kind>");
    expect(subscribe?.description).toContain("whatsapp.<phone>.<jid>.<kind>");
    expect(subscribe?.description).toContain("envoy.exceptions.<original-topic>");
    expect(publish?.description).toContain("at-least-once, possibly out of order across topics");
    expect(publish?.description).toContain("id for dedupe and at for freshness");
    for (const description of [subscribe?.description, publish?.description]) {
      expect(description).toContain(
        "reaches each subscribed session once from the durable consumer its listener relays to it, also one that was away or restarted"
      );
      // A team topic is stored like every other one, and a 200 says the stream stored it.
      expect(description).toContain("whatsapp, team;");
      expect(description).toContain(
        "A 200 means the stream stored the message for each subscribed session's consumer, not that any recipient has read it."
      );
      expect(description).not.toContain("live only");
    }
    // Every way a role publish ends in an error: no holder (404), the holder's refusal (403),
    // and the window the listener spends waiting for the stream (504).
    expect(publish?.description).toContain(
      "A role message gets an error when no session holds the role, when the holder refuses the message, and when the forward is not stored within four seconds."
    );
    expect(publish?.description).toContain(
      "A notifications. topic no stream subject captures is refused by name: nothing would store it."
    );
  });
});

describe("message metadata", () => {
  test("maps validated wire fields to the client input shape", () => {
    const metadata = MessageMetadataSchema.parse({
      in_reply_to: "event-before",
      supersedes: "event-obsolete",
      urgency: "blocking",
      expects_reply: "required",
      expires_at: 1_788_956_000_000,
    });

    expect(toMessageMetadata(metadata)).toEqual({
      inReplyTo: "event-before",
      supersedes: "event-obsolete",
      urgency: "blocking",
      expectsReply: "required",
      expiresAt: 1_788_956_000_000,
    });
  });

  test("rejects fractional metadata expiration", () => {
    const schema = z.object(messageMetadataShape(zodSchemaApi(z)) as z.ZodRawShape);

    expect(schema.safeParse({ expires_at: 1.5 }).success).toBe(false);
  });

  test("identifies an invalid metadata enum by field", () => {
    const parsed = MessageMetadataSchema.safeParse({ urgency: "urgent" });
    if (parsed.success) throw new Error("invalid urgency unexpectedly parsed");

    expect(parsed.error.issues).toEqual([
      expect.objectContaining({
        path: ["urgency"],
        message: 'Invalid option: expected one of "low"|"med"|"high"|"blocking"',
      }),
    ]);
  });

  // Nothing in Envoy orders, holds or delivers a message by its urgency: the renderers only show
  // it. A sender told nothing more takes a "high" for a faster lane that does not exist.
  test("tells a sender that urgency is for the reader and does not deliver a message sooner", () => {
    // The JSON schema is what a model is shown for the tool's arguments.
    const shown = JSON.stringify(z.toJSONSchema(MessageMetadataSchema));

    expect(shown).toContain("how soon the recipient should act");
    expect(shown).toContain("does not deliver the message sooner or ahead of others");
  });

  test("refuses an Envoy tool call once with every problem named", () => {
    const failure = (() => {
      try {
        parseEnvoyToolArguments("send", { session_id: "s1", expects_reply: "no", msg: "hi" });
        return undefined;
      } catch (error) {
        return error;
      }
    })();
    if (!(failure instanceof ToolInputError)) throw new Error("expected ToolInputError");
    expect(failure.problems).toEqual([
      "message is required (string)",
      'expects_reply must be one of none|optional|required; got "no"',
      'unknown field "msg"; allowed: session_id, message, in_reply_to, supersedes, urgency, expects_reply, expires_at',
    ]);
    expect(failure.message).toContain("envoy_send was not called: 3 problems");
  });

  test("returns the typed arguments of a valid Envoy tool call", () => {
    expect(
      parseEnvoyToolArguments("publish", { topic: "t", message: "m", urgency: "high" })
    ).toEqual({ topic: "t", message: "m", urgency: "high" });
  });

  test("takes a Slack reply's channel, thread and text, the text at most the listener's cap", () => {
    const reply = {
      channel: "C00000000",
      thread_ts: "1700000000.000100",
      text: "Filed as ACME-12.",
    };
    expect(parseEnvoyToolArguments("slackPost", reply)).toEqual(reply);
    const failure = (() => {
      try {
        parseEnvoyToolArguments("slackPost", {
          channel: "C00000000",
          text: "a".repeat(SLACK_POST_TEXT_MAX + 1),
        });
        return undefined;
      } catch (error) {
        return error;
      }
    })();
    if (!(failure instanceof ToolInputError)) throw new Error("expected ToolInputError");
    expect(failure.problems).toEqual([
      "thread_ts is required (string)",
      `text is 1 characters over the ${SLACK_POST_TEXT_MAX}-character limit (${SLACK_POST_TEXT_MAX + 1}/${SLACK_POST_TEXT_MAX})`,
    ]);
    expect(failure.message).toContain("slack_post was not called: 2 problems");
  });
});

describe("sessionsOnMachine", () => {
  const session = (id: string, machineID: string, host: string) => ({
    session_id: id,
    machine_id: machineID,
    host,
    dir: "/work",
    port: 0,
    title: "",
    topics: [],
    self_subscribed: true,
  });
  const onHub = session("ses_on_hub", "envoy-hub", "gke-node-7");
  const onDevbox = session("ses_on_devbox", "devbox", "devbox");
  const sessions = [onHub, onDevbox];

  test("finds a session by the machine it runs on and by the listener it registered through", () => {
    expect(sessionsOnMachine(sessions, "envoy-hub")).toEqual([onHub]);
    expect(sessionsOnMachine(sessions, "gke-node-7")).toEqual([onHub]);
    expect(sessionsOnMachine(sessions, "devbox")).toEqual([onDevbox]);
    expect(sessionsOnMachine(sessions, "other")).toEqual([]);
  });

  test("no filter lists every session", () => {
    expect(sessionsOnMachine(sessions, undefined)).toEqual(sessions);
    expect(sessionsOnMachine(sessions, "")).toEqual(sessions);
  });

  test("a session an earlier listener reported with no host still matches its listener", () => {
    const hostless = {
      session_id: "ses_old",
      machine_id: "devbox",
      dir: "/work",
      port: 0,
      title: "",
      topics: [],
      self_subscribed: true,
    };

    expect(sessionsOnMachine([hostless], "devbox")).toEqual([hostless]);
    expect(sessionsOnMachine([hostless], "gke-node-7")).toEqual([]);
  });
});
