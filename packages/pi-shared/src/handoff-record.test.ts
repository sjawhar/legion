import { afterEach, expect, test } from "bun:test";
import { lstat, mkdtemp, rm, utimes, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import * as path from "node:path";
import { forgetBrokerClients } from "@legion/envoy-client/broker-credential";
import { DispatchServiceError } from "@legion/envoy-client/dispatch-http";
import { fakeLatestHandoff } from "@legion/envoy-client/test/fake-dispatch";
import { restoreEnvironment } from "@legion/test-fixtures/environment";
import { hostTool, skipWithout } from "@legion/test-fixtures/host-tools";
import {
  versionControlEnvironment,
  versionControlIdentity,
  versionControlNames,
} from "@legion/test-fixtures/version-control-environment";
import {
  findHandoffReport,
  findPushedBranchHead,
  type JjRunner,
  paneLatestHandoff,
  pathJj,
} from "./handoff-record";

const ISSUE = "LEGION-208";
const STANDING = "c0de0000000000000000000000000000000000ff";
const TIP = "210d53a9d1b109df97fbb5dd5041d659dbab1323";
const REMOTE = 'remote_bookmarks(exact:"legion/LEGION-208", exact:"origin")';
const RECORD = { id: "5b1f0c2e-9d7a-4c3b-8e61-2f4a9d0c7b13", version: 3 };

const env = { LEGION_WORKSPACE: "/workspaces/LEGION-208", LEGION_ISSUE: ISSUE };

/** A jj that answers the report's commands, `@-` on the branch unless `pushed` is false, and
 * records each call's args. */
function fakeJj(pushed = true): { jj: JjRunner; calls: string[][] } {
  const calls: string[][] = [];
  const jj: JjRunner = async (args, cwd) => {
    expect(cwd).toBe(env.LEGION_WORKSPACE);
    calls.push([...args]);
    const revision = args[2];
    if (revision === "@-") return STANDING;
    if (revision === `${REMOTE} & ${STANDING}::`) return pushed ? TIP : "";
    throw new Error(`unexpected jj ${args.join(" ")}`);
  };
  return { jj, calls };
}

/** A Dispatch whose every phase answers RECORD, recording each read. */
const recordingDispatch = () => fakeLatestHandoff(async () => RECORD);

test("the implementer's round reports the newest implement record and the pushed tip", async () => {
  const { jj, calls } = fakeJj();
  const { latestHandoff, reads } = recordingDispatch();
  await expect(
    findHandoffReport({ phase: "implementing", role: "implementer", env, jj, latestHandoff })
  ).resolves.toEqual({ commit: TIP, handoffId: RECORD.id, handoffVersion: RECORD.version });
  expect(reads).toEqual(["LEGION-208 implement"]);
  // The commit it stands on, then the commit the branch names at origin when that one is on it.
  expect(calls).toEqual([
    ["log", "-r", "@-", "--no-graph", "-T", "commit_id"],
    ["log", "-r", `${REMOTE} & ${STANDING}::`, "--no-graph", "-T", "commit_id"],
  ]);
});

test("the planner, the tester and the reviewer report their phase's newest record and no commit", async () => {
  for (const [phase, role, word] of [
    ["planning", "planner", "plan"],
    ["testing", "tester", "test"],
    ["reviewing", "reviewer", "review"],
  ] as const) {
    const { jj, calls } = fakeJj();
    const { latestHandoff, reads } = recordingDispatch();
    await expect(findHandoffReport({ phase, role, env, jj, latestHandoff })).resolves.toEqual({
      handoffId: RECORD.id,
      handoffVersion: RECORD.version,
    });
    expect(reads).toEqual([`LEGION-208 ${word}`]);
    expect(calls).toEqual([]);
  }
});

test("every other completion reports the commit the workspace stands on, and reads no record", async () => {
  // Retro and the merger's READY write no record; a role reporting a phase it does not run reports
  // @- too, and the daemon refuses it naming whose phase it is.
  for (const [phase, role] of [
    ["retro", "implementer"],
    ["merging", "merger"],
    ["implementing", "tester"],
    ["admitted", "architect"],
  ] as const) {
    const { jj, calls } = fakeJj();
    const { latestHandoff, reads } = recordingDispatch();
    await expect(findHandoffReport({ phase, role, env, jj, latestHandoff })).resolves.toEqual({
      commit: STANDING,
    });
    expect(calls).toEqual([["log", "-r", "@-", "--no-graph", "-T", "commit_id"]]);
    expect(reads).toEqual([]);
  }
});

test("no record, or a Dispatch that does not answer, is refused naming what to do", async () => {
  const refusals: ReadonlyArray<readonly [() => Promise<never>, string]> = [
    [
      async () => {
        throw new DispatchServiceError("HANDOFF_NOT_FOUND", 404, "no handoff for test");
      },
      "LEGION-208 has no test handoff in Dispatch: write it with `dispatch handoff-write --issue LEGION-208 --phase test --body-file <file>`, then complete again",
    ],
    [
      async () => {
        throw new DispatchServiceError("UNAUTHORIZED", 401, "invalid token");
      },
      "handoff_complete could not read LEGION-208's newest test handoff from Dispatch: 401 UNAUTHORIZED: invalid token; complete again once Dispatch answers",
    ],
    [
      async () => {
        throw new TypeError("Unable to connect. Is the computer able to access the url?");
      },
      "handoff_complete could not read LEGION-208's newest test handoff from Dispatch: Unable to connect. Is the computer able to access the url?; complete again once Dispatch answers",
    ],
  ];
  for (const [answer, message] of refusals) {
    const { jj } = fakeJj();
    const { latestHandoff } = fakeLatestHandoff(answer);
    await expect(
      findHandoffReport({ phase: "testing", role: "tester", env, jj, latestHandoff })
    ).rejects.toThrow(new Error(message));
  }
});

test("an implementer whose @- is not on its branch at origin is refused naming the push", async () => {
  const { jj } = fakeJj(false);
  const { latestHandoff } = recordingDispatch();
  await expect(
    findHandoffReport({ phase: "implementing", role: "implementer", env, jj, latestHandoff })
  ).rejects.toThrow(
    new Error(
      `the commit this workspace stands on, ${STANDING}, is not on legion/LEGION-208@origin: push it with \`jj bookmark set legion/LEGION-208 -r @- && jj git push --bookmark legion/LEGION-208\`, then complete again`
    )
  );
});

test("the workspace and a well-formed issue key are required before jj or Dispatch is asked", async () => {
  const { jj, calls } = fakeJj();
  const { latestHandoff, reads } = recordingDispatch();
  await expect(
    findHandoffReport({
      phase: "retro",
      role: "implementer",
      env: { ...env, LEGION_WORKSPACE: undefined },
      jj,
      latestHandoff,
    })
  ).rejects.toThrow("LEGION_WORKSPACE is not set");
  await expect(
    findHandoffReport({
      phase: "implementing",
      role: "implementer",
      env: { ...env, LEGION_ISSUE: "LEGION-208#x" },
      jj,
      latestHandoff,
    })
  ).rejects.toThrow('LEGION_ISSUE "LEGION-208#x" is not a Dispatch issue key like LEGION-1');
  expect(calls).toEqual([]);
  expect(reads).toEqual([]);
});

test("a failing jj surfaces its command and stderr", async () => {
  const jj: JjRunner = async (args) => {
    throw new Error(`jj ${args.join(" ")} failed: Error: There is no jj repo in "."`);
  };
  const { latestHandoff } = recordingDispatch();
  await expect(
    findHandoffReport({ phase: "retro", role: "implementer", env, jj, latestHandoff })
  ).rejects.toThrow(
    'jj log -r @- --no-graph -T commit_id failed: Error: There is no jj repo in "."'
  );
});

// The head `ready_for_update` reports: the issue branch at the remote, read with the pane's jj.
test("the branch head is the issue bookmark at origin, and empty when the remote has no such branch", async () => {
  const calls: string[][] = [];
  const answer = async (head: string) => {
    const jj: JjRunner = async (args, cwd) => {
      expect(cwd).toBe(env.LEGION_WORKSPACE);
      calls.push([...args]);
      return head;
    };
    return findPushedBranchHead({ env, jj });
  };
  await expect(answer(TIP)).resolves.toBe(TIP);
  expect(calls).toEqual([["log", "-r", REMOTE, "--no-graph", "-T", "commit_id"]]);
  // A planner before its first push: no branch at the remote, and no error.
  await expect(answer("")).resolves.toBe("");

  // The issue key names the branch, and a worker sets its own environment.
  await expect(
    findPushedBranchHead({ env: { ...env, LEGION_ISSUE: "../x" }, jj: async () => TIP })
  ).rejects.toThrow(new Error('LEGION_ISSUE "../x" is not a Dispatch issue key like LEGION-1'));
  await expect(
    findPushedBranchHead({ env: { ...env, LEGION_WORKSPACE: "" }, jj: async () => TIP })
  ).rejects.toThrow(
    new Error(
      "LEGION_WORKSPACE is not set, so ready_for_update cannot find the branch head to report"
    )
  );
});

const scratch: string[] = [];
afterEach(async () => {
  for (const directory of scratch.splice(0)) await rm(directory, { recursive: true, force: true });
});
afterEach(forgetBrokerClients);

test("the pane's Dispatch reads the phase's newest record as the session's config names it", async () => {
  const root = await mkdtemp(path.join(tmpdir(), "legion-handoff-record-"));
  scratch.push(root);
  const file = { path: path.join(root, "envoy.json"), error: null } as const;
  const requests: Array<{ url: string; authorization: string | null }> = [];
  const answers: Response[] = [];
  const dispatch = (async (input, init) => {
    requests.push({
      url: input.toString(),
      authorization: new Headers(init?.headers).get("authorization"),
    });
    return answers.shift() ?? Response.json({ error: "unexpected" }, { status: 500 });
  }) as typeof fetch;
  const paneEnv = {
    ...env,
    DISPATCH_URL: "http://dispatch.test",
    DISPATCH_TOKEN: "pane-dispatch-token",
  };
  const latestHandoff = paneLatestHandoff(file, paneEnv, root, "pane-session", dispatch);
  const report = () =>
    findHandoffReport({ phase: "reviewing", role: "reviewer", env: paneEnv, latestHandoff });

  answers.push(
    Response.json({
      ...RECORD,
      issue_key: ISSUE,
      phase: "review",
      author: { kind: "agent", session_id: "ses_reviewer" },
      created_at: "2026-10-26T09:00:00Z",
      body: '{"verdict":"approve"}',
    })
  );
  await expect(report()).resolves.toEqual({
    handoffId: RECORD.id,
    handoffVersion: RECORD.version,
  });
  expect(requests).toEqual([
    {
      url: "http://dispatch.test/api/v1/issues/LEGION-208/handoffs/review/latest",
      authorization: "Bearer pane-dispatch-token",
    },
  ]);

  // Dispatch's own 404 for a phase with none is the missing-record refusal.
  answers.push(
    Response.json(
      { error: "no review handoff on LEGION-208", code: "HANDOFF_NOT_FOUND" },
      { status: 404 }
    )
  );
  await expect(report()).rejects.toThrow(
    "LEGION-208 has no review handoff in Dispatch: write it with `dispatch handoff-write --issue LEGION-208 --phase review --body-file <file>`, then complete again"
  );

  // A pane whose Dispatch is off says so, and asks nothing.
  const off = paneLatestHandoff(file, env, root, "pane-session", dispatch);
  await expect(
    findHandoffReport({ phase: "reviewing", role: "reviewer", env, latestHandoff: off })
  ).rejects.toThrow(
    /^handoff_complete could not read LEGION-208's newest review handoff from Dispatch: Dispatch is off for this pane: .*Dispatch is off; complete again once Dispatch answers$/
  );
  expect(requests).toHaveLength(2);
});

// The route the reviewer's `handoff_complete` failure came from: a pane whose Dispatch has no
// bearer at all and trades its own identity at the secrets broker. There is one credential field,
// so this pane presents the DPoP token the trade minted rather than an empty bearer.
test("a pane whose Dispatch is the broker's reads the record under its own session's token", async () => {
  const root = await mkdtemp(path.join(tmpdir(), "legion-handoff-broker-"));
  scratch.push(root);
  const file = { path: path.join(root, "envoy.json"), error: null } as const;
  const trades: URLSearchParams[] = [];
  const broker = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: async (request) => {
      trades.push(new URLSearchParams(await request.text()));
      return Response.json({
        access_token: "broker-access-token",
        issued_token_type: "urn:ietf:params:oauth:token-type:access_token",
        token_type: "DPoP",
        expires_in: 3600,
      });
    },
  });
  const identityFile = path.join(root, "projected-token");
  await Bun.write(identityFile, "projected.service.account.token\n");
  const asked: Array<{ authorization: string | null; proof: string | null }> = [];
  const dispatch = (async (_input, init) => {
    const headers = new Headers(init?.headers);
    asked.push({
      authorization: headers.get("authorization"),
      proof: headers.get("dpop"),
    });
    return Response.json(RECORD);
  }) as typeof fetch;
  const paneEnv = {
    ...env,
    DISPATCH_URL: "http://dispatch.test",
    AGENT_SECRETS_URL: `http://127.0.0.1:${broker.port}`,
    AGENT_SECRETS_MACHINE_IDENTITY: "kubernetes",
    AGENT_SECRETS_MACHINE_TOKEN_FILE: identityFile,
  };
  try {
    const latest = paneLatestHandoff(file, paneEnv, root, "pane-session", dispatch);
    await latest("LEGION-208", "review");
  } finally {
    broker.stop(true);
  }

  expect(trades.map((form) => form.get("session"))).toEqual(["pane-session"]);
  expect(asked.map((one) => one.authorization)).toEqual(["DPoP broker-access-token"]);
  expect(asked[0]?.proof).toBeString();
});

