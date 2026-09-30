import { afterAll, describe, expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

// require_proof_human (lib/workflow.sh) against a fake gh first on PATH: it prints FAKE_GH_LOGIN
// for the viewer query, writes FAKE_GH_STDERR to stderr, exits FAKE_GH_EXIT, and logs each call
// with the GH_REPO it was given.
const lib = join(import.meta.dir, "workflow.sh");
const dir = mkdtempSync(join(tmpdir(), "proof-human-test."));
afterAll(() => rmSync(dir, { recursive: true, force: true }));
writeFileSync(
  join(dir, "gh"),
  `#!/usr/bin/env bash
printf 'GH_REPO=%s %s\\n' "\${GH_REPO:-}" "$*" >>"$FAKE_GH_LOG"
[ -z "\${FAKE_GH_STDERR:-}" ] || printf '%s\\n' "$FAKE_GH_STDERR" >&2
case "$*" in *viewer*) printf '%s\\n' "$FAKE_GH_LOGIN" ;; esac
exit "\${FAKE_GH_EXIT:-0}"
`,
  { mode: 0o755 }
);

// The stage scripts' shell options, ERR trap, and check helpers, then the lib.
const prelude = `set -Eeuo pipefail
check=prerequisites
note() { printf '   %s\\n' "$*"; }
fail() { printf 'FAIL %s: %s\\n' "$check" "$*" >&2; exit 1; }
trap 'printf "FAIL %s: line %s exited %s: %s\\n" "$check" "$LINENO" "$?" "$BASH_COMMAND" >&2' ERR
repo=sjawhar/legion-smoke project=S3TEST ptoken=s3test ok=
. "$LIB"
`;

let runs = 0;
function run(script: string, fake: { login?: string; stderr?: string; exit?: number } = {}) {
  const work = join(dir, `run-${++runs}`);
  const log = join(dir, `calls-${runs}`);
  writeFileSync(log, "");
  mkdirSync(work);
  const result = Bun.spawnSync(["bash", "-c", prelude + script], {
    env: {
      PATH: `${dir}:/usr/bin:/bin`,
      HOME: dir,
      LIB: lib,
      work,
      FAKE_GH_LOG: log,
      FAKE_GH_LOGIN: fake.login ?? "",
      FAKE_GH_STDERR: fake.stderr ?? "",
      FAKE_GH_EXIT: String(fake.exit ?? 0),
    },
  });
  return {
    status: result.exitCode,
    stdout: result.stdout.toString(),
    stderr: result.stderr.toString(),
    calls: readFileSync(log, "utf8").split("\n").filter(Boolean),
  };
}

// The refusal names the account it found right before the smoke repository; the proof human's own
// name and the repository appear on every refusal, so only that position tells the two apart.
const refusedAs = (login: string) =>
  new RegExp(
    `^FAIL prerequisites: .* ${login.replace(/[[\]]/g, "\\$&")} for sjawhar/legion-smoke\\b`
  );

describe("require_proof_human", () => {
  test("passes the sjawhar-agent App's bot, asking gh with GH_REPO naming the smoke repository", () => {
    const r = run("require_proof_human", { login: "sjawhar-agent[bot]" });
    expect(r.status).toBe(0);
    expect(r.calls).toHaveLength(1);
    expect(r.calls[0]).toStartWith("GH_REPO=sjawhar/legion-smoke api graphql ");
  });

  test("refuses every other account in one line naming it", () => {
    for (const login of [
      "sjawhar",
      "sjawhar-agent",
      "legion-reviewer[bot]",
      "legion-implementer[bot]",
    ]) {
      const r = run("require_proof_human\necho unreachable", { login });
      expect(r.status).toBe(1);
      expect(r.stdout).not.toContain("unreachable");
      expect(r.stderr.trimEnd().split("\n")).toHaveLength(1);
      expect(r.stderr).toMatch(refusedAs(login));
    }
  });

  test("refuses an empty answer as no account", () => {
    const r = run("require_proof_human", { login: "" });
    expect(r.status).toBe(1);
    expect(r.stderr).toMatch(refusedAs("no account"));
  });

  // An agent session that inherited a personal GH_TOKEN still reaches the user's account; the
  // devbox shim says so on stderr, which is the operator's only clue.
  test("a refusal carries what gh said on stderr, joined into its one line", () => {
    const said =
      "gh shim: GH_TOKEN inherited from the environment (ghp_…, a PERSONAL token); not routing";
    const r = run("require_proof_human", { login: "sjawhar", stderr: `${said}\nsecond line` });
    expect(r.status).toBe(1);
    expect(r.stderr.trimEnd().split("\n")).toHaveLength(1);
    expect(r.stderr).toMatch(refusedAs("sjawhar"));
    expect(r.stderr).toContain(`${said} second line`);
  });

  test("refuses a gh that cannot answer, with its reason", () => {
    const reason = "gh: To use GitHub CLI in automation, set the GH_TOKEN environment variable.";
    const r = run("require_proof_human", { stderr: reason, exit: 4 });
    expect(r.status).toBe(1);
    expect(r.stderr.trimEnd().split("\n")).toHaveLength(1);
    expect(r.stderr).toContain(reason);
  });

  // Three ways reach a wrong account, and gh can fail outright: a plain shell (the user's login),
  // a Legion pane (its own App), an agent session with a personal GH_TOKEN (the token's owner). The
  // gh-failure reason is one no requirement word appears in, so the words below are the refusal's.
  test("every refusal states the one requirement, whatever reached the wrong account", () => {
    const causes = [
      { login: "sjawhar" },
      { login: "legion-implementer[bot]" },
      { login: "sjawhar", stderr: "gh shim: inherited from the environment (a PERSONAL token)" },
      { stderr: "gh: Bad credentials (HTTP 401)", exit: 4 },
    ];
    for (const fake of causes) {
      const r = run("require_proof_human", fake);
      expect(r.status).toBe(1);
      expect(r.stderr).toMatch(/own Oh My Pi session/);
      expect(r.stderr).toMatch(/not a Legion pane/);
      expect(r.stderr).toMatch(/no personal GH_TOKEN/);
    }
  });
});

describe("close_unpassed_run_pull_requests", () => {
  test("makes no gh call in a run that never passed require_proof_human", () => {
    const r = run("close_unpassed_run_pull_requests");
    expect(r.status).toBe(0);
    expect(r.calls).toEqual([]);
  });

  // A refused run's own sequence: the check fails, the run exits, and the EXIT trap runs the close.
  // The probe must be the only gh call, since every other would act as the refused account.
  test("a refused run's EXIT trap makes no gh call after the probe", () => {
    const r = run("trap close_unpassed_run_pull_requests EXIT\nrequire_proof_human", {
      login: "sjawhar",
    });
    expect(r.status).toBe(1);
    expect(r.calls).toHaveLength(1);
    expect(r.calls[0]).toContain(" api graphql ");
  });

  test("lists the run's pull requests once require_proof_human passed", () => {
    const r = run("require_proof_human\nclose_unpassed_run_pull_requests", {
      login: "sjawhar-agent[bot]",
    });
    expect(r.status).toBe(0);
    expect(r.calls.some((c) => c.includes("-R sjawhar/legion-smoke pr list"))).toBe(true);
  });
});
