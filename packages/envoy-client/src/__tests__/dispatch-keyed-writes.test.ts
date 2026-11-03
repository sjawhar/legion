import { beforeEach, describe, expect, test } from "bun:test";
import { type AddressInfo, createServer, type Socket } from "node:net";
import {
  type CommentWriteResponse,
  IDEMPOTENCY_ANSWER_NOT_KEPT,
  IDEMPOTENCY_KEY_COMMITTED,
  IDEMPOTENCY_KEY_IN_FLIGHT,
  IDEMPOTENCY_KEY_PATTERN,
} from "@legion/contracts";
import { bearerCredential } from "../bearer";
import {
  DISPATCH_WRITE_RETRY,
  DispatchClient,
  DispatchGatewayError,
  DispatchServiceError,
  DispatchWriteLandedError,
  type DispatchWriteRetry,
  DispatchWriteUnsettledError,
  forgetKeyedLayerMemory,
  KEYED_LAYER_MEMORY_MS,
} from "../dispatch-http";

// Every write the `dispatch` command makes (a POST or a PATCH) carries one `Idempotency-Key` for
// that call, and asks Dispatch again under it while an attempt gets no definitive answer, so a
// dropped answer never makes the session's write land twice and a short outage never fails it. It
// asks again only against a Dispatch that runs the keyed layer: one whose answer echoed the key,
// or whose `/healthz` says `idempotency_keys`.

interface Sent {
  readonly method: string;
  readonly url: string;
  readonly headers: Record<string, string>;
  readonly body: RequestInit["body"];
}

/** A scripted answer: a response, one made when the request arrives (given its signal), or a
 *  transport failure. */
type Answer =
  | Response
  | ((signal: AbortSignal | null | undefined) => Response | Promise<Response>)
  | Error;

/** What a scripted Dispatch's `/healthz` answers the probe: its JSON body, or a failure. */
type Health = Record<string, unknown> | Error | Response;

/**
 * A fetch that answers each write with the next of `answers`, recording what it was sent, and
 * `/healthz` with `health` (recorded apart, in `probes`). A JSON answer stands for one of
 * Dispatch's own and echoes the request's key, as the keyed layer does, unless `echo` says no for
 * that request (1-based; false for every one is a Dispatch without the layer); an HTML page stands
 * for a gateway's, which never echoes.
 */
function scripted(
  answers: readonly Answer[],
  options: {
    readonly echo?: boolean | ((request: number) => boolean);
    readonly health?: Health;
  } = {}
) {
  const sent: Sent[] = [];
  const probes: string[] = [];
  const health = options.health ?? { status: "ok", idempotency_keys: true };
  const echo = options.echo ?? true;
  const fetchImpl = async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    const url = String(input);
    if (url.endsWith("/healthz")) {
      probes.push(url);
      if (health instanceof Error) throw health;
      return health instanceof Response ? health.clone() : json(health);
    }
    const headers = { ...(init?.headers as Record<string, string>) };
    sent.push({ method: init?.method ?? "GET", url, headers, body: init?.body });
    const answer = answers[sent.length - 1];
    if (answer === undefined) throw new Error(`unexpected request ${sent.length}`);
    if (answer instanceof Error) throw answer;
    const response = typeof answer === "function" ? await answer(init?.signal) : answer;
    const key = headers["Idempotency-Key"];
    const echoes = typeof echo === "function" ? echo(sent.length) : echo;
    if (!echoes || key === undefined || !isDispatchOwn(response)) return response;
    const echoed = new Response(response.body, response);
    echoed.headers.set("Idempotency-Key", key);
    return echoed;
  };
  return { fetchImpl: fetchImpl as typeof fetch, sent, probes };
}

/** Whether a scripted answer stands for Dispatch's own (JSON) rather than a gateway's page. */
function isDispatchOwn(response: Response): boolean {
  return response.headers.get("Content-Type")?.includes("application/json") ?? false;
}

/**
 * The production pacing, with a clock that moves only when the client pauses or the test calls
 * `advance`.
 */
function pacing(policy: Omit<DispatchWriteRetry, "pause" | "now"> = DISPATCH_WRITE_RETRY) {
  const pauses: number[] = [];
  let clock = 0;
  const retry: DispatchWriteRetry = {
    ...policy,
    pause: async (ms) => {
      pauses.push(ms);
      clock += ms;
    },
    now: () => clock,
  };
  return {
    retry,
    pauses,
    advance: (ms: number) => {
      clock += ms;
    },
    clock: () => clock,
  };
}

function json(body: unknown, status = 200, headers: Record<string, string> = {}): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json", ...headers },
  });
}

function refusal(status: number, code: string, error: string, headers = {}): Response {
  return json({ code, error }, status, headers);
}

function page(status: number, statusText: string): Response {
  return new Response(`<html><body><h1>${status} ${statusText}</h1></body></html>`, {
    status,
    statusText,
    headers: { "Content-Type": "text/html" },
  });
}

const inFlight = () =>
  refusal(409, IDEMPOTENCY_KEY_IN_FLIGHT, "an earlier request with this key is still running", {
    "Retry-After": "1",
  });

/** An answer that never comes: the request waits until its signal aborts, then fails with its
 *  reason, as fetch does. */
function hang(signal: AbortSignal | null | undefined): Promise<Response> {
  const { promise, reject } = Promise.withResolvers<Response>();
  signal?.addEventListener("abort", () => reject(signal.reason), { once: true });
  return promise;
}

const actor = { kind: "session" as const, id: "session-1" };
const comment = { body: "Shipped.", actor };
/** What these tests read of a posted comment; the rest of the shape is left out. */
const posted = {
  comment: { id: "comment-1", body: "Shipped." },
} as unknown as CommentWriteResponse;

function client(fetchImpl: typeof fetch, retry: DispatchWriteRetry, signal?: AbortSignal) {
  return new DispatchClient(
    "http://dispatch.test",
    bearerCredential("secret"),
    fetchImpl,
    signal,
    retry
  );
}

function failure(call: Promise<unknown>): Promise<Error> {
  return call.then(
    () => {
      throw new Error("expected the call to fail");
    },
    (error: Error) => error
  );
}

