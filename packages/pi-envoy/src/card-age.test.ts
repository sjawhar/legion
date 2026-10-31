import { describe, expect, test } from "bun:test";
import { renderInbound } from "@legion/envoy-client/delivery";
import { envoyNoticeText, failureNoticeFields } from "@legion/envoy-client/replay";
import { CardAges, duration, MATERIAL_WAIT_MS } from "./card-age";
import { REPLAY_DELIVERED_ENTRY } from "./missed-messages";

const SESSION = "ses_reader";
const SENT = Date.parse("2026-10-31T06:27:17Z");
const LATE = Date.parse("2026-10-31T11:13:02Z");

/** A card as the extension hands it: `renderInbound`'s text of a message sent at `sentAt`. */
function card(eventID: string, sentAt: number, handedAt: number): Record<string, unknown> {
  const rendered = renderInbound(
    JSON.stringify({
      event_id: eventID,
      source: "agent",
      topic: `notifications.agent.${SESSION}`,
      source_session: "ses_sender",
      issued_at: sentAt,
      payload_summary: "Its log reports 06:27:17Z from date -u.",
      urgency: "high",
    }),
    SESSION
  );
  return {
    role: "custom",
    customType: "envoy-message",
    content: rendered.content,
    display: true,
    details: { event_id: eventID },
    timestamp: handedAt,
  };
}

/**
 * The notice the extension shows in place of a frame the host refused, as `showReplayCard` hands
 * it: an inert one carries the frame's identity alone, an ordinary one the wake's card marker too.
 */
function failureNotice(eventID: string, options: { btw?: boolean }): Record<string, unknown> {
  return {
    role: "custom",
    customType: "envoy-message",
    content: envoyNoticeText(
      failureNoticeFields({
        eventId: eventID,
        sender: "ses_sender",
        summary: "unread card",
        publishedAt: new Date(SENT).toISOString(),
        error: "host refused card",
        readWith: "envoy_inbox",
        envelope: { event_id: eventID, source: "agent" },
      })
    ),
    display: true,
    details: {
      event_id: eventID,
      ...(options.btw === true ? {} : { envoyCard: "c0ffee00-0000-4000-8000-000000000000" }),
    },
    timestamp: SENT,
  };
}

/** The text of the card among `messages` that names `eventID`: its content, or its first block's. */
function textOf(messages: readonly unknown[] | undefined, eventID: string): string {
  for (const message of messages ?? []) {
    if (typeof message !== "object" || message === null) continue;
    if (!("details" in message) || !("content" in message)) continue;
    const { details, content } = message;
    if (typeof details !== "object" || details === null || !("event_id" in details)) continue;
    if (details.event_id !== eventID) continue;
    if (typeof content === "string") return content;
    const [first]: unknown[] = Array.isArray(content) ? content : [];
    if (typeof first === "object" && first !== null && "text" in first) return String(first.text);
  }
  return "";
}

/** The content blocks of `message`, when it has them. */
function blocksOf(message: unknown): readonly unknown[] {
  if (typeof message !== "object" || message === null || !("content" in message)) return [];
  return Array.isArray(message.content) ? message.content : [];
}

