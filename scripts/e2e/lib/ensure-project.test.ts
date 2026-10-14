import { afterAll, describe, expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

// ensure_project, write_dispatch_header and dispatch_agent (lib/workflow.sh) against a fake curl on
// PATH: it logs every invocation, writes FAKE_CURL_BODY to whatever file follows -o, prints
// FAKE_CURL_STATUS to stdout (curl's own -w '%{http_code}' convention, since -o diverts the body),
// and on a nonzero FAKE_CURL_EXIT prints FAKE_CURL_ERROR to stderr and 000 to stdout and writes
// nothing, as real curl does when the connection fails (no response, so -w writes the status as
// 000).
const lib = join(import.meta.dir, "workflow.sh");
const dir = mkdtempSync(join(tmpdir(), "ensure-project-test."));
afterAll(() => rmSync(dir, { recursive: true, force: true }));
writeFileSync(
  join(dir, "curl"),
  `#!/usr/bin/env bash
out=
args=("$@")
for ((i = 0; i < \${#args[@]}; i++)); do [ "\${args[i]}" = -o ] && out=\${args[i + 1]}; done
printf 'curl %s\\n' "$*" >>"$FAKE_CURL_LOG"
if [ "\${FAKE_CURL_EXIT:-0}" != 0 ]; then
  printf '%s\\n' "\${FAKE_CURL_ERROR:-curl: (7) Failed to connect}" >&2
  printf '000'
  exit "$FAKE_CURL_EXIT"
fi
[ -z "$out" ] || printf '%s' "$FAKE_CURL_BODY" >"$out"
printf '%s' "$FAKE_CURL_STATUS"
`,
  { mode: 0o755 }
);

// The stage scripts' shell options, ERR trap, and check helpers, then the lib. note, fail and
// scrub live in stage4b-sandbox-tree.sh, not workflow.sh, so the prelude stands in for them: note
// and scrub are stage4b's own (scrub here is the identity, since no service host is under test),
// fail matches stage4b's exact wording.
const prelude = `set -Eeuo pipefail
check=prerequisites
note() { printf '   %s\\n' "$*"; }
fail() { printf 'CHECK %s: FAIL: %s\\n' "$check" "$*" >&2; exit 1; }
scrub() { cat; }
trap 'printf "CHECK %s: FAIL: line %s exited %s: %s\\n" "$check" "$LINENO" "$?" "$BASH_COMMAND" >&2' ERR
dispatch_base=https://dispatch.example
repo=sjawhar/legion-smoke ptoken=s3test ok=
. "$LIB"
`;

let runs = 0;
// The proof human's session every write names as its actor, as stage4b-sandbox-tree.sh sets it.
const proofActor = "legion-e2e4b-proof-human-4242";
function run(
  script: string,
  fake: { status?: string; body?: string; exit?: number; error?: string } = {},
  actor = proofActor
) {
  const work = join(dir, `run-${++runs}`);
  const log = join(dir, `calls-${runs}`);
  const tokenFile = join(dir, `token-${runs}`);
  writeFileSync(log, "");
  writeFileSync(tokenFile, "stage4b-fake-bearer\n", { mode: 0o600 });
  mkdirSync(work);
  const result = Bun.spawnSync(
    [
      "bash",
      "-c",
      prelude +
        `work=${JSON.stringify(work)} dispatch_token_file=${JSON.stringify(tokenFile)} project=LPROOF2 dispatch_actor=${JSON.stringify(actor)}\n` +
        script,
    ],
    {
      env: {
        PATH: `${dir}:/usr/bin:/bin`,
        HOME: dir,
        LIB: lib,
        FAKE_CURL_LOG: log,
        FAKE_CURL_STATUS: fake.status ?? "",
        FAKE_CURL_BODY: fake.body ?? "",
        FAKE_CURL_EXIT: String(fake.exit ?? 0),
        FAKE_CURL_ERROR: fake.error ?? "",
      },
    }
  );
  return {
    status: result.exitCode,
    stdout: result.stdout.toString(),
    stderr: result.stderr.toString(),
    work,
    calls: readFileSync(log, "utf8").split("\n").filter(Boolean),
    bearer: (() => {
      try {
        return readFileSync(join(work, "dispatch-human-header"), "utf8");
      } catch {
        return null;
      }
    })(),
  };
}

describe("write_dispatch_header", () => {
  test("writes dispatch-auth-header and dispatch-human-header identically from the token file", () => {
    const r = run("write_dispatch_header");
    expect(r.status).toBe(0);
    expect(r.bearer).toBe("Authorization: Bearer stage4b-fake-bearer\n");
  });
});

describe("ensure_project", () => {
  // Production Dispatch answers a bearer that names no session 400 ACTOR_KIND before it reads the
  // key, so a body without the actor would fail every run against it.
  test("201: a created project is ready; the body posts {key, name} and the proof human's actor", () => {
    const r = run("write_dispatch_header\nensure_project", {
      status: "201",
      body: '{"key":"LPROOF2","name":"LPROOF2","created_at":"2026-10-11T00:00:00Z"}',
    });
    expect(r.status).toBe(0);
    expect(r.stdout).toContain("created project LPROOF2");
    expect(r.calls).toHaveLength(1);
    expect(r.calls[0]).toContain("-X POST");
    expect(r.calls[0]).toContain("/api/v1/projects");
    // The bearer goes by the header file write_dispatch_header wrote, never in argv.
    expect(r.calls[0]).toContain(`-H @${r.work}/dispatch-human-header `);
    expect(r.bearer).toBe("Authorization: Bearer stage4b-fake-bearer\n");
    expect(r.calls[0]).toContain(
      `{"key":"LPROOF2","name":"LPROOF2","actor":{"kind":"session","id":"${proofActor}"}}`
    );
  });

  test("an empty dispatch_actor posts {key, name} alone, as a scratch Dispatch's human header writes", () => {
    const r = run(
      "write_dispatch_header\nensure_project",
      { status: "201", body: '{"key":"LPROOF2","name":"LPROOF2"}' },
      ""
    );
    expect(r.status).toBe(0);
    expect(r.calls).toHaveLength(1);
    expect(r.calls[0]).toContain('{"key":"LPROOF2","name":"LPROOF2"}');
    expect(r.calls[0]).not.toContain("actor");
  });

  test("a refused actor (400 ACTOR_KIND) fails loudly, naming the status and the body", () => {
    const r = run("write_dispatch_header\nensure_project", {
      status: "400",
      body: '{"error":"bearer callers require actor.kind session","code":"ACTOR_KIND"}',
    });
    expect(r.status).toBe(1);
    expect(r.stderr).toContain("answered 400");
    expect(r.stderr).toContain('"code":"ACTOR_KIND"');
  });

  test("409 PROJECT_EXISTS: an existing project is also ready", () => {
    const r = run("write_dispatch_header\nensure_project", {
      status: "409",
      body: '{"error":"a project with this key already exists","code":"PROJECT_EXISTS"}',
    });
    expect(r.status).toBe(0);
    expect(r.stdout).toContain("project LPROOF2 already exists");
  });

  // A 409 could in principle mean something other than this key's own conflict; ensure_project
  // must not read any 409 as success, only the one the code names.
  test("409 with another code fails loudly, naming the status and the body", () => {
    const r = run("write_dispatch_header\nensure_project", {
      status: "409",
      body: '{"error":"something else entirely","code":"SOMETHING_ELSE"}',
    });
    expect(r.status).toBe(1);
    expect(r.stderr).toContain("409");
    expect(r.stderr).toContain("not PROJECT_EXISTS");
    expect(r.stderr).toContain('"code":"SOMETHING_ELSE"');
  });

  test("500 fails loudly, naming the status and the body", () => {
    const r = run("write_dispatch_header\nensure_project", {
      status: "500",
      body: '{"error":"internal error"}',
    });
    expect(r.status).toBe(1);
    expect(r.stderr).toContain("500");
    expect(r.stderr).toContain('{"error":"internal error"}');
  });

  // Real curl prints 000 on stdout when the connection fails; the FAIL line carries curl's error
  // alone, on one line.
  test("a connection failure fails loudly as unreachable, in one line, never as a refusal", () => {
    const r = run("write_dispatch_header\nensure_project", {
      exit: 7,
      error: "curl: (7) Failed to connect to dispatch.example port 443",
    });
    expect(r.status).toBe(1);
    expect(r.stderr.trimEnd().split("\n")).toEqual([
      "CHECK prerequisites: FAIL: Dispatch could not be reached to create LPROOF2: curl: (7) Failed to connect to dispatch.example port 443",
    ]);
  });
});

describe("dispatch_agent", () => {
  // A bearer caller must name its session in the body (400 ACTOR_KIND otherwise), and the session
  // dispatch_agent names is the stand-in architect's, never the proof human's dispatch_actor: the
  // write is an agent's, over the agents' bearer rather than the human header.
  test("sends the bearer header file, JSON, the method, the path and the body plus the stand-in session's actor", () => {
    const r = run(
      `write_dispatch_header\ndispatch_agent POST "artifacts/doc-1/edits" '{"ops":[{"op":"insert","after":"end","markdown":"Hello."}]}'`
    );
    expect(r.status).toBe(0);
    expect(r.calls).toHaveLength(1);
    expect(r.calls[0]).toContain("-X POST");
    expect(r.calls[0]).toContain(`-H @${r.work}/dispatch-auth-header `);
    expect(r.calls[0]).not.toContain("dispatch-human-header");
    expect(readFileSync(join(r.work, "dispatch-auth-header"), "utf8")).toBe(
      "Authorization: Bearer stage4b-fake-bearer\n"
    );
    expect(r.calls[0]).toContain("-H content-type: application/json");
    expect(r.calls[0]).toContain(" https://dispatch.example/api/v1/artifacts/doc-1/edits");
    expect(r.calls[0]).toContain(
      '--data {"ops":[{"op":"insert","after":"end","markdown":"Hello."}],"actor":{"kind":"session","id":"stage3-architect-stand-in"}}'
    );
    expect(r.calls[0]).not.toContain(proofActor);
  });
});