describe("DispatchClient's writes", () => {
  // What a Dispatch runs is learned per process and per origin; each test drives its own.
  beforeEach(forgetKeyedLayerMemory);

  test("send one Idempotency-Key per write call and none on a read, PUT or DELETE", async () => {
    const { fetchImpl, sent } = scripted(Array.from({ length: 8 }, () => json({})));
    const dispatch = client(fetchImpl, pacing().retry);

    await dispatch.comment("DSP-1", comment);
    await dispatch.comment("DSP-1", comment);
    await dispatch.updateIssue("DSP-1", { status: "testing", actor });
    await dispatch.syncArchitectureSource("DSP", "acme/infra");
    await dispatch.writeHandoff("DSP-1", "implement", { actor, body: '{"schemaVersion":1}' });
    await dispatch.getIssue("DSP-1");
    await dispatch.followAsk("ask-1", "session-1", actor);
    await dispatch.unfollowAsk("ask-1", "session-1", actor);

    const keys = sent.map((request): string | undefined => request.headers["Idempotency-Key"]);
    expect(sent.map((request) => request.method)).toEqual([
      "POST",
      "POST",
      "PATCH",
      "POST",
      "POST",
      "GET",
      "PUT",
      "DELETE",
    ]);
    for (const key of keys.slice(0, 5)) expect(key).toMatch(IDEMPOTENCY_KEY_PATTERN);
    // Two presses are two writes: the same comment posted twice is two intents.
    expect(new Set(keys.slice(0, 5)).size).toBe(5);
    expect(keys.slice(5)).toEqual([undefined, undefined, undefined]);
  });

  // A caller whose intent outlives the call names the key itself: AskGate's decision document is
  // created once per session, whatever process reaches for it and however many times, and each of
  // its records is appended once (packages/pi-envoy/extensions/askgate/decision-record.ts). The
  // key it gives goes on every attempt of that write, and on every call it is given to, so
  // Dispatch runs that intent once and replays it rather than writing a second time.
  test("send a caller's own key on every attempt of its write, and mint one only where it gives none", async () => {
    const { fetchImpl, sent } = scripted([
      new TypeError("fetch failed"),
      refusal(503, "UNAVAILABLE", "dispatch is restarting"),
      json({ artifact: { id: "doc-1" } }),
      json({ applied: 1 }),
      json({}),
    ]);
    const dispatch = client(fetchImpl, pacing().retry);
    const create = "askgate-doc.3f7a91";
    const append = "askgate-rec.0c5e22";

    await dispatch.projectArtifact(
      "DSP",
      { name: "askgate-decisions-sess-1.md", content: "# Decisions", actor },
      create
    );
    await dispatch.docEdit(
      "doc-1",
      { ops: [{ op: "insert", markdown: "```json\n{}\n```", after: "end" }], actor },
      append
    );
    await dispatch.comment("DSP-1", comment);

    const keys = sent.map((request): string | undefined => request.headers["Idempotency-Key"]);
    // The three attempts of the create are one intent, and the key the caller chose is each one's.
    expect(keys.slice(0, 4)).toEqual([create, create, create, append]);
    expect(keys[4]).toMatch(IDEMPOTENCY_KEY_PATTERN);
    expect(keys[4]).not.toBe(create);
  });

  test("ask again under the same key through a transport failure, a 5xx, a 429 and 409 KEY_IN_FLIGHT, answering the first definitive answer", async () => {
    const { fetchImpl, sent } = scripted([
      new TypeError("fetch failed"),
      page(502, "Bad Gateway"),
      page(429, "Too Many Requests"),
      inFlight(),
      json(posted, 201, { "Idempotency-Replayed": "?1" }),
    ]);
    const { retry, pauses } = pacing();

    await expect(client(fetchImpl, retry).comment("DSP-1", comment)).resolves.toEqual(posted);

    expect(sent).toHaveLength(5);
    const key = sent[0]?.headers["Idempotency-Key"];
    expect(key).toBeString();
    for (const request of sent) {
      expect(request.method).toBe("POST");
      expect(request.url).toBe("http://dispatch.test/api/v1/issues/DSP-1/comments");
      expect(request.headers["Idempotency-Key"]).toBe(key as string);
      expect(request.body).toBe(JSON.stringify(comment));
    }
    // After KEY_IN_FLIGHT the write is asked at most 3 s later, not on the schedule's 8 s.
    expect(pauses).toEqual([1_000, 2_000, 4_000, 3_000]);
  });

  // An answer that echoed the key is itself the proof that this Dispatch runs the keyed layer, so
  // a failure it carries is resent without asking anything.
  test("resend an echoed failure without asking /healthz", async () => {
    const { fetchImpl, sent, probes } = scripted([
      refusal(503, "UNAVAILABLE", "database unavailable"),
      json(posted, 201),
    ]);

    await expect(client(fetchImpl, pacing().retry).comment("DSP-1", comment)).resolves.toEqual(
      posted
    );
    expect(sent).toHaveLength(2);
    expect(probes).toEqual([]);
  });

  // A Dispatch without the layer would run a repeat as a second write, and says nothing: it echoes
  // no key and its /healthz has no idempotency_keys. Every failure it can give is the call's.
  test("ask a Dispatch that runs no keyed layer once, whatever the failure", async () => {
    for (const answer of [
      refusal(500, "INTERNAL", "internal server error"),
      refusal(429, "RATE_LIMITED", "slow down"),
      page(502, "Bad Gateway"),
      new TypeError("fetch failed"),
    ] as const) {
      forgetKeyedLayerMemory();
      const { fetchImpl, sent, probes } = scripted([answer, json(posted, 201)], {
        echo: false,
        health: { status: "ok" },
      });
      const { retry, pauses } = pacing();

      await failure(client(fetchImpl, retry).comment("DSP-1", comment));

      expect(sent).toHaveLength(1);
      expect(pauses).toEqual([]);
      expect(probes).toHaveLength(1);
    }
  });

  // A gateway's answer and a transport failure carry no echo, so the write asks /healthz before it
  // resends: `idempotency_keys` says the repeat is answered from the key rather than run again.
  test("ask /healthz once, then resend under the key when it says idempotency_keys", async () => {
    const { fetchImpl, sent, probes } = scripted(
      [
        page(502, "Bad Gateway"),
        json(posted, 201, { "Idempotency-Replayed": "?1" }),
        new TypeError("fetch failed"),
        json(posted, 201),
      ],
      { echo: false }
    );
    const dispatch = client(fetchImpl, pacing().retry);

    await expect(dispatch.comment("DSP-1", comment)).resolves.toEqual(posted);
    expect(probes).toEqual(["http://dispatch.test/healthz"]);
    expect(sent.map((request) => request.headers["Idempotency-Key"])).toEqual([
      sent[0]?.headers["Idempotency-Key"] as string,
      sent[0]?.headers["Idempotency-Key"] as string,
    ]);

    // The second write of the process knows already, and asks nothing.
    await expect(dispatch.comment("DSP-1", comment)).resolves.toEqual(posted);
    expect(probes).toHaveLength(1);
    expect(sent).toHaveLength(4);
  });

  test("report the failure when the probe fails, times out or answers no JSON", async () => {
    for (const health of [
      new TypeError("fetch failed"),
      new DOMException("The operation timed out.", "TimeoutError"),
      new Response("<html><body>healthz</body></html>", {
        headers: { "Content-Type": "text/html" },
      }),
      new Response("{}", { status: 503, headers: { "Content-Type": "application/json" } }),
    ] as const) {
      forgetKeyedLayerMemory();
      const { fetchImpl, sent, probes } = scripted([page(502, "Bad Gateway"), json(posted, 201)], {
        echo: false,
        health,
      });

      const error = await failure(client(fetchImpl, pacing().retry).comment("DSP-1", comment));

      expect(sent).toHaveLength(1);
      expect(probes).toHaveLength(1);
      expect(error).toBeInstanceOf(DispatchGatewayError);
      expect(error).toEqual(expect.objectContaining({ mayHaveReachedDispatch: true }));
    }
  });

  // Dispatch answers /healthz 503 whenever its database or NATS is down - the outage a failed
  // write most likely met - and still says idempotency_keys, a fact of its binary: the probe reads
  // the body whatever the status.
  test("resend after a /healthz that answers 503 with idempotency_keys", async () => {
    for (const first of [
      refusal(503, "STORE_UNAVAILABLE", "database unavailable"),
      page(502, "Bad Gateway"),
    ]) {
      forgetKeyedLayerMemory();
      const { fetchImpl, sent, probes } = scripted([first, json(posted, 201)], {
        echo: (request) => request > 1,
        health: json({ ok: false, db: false, nats: true, idempotency_keys: true }, 503),
      });

      await expect(client(fetchImpl, pacing().retry).comment("DSP-1", comment)).resolves.toEqual(
        posted
      );
      expect(sent).toHaveLength(2);
      expect(new Set(sent.map((request) => request.headers["Idempotency-Key"])).size).toBe(1);
      expect(probes).toHaveLength(1);
    }
  });

  // A yes, from an echo or from /healthz, holds for KEYED_LAYER_MEMORY_MS from the answer that
  // showed it; past that a failure that says nothing asks /healthz again, so a rollback to a
  // Dispatch without the layer is noticed within that bound.
  test("remember a yes for five minutes, then ask /healthz again", async () => {
    const probed = scripted(
      [
        page(502, "Bad Gateway"),
        json(posted, 201),
        page(502, "Bad Gateway"),
        json(posted, 201),
        page(502, "Bad Gateway"),
        json(posted, 201),
      ],
      { echo: false }
    );
    const { retry, advance } = pacing();
    // A client lives at most its 60 s deadline, so each call five minutes apart has its own; what
    // the process learned of the layer outlives them.
    const dispatch = () => client(probed.fetchImpl, retry);

    // The probe answers at 0 s, before the 1 s pause.
    await expect(dispatch().comment("DSP-1", comment)).resolves.toEqual(posted);
    expect(probed.probes).toHaveLength(1);
    advance(KEYED_LAYER_MEMORY_MS - 2_000);
    await expect(dispatch().comment("DSP-1", comment)).resolves.toEqual(posted);
    expect(probed.probes).toHaveLength(1);
    advance(1);
    await expect(dispatch().comment("DSP-1", comment)).resolves.toEqual(posted);
    expect(probed.probes).toHaveLength(2);
    expect(probed.sent).toHaveLength(6);

    // An echo is remembered the same way: once it is older, a gateway's failure asks again, and a
    // Dispatch that now says nothing of the layer is not resent to.
    forgetKeyedLayerMemory();
    const echoed = scripted(
      [json(posted, 201), page(502, "Bad Gateway"), json(posted, 201), page(502, "Bad Gateway")],
      { health: { status: "ok" } }
    );
    const clock = pacing();
    const rolledBack = () => client(echoed.fetchImpl, clock.retry);
    await rolledBack().comment("DSP-1", comment);
    clock.advance(KEYED_LAYER_MEMORY_MS - 1);
    await expect(rolledBack().comment("DSP-1", comment)).resolves.toEqual(posted);
    expect(echoed.probes).toEqual([]);
    clock.advance(KEYED_LAYER_MEMORY_MS);
    await expect(failure(rolledBack().comment("DSP-1", comment))).resolves.toBeInstanceOf(
      DispatchGatewayError
    );
    expect(echoed.probes).toHaveLength(1);
    expect(echoed.sent).toHaveLength(4);
  });

  // Only a yes is kept: a Dispatch upgraded to the layer is believed the next time a write fails.
  test("ask /healthz again for each failed write while it says no layer", async () => {
    const { fetchImpl, sent, probes } = scripted(
      [page(502, "Bad Gateway"), page(502, "Bad Gateway")],
      { echo: false, health: { status: "ok" } }
    );
    const dispatch = client(fetchImpl, pacing().retry);

    await failure(dispatch.comment("DSP-1", comment));
    await failure(dispatch.comment("DSP-1", comment));

    expect(sent).toHaveLength(2);
    expect(probes).toHaveLength(2);
  });

  // Dispatch's own refusal that carries no key back says its keyed layer did not judge the
  // request - a release without the layer, as after a rollback - so an echo the process saw
  // earlier does not license a resend: /healthz decides afresh.
  test("ask /healthz afresh on Dispatch's own refusal that echoes no key", async () => {
    const { fetchImpl, sent, probes } = scripted(
      [json(posted, 201), refusal(500, "INTERNAL", "internal server error"), json(posted, 201)],
      { echo: (request) => request === 1, health: { status: "ok" } }
    );
    const dispatch = client(fetchImpl, pacing().retry);

    await dispatch.comment("DSP-1", comment);
    const error = await failure(dispatch.comment("DSP-1", comment));

    expect(error).toEqual(expect.objectContaining({ code: "INTERNAL", status: 500 }));
    expect(sent).toHaveLength(2);
    expect(probes).toHaveLength(1);
  });

  test("ask again after a gateway's 408 and Dispatch's own 5xx", async () => {
    const { fetchImpl, sent } = scripted([
      page(408, "Request Timeout"),
      refusal(500, "INTERNAL", "internal server error"),
      refusal(503, "UNAVAILABLE", "database unavailable"),
      json({ key: "DSP-1", status: "testing" }),
    ]);

    await expect(
      client(fetchImpl, pacing().retry).updateIssue("DSP-1", { status: "testing", actor })
    ).resolves.toEqual({ key: "DSP-1", status: "testing" } as never);
    expect(sent.map((request) => request.method)).toEqual(["PATCH", "PATCH", "PATCH", "PATCH"]);
    expect(new Set(sent.map((request) => request.headers["Idempotency-Key"])).size).toBe(1);
  });

  test("answer any other refusal at once, KEY_REUSED and other 409s included", async () => {
    for (const answer of [
      refusal(422, "KEY_REUSED", "this key was used for another request"),
      refusal(409, "ISSUE_CLAIMED", "session-2 holds DSP-1"),
      refusal(400, "INVALID_COMMENT", "body must not be blank"),
      refusal(404, "ISSUE_NOT_FOUND", "no such issue"),
      page(404, "Not Found"),
    ]) {
      const { fetchImpl, sent } = scripted([answer]);
      const { retry, pauses } = pacing();

      const error = await failure(client(fetchImpl, retry).comment("DSP-1", comment));

      expect(error).toBeInstanceOf(DispatchServiceError);
      expect(error).toEqual(expect.objectContaining({ status: answer.status }));
      expect(sent).toHaveLength(1);
      expect(pauses).toEqual([]);
    }
  });

  // A status that carries no body - a 304 - cannot be rebuilt around one: the Fetch standard's
  // `new Response` refuses a body for it, as Node's and every browser's does. Bun's accepts one, so
  // the test holds the client to the standard's constructor; the answer is handed on as it came,
  // and the call fails with Dispatch's status rather than with the client's own TypeError.
  test("answer a bodiless refusal at once, as it came", async () => {
    const { fetchImpl, sent } = scripted([
      new Response(null, { status: 304, statusText: "Not Modified" }),
    ]);
    const { retry, pauses } = pacing();
    const BunResponse = globalThis.Response;
    globalThis.Response = class extends BunResponse {
      constructor(body?: BodyInit | null, init?: ResponseInit) {
        if (
          body !== null &&
          body !== undefined &&
          [101, 204, 205, 304].includes(init?.status ?? 200)
        ) {
          throw new TypeError(`Response constructor: a ${init?.status} response has no body`);
        }
        super(body, init);
      }
    } as typeof Response;
    try {
      const error = await failure(client(fetchImpl, retry).comment("DSP-1", comment));

      expect(error).not.toBeInstanceOf(TypeError);
      expect(error).toEqual(expect.objectContaining({ status: 304 }));
      expect(sent).toHaveLength(1);
      expect(pauses).toEqual([]);
    } finally {
      globalThis.Response = BunResponse;
    }
  });

  test("a 409 answered after a retry keeps its code, candidates and message", async () => {
    const candidates = [{ from: 1, to: 2, context: "same" }];
    const { fetchImpl } = scripted([
      page(502, "Bad Gateway"),
      json({ error: "quote is ambiguous", code: "TARGET_AMBIGUOUS", candidates }, 409),
    ]);

    await expect(
      client(fetchImpl, pacing().retry).docEdit("artifact-1", {
        ops: [{ op: "delete", find: "same" }],
        actor,
      })
    ).rejects.toEqual(
      expect.objectContaining({
        name: "DispatchServiceError",
        code: "TARGET_AMBIGUOUS",
        status: 409,
        message: "quote is ambiguous",
        candidates,
      })
    );
  });

  test("stop after five attempts and throw the last attempt's refusal as it stands", async () => {
    const { fetchImpl, sent } = scripted(Array.from({ length: 5 }, () => page(502, "Bad Gateway")));
    const { retry, pauses } = pacing();

    const error = await failure(client(fetchImpl, retry).comment("DSP-1", comment));

    expect(sent).toHaveLength(5);
    expect(pauses).toEqual([1_000, 2_000, 4_000, 8_000]);
    expect(error).toBeInstanceOf(DispatchGatewayError);
    expect(error.message).toBe(
      "POST http://dispatch.test/api/v1/issues/DSP-1/comments answered 502 Bad Gateway with a " +
        `body that is not Dispatch's error JSON ("502 Bad Gateway"), which looks like a proxy or ` +
        "gateway page rather than Dispatch's own answer, so the write may or may not have reached " +
        "Dispatch: check whether it took effect before retrying it."
    );
  });

  test("a transport failure on every attempt throws the last one", async () => {
    const { fetchImpl, sent } = scripted(
      Array.from({ length: 5 }, (_, n) => new TypeError(`fetch failed ${n + 1}`))
    );

    const error = await failure(client(fetchImpl, pacing().retry).comment("DSP-1", comment));

    expect(sent).toHaveLength(5);
    expect(error).toBeInstanceOf(TypeError);
    expect(error.message).toBe("fetch failed 5");
  });

  test("wait a Retry-After longer than the pause, and stop rather than start past 30 s", async () => {
    const { fetchImpl, sent } = scripted([
      refusal(429, "RATE_LIMITED", "slow down", { "Retry-After": "5" }),
      page(503, "Service Unavailable"),
      refusal(429, "RATE_LIMITED", "slow down", { "Retry-After": "40" }),
    ]);
    const { retry, pauses } = pacing();

    const error = await failure(client(fetchImpl, retry).comment("DSP-1", comment));

    // 5 s honours the first Retry-After; 2 s is the schedule's; 40 s would start past 30 s.
    expect(pauses).toEqual([5_000, 2_000]);
    expect(sent).toHaveLength(3);
    expect(error).toBeInstanceOf(DispatchWriteUnsettledError);
    expect((error as DispatchWriteUnsettledError).last).toEqual(
      expect.objectContaining({ code: "RATE_LIMITED", status: 429 })
    );
  });

  test("stop at the 30 s bound however few attempts that leaves", async () => {
    const { fetchImpl, sent } = scripted(Array.from({ length: 5 }, () => page(503, "Unavailable")));
    const { retry, pauses } = pacing({
      ...DISPATCH_WRITE_RETRY,
      pausesMs: [10_000, 15_000, 20_000],
    });

    await failure(client(fetchImpl, retry).comment("DSP-1", comment));

    expect(pauses).toEqual([10_000, 15_000]);
    expect(sent).toHaveLength(3);
  });

  test("say a write may have landed when the last refusal alone says it did not", async () => {
    const after502 = scripted([
      page(502, "Bad Gateway"),
      ...Array.from({ length: 4 }, () => page(429, "Too Many Requests")),
    ]);
    const landed = await failure(
      client(after502.fetchImpl, pacing().retry).comment("DSP-1", comment)
    );

    expect(landed).toBeInstanceOf(DispatchWriteUnsettledError);
    expect(landed).not.toBeInstanceOf(DispatchServiceError);
    expect((landed as DispatchWriteUnsettledError).attempts).toBe(5);
    expect((landed as DispatchWriteUnsettledError).last).toBeInstanceOf(DispatchGatewayError);
    // /healthz said this Dispatch runs the keyed layer, so the message says it finishes the write.
    expect(landed.message).toBe(
      "POST http://dispatch.test/api/v1/issues/DSP-1/comments answered 429 Too Many Requests " +
        `with a body that is not Dispatch's error JSON ("429 Too Many Requests"), which looks like ` +
        "a proxy or gateway page rather than Dispatch's own answer (the last of 5 attempts), and an " +
        "earlier attempt of the write may have reached Dispatch, so it may or may not have taken " +
        "effect. Dispatch finishes an attempt it received whatever this command does, so read the " +
        "write back before you send it again: a new command sends it under a new key."
    );

    // KEY_IN_FLIGHT says Dispatch may still be running the write, so it is asked every 3 s at most
    // until 55 s: first at 0 s, then at 1 and 3 s on the schedule, then every 3 s from 6 to 54 s.
    const running = scripted([
      new TypeError("fetch failed"),
      ...Array.from({ length: 30 }, () => inFlight()),
    ]);
    const stillRunning = await failure(
      client(running.fetchImpl, pacing().retry).comment("DSP-1", comment)
    );

    expect(running.sent).toHaveLength(20);
    expect(stillRunning).toBeInstanceOf(DispatchWriteUnsettledError);
    expect(stillRunning.message).toBe(
      "KEY_IN_FLIGHT (the last of 20 " +
        "attempts), and Dispatch was still running an earlier attempt of the write, so it may or " +
        "may not have taken effect. Dispatch finishes an attempt it received whatever this " +
        "command does, so read the write back before you send it again: a new command sends it " +
        "under a new key."
    );
  });

  test("a write no attempt of which can have reached Dispatch keeps the last refusal", async () => {
    const { fetchImpl } = scripted(Array.from({ length: 5 }, () => page(429, "Too Many Requests")));

    const error = await failure(client(fetchImpl, pacing().retry).comment("DSP-1", comment));

    expect(error).toBeInstanceOf(DispatchGatewayError);
    expect(error).toEqual(expect.objectContaining({ mayHaveReachedDispatch: false }));
  });

  // The call's deadline is the caller's limit, never a reason to ask again. A write it ends while
  // an attempt is out, which may have reached Dispatch, may or may not have taken effect, and says
  // so; the caller's own cancellation is thrown as it is.
  test("the deadline or the caller's cancellation ends a write's attempts", async () => {
    const hung = scripted([hang]);
    const timeout = await failure(
      client(hung.fetchImpl, pacing().retry, AbortSignal.timeout(30)).comment("DSP-1", comment)
    );
    expect(timeout).toBeInstanceOf(DispatchWriteUnsettledError);
    expect((timeout as DispatchWriteUnsettledError).last.name).toBe("TimeoutError");
    expect(timeout.message).toBe(
      "POST http://dispatch.test/api/v1/issues/DSP-1/comments had no definitive answer when its " +
        "deadline ended it (its only attempt), and an attempt of the write may have reached " +
        "Dispatch, so it may or may not have taken effect: check whether it did before retrying it."
    );
    expect(hung.sent).toHaveLength(1);

    // A timeout the fetch reports of its own is a deadline too.
    const timedOut = scripted([
      () => {
        throw new DOMException("The operation timed out.", "TimeoutError");
      },
    ]);
    const thrown = await failure(
      client(timedOut.fetchImpl, pacing().retry).comment("DSP-1", comment)
    );
    expect(thrown).toBeInstanceOf(DispatchWriteUnsettledError);
    expect(timedOut.sent).toHaveLength(1);

    const controller = new AbortController();
    const cancelled = scripted([page(502, "Bad Gateway"), json(posted, 201)]);
    const paused: DispatchWriteRetry = {
      ...DISPATCH_WRITE_RETRY,
      pause: (_ms, signal) => {
        controller.abort(new DOMException("The operation was aborted.", "AbortError"));
        return Promise.reject(signal.reason);
      },
    };
    const aborted = await failure(
      client(cancelled.fetchImpl, paused, controller.signal).comment("DSP-1", comment)
    );
    expect(aborted.name).toBe("AbortError");
    expect(cancelled.sent).toHaveLength(1);
  });

  test("the production pause waits out its time and stops when the signal aborts", async () => {
    const { fetchImpl, sent } = scripted([page(502, "Bad Gateway"), json(posted, 201)]);
    const started = performance.now();

    await expect(
      client(fetchImpl, { ...DISPATCH_WRITE_RETRY, pausesMs: [50], withinMs: 1_000 }).comment(
        "DSP-1",
        comment
      )
    ).resolves.toEqual(posted);
    expect(performance.now() - started).toBeGreaterThanOrEqual(45);
    expect(sent).toHaveLength(2);

    // The deadline ends the pause after a 502 that may have reached Dispatch.
    const stalled = scripted([page(502, "Bad Gateway")]);
    const stop = AbortSignal.timeout(50);
    const startedStalled = performance.now();
    const error = await failure(
      client(
        stalled.fetchImpl,
        { ...DISPATCH_WRITE_RETRY, pausesMs: [30_000], withinMs: 120_000 },
        stop
      ).comment("DSP-1", comment)
    );
    expect(error).toBeInstanceOf(DispatchWriteUnsettledError);
    expect((error as DispatchWriteUnsettledError).last.name).toBe("TimeoutError");
    expect(performance.now() - startedStalled).toBeLessThan(5_000);
  });

  // An attempt that hears nothing for silenceMs is cut off and asked again under its key, so a
  // write Dispatch is slow to finish gets its KEY_IN_FLIGHT and then its replay inside the call's
  // deadline, rather than a bare timeout at its end. The bound is a real timer aborting the fetch's
  // own signal, so these two run it for tens of milliseconds, as the pause test above does.
  test("cut off an attempt silent for silenceMs and ask again under its key", async () => {
    const { fetchImpl, sent, probes } = scripted([
      hang,
      inFlight(),
      json(posted, 201, { "Idempotency-Replayed": "?1" }),
    ]);
    const { retry, pauses } = pacing({ ...DISPATCH_WRITE_RETRY, silenceMs: 50 });
    const started = performance.now();

    await expect(client(fetchImpl, retry).comment("DSP-1", comment)).resolves.toEqual(posted);

    expect(performance.now() - started).toBeGreaterThanOrEqual(45);
    expect(sent).toHaveLength(3);
    expect(new Set(sent.map((request) => request.headers["Idempotency-Key"])).size).toBe(1);
    expect(pauses).toEqual([1_000, 2_000]);
    // The cut says nothing of the keyed layer, so /healthz is asked before the first resend.
    expect(probes).toHaveLength(1);
  });

  /**
   * The production pacing for a write whose attempts stall. The silence cut is a real timer, so it
   * fires here after 20 ms, and the client's clock moves by the production `silenceMs` when it
   * does, as 20 s of real silence would move it; every window and pause is production's.
   */
  function stalling() {
    const paced = pacing({ ...DISPATCH_WRITE_RETRY, silenceMs: 20 });
    const stall = (signal: AbortSignal | null | undefined): Promise<Response> => {
      signal?.addEventListener("abort", () => paced.advance(DISPATCH_WRITE_RETRY.silenceMs), {
        once: true,
      });
      return hang(signal);
    };
    return { ...paced, stall };
  }

  const replayed = () => json(posted, 201, { "Idempotency-Replayed": "?1" });

  // Measured on 2026-11-02 from the transcript store: one write cut off for silence took 52 s
  // when it was sent again, and succeeded. Dispatch finishes a keyed write whatever its client
  // does, so the call keeps asking under its key, each repeat answered 409 KEY_IN_FLIGHT, until
  // the first run's stored answer comes back. Before, it stopped asking 30 s after it began.
  test("collect under its key a write Dispatch finishes 52 s after it began, past a silence cut", async () => {
    const { retry, pauses, clock, stall } = stalling();
    const finished = () => (clock() < 52_000 ? inFlight() : replayed());
    const { fetchImpl, sent } = scripted([stall, ...Array.from({ length: 20 }, () => finished)]);

    await expect(client(fetchImpl, retry).comment("DSP-1", comment)).resolves.toEqual(posted);

    expect(new Set(sent.map((request) => request.headers["Idempotency-Key"])).size).toBe(1);
    // Cut at 20 s, then asked 1 s later and at most every 3 s while the run lives: the replay is
    // collected at 53 s, inside the 55 s the call keeps for a write Dispatch may still be running.
    expect(pauses).toEqual([1_000, 2_000, ...Array.from({ length: 10 }, () => 3_000)]);
    expect(clock()).toBe(53_000);
  });

  test("ask again under its key after one silent attempt", async () => {
    const { retry, pauses, clock, stall } = stalling();
    const { fetchImpl, sent } = scripted([stall, replayed()]);

    await expect(client(fetchImpl, retry).comment("DSP-1", comment)).resolves.toEqual(posted);

    expect(sent).toHaveLength(2);
    expect(pauses).toEqual([1_000]);
    expect(clock()).toBe(21_000);
  });

  test("ask again under its key after two silent attempts in a row", async () => {
    const { retry, pauses, clock, stall } = stalling();
    const { fetchImpl, sent } = scripted([stall, stall, replayed()]);

    await expect(client(fetchImpl, retry).comment("DSP-1", comment)).resolves.toEqual(posted);

    expect(sent).toHaveLength(3);
    expect(pauses).toEqual([1_000, 2_000]);
    expect(clock()).toBe(43_000);
  });

  test("a write whose every attempt is silent may or may not have taken effect", async () => {
    const { retry, stall } = stalling();
    const { fetchImpl, sent } = scripted(Array.from({ length: 5 }, () => stall));

    const error = await failure(client(fetchImpl, retry).comment("DSP-1", comment));

    // Cut at 20, 41 and 63 s; no ask starts past 55 s.
    expect(sent).toHaveLength(3);
    expect(error).toBeInstanceOf(DispatchWriteUnsettledError);
    expect(error.message).toBe(
      "POST http://dispatch.test/api/v1/issues/DSP-1/comments made no progress for 0.02 s, so it " +
        "was cut off (the last of 3 attempts), and an attempt of the write may have reached " +
        "Dispatch, so it may or may not have taken effect. Dispatch finishes an attempt it " +
        "received whatever this command does, so read the write back before you send it again: " +
        "a new command sends it under a new key."
    );
  });

  // A Dispatch not known to run the keyed layer is never asked twice, however long the call has.
  test("a silent attempt against a Dispatch not known to run the keyed layer is its only one", async () => {
    const { retry, stall } = stalling();
    const { fetchImpl, sent, probes } = scripted([stall, replayed()], {
      health: { status: "ok" },
    });

    const error = await failure(client(fetchImpl, retry).comment("DSP-1", comment));

    expect(sent).toHaveLength(1);
    expect(probes).toHaveLength(1);
    expect(error.message).toBe(
      "POST http://dispatch.test/api/v1/issues/DSP-1/comments made no progress for 0.02 s, so it " +
        "was cut off (its only attempt), and an attempt of the write may have reached Dispatch, " +
        "so it may or may not have taken effect: check whether it did before retrying it."
    );
  });

  const keyedTail =
    ". Dispatch finishes an attempt it received whatever this command does, so read the write " +
    "back before you send it again: a new command sends it under a new key.";

  // The client's 60 s deadline counts from when it was made, so a write that begins late in the
  // client's life asks no later than 55 s after that, not 55 s after its own first attempt.
  test("no ask starts more than 55 s after the client was made", async () => {
    const { retry, advance, stall } = stalling();
    const { fetchImpl, sent } = scripted([stall, ...Array.from({ length: 20 }, () => inFlight())]);
    const dispatch = client(fetchImpl, retry);
    // Earlier calls of the same client took 30 s of its deadline.
    advance(30_000);

    const error = await failure(dispatch.comment("DSP-1", comment));

    // Cut at 50 s, then asked at 51 and 53 s; the next ask would start at 56 s.
    expect(sent).toHaveLength(3);
    expect(error.message).toBe(
      "KEY_IN_FLIGHT (the last of 3 " +
        "attempts), and Dispatch was still running an earlier attempt of the write, so it may or " +
        `may not have taken effect${keyedTail}`
    );
  });

  // The /healthz probe runs after the bound was read and takes time of its own, up to its 5 s.
  test("the bound is read again after the /healthz probe answers", async () => {
    const { retry, advance, stall } = stalling();
    const dispatch = scripted([stall, replayed()]);
    const slowProbe = (async (input: RequestInfo | URL, init?: RequestInit) => {
      if (String(input).endsWith("/healthz")) advance(4_000);
      return dispatch.fetchImpl(input, init);
    }) as typeof fetch;
    const late = client(slowProbe, retry);
    advance(31_000);

    const error = await failure(late.comment("DSP-1", comment));

    // Cut at 51 s, the probe answers at 55 s, and an ask a second later would pass the bound.
    expect(dispatch.sent).toHaveLength(1);
    expect(dispatch.probes).toHaveLength(1);
    expect(error.message).toBe(
      "POST http://dispatch.test/api/v1/issues/DSP-1/comments made no progress for 0.02 s, so it " +
        "was cut off (its only attempt), and an attempt of the write may have reached Dispatch, " +
        `so it may or may not have taken effect${keyedTail}`
    );
  });

  // A timer can fire late on a loaded host, so a pause that ends past the bound starts no ask.
  test("a pause that ends past the bound starts no ask", async () => {
    const { retry, advance, clock, stall } = stalling();
    const { fetchImpl, sent } = scripted([stall, ...Array.from({ length: 20 }, () => inFlight())]);
    const late: DispatchWriteRetry = {
      ...retry,
      pause: async (ms) => {
        advance(ms);
        if (clock() > 52_000) advance(3_000);
      },
    };

    await failure(client(fetchImpl, late).comment("DSP-1", comment));

    // Asked at 0, 21, 23, then every 3 s to 50 s; the pause after 50 s ends at 56 s.
    expect(sent).toHaveLength(12);
    expect(clock()).toBe(56_000);
  });

  // Whether the write may be asked again is decided afresh at each answer; that the call saw the
  // keyed layer judge it, which the message says, is never forgotten.
  test("a refusal the keyed layer did not judge stops the asking but keeps what the call saw", async () => {
    const { fetchImpl, sent, probes } = scripted(
      [inFlight(), refusal(429, "RATE_LIMITED", "slow down"), replayed()],
      { echo: (request) => request === 1, health: { status: "ok" } }
    );

    const error = await failure(client(fetchImpl, pacing().retry).comment("DSP-1", comment));

    expect(sent).toHaveLength(2);
    expect(probes).toHaveLength(1);
    expect(error).toBeInstanceOf(DispatchWriteUnsettledError);
    expect(error.message).toBe(
      "RATE_LIMITED: slow down (the last of 2 attempts), and an earlier attempt of the write may " +
        `have reached Dispatch, so it may or may not have taken effect${keyedTail}`
    );
  });

  // Each repeat of an upload sends its whole body again, so an upload keeps the schedule alone.
  test("an upload Dispatch may still be running keeps the five-attempt schedule", async () => {
    const { retry, pauses, stall } = stalling();
    const { fetchImpl, sent } = scripted([stall, ...Array.from({ length: 20 }, () => inFlight())]);

    const error = await failure(
      client(fetchImpl, retry).artifact("DSP-1", {
        name: "build.tar",
        file: new Blob([new Uint8Array(1 << 20)]),
        actor,
      })
    );

    // Cut at 20 s, then asked at 21, 23 and 27 s; 8 s more would pass 30 s.
    expect(sent).toHaveLength(4);
    expect(pauses).toEqual([1_000, 2_000, 4_000]);
    expect(error).toBeInstanceOf(DispatchWriteUnsettledError);
  });

  test("a retried upload sends the bytes and boundary its first attempt sent", async () => {
    const { fetchImpl, sent } = scripted([
      page(502, "Bad Gateway"),
      json({ artifact: { id: "artifact-1" } }, 201),
    ]);

    await client(fetchImpl, pacing().retry).artifact("DSP-1", {
      name: "spec.md",
      file: new Blob(["# Spec"], { type: "text/markdown" }),
      summary: "Initial spec",
      actor,
    });

    expect(sent).toHaveLength(2);
    const [first, second] = sent as [Sent, Sent];
    expect(second.headers["Content-Type"]).toBe(first.headers["Content-Type"] as string);
    expect(first.headers["Content-Type"]).toStartWith("multipart/form-data; boundary=");
    expect(second.headers["Idempotency-Key"]).toBe(first.headers["Idempotency-Key"] as string);
    expect(first.body).toBeInstanceOf(Uint8Array);
    expect(second.body).toBe(first.body);
    const form = await new Response(first.body, {
      headers: { "Content-Type": first.headers["Content-Type"] as string },
    }).formData();
    expect(form.get("name")).toBe("spec.md");
    expect(form.get("summary")).toBe("Initial spec");
    expect(form.get("actor")).toBe(JSON.stringify(actor));
    expect(await (form.get("file") as Blob).text()).toBe("# Spec");
  });

  // A write Dispatch made whose answer is gone is no refusal: a replay of a first answer larger
  // than Dispatch keeps (2xx ANSWER_NOT_KEPT), or 409 KEY_COMMITTED (the write landed before its
  // answer was lost, and Dispatch will not run it again). It is never asked again, and its one line
  // says what was made and to read it back.
  test("a write whose answer is gone is a landing, never resent", async () => {
    const url = "http://dispatch.test/api/v1/issues/DSP-1/comments";
    for (const [answer, code, status, lost] of [
      [
        json({ code: IDEMPOTENCY_ANSWER_NOT_KEPT, error: "not kept" }, 201, {
          "Idempotency-Replayed": "?1",
        }),
        IDEMPOTENCY_ANSWER_NOT_KEPT,
        201,
        "Dispatch kept no copy of its answer, which was too large",
      ],
      [
        refusal(409, IDEMPOTENCY_KEY_COMMITTED, "the write landed and its answer was lost"),
        IDEMPOTENCY_KEY_COMMITTED,
        409,
        "its answer was lost on the way back",
      ],
    ] as const) {
      const { fetchImpl, sent } = scripted([
        refusal(503, "UNAVAILABLE", "database unavailable"),
        answer,
        json(posted, 201),
      ]);
      const { retry, pauses } = pacing();

      const error = await failure(client(fetchImpl, retry).comment("DSP-1", comment));

      expect(sent).toHaveLength(2);
      expect(pauses).toEqual([1_000]);
      expect(error).toBeInstanceOf(DispatchWriteLandedError);
      expect(error).not.toBeInstanceOf(DispatchServiceError);
      expect(error).toEqual(expect.objectContaining({ code, status }));
      expect(error.message).toBe(
        `POST ${url} was made, but ${lost} (${code}), so this command cannot print what it made: ` +
          "read it back with `dispatch read`, or with the matching list for something it created."
      );
    }
  });

  test("the production pacing is five attempts within 30 s, each cut off after 20 s of silence, and asks every 3 s until 55 s while Dispatch may be running the write", () => {
    expect(DISPATCH_WRITE_RETRY).toEqual({
      pausesMs: [1_000, 2_000, 4_000, 8_000],
      withinMs: 30_000,
      silenceMs: 20_000,
      inFlightPauseMs: 3_000,
      inFlightWithinMs: 55_000,
    });
  });
});

