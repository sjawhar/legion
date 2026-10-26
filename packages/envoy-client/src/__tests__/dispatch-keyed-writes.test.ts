import { beforeEach, describe, expect, test } from "bun:test";
import { type AddressInfo, createServer, type Socket } from "node:net";
import {
  type CommentWriteResponse,
  IDEMPOTENCY_ANSWER_NOT_KEPT,
  IDEMPOTENCY_KEY_COMMITTED,
  IDEMPOTENCY_KEY_IN_FLIGHT,
  IDEMPOTENCY_KEY_PATTERN,
} from "@legion/contracts";
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
  return new DispatchClient("http://dispatch.test", "secret", fetchImpl, signal, retry);
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
    const { fetchImpl, sent } = scripted(Array.from({ length: 7 }, () => json({})));
    const dispatch = client(fetchImpl, pacing().retry);

    await dispatch.comment("DSP-1", comment);
    await dispatch.comment("DSP-1", comment);
    await dispatch.updateIssue("DSP-1", { status: "testing", actor });
    await dispatch.syncArchitectureSource("DSP", "acme/infra");
    await dispatch.getIssue("DSP-1");
    await dispatch.followAsk("ask-1", "session-1", actor);
    await dispatch.unfollowAsk("ask-1", "session-1", actor);

    const keys = sent.map((request): string | undefined => request.headers["Idempotency-Key"]);
    expect(sent.map((request) => request.method)).toEqual([
      "POST",
      "POST",
      "PATCH",
      "POST",
      "GET",
      "PUT",
      "DELETE",
    ]);
    for (const key of keys.slice(0, 4)) expect(key).toMatch(IDEMPOTENCY_KEY_PATTERN);
    // Two presses are two writes: the same comment posted twice is two intents.
    expect(new Set(keys.slice(0, 4)).size).toBe(4);
    expect(keys.slice(4)).toEqual([undefined, undefined, undefined]);
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
    expect(pauses).toEqual([1_000, 2_000, 4_000, 8_000]);
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
    const dispatch = client(probed.fetchImpl, retry);

    // The probe answers at 0 s, before the 1 s pause.
    await expect(dispatch.comment("DSP-1", comment)).resolves.toEqual(posted);
    expect(probed.probes).toHaveLength(1);
    advance(KEYED_LAYER_MEMORY_MS - 2_000);
    await expect(dispatch.comment("DSP-1", comment)).resolves.toEqual(posted);
    expect(probed.probes).toHaveLength(1);
    advance(1);
    await expect(dispatch.comment("DSP-1", comment)).resolves.toEqual(posted);
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
    const rolledBack = client(echoed.fetchImpl, clock.retry);
    await rolledBack.comment("DSP-1", comment);
    clock.advance(KEYED_LAYER_MEMORY_MS - 1);
    await expect(rolledBack.comment("DSP-1", comment)).resolves.toEqual(posted);
    expect(echoed.probes).toEqual([]);
    clock.advance(KEYED_LAYER_MEMORY_MS);
    await expect(failure(rolledBack.comment("DSP-1", comment))).resolves.toBeInstanceOf(
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
    expect(landed.message).toBe(
      "POST http://dispatch.test/api/v1/issues/DSP-1/comments answered 429 Too Many Requests " +
        `with a body that is not Dispatch's error JSON ("429 Too Many Requests"), which looks like ` +
        "a proxy or gateway page rather than Dispatch's own answer (the last of 5 attempts), and an " +
        "earlier attempt of the write may have reached Dispatch, so it may or may not have taken " +
        "effect: check whether it did before retrying it."
    );

    const running = scripted([
      new TypeError("fetch failed"),
      ...Array.from({ length: 4 }, () => inFlight()),
    ]);
    const stillRunning = await failure(
      client(running.fetchImpl, pacing().retry).comment("DSP-1", comment)
    );

    expect(stillRunning).toBeInstanceOf(DispatchWriteUnsettledError);
    expect(stillRunning.message).toBe(
      "KEY_IN_FLIGHT: an earlier request with this key is still running (the last of 5 " +
        "attempts), and Dispatch was still running an earlier attempt of the write, so it may or " +
        "may not have taken effect: check whether it did before retrying it."
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
        { ...DISPATCH_WRITE_RETRY, pausesMs: [60_000], withinMs: 120_000 },
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

  test("a write whose every attempt is silent may or may not have taken effect", async () => {
    const { fetchImpl, sent } = scripted(Array.from({ length: 5 }, () => hang));

    const error = await failure(
      client(fetchImpl, pacing({ ...DISPATCH_WRITE_RETRY, silenceMs: 20 }).retry).comment(
        "DSP-1",
        comment
      )
    );

    expect(sent).toHaveLength(5);
    expect(error).toBeInstanceOf(DispatchWriteUnsettledError);
    expect(error.message).toBe(
      "POST http://dispatch.test/api/v1/issues/DSP-1/comments made no progress for 0.02 s, so it " +
        "was cut off (the last of 5 attempts), and an attempt of the write may have reached " +
        "Dispatch, so it may or may not have taken effect: check whether it did before retrying it."
    );
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

  test("the production pacing is five attempts within 30 s, each cut off after 20 s of silence", () => {
    expect(DISPATCH_WRITE_RETRY).toEqual({
      pausesMs: [1_000, 2_000, 4_000, 8_000],
      withinMs: 30_000,
      silenceMs: 20_000,
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
    new DispatchClient(url, "secret", fetch, undefined, {
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
