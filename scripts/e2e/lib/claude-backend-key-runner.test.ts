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

// claude-backend-tmux.sh's prerequisites with LEGION_E2E_APP_KEY_RUNNER naming the runner the App
// keys come through, on a box whose own `secrets` store holds neither key (the devbox once its
// local copies of another deployment's keys were removed): the run must reach its App keys through
// that runner alone and never ask the local store, or it can never pass there. Every tool the
// prerequisites ask for is a stand-in, so the run stops at the first step that needs a real one
// and touches nothing outside this test's directory.
const script = join(import.meta.dir, "..", "claude-backend-tmux.sh");
const dir = mkdtempSync(join(tmpdir(), "claude-backend-key-runner-test."));
afterAll(() => rmSync(dir, { recursive: true, force: true }));

const bin = join(dir, "bin");
mkdirSync(bin);
const scratch = join(dir, "tmp");
mkdirSync(scratch);
function stub(name: string, body: string) {
  writeFileSync(join(bin, name), `#!/bin/sh\n${body}\n`);
  chmodSync(join(bin, name), 0o755);
}
for (const name of ["go", "docker", "ss", "tmux", "bun", "mise", "gh", "jj", "pgrep", "claude"]) {
  stub(name, "exit 0");
}
// Anthropic accepts the model key.
stub("curl", "printf 200");
// The box's own store holds neither App key, and records that it was asked.
const asked = join(dir, "secrets-asked");
stub("secrets", `printf '%s\\n' "$*" >> '${asked}'\necho "no secret named $2" >&2\nexit 1`);
// The runner the keys come through: runs its command with the key in its environment.
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
const keyFile = join(dir, "anthropic-key");
writeFileSync(keyFile, "sk-test\n", { mode: 0o600 });

test("with LEGION_E2E_APP_KEY_RUNNER set, the prerequisites never ask the box's own secrets store for an App key", () => {
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
  expect(output).not.toMatch(/^FAIL prerequisites: secrets\b/m);
  expect(existsSync(asked) ? readFileSync(asked, "utf8") : "").toBe("");
}, 90_000);
