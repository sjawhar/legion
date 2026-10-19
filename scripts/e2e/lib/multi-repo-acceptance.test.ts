import { afterEach, describe, expect, test } from "bun:test";
import { mkdirSync, readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import {
  bashPath,
  closeServers,
  daemonActorEvent,
  daemonStub,
  dir,
  dispatchStub,
  fakeBin,
  fullInputs,
  humanActorEvent,
  isolatedPath,
  issueUpdatedEvent,
  makeStateBody,
  mutableDaemonStub,
  newFakeDir,
  nextRun,
  noneNote,
  run,
  script,
  spawnScript,
  stateBody,
} from "./multi-repo-acceptance-harness";

// scripts/e2e/multi-repo-acceptance.sh is the observer for the live multi-repository acceptance
// run; this file covers every observation but second-owner (multi-repo-second-owner.test.ts). The
// stubs and fakes it runs against are in multi-repo-acceptance-harness.ts.
let daemonServer: Server | undefined;
let dispatchServer: Server | undefined;

afterEach(() => {
  daemonServer?.stop(true);
  dispatchServer?.stop(true);
  daemonServer = undefined;
  dispatchServer = undefined;
  closeServers();
});

describe("usage: an absent or invalid observation is refused before any env, tool, or network check", () => {
  test("no observation named: exit 2, with no env set and no PATH at all", async () => {
    const work = join(dir, `run-${nextRun()}`);
    mkdirSync(work, { recursive: true });
    const { status, stderr } = await spawnScript([], "", work, {});
    expect(status).toBe(2);
    expect(stderr).toContain("usage:");
  });

  test("negative pr-in-x without before|after is refused usage, exit 2", async () => {
    const work = join(dir, `run-${nextRun()}`);
    mkdirSync(work, { recursive: true });
    const { status } = await spawnScript(["negative", "pr-in-x", "ACME-2"], "", work, {});
    expect(status).toBe(2);
  });

  test("negative pr-in-x after with the wrong argument count is refused usage, exit 2", async () => {
    const work = join(dir, `run-${nextRun()}`);
    mkdirSync(work, { recursive: true });
    const { status } = await spawnScript(["negative", "pr-in-x", "after", "run-1"], "", work, {});
    expect(status).toBe(2);
  });
});

describe("required inputs", () => {
  test("every input unset: the refusal names the first missing one", async () => {
    const { status, stderr } = await run(["done", "X-1"], {});
    expect(status).not.toBe(0);
    expect(stderr).toContain("LEGION_E2E_REPO_T is unset");
  });

  test("each input missing in turn names itself", async () => {
    const all = fullInputs("http://127.0.0.1:1", "http://127.0.0.1:1");
    for (const name of Object.keys(all)) {
      const env = { ...all };
      delete (env as Record<string, string>)[name];
      const { status, stderr } = await run(["done", "X-1"], env);
      expect(status).not.toBe(0);
      expect(stderr).toContain(`${name} is unset`);
    }
  });
});

describe("repository shape and collisions", () => {
  test("a malformed repository value is refused before any network read", async () => {
    const env = fullInputs("http://127.0.0.1:1", "http://127.0.0.1:1");
    env.LEGION_E2E_REPO_T = "not-a-repo; $(touch /tmp/pwned)";
    const { status, stderr } = await run(["done", "X-1"], env);
    expect(status).not.toBe(0);
    expect(stderr).toContain("not an owner/name repository");
  });

  test("T and O sharing an owner in a different case is still refused (case-insensitive)", async () => {
    const env = fullInputs("http://127.0.0.1:1", "http://127.0.0.1:1");
    env.LEGION_E2E_REPO_T = "Acme/widgets";
    env.LEGION_E2E_REPO_O = "ACME/other-repo";
    const { status, stderr } = await run(["done", "X-1"], env);
    expect(status).not.toBe(0);
    expect(stderr).toContain("share an owner");
  });

  test("two repository variables naming the same repository in different case is still refused", async () => {
    const env = fullInputs("http://127.0.0.1:1", "http://127.0.0.1:1");
    env.LEGION_E2E_REPO_C = "ACME/Widgets";
    const { status, stderr } = await run(["done", "X-1"], env);
    expect(status).not.toBe(0);
    expect(stderr).toContain("both name");
  });
});

describe("the daemon health refusal", () => {
  test("a stub whose /healthz answers 404 is refused, naming the status", async () => {
    daemonServer = Bun.serve({ port: 0, fetch: () => new Response("not found", { status: 404 }) });
    const env = fullInputs(`http://127.0.0.1:${daemonServer.port}`, "http://127.0.0.1:1");
    const { status, stderr } = await run(["done", "X-1"], env);
    expect(status).not.toBe(0);
    expect(stderr).toContain(
      `LEGION_E2E_DAEMON_URL http://127.0.0.1:${daemonServer.port} is not a Legion daemon`
    );
  });
});

describe("children: structured Dispatch + state agreement, with raw evidence", () => {
  test("one child resolves to T and one to C in both Dispatch and the daemon's state: PASS, with raw reads kept", async () => {
    daemonServer = daemonStub();
    dispatchServer = dispatchStub({
      "ACME-1": JSON.stringify({ key: "ACME-1", children: [{ key: "ACME-2" }, { key: "ACME-3" }] }),
      "ACME-2": JSON.stringify({
        key: "ACME-2",
        repo: "acme/widgets",
        resolved_repo: "acme/widgets",
      }),
      "ACME-3": JSON.stringify({
        key: "ACME-3",
        repo: "acme/gadgets",
        resolved_repo: "acme/gadgets",
      }),
    });
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { status, stdout, evidence } = await run(["children", "ACME-1"], env);
    expect(status).toBe(0);
    expect(stdout).toContain("CHECK children: PASS");
    expect(stdout).toContain("ACME-2: dispatch=acme/widgets state=acme/widgets");
    // The raw Dispatch and state reads this observation judged are kept, not just a summary,
    // named per observation (children-*), never a bare state.raw every observation shares.
    expect(
      JSON.parse(readFileSync(join(evidence, "children-dispatch-issue-ACME-2.raw"), "utf8"))
        .resolved_repo
    ).toBe("acme/widgets");
    expect(
      JSON.parse(readFileSync(join(evidence, "children-state.raw"), "utf8")).issues["ACME-2"].repo
    ).toBe("acme/widgets");
  });

  test("a child Dispatch and the daemon's state disagree on: FAIL", async () => {
    daemonServer = daemonStub();
    dispatchServer = dispatchStub({
      "ACME-1": JSON.stringify({ key: "ACME-1", children: [{ key: "ACME-2" }, { key: "ACME-3" }] }),
      "ACME-2": JSON.stringify({
        key: "ACME-2",
        repo: "acme/gadgets",
        resolved_repo: "acme/gadgets",
      }),
      "ACME-3": JSON.stringify({
        key: "ACME-3",
        repo: "acme/gadgets",
        resolved_repo: "acme/gadgets",
      }),
    });
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { status, stderr } = await run(["children", "ACME-1"], env);
    expect(status).not.toBe(0);
    expect(stderr).toContain("CHECK children: FAIL");
  });
});

describe("done: Dispatch's structured .status field", () => {
  test("every named issue reading status done is a PASS", async () => {
    daemonServer = daemonStub();
    dispatchServer = dispatchStub({
      "ACME-2": JSON.stringify({ key: "ACME-2", status: "done" }),
      "ACME-3": JSON.stringify({ key: "ACME-3", status: "done" }),
    });
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { status, stdout } = await run(["done", "ACME-2", "ACME-3"], env);
    expect(status).toBe(0);
    expect(stdout).toContain("CHECK done: PASS");
  });

  test("one issue still todo is a FAIL", async () => {
    daemonServer = daemonStub();
    dispatchServer = dispatchStub({ "ACME-2": JSON.stringify({ key: "ACME-2", status: "todo" }) });
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { status, stderr } = await run(["done", "ACME-2"], env);
    expect(status).not.toBe(0);
    expect(stderr).toContain("CHECK done: FAIL");
  });
});

describe("a failed Dispatch read never prints the bearer", () => {
  test("done against a 404'd issue fails without the Dispatch token anywhere in stdout or stderr", async () => {
    daemonServer = daemonStub();
    dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { status, stdout, stderr } = await run(["done", "ACME-404"], env);
    expect(status).not.toBe(0);
    expect(stdout).not.toContain("dispatch-bearer");
    expect(stderr).not.toContain("dispatch-bearer");
    expect(stderr).not.toContain("Authorization");
    expect(stderr).toContain("GET");
    expect(stderr).toContain("failed");
  });
});

// A SIGTERM to the observer's own pid, outside any process group signal,
// must stop the script immediately with the shell's conventional 143 — not clean up and press on
// into a later, unrelated read that then fails for the wrong reason — and a group signal (every
// process in the pipeline, curl included) must not leave a raw_fetch temp file behind even though
// most raw_fetch calls run inside a `$(...)` subshell, where a plain variable a signal handler
// reads in the parent is never set.
//
// Both tests send a real OS signal to a real bash subprocess mid-fetch and then wait on its real
// exit: there is no in-process JS timer to fake here (the clock being driven is the kernel's, not
// Bun's), so a real `Bun.sleep` window for the signal to land mid-request, and a real slow HTTP
// response to hold that window open, are the only way to exercise this.
describe("an interrupted run cleans up and actually stops", () => {
  test("a SIGTERM to the observer's own pid stops it immediately with exit 143, not a later failure", async () => {
    daemonServer = Bun.serve({
      port: 0,
      async fetch(req) {
        const url = new URL(req.url);
        if (url.pathname === "/healthz") return Response.json({ status: "ok" });
        if (url.pathname === "/legion/v1/state") {
          await Bun.sleep(3000);
          return new Response(stateBody, { headers: { "content-type": "application/json" } });
        }
        return new Response("not found", { status: 404 });
      },
    });
    dispatchServer = dispatchStub({
      "ACME-1": JSON.stringify({ key: "ACME-1", children: [{ key: "ACME-2" }] }),
      "ACME-2": JSON.stringify({ key: "ACME-2", resolved_repo: "acme/widgets" }),
    });
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { bin, evidence } = newFakeDir();
    const proc = Bun.spawn([bashPath, script, "children", "ACME-1"], {
      env: { PATH: isolatedPath(bin), MULTIREPO_EVIDENCE_DIR: evidence, ...env },
      stdout: "pipe",
      stderr: "pipe",
    });
    await Bun.sleep(300); // real wait: lets the real subprocess reach its real slow fetch
    process.kill(proc.pid, "SIGTERM");
    const status = await proc.exited;
    expect(status).toBe(143);
  });

  test("a group SIGINT removes a raw_fetch temp file created inside a $(...) subshell", async () => {
    daemonServer = Bun.serve({
      port: 0,
      async fetch(req) {
        const url = new URL(req.url);
        if (url.pathname === "/healthz") return Response.json({ status: "ok" });
        if (url.pathname === "/legion/v1/state") {
          await Bun.sleep(3000);
          return new Response(stateBody, { headers: { "content-type": "application/json" } });
        }
        return new Response("not found", { status: 404 });
      },
    });
    dispatchServer = dispatchStub({
      "ACME-1": JSON.stringify({ key: "ACME-1", children: [{ key: "ACME-2" }] }),
      "ACME-2": JSON.stringify({ key: "ACME-2", resolved_repo: "acme/widgets" }),
    });
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { bin, evidence } = newFakeDir();
    const proc = Bun.spawn(["setsid", "--", bashPath, script, "children", "ACME-1"], {
      env: { PATH: isolatedPath(bin), MULTIREPO_EVIDENCE_DIR: evidence, ...env },
      stdout: "pipe",
      stderr: "pipe",
    });
    await Bun.sleep(300); // real wait: lets the real subprocess reach its real slow fetch
    process.kill(-proc.pid, "SIGINT");
    const status = await proc.exited;
    expect(status).toBe(130);
    const stray = readdirSync(evidence).filter((f) => /\.raw\.[^.]{6}$/.test(f));
    expect(stray).toEqual([]);
  });
});

describe("raw evidence is named per observation, never overwritten by a later one", () => {
  test("children's state.raw survives a later, different observation's own state read", async () => {
    const { server, setBody } = mutableDaemonStub(
      makeStateBody({
        issues: {
          "ACME-2": {
            key: "ACME-2",
            generation: 1,
            phase: "implementing",
            status: "todo",
            repo: "acme/widgets",
          },
          "ACME-3": {
            key: "ACME-3",
            generation: 1,
            phase: "implementing",
            status: "todo",
            repo: "acme/gadgets",
          },
        },
      })
    );
    daemonServer = server;
    dispatchServer = dispatchStub({
      "ACME-1": JSON.stringify({ key: "ACME-1", children: [{ key: "ACME-2" }, { key: "ACME-3" }] }),
      "ACME-2": JSON.stringify({
        key: "ACME-2",
        repo: "acme/widgets",
        resolved_repo: "acme/widgets",
      }),
      "ACME-3": JSON.stringify({
        key: "ACME-3",
        repo: "acme/gadgets",
        resolved_repo: "acme/gadgets",
      }),
      "ACME-9": JSON.stringify({ key: "ACME-9", resolved_repo: null }),
    });
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { bin, evidence } = newFakeDir();
    const path = isolatedPath(bin);
    const children1 = await spawnScript(["children", "ACME-1"], path, evidence, env);
    expect(children1.status).toBe(0);
    const childrenStateBefore = readFileSync(join(evidence, "children-state.raw"), "utf8");
    // The daemon's state moves on (ACME-9 now admitted) before a later, unrelated observation
    // reads it; children's own raw read must still be what it actually read, not this one.
    setBody(
      makeStateBody({
        issues: {
          "ACME-9": {
            key: "ACME-9",
            generation: 1,
            phase: "implementing",
            status: "todo",
            repo: "acme/widgets",
          },
        },
      })
    );
    await spawnScript(["negative", "no-repo", "ACME-9"], path, evidence, env);
    const childrenStateAfter = readFileSync(join(evidence, "children-state.raw"), "utf8");
    expect(childrenStateAfter).toBe(childrenStateBefore);
    expect(readFileSync(join(evidence, "negative-no-repo-state.raw"), "utf8")).not.toBe(
      childrenStateBefore
    );
  });

  // A state read made fresh per child could judge an early child against
  // one read and a later child against a different one the daemon moved to meanwhile, while the
  // kept evidence could only ever hold the last — contradicting the judgment of a child read before it.
  // children reads state exactly once, before any child's judgment.
  test("children reads state exactly once for a multi-child parent, never once per child", async () => {
    let stateRequests = 0;
    daemonServer = Bun.serve({
      port: 0,
      fetch(req) {
        const url = new URL(req.url);
        if (url.pathname === "/healthz") return Response.json({ status: "ok" });
        if (url.pathname === "/legion/v1/state") {
          stateRequests += 1;
          return new Response(
            makeStateBody({
              issues: {
                "ACME-2": { key: "ACME-2", repo: "acme/widgets" },
                "ACME-3": { key: "ACME-3", repo: "acme/gadgets" },
              },
            }),
            { headers: { "content-type": "application/json" } }
          );
        }
        return new Response("not found", { status: 404 });
      },
    });
    dispatchServer = dispatchStub({
      "ACME-1": JSON.stringify({ key: "ACME-1", children: [{ key: "ACME-2" }, { key: "ACME-3" }] }),
      "ACME-2": JSON.stringify({
        key: "ACME-2",
        repo: "acme/widgets",
        resolved_repo: "acme/widgets",
      }),
      "ACME-3": JSON.stringify({
        key: "ACME-3",
        repo: "acme/gadgets",
        resolved_repo: "acme/gadgets",
      }),
    });
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { status, stdout } = await run(["children", "ACME-1"], env);
    expect(status).toBe(0);
    expect(stdout).toContain("CHECK children: PASS");
    expect(stateRequests).toBe(1);
  });
});

describe("record-head and pin: empty/invalid values never pass, trailing bytes kept", () => {
  test("record-head refuses a repository argument that is not T", async () => {
    daemonServer = daemonStub();
    dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { status, stderr } = await run(["record-head", "acme/gadgets"], env, {});
    expect(status).not.toBe(0);
    expect(stderr).toContain("not LEGION_E2E_REPO_T");
  });

  test("an empty commit sha from GitHub is never recorded as the head: FAIL, not PASS", async () => {
    daemonServer = daemonStub();
    dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const gh = `
case "$*" in
  "api repos/acme/widgets --jq .default_branch") printf 'main' ;;
  "api repos/acme/widgets/branches/main --jq .commit.sha") printf '' ;;
  *) echo "gh: unexpected call: $*" >&2; exit 1 ;;
esac`;
    const { status, stderr } = await run(["record-head", "acme/widgets"], env, { gh });
    expect(status).not.toBe(0);
    expect(stderr).toContain("CHECK record-head: FAIL");
  });

  test("a well-formed record-head is a PASS", async () => {
    daemonServer = daemonStub();
    dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const sha = "a".repeat(40);
    const gh = `
case "$*" in
  "api repos/acme/widgets --jq .default_branch") printf 'main' ;;
  "api repos/acme/widgets/branches/main --jq .commit.sha") printf '${sha}' ;;
  *) echo "gh: unexpected call: $*" >&2; exit 1 ;;
esac`;
    const { status, stdout } = await run(["record-head", "acme/widgets"], env, { gh });
    expect(status).toBe(0);
    expect(stdout).toContain(sha);
    expect(stdout).toContain("CHECK record-head: PASS");
  });

  test("pin refuses an empty merge-sha argument", async () => {
    daemonServer = daemonStub();
    dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { status, stderr } = await run(["pin", "acme/gadgets", ""], env, {});
    expect(status).not.toBe(0);
    expect(stderr).toContain("not a full 40-character commit id");
  });

  // A pin file ending in two newlines must keep both in evidence/pin.txt.
  test("the pin file's trailing bytes (two trailing newlines) are kept exactly in evidence", async () => {
    daemonServer = daemonStub();
    dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const sha = "b".repeat(40);
    const pinContent = `pin: ${sha}\n\n`;
    const b64 = Buffer.from(pinContent).toString("base64");
    const gh = `
case "$*" in
  "api repos/acme/gadgets --jq .default_branch") printf 'main' ;;
  "api repos/acme/gadgets/contents/toolchain-pin.txt?ref=main --jq .content") printf '${b64}' ;;
  *) echo "gh: unexpected call: $*" >&2; exit 1 ;;
esac`;
    const { status, evidence } = await run(["pin", "acme/gadgets", sha], env, { gh });
    expect(status).toBe(0);
    const written = readFileSync(join(evidence, "pin.txt"));
    expect(written.length).toBe(Buffer.byteLength(pinContent));
    expect(written.toString()).toBe(pinContent);
  });
});

describe("pin compares the pinned commit exactly; a branch name and an issue key never change a request", () => {
  const pinGh = (path: string, content: string, branch = "main", encodedRef = branch) => `
case "$*" in
  "api repos/acme/gadgets --jq .default_branch") printf '${branch}' ;;
  "api repos/acme/gadgets/contents/${path}?ref=${encodedRef} --jq .content") printf '${Buffer.from(content).toString("base64")}' ;;
  *) echo "gh: unexpected call: $*" >&2; exit 1 ;;
esac`;

  // The pin is the one commit id the file holds. The sha in a comment beside a pin naming another
  // commit, or inside a longer hex string, is not the pin.
  test("a pin file naming another commit, with the sha only in a comment, is a FAIL", async () => {
    daemonServer = daemonStub();
    dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const sha = "b".repeat(40);
    const gh = pinGh("toolchain-pin.txt", `# previous: ${sha}\npin: ${"c".repeat(40)}\n`);
    const { status, stderr } = await run(["pin", "acme/gadgets", sha], env, { gh });
    expect(status).not.toBe(0);
    expect(stderr).toContain("CHECK pin: FAIL");
  });

  test("a pin file whose only hex string is longer than the sha (one containing it) is a FAIL", async () => {
    daemonServer = daemonStub();
    dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const sha = "b".repeat(40);
    const gh = pinGh("toolchain-pin.txt", `sha256: ${sha}${"0".repeat(24)}\n`);
    const { status, stderr } = await run(["pin", "acme/gadgets", sha], env, { gh });
    expect(status).not.toBe(0);
    expect(stderr).toContain("CHECK pin: FAIL");
  });

  test("the same commit id spelled in capitals, named once or twice, is the pin: PASS", async () => {
    daemonServer = daemonStub();
    dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const sha = "ab12".repeat(10);
    const gh = pinGh("toolchain-pin.txt", `rev = "${sha.toUpperCase()}" # ${sha}\n`);
    const { status, stdout } = await run(["pin", "acme/gadgets", sha], env, { gh });
    expect(status).toBe(0);
    expect(stdout).toContain("CHECK pin: PASS");
  });

  // A default branch is whatever GitHub says: a `?` or `#` in it must not end or change the request.
  test("a default branch with a ? and a # is percent-encoded in the request, its slash kept", async () => {
    daemonServer = daemonStub();
    dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const sha = "b".repeat(40);
    const gh = pinGh("toolchain-pin.txt", `pin: ${sha}\n`, "release/v7?x#y", "release/v7%3Fx%23y");
    const { status, stdout } = await run(["pin", "acme/gadgets", sha], env, { gh });
    expect(status).toBe(0);
    expect(stdout).toContain("CHECK pin: PASS");
  });

  // Issue keys reach evidence file names and API paths, so a key shaped like anything else is a
  // usage error before any input is read.
  test("an issue key that is not PROJECT-123 is a usage error, for every observation that takes one", async () => {
    for (const args of [
      ["children", "../etc/passwd"],
      ["branch-base", "acme-1"],
      ["done", "ACME-1", "ACME-1/../x"],
      ["second-owner", "ACME-1?x", "the-pod"],
      ["cache", "ACME-", "the-pod"],
      ["negative", "no-repo", "ACME-1 ; id"],
      ["negative", "pr-in-x", "before", "../ACME-1"],
      ["negative", "repo-edit", "after", "somerun", "ACME-x"],
    ]) {
      const { status, stderr } = await run(args, {}, {});
      expect(status, args.join(" ")).toBe(2);
      expect(stderr, args.join(" ")).toContain("is not an issue key (PROJECT-123)");
    }
  });
});

describe("cache: pod binding and the producer's exact, anchored success line", () => {
  const stateWithCache = makeStateBody({
    issues: {
      "ACME-2": {
        key: "ACME-2",
        generation: 1,
        phase: "implementing",
        status: "todo",
        repo: "acme/widgets",
      },
    },
    repositories: {
      owners: [],
      mapped: [
        {
          repo: "acme/widgets",
          default: true,
          cache: { repo: "acme/widgets", sandbox: "s", state: "ready", ready: true },
        },
      ],
      refused: [],
      caches: [],
    },
  });

  test("an unrelated pod is refused, not read as the issue's", async () => {
    daemonServer = daemonStub(stateWithCache);
    dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const kubectl = `
case "$*" in
  "get pod other-pod -o jsonpath={.metadata.labels.legion\\.dev/issue}") printf 'ACME-99' ;;
  *) echo "kubectl: unexpected call: $*" >&2; exit 1 ;;
esac`;
    const { status, stderr } = await run(["cache", "ACME-2", "other-pod"], env, { kubectl });
    expect(status).not.toBe(0);
    expect(stderr).toContain("is not that issue's pod");
  });

  // The anchored form rejects a near-miss line an unanchored `.*`-heavy regex would accept:
  // an error line that happens to contain the same two keywords (cache, GitHub) in another shape.
  test("an error line mentioning cache and github in another shape is a FAIL, not a PASS", async () => {
    daemonServer = daemonStub(stateWithCache);
    dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const kubectl = `
case "$*" in
  "get pod the-pod -o jsonpath={.metadata.labels.legion\\.dev/issue}") printf 'ACME-2' ;;
  "logs the-pod -c workspace-fetch") printf 'workspace-init fetch: the repository cache failed; fell back to GitHub for everything\\n' ;;
  *) echo "kubectl: unexpected call: $*" >&2; exit 1 ;;
esac`;
    const { status, stderr } = await run(["cache", "ACME-2", "the-pod"], env, { kubectl });
    expect(status).not.toBe(0);
    expect(stderr).toContain("CHECK cache: FAIL");
  });

  test("the producer's exact anchored success line, on the right pod, with cache.ready true: PASS", async () => {
    daemonServer = daemonStub(stateWithCache);
    dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const kubectl = `
case "$*" in
  "get pod the-pod -o jsonpath={.metadata.labels.legion\\.dev/issue}") printf 'ACME-2' ;;
  "logs the-pod -c workspace-fetch") printf 'workspace-init fetch: 6,612,345,678 bytes from the repository cache http://cache:8418 in 3s, its history verified in 1s; 128 bytes from GitHub for the delta (2 refs)\\n' ;;
  *) echo "kubectl: unexpected call: $*" >&2; exit 1 ;;
esac`;
    const { status, stdout, evidence } = await run(["cache", "ACME-2", "the-pod"], env, {
      kubectl,
    });
    expect(status).toBe(0);
    expect(stdout).toContain("CHECK cache: PASS");
    expect(readFileSync(join(evidence, "cache-logs-ACME-2.raw"), "utf8")).toContain(
      "workspace-init fetch:"
    );
  });

  // The duration class was [0-9.]+(µs|ms|s), which a duration of a minute or more (Go's
  // time.Duration.String(), e.g. "1m2.345s") never matches: a slow fetch would FAIL a real PASS.
  test("a fetch slower than a minute (Go's 1m2.345s duration form) is still a PASS", async () => {
    daemonServer = daemonStub(stateWithCache);
    dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const kubectl = `
case "$*" in
  "get pod the-pod -o jsonpath={.metadata.labels.legion\\.dev/issue}") printf 'ACME-2' ;;
  "logs the-pod -c workspace-fetch") printf 'workspace-init fetch: 6,612,345,678 bytes from the repository cache http://cache:8418 in 1m2.345s, its history verified in 1m0.1s; 128 bytes from GitHub for the delta (2 refs)\\n' ;;
  *) echo "kubectl: unexpected call: $*" >&2; exit 1 ;;
esac`;
    const { status, stdout } = await run(["cache", "ACME-2", "the-pod"], env, { kubectl });
    expect(status).toBe(0);
    expect(stdout).toContain("CHECK cache: PASS");
  });
});

describe("negative no-repo: exact daemon actor, outbox marker, resolved_repo, and the issues-type guard", () => {
  test("the exact none-note from the daemon's own actor, Dispatch still resolving to none: PASS", async () => {
    daemonServer = daemonStub();
    dispatchServer = dispatchStub(
      { "ACME-9": JSON.stringify({ key: "ACME-9", resolved_repo: null }) },
      {
        "ACME-9": JSON.stringify([
          daemonActorEvent("ACME", 5, `${noneNote}\n\n<!-- legion-outbox:41 -->`),
        ]),
      }
    );
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { status, stdout } = await run(["negative", "no-repo", "ACME-9"], env);
    expect(status).toBe(0);
    expect(stdout).toContain("CHECK negative-no-repo-ACME-9: PASS");
  });

  test("a human message quoting the exact none-note is a FAIL: the actor must be the daemon's own session", async () => {
    daemonServer = daemonStub();
    dispatchServer = dispatchStub(
      { "ACME-9": JSON.stringify({ key: "ACME-9", resolved_repo: null }) },
      { "ACME-9": JSON.stringify([humanActorEvent(`FYI the daemon said: ${noneNote}`)]) }
    );
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { status, stderr } = await run(["negative", "no-repo", "ACME-9"], env);
    expect(status).not.toBe(0);
    expect(stderr).toContain("CHECK negative-no-repo-ACME-9: FAIL");
  });

  test("Dispatch now resolves the issue to a real repository (a stale none-note from before): FAIL", async () => {
    daemonServer = daemonStub();
    dispatchServer = dispatchStub(
      { "ACME-9": JSON.stringify({ key: "ACME-9", resolved_repo: "acme/gadgets" }) },
      {
        "ACME-9": JSON.stringify([
          daemonActorEvent("ACME", 5, `${noneNote}\n\n<!-- legion-outbox:41 -->`),
        ]),
      }
    );
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { status, stderr } = await run(["negative", "no-repo", "ACME-9"], env);
    expect(status).not.toBe(0);
    expect(stderr).toContain("CHECK negative-no-repo-ACME-9: FAIL");
  });

  test("the daemon's latest repository note is a different one (App not installed), not the none-note: FAIL", async () => {
    daemonServer = daemonStub();
    const laterNote =
      "Legion did not admit this issue: the review App is not installed on acme/gadgets. Install it on acme/gadgets, or set another repository (the issue header's Repository picker, or dispatch issue-update --repo owner/name), and move the issue to todo again.";
    dispatchServer = dispatchStub(
      { "ACME-9": JSON.stringify({ key: "ACME-9", resolved_repo: null }) },
      {
        "ACME-9": JSON.stringify([
          daemonActorEvent("ACME", 6, `${laterNote}\n\n<!-- legion-outbox:42 -->`),
          daemonActorEvent("ACME", 5, `${noneNote}\n\n<!-- legion-outbox:41 -->`),
        ]),
      }
    );
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { status, stderr } = await run(["negative", "no-repo", "ACME-9"], env);
    expect(status).not.toBe(0);
    expect(stderr).toContain("CHECK negative-no-repo-ACME-9: FAIL");
  });

  test("a state body of {} or null never satisfies `.issues has(key)` vacuously: FAIL", async () => {
    daemonServer = daemonStub("{}");
    dispatchServer = dispatchStub(
      { "ACME-9": JSON.stringify({ key: "ACME-9", resolved_repo: null }) },
      {
        "ACME-9": JSON.stringify([
          daemonActorEvent("ACME", 5, `${noneNote}\n\n<!-- legion-outbox:41 -->`),
        ]),
      }
    );
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { status, stderr } = await run(["negative", "no-repo", "ACME-9"], env);
    expect(status).not.toBe(0);
    expect(stderr).toContain("CHECK negative-no-repo-ACME-9: FAIL");
  });
});

// prsGhFake is a fake `gh` answering both calls negative_pr_in_x makes against X's pull-request
// list: the `--jq [.[].number] | max // 0` cursor read at `before`, and the raw list read at
// `after` (raw_fetch, no --jq) — both must answer the same prsJson consistently within one test.
function prsGhFake(prsJson: string): string {
  const highest = Math.max(
    0,
    ...(JSON.parse(prsJson) as Array<{ number: number }>).map((pr) => pr.number)
  );
  return `
case "$*" in
  "api repos/other/outside/pulls?state=all&per_page=100 --jq [.[].number] | max // 0") printf '${highest}' ;;
  "api repos/other/outside/pulls?state=all&per_page=100") printf '${prsJson}' ;;
  *) echo "gh: unexpected: $*" >&2; exit 1 ;;
esac`;
}

// baselineRunId pulls the minted run id out of a `before` call's stdout.
function baselineRunId(stdout: string): string {
  const runId = stdout.match(/RUN_ID: (\S+)/)?.[1] ?? "";
  expect(runId).not.toBe("");
  return runId;
}

describe("negative pr-in-x: PR-number cursor and the daemon-side check", () => {
  test("before prints BASELINE, never PASS, for a recorded child", async () => {
    daemonServer = daemonStub();
    dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const gh = prsGhFake("[]");
    const { status, stdout } = await run(["negative", "pr-in-x", "before", "ACME-2"], env, { gh });
    expect(status).toBe(0);
    expect(stdout).toContain("BASELINE pr-in-x-ACME-2: captured");
    expect(stdout).not.toContain("PASS");
  });

  test("before on an unrecorded issue is refused: no recorded child, nothing to baseline", async () => {
    daemonServer = daemonStub();
    dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { status, stderr } = await run(["negative", "pr-in-x", "before", "ACME-404"], env);
    expect(status).not.toBe(0);
    expect(stderr).toContain("not the recorded child this control needs");
  });

  test("after with an unknown run id is refused", async () => {
    daemonServer = daemonStub();
    dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { status, stderr } = await run(
      ["negative", "pr-in-x", "after", "nonexistent-run", "ACME-2"],
      env
    );
    expect(status).not.toBe(0);
    expect(stderr).toContain("no baseline for run");
  });

  // A run id with a path-traversal shape is refused outright, never used to open a
  // baseline file outside this run's own evidence directory.
  test("a run id containing '..' is refused as unsafe, never read as a path", async () => {
    daemonServer = daemonStub();
    dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { status, stderr } = await run(
      ["negative", "pr-in-x", "after", "../../etc/passwd", "ACME-2"],
      env
    );
    expect(status).not.toBe(0);
    expect(stderr).toContain("not a bare id");
  });

  test("after immediately (no PR opened yet) with highest-PR 0 in X: FAIL", async () => {
    daemonServer = daemonStub();
    dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { bin, evidence } = newFakeDir({
      gh: prsGhFake("[]"),
    });
    const path = isolatedPath(bin);
    const before = await spawnScript(
      ["negative", "pr-in-x", "before", "ACME-2"],
      path,
      evidence,
      env
    );
    expect(before.status).toBe(0);
    const runId = baselineRunId(before.stdout);
    const after = await spawnScript(
      ["negative", "pr-in-x", "after", runId, "ACME-2"],
      path,
      evidence,
      env
    );
    expect(after.status).not.toBe(0);
    expect(after.stderr).toContain("CHECK negative-pr-in-x-ACME-2: FAIL");
  });

  // A PR opened and closed in X, with the daemon's own record also showing
  // the change (pullRequest now names X), must never PASS — the operation proof alone is not
  // enough; the daemon side must be unaffected too.
  test("a closed PR exists in X, but the daemon's own pullRequest now names X too: FAIL", async () => {
    const { server, setBody } = mutableDaemonStub(stateBody); // ACME-2 starts as acme/widgets, no PR
    daemonServer = server;
    dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${server.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { bin, evidence } = newFakeDir({
      gh: prsGhFake("[]"),
    });
    const path = isolatedPath(bin);
    const before = await spawnScript(
      ["negative", "pr-in-x", "before", "ACME-2"],
      path,
      evidence,
      env
    );
    expect(before.status).toBe(0);
    const runId = baselineRunId(before.stdout);
    setBody(
      makeStateBody({
        issues: {
          "ACME-2": {
            key: "ACME-2",
            generation: 1,
            phase: "needs_review",
            status: "todo",
            repo: "acme/widgets",
            pullRequest: { repo: "other/outside", number: 5 },
          },
        },
      })
    );
    fakeBin(
      bin,
      "gh",
      prsGhFake(
        '[{"number":5,"head":{"ref":"legion/ACME-2"},"state":"closed","created_at":"2026-01-02T00:00:00Z","closed_at":"2026-01-02T01:00:00Z"}]'
      )
    );
    const after = await spawnScript(
      ["negative", "pr-in-x", "after", runId, "ACME-2"],
      path,
      evidence,
      env
    );
    expect(after.status).not.toBe(0);
    expect(after.stderr).toContain("CHECK negative-pr-in-x-ACME-2: FAIL");
  });

  test("a closed PR numbered above the baseline's cursor, daemon side unaffected: PASS", async () => {
    daemonServer = daemonStub(stateBody);
    dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { bin, evidence } = newFakeDir({
      gh: prsGhFake('[{"number":4,"head":{"ref":"legion/OTHER-1"},"state":"open"}]'),
    });
    const path = isolatedPath(bin);
    const before = await spawnScript(
      ["negative", "pr-in-x", "before", "ACME-2"],
      path,
      evidence,
      env
    );
    expect(before.status).toBe(0);
    const runId = baselineRunId(before.stdout);
    fakeBin(
      bin,
      "gh",
      prsGhFake(
        '[{"number":4,"head":{"ref":"legion/OTHER-1"},"state":"open"},{"number":5,"head":{"ref":"legion/ACME-2"},"state":"closed","created_at":"2026-01-02T00:00:00Z","closed_at":"2026-01-02T01:00:00Z"}]'
      )
    );
    const after = await spawnScript(
      ["negative", "pr-in-x", "after", runId, "ACME-2"],
      path,
      evidence,
      env
    );
    expect(after.status).toBe(0);
    expect(after.stdout).toContain("CHECK negative-pr-in-x-ACME-2: PASS");
  });

  // A generation bump between before and after (a hand-over or re-admission) is a
  // foreign baseline, refused outright rather than compared across two different issue lifetimes.
  test("the issue's generation moved since the baseline: refused as foreign", async () => {
    const { server, setBody } = mutableDaemonStub(stateBody);
    daemonServer = server;
    dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${server.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { bin, evidence } = newFakeDir({
      gh: prsGhFake("[]"),
    });
    const path = isolatedPath(bin);
    const before = await spawnScript(
      ["negative", "pr-in-x", "before", "ACME-2"],
      path,
      evidence,
      env
    );
    expect(before.status).toBe(0);
    const runId = baselineRunId(before.stdout);
    setBody(
      makeStateBody({
        issues: {
          "ACME-2": {
            key: "ACME-2",
            generation: 2,
            phase: "implementing",
            status: "todo",
            repo: "acme/widgets",
          },
        },
      })
    );
    const after = await spawnScript(
      ["negative", "pr-in-x", "after", runId, "ACME-2"],
      path,
      evidence,
      env
    );
    expect(after.status).not.toBe(0);
    expect(after.stderr).toContain("a hand-over or re-admission since");
  });

  test("two overlapping before calls for the same issue never collide: each gets its own run id and baseline", async () => {
    daemonServer = daemonStub();
    dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const gh = prsGhFake("[]");
    const { bin, evidence } = newFakeDir({ gh });
    const path = isolatedPath(bin);
    const [first, second] = await Promise.all([
      spawnScript(["negative", "pr-in-x", "before", "ACME-2"], path, evidence, env),
      spawnScript(["negative", "pr-in-x", "before", "ACME-2"], path, evidence, env),
    ]);
    expect(first.status).toBe(0);
    expect(second.status).toBe(0);
    expect(baselineRunId(first.stdout)).not.toBe(baselineRunId(second.stdout));
  });
});

describe("negative repo-edit: Dispatch's seq cursor and the exact kept-repository message", () => {
  test("before refuses when Dispatch already resolves to X", async () => {
    daemonServer = daemonStub();
    dispatchServer = dispatchStub({
      "ACME-2": JSON.stringify({ key: "ACME-2", resolved_repo: "other/outside" }),
    });
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { status, stderr } = await run(["negative", "repo-edit", "before", "ACME-2"], env);
    expect(status).not.toBe(0);
    expect(stderr).toContain("already resolves to X");
  });

  test("before on an unrecorded issue is refused", async () => {
    daemonServer = daemonStub();
    dispatchServer = dispatchStub({ "ACME-404": JSON.stringify({ key: "ACME-404" }) });
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { status, stderr } = await run(["negative", "repo-edit", "before", "ACME-404"], env);
    expect(status).not.toBe(0);
    expect(stderr).toContain("not the recorded child this control needs");
  });

  const xTargetedNote =
    "Legion keeps acme/widgets as this issue's repository; the change to other/outside in Dispatch is not applied — an issue's repository is fixed when Legion records it. Create a new issue for other/outside instead.";

  // The repository and the message both already reflect the edit, but the
  // only post-baseline event is the daemon's own routine status write (also issue.updated) — this
  // must never satisfy the seq-cursor check, which requires payload.repo == X on that event.
  test("the only issue.updated after the baseline is a routine status write (payload.repo unset): FAIL", async () => {
    daemonServer = daemonStub(stateBody); // ACME-2: acme/widgets
    dispatchServer = dispatchStub({
      "ACME-2": JSON.stringify({ key: "ACME-2", resolved_repo: "acme/widgets" }),
    });
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { bin, evidence } = newFakeDir();
    const path = isolatedPath(bin);
    const before = await spawnScript(
      ["negative", "repo-edit", "before", "ACME-2"],
      path,
      evidence,
      env
    );
    expect(before.status).toBe(0);
    const runId = baselineRunId(before.stdout);
    dispatchServer.stop(true);
    dispatchServer = dispatchStub(
      { "ACME-2": JSON.stringify({ key: "ACME-2", repo: "other/outside" }) },
      {
        "ACME-2": JSON.stringify([
          issueUpdatedEvent(20, null), // a routine status write: no repo in its payload
          daemonActorEvent("ACME", 19, `${xTargetedNote}\n\n<!-- legion-outbox:9 -->`),
        ]),
      }
    );
    const env2 = { ...env, DISPATCH_URL: `http://127.0.0.1:${dispatchServer.port}` };
    const after = await spawnScript(
      ["negative", "repo-edit", "after", runId, "ACME-2"],
      path,
      evidence,
      env2
    );
    expect(after.status).not.toBe(0);
    expect(after.stderr).toContain("CHECK negative-repo-edit-ACME-2: FAIL");
  });

  test("an issue.updated after the baseline naming X, with the exact kept-repository message from the daemon: PASS", async () => {
    daemonServer = daemonStub(stateBody);
    dispatchServer = dispatchStub({
      "ACME-2": JSON.stringify({ key: "ACME-2", resolved_repo: "acme/widgets" }),
    });
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { bin, evidence } = newFakeDir();
    const path = isolatedPath(bin);
    const before = await spawnScript(
      ["negative", "repo-edit", "before", "ACME-2"],
      path,
      evidence,
      env
    );
    expect(before.status).toBe(0);
    const runId = baselineRunId(before.stdout);
    dispatchServer.stop(true);
    dispatchServer = dispatchStub(
      { "ACME-2": JSON.stringify({ key: "ACME-2", repo: "other/outside" }) },
      {
        "ACME-2": JSON.stringify([
          issueUpdatedEvent(20, "other/outside"),
          daemonActorEvent("ACME", 19, `${xTargetedNote}\n\n<!-- legion-outbox:9 -->`),
        ]),
      }
    );
    const env2 = { ...env, DISPATCH_URL: `http://127.0.0.1:${dispatchServer.port}` };
    const after = await spawnScript(
      ["negative", "repo-edit", "after", runId, "ACME-2"],
      path,
      evidence,
      env2
    );
    expect(after.status).toBe(0);
    expect(after.stdout).toContain("CHECK negative-repo-edit-ACME-2: PASS");
  });

  // The operator spells X differently than Dispatch resolved and wrote it
  // (LEGION_E2E_REPO_X=Other/Outside here; Dispatch's own .repo and the daemon's note both read
  // "other/outside"). The comparisons above fold case; the note's own quoted spelling must
  // come from Dispatch's resolved spelling (dispatch_repo_now), never the operator's literal
  // LEGION_E2E_REPO_X, or this would wrongly FAIL on a mere case difference.
  test("the operator's case for X differs from Dispatch's own resolved spelling: still PASS", async () => {
    daemonServer = daemonStub(stateBody);
    dispatchServer = dispatchStub({
      "ACME-2": JSON.stringify({ key: "ACME-2", resolved_repo: "acme/widgets" }),
    });
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    env.LEGION_E2E_REPO_X = "Other/Outside";
    const { bin, evidence } = newFakeDir();
    const path = isolatedPath(bin);
    const before = await spawnScript(
      ["negative", "repo-edit", "before", "ACME-2"],
      path,
      evidence,
      env
    );
    expect(before.status).toBe(0);
    const runId = baselineRunId(before.stdout);
    dispatchServer.stop(true);
    dispatchServer = dispatchStub(
      { "ACME-2": JSON.stringify({ key: "ACME-2", repo: "other/outside" }) },
      {
        "ACME-2": JSON.stringify([
          issueUpdatedEvent(20, "other/outside"),
          daemonActorEvent("ACME", 19, `${xTargetedNote}\n\n<!-- legion-outbox:9 -->`),
        ]),
      }
    );
    const env2 = { ...env, DISPATCH_URL: `http://127.0.0.1:${dispatchServer.port}` };
    const after = await spawnScript(
      ["negative", "repo-edit", "after", runId, "ACME-2"],
      path,
      evidence,
      env2
    );
    expect(after.status).toBe(0);
    expect(after.stdout).toContain("CHECK negative-repo-edit-ACME-2: PASS");
  });

  // noteOnce posts the kept-repository note only once per resolution: a rerun that has no fresh
  // message must still PASS as long as the seq-bound issue.updated and the daemon's repo are
  // right, since the message check only verifies the format, never requiring a brand-new one.
  test("no fresh message after the baseline, but the seq-bound issue.updated and an already-present matching note both exist: PASS", async () => {
    daemonServer = daemonStub(stateBody);
    dispatchServer = dispatchStub({
      "ACME-2": JSON.stringify({ key: "ACME-2", resolved_repo: "acme/widgets" }),
    });
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { bin, evidence } = newFakeDir();
    const path = isolatedPath(bin);
    const before = await spawnScript(
      ["negative", "repo-edit", "before", "ACME-2"],
      path,
      evidence,
      env
    );
    expect(before.status).toBe(0);
    const runId = baselineRunId(before.stdout);
    dispatchServer.stop(true);
    dispatchServer = dispatchStub(
      { "ACME-2": JSON.stringify({ key: "ACME-2", repo: "other/outside" }) },
      {
        "ACME-2": JSON.stringify([
          issueUpdatedEvent(20, "other/outside"),
          daemonActorEvent("ACME", 3, `${xTargetedNote}\n\n<!-- legion-outbox:1 -->`), // an older message, before the baseline's own last_seq
        ]),
      }
    );
    const env2 = { ...env, DISPATCH_URL: `http://127.0.0.1:${dispatchServer.port}` };
    const after = await spawnScript(
      ["negative", "repo-edit", "after", runId, "ACME-2"],
      path,
      evidence,
      env2
    );
    expect(after.status).toBe(0);
    expect(after.stdout).toContain("CHECK negative-repo-edit-ACME-2: PASS");
  });
});