// The revsets are jj's, so a fake cannot prove them: one real repository with a bare origin, the
// round's change committed and the branch pushed, run through PATH's jj, as a pane is. A host
// with no jj runs neither row and says so, once for the file. The jj the rows below run is the one
// this walk found, so no spawn of theirs walks PATH again.
const jjProgram = hostTool("jj");
const withoutJj = skipWithout("jj");

// `isolatedHome` puts these on the test process as well, so the jj that the code under test
// (`pathJj`) starts reads the test's home rather than the developer's.
restoreEnvironment(versionControlNames);

/**
 * The environment this file's real jj and git run in, and the one it puts on the test process: the
 * shared isolation fixture over a home under `root`, so neither the developer's
 * `~/.jjconfig.toml`, nor their `~/.gitconfig`, nor the system gitconfig decides a result. It
 * reaches the test process too, because `pathJj` — the code under test — starts jj with the
 * process's own environment.
 */
function isolatedHome(root: string): Record<string, string> {
  const isolated = versionControlEnvironment(path.join(root, "home"));
  Object.assign(process.env, isolated);
  return { PATH: process.env.PATH ?? "", ...isolated };
}

/** Runs jj in `cwd` with the test's own environment, failing on a non-zero exit. */
async function jj(cwd: string, env: Record<string, string>, ...args: string[]): Promise<string> {
  const child = Bun.spawn([jjProgram ?? "jj", ...args], {
    cwd,
    env,
    stdout: "pipe",
    stderr: "pipe",
  });
  const [stdout, stderr, code] = await Promise.all([
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
    child.exited,
  ]);
  if (code !== 0) throw new Error(`jj ${args.join(" ")} exited ${code}:\n${stderr}`);
  return stdout.trim();
}

