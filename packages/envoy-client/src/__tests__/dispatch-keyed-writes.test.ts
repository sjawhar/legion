import { beforeEach, describe, expect, test } from "bun:test";
import {
  type CommentWriteResponse,
  IDEMPOTENCY_ANSWER_NOT_KEPT,
  IDEMPOTENCY_KEY_IN_FLIGHT,
  IDEMPOTENCY_KEY_PATTERN,
} from "@legion/contracts";
import {
  DISPATCH_WRITE_RETRY,
  DispatchAnswerNotKeptError,
  DispatchClient,
  DispatchGatewayError,
  DispatchServiceError,
  type DispatchWriteRetry,
  DispatchWriteUnsettledError,
  forgetKeyedLayerMemory,
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

type Answer = Response | (() => Response) | Error;

/** What a scripted Dispatch's `/healthz` answers the probe: its JSON body, or a failure. */
type Health = Record<string, unknown> | Error | Response;

/**
 * A fetch that answers each write with the next of `answers`, recording what it was sent, and
 * `/healthz` with `health` (recorded apart, in `probes`). A JSON answer stands for one of
 * Dispatch's own and echoes the request's key, as the keyed layer does, unless `echo` is false (a
 * Dispatch without the layer); an HTML page stands for a gateway's, which never echoes.
 */
function scripted(
  answers: readonly Answer[],
  options: { readonly echo?: boolean; readonly health?: Health } = {}
) {
  const sent: Sent[] = [];
  const probes: string[] = [];
  const health = options.health ?? { status: "ok", idempotency_keys: true };
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
    const response = typeof answer === "function" ? answer() : answer;
    const key = headers["Idempotency-Key"];
    if (options.echo === false || key === undefined || !isDispatchOwn(response)) return response;
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

/** The production pacing, with a clock that moves only when the client pauses. */
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
  return { retry, pauses };
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
    await dispatch.syncArchitectureSource("DSP");
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
    const { retry, pauses } = pacing({ pausesMs: [10_000, 15_000, 20_000], withinMs: 30_000 });

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

  test("the deadline or the caller's cancellation ends a write's attempts", async () => {
    const timedOut = scripted([
      () => {
        throw new DOMException("The operation timed out.", "TimeoutError");
      },
    ]);
    const timeout = await failure(
      client(timedOut.fetchImpl, pacing().retry).comment("DSP-1", comment)
    );
    expect(timeout.name).toBe("TimeoutError");
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
      client(fetchImpl, { pausesMs: [50], withinMs: 1_000 }).comment("DSP-1", comment)
    ).resolves.toEqual(posted);
    expect(performance.now() - started).toBeGreaterThanOrEqual(45);
    expect(sent).toHaveLength(2);

    const stalled = scripted([page(502, "Bad Gateway")]);
    const stop = AbortSignal.timeout(50);
    const startedStalled = performance.now();
    const error = await failure(
      client(stalled.fetchImpl, { pausesMs: [60_000], withinMs: 120_000 }, stop).comment(
        "DSP-1",
        comment
      )
    );
    expect(error.name).toBe("TimeoutError");
    expect(performance.now() - startedStalled).toBeLessThan(5_000);
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

  // A replay whose first answer was larger than Dispatch keeps carries the first status and no
  // resource. The write ran, so it is no refusal; the session is told to read what it wrote.
  test("a replay without its kept answer says the write ran and to read what it wrote", async () => {
    const notKept =
      "Dispatch ran this request and answered 201, but its answer was larger than the 1048576 " +
      "bytes a key keeps, so it is not replayed: read what the request wrote to see it";
    const { fetchImpl, sent } = scripted([
      new TypeError("fetch failed"),
      json({ code: IDEMPOTENCY_ANSWER_NOT_KEPT, error: notKept }, 201, {
        "Idempotency-Replayed": "?1",
      }),
    ]);

    const error = await failure(client(fetchImpl, pacing().retry).comment("DSP-1", comment));

    expect(sent).toHaveLength(2);
    expect(error).toBeInstanceOf(DispatchAnswerNotKeptError);
    expect(error).not.toBeInstanceOf(DispatchServiceError);
    expect(error).toEqual(expect.objectContaining({ status: 201 }));
    expect(error.message).toBe(`${IDEMPOTENCY_ANSWER_NOT_KEPT}: ${notKept}`);
  });

  test("the production pacing is five attempts within 30 s", () => {
    expect(DISPATCH_WRITE_RETRY).toEqual({
      pausesMs: [1_000, 2_000, 4_000, 8_000],
      withinMs: 30_000,
    });
  });
});
