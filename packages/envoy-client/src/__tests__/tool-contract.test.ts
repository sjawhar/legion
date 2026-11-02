import { describe, expect, test } from "bun:test";
import { SLACK_POST_TEXT_MAX, zodSchemaApi } from "@legion/contracts";
import { z } from "zod";
import {
  envoyToolSpecs,
  MessageMetadataSchema,
  messageMetadataShape,
  parseEnvoyToolArguments,
  publishConfirmationText,
  sendConfirmationText,
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
        "reach each subscribed session once from the durable consumer its listener relays to it, also one that was away or restarted"
      );
      expect(description).toContain("Team and other ad-hoc topics are live only.");
    }
    expect(publish?.description).toContain("errs naming each session that did not confirm");
    expect(publish?.description).toContain(
      "a session that stopped without unregistering stays registered for up to 5 minutes and is named in that time, and a named session is not a failed delivery to the other sessions"
    );
    expect(publish?.description).toContain(
      "a role message errs when its holder did not confirm it within the listener's window"
    );
  });

  test("says what the recipient counts and the two session states mean", () => {
    const send = envoyToolSpecs.find((spec) => spec.name === "envoy_send");
    const publish = envoyToolSpecs.find((spec) => spec.name === "envoy_publish");

    expect(send?.description).toContain(
      "The answer says what the target was when Envoy stored the message: connected means it is running, stopped means it is not and Envoy keeps the message 72 hours, so it arrives if that session runs again in that time."
    );
    // `state` is only ever connected or stopped, so nothing tells the model to look for a third.
    expect(send?.description).not.toContain("Queued means");
    expect(publish?.description).toContain(
      "recipients is every session other than you that the message goes to, one that is stopped and gets it when it runs again within 72 hours included, and recipients_connected is how many of those are running now"
    );
    expect(publish?.description).toContain("a role topic names its holder instead");
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

describe("sendConfirmationText", () => {
  const connected = {
    envelope: { event_id: "evt_01" },
    recipient: "ses_target",
    confirmed: true,
    state: "connected",
  } as const;
  // 2026-11-02T09:15:00Z, as the listener sends it: unix milliseconds.
  const lastSeen = Date.UTC(2026, 10, 2, 9, 15);

  test("a connected target is sent to, and an unconfirmed recipient is still said so", () => {
    expect(sendConfirmationText(connected)).toBe("sent evt_01 to ses_target, which is connected");
    expect(sendConfirmationText({ ...connected, confirmed: false })).toBe(
      "sent evt_01 to ses_target, which is connected (recipient unconfirmed by listener)"
    );
  });

  test("a stopped target is queued, with when it was last seen as a UTC instant", () => {
    const text = sendConfirmationText({ ...connected, state: "stopped", lastSeen });

    expect(text).toBe(
      "queued evt_01 for ses_target: the session is stopped (last seen 2026-11-02T09:15:00Z); if it runs again within 72 hours, it gets the message"
    );
    // Acceptance A7: a model that reads "sent" waits for an answer from a session that is not
    // running, so the queued line says nothing of the kind.
    expect(text).toMatch(/^queued \S+ for /);
    expect(text).not.toContain("sent");
  });

  test("a stopped target the listener gives no last seen for is still queued", () => {
    const text = sendConfirmationText({ ...connected, state: "stopped" });

    expect(text).toBe(
      "queued evt_01 for ses_target: the session is stopped; if it runs again within 72 hours, it gets the message"
    );
    expect(text).toMatch(/^queued \S+ for /);
    expect(text).not.toContain("sent");
  });
});

describe("publishConfirmationText", () => {
  const envelope = { event_id: "evt_02", topic: "notifications.team.deploy-squad.all" };

  test("counts the other sessions that get the message, and how many run now", () => {
    const text = publishConfirmationText({ envelope, recipients: 3, recipientsConnected: 2 });

    expect(text).toBe("published evt_02 to 3 other sessions (2 running now)");
    // Acceptance A2.
    expect(text).toMatch(/^published \S+ to 3 other sessions/);
  });

  test("one other session takes the singular", () => {
    expect(publishConfirmationText({ envelope, recipients: 1, recipientsConnected: 1 })).toBe(
      "published evt_02 to 1 other session (1 running now)"
    );
    expect(publishConfirmationText({ envelope, recipients: 1, recipientsConnected: 0 })).toBe(
      "published evt_02 to 1 other session (0 running now)"
    );
  });

  test("a count of none says the message reached nobody, and names the topic", () => {
    const text = publishConfirmationText({ envelope, recipients: 0, recipientsConnected: 0 });

    expect(text).toBe(
      "published evt_02; no other session subscribes to notifications.team.deploy-squad.all, so no other session gets it"
    );
    // Acceptance A2.
    expect(text).toMatch(/^published \S+; no other session subscribes to /);
  });

  test("a listener that counts nothing is answered exactly the line it always was", () => {
    expect(publishConfirmationText({ envelope })).toBe("published evt_02");
  });

  test("a role topic names its holder and is given no count", () => {
    expect(
      publishConfirmationText({
        envelope: { event_id: "evt_03", topic: "notifications.role.controller" },
        holder: "ses_controller",
      })
    ).toBe("published evt_03; holder ses_controller");
  });
});
