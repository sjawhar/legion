import { describe, expect, test } from "bun:test";
import {
  ISSUE_CLAIM_UNRESPONSIVE_GRACE_MS,
  SLACK_POST_TEXT_MAX,
  zodSchemaApi,
} from "@legion/contracts";
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
import { UNRESPONSIVE_RULE } from "../transport";

/** The grace the unresponsive rule names, in minutes, from the constant the Dispatch server
 *  refuses a claim take by: the rule's text must carry that number, and a change of the constant
 *  must change the text rather than leave it saying a number nothing enforces. */
const GRACE_MINUTES = ISSUE_CLAIM_UNRESPONSIVE_GRACE_MS / 60_000;

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
      "idempotency_key",
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
      "a session that stopped without unregistering stays registered until Envoy finds it gone and is named until then. A named session is not a failed delivery to the other sessions"
    );
    // The listener answers 200 for an unresponsive follower and names it under `unresponsive`
    // (`cmd/listener/api.go`'s `publishResponse`), so the description must not call that an
    // error (dispatch://INFRA-392, R7).
    expect(publish?.description).toContain(
      "A session that is only unresponsive is named as no failure at all. The listener stores that session's copy, asks it for no receipt, and names the session under unresponsive. The accepted publish and the error both name it there. That session takes the message when it answers again."
    );
  });

  test("tells an agent the three answers a role message has, the kept one as a success", () => {
    const publish = envoyToolSpecs.find((spec) => spec.name === "envoy_publish");

    expect(publish?.description).toContain("A role message has three possible answers.");
    // 200: a live holder took it, and must confirm it.
    expect(publish?.description).toContain(
      "the session that holds the role takes the message. That session must confirm the message inside the listener's window, or this tool errs"
    );
    // 202: nobody holds the role, so the listener keeps the message. This is not a refusal.
    expect(publish?.description).toContain(
      "The listener then keeps the message, and gives it to the next session that claims that role in the hour that follows. This is a success"
    );
    // 404: nothing was kept, and the one state that reaches it - a holder that stopped
    // answering, which keeps its role - must tell the model to publish again rather than to
    // treat the message as waiting for that holder (dispatch://INFRA-392, R3 and R7).
    expect(publish?.description).toContain("the listener keeps nothing and errs");
    expect(publish?.description).toContain(
      "A holder that stopped answering keeps its role but was not sent the message, and nothing waits for it, so publish the message again once that holder's holder_state reads live, and start no replacement for that session."
    );
  });
});

describe("publishConfirmationText", () => {
  test("says a kept answer is a success, and that the message waits for the role's next holder", () => {
    const line = publishConfirmationText({
      envelope: { event_id: "event-kept" },
      unresponsive: [],
      kept: true,
    });

    expect(line).toBe(
      "published event-kept. Envoy kept it for the role: no session holds it yet, and the message waits for the session that claims it next."
    );
  });

  test("names no holder on a kept answer whose resolution named one", () => {
    // `holder` on a kept answer is the session the resolution read, which never got the
    // message, so naming it as the holder would be a lie.
    const line = publishConfirmationText({
      envelope: { event_id: "event-lapsed" },
      holder: "ses_lapsed",
      unresponsive: [],
      kept: true,
    });

    expect(line).not.toContain("ses_lapsed");
    expect(line).toContain("kept it for the role");
  });

  test("names the holder a delivered role message reached, and nothing else for a plain topic", () => {
    expect(
      publishConfirmationText({
        envelope: { event_id: "event-held" },
        holder: "ses_architect",
        unresponsive: [],
      })
    ).toBe("published event-held. Its role holder is ses_architect.");
    expect(
      publishConfirmationText({ envelope: { event_id: "event-team" }, unresponsive: [] })
    ).toBe("published event-team.");
  });

  // The description is what an agent reads at the moment it decides whether a peer is alive. The
  // old wording ("all live sessions", "up to 5 minutes") said a quiet session was a dead one, and
  // agents acted on it: role claims taken, replacements started (dispatch://INFRA-392).
  test("every tool that names another session carries the three states and the unresponsive rule", () => {
    const named = ["envoy_sessions", "envoy_role_get", "envoy_send", "envoy_publish"];

    for (const name of named) {
      const description = envoyToolSpecs.find((spec) => spec.name === name)?.description ?? "";
      expect(description).toContain(
        "A session is live, unresponsive or gone, by how long ago it last registered: live within 5 minutes, unresponsive after that and within an hour, gone past the hour or with no record at all."
      );
      expect(description).toContain(
        "An unresponsive session is not dead: it keeps its role claims and its issue claims, and a message sent to it is stored until it answers. Start no replacement for it and take no claim from it until it is gone, " +
          `or until ${GRACE_MINUTES} minutes have passed since its last_seen, the time of its last registration.`
      );
      expect(description).toContain("A session named with no state is live.");
    }
    expect(
      envoyToolSpecs.find((spec) => spec.name === "envoy_sessions")?.description
    ).not.toContain("List all live sessions");
    expect(envoyToolSpecs.find((spec) => spec.name === "envoy_publish")?.description).not.toContain(
      "up to 5 minutes"
    );
  });
});

