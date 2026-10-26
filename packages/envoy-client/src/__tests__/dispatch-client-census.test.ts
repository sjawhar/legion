import { afterEach, expect, spyOn, test } from "bun:test";
import { IDEMPOTENCY_KEY_HEADER, IDEMPOTENCY_KEY_PATTERN } from "@legion/contracts";
import { DISPATCH_WRITE_RETRY, DispatchClient, forgetKeyedLayerMemory } from "../dispatch-http";

// Every write the `dispatch` command makes carries one key because every request DispatchClient
// makes goes through its one request function. This drives every method of the class through a
// recording fetch, so a method added later that reaches Dispatch any other way - its own fetch, the
// global one, a write without the key - fails here, as does a method the table below leaves out.

/** The names of DispatchClient's methods: every function member but the injected fetch. */
type ClientMethod = Exclude<
  {
    [K in keyof DispatchClient]: DispatchClient[K] extends (...args: never[]) => unknown
      ? K
      : never;
  }[keyof DispatchClient],
  "fetchImpl"
>;

const actor = { kind: "session" as const, id: "session-1" };
const file = { name: "notes.md", file: new Blob(["# Notes"], { type: "text/markdown" }), actor };
const inline = { name: "notes.md", content: "# Notes", actor };

/** One call of each method, with arguments of the shapes it takes; what it answers is not read. */
const CALLS: Record<ClientMethod, (client: DispatchClient) => Promise<unknown>> = {
  issue: (client) => client.issue({ project: "DSP", title: "A title", actor }),
  resolveIssue: (client) => client.resolveIssue("owner/repo#1"),
  listIssuePage: (client) => client.listIssuePage({}, { limit: 1, offset: 0 }),
  listAllIssues: (client) => client.listAllIssues({}),
  listProjectArtifacts: (client) => client.listProjectArtifacts("DSP"),
  projectArtifact: (client) => client.projectArtifact("DSP", inline),
  getProjectArtifact: (client) => client.getProjectArtifact("DSP", "notes"),
  agentArtifact: (client) => client.agentArtifact("session-1", file),
  getAgentArtifact: (client) => client.getAgentArtifact("session-1", "notes"),
  getIssueArtifact: (client) => client.getIssueArtifact("DSP-1", "notes"),
  search: (client) => client.search("notes"),
  getIssue: (client) => client.getIssue("DSP-1"),
  updateIssue: (client) => client.updateIssue("DSP-1", { status: "testing", actor }),
  claimIssue: (client) => client.claimIssue("DSP-1", { actor }),
  releaseIssueClaim: (client) => client.releaseIssueClaim("DSP-1", { actor }),
  getIssueEvents: (client) => client.getIssueEvents("DSP-1"),
  read: (client) => client.read("DSP-1"),
  ask: (client) => client.ask("DSP-1", { question: "Which?", actor } as never),
  openAsks: (client) => client.openAsks("session-1"),
  openAsksForProject: (client) => client.openAsksForProject("DSP"),
  listAgents: (client) => client.listAgents(),
  whoami: (client) => client.whoami(),
  createProject: (client) => client.createProject({ key: "DSP", name: "Dispatch" } as never),
  listProjects: (client) => client.listProjects(),
  listArchitectureSources: (client) => client.listArchitectureSources("DSP"),
  syncArchitectureSources: (client) => client.syncArchitectureSources("DSP"),
  syncArchitectureSource: (client) => client.syncArchitectureSource("DSP", "acme/infra"),
  resolveAsk: (client) => client.resolveAsk("ask-1", { answer: "This one.", actor } as never),
  requestApproval: (client) => client.requestApproval("artifact-1", { actor, summary: "Spec" }),
  editAsk: (client) => client.editAsk("ask-1", { question: "Which one?", actor } as never),
  comment: (client) => client.comment("DSP-1", { body: "Shipped.", actor }),
  listIssueAsks: (client) => client.listIssueAsks("DSP-1"),
  getArtifactAsks: (client) => client.getArtifactAsks("artifact-1"),
  artifactAsk: (client) => client.artifactAsk("artifact-1", { question: "Which?", actor } as never),
  suggest: (client) => client.suggest("DSP-1", { replace_with: "better", actor } as never),
  getArtifactComments: (client) => client.getArtifactComments("artifact-1"),
  artifactComment: (client) => client.artifactComment("artifact-1", { body: "Noted.", actor }),
  artifactSuggest: (client) =>
    client.artifactSuggest("artifact-1", { replace_with: "better", actor } as never),
  message: (client) => client.message("DSP-1", { body: "Shipped.", actor }),
  messageReply: (client) => client.messageReply("message-1", { body: "Done.", actor } as never),
  deliveryReply: (client) => client.deliveryReply("message", "message-1", { body: "Done." }),
  getMessage: (client) => client.getMessage("DSP-1", "message-1"),
  getMessageThread: (client) => client.getMessageThread("message-1", "session-1"),
  acceptMessageDelivery: (client) => client.acceptMessageDelivery("message-1", 1, { actor }),
  artifact: (client) => client.artifact("DSP-1", file),
  getArtifact: (client) => client.getArtifact("artifact-1"),
  docRead: (client) => client.docRead("artifact-1"),
  fileVersion: (client) => client.fileVersion("artifact-1", 1),
  artifactBlocks: (client) => client.artifactBlocks("artifact-1"),
  docEdit: (client) =>
    client.docEdit("artifact-1", { ops: [{ op: "delete", find: "old" }], actor } as never),
  nameArtifactVersion: (client) =>
    client.nameArtifactVersion("artifact-1", { name: "v1", actor } as never),
  getArtifactEvents: (client) => client.getArtifactEvents("artifact-1"),
  getArtifactReferences: (client) => client.getArtifactReferences("artifact-1"),
  getAsk: (client) => client.getAsk("ask-1"),
  followAsk: (client) => client.followAsk("ask-1", "session-1", actor),
  unfollowAsk: (client) => client.unfollowAsk("ask-1", "session-1", actor),
  getComment: (client) => client.getComment("comment-1"),
  resolveComment: (client) => client.resolveComment("comment-1", actor),
  getComments: (client) => client.getComments("DSP-1"),
  getIssueReferences: (client) => client.getIssueReferences("DSP-1"),
  getReferences: (client) => client.getReferences({ to: "DSP-1" }),
};

