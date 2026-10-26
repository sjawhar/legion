import { afterAll, describe, expect, test } from "bun:test";
import { createVerify, generateKeyPairSync } from "node:crypto";
import {
  copyFileSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { appJWT, appKeySources } from "./app-logins";

// lib/app-logins.ts run as the stage proofs run it, against a stand-in GitHub that answers GET /app
// with the id, slug and permissions of the App whose key signed the JWT (no slug for an App that
// has none, and another App's id for the one App whose answer names one), and refuses any other
// request. The implement App holds checks: read alone, as the real one does.
const dir = mkdtempSync(join(tmpdir(), "app-logins-test."));
const script = join(import.meta.dir, "app-logins.ts");

function appKey(): { publicKey: string; privateKey: string } {
  return generateKeyPairSync("rsa", {
    modulusLength: 2048,
    publicKeyEncoding: { type: "spki", format: "pem" },
    privateKeyEncoding: { type: "pkcs1", format: "pem" },
  });
}
interface StandInApp {
  slug?: string;
  /** The App id GET /app answers, when it is not the JWT's issuer. */
  answers?: number;
  permissions: Record<string, string>;
  key: { publicKey: string; privateKey: string };
}
const implementPermissions = { contents: "write", checks: "read", pull_requests: "write" };
const reviewPermissions = { contents: "write", checks: "write", pull_requests: "write" };
const apps: Record<string, StandInApp> = {
  "101": { slug: "example-implement", permissions: implementPermissions, key: appKey() },
  "202": { slug: "example-review", permissions: reviewPermissions, key: appKey() },
  "303": { permissions: reviewPermissions, key: appKey() },
  "404": { slug: "example-other", answers: 405, permissions: reviewPermissions, key: appKey() },
};
for (const id of Object.keys(apps))
  writeFileSync(join(dir, `key-${id}.pem`), apps[id].key.privateKey);

const seen: { path: string; iss: string; iat: number; exp: number }[] = [];
const github = Bun.serve({
  port: 0,
  fetch(request) {
    const path = new URL(request.url).pathname;
    const jwt = (request.headers.get("authorization") ?? "").replace(/^Bearer /, "");
    const [header, claims, signature] = jwt.split(".");
    const decoded = JSON.parse(Buffer.from(claims ?? "", "base64url").toString());
    seen.push({ path, iss: decoded.iss, iat: decoded.iat, exp: decoded.exp });
    const app = apps[decoded.iss];
    const signed =
      app !== undefined &&
      createVerify("RSA-SHA256")
        .update(`${header}.${claims}`)
        .verify(app.key.publicKey, Buffer.from(signature ?? "", "base64url"));
    if (path !== "/app" || !signed) {
      return Response.json({ message: "A JSON web token could not be decoded" }, { status: 401 });
    }
    return Response.json({
      id: app.answers ?? Number(decoded.iss),
      slug: app.slug,
      permissions: app.permissions,
    });
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

let configs = 0;
function config(implement: string, review: string): string {
  const path = join(dir, `legion-${++configs}.yaml`);
  writeFileSync(
    path,
    `project: ACME\ngithub_apps:\n  implement:\n${implement}\n  review:\n${review}\n`
  );
  return path;
}
const app = (id: string, command = `cat ${join(dir, `key-${id}.pem`)}`) =>
  `    app_id: "${id}"\n    private_key_command: ${JSON.stringify(command)}`;

// The run is asynchronous: the stand-in GitHub answers from this test's own event loop. `env` is
// added to this test's own environment. The helper is given stdin, so a key command that inherited
// it would read `stdin-leak` where the closed-stdin case expects nothing.
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

describe("app-logins.ts", () => {
  test("prints each App's bot login from the slug GET /app answers under that App's own JWT", async () => {
    seen.length = 0;
    const { code, stdout, stderr } = await run(config(app("101"), app("202")));
    expect(stderr).toBe("");
    expect(code).toBe(0);
    expect(JSON.parse(stdout)).toEqual({
      implement: "example-implement[bot]",
      review: "example-review[bot]",
    });
    expect(seen.map(({ path, iss }) => `${path} ${iss}`)).toEqual(["/app 101", "/app 202"]);
    for (const { iat, exp } of seen) expect(exp - iat).toBe(660);
  });

  test("a key GitHub does not take is named by its role and GitHub's status", async () => {
    const { code, stdout, stderr } = await run(
      config(app("101", `cat ${join(dir, "key-202.pem")}`), app("202"))
    );
    expect(code).toBe(1);
    expect(stdout).toBe("");
    expect(stderr).toMatch(namesApp("implement", "101"));
    expect(stderr).toContain("401");
  });

  test("an answer with no slug is named by the App's role and app id", async () => {
    const { code, stdout, stderr } = await run(config(app("101"), app("303")));
    expect(code).toBe(1);
    expect(stdout).toBe("");
    expect(stderr).toMatch(namesApp("review", "303"));
  });

  test("a GitHub that never answers is named by the App's role and app id", async () => {
    const { code, stdout, stderr } = await run(config(app("101"), app("202")), {}, unreachable);
    expect(code).toBe(1);
    expect(stdout).toBe("");
    expect(stderr).toMatch(namesApp("implement", "101"));
  });

  test("a key command that fails is named by its field and exit code, its stdout never quoted", async () => {
    const { code, stderr } = await run(config(app("101", "echo not-a-key; exit 3"), app("202")));
    expect(code).toBe(1);
    expect(stderr).toBe("app-logins: github_apps.implement.private_key_command failed (exit 3)");
  });

  test("a key command runs as the daemon runs it: no NATS seed, stdin closed, stderr in the failure", async () => {
    // An unset variable prints empty, so the two seeds read `||` and the _FILE pointer stays.
    const probe =
      'printf "%s|%s|%s|%s" "$NATS_NKEY_SEED" "$NATS_DAEMON_NKEY_SEED" "$NATS_NKEY_SEED_FILE" "$(cat)" >&2; exit 4';
    const { code, stderr } = await run(config(app("101", probe), app("202")), {
      NATS_NKEY_SEED: "pane-seed",
      NATS_DAEMON_NKEY_SEED: "daemon-seed",
      NATS_NKEY_SEED_FILE: "/seed-file",
    });
    expect(code).toBe(1);
    expect(stderr).toBe(
      "app-logins: github_apps.implement.private_key_command failed (exit 4): ||/seed-file|"
    );
  });

  test("two roles on one App are refused, as the daemon refuses them at boot", async () => {
    const { code, stderr } = await run(config(app("202"), app("202")));
    expect(code).toBe(1);
    expect(stderr).toContain("both github_apps roles are the App example-review[bot]");
  });

  test("an answer naming another App id is refused, naming both ids and never the key", async () => {
    const { code, stdout, stderr } = await run(config(app("101"), app("404")));
    expect(code).toBe(1);
    expect(stdout).toBe("");
    expect(stderr).toMatch(namesApp("review", "404"));
    expect(stderr).toContain("answered App id 405, want 404");
    expect(stderr).not.toContain("PRIVATE KEY");
  });

  test("an App without its role's write permission is refused, the two Apps swapped whole too", async () => {
    const { code, stdout, stderr } = await run(config(app("202"), app("101")));
    expect(code).toBe(1);
    expect(stdout).toBe("");
    expect(stderr).toMatch(namesApp("review", "101"));
    expect(stderr).toContain('answered permissions.checks "read", want write');
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

  test("an App whose key is not a private_key_command is refused by name", () => {
    expect(() =>
      appKeySources(
        'github_apps:\n  implement: { app_id: "1", private_key_secret: KEY }\n  review: { app_id: "2", private_key_command: "true" }\n'
      )
    ).toThrow("github_apps.implement names its key without a private_key_command");
    expect(() => appKeySources("project: ACME\n")).toThrow("the config has no github_apps");
  });

  test("the JWT is the daemon's: RS256, issued a minute back, ten minutes of life", () => {
    const jwt = appJWT("101", apps["101"].key.privateKey, new Date("2026-10-16T06:00:00Z"));
    const [header, claims] = jwt
      .split(".")
      .slice(0, 2)
      .map((part) => JSON.parse(Buffer.from(part, "base64url").toString()));
    const at = Date.parse("2026-10-16T06:00:00Z") / 1000;
    expect(header).toEqual({ alg: "RS256", typ: "JWT" });
    expect(claims).toEqual({ iss: "101", iat: at - 60, exp: at + 600 });
  });
});

describe("read_app_logins", () => {
  test("gives each role its App's login as GET /app answered it, and GraphQL the bare slug", async () => {
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
// handed the stand-in GitHub after the helper's own arguments, and a stand-in `secrets` first on
// PATH, which puts the store's value for the key it names (the App's PEM in the store's Node-style
// base64: URL-safe alphabet, unpadded) into the environment of the command after `--`, as the real
// CLI does.
const store = join(dir, "store");
const fakeBin = join(dir, "bin");
mkdirSync(store);
mkdirSync(fakeBin);
writeFileSync(
  join(store, "IMPLEMENT_KEY"),
  Buffer.from(apps["101"].key.privateKey).toString("base64url")
);
writeFileSync(
  join(store, "REVIEW_KEY"),
  Buffer.from(apps["202"].key.privateKey).toString("base64url")
);
writeFileSync(
  join(store, "OTHER_KEY"),
  Buffer.from(apps["404"].key.privateKey).toString("base64url")
);
writeFileSync(
  join(fakeBin, "secrets"),
  `#!/bin/sh\nname=$1\nshift\n[ "$1" = -- ] && shift\nexport "$name=$(cat "${store}/$name")"\nexec "$@"\n`,
  { mode: 0o755 }
);
const appInputs = {
  LEGION_E2E_IMPLEMENT_APP_ID: "101",
  LEGION_E2E_IMPLEMENT_APP_KEY_NAME: "IMPLEMENT_KEY",
  LEGION_E2E_REVIEW_APP_ID: "202",
  LEGION_E2E_REVIEW_APP_KEY_NAME: "REVIEW_KEY",
};

async function githubApps(inputs: Record<string, string | undefined>, then: string) {
  const work = mkdtempSync(join(dir, "apps."));
  const env: Record<string, string | undefined> = {
    ...process.env,
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

describe("lib/github-apps.sh", () => {
  const proveBoth = `require_app_inputs implement review
write_github_apps implement review
check_github_apps implement review
echo "logins $github_app_logins"`;

  test("an input left unset takes this deployment's documented default, and sets no other role's", async () => {
    seen.length = 0;
    const { code, stdout, stderr, work } = await githubApps(
      {
        LEGION_E2E_IMPLEMENT_APP_ID: undefined,
        LEGION_E2E_IMPLEMENT_APP_KEY_NAME: undefined,
        LEGION_E2E_REVIEW_APP_ID: "202",
        LEGION_E2E_REVIEW_APP_KEY_NAME: "REVIEW_KEY",
      },
      `require_app_inputs implement review
write_github_apps implement review
echo "defaults $default_implement_app_id $default_implement_app_key_name"`
    );
    expect(stderr).toBe("");
    expect(code).toBe(0);
    // No input named the implement App, so the block must carry the lib's own default pair, which
    // this test never spells out: the lib is the one place this deployment's values live.
    const defaults = (stdout.split("\n").find((l) => l.startsWith("defaults ")) ?? "").split(" ");
    const sources = appKeySources(readFileSync(join(work, "github-apps.yaml"), "utf8"));
    expect(sources.implement?.appId).toBe(defaults[1]);
    expect(sources.implement?.privateKeyCommand).toStartWith(`secrets ${defaults[2]} -- sh -c '`);
    expect(sources.review?.appId).toBe("202");
    // The default pair is never asked of GitHub by this test: check_github_apps did not run.
    expect(seen).toEqual([]);
  });

  test("an input set to an empty value is refused, not read as a choice of the default", async () => {
    const { code, stderr } = await githubApps(
      { ...appInputs, LEGION_E2E_REVIEW_APP_ID: "" },
      proveBoth
    );
    expect(code).toBe(1);
    expect(stderr).toContain("LEGION_E2E_REVIEW_APP_ID must be");
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

  test("app_key_unsets strips each role's key, this deployment's own two, and every PRIVATE_KEY variable", async () => {
    // Each name is exported first, so the strip list has something to remove: without it the
    // child would print them all.
    const { code, stdout, stderr } = await githubApps(
      { ...appInputs, SOME_OTHER_APP_PRIVATE_KEY_B64: "irrelevant" },
      `export IMPLEMENT_KEY=x REVIEW_KEY=x
require_app_inputs implement review
export "$default_implement_app_key_name=x" "$default_review_app_key_name=x"
names="IMPLEMENT_KEY|REVIEW_KEY|SOME_OTHER_APP_PRIVATE_KEY_B64|$default_implement_app_key_name|$default_review_app_key_name"
echo "set $(env | grep -c -E "^($names)=")"
env "\${app_key_unsets[@]}" sh -c "echo \\"left \\$(env | grep -c -E '^($names)=' || true)\\""`
    );
    expect(stderr).toBe("");
    expect(code).toBe(0);
    expect(stdout).toContain("set 5");
    expect(stdout).toContain("left 0");
  });

  test("an id that is not a number, a key name unsafe in a shell, or one App for both is refused", async () => {
    const cases: [Record<string, string>, string][] = [
      [{ LEGION_E2E_IMPLEMENT_APP_ID: "12a" }, "LEGION_E2E_IMPLEMENT_APP_ID must be"],
      [{ LEGION_E2E_REVIEW_APP_KEY_NAME: "KEY;true" }, "LEGION_E2E_REVIEW_APP_KEY_NAME must be"],
      [{ LEGION_E2E_REVIEW_APP_ID: "101" }, "are both 101: the workflow needs two Apps"],
      [{ LEGION_E2E_REVIEW_APP_KEY_NAME: "IMPLEMENT_KEY" }, "are both IMPLEMENT_KEY"],
    ];
    for (const [inputs, want] of cases) {
      const { code, stderr, work } = await githubApps({ ...appInputs, ...inputs }, proveBoth);
      expect(code).toBe(1);
      expect(stderr).toContain(want);
      expect(existsSync(join(work, "github-apps.yaml"))).toBe(false);
    }
  });

  test("a malformed input is refused without printing its value, a key-shaped one by its length", async () => {
    const pasted = readFileSync(join(store, "IMPLEMENT_KEY"), "utf8")
      .trim()
      .replace(/[^A-Za-z0-9_]/g, "A");
    expect(pasted.length).toBeGreaterThan(64);
    for (const [name, value] of [
      ["LEGION_E2E_IMPLEMENT_APP_KEY_NAME", pasted],
      ["LEGION_E2E_REVIEW_APP_ID", "1234567890123"],
    ]) {
      const { code, stderr } = await githubApps({ ...appInputs, [name]: value }, proveBoth);
      expect(code).toBe(1);
      expect(stderr).toContain(`${name} must be`);
      expect(stderr).not.toContain(value.slice(0, 13));
    }
  });

  test("a key GitHub answers for another App id fails the check, naming both ids and never the key", async () => {
    const { code, stdout, stderr } = await githubApps(
      {
        ...appInputs,
        LEGION_E2E_REVIEW_APP_ID: "404",
        LEGION_E2E_REVIEW_APP_KEY_NAME: "OTHER_KEY",
      },
      proveBoth
    );
    expect(code).toBe(1);
    expect(stdout).not.toContain("logins");
    expect(stderr).toContain("answered App id 405, want 404");
    expect(stderr).toMatch(/^FAIL prerequisites: lib\/app-logins.ts could not prove/m);
    expect(stderr).not.toContain(readFileSync(join(store, "OTHER_KEY"), "utf8"));
    expect(stderr).not.toContain("PRIVATE KEY");
  });

  test("a key of another App is refused by GitHub's 401, naming the App's role and id", async () => {
    const { code, stderr } = await githubApps(
      { ...appInputs, LEGION_E2E_IMPLEMENT_APP_KEY_NAME: "OTHER_KEY" },
      proveBoth
    );
    expect(code).toBe(1);
    expect(stderr).toMatch(namesApp("implement", "101"));
    expect(stderr).toContain("401");
  });

  test("matching Apps pass, and the block written is the daemon's: each id and its secrets command", async () => {
    const { code, stdout, stderr, work } = await githubApps(appInputs, proveBoth);
    expect(stderr).toBe("");
    expect(code).toBe(0);
    const logins = stdout.split("\n").find((line) => line.startsWith("logins "));
    expect(JSON.parse((logins ?? "").slice("logins ".length))).toEqual({
      implement: "example-implement[bot]",
      review: "example-review[bot]",
    });
    const sources = appKeySources(readFileSync(join(work, "github-apps.yaml"), "utf8"));
    expect(sources.implement?.appId).toBe("101");
    expect(sources.implement?.privateKeyCommand).toStartWith("secrets IMPLEMENT_KEY -- sh -c '");
    expect(sources.review?.appId).toBe("202");
    expect(sources.review?.privateKeyCommand).toStartWith("secrets REVIEW_KEY -- sh -c '");
  });

  test("one role proves that App alone, as Stage 4a runs it", async () => {
    const { code, stdout, stderr } = await githubApps(
      { LEGION_E2E_IMPLEMENT_APP_ID: "101", LEGION_E2E_IMPLEMENT_APP_KEY_NAME: "IMPLEMENT_KEY" },
      `require_app_inputs implement
write_github_apps implement
check_github_apps implement
echo "logins $github_app_logins"`
    );
    expect(stderr).toBe("");
    expect(code).toBe(0);
    expect(stdout).toContain('logins {"implement":"example-implement[bot]"}');
  });
});
