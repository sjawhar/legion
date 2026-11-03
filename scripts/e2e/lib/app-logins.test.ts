import { afterAll, describe, expect, test } from "bun:test";
import {
  copyFileSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readdirSync,
  readFileSync,
  rmSync,
  statSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { appTokenSources } from "./app-logins";

// lib/app-logins.ts run as the stage proofs run it, against a stand-in GitHub that answers
// GraphQL's viewer and GET /apps/<slug> under an installation token of one of its Apps, and
// refuses every other request, the JWT routes (GET /app, the installation routes) among them. The
// implement App holds checks: read alone, as the real one does.
const dir = mkdtempSync(join(tmpdir(), "app-logins-test."));
const script = join(import.meta.dir, "app-logins.ts");

interface StandInApp {
  id: number;
  slug: string;
  /** The App id GET /apps/<slug> answers, when it is not `id`. */
  answers?: number;
  /** The login GraphQL's viewer answers, when it is not `<slug>[bot]`. */
  login?: string;
  permissions: Record<string, string>;
}
const implementPermissions = { contents: "write", checks: "read", pull_requests: "write" };
const reviewPermissions = { contents: "write", checks: "write", pull_requests: "write" };
/** Each stand-in App by its installation token. */
const tokens: Record<string, StandInApp> = {
  ghs_implement_101_secret: { id: 101, slug: "example-implement", permissions: implementPermissions },
  ghs_review_202_secret: { id: 202, slug: "example-review", permissions: reviewPermissions },
  ghs_other_404_secret: {
    id: 404,
    slug: "example-other",
    answers: 405,
    permissions: reviewPermissions,
  },
  ghs_person_303_secret: {
    id: 303,
    slug: "example-person",
    login: "a-person",
    permissions: reviewPermissions,
  },
};
const tokenOf = (id: number) =>
  Object.keys(tokens).find((token) => tokens[token].id === id) ?? "";
const allTokens = Object.keys(tokens);

const seen: string[] = [];
const github = Bun.serve({
  port: 0,
  async fetch(request) {
    const path = new URL(request.url).pathname;
    seen.push(`${request.method} ${path}`);
    const token = (request.headers.get("authorization") ?? "").replace(/^token /, "");
    const app = tokens[token];
    if (app === undefined || path === "/app" || path.startsWith("/app/")) {
      return Response.json({ message: `Bad credentials for ${token}` }, { status: 401 });
    }
    if (request.method === "POST" && path === "/graphql") {
      const query = (await request.json()) as { query?: string };
      if (query.query !== "{viewer{login}}") return Response.json({}, { status: 400 });
      return Response.json({ data: { viewer: { login: app.login ?? `${app.slug}[bot]` } } });
    }
    const slug = /^\/apps\/([^/]+)$/.exec(path)?.[1];
    const named = Object.values(tokens).find((candidate) => candidate.slug === slug);
    if (request.method === "GET" && named !== undefined) {
      return Response.json({
        id: named.answers ?? named.id,
        slug: named.slug,
        permissions: named.permissions,
      });
    }
    return Response.json({ message: "Not Found" }, { status: 404 });
  },
});
const standIn = `http://127.0.0.1:${github.port}/`;
// A GitHub that never answers: the port of a server stopped before any run.
const closed = Bun.serve({ port: 0, fetch: () => new Response() });
const unreachable = `http://127.0.0.1:${closed.port}/`;
await closed.stop(true);
afterAll(() => {
  github.stop(true);
  rmSync(dir, { recursive: true, force: true });
});

/** A release as the secrets broker gives one: one entry per installation, keyed by login. */
function release(installations: Record<string, string>, expires = "2099-01-01T00:00:00Z") {
  return JSON.stringify({
    installations: Object.fromEntries(
      Object.entries(installations).map(([owner, token], index) => [
        owner,
        { installation_id: 1000 + index, token, expires_at: expires },
      ])
    ),
  });
}
for (const id of [101, 202, 303, 404]) {
  writeFileSync(join(dir, `release-${id}.json`), release({ sjawhar: tokenOf(id) }));
}

let configs = 0;
function config(implement: string, review: string): string {
  const path = join(dir, `legion-${++configs}.yaml`);
  writeFileSync(
    path,
    `project: ACME\ngithub_apps:\n  implement:\n${implement}\n  review:\n${review}\n`
  );
  return path;
}
const app = (id: string, command = `cat ${join(dir, `release-${id}.json`)}`) =>
  `    app_id: "${id}"\n    token_command: ${JSON.stringify(command)}`;

// The run is asynchronous: the stand-in GitHub answers from this test's own event loop. `env` is
// added to this test's own environment. The helper is given stdin, so a token command that
// inherited it would read `stdin-leak` where the closed-stdin case expects nothing.
async function run(path: string, env: Record<string, string> = {}, api = standIn) {
  const child = Bun.spawn(["bun", script, path, api], {
    env: { ...process.env, ...env },
    stdin: new Blob(["stdin-leak"]),
    stdout: "pipe",
    stderr: "pipe",
  });
  const [stdout, stderr, code] = await Promise.all([
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
    child.exited,
  ]);
  return { code, stdout: stdout.trim(), stderr: stderr.trim() };
}

// read_app_logins as a stage proof calls it: lib/workflow.sh sourced under the stage scripts' shell
// options and check helpers (the lib reads repo as it is sourced), with the config as
// $work/github-apps.yaml, the file write_github_apps writes, then `then`. The one seam is bun,
// which is handed `api` after the helper's own arguments, the GitHub API URL the helper takes.
async function readAppLogins(path: string, then: string, api = standIn) {
  const work = mkdtempSync(join(dir, "work."));
  copyFileSync(path, join(work, "github-apps.yaml"));
  const shell = `set -Eeuo pipefail
check=boot repo=sjawhar/legion-smoke
note() { printf '   %s\\n' "$*"; }
fail() { printf 'FAIL %s: %s\\n' "$check" "$*" >&2; exit 1; }
bun() { command bun "$@" "$API"; }
. "$LIB"
read_app_logins
${then}`;
  const child = Bun.spawn(["bash", "-c", shell], {
    env: { ...process.env, LIB: join(import.meta.dir, "workflow.sh"), work, API: api },
    stdout: "pipe",
    stderr: "pipe",
  });
  const [stdout, stderr, code] = await Promise.all([
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
    child.exited,
  ]);
  return { code, stdout: stdout.trimEnd(), stderr: stderr.trim() };
}

// A failure names the App it could not read by its role and app id, on one line of stderr.
const namesApp = (role: string, id: string) => new RegExp(`\\b${role} App\\b.*\\bapp_id ${id}\\b`);
const holdsNoToken = (text: string) => {
  for (const token of allTokens) expect(text).not.toContain(token);
};

describe("app-logins.ts", () => {
  test("prints each App's bot login from GraphQL's viewer, proved by GET /apps/<slug> under its own token", async () => {
    seen.length = 0;
    const { code, stdout, stderr } = await run(config(app("101"), app("202")));
    expect(stderr).toBe("");
    expect(code).toBe(0);
    expect(JSON.parse(stdout)).toEqual({
      implement: "example-implement[bot]",
      review: "example-review[bot]",
    });
    // No JWT route is asked: the proof holds no key.
    expect(seen).toEqual([
      "POST /graphql",
      "GET /apps/example-implement",
      "POST /graphql",
      "GET /apps/example-review",
    ]);
    holdsNoToken(stdout);
  });

  test("an App whose id is not the configured one is refused, naming both ids and no token", async () => {
    const { code, stdout, stderr } = await run(config(app("101"), app("404")));
    expect(code).toBe(1);
    expect(stdout).toBe("");
    expect(stderr).toMatch(namesApp("review", "404"));
    expect(stderr).toContain("answered App id 405, want 404");
    holdsNoToken(stderr);
  });

  test("an App without its role's write permission is refused, the two Apps swapped whole too", async () => {
    const { code, stdout, stderr } = await run(config(app("202", `cat ${join(dir, "release-202.json")}`), app("101")));
    expect(code).toBe(1);
    expect(stdout).toBe("");
    // The implement role on the review App passes (contents: write); the review role on the
    // implement App does not.
    expect(stderr).toMatch(namesApp("review", "101"));
    expect(stderr).toContain('answered permissions.checks "read", want write');
    holdsNoToken(stderr);
  });

  test("a token command that fails is named by its field, exit code and stderr, its stdout never quoted", async () => {
    const { code, stderr } = await run(
      config(app("101", `echo ${tokenOf(101)}; echo grant denied >&2; exit 3`), app("202"))
    );
    expect(code).toBe(1);
    // The stderr reaches the operator as it is written, then again in the failure.
    expect(stderr).toBe(
      "grant denied\napp-logins: the implement App (app_id 101): github_apps.implement.token_command failed (exit 3): grant denied"
    );
    holdsNoToken(stderr);
  });

  test("a token command runs as the daemon runs it: no NATS seed, stdin closed, owner and read-only set empty", async () => {
    // An unset variable prints `unset`, so a set-empty owner and read-only flag print nothing.
    const probe =
      'printf "%s|%s|%s|%s|%s|%s" "$NATS_NKEY_SEED" "$NATS_DAEMON_NKEY_SEED" "$NATS_NKEY_SEED_FILE" "${LEGION_GITHUB_OWNER-unset}" "${LEGION_GITHUB_READ_ONLY-unset}" "$(cat)" >&2; exit 4';
    const { code, stderr } = await run(config(app("101", probe), app("202")), {
      NATS_NKEY_SEED: "pane-seed",
      NATS_DAEMON_NKEY_SEED: "daemon-seed",
      NATS_NKEY_SEED_FILE: "/seed-file",
      LEGION_GITHUB_OWNER: "leaked-owner",
      LEGION_GITHUB_READ_ONLY: "1",
    });
    expect(code).toBe(1);
    expect(stderr).toBe(
      "||/seed-file|||app-logins: the implement App (app_id 101): github_apps.implement.token_command failed (exit 4): ||/seed-file|||"
    );
  });

  test("a token command's stderr reaches the operator on success too", async () => {
    const pending = `printf 'request req-42 pending\\n' >&2; cat ${join(dir, "release-101.json")}`;
    const { code, stdout, stderr } = await run(config(app("101", pending), app("202")));
    expect(code).toBe(0);
    expect(stderr).toBe("request req-42 pending");
    expect(JSON.parse(stdout).implement).toBe("example-implement[bot]");
  });

  test("a token command whose newline-delimited stderr exceeds the cap is killed", async () => {
    const noisy =
      "i=0; while [ \"$i\" -lt 257 ]; do printf '%04095d\\n' 0 >&2; i=$((i + 1)); done; exit 3";
    const { code, stderr } = await run(config(app("101", noisy), app("202")));
    expect(code).toBe(1);
    expect(stderr.slice(-1000)).toContain("wrote more than 1048576 bytes to stderr");
  });

  test("output that is no release, an expired token, or no installation is refused without quoting it", async () => {
    const cases: [string, string][] = [
      [`echo '{"token":"${tokenOf(101)}"'`, "printed no release"],
      [`echo '${release({ sjawhar: tokenOf(101) }, "2000-01-01T00:00:00Z")}'`, "expired at"],
      [`echo '{"installations":{}}'`, "released no installation"],
    ];
    for (const [command, want] of cases) {
      const { code, stderr } = await run(config(app("101", command), app("202")));
      expect(code).toBe(1);
      expect(stderr).toMatch(namesApp("implement", "101"));
      expect(stderr).toContain(want);
      holdsNoToken(stderr);
    }
  });

  test("the first installation by login is the one read, as the daemon's AppLogin reads it", async () => {
    const two = join(dir, "release-two.json");
    writeFileSync(two, release({ zeta: "ghs_unknown_token", Alpha: tokenOf(101) }));
    const { code, stdout, stderr } = await run(config(app("101", `cat ${two}`), app("202")));
    expect(stderr).toBe("");
    expect(code).toBe(0);
    expect(JSON.parse(stdout).implement).toBe("example-implement[bot]");
  });

  test("a token GitHub refuses is named by the App and GitHub's status, its echo of the token masked", async () => {
    const unknown = join(dir, "release-unknown.json");
    writeFileSync(unknown, release({ sjawhar: "ghs_unknown_token" }));
    const { code, stderr } = await run(config(app("101", `cat ${unknown}`), app("202")));
    expect(code).toBe(1);
    expect(stderr).toMatch(namesApp("implement", "101"));
    expect(stderr).toContain("401");
    expect(stderr).not.toContain("ghs_unknown_token");
  });

  test("a token that acts as a person, not an App's bot, is refused", async () => {
    const { code, stderr } = await run(config(app("101"), app("303")));
    expect(code).toBe(1);
    expect(stderr).toMatch(namesApp("review", "303"));
    expect(stderr).toContain("acts as a-person, not an App's bot");
  });

  test("a GitHub that never answers is named by the App's role and app id", async () => {
    const { code, stdout, stderr } = await run(config(app("101"), app("202")), {}, unreachable);
    expect(code).toBe(1);
    expect(stdout).toBe("");
    expect(stderr).toMatch(namesApp("implement", "101"));
  });

  test("two roles on one App are refused, as the daemon refuses them at boot", async () => {
    const { code, stderr } = await run(config(app("202"), app("202")));
    expect(code).toBe(1);
    expect(stderr).toContain("both github_apps roles are the App example-review[bot]");
  });

  test("--roles reads only the roles it names, and refuses one it does not know", async () => {
    const path = join(dir, `legion-${++configs}.yaml`);
    writeFileSync(path, `github_apps:\n  implement:\n${app("101")}\n`);
    const one = Bun.spawn(["bun", script, "--roles", "implement", path, standIn], {
      stdout: "pipe",
      stderr: "pipe",
    });
    const [stdout, stderr, code] = await Promise.all([
      new Response(one.stdout).text(),
      new Response(one.stderr).text(),
      one.exited,
    ]);
    expect(stderr).toBe("");
    expect(code).toBe(0);
    expect(JSON.parse(stdout)).toEqual({ implement: "example-implement[bot]" });
    for (const roles of ["merger", "implement,implement", ""]) {
      const bad = Bun.spawnSync(["bun", script, "--roles", roles, path], { stderr: "pipe" });
      expect(bad.exitCode).toBe(2);
      expect(bad.stderr.toString()).toContain("usage:");
    }
  });

  test("an App configured with no token_command is refused by name", () => {
    expect(() =>
      appTokenSources(
        'github_apps:\n  implement: { app_id: "1", private_key_command: "true" }\n  review: { app_id: "2", token_command: "true" }\n'
      )
    ).toThrow("github_apps.implement names its credential without a token_command");
    expect(() => appTokenSources("project: ACME\n")).toThrow("the config has no github_apps");
  });
});

describe("read_app_logins", () => {
  test("gives each role its App's login as GitHub answered it, and GraphQL the bare slug", async () => {
    const roles = [
      "implementer",
      "merger",
      "planner",
      "tester",
      "reviewer",
      "architect",
      "controller",
    ];
    const { code, stdout, stderr } = await readAppLogins(
      config(app("101"), app("202")),
      `for role in ${roles.join(" ")}; do echo "$role $(role_app "$role") $(role_app_slug "$role")"; done`
    );
    expect(stderr).toBe("");
    expect(code).toBe(0);
    const answers = stdout.split("\n").filter((line) => roles.includes(line.split(" ")[0]));
    expect(answers).toEqual([
      "implementer example-implement[bot] example-implement",
      "merger example-implement[bot] example-implement",
      "planner example-review[bot] example-review",
      "tester example-review[bot] example-review",
      "reviewer example-review[bot] example-review",
      "architect example-review[bot] example-review",
      "controller example-review[bot] example-review",
    ]);
  });

  test("a GitHub that never answers fails the run, naming the App it could not read", async () => {
    const { code, stdout, stderr } = await readAppLogins(
      config(app("101"), app("202")),
      "echo after-read",
      unreachable
    );
    expect(code).toBe(1);
    expect(stdout).not.toContain("after-read");
    expect(stderr).toMatch(namesApp("implement", "101"));
    expect(stderr).toMatch(/^FAIL boot: /m);
  });
});

// lib/github-apps.sh as a stage proof uses it: sourced under the stage scripts' shell options with
// their check helpers, the inputs in the environment, and `then` run after. The seams are bun,
// handed the stand-in GitHub after the helper's own arguments, and a stand-in `agent-secrets` first
// on PATH, which, as the real CLI does, puts the release the github-app secret it names gives
// (STORE/<name>) into the environment of the command after `--` under that name. It appends its
// argv to ARGV_LOG, fails with exit 3 when the `<owner>/<read-only>` it was asked for matches
// FAIL_MINT, releases nothing when EMPTY_MINT is set, and streams stdout when OVERSIZED_MINT is set.
const store = join(dir, "store");
const fakeBin = join(dir, "bin");
mkdirSync(store);
mkdirSync(fakeBin);
writeFileSync(join(store, "IMPLEMENT_APP"), release({ sjawhar: tokenOf(101) }));
writeFileSync(join(store, "REVIEW_APP"), release({ sjawhar: tokenOf(202) }));
writeFileSync(join(store, "OTHER_APP"), release({ sjawhar: tokenOf(404) }));
writeFileSync(
  join(fakeBin, "agent-secrets"),
  `#!/bin/sh
printf '%s\\n' "$*" >>"\${ARGV_LOG:-/dev/null}"
name=$1
shift
owner=
ro=
while [ "$1" != -- ]; do
  case $1 in --github-owner) owner=$2; shift ;; --github-read-only) ro=1 ;; esac
  shift
done
shift
if [ -n "\${FAIL_MINT:-}" ]; then
  case "$owner/$ro" in $FAIL_MINT) echo "grant for $name denied" >&2; exit 3 ;; esac
fi
if [ -n "\${EMPTY_MINT:-}" ]; then export "$name="; exec "$@"; fi
if [ -n "\${OVERSIZED_MINT:-}" ]; then exec yes x; fi
value=$(cat "\${STORE:-${store}}/$name") || exit 9
export "$name=$value"
exec "$@"
`,
  { mode: 0o755 }
);
const appInputs = {
  LEGION_E2E_IMPLEMENT_APP_ID: "101",
  LEGION_E2E_IMPLEMENT_APP_SECRET: "IMPLEMENT_APP",
  LEGION_E2E_REVIEW_APP_ID: "202",
  LEGION_E2E_REVIEW_APP_SECRET: "REVIEW_APP",
};
// The sh the bridge daemon's container runs a token command with is busybox's; where the host has
// none, the host's POSIX sh stands in for it.
const busyboxSh = Bun.which("busybox") === null ? ["sh"] : ["busybox", "sh"];

async function githubApps(inputs: Record<string, string | undefined>, then: string) {
  const work = mkdtempSync(join(dir, "apps."));
  const ambient = { ...process.env };
  // A devbox shell may export this deployment's own credential variables; left in, `compgen -e`
  // would strip them and hide whether the lib's own default entries do, so the child inherits none
  // and only a case's own inputs put one back.
  for (const name of Object.keys(ambient)) {
    if (name.includes("PRIVATE_KEY") || name.startsWith("LEGION_E2E_") || name.startsWith("LEGION_GITHUB_")) {
      delete ambient[name];
    }
  }
  const env: Record<string, string | undefined> = {
    ...ambient,
    PATH: `${fakeBin}:${process.env.PATH}`,
    LIB: join(import.meta.dir, "github-apps.sh"),
    work,
    API: standIn,
    ...inputs,
  };
  for (const name of Object.keys(env)) if (env[name] === undefined) delete env[name];
  const shell = `set -Eeuo pipefail
check=prerequisites
note() { printf '   %s\\n' "$*"; }
fail() { printf 'FAIL %s: %s\\n' "$check" "$*" >&2; exit 1; }
bun() { command bun "$@" "$API"; }
. "$LIB"
${then}`;
  const child = Bun.spawn(["bash", "-c", shell], { env, stdout: "pipe", stderr: "pipe" });
  const [stdout, stderr, code] = await Promise.all([
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
    child.exited,
  ]);
  return { code, stdout: stdout.trimEnd(), stderr: stderr.trim(), work };
}
const line = (stdout: string, prefix: string) =>
  (stdout.split("\n").find((l) => l.startsWith(prefix)) ?? "").slice(prefix.length);
const tokenCommand = (name: string) =>
  `agent-secrets ${name} --reason=legion-live-proof-github-app-token \${LEGION_GITHUB_OWNER:+--github-owner "$LEGION_GITHUB_OWNER"} \${LEGION_GITHUB_READ_ONLY:+--github-read-only} -- sh -c 'printf %s "$${name}"'`;

describe("lib/github-apps.sh", () => {
  const proveBoth = `require_app_inputs implement review
write_github_apps implement review
check_github_apps implement review
echo "logins $github_app_logins"`;

  test("the default pair is the lib's documented one, and an input left unset takes it", async () => {
    seen.length = 0;
    const { code, stdout, stderr, work } = await githubApps(
      { LEGION_E2E_REVIEW_APP_ID: "202", LEGION_E2E_REVIEW_APP_SECRET: "REVIEW_APP" },
      `require_app_inputs implement review
write_github_apps implement review
echo "defaults $default_implement_app_id $default_implement_app_secret $default_review_app_id $default_review_app_secret"`
    );
    expect(stderr).toBe("");
    expect(code).toBe(0);
    expect(line(stdout, "defaults ")).toBe("5341240 LEGION_IMPLEMENT_APP 5341243 LEGION_REVIEW_APP");
    const sources = appTokenSources(readFileSync(join(work, "github-apps.yaml"), "utf8"));
    expect(sources.implement).toEqual({
      appId: "5341240",
      tokenCommand: tokenCommand("LEGION_IMPLEMENT_APP"),
    });
    expect(sources.review).toEqual({ appId: "202", tokenCommand: tokenCommand("REVIEW_APP") });
    // The default pair is never asked of GitHub by this test: check_github_apps did not run.
    expect(seen).toEqual([]);
  });

  test("an input set to an empty value is refused, not read as a choice of the default", async () => {
    const { code, stderr } = await githubApps(
      { ...appInputs, LEGION_E2E_REVIEW_APP_SECRET: "" },
      proveBoth
    );
    expect(code).toBe(1);
    expect(stderr).toContain("LEGION_E2E_REVIEW_APP_SECRET must be");
  });

  test("write_github_apps refuses a role require_app_inputs did not read, and writes nothing", async () => {
    const { code, stdout, stderr, work } = await githubApps(
      appInputs,
      `require_app_inputs implement
write_github_apps implement review
echo "wrote the block"`
    );
    expect(code).toBe(1);
    expect(stdout).not.toContain("wrote the block");
    expect(stderr).toContain(
      "the review App's inputs are unread: require_app_inputs review has not run"
    );
    expect(existsSync(join(work, "github-apps.yaml"))).toBe(false);
  });

  test("app_key_unsets strips each role's secret, this deployment's own two, and every PRIVATE_KEY variable", async () => {
    // Each name is exported first, so the strip list has something to remove: without it the
    // child would print them all.
    const { code, stdout, stderr } = await githubApps(
      { ...appInputs, SOME_OTHER_APP_PRIVATE_KEY_B64: "irrelevant" },
      `export IMPLEMENT_APP=x REVIEW_APP=x
require_app_inputs implement review
export "$default_implement_app_secret=x" "$default_review_app_secret=x"
names="IMPLEMENT_APP|REVIEW_APP|SOME_OTHER_APP_PRIVATE_KEY_B64|$default_implement_app_secret|$default_review_app_secret"
echo "set $(env | grep -c -E "^($names)=")"
env "\${app_key_unsets[@]}" sh -c "echo \\"left \\$(env | grep -c -E '^($names)=' || true)\\""`
    );
    expect(stderr).toBe("");
    expect(code).toBe(0);
    expect(stdout).toContain("set 5");
    expect(stdout).toContain("left 0");
  });

  test("an id that is not a number, a secret name unsafe in a shell, or one App for both is refused", async () => {
    const cases: [Record<string, string>, string][] = [
      [{ LEGION_E2E_IMPLEMENT_APP_ID: "12a" }, "LEGION_E2E_IMPLEMENT_APP_ID must be"],
      [{ LEGION_E2E_REVIEW_APP_SECRET: "APP;true" }, "LEGION_E2E_REVIEW_APP_SECRET must be"],
      [{ LEGION_E2E_REVIEW_APP_ID: "101" }, "are both 101: the workflow needs two Apps"],
      [{ LEGION_E2E_REVIEW_APP_SECRET: "IMPLEMENT_APP" }, "are both IMPLEMENT_APP"],
    ];
    for (const [inputs, want] of cases) {
      const { code, stderr, work } = await githubApps({ ...appInputs, ...inputs }, proveBoth);
      expect(code).toBe(1);
      expect(stderr).toContain(want);
      expect(existsSync(join(work, "github-apps.yaml"))).toBe(false);
    }
  });

  test("a malformed input is refused without printing its value, a token-shaped one by its length", async () => {
    const pasted = `ghs_${"A1b2C3d4".repeat(10)}`;
    for (const [name, value] of [
      ["LEGION_E2E_IMPLEMENT_APP_SECRET", pasted],
      ["LEGION_E2E_REVIEW_APP_ID", "1234567890123"],
    ]) {
      const { code, stderr } = await githubApps({ ...appInputs, [name]: value }, proveBoth);
      expect(code).toBe(1);
      expect(stderr).toContain(`${name} must be`);
      expect(stderr).not.toContain(value.slice(0, 13));
    }
  });

  test("app_token_command prints the broker client's command, its owner and read-only words left to the daemon's sh", async () => {
    const { code, stdout, stderr } = await githubApps(
      appInputs,
      `require_app_inputs implement review
printf 'command %s\\n' "$(app_token_command "$implement_app_secret")"`
    );
    expect(stderr).toBe("");
    expect(code).toBe(0);
    expect(line(stdout, "command ")).toBe(tokenCommand("IMPLEMENT_APP"));
  });

  test("the token command asks agent-secrets for the owner and read-only flag the daemon names, and prints the release", async () => {
    const argvLog = join(dir, "argv-command");
    const cases: [Record<string, string>, string][] = [
      [{}, "IMPLEMENT_APP --reason=legion-live-proof-github-app-token -- sh -c printf %s \"$IMPLEMENT_APP\""],
      [
        { LEGION_GITHUB_OWNER: "Acme-Org" },
        "IMPLEMENT_APP --reason=legion-live-proof-github-app-token --github-owner Acme-Org -- sh -c printf %s \"$IMPLEMENT_APP\"",
      ],
      [
        { LEGION_GITHUB_OWNER: "Acme-Org", LEGION_GITHUB_READ_ONLY: "1" },
        "IMPLEMENT_APP --reason=legion-live-proof-github-app-token --github-owner Acme-Org --github-read-only -- sh -c printf %s \"$IMPLEMENT_APP\"",
      ],
      [
        { LEGION_GITHUB_OWNER: "", LEGION_GITHUB_READ_ONLY: "" },
        "IMPLEMENT_APP --reason=legion-live-proof-github-app-token -- sh -c printf %s \"$IMPLEMENT_APP\"",
      ],
    ];
    for (const [daemonEnv, argv] of cases) {
      rmSync(argvLog, { force: true });
      const child = Bun.spawnSync(["sh", "-c", tokenCommand("IMPLEMENT_APP")], {
        env: { PATH: `${fakeBin}:${process.env.PATH}`, ARGV_LOG: argvLog, ...daemonEnv },
        stdout: "pipe",
        stderr: "pipe",
      });
      expect(child.stderr.toString()).toBe("");
      expect(child.exitCode).toBe(0);
      expect(child.stdout.toString()).toBe(readFileSync(join(store, "IMPLEMENT_APP"), "utf8"));
      expect(readFileSync(argvLog, "utf8")).toBe(`${argv}\n`);
    }
  });

  test("matching Apps pass through their token commands, and the block written is the daemon's", async () => {
    const { code, stdout, stderr, work } = await githubApps(appInputs, proveBoth);
    expect(stderr).toBe("");
    expect(code).toBe(0);
    expect(JSON.parse(line(stdout, "logins "))).toEqual({
      implement: "example-implement[bot]",
      review: "example-review[bot]",
    });
    expect(readFileSync(join(work, "github-apps.yaml"), "utf8")).toBe(
      `github_apps:\n  implement:\n    app_id: "101"\n    token_command: ${JSON.stringify(tokenCommand("IMPLEMENT_APP"))}\n  review:\n    app_id: "202"\n    token_command: ${JSON.stringify(tokenCommand("REVIEW_APP"))}\n`
    );
    holdsNoToken(stdout);
  });

  test("a secret that releases another App's tokens fails the check, naming both ids and no token", async () => {
    const { code, stdout, stderr } = await githubApps(
      { ...appInputs, LEGION_E2E_REVIEW_APP_ID: "404", LEGION_E2E_REVIEW_APP_SECRET: "OTHER_APP" },
      proveBoth
    );
    expect(code).toBe(1);
    expect(stdout).not.toContain("logins");
    expect(stderr).toContain("answered App id 405, want 404");
    expect(stderr).toMatch(/^FAIL prerequisites: lib\/app-logins.ts could not prove/m);
    holdsNoToken(stderr);
  });

  test("one role proves that App alone, as Stage 4a runs it", async () => {
    const { code, stdout, stderr } = await githubApps(
      { LEGION_E2E_IMPLEMENT_APP_ID: "101", LEGION_E2E_IMPLEMENT_APP_SECRET: "IMPLEMENT_APP" },
      `require_app_inputs implement
write_github_apps implement
check_github_apps implement
echo "logins $github_app_logins"`
    );
    expect(stderr).toBe("");
    expect(code).toBe(0);
    expect(stdout).toContain('logins {"implement":"example-implement[bot]"}');
  });

  // The refresher as the sandbox proof runs it under bridge: each role's releases minted here, into
  // owner-only files of a directory the caller names, and the block's token commands then read
  // those files alone.
  test("mint_app_tokens writes each release to an owner-only file atomically, one a role, owner and flag", async () => {
    const tokenDir = mkdtempSync(join(dir, "tokens."));
    const argvLog = join(dir, "argv-mint");
    rmSync(argvLog, { force: true });
    const { code, stdout, stderr } = await githubApps(
      { ...appInputs, TOKEN_DIR: tokenDir, ARGV_LOG: argvLog },
      `require_app_inputs implement review
mint_app_tokens "$TOKEN_DIR" SJawhar acme-org
echo minted`
    );
    expect(stderr).toBe("");
    expect(code).toBe(0);
    expect(stdout).toContain("minted");
    const files = ["_all.json", "acme-org.json", "acme-org.read.json", "sjawhar.json", "sjawhar.read.json"];
    for (const [role, secret] of [
      ["implement", "IMPLEMENT_APP"],
      ["review", "REVIEW_APP"],
    ] as const) {
      expect(statSync(join(tokenDir, role)).mode & 0o777).toBe(0o700);
      // No temporary file is left beside them.
      expect(readdirSync(join(tokenDir, role)).sort()).toEqual(files);
      for (const file of files) {
        const path = join(tokenDir, role, file);
        expect(statSync(path).mode & 0o777).toBe(0o600);
        expect(readFileSync(path, "utf8")).toBe(readFileSync(join(store, secret), "utf8"));
      }
    }
    const reason = "--reason=legion-live-proof-github-app-token";
    const asked = readFileSync(argvLog, "utf8")
      .trim()
      .split("\n")
      .map((argv) => argv.replace(/ -- sh -c .*$/, ""));
    expect(asked).toEqual(
      ["IMPLEMENT_APP", "REVIEW_APP"].flatMap((secret) => [
        `${secret} ${reason}`,
        `${secret} ${reason} --github-owner sjawhar`,
        `${secret} ${reason} --github-owner sjawhar --github-read-only`,
        `${secret} ${reason} --github-owner acme-org`,
        `${secret} ${reason} --github-owner acme-org --github-read-only`,
      ])
    );
    holdsNoToken(stdout + stderr);
  });

  test("a mint that fails fails the run naming the App, leaves no file of it and keeps the last good release", async () => {
    const tokenDir = mkdtempSync(join(dir, "tokens."));
    mkdirSync(join(tokenDir, "implement"), { mode: 0o700 });
    writeFileSync(join(tokenDir, "implement", "sjawhar.read.json"), "last good", { mode: 0o600 });
    const { code, stdout, stderr } = await githubApps(
      { ...appInputs, TOKEN_DIR: tokenDir, FAIL_MINT: "sjawhar/1" },
      `require_app_inputs implement review
mint_app_tokens "$TOKEN_DIR" sjawhar
echo minted`
    );
    expect(code).toBe(1);
    expect(stdout).not.toContain("minted");
    expect(stderr).toContain("grant for IMPLEMENT_APP denied");
    expect(stderr).toMatch(
      /^FAIL prerequisites: the implement App \(App 101, github-app secret IMPLEMENT_APP\) gave no release for .*sjawhar\.read\.json: agent-secrets exited 3; no token is printed$/m
    );
    expect(readdirSync(join(tokenDir, "implement")).sort()).toEqual([
      "_all.json",
      "sjawhar.json",
      "sjawhar.read.json",
    ]);
    expect(readFileSync(join(tokenDir, "implement", "sjawhar.read.json"), "utf8")).toBe("last good");
    // The run stopped at the first failure: the review App was never minted.
    expect(existsSync(join(tokenDir, "review"))).toBe(false);
    holdsNoToken(stdout + stderr);
  });

  test("a mint that prints nothing is refused, leaving no file", async () => {
    const tokenDir = mkdtempSync(join(dir, "tokens."));
    const { code, stderr } = await githubApps(
      { ...appInputs, TOKEN_DIR: tokenDir, EMPTY_MINT: "1" },
      `require_app_inputs implement review
mint_app_tokens "$TOKEN_DIR" sjawhar`
    );
    expect(code).toBe(1);
    expect(stderr).toMatch(/^FAIL prerequisites: the implement App \(App 101, .*_all\.json: agent-secrets printed nothing$/m);
    expect(readdirSync(join(tokenDir, "implement"))).toEqual([]);
  });

  test("an oversized release returns the documented overflow status and leaves no file", async () => {
    const tokenDir = mkdtempSync(join(dir, "tokens."));
    const { code, stdout, stderr } = await githubApps(
      { ...appInputs, TOKEN_DIR: tokenDir, OVERSIZED_MINT: "1" },
      `set +e
app_token_mint_file "$TOKEN_DIR/release.json" "" "" IMPLEMENT_APP
status=$?
set -e
printf 'status %s\n' "$status"`
    );
    expect(code).toBe(0);
    expect(stdout).toBe("status 126");
    expect(stderr).toBe("");
    expect(existsSync(join(tokenDir, "release.json"))).toBe(false);
  });

  test("a release whose rename fails leaves no temporary file", async () => {
    const tokenDir = mkdtempSync(join(dir, "tokens."));
    const failingMv = mkdtempSync(join(dir, "mv."));
    writeFileSync(join(failingMv, "mv"), "#!/bin/sh\nexit 1\n", { mode: 0o755 });
    const { code, stdout, stderr } = await githubApps(
      { ...appInputs, TOKEN_DIR: tokenDir, FAILING_MV: failingMv },
      `require_app_inputs implement review
PATH="$FAILING_MV:$PATH"
mint_app_tokens "$TOKEN_DIR" sjawhar`
    );
    expect(code).toBe(1);
    expect(readdirSync(join(tokenDir, "implement"))).toEqual([]);
    holdsNoToken(stdout + stderr);
  });

  test("the refresher mints the files again on its period and stops when told", async () => {
    const tokenDir = mkdtempSync(join(dir, "tokens."));
    const refreshStore = mkdtempSync(join(dir, "store."));
    for (const secret of ["IMPLEMENT_APP", "REVIEW_APP"]) {
      copyFileSync(join(store, secret), join(refreshStore, secret));
    }
    const { code, stdout, stderr } = await githubApps(
      { ...appInputs, TOKEN_DIR: tokenDir, STORE: refreshStore },
      `require_app_inputs implement review
mint_app_tokens "$TOKEN_DIR" sjawhar
app_token_refresh_seconds=1
start_app_token_refresher "$TOKEN_DIR"
printf '%s' '${release({ sjawhar: "ghs_rotated" })}' >"$STORE/IMPLEMENT_APP"
for _ in $(seq 50); do
  grep -q ghs_rotated "$TOKEN_DIR/implement/sjawhar.read.json" && break
  sleep 0.1
done
pid=$app_token_refresher
stop_app_token_refresher
kill -0 "$pid" 2>/dev/null && echo "still running" || echo "stopped"
echo "read $(cat "$TOKEN_DIR/implement/sjawhar.read.json")"`
    );
    expect(stderr).toBe("");
    expect(code).toBe(0);
    expect(stdout).toContain("stopped");
    expect(line(stdout, "read ")).toBe(release({ sjawhar: "ghs_rotated" }));
    expect(readdirSync(join(tokenDir, "implement")).sort()).toEqual([
      "_all.json",
      "sjawhar.json",
      "sjawhar.read.json",
    ]);
  });

  test("write_github_apps --token-dir writes commands that read the file of the owner and flag asked, in busybox sh", async () => {
    const tokenDir = mkdtempSync(join(dir, "tokens."));
    mkdirSync(join(tokenDir, "implement"));
    for (const file of ["_all", "sjawhar", "sjawhar.read"]) {
      writeFileSync(join(tokenDir, "implement", `${file}.json`), `file ${file}`);
    }
    const { code, stderr, work } = await githubApps(
      { ...appInputs, TOKEN_DIR: tokenDir },
      `require_app_inputs implement review
write_github_apps --token-dir "$TOKEN_DIR" implement review`
    );
    expect(stderr).toBe("");
    expect(code).toBe(0);
    const sources = appTokenSources(readFileSync(join(work, "github-apps.yaml"), "utf8"));
    expect(sources.implement?.appId).toBe("101");
    expect(sources.review?.tokenCommand).toContain(`'${tokenDir}/review/'`);
    const command = sources.implement?.tokenCommand ?? "";
    for (const [daemonEnv, want] of [
      [{ LEGION_GITHUB_OWNER: "", LEGION_GITHUB_READ_ONLY: "" }, "file _all"],
      [{ LEGION_GITHUB_OWNER: "SJawhar", LEGION_GITHUB_READ_ONLY: "" }, "file sjawhar"],
      [{ LEGION_GITHUB_OWNER: "sjawhar", LEGION_GITHUB_READ_ONLY: "1" }, "file sjawhar.read"],
    ] as const) {
      const ran = Bun.spawnSync([...busyboxSh, "-c", command], {
        env: { PATH: process.env.PATH, ...daemonEnv },
        stdout: "pipe",
        stderr: "pipe",
      });
      expect(ran.stderr.toString()).toBe("");
      expect(ran.exitCode).toBe(0);
      expect(ran.stdout.toString()).toBe(want);
    }
    // An owner the file layout could not name reads no file outside the directory.
    const escape = Bun.spawnSync([...busyboxSh, "-c", command], {
      env: { PATH: process.env.PATH, LEGION_GITHUB_OWNER: "../review/x" },
      stdout: "pipe",
      stderr: "pipe",
    });
    expect(escape.exitCode).toBe(2);
    expect(escape.stdout.toString()).toBe("");
  });

  test("the refresher's files and the --token-dir block are proved alone, agent-secrets broken after the mint", async () => {
    const tokenDir = mkdtempSync(join(dir, "tokens."));
    const brokenBin = mkdtempSync(join(dir, "broken-bin."));
    writeFileSync(join(brokenBin, "agent-secrets"), "#!/bin/sh\nexit 7\n", { mode: 0o755 });
    const { code, stdout, stderr } = await githubApps(
      { ...appInputs, TOKEN_DIR: tokenDir, BROKEN_BIN: brokenBin },
      `require_app_inputs implement review
mint_app_tokens "$TOKEN_DIR" sjawhar
PATH="$BROKEN_BIN:$PATH"
write_github_apps --token-dir "$TOKEN_DIR" implement review
check_github_apps implement review
echo "logins $github_app_logins"`
    );
    expect(stderr).toBe("");
    expect(code).toBe(0);
    expect(JSON.parse(line(stdout, "logins "))).toEqual({
      implement: "example-implement[bot]",
      review: "example-review[bot]",
    });
  });

  test("a token directory the token commands could not carry in their shell text is refused, and nothing is written", async () => {
    for (const name of ["tokens 'x", "tokens$x", "tokens;x"]) {
      const tokenDir = join(mkdtempSync(join(dir, "tokens.")), name);
      mkdirSync(tokenDir);
      for (const then of [
        `require_app_inputs implement review
mint_app_tokens "$TOKEN_DIR" sjawhar
echo "wrote"`,
        `require_app_inputs implement review
write_github_apps --token-dir "$TOKEN_DIR" implement review
echo "wrote"`,
      ]) {
        const { code, stdout, stderr, work } = await githubApps(
          { ...appInputs, TOKEN_DIR: tokenDir },
          then
        );
        expect(code).toBe(1);
        expect(stdout).not.toContain("wrote");
        expect(stderr).toContain("holds a character other than letters, digits and _ . / -");
        expect(readdirSync(tokenDir)).toEqual([]);
        expect(existsSync(join(work, "github-apps.yaml"))).toBe(false);
      }
    }
  });
});