afterEach(forgetKeyedLayerMemory);

test("every DispatchClient method reaches Dispatch through its one request function, every write keyed", async () => {
  expect(
    Object.getOwnPropertyNames(DispatchClient.prototype)
      .filter((name) => name !== "constructor")
      .sort()
  ).toEqual(Object.keys(CALLS).sort());
  const global = spyOn(globalThis, "fetch");
  const keys: string[] = [];
  try {
    for (const [name, call] of Object.entries(CALLS)) {
      const requests: { readonly method: string; readonly key: string | null }[] = [];
      const recording = (async (_url: RequestInfo | URL, init?: RequestInit) => {
        requests.push({
          method: init?.method ?? "GET",
          key: new Headers(init?.headers).get(IDEMPOTENCY_KEY_HEADER),
        });
        return Response.json({});
      }) as typeof fetch;
      const client = new DispatchClient(
        "http://dispatch.test",
        "secret",
        recording,
        undefined,
        DISPATCH_WRITE_RETRY
      );

      await call(client).catch(() => undefined);

      expect({ name, requested: requests.length > 0 }).toEqual({ name, requested: true });
      for (const { method, key } of requests) {
        if (method === "POST" || method === "PATCH") {
          expect({ name, method, key }).toEqual({
            name,
            method,
            key: expect.stringMatching(IDEMPOTENCY_KEY_PATTERN),
          });
          keys.push(key as string);
        } else {
          expect({ name, method, key }).toEqual({ name, method, key: null });
        }
      }
    }
    expect(global).not.toHaveBeenCalled();
  } finally {
    global.mockRestore();
  }
  // One key per write call: no two calls shared one, and the writes are many.
  expect(new Set(keys).size).toBe(keys.length);
  expect(keys.length).toBeGreaterThan(20);
});
