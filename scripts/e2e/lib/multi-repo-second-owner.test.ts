import { afterEach, describe, expect, test } from "bun:test";
import { createHash } from "node:crypto";
import {
  existsSync,
  mkdirSync,
  mkdtempSync,
  readdirSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { createServer } from "node:http";
import { tmpdir } from "node:os";
import { join } from "node:path";
import {
  closeServers,
  daemonStub,
  dir,
  dispatchStub,
  fakeBin,
  fakeRoleGh,
  fakeRoleLegion,
  fullInputs,
  imageBin,
  imageEnv,
  makeStateBody,
  newTrustedRoot,
  newWork,
  nextRun,
  OWNERS_DIR,
  realKubectl,
  recordedCalls,
  run,
  servers,
} from "./multi-repo-acceptance-harness";
import {
  type FakeProcTreeOptions,
  fakeProcTree,
  LEGION_BINARY,
  writeProcEntry,
} from "./multi-repo-proc-tree";

// second-owner proves, from inside the issue's pod and as the role's own agent process, that the
// role's GitHub credential reads T, can push to O (the positive control) and cannot push to T.
// scripts/e2e/lib/as-role.py runs unmodified under `python3 -I -` over the fake kubectl, against a
// fake /proc tree. It runs no git: it asks the role's own `legion git-credential get` for each
// repository's credential and sends GitHub the request a `git push` opens with
// (`GET <repo>.git/info/refs?service=git-receive-pack`) itself, here to a local stub that stands
// in for github.com through MULTIREPO_GITHUB_URL. The probes run the image's own `legion` (the
// credential helper) and `gh` (the read of T) by absolute path under a trusted root, as a pod's
// `/opt/legion/bin/...`; the agent's PATH carries only a fake `git` that records every call and
// fails, so a run that reached git would show.

afterEach(() => {
  closeServers();
});

const T = "acme/widgets";
const O = "zeta/tools";
const ADVERTISEMENT_TYPE = "application/x-git-receive-pack-advertisement";

interface GithubRequest {
  repo: string;
  authorization: string;
  query: string;
}

interface GithubAnswer {
  status: number;
  type?: string;
  body?: string;
}

// A handler answers one request, or returns "drop" to destroy the connection without a reply.
type GithubHandler = (request: GithubRequest) => GithubAnswer | "drop";

const advertisement: GithubAnswer = {
  status: 200,
  type: ADVERTISEMENT_TYPE,
  body: "001e# service=git-receive-pack\n0000",
};
const writeRefused: GithubAnswer = {
  status: 403,
  type: "text/plain",
  body: "Write access to repository not granted.",
};

// The token the fake `legion git-credential get` prints for a repository.
const tokenFor = (repo: string) => `TOKEN-${repo.replace("/", "_")}`;

const passwordOf = (authorization: string) =>
  Buffer.from(authorization.replace(/^Basic /, ""), "base64")
    .toString("utf8")
    .split(":")
    .slice(1)
    .join(":");

// By default O's repository accepts the push request and T's refuses it as a read-only token's
// does; `answers` overrides either.
const answers =
  (override: { o?: GithubAnswer | "drop"; t?: GithubAnswer | "drop" }): GithubHandler =>
  ({ repo }) =>
    (repo === O ? (override.o ?? advertisement) : (override.t ?? writeRefused)) as
      | GithubAnswer
      | "drop";

// githubStub stands in for github.com: it answers `GET /<owner>/<name>.git/info/refs` through
// `handler`, and records a request that is not the receive-pack advertisement request, or is not
// sent with that repository's own token as the Basic password, in `unexpected` and answers it 500.
interface GithubStub {
  url: string;
  requests: GithubRequest[];
  unexpected: string[];
}

async function githubStub(handler: GithubHandler = answers({})): Promise<GithubStub> {
  const requests: GithubRequest[] = [];
  const unexpected: string[] = [];
  const server = createServer((req, res) => {
    const url = new URL(req.url ?? "/", "http://stub");
    const match = url.pathname.match(/^\/([^/]+\/[^/]+)\.git\/info\/refs$/);
    if (!match || req.method !== "GET") {
      unexpected.push(`${req.method} ${req.url}`);
      res.writeHead(404).end("not found");
      return;
    }
    const request = {
      repo: match[1] ?? "",
      authorization: req.headers.authorization ?? "",
      query: url.search.slice(1),
    };
    requests.push(request);
    if (request.query !== "service=git-receive-pack") {
      unexpected.push(`${request.repo}: query ${request.query}`);
    }
    if (passwordOf(request.authorization) !== tokenFor(request.repo)) {
      unexpected.push(`${request.repo}: authorization is not that repository's own token`);
    }
    if (unexpected.length > 0) {
      res.writeHead(500).end("stub: unexpected request");
      return;
    }
    const answer = handler(request);
    if (answer === "drop") {
      req.socket.destroy();
      return;
    }
    res.writeHead(answer.status, { "content-type": answer.type ?? "text/plain" });
    res.end(answer.body ?? "");
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  servers.trackCloser(() => {
    server.close();
    server.closeAllConnections();
  });
  const address = server.address();
  const port = typeof address === "object" && address ? address.port : 0;
  return { url: `http://127.0.0.1:${port}`, requests, unexpected };
}

const stateWithPR = (phase: string) =>
  makeStateBody({
    issues: {
      "ACME-2": {
        key: "ACME-2",
        generation: 1,
        phase,
        status: "todo",
        repo: O,
        pullRequest: { repo: O, number: 7 },
      },
    },
  });

const goodGh = `
case "$*" in
  "api repos/zeta/tools/pulls/7 --jq .merged") printf 'true' ;;
  *) echo "gh: unexpected call: $*" >&2; exit 1 ;;
esac`;

const podLabelKubectl = `
case "$*" in
  "get pod the-pod -o jsonpath={.metadata.labels.legion\\.dev/issue}")
    printf 'ACME-2'
    ;;
  *)
    ${realKubectl}
    ;;
esac`;

// The agent's own PATH carries only the git recorder: the probes never run it.
function roleBin(): string {
  const bin = join(newWork(), "role-bin");
  mkdirSync(bin, { recursive: true });
  return bin;
}

// git is never run by the probes: this one records its arguments and fails.
function fakeRoleGit(bin: string, record: string): void {
  fakeBin(bin, "git", `printf '%s\\n' "$*" >>'${record}'\nexit 1`);
}

function roleEnviron(bin: string, home: string, pathPrefix?: string): Record<string, string> {
  return {
    PATH: `${pathPrefix ? `${pathPrefix}:` : ""}${bin}:/usr/bin:/bin`,
    HOME: home,
    POD_UID: "test-pod-uid",
    LEGION_GH_PATH: "/usr/local/bin/gh",
    LEGION_GH_OWNERS_DIR: OWNERS_DIR,
    GH_CONFIG_DIR: `${OWNERS_DIR}/zeta`,
    LEGION_WORKSPACE: "/workspace",
  };
}

interface Scenario {
  phase?: string;
  github?: GithubHandler;
  silent?: string[];
  readOutput?: string;
  environ?: Record<string, string>;
  // A directory put first on the agent's PATH.
  agentPathPrefix?: string;
  tree?: FakeProcTreeOptions;
  // Builds the proc tree itself, from the role's environment.
  buildTree?: (environ: Record<string, string>) => string;
  prepareHome?: (home: string) => void;
  cwd?: string;
  hostEnv?: Record<string, string>;
}

// scenario runs second-owner against the stubs and the role's fakes; MULTIREPO_PROC_ROOT and
// MULTIREPO_GITHUB_URL are accepted only beside the test-seam marker the run sets.
async function scenario(options: Scenario = {}): Promise<Outcome> {
  const daemon = daemonStub(stateWithPR(options.phase ?? "production_check"));
  const dispatch = dispatchStub({});
  const env = fullInputs(`http://127.0.0.1:${daemon.port}`, `http://127.0.0.1:${dispatch.port}`);
  const github = await githubStub(options.github);
  const bin = roleBin();
  const root = newTrustedRoot();
  const records = newWork();
  mkdirSync(records, { recursive: true });
  const legionRecord = join(records, "legion-calls");
  const ghRecord = join(records, "gh-calls");
  const gitRecord = join(records, "git-calls");
  fakeRoleGh(imageBin(root), T, options.readOutput ?? T, ghRecord);
  fakeRoleLegion(imageBin(root), options.silent ?? [], legionRecord);
  fakeRoleGit(bin, gitRecord);
  const home = join(newWork(), "home");
  mkdirSync(home, { recursive: true });
  options.prepareHome?.(home);
  const environ = { ...roleEnviron(bin, home, options.agentPathPrefix), ...options.environ };
  const procRoot = options.buildTree
    ? options.buildTree(environ)
    : fakeProcTree(environ, options.tree);
  const result = await run(
    ["second-owner", "ACME-2", "the-pod"],
    {
      ...env,
      ...options.hostEnv,
      MULTIREPO_PROC_ROOT: procRoot,
      MULTIREPO_TRUSTED_ROOT: root,
      MULTIREPO_TEST_SEAM: "1",
      MULTIREPO_GITHUB_URL: github.url,
    },
    { gh: goodGh, kubectl: podLabelKubectl },
    options.cwd
  );
  expect(github.unexpected).toEqual([]);
  const probePath = join(result.evidence, "second-owner-probe.raw");
  return {
    ...result,
    github,
    procRoot,
    root,
    gitRecord,
    legionRecord,
    ghRecord,
    probe: () => JSON.parse(readFileSync(probePath, "utf8")),
    summary: () => readFileSync(join(result.evidence, "second-owner.txt"), "utf8"),
  };
}

interface ProbeResult {
  status: number;
  content_type: string;
  output: string;
}

interface ProbeAnswer {
  pid: string;
  env_match: boolean;
  read: { status: number; output: string };
  push_o: ProbeResult;
  push_t: ProbeResult;
}

interface Outcome {
  status: number;
  stdout: string;
  stderr: string;
  evidence: string;
  github: GithubStub;
  procRoot: string;
  root: string;
  gitRecord: string;
  legionRecord: string;
  ghRecord: string;
  probe: () => ProbeAnswer;
  summary: () => string;
}

function expectPass(result: Outcome): void {
  expect(result.stderr).not.toContain("CHECK second-owner: FAIL");
  expect(result.stdout).toContain("CHECK second-owner: PASS");
  expect(result.status).toBe(0);
}

// A verdict FAIL, reached through the summary: the probe ran and its evidence was written.
function expectVerdictFail(result: Outcome): void {
  expect(result.status).not.toBe(0);
  expect(result.stderr).toContain("CHECK second-owner: FAIL");
  expect(existsSync(join(result.evidence, "second-owner.txt"))).toBe(true);
}

function expectGitNeverRun(result: Outcome): void {
  expect(existsSync(result.gitRecord)).toBe(false);
}

// cmdlineSha256 mirrors how writeProcEntry writes a /proc/<pid>/cmdline file (NUL-joined argv,
// trailing NUL, UTF-8 bytes): the digest the prober computes over the whole file.
function cmdlineSha256(argv: string[]): string {
  return createHash("sha256")
    .update(Buffer.from(`${argv.join("\0")}\0`, "utf8"))
    .digest("hex");
}

describe("second-owner: the role's credential, resolved once, with a positive push control", () => {
  test("O's push request succeeds and T's is refused: PASS, with the role, both answers and no git call recorded", async () => {
    const result = await scenario();
    expectPass(result);
    expectGitNeverRun(result);
    // Exactly the two requests a push opens with, each sent with its own repository's token.
    expect(result.github.requests.map((request) => request.repo)).toEqual([O, T]);
    for (const request of result.github.requests) {
      expect(request.query).toBe("service=git-receive-pack");
      expect(passwordOf(request.authorization)).toBe(tokenFor(request.repo));
    }
    const written = result.summary();
    expect(written).toContain(
      `positive control: GET ${O}.git/info/refs?service=git-receive-pack as role: HTTP 200, ${ADVERTISEMENT_TYPE}`
    );
    expect(written).toContain(`negative probe: the same request for ${T} as role: HTTP 403`);
    expect(written).toContain("---\nWrite access to repository not granted.");
    // The proc root this run scanned is in the evidence, so a live run's reader can see it was
    // really /proc (or, in a test, the fake tree it was told to scan).
    expect(written).toContain(`proc root: ${result.procRoot}`);
    // The resolved pid (9, the agent: the shim's own child) and its cmdline digest are recorded,
    // never the cmdline's own values.
    expect(written).toContain("role pid: 9");
    expect(written).toContain(
      `role cmdline: argv0=omp argc=3 sha256=${cmdlineSha256(["omp", "--mode", "rpc"])}`
    );
    expect(result.probe()).toMatchObject({
      pid: "9",
      env_match: true,
      read: { status: 0, output: T },
      push_o: { status: 200, content_type: ADVERTISEMENT_TYPE, output: "" },
      push_t: { status: 403, content_type: "text/plain" },
    });
  });

  for (const [name, body] of [
    ["a user the repository does not grant", `Permission to ${T}.git denied to zeta-tools-app.`],
    ["the same refusal in another case", "WRITE ACCESS to repository not granted."],
  ] as const) {
    test(`T's push refused with ${name}: PASS`, async () => {
      const result = await scenario({
        github: answers({ t: { status: 403, type: "text/plain", body } }),
      });
      expectPass(result);
      expect(result.summary()).toContain(body);
    });
  }

  test("a pre-merge phase (awaiting_merge) is a FAIL even with a merged PR and valid credentials", async () => {
    const result = await scenario({ phase: "awaiting_merge" });
    expectVerdictFail(result);
  });

  describe("a verdict FAIL, never a PASS, for each way the probes can go wrong", () => {
    const cases: Array<[string, Scenario, ((result: Outcome) => void)?]> = [
      [
        "T answers the receive-pack advertisement (the token can write)",
        { github: answers({ t: advertisement }) },
      ],
      [
        "T's 403 is another repository's refusal",
        {
          github: answers({
            t: {
              status: 403,
              type: "text/plain",
              body: "Permission to other/outside.git denied to bot.",
            },
          }),
        },
      ],
      [
        "T answers 404",
        { github: answers({ t: { status: 404, type: "text/plain", body: "Not Found" } }) },
      ],
      [
        "T answers 404 with the refusal's own text",
        { github: answers({ t: { ...writeRefused, status: 404 } }) },
      ],
      [
        "T's 403 body has two lines",
        {
          github: answers({
            t: {
              status: 403,
              type: "text/plain",
              body: "Write access to repository not granted.\nPermission to other/outside.git denied to bot.",
            },
          }),
        },
      ],
      [
        "T is unreachable (the connection is destroyed)",
        { github: answers({ t: "drop" }) },
        (result) => {
          expect(result.probe().push_t.status).toBe(0);
          expect(result.summary()).toContain(
            "negative probe: the same request for acme/widgets as role: HTTP 0"
          );
        },
      ],
      [
        "O's push is refused (the positive control fails)",
        { github: answers({ o: writeRefused }) },
      ],
      [
        "O answers 200 as text/plain, not the advertisement",
        { github: answers({ o: { status: 200, type: "text/plain", body: "ok" } }) },
      ],
      [
        "the role's credential helper prints nothing for T",
        { silent: [T] },
        (result) => {
          expect(result.probe().push_t).toMatchObject({ status: 0 });
          expect(result.probe().push_t.output).toContain(`answered nothing for ${T}`);
          expect(result.github.requests.map((request) => request.repo)).toEqual([O]);
        },
      ],
      [
        "the role's credential helper prints nothing for O",
        { silent: [O] },
        (result) => {
          expect(result.probe().push_o).toMatchObject({ status: 0 });
          expect(result.probe().push_o.output).toContain(`answered nothing for ${O}`);
          expect(result.github.requests.map((request) => request.repo)).toEqual([T]);
        },
      ],
      ["the role's read of T names another repository", { readOutput: "other/outside" }],
      ...[
        ["LEGION_GH_OWNERS_DIR names another directory", { LEGION_GH_OWNERS_DIR: "/elsewhere/gh" }],
        [
          "GH_CONFIG_DIR does not name the issue's own owner directory",
          { GH_CONFIG_DIR: `${OWNERS_DIR}/other-owner` },
        ],
        ["LEGION_GH_PATH names another path", { LEGION_GH_PATH: "/opt/legion/bin/gh" }],
      ].map(
        ([name, environ]) =>
          [
            `${name as string}: the environment does not match the image's`,
            { environ: environ as Record<string, string> },
            (result: Outcome) => expect(result.probe().env_match).toBe(false),
          ] as [string, Scenario, (result: Outcome) => void]
      ),
    ];
    for (const [name, options, extra] of cases) {
      test(`${name}: FAIL`, async () => {
        const result = await scenario(options);
        expectVerdictFail(result);
        extra?.(result);
      });
    }
  });

  // git reads ~/.netrc, and urllib does not; the probes send the credential the role's own helper
  // prints for each repository and nothing else. Here the agent's HOME carries O's token for
  // github.com while T's own token is the one GitHub lets push: a prober that used the netrc would
  // send O's token for T and see the refusal. It sends T's own, so the verdict is FAIL.
  test("a ~/.netrc carrying O's credential never replaces T's own: T's token can push, so FAIL", async () => {
    const result = await scenario({
      github: ({ repo, authorization }) =>
        repo === O || passwordOf(authorization) === tokenFor(T) ? advertisement : writeRefused,
      prepareHome: (home) =>
        writeFileSync(
          join(home, ".netrc"),
          `machine github.com login x-access-token password ${tokenFor(O)}\n`
        ),
    });
    expectVerdictFail(result);
    expect(result.probe().push_t.status).toBe(200);
    expect(result.github.requests.map((request) => passwordOf(request.authorization))).toEqual([
      tokenFor(O),
      tokenFor(T),
    ]);
  });

  // A ~/.gitconfig the agent writes (a url.<x>.insteadOf redirecting github.com) and any other
  // git setting are beside the point: no git runs.
  test("a planted ~/.gitconfig with an insteadOf cannot change the verdict, and git is never called", async () => {
    const result = await scenario({
      prepareHome: (home) =>
        writeFileSync(
          join(home, ".gitconfig"),
          '[url "https://attacker.example/"]\n\tinsteadOf = https://github.com/\n'
        ),
    });
    expectPass(result);
    expectGitNeverRun(result);
    expect(result.github.requests.map((request) => request.repo)).toEqual([O, T]);
  });

  // The prober runs the image's own `legion` and `gh` by absolute path, with constants of its own
  // and none of the agent's environment: the agent's code can rewrite its own /proc/<pid>/environ
  // in place, so nothing read from there may choose a binary, a search path or a credential
  // directory.
  describe("the image's own binaries run under the image's own constants", () => {
    test("a legion and a gh planted first on the agent's PATH are never run: the honest verdict is FAIL", async () => {
      const planted = join(newWork(), "planted");
      mkdirSync(planted, { recursive: true });
      const marker = join(newWork(), "planted-ran");
      const record = `echo "$0" >>'${marker}'`;
      // These would make every probe pass: O's token for any repository, and T's name for any read.
      fakeBin(
        planted,
        "legion",
        `${record}\nprintf 'username=x-access-token\\npassword=${tokenFor(O)}\\n'`
      );
      fakeBin(planted, "gh", `${record}\nprintf '${T}'`);
      const result = await scenario({
        agentPathPrefix: planted,
        readOutput: "other/outside",
        github: answers({ t: advertisement }),
      });
      expectVerdictFail(result);
      expect(existsSync(marker)).toBe(false);
      expect(result.probe().push_t.status).toBe(200);
      expect(result.probe().read.output).toBe("other/outside");
    });

    test("the helper and gh get exactly the image's constants, and nothing of the agent's environment", async () => {
      const result = await scenario({
        environ: {
          GIT_ASKPASS: "/agent/askpass",
          GIT_SSH_COMMAND: "agent-ssh",
          SSL_CERT_FILE: "/agent/ca.pem",
          GH_TOKEN: "agent-token",
        },
      });
      expectPass(result);
      const legion = recordedCalls(result.legionRecord);
      expect(legion.args).toEqual(["git-credential get", "git-credential get"]);
      expect(legion.envs).toEqual([imageEnv(result.root), imageEnv(result.root)]);
      const gh = recordedCalls(result.ghRecord);
      expect(gh.args).toEqual([`api repos/${T} --jq .full_name`]);
      expect(gh.envs).toEqual([imageEnv(result.root)]);
    });

    test("only the role's proxy variables are passed on, when it has them", async () => {
      const proxies = {
        https_proxy: "http://127.0.0.1:9",
        HTTPS_PROXY: "http://127.0.0.1:9",
        no_proxy: "internal.example",
        NO_PROXY: "internal.example",
      };
      const result = await scenario({
        environ: { ...proxies, http_proxy: "http://agent.example" },
      });
      expectPass(result);
      const expected = { ...imageEnv(result.root), ...proxies };
      expect(recordedCalls(result.legionRecord).envs).toEqual([expected, expected]);
      expect(recordedCalls(result.ghRecord).envs).toEqual([expected]);
    });
  });

  describe("the worker shim and the agent are found without guessing", () => {
    test("no process whose parent is worker-shim (the bare pod shell): FAIL", async () => {
      const result = await scenario({ tree: { shimPids: [] } });
      expect(result.status).not.toBe(0);
      expect(result.stderr).toContain(
        "second-owner: the credential probes in the-pod (implementer): failed"
      );
      expect(result.stderr).toContain(
        "expected exactly one worker-shim process whose own parent is PID 1, found 0"
      );
    });

    // Two processes both carrying the shim's own markers must refuse, naming both candidates,
    // never pick the lexically first one.
    test("two worker-shim processes: refused, naming both candidates", async () => {
      const result = await scenario({ tree: { shimPids: ["7", "20"] } });
      expect(result.status).not.toBe(0);
      expect(result.stderr).toContain(
        "expected exactly one worker-shim process whose own parent is PID 1, found 2"
      );
      expect(result.stderr).toContain("7");
      expect(result.stderr).toContain("20");
    });

    // The shim is identified by its parent being PID 1, not by its cmdline alone.
    test("a worker-shim-cmdline process whose own parent is not PID 1 is never the shim: FAIL", async () => {
      const result = await scenario({ tree: { shimParent: "5" } });
      expect(result.status).not.toBe(0);
      expect(result.stderr).toContain(
        "expected exactly one worker-shim process whose own parent is PID 1, found 0"
      );
    });

    // The shim is also identified by the image's own binary: a process the agent names or copies
    // itself, whatever it is called and wherever it was started, is no shim.
    for (const [name, shimExe] of [
      ["an exe symlink to another path", "/tmp/legion"],
      ["no exe symlink at all", null],
    ] as const) {
      test(`a worker-shim-cmdline process with ${name} is never the shim: refused`, async () => {
        const result = await scenario({ tree: { shimExe } });
        expect(result.status).not.toBe(0);
        expect(result.stderr).toContain(
          "expected exactly one worker-shim process whose own parent is PID 1, found 0"
        );
        expect(result.github.requests).toEqual([]);
      });
    }

    // A broker-enrolled pod's shim also parents an agent-secrets renew process for its whole life,
    // and a codegraph warm-up, so "exactly one child of worker-shim" would refuse every such pod.
    // Matching the shim's own argv after its "--" picks the agent out from among them.
    test("a shim with an agent-secrets renew and a codegraph child still resolves its one agent: PASS", async () => {
      const result = await scenario({ tree: { extraShimChildren: true } });
      expectPass(result);
    });

    // A supported plugin-reinstall launch wraps the shim's post-"--" argv in installArgv
    // (internal/runtime/sandbox/plugins.go), `/bin/sh -c <script> legion-plugins <agent argv>`,
    // whose final `exec "$@"` replaces that process's image with the real agent: the shim's own
    // recorded argv never changes, but the agent's real /proc/<pid>/cmdline becomes just
    // `<agent argv>` again. This spawns a real process through exactly that shape and copies its
    // real post-exec /proc/<pid>/cmdline bytes into the fake tree, so the match is checked
    // against genuine kernel-produced argv.
    test("an install-wrapper exec transition: the agent's real post-exec argv still resolves: PASS", async () => {
      const realProc = Bun.spawn(["sh", "-c", 'exec "$@"', "legion-plugins", "sleep", "999"], {
        stdout: "ignore",
        stderr: "ignore",
      });
      let postExecCmdline: Buffer | undefined;
      // Real wait: the exec is a real kernel transition on a real pid, not an in-process timer.
      for (let i = 0; i < 500; i++) {
        try {
          const buf = await Bun.file(`/proc/${realProc.pid}/cmdline`).arrayBuffer();
          if (Buffer.from(buf).equals(Buffer.from("sleep\x00999\x00"))) {
            postExecCmdline = Buffer.from(buf);
            break;
          }
        } catch {
          /* the process may not be visible yet */
        }
        await Bun.sleep(20);
      }
      try {
        expect(postExecCmdline).toEqual(Buffer.from("sleep\x00999\x00"));
        const result = await scenario({
          buildTree: (environ) => {
            const root = join(dir, `proc-${nextRun()}`);
            mkdirSync(root, { recursive: true });
            writeProcEntry(root, "1", { ppid: "0", cmdline: ["launcher"] });
            writeProcEntry(root, "7", {
              ppid: "1",
              cmdline: [
                "legion",
                "worker-shim",
                "--connect",
                "tcp://x:1",
                "--",
                "/bin/sh",
                "-c",
                'set -eu\n{ echo installing; } 1>&2\nexec "$@"\n',
                "legion-plugins",
                "sleep",
                "999",
              ],
              environ,
              exe: LEGION_BINARY,
            });
            const agentDir = join(root, "9");
            mkdirSync(agentDir, { recursive: true });
            writeFileSync(join(agentDir, "status"), "Name:\tproc9\nPPid:\t7\n");
            writeFileSync(join(agentDir, "cmdline"), postExecCmdline as Buffer);
            const pairs = Object.entries(environ).map(([k, v]) => `${k}=${v}`);
            writeFileSync(join(agentDir, "environ"), `${pairs.join("\0")}\0`);
            writeProcEntry(root, "10", { ppid: "9", cmdline: ["bash", "-c", "sleep 1"] });
            writeProcEntry(root, "15", { ppid: "9", cmdline: ["zombie"], environ: {} });
            return root;
          },
        });
        expectPass(result);
      } finally {
        realProc.kill();
      }
    });

    // Two children of the shim whose own argv both equal its post-"--" argv (a stale incarnation
    // briefly overlapping a fresh one) refuse by name, never picking either arbitrarily.
    test("two children of worker-shim matching its own argv: refused, naming both", async () => {
      const result = await scenario({ tree: { duplicateAgent: true } });
      expect(result.status).not.toBe(0);
      expect(result.stderr).toContain(
        "expected exactly one child of worker-shim 7 matching its own argv (directly, or through the install wrapper's exec), found 2"
      );
      expect(result.stderr).toContain("9");
      expect(result.stderr).toContain("12");
    });
  });

  describe("nothing of the agent's cmdline or environment reaches evidence", () => {
    // A pod agent's real cmdline carries --append-system-prompt <the whole composed prompt,
    // deployment instructions included>. The evidence holds only the pid, argv[0], the argument
    // count, and a sha256 of the whole /proc/<pid>/cmdline file, from one parse that reads the
    // file once and cannot restart mid-argv.
    test("the agent's cmdline never reaches evidence as values, only pid/argv0/argc/sha256: PASS", async () => {
      const marker = "DEPLOYMENT-INSTRUCTIONS-MARKER-do-not-leak-this-host.internal.example";
      const prompt = `# Role prompt\n\nAddressing fragment.\n\n${marker}\n`;
      const agentArgv = ["omp", "--mode", "rpc", "--append-system-prompt", prompt];
      const result = await scenario({ tree: { agentArgv } });
      expectPass(result);
      const written = result.summary();
      expect(written).not.toContain(marker);
      expect(written).not.toContain(prompt);
      expect(written).not.toContain("--append-system-prompt");
      expect(written).toContain(
        `role cmdline: argv0=omp argc=5 sha256=${cmdlineSha256(agentArgv)}`
      );
    });

    // A prompt just under the runtime's 131,072-byte argument bound
    // (packages/daemon/internal/runtime/sandbox/manifest.go) would make a line-batching redactor
    // (xargs) lose its "previous argument was --append-system-prompt" state across invocations.
    // The parser reads the whole file once; a near-limit prompt proves it never restarts.
    test("a ~130 KB prompt, near the runtime's own argument bound, is never in evidence: PASS", async () => {
      const marker = "DEPLOYMENT-INSTRUCTIONS-MARKER-must-not-leak";
      const prompt = marker + "-".repeat(130400 - marker.length);
      expect(prompt.length).toBe(130400);
      const agentArgv = ["omp", "--mode", "rpc", "--append-system-prompt", prompt];
      const result = await scenario({ tree: { agentArgv } });
      expectPass(result);
      const written = result.summary();
      expect(written).not.toContain(marker);
      expect(written).not.toContain(prompt);
      expect(written).toContain(
        `role cmdline: argv0=omp argc=5 sha256=${cmdlineSha256(agentArgv)}`
      );
    });

    // Every evidence file this observation wrote (summary and raw alike) is searched for the
    // prompt, instruction and environment markers and for the credentials' own tokens: GitHub's
    // refusal here even echoes the token back, and the prober must not keep it.
    test("no evidence file carries the prompt marker, the instruction marker, the environment values or a token", async () => {
      const promptMarker = "PROMPT-MARKER-xyz";
      const instructionMarker = "DEPLOYMENT-INSTRUCTIONS-MARKER-xyz";
      const resumeMarker = "/var/run/legion/sessions/resume-marker-xyz.json";
      const prompt = `${promptMarker}\n${instructionMarker}\n`;
      const agentArgv = [
        "omp",
        "--mode",
        "rpc",
        "--resume",
        resumeMarker,
        "--append-system-prompt",
        prompt,
      ];
      const environMarker = "/agent/environment-marker-xyz";
      const result = await scenario({
        tree: { agentArgv },
        environ: { GIT_ASKPASS: environMarker },
        github: answers({
          t: {
            status: 403,
            type: "text/plain",
            body: `Permission to ${T}.git denied to ${tokenFor(T)}.`,
          },
        }),
      });
      expectPass(result);
      expect(result.summary()).toContain("Permission to acme/widgets.git denied to <token>.");
      for (const name of readdirSync(result.evidence)) {
        const content = readFileSync(join(result.evidence, name), "utf8");
        for (const marker of [
          promptMarker,
          instructionMarker,
          resumeMarker,
          environMarker,
          tokenFor(O),
          tokenFor(T),
          "TOKEN-",
        ]) {
          expect(content, `${name} holds ${marker}`).not.toContain(marker);
        }
      }
    });
  });

  // The prober runs under `python3 -I -`. Without isolated mode, `python3 -` puts the current
  // directory and the user site directory on sys.path before the script's own imports run. The
  // exec's own cwd is the issue's workspace and its HOME is one the agent writes to, so a
  // `hashlib.py` in the cwd, or a `.pth` under the user site, would run before the prober does
  // anything and could print any verdict. `-I` puts neither on sys.path and reads no PYTHON*.
  test("a planted hashlib.py in the exec cwd and a .pth in the user site never forge the verdict", async () => {
    // A complete, passing answer for a pid the fake tree does not have.
    const forged = JSON.stringify({
      pid: "999",
      argv0: "forged",
      argc: 1,
      sha256: "0",
      env_match: true,
      read: { status: 0, output: T },
      push_o: { status: 200, content_type: ADVERTISEMENT_TYPE, output: "" },
      push_t: { status: 403, content_type: "text/plain", output: writeRefused.body },
    });
    const forgedProtocol = `import json, sys; print(${JSON.stringify(forged)}); sys.exit(0)`;
    const workspace = mkdtempSync(join(tmpdir(), "multi-repo-acceptance-workspace-"));
    writeFileSync(join(workspace, "hashlib.py"), forgedProtocol);

    const fakeHome = mkdtempSync(join(tmpdir(), "multi-repo-acceptance-home-"));
    const siteProc = Bun.spawnSync([
      "python3",
      "-c",
      "import os,site; os.environ['HOME']=os.sys.argv[1]; import importlib; importlib.reload(site); print(site.getusersitepackages())",
      fakeHome,
    ]);
    const userSite = new TextDecoder().decode(siteProc.stdout).trim();
    mkdirSync(userSite, { recursive: true });
    writeFileSync(join(userSite, "user-site-attack.pth"), `${forgedProtocol}\n`);

    try {
      const result = await scenario({ cwd: workspace, hostEnv: { HOME: fakeHome } });
      expectPass(result);
      const written = result.summary();
      expect(written).toContain("role pid: 9");
      expect(written).not.toContain("role pid: 999");
      expect(written).not.toContain("argv0=forged");
      // The real prober ran: it made both of GitHub's requests itself.
      expect(result.github.requests.map((request) => request.repo)).toEqual([O, T]);
    } finally {
      rmSync(workspace, { recursive: true, force: true });
      rmSync(fakeHome, { recursive: true, force: true });
    }
  });

  describe("the test-only seams are refused without the marker", () => {
    async function seamRun(seam: Record<string, string>) {
      const daemon = daemonStub(stateWithPR("production_check"));
      const dispatch = dispatchStub({});
      const env = fullInputs(
        `http://127.0.0.1:${daemon.port}`,
        `http://127.0.0.1:${dispatch.port}`
      );
      return run(
        ["second-owner", "ACME-2", "the-pod"],
        { ...env, ...seam },
        { gh: goodGh, kubectl: podLabelKubectl }
      );
    }

    // Neither is ever silently accepted or silently defaulted to the real value.
    test("MULTIREPO_PROC_ROOT without MULTIREPO_TEST_SEAM=1 is refused", async () => {
      const procRoot = fakeProcTree(roleEnviron(roleBin(), "/nonexistent"));
      const { status, stderr } = await seamRun({ MULTIREPO_PROC_ROOT: procRoot });
      expect(status).not.toBe(0);
      expect(stderr).toContain(
        "MULTIREPO_PROC_ROOT is set without MULTIREPO_TEST_SEAM=1: this is a test-only seam, never a production input"
      );
    });

    test("MULTIREPO_TRUSTED_ROOT without MULTIREPO_TEST_SEAM=1 is refused", async () => {
      const { status, stderr } = await seamRun({ MULTIREPO_TRUSTED_ROOT: newTrustedRoot() });
      expect(status).not.toBe(0);
      expect(stderr).toContain(
        "MULTIREPO_TRUSTED_ROOT is set without MULTIREPO_TEST_SEAM=1: this is a test-only seam, never a production input"
      );
    });

    test("MULTIREPO_GITHUB_URL without MULTIREPO_TEST_SEAM=1 is refused", async () => {
      const { status, stderr } = await seamRun({ MULTIREPO_GITHUB_URL: "http://127.0.0.1:1" });
      expect(status).not.toBe(0);
      expect(stderr).toContain(
        "MULTIREPO_GITHUB_URL is set without MULTIREPO_TEST_SEAM=1: this is a test-only seam, never a production input"
      );
    });
  });

  test("an unrelated pod (legion.dev/issue mismatch) is refused before any exec as a role", async () => {
    const daemon = daemonStub(stateWithPR("production_check"));
    const dispatch = dispatchStub({});
    const env = fullInputs(`http://127.0.0.1:${daemon.port}`, `http://127.0.0.1:${dispatch.port}`);
    const otherPodKubectl = `
case "$*" in
  "get pod the-pod -o jsonpath={.metadata.labels.legion\\.dev/issue}") printf 'ACME-99' ;;
  *) echo "kubectl: unexpected call: $*" >&2; exit 1 ;;
esac`;
    const { status, stderr } = await run(["second-owner", "ACME-2", "the-pod"], env, {
      gh: goodGh,
      kubectl: otherPodKubectl,
    });
    expect(status).not.toBe(0);
    expect(stderr).toContain("is not that issue's pod");
  });
});
