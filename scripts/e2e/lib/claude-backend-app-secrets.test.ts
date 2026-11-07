import { beforeAll, expect, test } from "bun:test";
import {
  chmodSync,
  existsSync,
  mkdirSync,
  readFileSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import { join } from "node:path";
import { hostTool, skipWithout } from "@legion/test-fixtures/host-tools";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";

// claude-backend-tmux.sh's prerequisites on a box whose own `secrets` store holds no App credential
// (the devbox once its local copies of another deployment's keys were removed): the run must reach
// its Apps' tokens through the secrets broker's client, `agent-secrets`, alone and never ask the
// local store, or it can never pass there. Every tool the
// prerequisites ask for is a stand-in, so the run stops at the first step that needs a real one
// and touches nothing outside this test's directory.
const script = join(import.meta.dir, "..", "claude-backend-tmux.sh");
const dir = temporaryDirectory("claude-backend-key-runner-test");

const bin = join(dir, "bin");
const scratch = join(dir, "tmp");
// Where the stand-in `secrets` records that it was asked.
const asked = join(dir, "secrets-asked");
const keyFile = join(dir, "anthropic-key");
function stub(name: string, body: string) {
  writeFileSync(join(bin, name), `#!/bin/sh\n${body}\n`);
  chmodSync(join(bin, name), 0o755);
}
// The real programs the run needs on the PATH it is given: each is linked into the run's own bin
// below. A host missing one of them would run the script against a PATH without it, which fails
// at whichever line first calls it, so the row is skipped and names what is missing instead.
const tools = [
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
];
// The gate is given every name: it walks each one and names the missing ones in its warning.
// `found` is the run's own walk, whose real binaries the stand-in PATH below is built from.
const found = new Map(tools.map((name) => [name, hostTool(name)] as const));
const skip = skipWithout(...tools);

// Nothing below is read by a run that will not happen, so a host that skips builds none of it;
// and it is built in a hook, below the one the reservation registered, so a run that selects no
// row of this file builds none of it either.
beforeAll(() => {
  if (skip) return;
  mkdirSync(bin);
  mkdirSync(scratch);
  for (const name of ["go", "docker", "ss", "tmux", "bun", "mise", "gh", "jj", "pgrep", "claude"]) {
    stub(name, "exit 0");
  }
  // Anthropic accepts the model key.
  stub("curl", "printf 200");
  // The box's own store holds no App credential, and records that it was asked.
  stub("secrets", `printf '%s\\n' "$*" >> '${asked}'\necho "no secret named $2" >&2\nexit 1`);
  // The broker's client the tokens come through: runs its command with a release in its
  // environment.
  stub("agent-secrets", 'name=$1; shift; [ "$1" = -- ] && shift; export "$name={}"; exec "$@"');
  for (const [name, path] of found) {
    if (path !== null) symlinkSync(path, join(bin, name));
  }
  writeFileSync(keyFile, "sk-test\n", { mode: 0o600 });
});

test.skipIf(skip)(
  "the prerequisites never ask the box's own secrets store for an App credential",
  () => {
    const run = Bun.spawnSync(["bash", script], {
      env: {
        HOME: dir,
        PATH: bin,
        TMPDIR: scratch,
        LEGION_E2E_MODEL_ROUTE: "key-file",
        LEGION_E2E_ANTHROPIC_KEY_FILE: keyFile,
      },
      stdout: "pipe",
      stderr: "pipe",
      timeout: 60_000,
    });
    const output = run.stdout.toString() + run.stderr.toString();
    expect(output).not.toMatch(/^FAIL prerequisites: secrets\b/m);
    expect(existsSync(asked) ? readFileSync(asked, "utf8") : "").toBe("");
  },
  90_000
);
