import { afterAll, describe, expect, test } from "bun:test";
import { createVerify, generateKeyPairSync } from "node:crypto";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { appJWT, appKeySources } from "./app-logins";

// lib/app-logins.ts run as the stage proofs run it, against a stand-in GitHub that answers GET /app
// with the slug of the App whose key signed the JWT, and refuses any other request.
const dir = mkdtempSync(join(tmpdir(), "app-logins-test."));
const script = join(import.meta.dir, "app-logins.ts");

function appKey(): { publicKey: string; privateKey: string } {
  return generateKeyPairSync("rsa", {
    modulusLength: 2048,
    publicKeyEncoding: { type: "spki", format: "pem" },
    privateKeyEncoding: { type: "pkcs1", format: "pem" },
  });
}
const apps: Record<string, { slug: string; key: { publicKey: string; privateKey: string } }> = {
  "101": { slug: "example-implement", key: appKey() },
  "202": { slug: "example-review", key: appKey() },
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
    return Response.json({ id: Number(decoded.iss), slug: app.slug });
  },
});
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
async function run(path: string, env: Record<string, string> = {}) {
  const child = Bun.spawn(["bun", script, path, `http://127.0.0.1:${github.port}/`], {
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
    expect(stderr).toContain("GET /app as the implement App (app_id 101) answered 401");
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
