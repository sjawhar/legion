import { describe, expect, test } from "bun:test";
import { renderInbound } from "@legion/envoy-client/delivery";
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
    ages.written(late);

    const text = textOf(
      ages.annotate([late], () => []),
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
    ages.written(fresh);

    const text = textOf(
      ages.annotate([fresh], () => []),
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
    ages.written(under);
    clock = SENT + MATERIAL_WAIT_MS;
    ages.written(at);

    const annotated = ages.annotate([under, at], () => []);

    expect(textOf(annotated, "evt-under")).not.toContain("waited:");
    expect(textOf(annotated, "evt-at")).toContain('waited: "15 min between at and delivered;');
  });

  // A request's bytes repeat on the next request, so the provider's cache reads them again.
  test("reads the same in every request, however much later the request is made", () => {
    let clock = SENT;
    const ages = new CardAges({ now: () => clock });
    const late = card("evt-late", SENT, SENT);
    clock = LATE;
    ages.written(late);
    const first = ages.annotate([late], () => []);
    clock = LATE + 3 * 3_600_000;
    const second = ages.annotate([late], () => []);

    expect(JSON.stringify(second)).toBe(JSON.stringify(first));
  });

  test("whose message_end came after its first request takes that request's time, and keeps it", () => {
    let clock = SENT;
    const ages = new CardAges({ now: () => clock });
    const handed = card("evt-handed", SENT, SENT + 1_000);
    clock = LATE;
    const first = textOf(
      ages.annotate([handed], () => []),
      "evt-handed"
    );
    clock = LATE + 60_000;
    ages.written(handed);

    expect(first).toContain('delivered: "2026-10-31T11:13:02Z"');
    expect(
      textOf(
        ages.annotate([handed], () => []),
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
      ages.annotate([old], () => branch),
      "evt-old"
    );

    expect(text).toContain('delivered: "2026-10-31T11:13:02Z"');
    expect(text).toContain('waited: "4 h 45 min');
  });

  test("written before this process started, with no ledger record, is left as it is", () => {
    const old = card("evt-old", SENT, SENT);
    const ages = new CardAges({ now: () => LATE });

    expect(ages.annotate([old], () => [])).toBeUndefined();
  });

  // Two copies of the extension can run in one process; the first copy's lines stand.
  test("that another copy already annotated is left as it is", () => {
    let clock = SENT;
    const ages = new CardAges({ now: () => clock });
    const late = card("evt-late", SENT, SENT);
    clock = LATE;
    ages.written(late);
    const annotated = ages.annotate([late], () => []);
    const other = new CardAges({ now: () => LATE + 60_000 });
    other.written(late);

    expect(other.annotate(annotated ?? [], () => [])).toBeUndefined();
  });

  test("with pictures is annotated in its text block, and its pictures kept", () => {
    let clock = SENT;
    const ages = new CardAges({ now: () => clock });
    const base = card("evt-pictures", SENT, SENT);
    const image = { type: "image", data: "AAAA", mimeType: "image/png" };
    const withPictures = { ...base, content: [{ type: "text", text: base.content }, image] };
    clock = LATE;
    ages.written(withPictures);

    const annotated = ages.annotate([withPictures], () => []);
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

    expect(ages.annotate([user, notice], () => [])).toBeUndefined();
  });
});

test("a wait reads in hours and minutes", () => {
  expect([duration(52 * 60_000 + 59_000), duration(4 * 3_600_000 + 45 * 60_000)]).toEqual([
    "52 min",
    "4 h 45 min",
  ]);
});
