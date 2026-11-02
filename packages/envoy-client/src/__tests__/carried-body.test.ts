import { describe, expect, test } from "bun:test";
import type { Event } from "@legion/contracts";

import { withWholeEventBody } from "../delivery";
import { DispatchClient } from "../dispatch-http";
import { eventTextsWhole } from "../dispatch-picture-tools";
import { pictureAddresses } from "../dispatch-pictures";

const SERVER = "https://dispatch.example";
const COMMENT_ID = "22222222-2222-4222-8222-222222222222";
const PICTURE = "dispatch://DSP-1/artifact/shot-png@v1";
/** The comment as it is stored: the picture its author embedded sits past where the event's
 *  share of the body ends. */
const STORED_BODY = `The first paragraph.\n\n![shot.png](${PICTURE})`;
const CARRIED_BODY = "The first paragraph.";

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

/** A Dispatch that answers the one comment read, recording each path; `fail` answers it 500
 *  instead, which is how a host meets a Dispatch that is down. */
function dispatch(fail = false) {
  const requests: string[] = [];
  const fetchImpl = async (input: RequestInfo | URL): Promise<Response> => {
    const url = new URL(input instanceof Request ? input.url : input.toString());
    requests.push(url.pathname);
    if (fail) return json({ code: "INTERNAL", error: "down" }, 500);
    if (url.pathname === `/api/v1/comments/${COMMENT_ID}`) {
      return json({ comment: { ...comment, body: STORED_BODY }, replies: [] });
    }
    return json({ code: "NOT_FOUND", error: "not found" }, 404);
  };
  return { requests, client: new DispatchClient(SERVER, "token", fetchImpl as typeof fetch) };
}

const comment = {
  id: COMMENT_ID,
  issue_key: "DSP-1",
  artifact_id: null,
  author: { kind: "user" as const, id: "alice" },
  body: CARRIED_BODY,
  anchor: null,
  reply_to: null,
  ask_id: null,
  turn: null,
  resolved: false,
  resolved_by: null,
  resolved_at: null,
  edited_at: null,
  suggestion: null,
  mentions: [],
  deliveries: [],
  created_at: "2026-09-09T00:00:00Z",
};

const cutEvent: Event = {
  id: 7,
  seq: 7,
  issue_key: "DSP-1",
  type: "comment.created",
  actor: { kind: "user", id: "alice" },
  notify: true,
  created_at: "2026-09-09T00:00:00Z",
  payload: { ...comment, body_truncated: true, artifact_name: "" },
};

const wholeEvent: Event = {
  ...cutEvent,
  id: 8,
  seq: 8,
  payload: { ...comment, body: STORED_BODY, artifact_name: "" },
};

/** One inbound envelope as Dispatch's outbox publishes it, carrying `event`. */
function envelopeOf(event: Event): string {
  return JSON.stringify({
    event_id: "dispatch-7",
    source: "dispatch",
    source_event_id: "7",
    topic: "notifications.dispatch.issue.DSP-1.comment.created",
    dedupe_key: "dispatch-7",
    issued_at: 1_788_756_072_000,
    payload_summary: "DSP-1 comment created",
    payload: JSON.stringify({ ...event, quiet: false }),
    trace_id: "dispatch-7",
  });
}

// A picture an author embedded past where the event's share of the body ends is named nowhere in
// the event, so the body is read whole from the server before its pictures are listed.
describe("pictures of a body an event carried only in part", () => {
  test("the cut body is read whole, so the picture past the cut is found", async () => {
    const { client, requests } = dispatch();

    const texts = await eventTextsWhole(client, [cutEvent]);

    expect(texts).toEqual([{ text: STORED_BODY, at: cutEvent.created_at }]);
    expect(pictureAddresses(texts[0]?.text ?? "")).toEqual([PICTURE]);
    expect(requests).toEqual([`/api/v1/comments/${COMMENT_ID}`]);
  });

  test("a body that rode whole is not read again", async () => {
    const { client, requests } = dispatch();

    const texts = await eventTextsWhole(client, [wholeEvent]);

    expect(texts).toEqual([{ text: STORED_BODY, at: wholeEvent.created_at }]);
    expect(requests).toEqual([]);
  });

  test("a read that fails leaves the event the part it carried", async () => {
    const { client } = dispatch(true);

    expect(await eventTextsWhole(client, [cutEvent])).toEqual([
      { text: CARRIED_BODY, at: cutEvent.created_at },
    ]);
  });
});

// A host that prompts its agent with the frame reads the whole body into the frame first, so the
// agent is given the body the comment route answers rather than a body that stops mid-thought.
describe("withWholeEventBody", () => {
  test("puts the whole body on the frame and drops the flag that said it was cut", async () => {
    const { client, requests } = dispatch();

    const whole = await withWholeEventBody(envelopeOf(cutEvent), () => client);

    const payload = JSON.parse(JSON.parse(whole).payload as string) as {
      payload: Record<string, unknown>;
    };
    expect(payload.payload.body).toBe(STORED_BODY);
    expect(payload.payload.body_truncated).toBeUndefined();
    expect(requests).toEqual([`/api/v1/comments/${COMMENT_ID}`]);
  });

  test("hands the frame back unchanged with no client, with no cut, and when the read fails", async () => {
    const cut = envelopeOf(cutEvent);
    const { client } = dispatch();

    expect(await withWholeEventBody(cut, () => undefined)).toBe(cut);
    expect(await withWholeEventBody(envelopeOf(wholeEvent), () => client)).toBe(
      envelopeOf(wholeEvent)
    );
    expect(await withWholeEventBody(cut, () => dispatch(true).client)).toBe(cut);
  });
});
