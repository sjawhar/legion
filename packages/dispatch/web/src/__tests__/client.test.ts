import { expect, jest, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";

import {
  ApiError,
  apiErrorMessage,
  createApiClient,
  type FetchImplementation,
  isCredentialFeatureOff,
  isDeliveryNotConfigured,
  isRetryableQueryError,
  isUnauthorized,
  REQUEST_DEADLINE_MS,
  RequestTimeoutError,
} from "../api/client";

interface RecordedRequest {
  body?: BodyInit | null;
  init?: RequestInit;
  path: string;
}

function stubFetch(
  handler: (request: RecordedRequest) => Response | Promise<Response> = () => Response.json({})
): { fetch: FetchImplementation; requests: RecordedRequest[] } {
  const requests: RecordedRequest[] = [];

  return {
    fetch: async (input, init) => {
      const request = {
        body: init?.body,
        init,
        path: input.toString(),
      };
      requests.push(request);
      return handler(request);
    },
    requests,
  };
}

/** A fetch Dispatch answers only when the test says so (`answer`, in request order), and that
 *  rejects as a browser's does once the request's signal aborts: with the signal's reason. */
function heldFetch(): {
  answer: (response: Response) => void;
  fetch: FetchImplementation;
  requests: RecordedRequest[];
} {
  const requests: RecordedRequest[] = [];
  const answers: ((response: Response) => void)[] = [];
  return {
    answer: (response) => answers.shift()?.(response),
    fetch: (input, init) => {
      requests.push({ body: init?.body, init, path: input.toString() });
      const { promise, reject, resolve } = Promise.withResolvers<Response>();
      answers.push(resolve);
      const signal = init?.signal;
      signal?.addEventListener("abort", () => reject(signal.reason), { once: true });
      return promise;
    },
    requests,
  };
}

/** What `pending` settled with: its value, or what it rejected with. */
function outcome(pending: Promise<unknown>): Promise<unknown> {
  return pending.then(
    (value) => value,
    (error: unknown) => error
  );
}

/** The bounds one press of a write gives the client: its key, which a `POST` or a `PATCH` sends as
 *  `Idempotency-Key` and a `PUT` or a `DELETE` leaves off. */
function keyed(key: string): { key: string } {
  return { key };
}

/** Runs `body` on the fake clock. A timer left at zero delay would hang a later test's `waitFor`
 *  once the real clock is back, so the fake one moves a millisecond before it goes. */
async function onFakeClock(body: () => Promise<void>): Promise<void> {
  jest.useFakeTimers();
  try {
    await body();
  } finally {
    jest.advanceTimersByTime(1);
    jest.useRealTimers();
  }
}

// A read Dispatch never answers would leave its page loading for good: every GET gives up at the
// deadline as a timeout, which the shared retry policy asks again, as it does a dropped connection.
test("a GET past the deadline fails with RequestTimeoutError", async () => {
  await onFakeClock(async () => {
    const stub = heldFetch();
    const api = createApiClient(stub.fetch);

    const read = outcome(api.getIssue("CORE-1"));
    jest.advanceTimersByTime(REQUEST_DEADLINE_MS - 1);
    expect(stub.requests[0]?.init?.signal?.aborted).toBe(false);
    jest.advanceTimersByTime(1);
    const error = await read;

    expect(error).toBeInstanceOf(RequestTimeoutError);
    expect(error).toMatchObject({ path: "/api/v1/issues/CORE-1" });
    expect(apiErrorMessage(error, "fallback")).toBe("Dispatch did not answer within 30 seconds");
    expect(isRetryableQueryError(error)).toBe(true);
  });
});

// The deadline is the wait for Dispatch's answer, not the download of it: headers that arrive in
// time start a body that may take as long as the line needs, so a large read on a slow line is
// never cut off and asked again from the start.
test("a GET whose headers arrive at 29 s reads a body that streams for another 20 s", async () => {
  await onFakeClock(async () => {
    const stub = heldFetch();
    const api = createApiClient(stub.fetch);
    const encoder = new TextEncoder();
    let stream: ReadableStreamDefaultController<Uint8Array> | undefined;

    const read = outcome(api.getIssue("CORE-1"));
    const signal = stub.requests[0]?.init?.signal;
    jest.advanceTimersByTime(29_000);
    stub.answer(
      new Response(
        new ReadableStream<Uint8Array>({
          start(body) {
            stream = body;
            body.enqueue(encoder.encode('{"key":'));
            // A browser errors the body of a request its signal aborts.
            signal?.addEventListener("abort", () => body.error(signal.reason), { once: true });
          },
        }),
        { headers: { "Content-Type": "application/json" } }
      )
    );
    await Promise.resolve();
    jest.advanceTimersByTime(10_000);
    stream?.enqueue(encoder.encode('"CORE-1",'));
    jest.advanceTimersByTime(10_000);
    expect(stub.requests[0]?.init?.signal?.aborted).toBe(false);
    stream?.enqueue(encoder.encode('"title":"Slow line"}'));
    stream?.close();

    expect(await read).toEqual({ key: "CORE-1", title: "Slow line" } as never);
  });
});

// W13. The deadline is on Dispatch's silence, not on the download: a body that keeps arriving is
// read however long it takes, so a slow line is never cut off and asked for again from the start.
test("a GET whose body streams a chunk every 10 s for a minute is read in full", async () => {
  await onFakeClock(async () => {
    const stub = heldFetch();
    const api = createApiClient(stub.fetch);
    const encoder = new TextEncoder();
    let stream: ReadableStreamDefaultController<Uint8Array> | undefined;

    const read = outcome(api.getIssue("CORE-1"));
    const signal = stub.requests[0]?.init?.signal;
    stub.answer(
      new Response(
        new ReadableStream<Uint8Array>({
          start(body) {
            stream = body;
            signal?.addEventListener("abort", () => body.error(signal.reason), { once: true });
          },
        }),
        { headers: { "Content-Type": "application/json" } }
      )
    );
    await Promise.resolve();
    const pieces = ['{"key":', '"CORE-1",', '"title":', '"Slow', ' line"', "}"];
    for (const piece of pieces) {
      jest.advanceTimersByTime(10_000);
      await Promise.resolve();
      expect(stub.requests[0]?.init?.signal?.aborted).toBe(false);
      stream?.enqueue(encoder.encode(piece));
      await Promise.resolve();
    }
    stream?.close();

    expect(await read).toEqual({ key: "CORE-1", title: "Slow line" } as never);
  });
});

// W13. And the silence between pieces is bounded as the wait for the headers is: a body that
// stops arriving is given up as a timeout, which the shared retry policy asks again, rather than
// holding one of the browser's few connections to Dispatch for good.
test("a GET whose body stalls past the deadline fails with RequestTimeoutError", async () => {
  await onFakeClock(async () => {
    const stub = heldFetch();
    const api = createApiClient(stub.fetch);

    const read = outcome(api.getIssue("CORE-1"));
    const signal = stub.requests[0]?.init?.signal;
    stub.answer(
      new Response(
        new ReadableStream<Uint8Array>({
          start(body) {
            body.enqueue(new TextEncoder().encode('{"key":'));
            signal?.addEventListener("abort", () => body.error(signal.reason), { once: true });
          },
        }),
        { headers: { "Content-Type": "application/json" } }
      )
    );
    await Promise.resolve();
    jest.advanceTimersByTime(REQUEST_DEADLINE_MS - 1);
    await Promise.resolve();
    expect(stub.requests[0]?.init?.signal?.aborted).toBe(false);
    jest.advanceTimersByTime(1);

    const error = await read;
    expect(error).toBeInstanceOf(RequestTimeoutError);
    expect(isRetryableQueryError(error)).toBe(true);
  });
});

// A refusal's own body is read under the same deadline: `errorFrom` reads it before the error is
// raised, so a 500 whose reason never finishes arriving would otherwise hold the request, and
// one of the browser's few connections to Dispatch, for good. Given up, the request settles as
// the refusal Dispatch's headers named, with whatever reason reached the page.
test("a refusal whose body stalls is given up at the deadline and settles as the refusal", async () => {
  await onFakeClock(async () => {
    const stub = heldFetch();
    const api = createApiClient(stub.fetch);

    const read = outcome(api.getIssue("CORE-1"));
    const signal = stub.requests[0]?.init?.signal;
    stub.answer(
      new Response(
        new ReadableStream<Uint8Array>({
          start(body) {
            body.enqueue(new TextEncoder().encode('{"error":'));
            signal?.addEventListener("abort", () => body.error(signal.reason), { once: true });
          },
        }),
        { headers: { "Content-Type": "application/json" }, status: 500 }
      )
    );
    await Promise.resolve();
    jest.advanceTimersByTime(REQUEST_DEADLINE_MS - 1);
    await Promise.resolve();
    expect(signal?.aborted).toBe(false);
    jest.advanceTimersByTime(1);

    const error = await read;
    expect(signal?.aborted).toBe(true);
    expect(error).toBeInstanceOf(ApiError);
    expect(error).toMatchObject({ status: 500 });
  });
});

// A write is not abandoned at the read deadline: a comment that mentions agents can take the
// server longer, and its answer is still the write's outcome.
test("a POST carries no deadline", async () => {
  await onFakeClock(async () => {
    const stub = heldFetch();
    const api = createApiClient(stub.fetch);

    const created = api.createIssue({ project: "CORE", title: "Slow to create" }, keyed("write-1"));
    jest.advanceTimersByTime(10 * 60_000);
    expect(stub.requests[0]?.init?.signal).toBeUndefined();
    stub.answer(Response.json({ key: "CORE-1" }));

    expect(await created).toEqual({ key: "CORE-1" } as never);
  });
});

// One attempt of a keyed send to a session is a write its caller bounds itself, and repeats.
test("a write given a deadline is held to it", async () => {
  await onFakeClock(async () => {
    const stub = heldFetch();
    const api = createApiClient(stub.fetch);

    const sent = outcome(
      api.createAgentMessage(
        "planner-session",
        { body: "Ready?", delivery: "btw" },
        { ...keyed("send-1"), deadlineMs: 15_000 }
      )
    );
    jest.advanceTimersByTime(15_000);
    const error = await sent;

    expect(error).toBeInstanceOf(RequestTimeoutError);
    expect(apiErrorMessage(error, "fallback")).toBe("Dispatch did not answer within 15 seconds");
  });
});

// An attempt's deadline is the whole answer's: the attempt is made again under its key, so headers
// that arrive in time with a body that never ends are no answer either.
test("a write given a deadline is held to it, its body included", async () => {
  await onFakeClock(async () => {
    const stub = heldFetch();
    const api = createApiClient(stub.fetch);

    const sent = outcome(
      api.createBroadcast(
        { body: "Ready?", delivery: "btw", session_ids: ["planner"] },
        { ...keyed("send-1"), deadlineMs: 15_000 }
      )
    );
    const signal = stub.requests[0]?.init?.signal;
    stub.answer(
      new Response(
        new ReadableStream({
          start(body) {
            body.enqueue(new TextEncoder().encode('{"id":'));
            signal?.addEventListener("abort", () => body.error(signal.reason), { once: true });
          },
        }),
        { headers: { "Content-Type": "application/json" } }
      )
    );
    await Promise.resolve();
    jest.advanceTimersByTime(15_000);

    expect(await sent).toBeInstanceOf(RequestTimeoutError);
  });
});

// The caller's own abort is the caller's: it ends the request as itself, not as a timeout, and
// a request the caller lets run is still held to the deadline.
test("a caller's signal still aborts", async () => {
  await onFakeClock(async () => {
    const stub = heldFetch();
    const api = createApiClient(stub.fetch);
    const caller = new AbortController();

    const left = outcome(api.githubRest("repos/acme/dispatch/pulls/7", { signal: caller.signal }));
    const reason = new Error("the reader left");
    caller.abort(reason);
    expect(await left).toBe(reason);

    const kept = outcome(
      api.githubRest("repos/acme/dispatch/pulls/8", { signal: new AbortController().signal })
    );
    jest.advanceTimersByTime(REQUEST_DEADLINE_MS);
    expect(await kept).toBeInstanceOf(RequestTimeoutError);
  });
});

// A version's bytes can be a large file on a slow line: its download is the one GET with no
// deadline.
test("a version download carries no deadline", async () => {
  await onFakeClock(async () => {
    const stub = heldFetch();
    const api = createApiClient(stub.fetch);

    const download = api.getArtifactVersion("artifact-1", 3);
    jest.advanceTimersByTime(10 * 60_000);
    expect(stub.requests[0]?.init?.signal).toBeUndefined();
    stub.answer(new Response(new Blob(["bytes"]), { headers: { "Content-Type": "image/png" } }));

    expect(await download).toMatchObject({ mime: "image/png", version: 3 });
  });
});

test("API client does not invoke its default fetch as a client method", async () => {
  const originalFetch = globalThis.fetch;
  let receiver: unknown;

  globalThis.fetch = function (this: unknown): Promise<Response> {
    receiver = this;
    return Promise.resolve(Response.json({ login: "alice" }));
  } as unknown as typeof fetch;

  try {
    const api = createApiClient();
    await api.whoAmI();
    expect(receiver).not.toBe(api);
  } finally {
    globalThis.fetch = originalFetch;
  }
});

test("API client uploads inline artifact content as JSON and files as multipart", async () => {
  const stub = stubFetch(() => Response.json({ artifact: {}, version: {} }));
  const api = createApiClient(stub.fetch);

  await api.uploadArtifact(
    { issue: "CORE-1" },
    { content: "# Spec\n", name: "spec.md" },
    keyed("upload-1")
  );
  await api.uploadArtifact(
    { issue: "CORE-1" },
    {
      file: new File(["spec"], "spec.md", { type: "text/markdown" }),
      name: "spec.md",
    },
    keyed("upload-2")
  );

  const [inline, multipart] = stub.requests;
  expect(inline?.path).toBe("/api/v1/issues/CORE-1/artifacts");
  expect(inline?.init?.method).toBe("POST");
  expect(JSON.parse(String(inline?.init?.body))).toEqual({
    content: "# Spec\n",
    name: "spec.md",
  });
  expect(multipart?.init?.body).toBeInstanceOf(FormData);
  expect((multipart?.init?.body as FormData | undefined)?.get("name")).toBe("spec.md");
});

test("API client sends the documented method and JSON body for mutations", async () => {
  const stub = stubFetch();
  const api = createApiClient(stub.fetch);

  await api.createProject(
    {
      actor: { id: "session-1", kind: "session" },
      key: "CORE",
      name: "Core",
    },
    keyed("write-1")
  );
  await api.patchIssue("CORE-1", { status: "done" }, keyed("write-2"));
  await api.answerAsk(
    "ask-1",
    {
      selected: ["Ship"],
      text: "Approved",
      expected_edited_at: "2026-09-12T12:00:00Z",
    },
    keyed("write-3")
  );

  expect(stub.requests.map(({ init, path }) => [init?.method, path])).toEqual([
    ["POST", "/api/v1/projects"],
    ["PATCH", "/api/v1/issues/CORE-1"],
    ["POST", "/api/v1/asks/ask-1/answer"],
  ]);
  expect(JSON.parse(stub.requests[0]?.body as string)).toEqual({
    actor: { id: "session-1", kind: "session" },
    key: "CORE",
    name: "Core",
  });
  expect(JSON.parse(stub.requests[1]?.body as string)).toEqual({ status: "done" });
  expect(JSON.parse(stub.requests[2]?.body as string)).toEqual({
    expected_edited_at: "2026-09-12T12:00:00Z",
    selected: ["Ship"],
    text: "Approved",
  });
});

test("API client sends comment edits and reopen actions to their endpoints", async () => {
  const stub = stubFetch();
  const api = createApiClient(stub.fetch);

  await api.editComment("comment-1", { body: "Revised comment" }, keyed("write-1"));
  await api.reopenComment("comment-1", keyed("write-2"));

  expect(stub.requests.map(({ init, path }) => [init?.method, path])).toEqual([
    ["PATCH", "/api/v1/comments/comment-1"],
    ["POST", "/api/v1/comments/comment-1/reopen"],
  ]);
  expect(JSON.parse(stub.requests[0]?.body as string)).toEqual({ body: "Revised comment" });
  expect(JSON.parse(stub.requests[1]?.body as string)).toEqual({});
});

test("API client sends repository project mapping requests to their settings routes", async () => {
  const stub = stubFetch(() => Response.json({ repo: "owner/repo", project: "CORE" }));
  const settingsApi = createApiClient(stub.fetch);

  await settingsApi.listRepoProjects();
  await settingsApi.putRepoProject("owner/repo", { project: "CORE" });
  await settingsApi.deleteRepoProject("owner/repo");

  expect(stub.requests.map(({ init, path }) => [init?.method ?? "GET", path])).toEqual([
    ["GET", "/api/v1/settings/repo-projects"],
    ["PUT", "/api/v1/settings/repo-projects/owner/repo"],
    ["DELETE", "/api/v1/settings/repo-projects/owner/repo"],
  ]);
  expect(JSON.parse(stub.requests[1]?.body as string)).toEqual({ project: "CORE" });
});

test("API client sends architecture source requests to their routes", async () => {
  const stub = stubFetch(() =>
    Response.json({ project: "CORE", repo: "legion/arch", branch: "main" })
  );
  const settingsApi = createApiClient(stub.fetch);

  await settingsApi.listArchitectureSources();
  await settingsApi.getArchitectureSource("CORE");
  await settingsApi.putArchitectureSource("CORE", { branch: "main", repo: "legion/arch" });
  await settingsApi.syncArchitectureSource("CORE", keyed("write-1"));
  await settingsApi.deleteArchitectureSource("CORE");

  expect(stub.requests.map(({ init, path }) => [init?.method ?? "GET", path])).toEqual([
    ["GET", "/api/v1/settings/architecture-sources"],
    ["GET", "/api/v1/projects/CORE/architecture-source"],
    ["PUT", "/api/v1/projects/CORE/architecture-source"],
    ["POST", "/api/v1/projects/CORE/architecture-source/sync"],
    ["DELETE", "/api/v1/projects/CORE/architecture-source"],
  ]);
  expect(JSON.parse(stub.requests[2]?.body as string)).toEqual({
    branch: "main",
    repo: "legion/arch",
  });
});

test("API client sends inline artifacts as JSON", async () => {
  const stub = stubFetch(() => Response.json({ artifact: {}, version: {} }));
  const api = createApiClient(stub.fetch);

  await api.uploadArtifact(
    { issue: "CORE-1" },
    {
      content: "# Draft",
      name: "spec.md",
      summary: "Initial",
    },
    keyed("upload-1")
  );

  expect(stub.requests.map(({ init, path }) => [init?.method, path])).toEqual([
    ["POST", "/api/v1/issues/CORE-1/artifacts"],
  ]);
  expect(stub.requests[0]?.init?.headers).toEqual({
    "Content-Type": "application/json",
    "Idempotency-Key": "upload-1",
  });
  expect(JSON.parse(stub.requests[0]?.body as string)).toEqual({
    content: "# Draft",
    name: "spec.md",
    summary: "Initial",
  });
});

test("API client reads the server-owned block schema", async () => {
  const stub = stubFetch(() =>
    Response.json({
      types: [{ attributes: {}, content: "paragraph+", name: "callout", render: "host" }],
      version: 1,
    })
  );
  const api = createApiClient(stub.fetch);

  const schema = await api.getBlockSchema();

  expect(schema.version).toBe(1);
  expect(schema.types[0]?.name).toBe("callout");
  expect(stub.requests.map(({ path }) => path)).toEqual(["/api/v1/schema/blocks"]);
});

test("API client encodes list filters and artifact version query parameters", async () => {
  const stub = stubFetch();
  const api = createApiClient(stub.fetch);

  await api.listIssues({
    pinned: true,
    open: true,
    project: "CORE",
    status: "in progress",
    parent: "CORE-1",
    updated_since: "2026-09-10T12:00:00Z",
    labels: ["frontend", "docs"],
  });
  await api.getIssueEvents("CORE-1", { after: 3, limit: 20 });
  await api.getIssueEvents("CORE-1", { before: 40, order: "desc" });
  await api.getIssueEvents("CORE-1", { ids: ["42", "10"] });
  await api.listComments("CORE-1", "spec");

  expect(stub.requests.map(({ path }) => path)).toEqual([
    "/api/v1/issues?pinned=true&open=true&project=CORE&status=in+progress&parent=CORE-1&updated_since=2026-09-10T12%3A00%3A00Z&label=frontend&label=docs",
    "/api/v1/issues/CORE-1/events?after=3&limit=20",
    "/api/v1/issues/CORE-1/events?before=40&order=desc",
    "/api/v1/issues/CORE-1/events?ids=42%2C10",
    "/api/v1/issues/CORE-1/comments?artifact=spec",
  ]);
});
test("API client searches with the documented query parameters", async () => {
  const stub = stubFetch(() => Response.json({ results: [], took_ms: 1 }));
  const api = createApiClient(stub.fetch);

  await api.search("astrolabe", { project: "LEGION", limit: 5 });

  expect(stub.requests.map(({ path }) => path)).toEqual([
    "/api/v1/search?q=astrolabe&project=LEGION&limit=5",
  ]);
});
test("API client reaches project artifact and owner-scoped document endpoints", async () => {
  const stub = stubFetch((request) =>
    request.path.startsWith("/api/v1/artifacts/artifact-1/asks?")
      ? Response.json([])
      : Response.json({})
  );
  const api = createApiClient(stub.fetch);

  await api.listProjectArtifacts("CORE");
  await api.listProjectArtifacts("CORE", true);
  await api.uploadArtifact(
    { project: "CORE" },
    { content: "# Design notes\n", name: "Design notes" },
    keyed("upload-1")
  );
  await api.getProjectArtifact("CORE", "design-notes");
  await api.listArtifactAsks("artifact-1", "open");
  await api.createArtifactAsk("artifact-1", { question: "Ship?" }, keyed("write-1"));
  await api.listArtifactComments("artifact-1");
  await api.createArtifactComment("artifact-1", { body: "Looks good" }, keyed("write-2"));
  await api.getArtifactBlocks("artifact-1");
  await api.getIssueReferences("CORE-1");

  expect(stub.requests.map(({ init, path }) => [init?.method ?? "GET", path])).toEqual([
    ["GET", "/api/v1/projects/CORE/artifacts"],
    ["GET", "/api/v1/projects/CORE/artifacts?unlinked=true"],
    ["POST", "/api/v1/projects/CORE/artifacts"],
    ["GET", "/api/v1/projects/CORE/artifacts/design-notes"],
    ["GET", "/api/v1/artifacts/artifact-1/asks?state=open"],
    ["POST", "/api/v1/artifacts/artifact-1/asks"],
    ["GET", "/api/v1/artifacts/artifact-1/comments"],
    ["POST", "/api/v1/artifacts/artifact-1/comments"],
    ["GET", "/api/v1/artifacts/artifact-1/blocks"],
    ["GET", "/api/v1/issues/CORE-1/references"],
  ]);
  expect(JSON.parse(stub.requests[2]?.body as string)).toEqual({
    content: "# Design notes\n",
    name: "Design notes",
  });
});
test("API client reaches an agent conversation's artifacts under the agent's session", async () => {
  const stub = stubFetch(() => Response.json({}));
  const api = createApiClient(stub.fetch);

  await api.uploadArtifact(
    { session: "ses/1" },
    { file: new File(["png"], "shot.png", { type: "image/png" }), name: "shot.png" },
    keyed("upload-1")
  );
  await api.getAgentArtifact("ses/1", "shot-png");

  expect(stub.requests.map(({ init, path }) => [init?.method ?? "GET", path])).toEqual([
    ["POST", "/api/v1/agents/ses%2F1/artifacts"],
    ["GET", "/api/v1/agents/ses%2F1/artifacts/shot-png"],
  ]);
});

test("API client exposes response status and server error code on failure", async () => {
  const stub = stubFetch(() =>
    Response.json({ error: "Issue is closed", code: "ISSUE_CLOSED" }, { status: 409 })
  );
  const api = createApiClient(stub.fetch);

  await expect(api.getIssue("CORE-1")).rejects.toMatchObject({
    code: "ISSUE_CLOSED",
    message: "Issue is closed",
    status: 409,
  });
});
test("isUnauthorized distinguishes a 401 from a transient 5xx failure", async () => {
  const unauthorized = createApiClient(stubFetch(() => new Response(null, { status: 401 })).fetch);
  const serverError = createApiClient(stubFetch(() => new Response(null, { status: 503 })).fetch);

  const [unauthorizedError, serverErrorResult] = await Promise.all([
    unauthorized.whoAmI().catch((error: unknown) => error),
    serverError.whoAmI().catch((error: unknown) => error),
  ]);

  expect(isUnauthorized(unauthorizedError)).toBe(true);
  expect(isUnauthorized(serverErrorResult)).toBe(false);
  expect(isUnauthorized(new TypeError("network error"))).toBe(false);
});

test(
  "isRetryableQueryError exempts an auth outcome (401), a credential-feature-off 404, an " +
    "unconfigured delivery timeline and a document outside the schema, but retries a transient 5xx",
  async () => {
    const unauthorized = createApiClient(
      stubFetch(() => new Response(null, { status: 401 })).fetch
    );
    const featureOff = createApiClient(
      stubFetch(() =>
        Response.json({ error: "not configured", code: "FEATURE_OFF" }, { status: 404 })
      ).fetch
    );
    const documentSchema = createApiClient(
      stubFetch(() =>
        Response.json(
          {
            error:
              "document is outside the Proof schema; replace the document from markdown to repair it",
            code: "DOC_SCHEMA",
          },
          { status: 409 }
        )
      ).fetch
    );
    const serverError = createApiClient(stubFetch(() => new Response(null, { status: 503 })).fetch);
    const deliveryUnset = createApiClient(
      stubFetch(() =>
        Response.json(
          { error: "delivery is not configured", code: "DELIVERY_NOT_CONFIGURED" },
          { status: 404 }
        )
      ).fetch
    );

    const [
      unauthorizedError,
      featureOffError,
      documentSchemaError,
      serverErrorResult,
      deliveryUnsetError,
    ] = await Promise.all([
      unauthorized.whoAmI().catch((error: unknown) => error),
      featureOff.getCredentialRecord("record-1").catch((error: unknown) => error),
      documentSchema.getArtifactText("artifact-1").catch((error: unknown) => error),
      serverError.whoAmI().catch((error: unknown) => error),
      // The run route answers 404 DELIVERY_NOT_CONFIGURED while no settings row exists.
      deliveryUnset.getDeliveryRun(7).catch((error: unknown) => error),
    ]);

    expect(isRetryableQueryError(unauthorizedError)).toBe(false);
    expect(isCredentialFeatureOff(featureOffError)).toBe(true);
    expect(isRetryableQueryError(featureOffError)).toBe(false);
    expect(isRetryableQueryError(documentSchemaError)).toBe(false);
    expect(isRetryableQueryError(serverErrorResult)).toBe(true);
    expect(isDeliveryNotConfigured(deliveryUnsetError)).toBe(true);
    expect(isRetryableQueryError(deliveryUnsetError)).toBe(false);
  }
);

test(
  "the production retry policy issues one credential request when the broker is unconfigured, " +
    "not three (every query shares this policy from main.tsx)",
  async () => {
    const stub = stubFetch(() =>
      Response.json({ error: "not configured", code: "FEATURE_OFF" }, { status: 404 })
    );
    const client = createApiClient(stub.fetch);
    // Mirrors main.tsx's shared QueryClient retry option exactly, with retryDelay shortened
    // so the test does not wait on the production 500ms backoff.
    const queryClient = new QueryClient({
      defaultOptions: {
        queries: {
          retry: (failureCount, error) => failureCount < 2 && isRetryableQueryError(error),
          retryDelay: 0,
        },
      },
    });

    try {
      await expect(
        queryClient.fetchQuery({
          queryFn: () => client.getCredentialRecord("record-1"),
          queryKey: ["credential-record", "record-1"],
        })
      ).rejects.toMatchObject({ code: "FEATURE_OFF" });

      // isRetryableQueryError must treat FEATURE_OFF as non-retryable: if it doesn't, every
      // credential page opened where DISPATCH_AGENT_SECRETS_URL is unset issues three requests
      // (and logs three failed fetches) instead of one, the same shape as an unconfigured
      // architecture source.
      expect(stub.requests).toHaveLength(1);
    } finally {
      queryClient.clear();
    }
  }
);
test("API client reaches every remaining documented endpoint", async () => {
  const stub = stubFetch((request) => {
    if (
      request.path.startsWith("/api/v1/inbox") ||
      request.path.startsWith("/api/v1/issues/CORE-1/asks?")
    ) {
      return Response.json([]);
    }
    if (request.path === "/api/v1/asks/ask-1") {
      return Response.json({
        ask: {
          anchor: null,
          answer: null,
          author: { id: "session-1", kind: "session" },
          created_at: "2026-09-09T00:00:00Z",
          custom: false,
          id: "ask-1",
          issue_key: "CORE-1",
          multiple: false,
          opened_event_id: 1,
          options: [],
          question: "Ship?",
          state: "open",
          urgency: "med",
        },
        replies: [],
      });
    }
    return Response.json({});
  });
  const api = createApiClient(stub.fetch);

  await api.listProjects();
  await api.listIssues();
  await api.search("astrolabe");
  await api.createIssue({ project: "CORE", title: "Ship it" }, keyed("write-1"));
  await api.getIssue("CORE-1");
  await api.getInbox("CORE");
  await api.createAsk("CORE-1", { question: "Ship?" }, keyed("write-2"));
  await api.listIssueAsks("CORE-1");
  await api.listIssueAskThreads("CORE-1");
  await api.getAsk("ask-1");
  await api.listMyAnswers({ limit: 50, offset: 100 });
  await api.listMyReplies({ limit: 50, offset: 0 });
  await api.listMyReplies({ issue: "CORE-1", limit: 200 });
  await api.listMyReplies({ artifact: "artifact-1", limit: 200 });
  await api.markThreadRead("comment-1", "2026-10-12T00:00:00.000Z");
  await api.createComment("CORE-1", { body: "Looks good" }, keyed("write-3"));
  await api.resolveComment("comment-1", keyed("write-4"));
  await api.acceptComment("comment-1", keyed("write-5"));
  await api.rejectComment("comment-1", keyed("write-6"));
  await api.createCommentDelivery(
    "comment-1",
    "session:planner-session",
    "steer",
    keyed("write-7")
  );
  await api.createMessage("CORE-1", { body: "Ready" }, keyed("write-8"));
  await api.createAgentMessage(
    "planner-session",
    { body: "Ready?", delivery: "btw" },
    keyed("send-1")
  );
  await api.listAgentMessages("planner-session");
  await api.listArtifacts("CORE-1");
  await api.listAgents();
  await api.uploadArtifact(
    { issue: "CORE-1" },
    {
      file: new File(["spec"], "spec.md", { type: "text/markdown" }),
      name: "spec.md",
    },
    keyed("upload-1")
  );
  await api.getArtifact("artifact-1");
  await api.getArtifactText("artifact-1");
  await api.getArtifactVersion("artifact-1", 3);
  await api.createArtifactVersion("artifact-1", { summary: "Save" }, keyed("write-9"));
  await api.listArtifactReviews("artifact-1");
  await api.createArtifactReview("artifact-1", { state: "approved" }, keyed("write-10"));
  await api.getMyState();
  await api.putIssueState("CORE-1", { pinned: true });
  await api.getMyAgentState();
  await api.putAgentState("planner-session", { cleared_before: "2026-09-15T20:51:00.000Z" });
  await api.whoAmI();
  await api.logout(keyed("write-11"));
  await api.githubRest("repos/acme/dispatch");
  await api.getDeliverySettings();
  await api.putDeliverySettings({
    deploy_repo: "acme/widgets",
    deploy_workflow_path: ".github/workflows/deploy.yml",
    excluded_repos: [],
    population_authors: ["octocat"],
    pr_checks_workflow_path: ".github/workflows/pr-checks.yml",
    production_job_names: ["widgets-release"],
  });

  expect(stub.requests.map(({ init, path }) => [init?.method ?? "GET", path])).toEqual([
    ["GET", "/api/v1/projects"],
    ["GET", "/api/v1/issues"],
    ["GET", "/api/v1/search?q=astrolabe"],
    ["POST", "/api/v1/issues"],
    ["GET", "/api/v1/issues/CORE-1"],
    ["GET", "/api/v1/inbox?project=CORE"],
    ["POST", "/api/v1/issues/CORE-1/asks"],
    ["GET", "/api/v1/issues/CORE-1/asks?state=all"],
    ["GET", "/api/v1/issues/CORE-1/asks?state=all&thread=1"],
    ["GET", "/api/v1/asks/ask-1"],
    ["GET", "/api/v1/me/answers?limit=50&offset=100"],
    ["GET", "/api/v1/me/replies?limit=50&offset=0"],
    ["GET", "/api/v1/me/replies?issue=CORE-1&limit=200"],
    ["GET", "/api/v1/me/replies?artifact=artifact-1&limit=200"],
    ["PUT", "/api/v1/me/threads/comment-1/read"],
    ["POST", "/api/v1/issues/CORE-1/comments"],
    ["POST", "/api/v1/comments/comment-1/resolve"],
    ["POST", "/api/v1/comments/comment-1/accept"],
    ["POST", "/api/v1/comments/comment-1/reject"],
    ["POST", "/api/v1/comments/comment-1/deliveries"],
    ["POST", "/api/v1/issues/CORE-1/messages"],
    ["POST", "/api/v1/agents/planner-session/messages"],
    ["GET", "/api/v1/agents/planner-session/messages"],
    ["GET", "/api/v1/issues/CORE-1/artifacts"],
    ["GET", "/api/v1/agents"],
    ["POST", "/api/v1/issues/CORE-1/artifacts"],
    ["GET", "/api/v1/artifacts/artifact-1"],
    ["GET", "/api/v1/artifacts/artifact-1/text"],
    ["GET", "/api/v1/artifacts/artifact-1/versions/3"],
    ["POST", "/api/v1/artifacts/artifact-1/versions"],
    ["GET", "/api/v1/artifacts/artifact-1/reviews"],
    ["POST", "/api/v1/artifacts/artifact-1/reviews"],
    ["GET", "/api/v1/me/state"],
    ["PUT", "/api/v1/me/issues/CORE-1/state"],
    ["GET", "/api/v1/me/agents/state"],
    ["PUT", "/api/v1/me/agents/planner-session/state"],
    ["GET", "/auth/whoami"],
    ["POST", "/auth/logout"],
    ["GET", "/api/github/rest/repos/acme/dispatch"],
    ["GET", "/api/v1/settings/delivery"],
    ["PUT", "/api/v1/settings/delivery"],
  ]);
});

test("API client keeps only small idempotent /me/ writes alive during navigation", async () => {
  const stub = stubFetch();
  const api = createApiClient(stub.fetch);
  const oversizedDismissed = Array.from(
    { length: 3_000 },
    (_unused, index) => `pinned_items:event:${index}`
  );

  await api.putIssueState("CORE-1", { pinned: true });
  await api.putAgentState("planner-session", { cleared_before: "2026-09-15T20:51:00.000Z" });
  await api.revokeAgentToken("token-1");
  await api.createAgentToken({ name: "planner" }, keyed("write-1"));
  await api.putIssueState("CORE-1", { dismissed: oversizedDismissed });

  const [smallIssueState, agentState, revokedToken, createdToken, oversizedIssueState] =
    stub.requests;
  expect(smallIssueState?.init?.keepalive).toBe(true);
  expect(agentState?.init?.keepalive).toBe(true);
  expect(revokedToken?.init?.keepalive).toBe(true);
  expect(createdToken?.init?.keepalive).toBeUndefined();
  expect(createdToken?.init?.method).toBe("POST");
  expect(new TextEncoder().encode(String(oversizedIssueState?.body)).byteLength).toBeGreaterThan(
    64 * 1024
  );
  expect(oversizedIssueState?.init?.keepalive).toBeUndefined();
});

/**
 * The one keyed write path, over every write method the client has. A `POST` or a `PATCH` names
 * the person's action to Dispatch by the caller's key, so a repeat of it is that one write there;
 * a `PUT` or a `DELETE` is the same write however often it is made, so it carries no key and is
 * let finish as the page goes. A method this list forgets is a write that would reach Dispatch
 * unnamed, which is what this test is for: every write method of the client is called here, and
 * the ones that carry a key and the ones that do not are both named.
 */
test("every POST and PATCH sends its Idempotency-Key, and no PUT or DELETE does", async () => {
  const stub = stubFetch(() => Response.json({ artifact: {}, version: {} }));
  const api = createApiClient(stub.fetch);
  const bounds = keyed("press-1");

  await api.createProject({ key: "CORE", name: "Core" }, bounds);
  await api.syncArchitectureSource("CORE", bounds);
  await api.createAgentToken({ name: "planner" }, bounds);
  await api.createIssue({ project: "CORE", title: "Ship it" }, bounds);
  await api.patchIssue("CORE-1", { status: "done" }, bounds);
  await api.createAsk("CORE-1", { question: "Ship?" }, bounds);
  await api.answerAsk(
    "ask-1",
    { expected_edited_at: null, selected: ["Ship"], text: "Yes" },
    bounds
  );
  await api.createComment("CORE-1", { body: "Looks good" }, bounds);
  await api.resolveComment("comment-1", bounds);
  await api.reopenComment("comment-1", bounds);
  await api.editComment("comment-1", { body: "Revised" }, bounds);
  await api.acceptComment("comment-1", bounds);
  await api.rejectComment("comment-1", bounds);
  await api.createCommentDelivery("comment-1", "session:planner", "steer", bounds);
  await api.createMessage("CORE-1", { body: "Ready" }, bounds);
  await api.createAgentMessage("planner-session", { body: "Ready?", delivery: "btw" }, bounds);
  await api.createBroadcast({ body: "All", delivery: "btw", session_ids: ["planner"] }, bounds);
  await api.createMessageDelivery("message-1", "btw", bounds);
  await api.uploadArtifact({ issue: "CORE-1" }, { content: "# Spec\n", name: "spec.md" }, bounds);
  await api.uploadArtifact(
    { issue: "CORE-1" },
    { file: new File(["spec"], "spec.md", { type: "text/markdown" }), name: "spec.md" },
    bounds
  );
  await api.rebuildArtifact("artifact-1", undefined, bounds);
  await api.createArtifactVersion("artifact-1", { summary: "Save" }, bounds);
  await api.createArtifactReview("artifact-1", { state: "approved" }, bounds);
  await api.createArtifactAsk("artifact-1", { question: "Ship?" }, bounds);
  await api.createArtifactComment("artifact-1", { body: "Looks good" }, bounds);
  await api.createDeliverySessionGroup({ name: "Pair", sessions: ["planner"] }, bounds);
  await api.updateDeliverySessionGroup("group-1", { name: "Trio" }, bounds);
  await api.logout(bounds);
  await api.approveCredentialRecord("record-1", {}, bounds);
  await api.denyCredentialRecord("record-1", {}, bounds);
  await api.lookupMachineCredential("ABCD1234", bounds);
  await api.revokeCredentialGrant("grant-1", bounds);
  await api.revokeMachineLogin("credential-1", bounds);
  const keyedWrites = stub.requests.length;

  await api.putRepoProject("owner/repo", { project: "CORE" }, bounds);
  await api.deleteRepoProject("owner/repo", bounds);
  await api.putArchitectureSource("CORE", { branch: "main", repo: "legion/arch" }, bounds);
  await api.deleteArchitectureSource("CORE", bounds);
  await api.revokeAgentToken("token-1", bounds);
  await api.releaseIssueClaim("CORE-1", bounds);
  await api.snoozeAsk("ask-1", "2026-10-26T00:00:00Z", bounds);
  await api.unsnoozeAsk("ask-1", bounds);
  await api.markThreadRead("thread-1", "2026-10-25T00:00:00Z", bounds);
  await api.removeAskFollower("ask-1", "planner", bounds);
  await api.unsubscribeArtifactSession("artifact-1", "planner", bounds);
  await api.unsubscribeIssueSession("CORE-1", "planner", bounds);
  await api.putDeliverySettings(
    {
      deploy_repo: "acme/widgets",
      deploy_workflow_path: ".github/workflows/deploy.yml",
      excluded_repos: [],
      population_authors: ["octocat"],
      pr_checks_workflow_path: ".github/workflows/pr-checks.yml",
      production_job_names: ["widgets-release"],
    },
    bounds
  );
  await api.deleteDeliverySessionGroup("group-1", bounds);
  await api.putIssueState("CORE-1", { pinned: true }, bounds);
  await api.putAgentState("planner-session", { cleared_before: "2026-10-25T00:00:00Z" }, bounds);

  const keyOf = ({ init }: { init?: RequestInit }) =>
    new Headers(init?.headers).get("Idempotency-Key");
  const keyedRequests = stub.requests.slice(0, keyedWrites);
  const idempotentRequests = stub.requests.slice(keyedWrites);
  expect(keyedRequests).not.toHaveLength(0);
  expect(idempotentRequests).not.toHaveLength(0);
  // Named by path, so a method whose key went missing says which write it was.
  expect(
    keyedRequests.filter((request) => keyOf(request) !== "press-1").map((it) => it.path)
  ).toEqual([]);
  expect(
    keyedRequests.filter(({ init }) => init?.method !== "POST" && init?.method !== "PATCH")
  ).toEqual([]);
  expect(
    idempotentRequests.filter((request) => keyOf(request) !== null).map((it) => it.path)
  ).toEqual([]);
  expect(
    idempotentRequests.filter(({ init }) => init?.method !== "PUT" && init?.method !== "DELETE")
  ).toEqual([]);
});

test("API client exposes the current state with a stale state-write conflict", async () => {
  const staleState = {
    dismissed: ["pinned_items:event:1"],
    last_read_seq: 0,
    pinned: false,
    seq: 2,
  };
  const stub = stubFetch(() =>
    Response.json(
      { code: "STATE_STALE", error: "user state was updated by another write", state: staleState },
      { status: 409 }
    )
  );
  const api = createApiClient(stub.fetch);

  const error = await api
    .putIssueState("CORE-1", { dismissed: ["pinned_items:event:1"], seq: 1 })
    .then(
      () => undefined,
      (caught) => caught
    );

  expect(error).toMatchObject({ code: "STATE_STALE", state: staleState, status: 409 });
});
