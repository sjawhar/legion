import { describe, expect, test } from "bun:test";
import type { IssueSummary } from "@legion/contracts";
import { DispatchClient, DispatchGatewayError } from "../dispatch-http";

interface RecordedRequest {
  readonly url: string;
  readonly init: RequestInit;
}

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

function fakeFetch(responses: readonly Response[]) {
  const requests: RecordedRequest[] = [];
  const fetchImpl = async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    requests.push({ url: String(input), init: init ?? {} });
    const response = responses[requests.length - 1];
    if (!response) throw new Error("unexpected request");
    return response;
  };
  return { fetchImpl: fetchImpl as typeof fetch, requests };
}

function requestBody(request: RecordedRequest): unknown {
  return JSON.parse(request.init.body as string);
}

const actor = {
  kind: "session" as const,
  id: "session-1",
  origin: { host: "omp" as const, cwd: "/workspace", session_title: "Implement" },
};

describe("DispatchClient", () => {
  // A summary carrying only what these tests read; the rest of the shape is left out.
  const issue = (key: string) => ({ key, title: key, status: "todo" }) as unknown as IssueSummary;

  test("asks for a page of the issue listing with the filters and the updated_since boundary", async () => {
    const { fetchImpl, requests } = fakeFetch([
      jsonResponse({ issues: [], total: 0, limit: 5, offset: 10 }),
    ]);
    const client = new DispatchClient("http://dispatch.test", "secret", fetchImpl);

    await client.listIssuePage(
      { project: "DSP", updated_since: "2026-09-10T12:00:00Z" },
      { limit: 5, offset: 10 }
    );

    expect(requests).toHaveLength(1);
    expect(requests[0]?.url).toContain(
      "/api/v1/issues?project=DSP&updated_since=2026-09-10T12%3A00%3A00Z&limit=5&offset=10"
    );
  });

  test("reads a paging Dispatch's page as served", async () => {
    const served = { issues: [issue("DSP-4")], total: 4, limit: 3, offset: 3 };
    const { fetchImpl } = fakeFetch([jsonResponse(served)]);
    const client = new DispatchClient("http://dispatch.test", "secret", fetchImpl);

    // The page a paging Dispatch cut is the page: nothing is sliced out of it again.
    await expect(
      client.listIssuePage({ project: "DSP" }, { limit: 3, offset: 3 })
    ).resolves.toEqual(served);
  });

  // A Dispatch that ignores limit and offset answers the whole listing as an array. That is a
  // server older than the paging listing or a regression of it, so the client refuses it rather
  // than read every issue to cut the page itself. The refusal names the Dispatch and the whole
  // request, since a repository can point the tool at a Dispatch of its own.
  test("refuses a bare array answered to a paged request, naming the request and both causes", async () => {
    const { fetchImpl } = fakeFetch([
      jsonResponse(["DSP-1", "DSP-2", "DSP-3", "DSP-4"].map(issue)),
    ]);
    const client = new DispatchClient("http://dispatch.test", "secret", fetchImpl);

    await expect(
      client.listIssuePage({ project: "DSP", status: "todo" }, { limit: 2, offset: 1 })
    ).rejects.toThrow(
      "GET http://dispatch.test/api/v1/issues?project=DSP&status=todo&limit=2&offset=1 asked for " +
        "a page ({issues, total, limit, offset}) and got a bare array of 4 entries, the unpaged " +
        "listing, which means that Dispatch is older than sjawhar/legion#1612 or that change has " +
        "regressed. Retrying will not help: the same request gets the same answer until that " +
        "Dispatch is upgraded or fixed."
    );
  });

  // A JSON answer that is not a page is Dispatch's own and comes back the same on a retry; text in
  // its place is most likely a proxy or gateway page, which a retry can get past. The agent acts on
  // the advice, so each kind gets its own.
  test("refuses an issue listing answer that is not a page, naming what arrived and whether a retry can help", async () => {
    const { fetchImpl } = fakeFetch([
      jsonResponse({ issues: "DSP-1", total: 1 }),
      jsonResponse(null),
      new Response("<html>bad gateway</html>", { status: 200 }),
      new Response("{truncat", { status: 200, headers: { "Content-Type": "application/json" } }),
    ]);
    const client = new DispatchClient("http://dispatch.test", "secret", fetchImpl);
    const refusal = () =>
      client.listIssuePage({ project: "DSP" }, { limit: 50, offset: 0 }).then(
        () => "",
        (error: Error) => error.message
      );
    const sameAgain =
      ". Retrying will not help: the same request gets the same answer until that Dispatch is " +
      "upgraded or fixed.";
    const maySucceed =
      "and got text that is not a JSON page, which looks like a proxy or gateway page rather than " +
      "Dispatch's own answer, so a retry may succeed.";

    expect(await refusal()).toEndWith(
      `and got an object without an issues array or a numeric limit or a numeric offset${sameAgain}`
    );
    expect(await refusal()).toEndWith(`and got null${sameAgain}`);
    for (const text of [await refusal(), await refusal()]) {
      expect(text).toEndWith(maySucceed);
      expect(text).not.toContain("Retrying will not help");
    }
  });

  // The executor prints `showing A-B of N` from the served limit and offset, so a page missing
  // either must be refused rather than read as `showing NaN-NaN of N`.
  test("refuses a page that does not name its limit and offset", async () => {
    const { fetchImpl } = fakeFetch([
      jsonResponse({ issues: [issue("DSP-2")], total: 5 }),
      jsonResponse({ issues: [issue("DSP-2")], total: 5, limit: 1 }),
      jsonResponse({ issues: [issue("DSP-2")], total: 5, limit: "1", offset: 1 }),
    ]);
    const client = new DispatchClient("http://dispatch.test", "secret", fetchImpl);

    for (const lacking of [
      "a numeric limit or a numeric offset",
      "a numeric offset",
      "a numeric limit",
    ]) {
      await expect(
        client.listIssuePage({ project: "DSP" }, { limit: 1, offset: 1 })
      ).rejects.toThrow(`and got an object without ${lacking}. Retrying`);
    }
  });

  test("search encodes q, project, and limit and returns the response body", async () => {
    const body = {
      results: [
        {
          kind: "document" as const,
          owner: { key: "LEGION-2", kind: "issue" as const, title: "Astrolabe", status: "triage" },
          artifact: { slug: "spec", name: "spec.md" },
          id: "artifact-2",
          snippet: "<mark>astrolabe</mark>",
          rank: 1,
          href: "/issues/LEGION-2/spec?q=astrolabe",
        },
      ],
      took_ms: 12,
    };
    const { fetchImpl, requests } = fakeFetch([jsonResponse(body)]);
    const client = new DispatchClient("http://dispatch.test", "secret", fetchImpl);

    await expect(
      client.search("astrolabe sextant", { project: "LEGION", limit: 5 })
    ).resolves.toEqual(body);

    expect(requests).toHaveLength(1);
    expect(new URL(requests[0]?.url).pathname + new URL(requests[0]?.url).search).toBe(
      "/api/v1/search?q=astrolabe+sextant&project=LEGION&limit=5"
    );
    expect(requests[0]?.init).toMatchObject({
      method: "GET",
      headers: { Authorization: "Bearer secret", Accept: "application/json" },
    });
  });

  test("lists this session's open asks with an optional server baseline", async () => {
    const body = {
      session_id: "session-1",
      as_of: "2026-09-13T00:00:00Z",
      opened_since: false,
      count: 0,
      waiting_on_human: 0,
      waiting_on_agent: 0,
      asks: [],
    };
    const { fetchImpl, requests } = fakeFetch([jsonResponse(body)]);
    const client = new DispatchClient("http://dispatch.test", "secret", fetchImpl);

    await expect(client.openAsks("session-1", "2026-09-12T00:00:00Z")).resolves.toEqual(body);

    expect(requests).toHaveLength(1);
    expect(new URL(requests[0]?.url).pathname + new URL(requests[0]?.url).search).toBe(
      "/api/v1/asks/open?author_session=session-1&since=2026-09-12T00%3A00%3A00Z"
    );
    expect(requests[0]?.init).toMatchObject({
      method: "GET",
      headers: { Authorization: "Bearer secret", Accept: "application/json" },
    });
  });

  test("lists a project's open asks across issues and documents", async () => {
    const body = {
      session_id: "",
      as_of: "2026-09-13T00:00:00Z",
      opened_since: false,
      count: 0,
      waiting_on_human: 0,
      waiting_on_agent: 0,
      asks: [],
    };
    const { fetchImpl, requests } = fakeFetch([jsonResponse(body)]);
    const client = new DispatchClient("http://dispatch.test", "secret", fetchImpl);

    await expect(client.openAsksForProject("CORE")).resolves.toEqual(body);

    expect(requests).toHaveLength(1);
    expect(new URL(requests[0]?.url).pathname + new URL(requests[0]?.url).search).toBe(
      "/api/v1/asks/open?project=CORE"
    );
    expect(requests[0]?.init).toMatchObject({
      method: "GET",
      headers: { Authorization: "Bearer secret", Accept: "application/json" },
    });
  });

  test("maps project document and reference routes to authenticated API requests", async () => {
    const { fetchImpl, requests } = fakeFetch(
      Array.from({ length: 14 }, () => jsonResponse({ ok: true }))
    );
    const client = new DispatchClient("http://dispatch.test", "secret", fetchImpl);

    await client.listProjectArtifacts("CORE", true);
    await client.projectArtifact("CORE", { name: "runbook.md", content: "# Runbook", actor });
    await client.getProjectArtifact("CORE", "runbook-md");
    await client.getArtifactAsks("artifact-1");
    await client.artifactAsk("artifact-1", { question: "Publish?", actor });
    await client.getArtifactComments("artifact-1");
    await client.artifactComment("artifact-1", { body: "Looks good.", actor });
    await client.artifactSuggest("artifact-1", {
      anchor: { artifact: "artifact-1", quote: "draft" },
      replace_with: "final",
      actor,
    });
    await client.getArtifactEvents("artifact-1");
    await client.getIssueReferences("CORE-1");
    await client.getArtifactReferences("artifact-1");
    await client.getReferences({ to: "dispatch://CORE-1/ask/ask-1" });
    await client.getReferences({
      from: "dispatch://CORE/artifact/runbook-md",
      kind: ["mentions", "attached_to"],
      since: 918,
    });
    await client.getReferences({ to: "dispatch://CORE-1", kind: [] });

    expect(
      requests.map((request) => new URL(request.url).pathname + new URL(request.url).search)
    ).toEqual([
      "/api/v1/projects/CORE/artifacts?unlinked=true",
      "/api/v1/projects/CORE/artifacts",
      "/api/v1/projects/CORE/artifacts/runbook-md",
      "/api/v1/artifacts/artifact-1/asks",
      "/api/v1/artifacts/artifact-1/asks",
      "/api/v1/artifacts/artifact-1/comments",
      "/api/v1/artifacts/artifact-1/comments",
      "/api/v1/artifacts/artifact-1/comments",
      "/api/v1/artifacts/artifact-1/events?after=0&limit=200",
      "/api/v1/issues/CORE-1/references",
      "/api/v1/artifacts/artifact-1/references",
      "/api/v1/references?to=dispatch%3A%2F%2FCORE-1%2Fask%2Fask-1",
      "/api/v1/references?from=dispatch%3A%2F%2FCORE%2Fartifact%2Frunbook-md&kind=mentions%2Cattached_to&since=918",
      "/api/v1/references?to=dispatch%3A%2F%2FCORE-1",
    ]);
    expect(requestBody(requests[1] as RecordedRequest)).toEqual({
      name: "runbook.md",
      content: "# Runbook",
      actor,
    });
    expect(requestBody(requests[7] as RecordedRequest)).toEqual({
      anchor: { artifact: "artifact-1", quote: "draft" },
      suggestion: { replace_with: "final" },
      actor,
    });
  });
  test("maps dispatch operations to authenticated JSON and multipart API requests", async () => {
    const { fetchImpl, requests } = fakeFetch([
      ...Array.from({ length: 15 }, () => jsonResponse({ ok: true })),
      jsonResponse({ last_seq: 4 }),
      jsonResponse([]),
    ]);
    const client = new DispatchClient("http://dispatch.test/", "secret", fetchImpl);

    await client.issue({ project: "DSP", title: "Issue", spec: "# Spec", actor });
    await client.getIssue("DSP-1");
    await client.getIssueEvents("DSP-1", 4, 10);
    await client.ask("DSP-1", { question: "Choose", actor });
    await client.comment("DSP-1", { body: "Review", actor });
    await client.suggest("DSP-1", {
      body: "Use this",
      anchor: { artifact: "artifact-1", quote: "before" },
      replace_with: "after",
      actor,
    });
    await client.message("DSP-1", { body: "Update", actor });
    await client.artifact("DSP-1", {
      name: "spec.md",
      file: new Blob(["# Spec"], { type: "text/markdown" }),
      summary: "Initial spec",
      actor,
    });
    await client.getArtifact("artifact-1");
    await client.docRead("artifact-1");
    await client.docRead("artifact-1", 2);
    await client.docEdit("artifact-1", { ops: [], actor });
    await client.nameArtifactVersion("artifact-1", { summary: "Named", actor });
    await client.getAsk("ask-1");
    await client.getComments("DSP-1", "artifact-1");
    await client.read("DSP-1");

    expect(
      requests.map((request) => new URL(request.url).pathname + new URL(request.url).search)
    ).toEqual([
      "/api/v1/issues",
      "/api/v1/issues/DSP-1",
      "/api/v1/issues/DSP-1/events?after=4&limit=10",
      "/api/v1/issues/DSP-1/asks",
      "/api/v1/issues/DSP-1/comments",
      "/api/v1/issues/DSP-1/comments",
      "/api/v1/issues/DSP-1/messages",
      "/api/v1/issues/DSP-1/artifacts",
      "/api/v1/artifacts/artifact-1",
      "/api/v1/artifacts/artifact-1/text",
      "/api/v1/artifacts/artifact-1/versions/2",
      "/api/v1/artifacts/artifact-1/edits",
      "/api/v1/artifacts/artifact-1/versions",
      "/api/v1/asks/ask-1",
      "/api/v1/issues/DSP-1/comments?artifact=artifact-1",
      "/api/v1/issues/DSP-1",
      "/api/v1/issues/DSP-1/events?after=0&limit=10",
    ]);
    expect(requests.map((request) => request.init.method)).toEqual([
      "POST",
      "GET",
      "GET",
      "POST",
      "POST",
      "POST",
      "POST",
      "POST",
      "GET",
      "GET",
      "GET",
      "POST",
      "POST",
      "GET",
      "GET",
      "GET",
      "GET",
    ]);

    for (const request of requests) {
      expect((request.init.headers as Record<string, string>).Authorization).toBe("Bearer secret");
      expect((request.init.headers as Record<string, string>).Accept).toBe("application/json");
    }
    expect(requestBody(requests[0] as RecordedRequest)).toEqual({
      project: "DSP",
      title: "Issue",
      spec: "# Spec",
      actor,
    });
    expect(requestBody(requests[3] as RecordedRequest)).toEqual({ question: "Choose", actor });
    expect(requestBody(requests[4] as RecordedRequest)).toEqual({ body: "Review", actor });
    expect(requestBody(requests[5] as RecordedRequest)).toEqual({
      body: "Use this",
      anchor: { artifact: "artifact-1", quote: "before" },
      suggestion: { replace_with: "after" },
      actor,
    });
    expect(requestBody(requests[6] as RecordedRequest)).toEqual({ body: "Update", actor });
    expect(requestBody(requests[11] as RecordedRequest)).toEqual({ ops: [], actor });
    expect(requestBody(requests[12] as RecordedRequest)).toEqual({ summary: "Named", actor });

    const artifactBody = requests[7]?.init.body as FormData;
    expect(artifactBody).toBeInstanceOf(FormData);
    expect(artifactBody.get("name")).toBe("spec.md");
    expect(artifactBody.get("primary")).toBeNull();
    expect(artifactBody.get("summary")).toBe("Initial spec");
    expect(artifactBody.get("actor")).toBe(JSON.stringify(actor));
    expect((artifactBody.get("file") as Blob).type).toBe("text/markdown");
  });

  test("maps a structured conflict response into DispatchServiceError candidates", async () => {
    const candidates = [{ from: 12, to: 18, context: "duplicated text" }];
    const { fetchImpl } = fakeFetch([
      jsonResponse({ error: "quote is ambiguous", code: "TARGET_AMBIGUOUS", candidates }, 409),
    ]);
    const client = new DispatchClient("http://dispatch.test", "secret", fetchImpl);

    await expect(
      client.docEdit("artifact-1", { ops: [{ op: "delete", find: "same" }], actor })
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

  test("preserves a document precondition conflict's current tokens", async () => {
    const current = {
      document: "sha256:current-document",
      blocks: [{ id: "block-1", token: "sha256:current-block" }],
    };
    const mismatches = [
      {
        scope: "block",
        block_id: "block-1",
        expected: "sha256:stale-block",
        current: "sha256:current-block",
      },
    ];
    const { fetchImpl } = fakeFetch([
      jsonResponse(
        {
          error: 'document edit precondition failed: block "block-1" changed',
          code: "PRECONDITION_FAILED",
          current,
          mismatches,
        },
        409
      ),
    ]);
    const client = new DispatchClient("http://dispatch.test", "secret", fetchImpl);

    await expect(
      client.docEdit("artifact-1", {
        ops: [{ op: "delete", find: "same" }],
        precondition: { blocks: [{ id: "block-1", token: "sha256:stale-block" }] },
        actor,
      })
    ).rejects.toEqual(
      expect.objectContaining({
        name: "DispatchServiceError",
        code: "PRECONDITION_FAILED",
        status: 409,
        current,
        mismatches,
      })
    );
  });

  // Dispatch writes every answer as JSON, so a non-2xx body that is not its `{code, error}` came
  // from something in front of it: a gateway's HTML page, an empty body, JSON of another shape. The
  // agent reads only the error's message, so it names the request and the status, quotes the body
  // only as a one-line plain-text excerpt (scripts, styles and tags dropped), and says whether a
  // retry can help: on a read, a gateway's 5xx, 408 or 429 can clear, while any other status
  // answers the same request the same way. The bearer travels in a header, so the URL never
  // carries it. The error says it is a gateway's answer, so a caller with advice of its own can
  // use `answer` without the client's.
  test("names the request, status and a plain-text excerpt, advising a read's retry only for a status that can clear", async () => {
    const nginx =
      "<html>\r\n<head><title>502 Bad Gateway</title></head>\r\n<body>\r\n<center><h1>502 Bad Gateway</h1>" +
      "</center>\r\n<hr><center>nginx/1.25.3</center>\r\n</body>\r\n</html>\r\n";
    const cloudflare =
      "<html><head><style>body{margin:0}</style><script>var x=1;</script></head><body>" +
      "<h1>Error 522</h1><p>Connection timed out</p></body></html>";
    const { fetchImpl } = fakeFetch([
      new Response(nginx, {
        status: 502,
        statusText: "Bad Gateway",
        headers: { "Content-Type": "text/html" },
      }),
      new Response(cloudflare, { status: 522, headers: { "Content-Type": "text/html" } }),
      new Response(null, { status: 503, statusText: "Service Unavailable" }),
      new Response("x".repeat(500), { status: 429, headers: { "Content-Type": "text/plain" } }),
      new Response(JSON.stringify({ message: "upstream timed out" }), {
        status: 504,
        statusText: "Gateway Timeout",
        headers: { "Content-Type": "application/json" },
      }),
      new Response("<html><body><h1>404 Not Found</h1></body></html>", {
        status: 404,
        statusText: "Not Found",
        headers: { "Content-Type": "text/html" },
      }),
    ]);
    const client = new DispatchClient("http://dispatch.test", "secret", fetchImpl);
    const refusal = () =>
      client.getIssueEvents("DSP-1", 4, 10).then(
        () => {
          throw new Error("expected a refusal");
        },
        (error: Error) => error
      );
    const request =
      "GET http://dispatch.test/api/v1/issues/DSP-1/events?after=4&limit=10 answered ";
    const gateway = ", which looks like a proxy or gateway page rather than Dispatch's own answer";
    const maySucceed = `${gateway}, so a retry may succeed.`;
    const sameAgain =
      `${gateway}, so a retry gets the same answer until the Dispatch URL, or whatever answers ` +
      "in its place, is fixed.";

    const badGateway = await refusal();
    const answer =
      `${request}502 Bad Gateway with a body that is not Dispatch's error JSON ` +
      `("502 Bad Gateway 502 Bad Gateway nginx/1.25.3")${gateway}`;
    expect(badGateway).toBeInstanceOf(DispatchGatewayError);
    expect(badGateway).toEqual(
      expect.objectContaining({
        name: "DispatchServiceError",
        code: "HTTP_502",
        status: 502,
        answer,
        transient: true,
        message: `${answer}, so a retry may succeed.`,
      })
    );
    expect((await refusal()).message).toBe(
      `${request}522 with a body that is not Dispatch's error JSON ("Error 522 Connection timed out")${maySucceed}`
    );
    expect((await refusal()).message).toBe(
      `${request}503 Service Unavailable with an empty body${maySucceed}`
    );
    expect((await refusal()).message).toBe(
      `${request}429 with a body that is not Dispatch's error JSON ("${"x".repeat(120)}…")${maySucceed}`
    );
    expect((await refusal()).message).toBe(
      `${request}504 Gateway Timeout with a body that is not Dispatch's error JSON ` +
        `(${JSON.stringify('{"message":"upstream timed out"}')})${maySucceed}`
    );
    const notFound = await refusal();
    expect(notFound.message).toBe(
      `${request}404 Not Found with a body that is not Dispatch's error JSON ("404 Not Found")${sameAgain}`
    );
    expect(notFound).toEqual(expect.objectContaining({ code: "HTTP_404", transient: false }));
  });

  // A gateway's 5xx can come back after it forwarded a write and Dispatch applied it, so advising
  // a retry there can post a message twice: the write is told to check what took effect. A 408 or
  // 429 is the gateway's own timeout or rate limit, sent before it forwards the request, so the
  // write never reached Dispatch and a retry may succeed. Any other status still gets the same
  // advice as a read, since the request never got past the gateway either.
  test("tells a write a gateway answered whether it may have reached Dispatch", async () => {
    const page = (status: number, statusText: string) =>
      new Response(`<html><body><h1>${status} ${statusText}</h1></body></html>`, {
        status,
        statusText,
        headers: { "Content-Type": "text/html" },
      });
    const { fetchImpl } = fakeFetch([
      page(502, "Bad Gateway"),
      page(408, "Request Timeout"),
      page(429, "Too Many Requests"),
      page(404, "Not Found"),
    ]);
    const client = new DispatchClient("http://dispatch.test", "secret", fetchImpl);
    const refusal = () =>
      client.message("DSP-1", { body: "Shipped.", actor }).then(
        () => {
          throw new Error("expected a refusal");
        },
        (error: Error) => error
      );
    const request = "POST http://dispatch.test/api/v1/issues/DSP-1/messages answered ";
    const gateway = ", which looks like a proxy or gateway page rather than Dispatch's own answer";
    const page404 = `${request}404 Not Found with a body that is not Dispatch's error JSON ("404 Not Found")`;

    const badGateway = await refusal();
    expect(badGateway.message).toBe(
      `${request}502 Bad Gateway with a body that is not Dispatch's error JSON ("502 Bad Gateway")` +
        `${gateway}, so the write may or may not have reached Dispatch: check whether it took ` +
        "effect before retrying it."
    );
    expect(badGateway).toEqual(expect.objectContaining({ mayHaveReachedDispatch: true }));
    for (const [status, statusText] of [
      [408, "Request Timeout"],
      [429, "Too Many Requests"],
    ] as const) {
      const refused = await refusal();
      expect(refused.message).toBe(
        `${request}${status} ${statusText} with a body that is not Dispatch's error JSON ` +
          `("${status} ${statusText}")${gateway}, so the write did not reach Dispatch, and a ` +
          "retry may succeed."
      );
      expect(refused).toEqual(
        expect.objectContaining({ transient: true, mayHaveReachedDispatch: false })
      );
    }
    expect((await refusal()).message).toBe(
      `${page404}${gateway}, so a retry gets the same answer until the Dispatch URL, or whatever ` +
        "answers in its place, is fixed."
    );
  });

  // The excerpt and the reason phrase are text a gateway chose, and a misconfigured one can echo
  // the request back, headers included. Two kinds of credential are redacted before either is
  // quoted: the client's own bearer wherever it appears (trimmed, as fetch sends it), and the
  // value after `Authorization:` or `Bearer`. The reason phrase is cut to one line like the body.
  test("redacts the bearer and an Authorization or Bearer value from the excerpt and the reason phrase", async () => {
    const token = "dsp_live_4f9a2c7e";
    const echo =
      `<html><body><h1>401 Unauthorized</h1><pre>token=${token}\r\n` +
      "Authorization: Basic dXNlcjpwYXNz\r\nX-Forwarded: Bearer other.jwt-value</pre></body></html>";
    const { fetchImpl } = fakeFetch([
      new Response(echo, {
        status: 401,
        statusText: "Unauthorized",
        headers: { "Content-Type": "text/html" },
      }),
      new Response(JSON.stringify({ headers: { authorization: `Bearer ${token}` } }), {
        status: 502,
        headers: { "Content-Type": "application/json" },
      }),
      new Response(null, { status: 502, statusText: `Denied for ${token} ${"x".repeat(200)}` }),
      new Response(`<p>token=${token}</p>`, { status: 401, statusText: "Unauthorized" }),
    ]);
    const client = new DispatchClient("http://dispatch.test", token, fetchImpl);
    const refusal = (from: DispatchClient) =>
      from.getIssue("DSP-1").then(
        () => "",
        (error: Error) => error.message
      );
    const request = "GET http://dispatch.test/api/v1/issues/DSP-1 answered ";
    const gateway = ", which looks like a proxy or gateway page rather than Dispatch's own answer";

    const unauthorized = await refusal(client);
    expect(unauthorized).toBe(
      `${request}401 Unauthorized with a body that is not Dispatch's error JSON ("401 Unauthorized ` +
        'token=[redacted] Authorization: Basic [redacted] X-Forwarded: Bearer [redacted]")' +
        `${gateway}, so a retry gets the same answer until the Dispatch URL, or whatever answers ` +
        "in its place, is fixed."
    );
    const echoedJson = await refusal(client);
    expect(echoedJson).toContain(
      JSON.stringify('{"headers":{"authorization":"Bearer [redacted]"}}')
    );
    const reasonPhrase = await refusal(client);
    expect(reasonPhrase).toBe(
      `${request}502 Denied for [redacted] ${"x".repeat(98)}… with an empty body${gateway}, so a ` +
        "retry may succeed."
    );
    const untrimmed = new DispatchClient("http://dispatch.test", ` ${token}\n`, fetchImpl);
    const echoedTrimmed = await refusal(untrimmed);
    expect(echoedTrimmed).toContain('("token=[redacted]")');
    for (const message of [unauthorized, echoedJson, reasonPhrase, echoedTrimmed]) {
      expect(message).not.toContain(token);
      expect(message).not.toContain("dXNlcjpwYXNz");
      expect(message).not.toContain("other.jwt-value");
    }
  });

  // Whatever answers at the Dispatch URL (a repository's own `.opencode/envoy.json` can name it)
  // controls the body, and the excerpt is built synchronously on the host's event loop, before the
  // 120-character cut. Each pass over the body must stay linear and the scan bounded, or a page of
  // unclosed tags freezes the session: a quadratic pass takes seconds on tens of kilobytes. The
  // bound is generous so a loaded machine passes and a quadratic pass does not.
  test("builds the excerpt of a megabyte of unclosed tags in bounded time", async () => {
    const megabyte = 1 << 20;
    // The last three hold the client's bearer, "secret", everywhere, or almost everywhere.
    for (const unit of [
      "<",
      "<script",
      "<script x",
      "<a <b",
      "</script>",
      "<script></script ",
      "secret",
      "<script>secret",
      "secre",
    ]) {
      const hostile = unit.repeat(Math.ceil(megabyte / unit.length));
      const { fetchImpl } = fakeFetch([
        new Response(hostile, { status: 502, headers: { "Content-Type": "text/html" } }),
      ]);
      const client = new DispatchClient("http://dispatch.test", "secret", fetchImpl);
      const started = performance.now();
      const message = await client.getIssue("DSP-1").then(
        () => "",
        (error: Error) => error.message
      );

      expect(performance.now() - started).toBeLessThan(2_000);
      expect(message).toStartWith(
        "GET http://dispatch.test/api/v1/issues/DSP-1 answered 502 with "
      );
    }
  });

  // The excerpt reads at most the first 64 KiB of the body, so what it costs has a bound however
  // large the body is. Redacting the bearer shortens the text, so it runs on that slice: redacting
  // the whole body first pulls text from past the limit into the excerpt, and a slice cut through
  // a bearer would leave its first characters to be pulled in the same way. A bearer that starts
  // inside the limit and runs past it is redacted whole.
  test("scans at most the first 64 KiB of a body, however often it holds the bearer", async () => {
    const limit = 64 * 1024;
    const bearer = `dsp_${"Zq9x".repeat(10)}`;
    const styled = `<style>${bearer.repeat(Math.floor((limit - 80) / bearer.length))}</style>`;
    const shortOfLimit = limit - styled.length;
    // Visible words end at the limit, and a bearer starts one character past it.
    const pastLimit = `${styled}${"p".repeat(shortOfLimit)} ${bearer} after the limit`;
    // A bearer starts five characters short of the limit and ends past it.
    const acrossLimit = `${styled}${"q".repeat(shortOfLimit - 5)}${bearer} after the limit`;
    const { fetchImpl } = fakeFetch(
      [pastLimit, acrossLimit].map(
        (body) => new Response(body, { status: 502, headers: { "Content-Type": "text/html" } })
      )
    );
    const client = new DispatchClient("http://dispatch.test", bearer, fetchImpl);
    const refusal = () =>
      client.getIssue("DSP-1").then(
        () => "",
        (error: Error) => error.message
      );

    const past = await refusal();
    expect(past).toContain(`("${"p".repeat(shortOfLimit)}")`);
    const across = await refusal();
    expect(across).toContain(`("${"q".repeat(shortOfLimit - 5)}[redacted]")`);
    for (const message of [past, across]) {
      expect(message).not.toContain("after the limit");
      expect(message).not.toContain(bearer.slice(0, 8));
    }
  });

  // A gateway can echo the header cut short, split by markup, or twice with the copies
  // overlapping (a bearer that ends in its own first characters), and an exact match finds none
  // of those. Every run of 8 or more of the bearer's characters within what the excerpt reads is
  // redacted, one that starts inside the scan limit and runs past it included, so no piece of the
  // bearer that long reaches the message. Each body holds such a piece, so the search can see one.
  // Dispatch's tokens are `dsp_` and 43 base64url characters; this one ends in its first two.
  const pieceBearer = "dsp_Q7vK2mXr9LtW4nBz8YcH1jPe5sAf3gUo6iNq0wEbTds";
  /** Every run of 8 of the bearer's characters `text` holds; any longer piece holds one. */
  const bearerPieces = (text: string) =>
    Array.from({ length: pieceBearer.length - 7 }, (_, at) => pieceBearer.slice(at, at + 8)).filter(
      (piece) => text.includes(piece)
    );
  test.each([
    [
      "cut short by its last character",
      `<p>echo=${pieceBearer.slice(0, -1)} end</p>`,
      "echo=[redacted] end",
    ],
    [
      "cut short by its last 10 characters",
      `<p>echo=${pieceBearer.slice(0, -10)} end</p>`,
      "echo=[redacted] end",
    ],
    [
      "followed by a copy that starts inside its last characters",
      `<p>echo=${pieceBearer}${pieceBearer.slice(2)} end</p>`,
      "echo=[redacted] end",
    ],
    [
      "split by a tag",
      `<p>echo=${pieceBearer.slice(0, 20)}<wbr>${pieceBearer.slice(20)} end</p>`,
      "echo=[redacted] [redacted] end",
    ],
    [
      // A 64 KiB scan limit; the copy starts 20 characters inside it and ends 17 past it.
      "cut short, across the scan limit",
      `<style>${"s".repeat(64 * 1024 - 75)}</style>${"q".repeat(40)}${pieceBearer.slice(0, -10)} after the limit`,
      `${"q".repeat(40)}[redacted]`,
    ],
  ] as const)("redacts a piece of the bearer %s", async (_copy, body, quoted) => {
    const { fetchImpl } = fakeFetch([
      new Response(body, { status: 502, headers: { "Content-Type": "text/html" } }),
    ]);
    const client = new DispatchClient("http://dispatch.test", pieceBearer, fetchImpl);

    const message = await client.getIssue("DSP-1").then(
      () => "",
      (error: Error) => error.message
    );

    expect(bearerPieces(body)).not.toEqual([]);
    expect(bearerPieces(message)).toEqual([]);
    expect(message).toContain(`(${JSON.stringify(quoted)})`);
    expect(message).not.toContain("after the limit");
  });

  // A body of nothing but markup is not empty, and an agent told it was would look for a gateway
  // that sent nothing. The answer gives the body's size in bytes and says none of it reads as
  // text, or none of what the excerpt reads when the body runs past that.
  test("says a body of markup alone has no readable text, with its size, rather than that it is empty", async () => {
    const { fetchImpl } = fakeFetch([
      new Response("<p>\u00a0</p>", { status: 502, headers: { "Content-Type": "text/html" } }),
      new Response("<script>".repeat(1 << 17), {
        status: 502,
        headers: { "Content-Type": "text/html" },
      }),
    ]);
    const client = new DispatchClient("http://dispatch.test", "secret", fetchImpl);
    const refusal = () =>
      client.getIssue("DSP-1").then(
        () => "",
        (error: Error) => error.message
      );
    const request = "GET http://dispatch.test/api/v1/issues/DSP-1 answered 502 with ";
    const gateway =
      ", which looks like a proxy or gateway page rather than Dispatch's own answer, so a retry " +
      "may succeed.";

    expect(await refusal()).toBe(`${request}a body of 9 bytes and no readable text${gateway}`);
    expect(await refusal()).toBe(
      `${request}a body of 1,048,576 bytes and no readable text in its first 64 KiB${gateway}`
    );
  });

  // Dispatch's own refusal is JSON with a string `error`; `code` names the check when the server
  // sets one (`writeError`) and is `HTTP_<status>` otherwise. Both render as they always did: the
  // server's text, nothing about requests or retries.
  test("renders Dispatch's own JSON error as its text, with or without a code", async () => {
    const { fetchImpl } = fakeFetch([
      jsonResponse({ code: "ISSUE_CLOSED", error: "issue is closed" }, 409),
      jsonResponse({ error: "no route for GET /api/v1/issues/DSP-1" }, 404),
    ]);
    const client = new DispatchClient("http://dispatch.test", "secret", fetchImpl);

    await expect(client.getIssue("DSP-1")).rejects.toEqual(
      expect.objectContaining({ code: "ISSUE_CLOSED", status: 409, message: "issue is closed" })
    );
    await expect(client.getIssue("DSP-1")).rejects.toEqual(
      expect.objectContaining({
        code: "HTTP_404",
        status: 404,
        message: "no route for GET /api/v1/issues/DSP-1",
      })
    );
  });

  test("resolves an external issue reference once before native-key routes", async () => {
    const { fetchImpl, requests } = fakeFetch([
      jsonResponse({ key: "DSP-42" }),
      jsonResponse({ key: "DSP-42", last_seq: 3 }),
      jsonResponse([]),
    ]);
    const client = new DispatchClient("http://dispatch.test", "secret", fetchImpl);

    await client.read("owner/repo#42");

    expect(
      requests.map((request) => new URL(request.url).pathname + new URL(request.url).search)
    ).toEqual([
      "/api/v1/issues/resolve?ref=owner%2Frepo%2342",
      "/api/v1/issues/DSP-42",
      "/api/v1/issues/DSP-42/events?after=0&limit=10",
    ]);
  });

  test("maps comment reply-chain reads to the API", async () => {
    const { fetchImpl, requests } = fakeFetch([
      jsonResponse({
        comment: {
          id: "comment-1",
          issue_key: "DSP-1",
          author: actor,
          body: "Root comment",
          anchor: null,
          reply_to: null,
          resolved: false,
          suggestion: null,
          created_at: "2026-09-09T00:00:00Z",
        },
        replies: [
          {
            id: "comment-2",
            issue_key: "DSP-1",
            author: actor,
            body: "Reply",
            anchor: null,
            reply_to: "comment-1",
            resolved: false,
            suggestion: null,
            created_at: "2026-09-09T00:01:00Z",
          },
        ],
      }),
    ]);
    const client = new DispatchClient("http://dispatch.test", "secret", fetchImpl);

    await client.getComment("comment-1");
    expect(
      requests.map((request) => new URL(request.url).pathname + new URL(request.url).search)
    ).toEqual(["/api/v1/comments/comment-1"]);
  });
});

test("maps an ask resolution to the authenticated API", async () => {
  const { fetchImpl, requests } = fakeFetch([
    jsonResponse({
      id: "ask-1",
      issue_key: "DSP-1",
      state: "resolved",
      resolution: {
        actor,
        at: "2026-09-10T00:00:00Z",
        kind: "retracted",
        reason: "A newer question supersedes this one.",
      },
    }),
  ]);
  const client = new DispatchClient("http://dispatch.test", "secret", fetchImpl);
  const resolveAsk = Reflect.get(client, "resolveAsk");

  expect(typeof resolveAsk).toBe("function");
  if (typeof resolveAsk !== "function") throw new Error("DispatchClient.resolveAsk is missing");
  await resolveAsk.call(client, "ask-1", {
    actor,
    kind: "retracted",
    reason: "A newer question supersedes this one.",
  });

  expect(requests.map((request) => [request.init.method, new URL(request.url).pathname])).toEqual([
    ["POST", "/api/v1/asks/ask-1/resolve"],
  ]);
  expect(requestBody(requests[0] as RecordedRequest)).toEqual({
    actor,
    kind: "retracted",
    reason: "A newer question supersedes this one.",
  });
});
