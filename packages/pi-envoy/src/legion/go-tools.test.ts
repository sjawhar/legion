import { expect, test } from "bun:test";
import { z } from "zod";
import type { PiApi, SessionContext } from "../pi-types";
import { createGoLegionTool } from "./go-tools";


function context(sessionId = "ses_208"): SessionContext {
  return {
    cwd: "/workspace",
    hasUI: true,
    setInterval: () => undefined,
    setTimeout: () => undefined,
    sessionManager: {
      getSessionId: () => sessionId,
      getSessionFile: () => "/sessions/208.jsonl",
      ensureOnDisk: async () => undefined,
    },
    ui: { notify: () => undefined },
  };
}

const pi = {
  zod: z,
  sendMessage: () => undefined,
  appendEntry: () => undefined,
  getActiveTools: () => [],
  setActiveTools: async () => undefined,
  on: () => undefined,
  registerTool: () => undefined,
  registerCommand: () => undefined,
  registerMessageRenderer: () => undefined,
} as unknown as PiApi;

test("the Go Legion tool exposes only the workflow operations each role owns", async () => {
  const calls: Array<readonly [string, object | undefined]> = [];
  const daemon = () =>
    Object.fromEntries(
      [
        "grant",
        "gateRegister",
        "waveRelease",
        "phaseBackward",
        "phaseRetry",
        "signOff",
        "childPark",
        "childRerun",
        "issueStatus",
      ].map((name) => [
        name,
        async (input: object) => {
          calls.push([name, input]);
          return name === "grant" ? { grantId: "grant-208" } : {};
        },
      ])
    ) as Record<string, (input?: object) => Promise<unknown>> & {
      readonly state: () => Promise<unknown>;
    };
  const state = {
    issues: { "LEGION-208": { key: "LEGION-208", phase: "held" } },
  };
  const daemonWithState = () => ({ ...daemon(), state: async () => state });
  const run = async (
    kind: "architect" | "phase-worker",
    parameters: Record<string, unknown>
  ) =>
    createGoLegionTool({
      pi,
      daemon: daemonWithState as never,
      onPhaseCompleted: () => undefined,
      resolveDocument: async (_issue: string, reference: string) => {
        throw new Error(`no document lookup expected for "${reference}"`);
      },
      session: () => ({
        kind,
        sessionId: "ses_208",
        tree: "LEGION-208",
        issue: "LEGION-208",
        secret: "claim-secret",
      }),
    }).execute("", parameters, undefined, undefined, context());

  await expect(
    run("architect", {
      op: "register_gate",
      issue: "LEGION-208",
      artifactId: "d2f1c6b4-8e07-4a53-9c1d-6b8f2e5a7093",
      version: 7,
    })
  ).resolves.toMatchObject({ details: {} });
  expect(calls).toEqual([
    [
      "grant",
      { sessionId: "ses_208", secret: "claim-secret", tree: "LEGION-208", issue: "LEGION-208" },
    ],
    [
      "gateRegister",
      {
        grantId: "grant-208",
        issue: "LEGION-208",
        artifactId: "d2f1c6b4-8e07-4a53-9c1d-6b8f2e5a7093",
        version: 7,
      },
    ],
  ]);

  await expect(
    run("phase-worker", { op: "request_backward_move", to: "implementing", reason: "test failed" })
  ).resolves.toMatchObject({ details: {} });

  await expect(
    run("architect", { op: "release_children", issues: ["LEGION-209"] })
  ).resolves.toMatchObject({ details: {} });
  expect(calls.at(-1)).toEqual([
    "waveRelease",
    { grantId: "grant-208", issues: ["LEGION-209"] },
  ]);
  await expect(
    run("architect", { op: "retry_or_escalate", issue: "LEGION-208", decision: "retry" })
  ).resolves.toMatchObject({ details: {} });
  expect(calls.at(-1)).toEqual([
    "phaseRetry",
    { grantId: "grant-208", issue: "LEGION-208", decision: "retry" },
  ]);
  await expect(run("architect", { op: "sign_off", issue: "LEGION-208" })).resolves.toMatchObject({
    details: {},
  });
  expect(calls.at(-1)).toEqual(["signOff", { grantId: "grant-208", issue: "LEGION-208" }]);
  await expect(run("architect", { op: "park_child", issue: "LEGION-209" })).resolves.toMatchObject({
    details: {},
  });
  expect(calls.at(-1)).toEqual(["childPark", { grantId: "grant-208", issue: "LEGION-209" }]);
  await expect(run("architect", { op: "rerun_child", issue: "LEGION-209" })).resolves.toMatchObject({
    details: {},
  });
  expect(calls.at(-1)).toEqual(["childRerun", { grantId: "grant-208", issue: "LEGION-209" }]);
  await expect(run("architect", { op: "read_record", issue: "LEGION-208" })).resolves.toMatchObject({
    details: { record: state.issues["LEGION-208"] },
  });
  for (const op of ["sign_off", "park_child", "rerun_child"]) {
    await expect(run("phase-worker", { op, issue: "LEGION-208" })).resolves.toMatchObject({
      isError: true,
    });
  }
  await expect(run("architect", { op: "spawn_worker", issue: "LEGION-208" })).resolves.toMatchObject({
    isError: true,
  });
});