/** How one write's connection to `socketDispatch` takes its body. */
type Intake = "full" | "stall" | { readonly bytesPerMs: number };

/** One write `socketDispatch` took: the key it carried, its `Content-Length` and what arrived. */
interface Taken {
  readonly key: string | undefined;
  readonly length: number;
  received: number;
}

/** Answers `socket` with `status` and a JSON `body`, carrying back `key`, and closes it. */
function answerOn(socket: Socket, status: number, body: unknown, key?: string): void {
  const text = JSON.stringify(body);
  socket.end(
    `HTTP/1.1 ${status} ${status === 201 ? "Created" : "OK"}\r\n` +
      "Content-Type: application/json\r\n" +
      (key === undefined ? "" : `Idempotency-Key: ${key}\r\n`) +
      `Content-Length: ${Buffer.byteLength(text)}\r\nConnection: close\r\n\r\n${text}`
  );
}

/**
 * A Dispatch on a real socket, so a request's body goes out at the pace the connection takes it.
 * Each write's connection takes its body as the next of `intakes` says - at once, at a rate, or
 * nothing past its headers - and answers 201 carrying back its key once it holds all of it.
 * `GET /healthz` answers that the keyed layer runs.
 */
async function socketDispatch(intakes: readonly Intake[]) {
  const writes: Taken[] = [];
  const sockets = new Set<Socket>();
  const server = createServer((socket) => {
    sockets.add(socket);
    socket.on("close", () => sockets.delete(socket));
    let head = Buffer.alloc(0);
    let taken: Taken | undefined;
    let intake: Intake = "full";
    socket.on("data", (data: Buffer) => {
      let chunk = data;
      if (taken === undefined) {
        head = Buffer.concat([head, chunk]);
        const end = head.indexOf("\r\n\r\n");
        if (end === -1) return;
        const lines = head.subarray(0, end).toString("latin1").split("\r\n");
        if (lines[0]?.startsWith("GET /healthz ")) {
          answerOn(socket, 200, { status: "ok", idempotency_keys: true });
          return;
        }
        const header = (name: string): string | undefined =>
          lines
            .find((line) => line.toLowerCase().startsWith(`${name}:`))
            ?.slice(name.length + 1)
            .trim();
        taken = {
          key: header("idempotency-key"),
          length: Number(header("content-length")),
          received: 0,
        };
        intake = intakes[writes.length] ?? "full";
        writes.push(taken);
        chunk = head.subarray(end + 4);
        if (intake === "stall") {
          socket.pause();
          return;
        }
      }
      taken.received += chunk.length;
      if (taken.received >= taken.length) {
        answerOn(socket, 201, { artifact: { id: "artifact-1" } }, taken.key);
      } else if (typeof intake === "object") {
        socket.pause();
        setTimeout(() => socket.resume(), chunk.length / intake.bytesPerMs);
      }
    });
  });
  const { promise: listening, resolve } = Promise.withResolvers<void>();
  server.listen(0, "127.0.0.1", resolve);
  await listening;
  const { port } = server.address() as AddressInfo;
  return {
    url: `http://127.0.0.1:${port}`,
    writes,
    close(): void {
      for (const socket of sockets) socket.destroy();
      server.close();
    },
  };
}

