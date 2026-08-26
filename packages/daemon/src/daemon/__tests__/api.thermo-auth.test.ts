import { afterEach, describe, expect, it } from "bun:test";
import { formatIssueKey } from "@legion/contracts";
import type { CommandRunner } from "../../state/fetch";
import { type LegionApi, type LegionApiDeps, startLegionApi } from "../api";
import { type LegionState, newLegionState } from "../legion-state";

const root = formatIssueKey("acme", "widgets", 1);

type StartedSession = { secret: string };

function stateWithRoot(): LegionState {
  const state = newLegionState("omp", 2);
  state.issues[root] = {
    key: root,
    title: "Root",
    state: "open",
    children: [],
    released: true,
    labels: [],
  };
  state.trees[root] = {
    root,
    generation: 3,
    locator: { tmuxSession: "legion-omp", tmuxWindowId: "@1" },
    status: "queued",
    launchFailures: 0,
    heldEvents: [],
  };
  return state;
}

function response(command: string[]): { stdout: string; stderr: string; exitCode: number } {
  if (command.some((part) => part.endsWith("/pulls/17"))) {
    return {
      stdout: JSON.stringify({
        number: 17,
        head: { ref: "legion/issue-1", sha: "live-head" },
      }),
      stderr: "",
      exitCode: 0,
    };
  }
  if (command.some((part) => part.endsWith("/reviews"))) {
    return {
      stdout: JSON.stringify([
        { user: { login: "sami" }, state: "APPROVED", commit_id: "live-head" },
      ]),
      stderr: "",
      exitCode: 0,
    };
  }
  return { stdout: "{}", stderr: "", exitCode: 0 };
}

function startApi(
  state: LegionState,
  options: {
    runner?: CommandRunner;
    markProcessDead?: () => void;
    unresolvedAppLogins?: boolean;
  } = {}
): LegionApi {
  const deps: LegionApiDeps = {
    state,
    saveState: async () => {},
    runner: options.runner ?? (async (command) => response(command)),
    tokenManager: {
      getToken: async () => ({
        token: "test-token",
        expiresAt: "2099-01-01T00:00:00.000Z",
        gitIdentity: {
          name: "legion-implementer[bot]",
          email: "1+legion-implementer[bot]@users.noreply.github.com",
        },
      }),
    },
    processManager: {
      admit: () => "spawned",
      releaseSlot: () => {},
      registerRoleBacking: () => {},
      markProcessDead: options.markProcessDead ?? (() => {}),
      closeTree: () => {},
      markTreeReady: () => {},
      beginLinger: () => {},
    },
    envoyPublish: async () => {},
    dispatch: { url: "http://dispatch.test", bearer: "test" },
    onControllerReady: async () => {},
    onControllerEvent: async () => {},
  };
  return startLegionApi(
    {
      port: 0,
      hostname: "127.0.0.1",
      gates: { design: "root-issues", merge: "human" },
      ...(options.unresolvedAppLogins ? {} : { appLogins: ["legion-implementer[bot]"] }),
    },
    deps
  );
}

async function post(api: LegionApi, pathname: string, body: unknown): Promise<Response> {
  return await fetch(`http://127.0.0.1:${api.server.port}${pathname}`, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(body),
  });
}

async function startRoot(
  api: LegionApi
): Promise<{ sessionId: string; bootToken: string; secret: string }> {
  const sessionId = "ses_root";
  const bootToken = await api.mintBootToken(root, 3);
  const started = await post(api, "/legion/v1/process/started", {
    tree: root,
    generation: 3,
    bootToken,
    rootSessionId: sessionId,
    agentId: "root-transcript",
    ompSessionFile: "/tmp/root.jsonl",
  });
  expect(started.status).toBe(200);
  const { secret } = (await started.json()) as StartedSession;
  return { sessionId, bootToken, secret };
}

describe("thermonuclear API regressions", () => {
  let api: LegionApi | undefined;

  afterEach(() => api?.stop());

  it("refuses a human merge gate without resolved GitHub App logins", () => {
    const state = stateWithRoot();
    let unresolvedApi: LegionApi | undefined;
    let startupError: unknown;
    try {
      unresolvedApi = startApi(state, { unresolvedAppLogins: true });
    } catch (error) {
      startupError = error;
    } finally {
      unresolvedApi?.stop();
    }
    expect(startupError).toBeInstanceOf(Error);
    expect((startupError as Error).message).toBe(
      "gates.merge=human requires at least one configured GitHub App login"
    );
  });

  it("keeps role identities private and requires a daemon-issued recovery token", async () => {
    const state = stateWithRoot();
    api = startApi(state);

    const rootSession = await startRoot(api);

    expect(
      await (await fetch(`http://127.0.0.1:${api.server.port}/legion/v1/state`)).json()
    ).toEqual({
      project: "omp",
    });

    const publicIdentifierAttempt = await post(api, "/legion/v1/worker-session", {
      sessionId: rootSession.sessionId,
      agentId: "root-transcript",
    });
    expect(publicIdentifierAttempt.status).toBe(400);

    const recovered = await post(api, "/legion/v1/worker-session", {
      sessionId: rootSession.sessionId,
      recoveryToken: rootSession.bootToken,
    });
    expect(recovered.status).toBe(200);
    expect(await recovered.json()).toMatchObject({ tree: root, issue: root, role: "architect" });
  });

  it("refreshes a cached PR to its live head before evaluating human approval", async () => {
    const state = stateWithRoot();
    state.prs["acme/widgets#17"] = {
      key: root,
      repo: "acme/widgets",
      number: 17,
      headSha: "cached-head",
      checks: { required: { status: "completed", conclusion: "failure" } },
      firstRedEmitted: true,
      settledRedEmitted: true,
      greenEmitted: true,
      reviewDecision: "approved",
      lastEventAt: 1,
      fixAttempts: 1,
    };
    api = startApi(state);
    const rootSession = await startRoot(api);

    const gate = await post(api, "/legion/v1/merge-gate", {
      tree: root,
      pr: 17,
      sessionId: rootSession.sessionId,
      secret: rootSession.secret,
    });

    expect(gate.status).toBe(200);
    expect(await gate.json()).toEqual({ approved: true, pr: 17, headSha: "live-head" });
    expect(state.prs["acme/widgets#17"]).toMatchObject({
      headSha: "live-head",
      checks: {},
      firstRedEmitted: false,
      settledRedEmitted: false,
      greenEmitted: false,
      fixAttempts: 2,
    });
    expect(state.prs["acme/widgets#17"]?.reviewDecision).toBeUndefined();
  });

  it("rejects an invalid contract before process exit can mutate lifecycle state", async () => {
    const state = stateWithRoot();
    let markedDead = false;
    api = startApi(state, { markProcessDead: () => (markedDead = true) });
    const rootSession = await startRoot(api);

    const exit = await post(api, "/legion/v1/process/exit", {
      tree: root,
      generation: 3,
      sessionId: rootSession.sessionId,
      secret: rootSession.secret,
      unrecognized: true,
    });

    expect(exit.status).toBe(400);
    expect(markedDead).toBe(false);
  });
});
