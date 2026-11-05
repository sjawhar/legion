import { expect, test } from "bun:test";
import { executeDispatchTool } from "../dispatch-execute";

const interpretation = {
  block_id: "reading",
  decision_ref: "dispatch://CORE-1/spec#b-choice",
  ref: "dispatch://CORE-1/spec#b-reading",
  text: "A reading.",
  author: { kind: "session", id: "reader" },
};

async function interpret(extra: Record<string, unknown>) {
  return executeDispatchTool({
    tool: "dispatch_interpret",
    args: { ref: "dispatch://CORE-1/spec", decision: "choice", body: "A reading." },
    cwd: "/workspace",
    host: "omp",
    sessionId: "reader",
    config: { url: "http://dispatch.test", token: "secret" },
    env: {},
    fetchImpl: (async (url: RequestInfo | URL, init?: RequestInit) => {
      const pathname = new URL(String(url)).pathname;
      if (pathname === "/api/v1/issues/CORE-1") {
        return Response.json({
          key: "CORE-1",
          primary_artifact_id: "spec-id",
          artifacts: [{ id: "spec-id", slug: "spec", name: "spec.md", primary: true }],
        });
      }
      if (pathname === "/api/v1/artifacts/spec-id/interpretations") {
        expect(init?.method).toBe("POST");
        expect(JSON.parse(String(init?.body))).toMatchObject({
          decision: "choice",
          body: "A reading.",
          actor: { kind: "session", id: "reader" },
        });
        return Response.json({ ...interpretation, ...extra }, { status: 201 });
      }
      throw new Error(`unexpected request: ${pathname}`);
    }) as typeof fetch,
  });
}

test("interpret reports a lost insert and an unknown survival verdict instead of claiming success", async () => {
  const lost = await interpret({ lost_ops: [0] });
  expect(lost.text).toContain("A concurrent change removed this interpretation");
  expect(lost.text).not.toContain("Interpretation by reader:");
  expect(lost.details).toMatchObject({ lost_ops: [0] });

  const unknown = await interpret({ lost_ops: null });
  expect(unknown.text).toContain("Could not confirm this interpretation survived");
  expect(unknown.text).not.toContain("Interpretation by reader:");
  expect(unknown.details).toMatchObject({ lost_ops: null });

  for (const response of [{ lost_ops: [] }, {}]) {
    const survived = await interpret(response);
    expect(survived.text).toBe("Interpretation by reader: dispatch://CORE-1/spec#b-reading");
  }
});

test("interpret shows approval advice from the write", async () => {
  const advice = {
    issue_status: "in_progress",
    session_writes_since_human: 0,
    your_open_asks: [],
    superseded_approval: { approved_version: 1, version: 2, approved_by: "alice" },
  };
  const result = await interpret({ lost_ops: [], advice });
  expect(result.text).toContain("approval");
  expect(result.text).toContain("alice");
  expect(result.details).toMatchObject({ advice });
});
