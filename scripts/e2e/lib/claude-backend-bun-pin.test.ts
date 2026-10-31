import { afterAll, expect, test } from "bun:test";
import {
  chmodSync,
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
  const found = Bun.which(name);
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