test.skipIf(withoutJj)(
  "against a real jj workspace, the report refuses an unpushed round and names the pushed tip once pushed",
  async () => {
    const root = await mkdtemp(path.join(tmpdir(), "legion-handoff-record-"));
    scratch.push(root);
    const workspace = path.join(root, "workspace");
    const childEnv = isolatedHome(root);
    const paneEnv = { ...versionControlIdentity, LEGION_WORKSPACE: workspace, LEGION_ISSUE: ISSUE };
    await Bun.spawn(["git", "init", "--bare", "--quiet", path.join(root, "origin.git")], {
      env: childEnv,
    }).exited;
    await jj(root, childEnv, "git", "init", workspace);
    await jj(workspace, childEnv, "git", "remote", "add", "origin", path.join(root, "origin.git"));
    await writeFile(path.join(workspace, "main.py"), 'print("hi")\n');
    await jj(workspace, childEnv, "describe", "-m", "implement: the change");
    await jj(workspace, childEnv, "new");
    const change = await jj(
      workspace,
      childEnv,
      "log",
      "-r",
      "@-",
      "--no-graph",
      "-T",
      "commit_id"
    );
    expect(change).toMatch(/^[0-9a-f]{40}$/);
    const { latestHandoff } = recordingDispatch();
    const report = () =>
      findHandoffReport({
        phase: "implementing",
        role: "implementer",
        env: paneEnv,
        jj: pathJj,
        latestHandoff,
      });

    // Committed but not pushed: the branch does not exist at origin yet.
    await expect(report()).rejects.toThrow(
      `the commit this workspace stands on, ${change}, is not on legion/${ISSUE}@origin`
    );

    // Pushed: the branch's tip is reported with the record.
    await jj(workspace, childEnv, "bookmark", "set", `legion/${ISSUE}`, "-r", "@-");
    await jj(workspace, childEnv, "git", "push", "--bookmark", `legion/${ISSUE}`);
    await expect(report()).resolves.toEqual({
      commit: change,
      handoffId: RECORD.id,
      handoffVersion: RECORD.version,
    });

    // A later commit not yet pushed is refused again.
    await writeFile(path.join(workspace, "main.py"), 'print("bye")\n');
    await jj(workspace, childEnv, "describe", "-m", "implement: the fix");
    await jj(workspace, childEnv, "new");
    const fix = await jj(workspace, childEnv, "log", "-r", "@-", "--no-graph", "-T", "commit_id");
    await expect(report()).rejects.toThrow(
      `the commit this workspace stands on, ${fix}, is not on legion/${ISSUE}@origin`
    );

    // Retro reports the commit the workspace stands on, pushed or not.
    await expect(
      findHandoffReport({
        phase: "retro",
        role: "implementer",
        env: paneEnv,
        jj: pathJj,
        latestHandoff,
      })
    ).resolves.toEqual({ commit: fix });
  },
  60_000
);

