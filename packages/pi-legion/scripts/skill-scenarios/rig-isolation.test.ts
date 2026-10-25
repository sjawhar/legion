import { afterAll, expect, setDefaultTimeout, test } from "bun:test";
import {
  chmodSync,
  copyFileSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  realpathSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { homedir, tmpdir } from "node:os";
import path from "node:path";

// Two runs of one batch, started at the same time as the batch's xargs starts them:
// `rig.sh run askgate on <n> <index> <start>`. The rig.sh under test is this one, copied into a
// checkout that holds its e2e helpers and stand-ins for what a run reaches:
//   - docker: the batch's Postgres container;
//   - packages/dispatch/e2e/run-server.sh: a server that answers every request with the database
//     its DATABASE_URL names;
//   - seed.ts: records the Dispatch that each seed reached, and writes the fixture;
//   - tmux: runs the pane's command at once;
//   - mise: the agent, which records the Dispatch variables of its environment.
// Each run must get a database of its own, a server of its own and a DISPATCH_URL of its own, and
// must stop its server when it ends.
setDefaultTimeout(120_000);

const root = path.resolve(import.meta.dir, "../../../..");
const scratch = realpathSync(mkdtempSync(path.join(tmpdir(), "skill-scenarios-isolation-")));
afterAll(() => rmSync(scratch, { recursive: true, force: true }));

/** Writes `text` to `file`, making its directory, and makes it executable when `mode` says so. */
function put(file: string, text: string, mode = 0o644) {
  mkdirSync(path.dirname(file), { recursive: true });
  writeFileSync(file, text);
  chmodSync(file, mode);
}

// The checkout: this rig.sh and the e2e helpers it sources, with the two Dispatch stand-ins.
const co = path.join(scratch, "checkout");
const rig = path.join(co, "packages/pi-legion/scripts/skill-scenarios/rig.sh");
mkdirSync(path.dirname(rig), { recursive: true });
copyFileSync(path.join(import.meta.dir, "rig.sh"), rig);
for (const lib of ["rig.sh", "free-port.sh"]) {
  mkdirSync(path.join(co, "scripts/e2e/lib"), { recursive: true });
  copyFileSync(path.join(root, "scripts/e2e/lib", lib), path.join(co, "scripts/e2e/lib", lib));
}
const stub = path.join(scratch, "stub");
mkdirSync(stub);
put(
  path.join(co, "packages/dispatch/e2e/run-server.sh"),
  [
    "#!/usr/bin/env bash",
    'printf \'%s %s %s\\n\' "$$" "$DISPATCH_E2E_PORT" "$DATABASE_URL" >>"$STUB_DIR/servers.log"',
    'exec bun "$STUB_DIR/server.ts"',
    "",
  ].join("\n"),
  0o755
);
// A port another process took first ends the stub as it ends the Go server, with "address already
// in use", which start_dispatch answers by picking another port.
put(
  path.join(stub, "server.ts"),
  [
    'const database = new URL(process.env.DATABASE_URL ?? "").pathname.slice(1);',
    "try {",
    "  Bun.serve({",
    '    hostname: "127.0.0.1",',
    "    port: Number(process.env.DISPATCH_E2E_PORT),",
    "    fetch: () => Response.json({ database }),",
    "  });",
    "} catch {",
    '  console.error("listen: bind: address already in use");',
    "  process.exit(1);",
    "}",
    "",
  ].join("\n")
);
put(
  path.join(co, "packages/pi-legion/scripts/skill-scenarios/seed.ts"),
  [
    'import { appendFileSync, writeFileSync } from "node:fs";',
    'import path from "node:path";',
    "const [command, file, runDir] = Bun.argv.slice(2);",
    "const port = Number(process.env.DISPATCH_E2E_PORT);",
    'const answer = await fetch("http://127.0.0.1:" + port + "/api/v1/whoami");',
    "const { database } = await answer.json();",
    "appendFileSync(",
    '  path.join(process.env.STUB_DIR ?? "", "seeds.log"),',
    '  JSON.stringify({ command, port, database }) + "\\n"',
    ");",
    'if (command === "askgate")',
    "  writeFileSync(",
    '    file ?? "",',
    '    JSON.stringify({ issue: "LWEVAL-2", comment: "c1", comment_ref: "dispatch://LWEVAL-2/comment/c1" })',
    "  );",
    'if (command === "capture-askgate")',
    '  for (const name of ["asks.json", "artifacts.json"]) writeFileSync(path.join(runDir ?? "", name), "[]\\n");',
    "",
  ].join("\n")
);

// The stand-ins first on PATH.
const bin = path.join(scratch, "bin");
put(
  path.join(bin, "docker"),
  [
    "#!/usr/bin/env bash",
    'printf \'%s\\n\' "$*" >>"$STUB_DIR/docker.log"',
    'case $1 in port) echo "127.0.0.1:15432" ;; inspect) echo stub-postgres ;; exec) ;; *) exit 1 ;; esac',
    "",
  ].join("\n"),
  0o755
);
// tmux new-session -d -s <session> -x <w> -y <h> <command>: runs <command>, its last argument, at
// once, in the foreground.
put(
  path.join(bin, "tmux"),
  [
    "#!/usr/bin/env bash",
    "shift 2",
    '[ "$1" = new-session ] || exit 1',
    'for command in "$@"; do :; done',
    'bash -c "$command"',
    "",
  ].join("\n"),
  0o755
);
put(
  path.join(bin, "mise"),
  [
    "#!/usr/bin/env bash",
    'env | grep "^DISPATCH_" | sort >"$SKILL_SCENARIO_RUN/agent-env.txt"',
    "",
  ].join("\n"),
  0o755
);

// The work directory: the label's profile, and the batch's services file and Dispatch token.
const work = path.join(scratch, "work");
/** A label's built profile, with its Oh My Pi settings `settings` (the model route installers'). */
function label(name: string, settings: string) {
  const dir = path.join(work, "profiles", name);
  put(path.join(dir, `home/.omp/profiles/skills-${name}/agent/config.yml`), settings);
  for (const [file, text] of [
    ["checkout", co],
    ["pin", "omp@stub"],
    ["profile", `skills-${name}`],
    ["built-from", `checkout ${co}`],
    ["ready", ""],
  ])
    writeFileSync(path.join(dir, file), `${text}\n`);
}
label(
  "on",
  "modelRoles:\n  default: anthropic/m\nextensionHandlers:\n  toolCallTimeoutMs: 120000\n"
);
// A profile built before the installers raised the handler limit.
label("stale", "modelRoles:\n  default: anthropic/m\n");
put(path.join(work, "services/env"), "ENVOY_URL=http://127.0.0.1:1\n");
put(path.join(work, "services/dispatch-token"), "e2e-token\n");
/** The tag rig.sh names the work directory's containers with. */
const tag = `skill-scenarios-${new Bun.CryptoHasher("sha256").update(work).digest("hex").slice(0, 8)}`;

/** The caller's PATH behind the stand-ins, a HOME of the test's own, and the caller's mise data
 * and config directories: jq is a mise shim on a devbox, which finds nothing under a fresh HOME
 * without them. */
const env = {
  PATH: `${bin}:${process.env.PATH ?? ""}`,
  HOME: path.join(scratch, "home"),
  USER: process.env.USER ?? "rig",
  TMPDIR: scratch,
  MISE_DATA_DIR:
    process.env.MISE_DATA_DIR ??
    path.join(process.env.XDG_DATA_HOME ?? path.join(homedir(), ".local", "share"), "mise"),
  XDG_CONFIG_HOME: process.env.XDG_CONFIG_HOME ?? path.join(homedir(), ".config"),
  STUB_DIR: stub,
  SKILL_SCENARIOS_WORK: work,
  SKILL_SCENARIOS_ASKGATE_CASE: "facts",
};
mkdirSync(env.HOME);

const start = String(Math.floor(Date.now() / 1000));
const runs = await Promise.all(
  [1, 2].map(async (n) => {
    const child = Bun.spawn(["bash", rig, "run", "askgate", "on", String(n), String(n), start], {
      env,
      stdout: "pipe",
      stderr: "pipe",
    });
    const [code, stderr] = await Promise.all([child.exited, new Response(child.stderr).text()]);
    return { n, code, stderr, dir: path.join(work, "runs", `askgate-facts-on-${n}`) };
  })
);
const lines = (file: string) =>
  existsSync(file)
    ? readFileSync(file, "utf8")
        .split("\n")
        .filter((line) => line !== "")
    : [];
/** Each server a run started: its pid, its port and the database its DATABASE_URL names. */
const servers = lines(path.join(stub, "servers.log")).map((line) => {
  const [pid = "", port = "", url = ""] = line.split(" ");
  return { pid: Number(pid), port: Number(port), database: new URL(url).pathname.slice(1) };
});

test("both runs finish", () => {
  for (const { code, stderr } of runs) expect({ code, stderr }).toEqual({ code: 0, stderr: "" });
});

test("each run makes a database of its own in the batch's Postgres", () => {
  const made = lines(path.join(stub, "docker.log")).filter((line) =>
    line.includes("CREATE DATABASE")
  );
  for (const line of made) expect(line).toStartWith(`exec ${tag}-pg psql `);
  expect(made.map((line) => /CREATE DATABASE (\w+)/.exec(line)?.[1]).sort()).toEqual([
    "run_askgate_facts_on_1",
    "run_askgate_facts_on_2",
  ]);
});

test("each run starts a Dispatch server of its own, on its own database and port", () => {
  expect(servers.map((server) => server.database).sort()).toEqual([
    "run_askgate_facts_on_1",
    "run_askgate_facts_on_2",
  ]);
  expect(new Set(servers.map((server) => server.port)).size).toBe(2);
});

test("each run's agent, seeds and capture reach the run's own Dispatch and no other", () => {
  const seeds = lines(path.join(stub, "seeds.log")).map((line) => JSON.parse(line));
  const urls = new Set<string>();
  for (const { n, dir } of runs) {
    const database = `run_askgate_facts_on_${n}`;
    const server = servers.find((s) => s.database === database);
    const url = `http://127.0.0.1:${server?.port}`;
    urls.add(url);
    expect(JSON.parse(readFileSync(path.join(dir, "dispatch.json"), "utf8"))).toEqual({
      postgres: "stub-postgres",
      database,
      port: server?.port,
      url,
    });
    expect(lines(path.join(dir, "pane.env"))).toContain(`DISPATCH_URL=${url}`);
    expect(lines(path.join(dir, "agent-env.txt"))).toContain(`DISPATCH_URL=${url}`);
    // The run's world, its fixture and its capture, each through that server to that database.
    expect(seeds.filter((seed) => seed.database === database)).toEqual(
      ["project", "askgate", "capture-askgate"].map((command) => ({
        command,
        port: server?.port,
        database,
      }))
    );
  }
  expect(urls.size).toBe(2);
});

// The run's exit handler kills its server's whole tree before the run exits, so a server's pid is
// gone or a zombie by the time its run has ended.
test("each run stops its Dispatch server when it ends", () => {
  expect(servers.length).toBe(2);
  const running = servers.filter(
    ({ pid }) =>
      existsSync(`/proc/${pid}/stat`) &&
      !/^\d+ \(.*\) Z /.test(readFileSync(`/proc/${pid}/stat`, "utf8"))
  );
  expect(running).toEqual([]);
});

// AskGate's review would time out under Oh My Pi's default handler limit, so a run whose profile
// leaves the limit there scores the timeout rather than the rule: the rig refuses it, starting nothing.
test("a run on a profile whose settings leave the handler limit at its default is refused, naming the key", async () => {
  const child = Bun.spawn(["bash", rig, "run", "askgate", "stale", "1", "3", start], {
    env,
    stdout: "pipe",
    stderr: "pipe",
  });
  const [code, stderr] = await Promise.all([child.exited, new Response(child.stderr).text()]);
  expect(code).toBe(1);
  expect(stderr).toContain("extensionHandlers.toolCallTimeoutMs to 120000 or more");
  expect(existsSync(path.join(work, "runs", "askgate-facts-stale-1"))).toBe(false);
  expect(lines(path.join(stub, "docker.log")).join("\n")).not.toContain("stale");
});