test("a Go Legion workflow refusal tells the agent both its code and message", async () => {
  const refusal = Object.assign(
    new Error("POST /legion/v1/signoff failed with 409 SIGNOFF_EARLY: production check is incomplete"),
    { code: "SIGNOFF_EARLY", detail: "production check is incomplete" }
  );
  const tool = createGoLegionTool({
    pi,
    daemon: (() => ({
      grant: async () => ({ grantId: "grant-208" }),
      signOff: async () => Promise.reject(refusal),
    })) as never,
    onPhaseCompleted: () => undefined,
    resolveDocument: async (_issue: string, reference: string) => {
      throw new Error(`no document lookup expected for "${reference}"`);
    },
    session: () => ({
      kind: "architect",
      sessionId: "ses_208",
      tree: "LEGION-208",
      issue: "LEGION-208",
      secret: "claim-secret",
    }),
  });

  await expect(
    tool.execute("", { op: "sign_off", issue: "LEGION-208" }, undefined, undefined, context())
  ).resolves.toMatchObject({
    content: [{ type: "text", text: expect.stringContaining("SIGNOFF_EARLY") }],
    isError: true,
    details: {},
  });
});

test("register_gate takes the document reference the Dispatch tools take, and registers its id", async () => {
  const spec = "d2f1c6b4-8e07-4a53-9c1d-6b8f2e5a7093";
  const registered: unknown[] = [];
  const resolved: Array<readonly [string, string]> = [];
  const daemon = () =>
    ({
      grant: async () => ({ grantId: "grant-208" }),
      gateRegister: async (input: object) => {
        registered.push(input);
        return {};
      },
    }) as never;
  const run = (artifactId: string) =>
    createGoLegionTool({
      pi,
      daemon,
      onPhaseCompleted: () => undefined,
      resolveDocument: async (issue: string, reference: string) => {
        resolved.push([issue, reference]);
        if (reference === "spec" || reference === "spec-md" || reference === "spec.md") return spec;
        throw new Error(`No document "${reference}" on ${issue}`);
      },
      session: () => ({
        kind: "architect",
        sessionId: "ses_208",
        tree: "LEGION-208",
        issue: "LEGION-208",
        secret: "claim-secret",
      }),
    }).execute(
      "",
      { op: "register_gate", issue: "LEGION-208", artifactId, version: 2 },
      undefined,
      undefined,
      context()
    );

  for (const reference of ["spec", "spec-md", "spec.md", spec]) {
    await expect(run(reference)).resolves.toMatchObject({ details: {} });
  }
  expect(registered).toEqual(
    Array.from({ length: 4 }, () => ({
      grantId: "grant-208",
      issue: "LEGION-208",
      artifactId: spec,
      version: 2,
    }))
  );
  // A document id goes to the daemon as it is; only a slug or filename is looked up.
  expect(resolved).toEqual([
    ["LEGION-208", "spec"],
    ["LEGION-208", "spec-md"],
    ["LEGION-208", "spec.md"],
  ]);

  const refused = await run("notes");
  expect(refused.isError).toBe(true);
  expect(JSON.stringify(refused.content)).toContain('No document \\"notes\\" on LEGION-208');
  expect(registered).toHaveLength(4);
});
