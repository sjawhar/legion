import { describe, expect, test } from "bun:test";
import type { Event } from "@legion/contracts";

import { type CarriedItem, renderInbound, wholeCarriedBody } from "../delivery";
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

/** One inbound envelope as Dispatch's outbox publishes it, carrying `event`; `event` is the
 *  wire event, which a stand-in holds fewer fields of than the schema's own type. */
function envelopeOf(event: object): string {
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

/** The stand-in Dispatch's outbox publishes when the bus refuses an event whole: the payload's
 *  top-level fields short enough to send, and `payload_omitted`, with no body at all. */
const omittedEvent = {
  ...cutEvent,
  id: 9,
  seq: 9,
  payload: {
    id: COMMENT_ID,
    issue_key: "DSP-1",
    author: comment.author,
    created_at: comment.created_at,
    payload_omitted: true,
  },
};

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

// A host that prompts its agent with the frame reads the whole body into the card first:
// `renderInbound` names the item whose body the event carried in part, or not at all, the host
// reads that item, and a second render is given the body it answered.
describe("renderInbound and a body an event did not carry whole", () => {
  const SESSION = "ses_reader";
  const ITEM: CarriedItem = { resource: "comment", id: COMMENT_ID, issueKey: "DSP-1" };

  test("names the cut item, and the body read whole lands on the card with no note", async () => {
    const { client, requests } = dispatch();

    const cut = renderInbound(envelopeOf(cutEvent), SESSION);
    expect(cut.cutItem).toEqual(ITEM);
    expect(cut.content).toContain("the body is its first part only");

    const body = await wholeCarriedBody(client, ITEM, SESSION);
    const whole = renderInbound(envelopeOf(cutEvent), SESSION, undefined, body);

    expect(body).toBe(STORED_BODY);
    expect(requests).toEqual([`/api/v1/comments/${COMMENT_ID}`]);
    expect(whole.content).toContain(`body: ${JSON.stringify(STORED_BODY)}`);
    expect(whole.content).not.toContain("first part only");
    expect(whole.cutItem).toBeUndefined();
  });

  test("names the item of a stand-in whose body the bus dropped, and takes its body too", async () => {
    const { client, requests } = dispatch();

    const omitted = renderInbound(envelopeOf(omittedEvent), SESSION);
    expect(omitted.cutItem).toEqual(ITEM);
    expect(omitted.content).toContain(
      `this event carries no body: it was too large for the bus; read it whole with GET /api/v1/comments/${COMMENT_ID}`
    );

    const body = await wholeCarriedBody(client, ITEM, SESSION);
    const whole = renderInbound(envelopeOf(omittedEvent), SESSION, undefined, body);

    expect(requests).toEqual([`/api/v1/comments/${COMMENT_ID}`]);
    expect(whole.content).toContain(`body: ${JSON.stringify(STORED_BODY)}`);
    expect(whole.content).not.toContain("too large for the bus");
    expect(whole.cutItem).toBeUndefined();
  });

  test("keeps the part it carried, and the note, when the read fails or the body rode whole", async () => {
    const failing = dispatch(true).client;

    expect(await wholeCarriedBody(failing, ITEM, SESSION)).toBeUndefined();
    const kept = renderInbound(envelopeOf(cutEvent), SESSION, undefined, undefined);
    // A text holding no blank line is written as a plain scalar, where the whole body above,
    // which holds one, is written quoted.
    expect(kept.content).toContain(`body: ${CARRIED_BODY}`);
    expect(kept.content).toContain(
      `the body is its first part only; read it whole with GET /api/v1/comments/${COMMENT_ID}`
    );

    const rode = renderInbound(envelopeOf(wholeEvent), SESSION);
    expect(rode.cutItem).toBeUndefined();
    expect(rode.content).not.toContain("first part only");
  });
});