// An attempt's silence bound counts the connection taking its body as progress, so an upload is
// bounded by how long it goes unheard, never by how long it takes; one whose connection stops
// taking it is cut off and sent again under its key. Only a real socket shows that: the pace is
// the kernel's backpressure on the connection, which no fake clock drives, so these run in real
// time, a few seconds at most.
describe("DispatchClient's uploads on a real connection", () => {
  beforeEach(forgetKeyedLayerMemory);

  const upload = (url: string, silenceMs: number, size: number) =>
    new DispatchClient(url, bearerCredential("secret"), fetch, undefined, {
      ...DISPATCH_WRITE_RETRY,
      silenceMs,
    }).artifact("DSP-1", {
      name: "build.tar",
      file: new Blob([new Uint8Array(size).fill(97)]),
      actor,
    });

  test("an upload going out for longer than silenceMs lands in one attempt", async () => {
    const dispatch = await socketDispatch([{ bytesPerMs: 8 << 10 }]);
    try {
      const started = performance.now();
      await upload(dispatch.url, 1_000, 24 << 20);

      expect(performance.now() - started).toBeGreaterThan(2_000);
      expect(dispatch.writes).toHaveLength(1);
      expect(dispatch.writes[0]?.received).toBe(dispatch.writes[0]?.length as number);
    } finally {
      dispatch.close();
    }
  });

  test("an upload its connection stops taking is cut off and sent again under its key", async () => {
    const dispatch = await socketDispatch(["stall", "full"]);
    try {
      const started = performance.now();
      await upload(dispatch.url, 300, 16 << 20);

      expect(performance.now() - started).toBeGreaterThanOrEqual(300);
      expect(dispatch.writes).toHaveLength(2);
      const [stalled, landed] = dispatch.writes as [Taken, Taken];
      expect(stalled.key).toMatch(IDEMPOTENCY_KEY_PATTERN);
      expect(landed.key).toBe(stalled.key as string);
      expect(landed.received).toBe(landed.length);
    } finally {
      dispatch.close();
    }
  });
});
