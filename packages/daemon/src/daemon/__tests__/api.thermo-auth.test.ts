import { afterEach, describe, expect, it } from "bun:test";
import { type IssueKey, roleToken } from "@legion/contracts";
import { type LegionApi, type LegionApiDeps, startLegionApi } from "../api";
import { type LegionState, newLegionState } from "../legion-state";
import { fakeDispatchClient } from "./ci-fixtures";

const root = "WIDGETS-1" as IssueKey;

type StartedSession = { secret: string };

function stateWithRoot(): LegionState {
  const state = newLegionState("omp", 2);
  state.issues[root] = {
    key: root,
    title: "Root",
    status: "in_progress",
    children: [],
  };
  state.trees[root] = {
    root,
    generation: 3,
    locator: { runtime: "tmux", tmuxSession: "legion-omp", tmuxWindowId: "@1" },
    status: "queued",
    launchFailures: 0,
  };
  return state;
}

function startApi(
  state: LegionState,
  options: {
    markProcessDead?: () => void;
  } = {}
): LegionApi {
  const deps: LegionApiDeps = {
    state,
    saveState: async () => {},
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
    dispatchClient: fakeDispatchClient(),
    processManager: {
      admit: () => "spawned",
      releaseSlot: () => {},
      markProcessDead: options.markProcessDead ?? (() => {}),
      reportRootExit: () => {},
      closeTree: () => {},
      markTreeReady: () => {},
      confirmRootReady: () => {},
      markControllerReady: () => {},
      cancelBootWatchdog: () => {},
      spawnWorker: async () => ({ status: "spawned" as const, roleToken: "stub-role-token" }),
      workerReady: () => {},
      rejectIfTreeGone: () => {},
      mutateLiveRoleClaim: async (_tree, _issue, _token, fn) => fn(),
      beginLinger: () => {},
    },
    envoyPublish: async () => {},
    onControllerReady: async () => {},
    onControllerEvent: async () => {},
  };
  return startLegionApi(
    {
      port: 0,
      hostname: "127.0.0.1",
      repo: "acme/widgets",
      gates: { design: "root-issues" },
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

  it("exposes an established role's session on GET /legion/v1/state but never its recovery token or capability secret, and still requires the token to recover it", async () => {
    const state = stateWithRoot();
    api = startApi(state);

    const rootSession = await startRoot(api);

    const stateBody = (await (
      await fetch(`http://127.0.0.1:${api.server.port}/legion/v1/state`)
    ).json()) as Record<string, unknown>;
    // Session/role visibility is intentional (the controller needs it for triage); the boot
    // token and session capability secret are not — neither is a field this projection ever
    // names, so asserting their raw values are absent from the whole response also proves no
    // other field smuggled them in.
    expect(stateBody).toMatchObject({
      project: "omp",
      roles: {
        [roleToken(state.project, root, "architect")]: {
          role: "architect",
          issue: root,
          sessionId: rootSession.sessionId,
        },
      },
    });
    const stateText = JSON.stringify(stateBody);
    expect(stateText).not.toContain(rootSession.bootToken);
    expect(stateText).not.toContain(rootSession.secret);

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
