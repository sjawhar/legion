import { expect, test } from "bun:test";

import {
  createApiClient,
  type FetchImplementation,
  isRetryableQueryError,
  isUnauthorized,
} from "../api/client";
import type { Version } from "../api/types";

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

test("API client normalizes server asks with null options", async () => {
  const stub = stubFetch(() =>
    Response.json([
      {
        anchor: null,
        answer: null,
        author: { id: "agent", kind: "session" },
        created_at: "2026-09-09T00:00:00Z",
        id: "ask-1",
        issue_key: "CORE-1",
        multiple: false,
        options: null,
        question: "Which path?",
        state: "open",
        urgency: "med",
      },
    ])
  );

  expect((await createApiClient(stub.fetch).getInbox())[0]?.options).toEqual([]);
});
test("API client preserves the null version returned by a no-op edit", async () => {
  const stub = stubFetch(() => Response.json({ applied: 0, version: null }));
  const result: { applied: number; version: Version | null } = await createApiClient(
    stub.fetch
  ).editArtifact("artifact-1", { ops: [] });

  expect(result).toEqual({ applied: 0, version: null });
});

test("API client uploads inline artifact content as JSON and files as multipart", async () => {
  const stub = stubFetch(() => Response.json({ artifact: {}, version: {} }));
  const api = createApiClient(stub.fetch);

  await api.uploadArtifact("CORE-1", { content: "# Spec\n", name: "spec.md", primary: true });
  await api.uploadArtifact("CORE-1", {
    file: new File(["spec"], "spec.md", { type: "text/markdown" }),
    name: "spec.md",
  });

  const [inline, multipart] = stub.requests;
  expect(inline?.path).toBe("/api/v1/issues/CORE-1/artifacts");
  expect(inline?.init?.method).toBe("POST");
  expect(JSON.parse(String(inline?.init?.body))).toEqual({
    content: "# Spec\n",
    name: "spec.md",
    primary: true,
  });
  expect(multipart?.init?.body).toBeInstanceOf(FormData);
  expect((multipart?.init?.body as FormData | undefined)?.get("name")).toBe("spec.md");
});

