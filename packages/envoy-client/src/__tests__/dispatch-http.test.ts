import { describe, expect, test } from "bun:test";
import { DispatchClient } from "../dispatch-http";

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
  test("serializes the updated_since boundary when listing issues", async () => {
    const { fetchImpl, requests } = fakeFetch([jsonResponse([])]);
    const client = new DispatchClient("http://dispatch.test", "secret", fetchImpl);

    await client.listIssues({ project: "DSP", updated_since: "2026-09-10T12:00:00Z" });

    expect(requests).toHaveLength(1);
    expect(requests[0]?.url).toContain(
      "/api/v1/issues?project=DSP&updated_since=2026-09-10T12%3A00%3A00Z"
    );
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

  test("names malformed server errors without hiding their HTTP status", async () => {
    const { fetchImpl } = fakeFetch([new Response("unavailable", { status: 503 })]);
    const client = new DispatchClient("http://dispatch.test", "secret", fetchImpl);

    await expect(client.getIssue("DSP-1")).rejects.toEqual(
      expect.objectContaining({
        code: "HTTP_503",
        status: 503,
        message: "unavailable",
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

  test("creates an unlinked external issue once and resolves it on a later client request", async () => {
    const { fetchImpl, requests } = fakeFetch([
      jsonResponse({ code: "NOT_FOUND", error: "issue was not found" }, 404),
      jsonResponse({ key: "DSP-42" }, 201),
      jsonResponse({ key: "DSP-42" }),
    ]);
    const firstClient = new DispatchClient("http://dispatch.test", "secret", fetchImpl);
    const secondClient = new DispatchClient("http://dispatch.test", "secret", fetchImpl);

    await expect(firstClient.ensureIssue("owner/repo#42", actor)).resolves.toBe("DSP-42");
    await expect(secondClient.ensureIssue("owner/repo#42", actor)).resolves.toBe("DSP-42");

    expect(
      requests.map((request) => new URL(request.url).pathname + new URL(request.url).search)
    ).toEqual([
      "/api/v1/issues/resolve?ref=owner%2Frepo%2342",
      "/api/v1/issues",
      "/api/v1/issues/resolve?ref=owner%2Frepo%2342",
    ]);
    expect(requestBody(requests[1] as RecordedRequest)).toEqual({
      external: "owner/repo#42",
      actor,
    });
  });

  test("shares concurrent external issue creation in one client", async () => {
    const { fetchImpl, requests } = fakeFetch([
      jsonResponse({ code: "NOT_FOUND", error: "issue was not found" }, 404),
      jsonResponse({ key: "DSP-42" }, 201),
    ]);
    const client = new DispatchClient("http://dispatch.test", "secret", fetchImpl);

    await expect(
      Promise.all([
        client.ensureIssue("owner/repo#42", actor),
        client.ensureIssue("owner/repo#42", actor),
      ])
    ).resolves.toEqual(["DSP-42", "DSP-42"]);

    expect(
      requests.map((request) => new URL(request.url).pathname + new URL(request.url).search)
    ).toEqual(["/api/v1/issues/resolve?ref=owner%2Frepo%2342", "/api/v1/issues"]);
  });

  test("re-resolves an external issue after retryable creation failures", async () => {
    for (const status of [409, 500]) {
      const { fetchImpl, requests } = fakeFetch([
        jsonResponse({ code: "NOT_FOUND", error: "issue was not found" }, 404),
        jsonResponse({ error: "creation raced" }, status),
        jsonResponse({ key: "DSP-42" }),
      ]);
      const client = new DispatchClient("http://dispatch.test", "secret", fetchImpl);

      await expect(client.ensureIssue("owner/repo#42", actor)).resolves.toBe("DSP-42");
      expect(
        requests.map((request) => new URL(request.url).pathname + new URL(request.url).search)
      ).toEqual([
        "/api/v1/issues/resolve?ref=owner%2Frepo%2342",
        "/api/v1/issues",
        "/api/v1/issues/resolve?ref=owner%2Frepo%2342",
      ]);
    }
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