// A git lock an interrupted git or jj left in the workspace fails every jj that snapshots a change
// into the working copy (jj rewrites the git index then), and nothing under a running pod removes
// it. The failure names the lock and its age, and the lock stays.
test.skipIf(withoutJj)(
  "a jj that fails on a stale git lock names it, its age, and when it goes",
  async () => {
    const root = await mkdtemp(path.join(tmpdir(), "legion-handoff-lock-"));
    scratch.push(root);
    const workspace = path.join(root, "workspace");
    const childEnv = isolatedHome(root);
    await jj(root, childEnv, "git", "init", "--colocate", workspace);
    await writeFile(path.join(workspace, "main.py"), 'print("hi")\n');
    const lock = path.join(workspace, ".git", "index.lock");
    await writeFile(lock, "");
    const written = new Date(Date.now() - 31 * 60 * 1000);
    await utimes(lock, written, written);

    const failure = pathJj(["diff", "-r", "@", "--name-only"], workspace);
    await expect(failure).rejects.toThrow("Could not acquire lock for index file");
    await expect(failure).rejects.toThrow(
      new RegExp(
        `git lock ${lock.replaceAll(/[.*+?^${}()|[\]\\]/g, "\\$&")} was last written 31m\\d+s ago and blocks this command\\. Do not remove it while the issue's pod runs: .* Replacing the issue's pod removes it\\. Under the tmux runtime nothing removes it`
      )
    );
    expect((await lstat(lock)).isFile()).toBe(true);
  },
  60_000
);
