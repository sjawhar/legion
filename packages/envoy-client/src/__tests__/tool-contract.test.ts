import { describe, expect, test } from "bun:test";
import { z } from "zod";
import { envoyToolSpecs, MessageMetadataSchema, toMessageMetadata } from "../tool-contract";

describe("envoyToolSpecs", () => {
  test("defines the ten canonical Envoy tool names", () => {
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
    ]);
  });

  test("keeps subscription operations capability-backed rather than registered by the core", () => {
    const subscribe = envoyToolSpecs.find((spec) => spec.name === "envoy_subscribe");

    expect(subscribe?.operation).toBe("subscribe");
    expect(subscribe?.requiresSubscriptionCapability).toBe(true);
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
    ]);
  });

  test("exposes additive message metadata and listener session filters", () => {
    const send = envoyToolSpecs.find((spec) => spec.name === "envoy_send");
    const publish = envoyToolSpecs.find((spec) => spec.name === "envoy_publish");
    const sessions = envoyToolSpecs.find((spec) => spec.name === "envoy_sessions");

    expect(Object.keys(send?.arguments(z) ?? {}).sort()).toEqual([
      "expects_reply",
      "expires_at",
      "in_reply_to",
      "message",
      "session_id",
      "supersedes",
      "urgency",
    ]);
    expect(Object.keys(publish?.arguments(z) ?? {}).sort()).toEqual([
      "expects_reply",
      "expires_at",
      "in_reply_to",
      "message",
      "supersedes",
      "topic",
      "urgency",
    ]);
    expect(Object.keys(sessions?.arguments(z) ?? {}).sort()).toEqual(["dir", "machine", "title"]);
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
});