test("API client sends the documented method and JSON body for mutations", async () => {
  const stub = stubFetch();
  const api = createApiClient(stub.fetch);

  await api.createProject({
    actor: { id: "session-1", kind: "session" },
    key: "CORE",
    name: "Core",
  });
  await api.patchIssue("CORE-1", { status: "done" });
  await api.answerAsk("ask-1", { selected: ["Ship"], text: "Approved" });
  await api.editArtifact("artifact-1", {
    ops: [{ op: "replace", find: "draft", with: "final" }],
    summary: "Publish final copy",
  });

  expect(stub.requests.map(({ init, path }) => [init?.method, path])).toEqual([
    ["POST", "/api/v1/projects"],
    ["PATCH", "/api/v1/issues/CORE-1"],
    ["POST", "/api/v1/asks/ask-1/answer"],
    ["POST", "/api/v1/artifacts/artifact-1/edits"],
  ]);
  expect(JSON.parse(stub.requests[0]?.body as string)).toEqual({
    actor: { id: "session-1", kind: "session" },
    key: "CORE",
    name: "Core",
  });
  expect(JSON.parse(stub.requests[1]?.body as string)).toEqual({ status: "done" });
  expect(JSON.parse(stub.requests[2]?.body as string)).toEqual({
    selected: ["Ship"],
    text: "Approved",
  });
  expect(JSON.parse(stub.requests[3]?.body as string)).toEqual({
    ops: [{ op: "replace", find: "draft", with: "final" }],
    summary: "Publish final copy",
  });
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

test("API client sends inline artifacts as JSON", async () => {
  const stub = stubFetch(() => Response.json({ artifact: {}, version: {} }));
  const api = createApiClient(stub.fetch);

  await api.uploadArtifact("CORE-1", { content: "# Draft", name: "spec.md", summary: "Initial" });

  expect(stub.requests.map(({ init, path }) => [init?.method, path])).toEqual([
    ["POST", "/api/v1/issues/CORE-1/artifacts"],
  ]);
  expect(stub.requests[0]?.init?.headers).toEqual({ "Content-Type": "application/json" });
  expect(JSON.parse(stub.requests[0]?.body as string)).toEqual({
    content: "# Draft",
    name: "spec.md",
    summary: "Initial",
  });
});

test("API client encodes list filters and artifact version query parameters", async () => {
  const stub = stubFetch();
  const api = createApiClient(stub.fetch);

  await api.listIssues({
    project: "CORE",
    status: "in progress",
    parent: "CORE-1",
    updated_since: "2026-09-10T12:00:00Z",
  });
  await api.getIssueEvents("CORE-1", { after: 3, limit: 20 });
  await api.getIssueEvents("CORE-1", { before: 40, order: "desc" });
  await api.getIssueEvents("CORE-1", { ids: ["42", "10"] });
  await api.listComments("CORE-1", "spec");
  await api.resolveIssue("owner/repo#42");

  expect(stub.requests.map(({ path }) => path)).toEqual([
    "/api/v1/issues?project=CORE&status=in+progress&parent=CORE-1&updated_since=2026-09-10T12%3A00%3A00Z",
    "/api/v1/issues/CORE-1/events?after=3&limit=20",
    "/api/v1/issues/CORE-1/events?before=40&order=desc",
    "/api/v1/issues/CORE-1/events?ids=42%2C10",
    "/api/v1/issues/CORE-1/comments?artifact=spec",
    "/api/v1/issues/resolve?ref=owner%2Frepo%2342",
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

test("isRetryableQueryError exempts auth outcomes (401, 403) but retries a transient 5xx", async () => {
  const unauthorized = createApiClient(stubFetch(() => new Response(null, { status: 401 })).fetch);
  const forbidden = createApiClient(
    stubFetch(() =>
      Response.json({ error: "login not allowed", code: "LOGIN_NOT_ALLOWED" }, { status: 403 })
    ).fetch
  );
  const serverError = createApiClient(stubFetch(() => new Response(null, { status: 503 })).fetch);

  const [unauthorizedError, forbiddenError, serverErrorResult] = await Promise.all([
    unauthorized.whoAmI().catch((error: unknown) => error),
    forbidden.whoAmI().catch((error: unknown) => error),
    serverError.whoAmI().catch((error: unknown) => error),
  ]);

  expect(isRetryableQueryError(unauthorizedError)).toBe(false);
  expect(isRetryableQueryError(forbiddenError)).toBe(false);
  expect(isRetryableQueryError(serverErrorResult)).toBe(true);
});
test("API client reaches every remaining documented endpoint", async () => {
  const stub = stubFetch((request) => {
    if (request.path.startsWith("/api/v1/inbox")) {
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

  await api.getProjects();
  await api.listIssues();
  await api.createIssue({ project: "CORE", title: "Ship it" });
  await api.getIssue("CORE-1");
  await api.getInbox("CORE");
  await api.createAsk("CORE-1", { question: "Ship?" });
  await api.getAsk("ask-1");
  await api.createComment("CORE-1", { body: "Looks good" });
  await api.resolveComment("comment-1");
  await api.acceptComment("comment-1");
  await api.rejectComment("comment-1");
  await api.createMessage("CORE-1", { body: "Ready" });
  await api.listArtifacts("CORE-1");
  await api.uploadArtifact("CORE-1", {
    file: new File(["spec"], "spec.md", { type: "text/markdown" }),
    name: "spec.md",
    primary: true,
  });
  await api.makeArtifactPrimary("artifact-1");
  await api.getArtifact("artifact-1");
  await api.getArtifactText("artifact-1");
  await api.getArtifactVersion("artifact-1", 3);
  await api.createArtifactVersion("artifact-1", { summary: "Save" });
  await api.getMyState();
  await api.putIssueState("CORE-1", { pinned: true });
  await api.whoAmI();
  await api.logout();
  await api.githubRest("repos/acme/dispatch");
  await api.githubGraphql("{ viewer { login } }");
  await api.health();

  expect(stub.requests.map(({ init, path }) => [init?.method ?? "GET", path])).toEqual([
    ["GET", "/api/v1/projects"],
    ["GET", "/api/v1/issues"],
    ["POST", "/api/v1/issues"],
    ["GET", "/api/v1/issues/CORE-1"],
    ["GET", "/api/v1/inbox?project=CORE"],
    ["POST", "/api/v1/issues/CORE-1/asks"],
    ["GET", "/api/v1/asks/ask-1"],
    ["POST", "/api/v1/issues/CORE-1/comments"],
    ["POST", "/api/v1/comments/comment-1/resolve"],
    ["POST", "/api/v1/comments/comment-1/accept"],
    ["POST", "/api/v1/comments/comment-1/reject"],
    ["POST", "/api/v1/issues/CORE-1/messages"],
    ["GET", "/api/v1/issues/CORE-1/artifacts"],
    ["POST", "/api/v1/issues/CORE-1/artifacts"],
    ["POST", "/api/v1/artifacts/artifact-1/primary"],
    ["GET", "/api/v1/artifacts/artifact-1"],
    ["GET", "/api/v1/artifacts/artifact-1/text"],
    ["GET", "/api/v1/artifacts/artifact-1/versions/3"],
    ["POST", "/api/v1/artifacts/artifact-1/versions"],
    ["GET", "/api/v1/me/state"],
    ["PUT", "/api/v1/me/issues/CORE-1/state"],
    ["GET", "/auth/whoami"],
    ["POST", "/auth/logout"],
    ["GET", "/api/github/rest/repos/acme/dispatch"],
    ["POST", "/api/github/graphql"],
    ["GET", "/healthz"],
  ]);
});
