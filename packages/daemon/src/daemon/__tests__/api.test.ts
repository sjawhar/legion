import { afterEach, beforeEach, describe, expect, it } from "bun:test";
import { mkdtemp, rm } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { controllerToken, formatIssueKey, type IssueKey, roleToken } from "@legion/contracts";
import type { CommandRunner } from "../../state/fetch";
import { type LegionApi, type LegionApiDeps, startLegionApi } from "../api";
import { type LegionState, loadState, newLegionState, saveState } from "../legion-state";
import { reduceGithubEvent } from "../reducers";

const root = formatIssueKey("acme", "widgets", 1);
const child = formatIssueKey("acme", "widgets", 2);
const otherRoot = formatIssueKey("acme", "other", 9);
const foreign = formatIssueKey("acme", "other", 10);

interface PhaseResponse {
  secret: string;
  gitName: string;
  gitEmail: string;
}

interface GrantResponse {
  grantId: string;
  expiresAt: string;
}
interface WorkerSessionResponse {
  tree: string;
  issue: string;
  role: string;
  secret: string;
}

describe("Legion HTTP API", () => {
  let api: LegionApi | undefined;
  let state: LegionState;
  let commands: string[][];
  let publications: Array<{ topic: string; payload: string }>;
  let tokenRoles: string[];
  let releaseSlots: IssueKey[];
  let closedTrees: IssueKey[];
  let admissions: IssueKey[];
  let backingRegistrations: Array<{
    tree: IssueKey;
    issue: IssueKey;
    role: string;
    agentId: string;
  }>;
  let now: number;
  let controllerSecret: string;

  beforeEach(() => {
    commands = [];
    publications = [];
    tokenRoles = [];
    releaseSlots = [];
    closedTrees = [];
    admissions = [];
    backingRegistrations = [];
    now = 1_700_000_000_000;
    state = newLegionState("omp", 2);
    state.issues[root] = {
      key: root,
      title: "Root",
      state: "open",
      children: [],
      released: true,
      labels: ["needs-approval"],
    };
    state.trees[root] = {
      root,
      generation: 3,
      locator: { tmuxSession: "legion-omp", tmuxWindowId: "@1" },
      status: "queued",
      launchFailures: 0,
      heldEvents: [],
    };
    state.issues[otherRoot] = {
      key: otherRoot,
      title: "Other root",
      state: "open",
      children: [foreign],
      released: true,
      labels: [],
    };
    state.issues[foreign] = {
      key: foreign,
      title: "Foreign child",
      parent: otherRoot,
      state: "open",
      children: [],
      released: true,
      labels: [],
    };
    state.trees[otherRoot] = {
      root: otherRoot,
      generation: 1,
      status: "active",
      launchFailures: 0,
      heldEvents: [],
    };
  });

  afterEach(() => api?.stop());

  async function start(options?: {
    runner?: CommandRunner;
    gates?: { design: "root-issues" | "off"; merge: "human" | "off" };
    state?: LegionState;
    saveState?: () => Promise<void>;
    mintController?: boolean;
    admissionResult?: "spawned" | "queued";
    admit?: (issue: IssueKey) => "spawned" | "queued";
    onTreeReady?: (tree: IssueKey) => Promise<void>;
    onControllerReady?: () => Promise<void>;
  }) {
    const runner =
      options?.runner ??
      ((async (command) => {
        commands.push(command);
        if (command[2] === "repos/acme/widgets/issues" && command[3] === "-f") {
          return {
            stdout: JSON.stringify({
              number: 2,
              html_url: "https://github.com/acme/widgets/issues/2",
              node_id: "I_child",
            }),
            stderr: "",
            exitCode: 0,
          };
        }
        if (command[2] === "repos/acme/widgets/issues/1") {
          return {
            stdout: JSON.stringify({ node_id: "I_parent" }),
            stderr: "",
            exitCode: 0,
          };
        }
        if (command[2] === "graphql") {
          return {
            stdout: JSON.stringify({
              data: { addSubIssue: { issue: { id: "I_child" } } },
            }),
            stderr: "",
            exitCode: 0,
          };
        }
        if (command.some((part) => part.endsWith("/comments"))) {
          return {
            stdout: JSON.stringify({
              id: 55,
              html_url: "https://github.com/acme/widgets/issues/2#issuecomment-55",
            }),
            stderr: "",
            exitCode: 0,
          };
        }
        return {
          stdout: JSON.stringify({ labels: [] }),
          stderr: "",
          exitCode: 0,
        };
      }) satisfies CommandRunner);

    const deps: LegionApiDeps = {
      state: options?.state ?? state,
      runner,
      tokenManager: {
        getToken: async (role, owner) => {
          tokenRoles.push(role);
          return {
            token: `minted-${role}-${owner}`,
            expiresAt: "2099-01-01T00:00:00.000Z",
            gitIdentity: {
              name: role === "review" ? "legion-review[bot]" : "legion-implement[bot]",
              email: `42+legion-${role}[bot]@users.noreply.github.com`,
            },
          };
        },
      },
      processManager: {
        admit: (issue) => {
          admissions.push(issue);
          return options?.admit?.(issue) ?? options?.admissionResult ?? "spawned";
        },
        releaseSlot: (issue) => {
          releaseSlots.push(issue);
        },
        registerRoleBacking: (tree, issue, role, agentId) => {
          backingRegistrations.push({ tree, issue, role, agentId });
        },
        markTreeReady: () => {},
        beginLinger: (tree) => {
          const treeState = state.trees[tree];
          if (treeState) treeState.status = "lingering";
        },
        markProcessDead: () => {},
        closeTree: (tree) => {
          releaseSlots.push(tree);
          closedTrees.push(tree);
          const treeState = state.trees[tree];
          if (treeState) treeState.status = "closed";
        },
      },
      envoyPublish: async (topic, payload) => {
        publications.push({ topic, payload });
      },
      onControllerReady: options?.onControllerReady ?? (async () => {}),
      onControllerEvent: async (payload) => {
        publications.push({
          topic: `notifications.role.${controllerToken(state.project)}`,
          payload: JSON.stringify(payload),
        });
      },
      saveState: options?.saveState ?? (async () => {}),
      onTreeReady: options?.onTreeReady,
    };
    api = startLegionApi(
      {
        port: 0,
        hostname: "127.0.0.1",
        gates: options?.gates ?? { design: "root-issues", merge: "human" },
        appLogins: ["legion-implement[bot]", "legion-review[bot]"],
        now: () => now,
      },
      deps
    );
    if (options?.mintController !== false) {
      controllerSecret = await api.mintControllerCapability();
    }
  }

  async function request(path: string, body?: unknown) {
    if (!api) throw new Error("API was not started");
    return fetch(`http://127.0.0.1:${api.server.port}${path}`, {
      method: body === undefined ? "GET" : "POST",
      headers: body === undefined ? undefined : { "content-type": "application/json" },
      body: body === undefined ? undefined : JSON.stringify(body),
    });
  }

  async function json<T = unknown>(path: string, body: unknown) {
    const response = await request(path, body);
    const responseBody = (await response.json()) as T;
    return { response, body: responseBody };
  }
  async function curl(path: string, body: unknown): Promise<{ status: number; body: string }> {
    if (!api) throw new Error("API was not started");
    const proc = Bun.spawn(
      [
        "curl",
        "--silent",
        "--show-error",
        "--write-out",
        "\n%{http_code}",
        "--request",
        "POST",
        "--header",
        "content-type: application/json",
        "--data",
        JSON.stringify(body),
        `http://127.0.0.1:${api.server.port}${path}`,
      ],
      { stdout: "pipe", stderr: "pipe" }
    );
    const [stdout, stderr, exitCode] = await Promise.all([
      new Response(proc.stdout).text(),
      new Response(proc.stderr).text(),
      proc.exited,
    ]);
    if (exitCode !== 0) throw new Error(`curl failed: ${stderr}`);
    const statusOffset = stdout.lastIndexOf("\n");
    return {
      status: Number(stdout.slice(statusOffset + 1)),
      body: stdout.slice(0, statusOffset),
    };
  }

  async function curlJson<T>(path: string, body: unknown): Promise<{ status: number; body: T }> {
    const response = await curl(path, body);
    const responseBody = JSON.parse(response.body) as T;
    return { status: response.status, body: responseBody };
  }

  it("rechecks a pre-gate approval against GitHub after its delivery was already consumed", async () => {
    state.issues[root].children.push(child);
    state.issues[child] = {
      key: child,
      title: "Child",
      parent: root,
      state: "open",
      children: [],
      released: true,
      labels: [],
    };
    state.prs["acme/widgets#17"] = {
      key: child,
      repo: "acme/widgets",
      number: 17,
      headSha: "approved-head",
      checks: { test: { status: "completed", conclusion: "success" } },
      firstRedEmitted: false,
      settledRedEmitted: false,
      greenEmitted: false,
      lastEventAt: 0,
      fixAttempts: 0,
    };
    state.roles[roleToken(state.project, child, "architect")] = {
      issue: child,
      role: "architect",
    };
    const approvalDelivery = reduceGithubEvent(
      state,
      "notifications.github.acme.widgets.pr.17.review",
      {
        event_id: "approved-during-retro",
        issued_at: 1_700_000_000_000,
        payload: {
          action: "submitted",
          repository: { full_name: "acme/widgets" },
          pull_request: { number: 17, head: { sha: "approved-head" } },
          review: {
            user: { login: "sami" },
            state: "approved",
            commit_id: "approved-head",
            body: "Approved while retro ran",
          },
        },
      },
      {
        boardProjectIds: [],
        appLogins: [],
        maxFixAttempts: 3,
      }
    );
    expect(approvalDelivery).toContainEqual({
      kind: "publish",
      role: roleToken(state.project, child, "architect"),
      payload: { type: "pr-ready", pr: 17 },
    });
    expect(state.prs["acme/widgets#17"]?.reviewDecision).toBe("approved");

    await start({
      runner: async (command) => {
        commands.push(command);
        if (command[2] === "repos/acme/widgets/pulls/17") {
          return {
            stdout: JSON.stringify({
              number: 17,
              head: { ref: "legion/issue-2", sha: "approved-head" },
            }),
            stderr: "",
            exitCode: 0,
          };
        }
        return {
          stdout: JSON.stringify([
            {
              user: { login: "sami" },
              state: "APPROVED",
              commit_id: "approved-head",
            },
          ]),
          stderr: "",
          exitCode: 0,
        };
      },
    });
    const bootToken = await api?.mintBootToken(root, 3);
    if (!bootToken) throw new Error("root boot token was not minted");
    const started = await json<{ secret: string }>("/legion/v1/process/started", {
      tree: root,
      generation: 3,
      rootSessionId: "ses_root",
      bootToken,
      agentId: "root-agent",
      ompSessionFile: "/tmp/root.json",
    });
    expect(started.response.status).toBe(200);

    const gate = await curlJson<{ approved: boolean; pr: number; headSha: string }>(
      "/legion/v1/merge-gate",
      {
        tree: root,
        pr: 17,
        sessionId: "ses_root",
        secret: started.body.secret,
      }
    );

    expect(gate).toEqual({
      status: 200,
      body: { approved: true, pr: 17, headSha: "approved-head" },
    });
    expect(commands).toEqual([
      ["gh", "api", "repos/acme/widgets/pulls/17"],
      ["gh", "api", "repos/acme/widgets/pulls/17/reviews"],
    ]);
  });
  it("reconstructs a missed Legion PR registration from its durable branch for a merge gate", async () => {
    await start({
      runner: async (command) => {
        commands.push(command);
        if (command[2] === "repos/acme/widgets/pulls/17") {
          return {
            stdout: JSON.stringify({
              number: 17,
              body: "Closes #1",
              head: { ref: "legion/issue-1", sha: "missed-head" },
            }),
            stderr: "",
            exitCode: 0,
          };
        }
        if (command[2] === "repos/acme/widgets/pulls/17/reviews") {
          return {
            stdout: JSON.stringify([
              {
                user: { login: "sami" },
                state: "APPROVED",
                commit_id: "missed-head",
              },
            ]),
            stderr: "",
            exitCode: 0,
          };
        }
        throw new Error(`Unexpected GitHub command: ${command.join(" ")}`);
      },
    });
    const bootToken = await api?.mintBootToken(root, 3);
    if (!bootToken) throw new Error("root boot token was not minted");
    const started = await json<{ secret: string }>("/legion/v1/process/started", {
      tree: root,
      generation: 3,
      rootSessionId: "ses_root",
      bootToken,
      agentId: "root-agent",
      ompSessionFile: "/tmp/root.json",
    });
    expect(started.response.status).toBe(200);

    const gate = await curlJson<{ approved: boolean; pr: number; headSha: string }>(
      "/legion/v1/merge-gate",
      {
        tree: root,
        pr: 17,
        sessionId: "ses_root",
        secret: started.body.secret,
      }
    );

    expect(gate).toEqual({
      status: 200,
      body: { approved: true, pr: 17, headSha: "missed-head" },
    });
    expect(state.prs["acme/widgets#17"]).toMatchObject({
      key: root,
      repo: "acme/widgets",
      number: 17,
      headSha: "missed-head",
    });
    expect(state.prByBranch["acme/widgets@legion/issue-1"]).toBe("acme/widgets#17");
    expect(commands).toEqual([
      ["gh", "api", "repos/acme/widgets/pulls/17"],
      ["gh", "api", "repos/acme/widgets/pulls/17/reviews"],
    ]);
  });
  it("reconstructs a missed merge-gate registration from the PR closing issue", async () => {
    await start({
      runner: async (command) => {
        commands.push(command);
        if (command[2] === "repos/acme/widgets/pulls/19") {
          return {
            stdout: JSON.stringify({
              number: 19,
              body: "Closes #1",
              head: { ref: "feature/recovered", sha: "body-head" },
            }),
            stderr: "",
            exitCode: 0,
          };
        }
        if (command[2] === "repos/acme/widgets/pulls/19/reviews") {
          return {
            stdout: JSON.stringify([
              {
                user: { login: "sami" },
                state: "APPROVED",
                commit_id: "body-head",
              },
            ]),
            stderr: "",
            exitCode: 0,
          };
        }
        throw new Error(`Unexpected GitHub command: ${command.join(" ")}`);
      },
    });
    const bootToken = await api?.mintBootToken(root, 3);
    if (!bootToken) throw new Error("root boot token was not minted");
    const started = await json<{ secret: string }>("/legion/v1/process/started", {
      tree: root,
      generation: 3,
      rootSessionId: "ses_root",
      bootToken,
      agentId: "root-agent",
      ompSessionFile: "/tmp/root.json",
    });
    expect(started.response.status).toBe(200);

    const gate = await curlJson<{ approved: boolean; pr: number; headSha: string }>(
      "/legion/v1/merge-gate",
      {
        tree: root,
        pr: 19,
        sessionId: "ses_root",
        secret: started.body.secret,
      }
    );

    expect(gate).toEqual({
      status: 200,
      body: { approved: true, pr: 19, headSha: "body-head" },
    });
    expect(state.prs["acme/widgets#19"]?.key).toBe(root);
    expect(commands).toEqual([
      ["gh", "api", "repos/acme/widgets/pulls/19"],
      ["gh", "api", "repos/acme/widgets/pulls/19/reviews"],
    ]);
  });

  it("rejects an unrelated pull request after checking durable merge-gate linkage", async () => {
    await start({
      runner: async (command) => {
        commands.push(command);
        if (command[2] === "repos/acme/widgets/pulls/18") {
          return {
            stdout: JSON.stringify({
              number: 18,
              body: "An unrelated pull request",
              head: { ref: "feature/unrelated", sha: "unrelated-head" },
            }),
            stderr: "",
            exitCode: 0,
          };
        }
        throw new Error(`Unexpected GitHub command: ${command.join(" ")}`);
      },
    });
    const bootToken = await api?.mintBootToken(root, 3);
    if (!bootToken) throw new Error("root boot token was not minted");
    const started = await json<{ secret: string }>("/legion/v1/process/started", {
      tree: root,
      generation: 3,
      rootSessionId: "ses_root",
      bootToken,
      agentId: "root-agent",
      ompSessionFile: "/tmp/root.json",
    });
    expect(started.response.status).toBe(200);

    const gate = await curlJson<{ error: string }>("/legion/v1/merge-gate", {
      tree: root,
      pr: 18,
      sessionId: "ses_root",
      secret: started.body.secret,
    });

    expect(gate).toEqual({
      status: 404,
      body: { error: "No PR #18 belongs to tree acme/widgets#1" },
    });
    expect(state.prs["acme/widgets#18"]).toBeUndefined();
    expect(commands).toEqual([["gh", "api", "repos/acme/widgets/pulls/18"]]);
  });

  it("drains each held event exactly once when a child wave releases", async () => {
    await start();
    const bootToken = await api?.mintBootToken(root, 3);
    if (!bootToken) throw new Error("boot nonce was not minted");

    const started = await json("/legion/v1/process/started", {
      tree: root,
      generation: 3,
      rootSessionId: "ses_root",
      bootToken,
      agentId: "root-agent",
      ompSessionFile: "/tmp/root.json",
    });
    expect(started.response.status).toBe(200);
    expect(started.body).toEqual({
      roleTokens: {
        architect: "legion-omp-acme__widgets-1-architect",
        planner: "legion-omp-acme__widgets-1-planner",
        implementer: "legion-omp-acme__widgets-1-implementer",
        tester: "legion-omp-acme__widgets-1-tester",
        reviewer: "legion-omp-acme__widgets-1-reviewer",
        merger: "legion-omp-acme__widgets-1-merger",
      },
      controlSubject: "legion.ctl.acme-widgets-1.3",
      gates: { design: "root-issues", merge: "human" },
      secret: expect.any(String),
    });
    if (
      typeof started.body !== "object" ||
      started.body === null ||
      !("secret" in started.body) ||
      typeof started.body.secret !== "string"
    ) {
      throw new Error("root start response is missing its architect capability");
    }
    const architect = { sessionId: "ses_root", secret: started.body.secret };
    expect(state.trees[root]).toMatchObject({
      status: "active",
      locator: { ompSessionFile: "/tmp/root.json" },
    });

    const created = await json("/legion/v1/issues", {
      tree: root,
      title: "Child",
      body: "Build it",
      labels: ["needs-approval"],
      ...architect,
    });
    expect(created.response.status).toBe(200);
    expect(created.body).toEqual({
      issue: child,
      url: "https://github.com/acme/widgets/issues/2",
    });
    expect(state.issues[child]).toEqual({
      key: child,
      title: "Child",
      parent: root,
      state: "open",
      children: [],
      released: false,
      labels: ["needs-approval", "legion-child"],
    });
    expect(state.issues[root]?.children).toEqual([child]);
    expect(commands[0]).toEqual([
      "gh",
      "api",
      "repos/acme/widgets/issues",
      "-f",
      "title=Child",
      "-f",
      "body=Build it",
      "-f",
      "labels[]=needs-approval",
      "-f",
      "labels[]=legion-child",
    ]);
    expect(commands[2]?.join(" ")).toContain("addSubIssue");

    state.trees[root]?.heldEvents.push({
      role: "legion-omp-acme__widgets-2-implementer",
      payloadJson: '{"type":"work"}',
      heldAt: "2026-08-23T00:00:00.000Z",
      eventId: "evt-1",
    });
    const released = await json("/legion/v1/waves/release", {
      tree: root,
      children: [child],
      ...architect,
    });
    expect(released.body).toEqual({ released: [child] });
    expect(state.issues[child]?.released).toBe(true);
    expect(publications).toEqual([
      {
        topic: "notifications.role.legion-omp-acme__widgets-2-implementer",
        payload: '{"type":"work"}',
      },
    ]);
    expect(state.trees[root]?.heldEvents).toEqual([]);
    const releasedAgain = await json("/legion/v1/waves/release", {
      tree: root,
      children: [child],
      ...architect,
    });
    expect(releasedAgain.body).toEqual({ released: [child] });
    expect(publications).toHaveLength(1);

    const rootIssue = state.issues[root];
    if (!rootIssue) throw new Error("Root issue is missing from test state");
    rootIssue.state = "closed";
    const unauthenticatedExit = await json("/legion/v1/process/exit", {
      tree: root,
      generation: 3,
    });
    expect(unauthenticatedExit.response.status).toBe(400);
    expect(releaseSlots).toEqual([]);
    const exited = await json("/legion/v1/process/exit", {
      tree: root,
      generation: 3,
      ...architect,
    });
    expect(exited.response.status).toBe(200);
    expect(releaseSlots).toEqual([root]);
    expect(state.trees[root]?.status).toBe("closed");
    expect(closedTrees).toEqual([root]);
  });
  it("requires a daemon-minted single-use boot nonce before root registration", async () => {
    await start();
    const input = {
      tree: root,
      generation: 3,
      rootSessionId: "ses_root",
      agentId: "root-agent",
      ompSessionFile: "/tmp/root.json",
    };

    expect((await json("/legion/v1/process/started", input)).response.status).toBe(400);
    const bootToken = await api?.mintBootToken(root, 3);
    if (!bootToken) throw new Error("boot nonce was not minted");
    expect(
      (
        await json("/legion/v1/process/started", {
          ...input,
          bootToken: "wrong-boot-token",
        })
      ).response.status
    ).toBe(403);
    expect(
      (
        await json("/legion/v1/process/started", {
          ...input,
          bootToken,
        })
      ).response.status
    ).toBe(200);
    expect(
      (
        await json("/legion/v1/process/started", {
          ...input,
          bootToken,
        })
      ).response.status
    ).toBe(403);
  });
  it("emits root catch-up only after the architect has confirmed readiness", async () => {
    const treeReady: IssueKey[] = [];
    await start({
      onTreeReady: async (tree) => {
        treeReady.push(tree);
      },
    });
    const bootToken = await api?.mintBootToken(root, 3);
    if (!bootToken) throw new Error("boot nonce was not minted");
    const started = await json<{ secret: string }>("/legion/v1/process/started", {
      tree: root,
      generation: 3,
      rootSessionId: "ses_root",
      bootToken,
      agentId: "root-agent",
      ompSessionFile: "/tmp/root.json",
    });

    expect(treeReady).toEqual([]);
    expect(
      (
        await json("/legion/v1/process/ready", {
          tree: root,
          sessionId: "ses_root",
          secret: started.body.secret,
        })
      ).response.status
    ).toBe(200);
    expect(treeReady).toEqual([root]);
  });

  it("writes only inside the caller tree and implements comments, bodies, labels, close, escalation, gates, admission, backlog, and redacted state", async () => {
    await start();
    const bootToken = await api?.mintBootToken(root, 3);
    if (!bootToken) throw new Error("boot nonce was not minted");
    const started = await json<{ secret: string }>("/legion/v1/process/started", {
      tree: root,
      generation: 3,
      rootSessionId: "ses_root",
      bootToken,
      agentId: "root-agent",
      ompSessionFile: "/tmp/root.json",
    });
    expect(started.response.status).toBe(200);
    const architect = { sessionId: "ses_root", secret: started.body.secret };
    const rootIssue = state.issues[root];
    if (!rootIssue) throw new Error("Root issue is missing from test state");
    rootIssue.children.push(child);
    state.issues[child] = {
      key: child,
      title: "Child",
      parent: root,
      state: "open",
      children: [],
      released: true,
      labels: ["legion-child"],
    };

    const outOfTree = await json("/legion/v1/issues/body", {
      tree: root,
      issue: foreign,
      body: "nope",
      ...architect,
    });
    expect(outOfTree.response.status).toBe(403);

    const comments = await json("/legion/v1/issues/comment", {
      tree: root,
      issue: child,
      body: "Please fix",
      ...architect,
    });
    expect(comments.body).toEqual({
      commentId: 55,
      url: "https://github.com/acme/widgets/issues/2#issuecomment-55",
    });
    expect(commands.at(-1)).toEqual([
      "gh",
      "api",
      "repos/acme/widgets/issues/2/comments",
      "-f",
      'body=Please fix\n\n<!-- legion: {"session":"ses_root","issue":"acme/widgets#2"} -->',
    ]);

    expect(
      (
        await json("/legion/v1/issues/body", {
          tree: root,
          issue: child,
          body: "# Spec",
          ...architect,
        })
      ).response.status
    ).toBe(200);
    expect(commands.at(-1)).toEqual([
      "gh",
      "api",
      "-X",
      "PATCH",
      "repos/acme/widgets/issues/2",
      "-f",
      "body=# Spec",
    ]);

    const rejectedLabel = await json("/legion/v1/issues/labels", {
      tree: root,
      issue: child,
      add: ["unknown-label"],
      ...architect,
    });
    expect(rejectedLabel.response.status).toBe(400);

    const controllerLabel = await json("/legion/v1/issues/labels", {
      tree: root,
      issue: child,
      add: ["human-approved"],
      remove: ["legion-child"],
      ...architect,
    });
    expect(controllerLabel.response.status).toBe(400);

    const labels = await json("/legion/v1/issues/labels", {
      tree: root,
      issue: child,
      add: ["needs-approval"],
      ...architect,
    });
    expect(labels.body).toEqual({ labels: ["legion-child", "needs-approval"] });
    expect(state.issues[child]?.labels).toEqual(["legion-child", "needs-approval"]);

    expect(
      (
        await json("/legion/v1/escalate", {
          tree: root,
          kind: "capacity",
          context: { blocked: true },
          ...architect,
        })
      ).response.status
    ).toBe(200);
    expect(publications.at(-1)).toEqual({
      topic: "notifications.role.legion-omp-controller",
      payload: JSON.stringify({
        type: "escalate",
        tree: root,
        kind: "capacity",
        context: { blocked: true },
      }),
    });

    const spawn = await json<{ spawnToken: string }>("/legion/v1/spawn-token", {
      tree: root,
      issue: child,
      role: "implementer",
      ...architect,
    });
    expect(
      (
        await json("/legion/v1/role-backing", {
          tree: root,
          issue: child,
          role: "implementer",
          agentId: "agent-17",
          sessionId: "ses_implementer",
          spawnToken: spawn.body.spawnToken,
        })
      ).response.status
    ).toBe(200);
    expect(backingRegistrations).toEqual([
      { tree: root, issue: child, role: "implementer", agentId: "agent-17" },
    ]);

    const provisioningCredential = await json<{ token: string }>(
      "/legion/v1/provisioning-credential",
      {
        tree: root,
        issue: child,
        ...architect,
      }
    );
    expect(provisioningCredential.response.status).toBe(200);
    expect(provisioningCredential.body).toEqual({ token: "minted-implement-acme" });

    const unauthenticatedGate = await json("/legion/v1/gates/approve", {
      issue: root,
    });
    expect(unauthenticatedGate.response.status).toBe(400);
    const unauthenticatedAdmission = await json("/legion/v1/admission", {
      issue: root,
    });
    expect(unauthenticatedAdmission.response.status).toBe(400);
    const unauthenticatedBacklog = await json("/legion/v1/backlog", {
      issue: root,
      marker: "needs design",
    });
    expect(unauthenticatedBacklog.response.status).toBe(400);
    const unauthenticatedReady = await json("/legion/v1/controller/ready", {});
    expect(unauthenticatedReady.response.status).toBe(400);
    const missingSessionReady = await json("/legion/v1/controller/ready", {
      secret: controllerSecret,
    });
    expect(missingSessionReady.response.status).toBe(400);

    expect(
      (
        await json("/legion/v1/gates/approve", {
          issue: root,
          secret: controllerSecret,
        })
      ).response.status
    ).toBe(200);
    expect(state.issues[root]?.labels).toEqual(["human-approved"]);

    expect(
      (
        await json("/legion/v1/admission", {
          issue: root,
          secret: controllerSecret,
        })
      ).body
    ).toEqual({ result: "spawned" });
    expect(admissions).toEqual([root]);

    expect(
      (
        await json("/legion/v1/backlog", {
          issue: root,
          marker: "needs design",
          secret: controllerSecret,
        })
      ).response.status
    ).toBe(200);
    expect(state.issues[root]).toMatchObject({
      labels: ["human-approved", "legion-backlog"],
      backlogMarker: "needs design",
    });
    expect(
      (
        await json("/legion/v1/controller/ready", {
          secret: controllerSecret,
          sessionId: "ses_controller",
        })
      ).response.status
    ).toBe(200);
    expect(state.roles[controllerToken(state.project)]).toEqual({
      role: "controller",
      sessionId: "ses_controller",
    });
    expect(
      (
        await json("/legion/v1/controller/ready", {
          secret: controllerSecret,
          sessionId: "ses_controller",
        })
      ).response.status
    ).toBe(200);

    const closed = await json("/legion/v1/issues/close", {
      tree: root,
      issue: child,
      comment: "Closing",
      ...architect,
    });
    expect(closed.response.status).toBe(200);
    expect(state.issues[child]?.state).toBe("closed");

    const stateResponse = await request("/legion/v1/state");
    const stateJson = await stateResponse.text();
    expect(stateJson).not.toContain("minted-");
    expect(stateJson).not.toContain("controllerCapabilityHash");
  });

  it("retries controller startup redelivery after a failed ready callback", async () => {
    let readyCalls = 0;
    await start({
      onControllerReady: async () => {
        readyCalls += 1;
        if (readyCalls === 1) throw new Error("redelivery failed");
      },
    });

    const first = await json("/legion/v1/controller/ready", {
      secret: controllerSecret,
      sessionId: "ses_controller",
    });
    expect(first.response.status).toBe(500);
    expect(first.body).toEqual({ error: "redelivery failed" });

    const second = await json("/legion/v1/controller/ready", {
      secret: controllerSecret,
      sessionId: "ses_controller",
    });
    expect(second.response.status).toBe(200);
    expect(readyCalls).toBe(2);
  });

  it("requires the owning architect capability for every architect lifecycle write", async () => {
    await start();
    const rootIssue = state.issues[root];
    const otherTree = state.trees[otherRoot];
    if (!rootIssue || !otherTree) throw new Error("test state is missing a root issue");
    rootIssue.children.push(child);
    state.issues[child] = {
      key: child,
      title: "Child",
      parent: root,
      state: "open",
      children: [],
      released: true,
      labels: ["legion-child"],
    };
    otherTree.locator = { tmuxSession: "legion-omp", tmuxWindowId: "@9" };

    const rootBootToken = await api?.mintBootToken(root, 3);
    const otherBootToken = await api?.mintBootToken(otherRoot, 1);
    if (!rootBootToken || !otherBootToken) throw new Error("root boot nonces were not minted");
    const rootStarted = await json<{ secret: string }>("/legion/v1/process/started", {
      tree: root,
      generation: 3,
      rootSessionId: "ses_root_architect",
      bootToken: rootBootToken,
      agentId: "root-agent",
      ompSessionFile: "/tmp/root.json",
    });
    const otherStarted = await json<{ secret: string }>("/legion/v1/process/started", {
      tree: otherRoot,
      generation: 1,
      rootSessionId: "ses_other_architect",
      bootToken: otherBootToken,
      agentId: "other-root-agent",
      ompSessionFile: "/tmp/other.json",
    });
    expect(rootStarted.response.status).toBe(200);
    expect(otherStarted.response.status).toBe(200);

    const workerSpawn = await json<{ spawnToken: string }>("/legion/v1/spawn-token", {
      tree: root,
      issue: root,
      role: "tester",
      sessionId: "ses_root_architect",
      secret: rootStarted.body.secret,
    });
    expect(workerSpawn.response.status).toBe(200);
    expect(
      (
        await json("/legion/v1/role-backing", {
          tree: root,
          issue: root,
          role: "tester",
          agentId: "agent-tester",
          sessionId: "ses_tester",
          spawnToken: workerSpawn.body.spawnToken,
        })
      ).response.status
    ).toBe(200);
    const workerPhase = await json<PhaseResponse>("/legion/v1/phase", {
      tree: root,
      issue: root,
      phase: "tester",
      sessionId: "ses_tester",
      spawnToken: workerSpawn.body.spawnToken,
    });
    expect(workerPhase.response.status).toBe(200);
    expect(state.roles[roleToken(state.project, root, "tester")]).toEqual({
      issue: root,
      role: "tester",
      sessionId: "ses_tester",
      agentId: "agent-tester",
    });

    const lifecycleWrites: Array<{
      path: string;
      body: Record<string, unknown>;
    }> = [
      {
        path: "/legion/v1/issues",
        body: {
          tree: root,
          title: "Child",
          body: "Build it",
          labels: ["needs-approval"],
        },
      },
      {
        path: "/legion/v1/waves/release",
        body: { tree: root, children: [child] },
      },
      {
        path: "/legion/v1/issues/comment",
        body: { tree: root, issue: child, body: "Please fix" },
      },
      {
        path: "/legion/v1/issues/body",
        body: { tree: root, issue: child, body: "# Spec" },
      },
      {
        path: "/legion/v1/issues/labels",
        body: { tree: root, issue: child, add: ["needs-approval"] },
      },
      { path: "/legion/v1/issues/close", body: { tree: root, issue: child } },
      {
        path: "/legion/v1/escalate",
        body: { tree: root, kind: "capacity", context: { blocked: true } },
      },
      {
        path: "/legion/v1/spawn-token",
        body: { tree: root, issue: child, role: "implementer" },
      },
    ];

    for (const write of lifecycleWrites) {
      expect((await json(write.path, write.body)).response.status).toBe(400);
      expect(
        (
          await json(write.path, {
            ...write.body,
            sessionId: "ses_root_architect",
            secret: "wrong-secret",
          })
        ).response.status
      ).toBe(403);
      expect(
        (
          await json(write.path, {
            ...write.body,
            sessionId: "ses_tester",
            secret: workerPhase.body.secret,
          })
        ).response.status
      ).toBe(403);
      expect(
        (
          await json(write.path, {
            ...write.body,
            sessionId: "ses_other_architect",
            secret: otherStarted.body.secret,
          })
        ).response.status
      ).toBe(403);
      expect(
        (
          await json(write.path, {
            ...write.body,
            sessionId: "ses_root_architect",
            secret: rootStarted.body.secret,
          })
        ).response.status
      ).toBe(200);
    }
  });

  it("preserves a worker spawn capability across daemon restart so its backed session can register phase", async () => {
    const tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-spawn-capability-"));
    const file = path.join(tempDir, "state.json");
    try {
      await start({ saveState: async () => saveState(file, state) });
      const bootToken = await api?.mintBootToken(root, 3);
      if (!bootToken) throw new Error("boot nonce was not minted");
      const started = await json<{ secret: string }>("/legion/v1/process/started", {
        tree: root,
        generation: 3,
        rootSessionId: "ses_architect",
        bootToken,
        agentId: "root-agent",
        ompSessionFile: "/tmp/root.json",
      });
      expect(started.response.status).toBe(200);
      const spawn = await json<{ spawnToken: string }>("/legion/v1/spawn-token", {
        tree: root,
        issue: root,
        role: "reviewer",
        sessionId: "ses_architect",
        secret: started.body.secret,
      });
      expect(
        (
          await json("/legion/v1/role-backing", {
            tree: root,
            issue: root,
            role: "reviewer",
            agentId: "agent-reviewer",
            sessionId: "ses_recreated",
            spawnToken: spawn.body.spawnToken,
          })
        ).response.status
      ).toBe(200);
      api?.stop();

      const reloaded = await loadState(file, { project: "omp", cap: 2 });
      await start({ state: reloaded, mintController: false });
      const phase = await json("/legion/v1/phase", {
        tree: root,
        issue: root,
        phase: "reviewer",
        sessionId: "ses_recreated",
        spawnToken: spawn.body.spawnToken,
      });

      expect(phase.response.status).toBe(200);
    } finally {
      await rm(tempDir, { recursive: true, force: true });
    }
  });
  it("surfaces a deferred admission retry as queued through the controller API", async () => {
    await start({ admissionResult: "queued" });

    const admission = await json("/legion/v1/admission", {
      issue: root,
      secret: controllerSecret,
    });
    expect(admission.response.status).toBe(200);
    expect(admission.body).toEqual({ result: "queued" });
    expect(admissions).toEqual([root]);
  });

  it("starts each authenticated re-admission attempt until the third launch failure", async () => {
    const manager = {
      active: [root] as IssueKey[],
      queue: [] as IssueKey[],
      status: "active" as "active" | "queued" | "launch-failed",
      launchFailures: 0,
      anomaly: undefined as
        | { type: "launch-failed"; issue: IssueKey; failures: number }
        | undefined,
      attempts: 0,
      admit(issue: IssueKey): "spawned" | "queued" {
        this.attempts += 1;
        if (this.status === "launch-failed") {
          this.status = "active";
          this.launchFailures = 0;
          this.active = [issue];
          return "spawned";
        }
        return this.attempts === 1 ? "spawned" : "queued";
      },
      failCurrentAttempt(): void {
        this.launchFailures += 1;
        this.active = [];
        if (this.launchFailures < 3) {
          this.status = "queued";
          this.queue = [root];
          return;
        }
        this.status = "launch-failed";
        this.queue = [];
        this.anomaly = { type: "launch-failed", issue: root, failures: 3 };
      },
    };
    await start({ admit: (issue) => manager.admit(issue) });

    for (const result of ["spawned", "queued", "queued"] as const) {
      const admission = await json("/legion/v1/admission", {
        issue: root,
        secret: controllerSecret,
      });
      expect(admission.body).toEqual({ result });
      manager.failCurrentAttempt();
    }

    expect(manager.attempts).toBe(3);
    expect(manager).toMatchObject({
      active: [],
      queue: [],
      status: "launch-failed",
      launchFailures: 3,
      anomaly: { type: "launch-failed", issue: root, failures: 3 },
    });
    expect(admissions).toEqual([root, root, root]);
  });

  it("persists a minted controller capability before controller spawn can proceed", async () => {
    const tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-controller-capability-"));
    const file = path.join(tempDir, "state.json");
    try {
      await start({ saveState: async () => saveState(file, state) });
      const mintedSecret = controllerSecret;
      api?.stop();

      const reloaded = await loadState(file, { project: "omp", cap: 2 });
      await start({ state: reloaded, mintController: false });
      expect(
        (
          await json("/legion/v1/gates/approve", {
            issue: root,
            secret: mintedSecret,
          })
        ).response.status
      ).toBe(200);
    } finally {
      await rm(tempDir, { recursive: true, force: true });
    }
  });

  it("rejects a phase whose spawn token was minted for another role", async () => {
    await start();
    const bootToken = await api?.mintBootToken(root, 3);
    if (!bootToken) throw new Error("boot nonce was not minted");
    const started = await json<{ secret: string }>("/legion/v1/process/started", {
      tree: root,
      generation: 3,
      rootSessionId: "ses_architect",
      bootToken,
      agentId: "root-agent",
      ompSessionFile: "/tmp/root.json",
    });
    expect(started.response.status).toBe(200);
    const testerSpawn = await json<{ spawnToken: string }>("/legion/v1/spawn-token", {
      tree: root,
      issue: root,
      role: "tester",
      sessionId: "ses_architect",
      secret: started.body.secret,
    });
    const reviewerSpawn = await json<{ spawnToken: string }>("/legion/v1/spawn-token", {
      tree: root,
      issue: root,
      role: "reviewer",
      sessionId: "ses_architect",
      secret: started.body.secret,
    });
    expect(
      (
        await json("/legion/v1/role-backing", {
          tree: root,
          issue: root,
          role: "reviewer",
          agentId: "agent-reviewer",
          sessionId: "ses_reviewer",
          spawnToken: reviewerSpawn.body.spawnToken,
        })
      ).response.status
    ).toBe(200);

    const replay = await json("/legion/v1/phase", {
      tree: root,
      issue: root,
      phase: "reviewer",
      sessionId: "ses_reviewer",
      spawnToken: testerSpawn.body.spawnToken,
    });

    expect(replay.response.status).toBe(403);
  });

  it("reissues a daemon-registered worker session capability and keeps grants short-lived", async () => {
    await start();
    const bootToken = await api?.mintBootToken(root, 3);
    if (!bootToken) throw new Error("boot nonce was not minted");
    const started = await json<{ secret: string }>("/legion/v1/process/started", {
      tree: root,
      generation: 3,
      rootSessionId: "ses_architect",
      bootToken,
      agentId: "root-agent",
      ompSessionFile: "/tmp/root.json",
    });
    expect(started.response.status).toBe(200);
    const spawn = await json<{ spawnToken: string }>("/legion/v1/spawn-token", {
      tree: root,
      issue: root,
      role: "tester",
      sessionId: "ses_architect",
      secret: started.body.secret,
    });
    const unregisteredSession = await json("/legion/v1/phase", {
      tree: root,
      issue: root,
      phase: "tester",
      sessionId: "ses_unregistered",
      spawnToken: spawn.body.spawnToken,
    });
    expect(unregisteredSession.response.status).toBe(403);
    expect(
      (
        await json("/legion/v1/role-backing", {
          tree: root,
          issue: root,
          role: "tester",
          agentId: "agent-tester",
          sessionId: "ses_tester",
          spawnToken: spawn.body.spawnToken,
        })
      ).response.status
    ).toBe(200);
    const roleBypass = await json("/legion/v1/phase", {
      tree: root,
      issue: root,
      phase: "reviewer",
      sessionId: "ses_tester",
      spawnToken: spawn.body.spawnToken,
    });
    expect(roleBypass.response.status).toBe(403);

    const phase = await curlJson<PhaseResponse>("/legion/v1/phase", {
      tree: root,
      issue: root,
      phase: "tester",
      sessionId: "ses_tester",
      spawnToken: spawn.body.spawnToken,
    });
    expect(phase.status).toBe(200);
    expect(phase.body).toEqual({
      secret: expect.any(String),
      gitName: "legion-implement[bot]",
      gitEmail: "42+legion-implement[bot]@users.noreply.github.com",
    });

    const recovered = await curlJson<WorkerSessionResponse>("/legion/v1/worker-session", {
      sessionId: "ses_tester",
      recoveryToken: spawn.body.spawnToken,
    });
    expect(recovered.status).toBe(200);
    expect(recovered.body).toEqual({
      tree: root,
      issue: root,
      role: "tester",
      secret: expect.any(String),
    });
    expect(
      (
        await json("/legion/v1/worker-session", {
          sessionId: "ses_tester",
          recoveryToken: "not-a-daemon-issued-token",
        })
      ).response.status
    ).toBe(403);

    for (const secret of [undefined, "wrong-secret"]) {
      const denied = await json("/legion/v1/grants", {
        tree: root,
        issue: root,
        sessionId: "ses_tester",
        ...(secret === undefined ? {} : { secret }),
      });
      expect(denied.response.status).toBe(secret === undefined ? 400 : 403);
    }

    const grant = await curlJson<GrantResponse>("/legion/v1/grants", {
      tree: root,
      issue: root,
      sessionId: "ses_tester",
      secret: recovered.body.secret,
    });
    expect(grant.status).toBe(200);
    expect(grant.body).toEqual({
      grantId: expect.any(String),
      expiresAt: new Date(now + 60_000).toISOString(),
    });

    const token = await json("/legion/v1/gh-token", {
      grantId: grant.body.grantId,
    });
    expect(token.body).toEqual({
      token: "minted-implement-acme",
      appLogin: "legion-implement[bot]",
    });
    expect(tokenRoles).toEqual(["implement", "implement"]);

    const credential = await curl("/legion/v1/git-credential", {
      grantId: grant.body.grantId,
    });
    expect(credential.status).toBe(200);
    expect(credential.body).toBe("username=x-access-token\npassword=minted-implement-acme");

    now += 60_001;
    const expired = await json("/legion/v1/gh-token", {
      grantId: grant.body.grantId,
    });
    expect(expired.response.status).toBe(403);
  });
  it("restores a root architect capability from durable transcript backing after a daemon restart", async () => {
    await start();
    const bootToken = await api?.mintBootToken(root, 3);
    if (!bootToken) throw new Error("boot nonce was not minted");
    const started = await json<{ secret: string }>("/legion/v1/process/started", {
      tree: root,
      generation: 3,
      rootSessionId: "ses_root",
      agentId: "root-transcript",
      bootToken,
      ompSessionFile: "/tmp/root-transcript.jsonl",
    });
    expect(started.response.status).toBe(200);

    api?.stop();
    await start({ state });
    const stale = await json("/legion/v1/process/ready", {
      tree: root,
      sessionId: "ses_root",
      secret: started.body.secret,
    });
    expect(stale.response.status).toBe(403);
    const recovered = await json<WorkerSessionResponse>("/legion/v1/worker-session", {
      sessionId: "ses_root",
      recoveryToken: bootToken,
    });
    expect(recovered.response.status).toBe(200);
    expect(recovered.body).toEqual({
      tree: root,
      issue: root,
      role: "architect",
      secret: expect.any(String),
    });
    expect(
      (
        await json("/legion/v1/process/ready", {
          tree: root,
          sessionId: "ses_root",
          secret: recovered.body.secret,
        })
      ).response.status
    ).toBe(200);
  });
  it("attributes a daemon-initiated root close and begins linger without waiting for GitHub", async () => {
    await start({
      runner: async (command) => {
        commands.push(command);
        if (command.some((part) => part.endsWith("/comments"))) {
          return {
            stdout: JSON.stringify({
              id: 99,
              html_url: "https://github.com/acme/widgets/issues/1#issuecomment-99",
            }),
            stderr: "",
            exitCode: 0,
          };
        }
        return { stdout: "{}", stderr: "", exitCode: 0 };
      },
    });
    const bootToken = await api?.mintBootToken(root, 3);
    if (!bootToken) throw new Error("boot nonce was not minted");
    const started = await json<{ secret: string }>("/legion/v1/process/started", {
      tree: root,
      generation: 3,
      rootSessionId: "ses_root",
      bootToken,
      agentId: "root-agent",
      ompSessionFile: "/tmp/root.json",
    });

    expect(
      (
        await json("/legion/v1/issues/close", {
          tree: root,
          issue: root,
          sessionId: "ses_root",
          secret: started.body.secret,
          comment: "Completed",
        })
      ).response.status
    ).toBe(200);
    expect(commands).toEqual([
      [
        "gh",
        "api",
        "repos/acme/widgets/issues/1/comments",
        "-f",
        'body=Completed\n\n<!-- legion: {"session":"ses_root","issue":"acme/widgets#1"} -->',
      ],
      ["gh", "api", "-X", "PATCH", "repos/acme/widgets/issues/1", "-f", "state=closed"],
    ]);
    expect(state.issues[root] as unknown).toMatchObject({
      state: "closed",
      finalCommentRef: "https://github.com/acme/widgets/issues/1#issuecomment-99",
    });
    expect(state.trees[root]).toMatchObject({ status: "lingering" });
  });
});