describe("a card in a model request", () => {
  // The case that made this: a session hours behind on its inbox read a card stamped 06:27:17Z
  // at 11:13Z, took the stamp for the present time, and told other sessions that the machines'
  // clocks ran 4 h 45 min slow.
  test("written 4 h 45 min after it was sent, says when it was delivered and how long it waited", () => {
    let clock = SENT;
    const ages = new CardAges({ now: () => clock });
    const late = card("evt-late", SENT, SENT);
    clock = LATE;
    ages.written(late, SESSION);

    const text = textOf(
      ages.annotate([late], () => [], SESSION),
      "evt-late"
    );

    expect(text).toContain('\n  at: "2026-10-31T06:27:17Z"\n  delivered: "2026-10-31T11:13:02Z"\n');
    expect(text).toContain(
      '\n  waited: "4 h 45 min between at and delivered; at is when it was sent, not the present time"\n'
    );
  });

  test("written at once says when it was delivered, and nothing about a wait", () => {
    const ages = new CardAges({ now: () => SENT + 2_000 });
    const fresh = card("evt-fresh", SENT, SENT + 1_000);
    ages.written(fresh, SESSION);

    const text = textOf(
      ages.annotate([fresh], () => [], SESSION),
      "evt-fresh"
    );

    expect(text).toContain('\n  delivered: "2026-10-31T06:27:19Z"\n');
    expect(text).not.toContain("waited:");
  });

  test("says nothing of a wait under the threshold, and states one at it", () => {
    let clock = SENT;
    const ages = new CardAges({ now: () => clock });
    const under = card("evt-under", SENT, SENT);
    const at = card("evt-at", SENT, SENT);
    clock = SENT + MATERIAL_WAIT_MS - 1_000;
    ages.written(under, SESSION);
    clock = SENT + MATERIAL_WAIT_MS;
    ages.written(at, SESSION);

    const annotated = ages.annotate([under, at], () => [], SESSION);

    expect(textOf(annotated, "evt-under")).not.toContain("waited:");
    expect(textOf(annotated, "evt-at")).toContain('waited: "15 min between at and delivered;');
  });

  // A request's bytes repeat on the next request, so the provider's cache reads them again.
  test("reads the same in every request, however much later the request is made", () => {
    let clock = SENT;
    const ages = new CardAges({ now: () => clock });
    const late = card("evt-late", SENT, SENT);
    clock = LATE;
    ages.written(late, SESSION);
    const first = ages.annotate([late], () => [], SESSION);
    clock = LATE + 3 * 3_600_000;
    const second = ages.annotate([late], () => [], SESSION);

    expect(JSON.stringify(second)).toBe(JSON.stringify(first));
  });

  test("whose message_end came after its first request takes that request's time, and keeps it", () => {
    let clock = SENT;
    const ages = new CardAges({ now: () => clock });
    const handed = card("evt-handed", SENT, SENT + 1_000);
    clock = LATE;
    const first = textOf(
      ages.annotate([handed], () => [], SESSION),
      "evt-handed"
    );
    clock = LATE + 60_000;
    ages.written(handed, SESSION);

    expect(first).toContain('delivered: "2026-10-31T11:13:02Z"');
    expect(
      textOf(
        ages.annotate([handed], () => [], SESSION),
        "evt-handed"
      )
    ).toBe(first);
  });

  test("written before this process started takes the time of its ledger record", () => {
    const old = card("evt-old", SENT, SENT);
    const ages = new CardAges({ now: () => LATE + 86_400_000 });
    const branch = [
      {
        type: "custom",
        customType: REPLAY_DELIVERED_ENTRY,
        data: { event_id: "evt-old", at: LATE },
      },
    ];

    const text = textOf(
      ages.annotate([old], () => branch, SESSION),
      "evt-old"
    );

    expect(text).toContain('delivered: "2026-10-31T11:13:02Z"');
    expect(text).toContain('waited: "4 h 45 min');
  });

  // The process ended after the host wrote the card and before its ledger record was appended: the
  // card's entry carries the time it was handed, so the entries written before it bound the write.
  test("written before this process started, its ledger record lost, takes the newest entry time up to its own entry", () => {
    const old = card("evt-old", SENT, SENT);
    const ages = new CardAges({ now: () => LATE + 86_400_000 });
    const branch = [
      { type: "message", timestamp: "2026-10-31T11:12:58.000Z", message: { role: "assistant" } },
      {
        type: "custom_message",
        customType: "envoy-message",
        timestamp: new Date(SENT).toISOString(),
        details: { event_id: "evt-old" },
        content: old.content,
      },
    ];

    const text = textOf(
      ages.annotate([old], () => branch, SESSION),
      "evt-old"
    );

    expect(text).toContain('delivered: "2026-10-31T11:12:58Z"');
    expect(text).toContain('waited: "4 h 45 min');
  });

  test("handed before this process started and never written into the session is left as it is", () => {
    const old = card("evt-old", SENT, SENT);
    const ages = new CardAges({ now: () => LATE });

    expect(ages.annotate([old], () => [], SESSION)).toBeUndefined();
  });

  // Two copies of the extension can run in one process; the first copy's lines stand.
  test("that another copy already annotated is left as it is", () => {
    let clock = SENT;
    const ages = new CardAges({ now: () => clock });
    const late = card("evt-late", SENT, SENT);
    clock = LATE;
    ages.written(late, SESSION);
    const annotated = ages.annotate([late], () => [], SESSION);
    const other = new CardAges({ now: () => LATE + 60_000 });
    other.written(late, SESSION);

    expect(other.annotate(annotated ?? [], () => [], SESSION)).toBeUndefined();
  });

  test("with pictures is annotated in its text block, and its pictures kept", () => {
    let clock = SENT;
    const ages = new CardAges({ now: () => clock });
    const base = card("evt-pictures", SENT, SENT);
    const image = { type: "image", data: "AAAA", mimeType: "image/png" };
    const withPictures = { ...base, content: [{ type: "text", text: base.content }, image] };
    clock = LATE;
    ages.written(withPictures, SESSION);

    const annotated = ages.annotate([withPictures], () => [], SESSION);
    const blocks = blocksOf(annotated?.[0]);

    expect(textOf(annotated, "evt-pictures")).toContain('delivered: "2026-10-31T11:13:02Z"');
    expect(blocks[1]).toEqual(image);
    expect(withPictures.content[0]).toEqual({ type: "text", text: base.content });
  });

  test("leaves every other message alone, and changes nothing when no card is there", () => {
    const ages = new CardAges({ now: () => LATE });
    const user = {
      role: "user",
      content: [{ type: "text", text: "at: 06:27:17Z" }],
      timestamp: LATE,
    };
    const notice = {
      role: "custom",
      customType: "envoy-message",
      content: "envoy:\n  notice: session id changed",
      display: true,
      details: { local: true },
      timestamp: LATE,
    };

    expect(ages.annotate([user, notice], () => [], SESSION)).toBeUndefined();
  });

  // One process serves one session at a time and moves between them. A session that followed the
  // same issue is shown the event again, and reads it when its own host writes it.
  test("replayed in a session switched to later takes that session's own delivery time", () => {
    let clock = SENT;
    const ages = new CardAges({ now: () => clock });
    const first = card("evt-shared", SENT, SENT);
    ages.written(first, SESSION);
    const early = textOf(
      ages.annotate([first], () => [], SESSION),
      "evt-shared"
    );

    // The host resumes a session that followed the same topic, hours later, and writes the card
    // there: its ledger record in that session's entries dates the write.
    clock = LATE;
    const second = card("evt-shared", SENT, SENT);
    ages.written(second, "ses_switched");
    const branch = [
      {
        type: "custom",
        customType: REPLAY_DELIVERED_ENTRY,
        data: { event_id: "evt-shared", at: LATE },
      },
    ];
    const late = textOf(
      ages.annotate([second], () => branch, "ses_switched"),
      "evt-shared"
    );

    expect(early).toContain('delivered: "2026-10-31T06:27:17Z"');
    expect(early).not.toContain("waited:");
    expect(late).toContain('delivered: "2026-10-31T11:13:02Z"');
    expect(late).toContain('waited: "4 h 45 min');
    // Back on the first session, its own request reads exactly as it did before.
    expect(
      textOf(
        ages.annotate([first], () => branch, SESSION),
        "evt-shared"
      )
    ).toBe(early);
  });

  // A session the host refused a card is shown a notice naming it instead, worded by the replay
  // engine: it dates the send `published_at:`, and reaches the session just as late.
  test("that is a replay failure notice says when the notice was delivered, ordinary or inert", () => {
    let clock = SENT;
    const ages = new CardAges({ now: () => clock });
    const ordinary = failureNotice("evt-notice", {});
    const inert = failureNotice("evt-notice-inert", { btw: true });
    clock = LATE;
    ages.written(ordinary, SESSION);
    ages.written(inert, SESSION);

    const annotated = ages.annotate([ordinary, inert], () => [], SESSION);

    for (const eventID of ["evt-notice", "evt-notice-inert"]) {
      const text = textOf(annotated, eventID);
      expect(text).toContain(
        '\n  published_at: "2026-10-31T06:27:17.000Z"\n  delivered: "2026-10-31T11:13:02Z"\n'
      );
      expect(text).toContain(
        '\n  waited: "4 h 45 min between published_at and delivered; published_at is when it was sent, not the present time"\n'
      );
    }
  });
});

test("a wait reads in hours and minutes", () => {
  expect([duration(52 * 60_000 + 59_000), duration(4 * 3_600_000 + 45 * 60_000)]).toEqual([
    "52 min",
    "4 h 45 min",
  ]);
});
