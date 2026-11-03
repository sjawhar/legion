import { afterAll, expect, test } from "bun:test";
import {
  chmodSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

// claude-backend-tmux.sh builds claude-envoy with packages/claude-envoy/scripts/build.ts, which
// refuses any Bun but the one the repository's .bun-version pins. A devbox whose `bun` is another
// release must be refused in prerequisites, naming the pin, before the run builds or starts
// anything, rather than failing later in the rig at the build line. Every tool is a stand-in, so
// the run stops at its first step that needs a real one and touches nothing outside this directory.
const root = join(import.meta.dir, "..", "..", "..");
const script = join(root, "scripts", "e2e", "claude-backend-tmux.sh");
const pinned = readFileSync(join(root, ".bun-version"), "utf8").trim();
const dir = mkdtempSync(join(tmpdir(), "claude-backend-bun-pin-test."));
afterAll(() => rmSync(dir, { recursive: true, force: true }));

const bin = join(dir, "bin");
mkdirSync(bin);
const scratch = join(dir, "tmp");
mkdirSync(scratch);
function stub(name: string, body: string) {
  writeFileSync(join(bin, name), `#!/bin/sh\n${body}\n`);
  chmodSync(join(bin, name), 0o755);
}
for (const name of [
  "go",
  "docker",
  "ss",
  "tmux",
  "mise",
  "gh",
  "jj",
  "pgrep",
  "claude",
  "python3",
]) {
  stub(name, "exit 0");
}
// A Bun that is not the pinned release: it answers --version with another one.
const wrong = pinned === "1.0.0" ? "1.0.1" : "1.0.0";
stub("bun", `case "$1" in --version) echo ${wrong};; esac\nexit 0`);
stub("curl", "printf 200");
stub("agent-secrets", 'name=$1; shift; [ "$1" = -- ] && shift; export "$name=a2V5"; exec "$@"');
// hostTool gives the real NAME binary to put on the run's PATH. Bun.which takes the first match on
// this process's PATH, which on a machine with a version manager is a shim: a wrapper that resolves
// the tool it stands for out of the caller's environment — the manager's configuration under HOME,
// the configuration of the directory the caller is in, and, with no version pinned, whatever comes
// next on PATH. The run below has a HOME and a PATH of its own, where the manager pins nothing and
// nothing follows the shim, so a shim fails in place of the tool (mise answers `jq is not a valid
// shim`, exit 1) and the run stops at a line that has nothing to do with the pin. So each name
// takes the first entry on PATH that answers `--version` the same way under the run's own
// environment as under this process's: a real binary, which no environment changes. A tool that
// has no `--version` refuses it the same way under either, so it is taken too.
function hostTool(name: string, path = process.env.PATH ?? ""): string | null {
  for (const entry of path.split(":")) {
    if (entry === "") continue;
    const candidate = join(entry, name);
    if (!existsSync(candidate)) continue;
    const answer = (env: Record<string, string>) => {
      try {
        const ran = Bun.spawnSync([candidate, "--version"], {
          env,
          stdout: "pipe",
          stderr: "pipe",
          timeout: 10_000,
        });
        return `${ran.exitCode} ${ran.stdout.toString()}`;
      } catch {
        return null; // not a file this process can run
      }
    };
    const here = answer({ HOME: process.env.HOME ?? "", PATH: path });
    if (here !== null && here === answer({ HOME: dir, PATH: bin })) return candidate;
  }
  return null;
}

for (const name of [
  "bash",
  "sh",
  "jq",
  "awk",
  "sed",
  "tr",
  "cat",
  "mktemp",
  "date",
  "getconf",
  "readlink",
  "realpath",
  "dirname",
  "basename",
  "grep",
  "env",
  "rm",
  "mkdir",
  "chmod",
  "ls",
  "head",
  "tail",
  "cut",
  "sort",
  "uniq",
  "wc",
  "sleep",
  "kill",
  "id",
  "stat",
  "seq",
  "timeout",
  "touch",
  "cp",
  "mv",
  "ln",
  "find",
  "xargs",
  "tee",
  "od",
  "base64",
  "flock",
  "ps",
  "sha256sum",
]) {
  const found = hostTool(name);
  if (found !== null) symlinkSync(found, join(bin, name));
}
const keyFile = join(dir, "model-key");
writeFileSync(keyFile, "sk-test\n", { mode: 0o600 });

test("a bun other than the pinned release is refused in prerequisites, naming the pin", () => {
  const run = Bun.spawnSync(["bash", script], {
    env: {
      HOME: dir,
      PATH: bin,
      TMPDIR: scratch,
      LEGION_E2E_MODEL_ROUTE: "key-file",
      LEGION_E2E_ANTHROPIC_KEY_FILE: keyFile,
      LEGION_E2E_APP_KEY_RUNNER: "agent-secrets",
    },
    stdout: "pipe",
    stderr: "pipe",
    timeout: 60_000,
  });
  const output = run.stdout.toString() + run.stderr.toString();
  const failLine = output.split("\n").find((line) => line.startsWith("FAIL ")) ?? "";
  expect(failLine).toMatch(/^FAIL prerequisites: /);
  expect(failLine).toContain(pinned);
}, 90_000);

test("a tool that answers only for this process's own environment is passed over for a real binary", () => {
  const shims = join(dir, "shims");
  const real = join(dir, "real");
  mkdirSync(shims);
  mkdirSync(real);
  // A shim as a version manager writes one: it stands for the tool while it can read the manager's
  // configuration under HOME, or, with nothing pinned there, while another copy of the tool follows
  // it on PATH. Under the run's own HOME and PATH it has neither, and says so.
  writeFileSync(
    join(shims, "probe"),
    `#!/bin/sh
case ":$PATH:" in *:${real}:*) exec ${join(real, "probe")} "$@" ;; esac
[ "$HOME" = ${JSON.stringify(process.env.HOME ?? "")} ] || { echo 'probe is not a valid shim' >&2; exit 1; }
echo 'probe 1.0'
`,
    { mode: 0o755 }
  );
  writeFileSync(join(real, "probe"), "#!/bin/sh\necho 'probe 1.0'\n", { mode: 0o755 });
  expect(hostTool("probe", `${shims}:${real}`)).toBe(join(real, "probe"));
  expect(hostTool("probe", shims)).toBe(null);
  expect(hostTool("probe", real)).toBe(join(real, "probe"));
});