describe("sendConfirmationText", () => {
  const envelope = { event_id: "evt_1" };

  test("names the recipient's state, and tells the sender an unresponsive one is not a failure", () => {
    expect(
      sendConfirmationText({
        envelope,
        recipient: "ses_peer",
        recipientState: "live",
        confirmed: true,
        duplicate: false,
      })
    ).toBe("sent evt_1 to ses_peer (live)");
    expect(
      sendConfirmationText({
        envelope,
        recipient: "ses_peer",
        recipientState: "unresponsive",
        confirmed: false,
        duplicate: false,
      })
    ).toBe(
      "sent evt_1 to ses_peer (unresponsive, so it is stored until that session answers; An unresponsive session is not dead: it keeps its role claims and its issue claims, and a message sent to it is stored until it answers. Start no replacement for it and take no claim from it until it is gone, " +
        `or until ${GRACE_MINUTES} minutes have passed since its last_seen, the time of its last registration.) (recipient unconfirmed by listener)`
    );
  });

  // The listener sends the recipient's last registration beside an unresponsive state, and a
  // sender deciding whether to wait reads how long the peer has been silent, not only that it is
  // (dispatch://INFRA-392, R7).
  test("says since when an unresponsive recipient has been silent, from the listener's last_seen", () => {
    expect(
      sendConfirmationText({
        envelope,
        recipient: "ses_peer",
        recipientState: "unresponsive",
        confirmed: true,
        lastSeen: 1_762_000_000_000,
        duplicate: false,
      })
    ).toContain("(unresponsive since 2025-11-01T12:26:40.000Z, so it is stored until");
  });
});

describe("publishConfirmationText", () => {
  const envelope = { event_id: "evt_1" };

  // A publish the listener accepted still leaves a stalled subscriber without the message: the
  // copy is stored for it. The line both hosts show is where the publishing agent learns that,
  // so it must name the session and say the rule (dispatch://INFRA-392, R7).
  test("names each recipient the message was only stored for, and says the rule once", () => {
    expect(publishConfirmationText({ envelope, unresponsive: [] })).toBe("published evt_1.");
    expect(publishConfirmationText({ envelope, unresponsive: ["ses_stalled"] })).toBe(
      `published evt_1. Envoy stored it for ses_stalled. That session stopped answering, so it has not taken it yet. ${UNRESPONSIVE_RULE}`
    );
    expect(publishConfirmationText({ envelope, unresponsive: ["ses_one", "ses_two"] })).toBe(
      `published evt_1. Envoy stored it for ses_one, ses_two. Those sessions stopped answering, so they have not taken it yet. ${UNRESPONSIVE_RULE}`
    );
  });

  test("names a role publish's holder and the state the listener resolved it in", () => {
    expect(
      publishConfirmationText({
        envelope,
        holder: "ses_holder",
        holderState: "live",
        unresponsive: [],
      })
    ).toBe("published evt_1. Its role holder is ses_holder (live).");
    // A listener predating the field names the holder alone, and the line says only that.
    expect(publishConfirmationText({ envelope, holder: "ses_holder", unresponsive: [] })).toBe(
      "published evt_1. Its role holder is ses_holder."
    );
  });
});

describe("envoy_send resend", () => {
  const key = "0f6c1e3a-7d2b-4c8e-9a51-3b7e2d4f6a80";

  test("takes the idempotency_key an unconfirmed send named", () => {
    expect(
      parseEnvoyToolArguments("send", { session_id: "ses_b", message: "hi", idempotency_key: key })
    ).toEqual({ session_id: "ses_b", message: "hi", idempotency_key: key });
  });

  test("refuses a key the listener would not store once", () => {
    for (const idempotency_key of ["retry-1", key.toUpperCase(), `${key}0`]) {
      expect(() =>
        parseEnvoyToolArguments("send", { session_id: "ses_b", message: "hi", idempotency_key })
      ).toThrow(ToolInputError);
    }
  });

  test("says a resend the listener already held stored no second copy", () => {
    const envelope = { event_id: "event-2" };
    expect(
      sendConfirmationText({
        envelope,
        recipient: "ses_b",
        recipientState: "live",
        confirmed: true,
        duplicate: true,
      })
    ).toBe(
      "sent to ses_b once: an earlier attempt under the same idempotency_key was already stored, so this one stored no second copy"
    );
    expect(
      sendConfirmationText({
        envelope,
        recipient: "ses_b",
        recipientState: "live",
        confirmed: true,
        duplicate: false,
      })
    ).toBe("sent event-2 to ses_b (live)");
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
      'expects_reply must be one of false|optional|required; got "no"',
      'unknown field "msg"; allowed: session_id, message, in_reply_to, supersedes, urgency, expects_reply, expires_at, idempotency_key',
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
    state: "live" as const,
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
      state: "live" as const,
    };

    expect(sessionsOnMachine([hostless], "devbox")).toEqual([hostless]);
    expect(sessionsOnMachine([hostless], "gke-node-7")).toEqual([]);
  });
});
