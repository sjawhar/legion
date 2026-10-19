import { afterEach, describe, expect, test } from "bun:test";
import { mkdirSync, readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import {
  bashPath,
  closeServers,
  daemonStub,
  dir,
  dispatchStub,
  fullInputs,
  isolatedPath,
  makeStateBody,
  mutableDaemonStub,
  newFakeDir,
  nextRun,
  run,
  script,
  servers,
  spawnScript,
  stateBody,
} from "./multi-repo-acceptance-harness";

// scripts/e2e/multi-repo-acceptance.sh is the observer for the live multi-repository acceptance
// run; this file covers every observation but second-owner (multi-repo-second-owner.test.ts) and
// the `negative` ones (multi-repo-negative.test.ts). The stubs and fakes it runs against are in
// multi-repo-acceptance-harness.ts.
afterEach(() => {
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
    const daemonServer = servers.track(
      Bun.serve({ port: 0, fetch: () => new Response("not found", { status: 404 }) })
    );
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
    const daemonServer = daemonStub();
    const dispatchServer = dispatchStub({
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
    const daemonServer = daemonStub();
    const dispatchServer = dispatchStub({
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
    const daemonServer = daemonStub();
    const dispatchServer = dispatchStub({
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
    const daemonServer = daemonStub();
    const dispatchServer = dispatchStub({
      "ACME-2": JSON.stringify({ key: "ACME-2", status: "todo" }),
    });
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
    const daemonServer = daemonStub();
    const dispatchServer = dispatchStub({});
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
    const daemonServer = servers.track(
      Bun.serve({
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
      })
    );
    const dispatchServer = dispatchStub({
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
    const daemonServer = servers.track(
      Bun.serve({
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
      })
    );
    const dispatchServer = dispatchStub({
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
    const { server: daemonServer, setBody } = mutableDaemonStub(
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
    const dispatchServer = dispatchStub({
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
    const daemonServer = servers.track(
      Bun.serve({
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
      })
    );
    const dispatchServer = dispatchStub({
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
    const daemonServer = daemonStub();
    const dispatchServer = dispatchStub({});
    const env = fullInputs(
      `http://127.0.0.1:${daemonServer.port}`,
      `http://127.0.0.1:${dispatchServer.port}`
    );
    const { status, stderr } = await run(["record-head", "acme/gadgets"], env, {});
    expect(status).not.toBe(0);
    expect(stderr).toContain("not LEGION_E2E_REPO_T");
  });

  test("an empty commit sha from GitHub is never recorded as the head: FAIL, not PASS", async () => {
    const daemonServer = daemonStub();
    const dispatchServer = dispatchStub({});
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
    const daemonServer = daemonStub();
    const dispatchServer = dispatchStub({});
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
    const daemonServer = daemonStub();
    const dispatchServer = dispatchStub({});
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
    const daemonServer = daemonStub();
    const dispatchServer = dispatchStub({});
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
    const daemonServer = daemonStub();
    const dispatchServer = dispatchStub({});
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
    const daemonServer = daemonStub();
    const dispatchServer = dispatchStub({});
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
    const daemonServer = daemonStub();
    const dispatchServer = dispatchStub({});
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
    const daemonServer = daemonStub();
    const dispatchServer = dispatchStub({});
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
    const daemonServer = daemonStub(stateWithCache);
    const dispatchServer = dispatchStub({});
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
    const daemonServer = daemonStub(stateWithCache);
    const dispatchServer = dispatchStub({});
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
    const daemonServer = daemonStub(stateWithCache);
    const dispatchServer = dispatchStub({});
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
    const daemonServer = daemonStub(stateWithCache);
    const dispatchServer = dispatchStub({});
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
