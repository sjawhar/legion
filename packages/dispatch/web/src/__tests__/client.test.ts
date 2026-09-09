import { expect, test } from "bun:test";

import { createApiClient, type FetchImplementation } from "../api/client";

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

test("API client sends the documented method and JSON body for mutations", async () => {
  const stub = stubFetch();
  const api = createApiClient(stub.fetch);

  await api.createProject({ key: "CORE", name: "Core" });
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
  expect(JSON.parse(stub.requests[0]?.body as string)).toEqual({ key: "CORE", name: "Core" });
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

test("API client encodes list filters and artifact version query parameters", async () => {
  const stub = stubFetch();
  const api = createApiClient(stub.fetch);

  await api.listIssues({ project: "CORE", status: "in progress", parent: "CORE-1" });
  await api.getIssueEvents("CORE-1", { after: 3, limit: 20 });
  await api.listComments("CORE-1", "spec");
  await api.resolveIssue("owner/repo#42");

  expect(stub.requests.map(({ path }) => path)).toEqual([
    "/api/v1/issues?project=CORE&status=in+progress&parent=CORE-1",
    "/api/v1/issues/CORE-1/events?after=3&limit=20",
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

test("API client reaches every remaining documented endpoint", async () => {
  const stub = stubFetch();
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
