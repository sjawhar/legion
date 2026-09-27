import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { spawnSync } from "node:child_process";
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import { createPaneGuard, type PaneGuard } from "./pane-guard";

// A pane laid out as the Go daemon's tmux runtime lays it out: the issue workspace inside the
// daemon's state directory, so the workspace allowance has to win over the state directory's
// refusal. `scratch` stands in for /tmp, `home` for the operator's home directory. Nothing any
// test names is ever run.
let base = "";
let scratch = "";
let home = "";
let state = "";
let workspace = "";
let guard: PaneGuard;
let env: NodeJS.ProcessEnv;

const repository = path.resolve(import.meta.dir, "../../../..");
let repositoryGuard: PaneGuard;
let repositoryEnv: NodeJS.ProcessEnv;

const EXPECTED_SCRIPT_REFUSALS: Record<string, string> = {
  ".github/scripts/release-push.sh": "a positional parameter",
  "packages/claude-envoy/scripts/smoke-channel.sh": "tmux kill-session",
  "packages/dispatch/e2e/acceptance/omp-roundtrip.sh": "tmux kill-session",
  "packages/envoy/deploy/scripts/autodeploy.sh": "dispatch-backups",
  "packages/envoy/deploy/scripts/autodeploy_test.sh": "dumps[i]",
  "packages/envoy/deploy/scripts/sync-host.sh": "$1",
  "packages/envoy/scripts/e2e-api.sh": "$sse_pid",
  "packages/envoy/scripts/verify-cluster.sh": "cannot parse",
  "packages/pi-envoy/scripts/grant-rig/setup.sh": "profiles/l12rig",
  "packages/pi-envoy/scripts/smoke-btw.sh": "tmux kill-session",
  "packages/pi-envoy/scripts/smoke-delivery.sh": "tmux kill-session",
  "scripts/e2e/controller-start-tmux.sh": "a loop variable",
  "scripts/e2e/lib/check-model-route.sh": "$control",
  "scripts/e2e/lib/install-model-gateway.sh": "realpath -m",
  "scripts/e2e/lib/install-plugin-profile.sh": "realpath -m",
  "scripts/e2e/stage2-tmux-supervision.sh": "realpath -m",
  "scripts/e2e/stage3-4b13b-acceptance.sh": "prod_header_file",
  "scripts/e2e/stage3-devbox-workflow.sh": "prod_header_file",
  "scripts/e2e/stage4b-sandbox-tree.sh": "$p",
  "scripts/sync-envoy-host.sh": "$1",
};

beforeAll(() => {
  base = mkdtempSync(path.join(os.tmpdir(), "legion-pane-guard-"));
  scratch = path.join(base, "tmp");
  home = path.join(base, "home");
  state = path.join(base, "state");
  workspace = path.join(state, "workspaces", "LEGION-1");
  for (const dir of [
    path.join(scratch, "mine"),
    path.join(scratch, "tmux-1000"),
    path.join(home, ".ssh"),
    workspace,
  ]) {
    mkdirSync(dir, { recursive: true });
  }
  writeFileSync(path.join(home, ".ssh", "id_ed25519"), "key");
  writeFileSync(path.join(home, ".bashrc"), "profile");
  writeFileSync(path.join(state, "legion.json"), "{}");
  writeFileSync(path.join(workspace, "notes.txt"), "notes");
  guard = createPaneGuard({ workspace, ompPid: process.pid, scratch });
  env = {
    HOME: home,
    LEGION_WORKSPACE: workspace,
    LEGION_STATE_DIR: state,
    TMPDIR: scratch,
    PATH: "/usr/bin:/bin",
  };
  repositoryGuard = createPaneGuard({
    workspace: repository,
    ompPid: process.pid,
    scratch: "/tmp",
  });
  repositoryEnv = { ...env, HOME: "/home/ubuntu", LEGION_WORKSPACE: repository, TMPDIR: "/tmp" };
});

afterAll(() => {
  rmSync(base, { recursive: true, force: true });
});

const bash = (command: string, cwd = workspace): string | undefined =>
  guard.bash(command, cwd, env);

/** Each family takes a target word and builds the command that would destroy it. */
const FAMILIES: Record<string, (target: string) => string> = {
  "rm -rf": (target) => `rm -rf ${target}`,
  rm: (target) => `rm -f ${target}`,
  unlink: (target) => `unlink ${target}`,
  "mv (source)": (target) => `mv ${target} "$LEGION_WORKSPACE/moved"`,
  "mv (destination)": (target) => `mv "$LEGION_WORKSPACE/notes.txt" ${target}`,
  "find -delete": (target) => `find ${target} -name '*.tmp' -delete`,
  "find -exec rm": (target) => `find ${target} -type f -exec rm -f {} +`,
  shred: (target) => `shred -u -n 3 ${target}`,
  "chmod -R": (target) => `chmod -R 700 ${target}`,
  "chown -R": (target) => `chown -R nobody ${target}`,
  truncate: (target) => `truncate -s 0 ${target}`,
  "> redirection": (target) => `echo overwritten > ${target}`,
  tee: (target) => `echo overwritten | tee ${target}`,
};

describe("the pane guard's command families", () => {
  for (const [family, build] of Object.entries(FAMILIES)) {
    test(`${family}: allowed in the workspace and /tmp scratch, refused for $HOME, ~/.ssh, the state directory, and a path reached through cd`, () => {
      const allowed = [
        build('"$LEGION_WORKSPACE/notes.txt"'),
        build("notes.txt"),
        build(`${scratch}/mine/notes.txt`),
        build('"$(mktemp -d)/x"'),
      ];
      expect(allowed.map((command) => [command, bash(command)])).toEqual(
        allowed.map((command) => [command, undefined])
      );
      // Redirection and tee refuse only an existing file: a new one overwrites nothing.
      const refused: [command: string, cwd: string, named: string][] = [
        [build('"$HOME"'), workspace, '`"$HOME"`'],
        [build("~/.ssh/id_ed25519"), workspace, "`~/.ssh/id_ed25519`"],
        [build('"$LEGION_STATE_DIR/legion.json"'), workspace, '`"$LEGION_STATE_DIR/legion.json"`'],
        [`cd ~ && ${build(".bashrc")}`, workspace, "`.bashrc`"],
      ];
      for (const [command, cwd, named] of refused) {
        const reason = bash(command, cwd);
        expect(reason, command).toContain(named);
        expect(reason, command).toContain(`outside the issue workspace ${workspace}`);
      }
      // The resolved path is named too, so the agent sees where the word led.
      expect(bash(`cd ~ && ${build(".bashrc")}`)).toContain(path.join(home, ".bashrc"));
    });
  }
});

describe("resolution", () => {
  test("sees through a variable assigned earlier, quoting, ~, braces, and cd", () => {
    expect(bash('d=~/.ssh; rm -rf "$d"')).toContain(path.join(home, ".ssh"));
    expect(bash("export D=~/.ssh && rm -rf $D")).toContain(path.join(home, ".ssh"));
    expect(bash('rm -rf "$HOME/.ssh-does-not-exist"')).toContain(".ssh-does-not-exist");
    // A quoted `~` is a file named `~` in the working directory, not the home directory.
    expect(bash('rm -rf "~"')).toBeUndefined();
    expect(bash("rm -rf ~")).toContain("`~`");
    expect(bash("rm -rf \\~")).toBeUndefined();
    expect(bash(`rm -rf ${scratch}/mine/{a,b} "$LEGION_WORKSPACE"/{c,d}`)).toBeUndefined();
    expect(bash(`rm -rf {${scratch}/mine/a,${home}}`)).toContain(home);
    expect(bash(`cd ${home}; cd .ssh; rm -f id_ed25519`)).toContain(
      path.join(home, ".ssh", "id_ed25519")
    );
    expect(bash(`cd ${home} && cd - && rm -rf build`)).toContain("cd -");
    // A subshell's `cd` does not leak out of it.
    expect(bash(`(cd ${home}) && rm -rf build`)).toBeUndefined();
    expect(bash('d="$(mktemp -d)"; cd "$d"; rm -rf *')).toBeUndefined();
    expect(bash(`rm -rf "\${WORK:-$LEGION_WORKSPACE/build}"`)).toBeUndefined();
    expect(bash(`rm -rf "\${HOME:-/nowhere}"`)).toContain(home);
  });

  test("refuses a target it cannot resolve, and never guesses one", () => {
    expect(bash('read d; rm -rf "$d"')).toContain("read from input");
    expect(bash('rm -rf "$NOT_SET_ANYWHERE"')).toContain("`$NOT_SET_ANYWHERE`");
    expect(bash('rm -rf "$(git rev-parse --show-toplevel)"')).toContain("a command's output");
    expect(bash('for d in a "$X"; do rm -rf "$d"; done')).toContain("loop variable");
    // A known prefix inside the workspace bounds what follows it.
    expect(bash('rm -rf "$LEGION_WORKSPACE/build/$(date +%s)"')).toBeUndefined();
  });

  test("refuses a command the parser reports as malformed", () => {
    expect(bash('rm -rf "$LEGION_WORKSPACE/unterminated')).toContain("cannot parse");
  });

  test("keeps /tmp itself, a glob over it, and its socket directories out of the scratch root", () => {
    expect(bash(`rm -rf ${scratch}/*`)).toContain("a glob over");
    expect(bash(`rm -rf ${scratch}`)).toContain(scratch);
    expect(bash(`rm -rf ${scratch}/tmux-1000`)).toContain("tmux-1000");
    expect(bash(`rm -rf ${scratch}/t*`)).toContain("a glob over");
    expect(bash(`rm -rf ${scratch}/mine/*`)).toBeUndefined();
    expect(bash(`rm -rf ${scratch}/mine*`)).toBeUndefined();
    expect(bash(`rm -rf ${scratch}/t`)).toBeUndefined();
  });

  test("follows command substitutions, subshells, functions, and wrappers", () => {
    expect(bash("echo $(rm -rf ~)")).toContain("`~`");
    expect(bash("ls `rm -rf ~`")).toContain("`~`");
    expect(bash("f() { rm -rf ~; }; f")).toContain("`~`");
    expect(bash("if true; then rm -rf ~; fi")).toContain("`~`");
    expect(bash("sudo -n rm -rf ~")).toContain("`~`");

    expect(bash("env FOO=1 nice -n 5 timeout 10 rm -rf ~")).toContain("`~`");
    expect(bash("find . -name x | xargs rm -rf")).toContain("standard input");
    expect(bash("echo hi 2>&1 >/dev/null | tee -a ~/.bashrc")).toBeUndefined();
    expect(bash("echo hi >> ~/.bashrc")).toBeUndefined();
    expect(bash("echo hi > ~/new-file-that-does-not-exist")).toBeUndefined();
  });

  test("follows xargs shell and interpreter text plus multicall applets", () => {
    for (const command of [
      "echo x | xargs sh -c 'rm -rf \"$HOME\"'",
      "echo x | xargs -I{} bash -c 'rm -rf $HOME/.ssh'",
      "echo x | xargs -I{} bash -c 'rm -rf {}'",
      "echo x | xargs -i sh -c 'rm -rf {}'",
      "echo x | xargs --replace sh -c 'rm -rf \"$HOME\"'",
      "echo x | xargs --eof sh -c 'rm -rf \"$HOME\"'",
      "echo x | xargs --max-lines sh -c 'rm -rf \"$HOME\"'",
      `echo x | xargs -I{} python3 -c 'import shutil; shutil.rmtree("${home}/{}")'`,
      `echo x | xargs -I{} node -e 'require("fs").rmSync("${home}/{}", { recursive: true })'`,
      "busybox rm -rf ~",
      "toybox rm -rf ~",
    ]) {
      expect(bash(command), command).toBeDefined();
    }
    expect(
      bash(`echo x | xargs -I{} python3 -c 'import shutil; shutil.rmtree("{}")'`)
    ).toBeUndefined();
  });

  test("refuses destructive synchronization and archive destinations", () => {
    for (const command of [
      'rsync -a --delete "$LEGION_WORKSPACE/build/" "$HOME/"',
      'rsync -a --delete "$LEGION_WORKSPACE/build/" "$HOME/" --exclude tmp',
      'tar -xf fixture.tar -C "$HOME"',
      'unzip -o fixture.zip -d "$HOME"',
    ]) {
      const reason = bash(command);
      expect(reason, command).toBeDefined();
      expect(reason ?? "", command).toContain(home);
    }
  });
  test("follows find -exec through a shell and execution wrappers", () => {
    for (const command of [
      "find ~ -type d -exec sh -c 'rm -rf \"$1\"' _ {} \\;",
      "find ~ -type f -exec bash -c 'rm -f \"$0\"' {} \\;",
      "find ~ -exec sudo rm -rf {} +",
      "find ~ -exec env rm -rf {} +",
      "sudo rm -rf ~",
    ]) {
      expect(bash(command), command).toContain(home);
    }
    expect(bash('find "$LEGION_WORKSPACE" -exec sh -c \'rm -rf "$HOME"\' _ {} \\;')).toContain(
      home
    );
  });

  test("refuses a brace expansion it cannot enumerate completely", () => {
    expect(bash(`rm -rf {a,b,c,d,e,f,g,h,~}{/x,/y,/z,/w,/v,/u,/t,/s}`)).toContain(
      "more than 64 alternatives"
    );
  });

  test("refuses a bounded command walk before nested loops block the pane", () => {
    const words = Array.from({ length: 16 }, (_, index) => String(index)).join(" ");
    let command = ":";
    for (const name of ["a", "b", "c", "d"]) {
      command = `for ${name} in ${words}; do ${command}; done`;
    }
    expect(bash(`${command}; rm -rf "$HOME"`)).toContain("walk limit");
  });

  test("leaves ordinary work alone", () => {
    for (const command of [
      'cd -- "$LEGION_WORKSPACE" && jj -R "$LEGION_WORKSPACE" new && rm -rf .legion && jj -R "$LEGION_WORKSPACE" describe -m "chore: remove .legion handoffs"',
      "bun test src/legion/pane-guard.test.ts 2>&1 | tail -5",
      'git -C "$LEGION_WORKSPACE" status --porcelain > /dev/null',
      "rm -rf node_modules/.cache dist",
      "chmod +x ~/bin/tool",
      "cat ~/.bashrc",
      'set -euo pipefail; tmp=$(mktemp); echo x > "$tmp"; rm -f "$tmp"',
    ]) {
      expect([command, bash(command)]).toEqual([command, undefined]);
    }
  });
});

describe("scripts a command runs", () => {
  const script = (name: string, content: string): string => {
    const file = path.join(scratch, "mine", name);
    writeFileSync(file, content);
    return file;
  };

  test("checks deferred functions and traps against later assignments", () => {
    expect(bash('d=; f() { rm -rf "$d"; }; d=$HOME; f')).toContain(home);
    expect(bash("work=; trap 'rm -rf \"$work\"' EXIT; work=$HOME")).toContain(home);
    expect(
      bash('work=/tmp/x; cleanup() { rm -rf "$work"; }; trap cleanup EXIT; work="$HOME"')
    ).toContain(home);
    const deferred = script(
      "deferred.sh",
      'set -euo pipefail\nwork=\ncleanup() { rm -rf "$work"; }\ntrap cleanup EXIT\nwork=$HOME\n'
    );
    expect(bash(`bash ${deferred}`)).toContain(home);
    expect(bash('f() { rm -rf "$1"; }; f "$LEGION_WORKSPACE/build"')).toBeUndefined();
  });

  test("runs a sourced parent's EXIT trap only when its parent script ends", () => {
    const sourced = script("source-trap-body.sh", ":\n");
    const parent = script(
      "source-trap-parent.sh",
      `p=$(pgrep x)\ncleanup() { kill "$p"; }\ntrap cleanup EXIT\n. ${sourced}\nrm -rf "$HOME/x"\n`
    );
    const reason = bash(`bash ${parent}`);
    expect(reason).toContain(`line 5 of ${parent}`);
    expect(reason).toContain('`rm -rf "$HOME/x"`');
    expect(reason).not.toContain(sourced);
  });

  test("attributes a sourced parent's EXIT trap to its declaring file", () => {
    const sourced = script("source-trap-owner-body.sh", ":\n");
    const parent = script(
      "source-trap-owner-parent.sh",
      `p=$(pgrep x)\ncleanup() { kill "$p"; }\ntrap cleanup EXIT\n. ${sourced}\n`
    );
    const reason = bash(`bash ${parent}`);
    expect(reason).toContain(`line 2 of ${parent}`);
    expect(reason).not.toContain(`line 2 of ${sourced}`);
  });

  test("attributes an EXIT trap declared by a sourced file to that file", () => {
    const sourced = script(
      "source-trap-declaring-file.sh",
      'p=$(pgrep x)\ncleanup() { kill "$p"; }\ntrap cleanup EXIT\n'
    );
    const parent = script("source-trap-includer.sh", `. ${sourced}\n`);
    const reason = bash(`bash ${parent}`);
    expect(reason).toContain(`line 2 of ${sourced}`);
    expect(reason).not.toContain(`line 2 of ${parent}`);
  });

  test("attributes a nested script call-site to the enclosing script", () => {
    const inner = script("nested-attribution-inner.sh", ':\n:\nrm -rf "$HOME"\n');
    const outer = script("nested-attribution-outer.sh", `:\n:\n:\nbash ${inner}\n`);
    const reason = bash(`bash ${outer}`);
    expect(reason).toContain(`line 4 of ${outer}`);
    expect(reason).toContain(`line 3 of ${inner}`);
  });

  test("attributes a sourced function body to its defining file", () => {
    const sourced = script(
      "source-function-defining-file.sh",
      'cleanup() {\n  rm -rf "$HOME"\n}\n'
    );
    const parent = script("source-function-caller.sh", `. ${sourced}\ncleanup\n`);
    const reason = bash(`bash ${parent}`);
    expect(reason).toContain(`line 2 of ${sourced}`);
    expect(reason).not.toContain(`line 2 of ${parent}`);
  });

  test("preserves the deepest defining file across sourced function calls", () => {
    const inner = script(
      "source-function-inner.sh",
      ':\n:\n:\n:\n:\n:\n:\n:\n:\n:\n  cleanup_inner() { rm -rf "$HOME"; }\n'
    );
    const outer = script(
      "source-function-outer.sh",
      `:\n:\ncleanup_outer() { cleanup_inner; }\n. ${inner}\n`
    );
    const parent = script("source-function-main.sh", `. ${outer}\ncleanup_outer\n`);
    const reason = bash(`bash ${parent}`);
    expect(reason).toContain(`line 11 of ${inner}`);
    expect(reason).not.toContain(`line 11 of ${outer}`);
  });

  test("keeps inline traps and function locations across script boundaries", () => {
    const trapLibrary = script("inline-trap-library.sh", ":\n:\ntrap 'rm -rf \"$HOME\"' EXIT\n");
    const trapMain = script("inline-trap-main.sh", `. ${trapLibrary}\n`);
    const trapTop = script("inline-trap-top.sh", `:\n:\n:\nbash ${trapMain}\n`);
    const trapReason = bash(`bash ${trapTop}`);
    expect(trapReason).toContain(`line 4 of ${trapTop}`);
    expect(trapReason).toContain(`line 3 of ${trapLibrary}`);
    expect((trapReason ?? "").match(/line \d+ of /g)).toHaveLength(3);

    const functionMid = script("function-mid.sh", ':\n:\nf() {\n  rm -rf "$HOME"\n}\nf\n');
    const functionTop = script("function-top.sh", `:\n:\nbash ${functionMid}\n`);
    const functionReason = bash(`bash ${functionTop}`);
    expect(functionReason).toContain(`line 3 of ${functionTop}`);
    expect(functionReason).toContain(`line 4 of ${functionMid}`);
    expect((functionReason ?? "").match(/line \d+ of /g)).toHaveLength(2);

    const inlineReason = bash("f() { rm -rf ~; }; f");
    expect(inlineReason).toContain("`rm -rf ~`");
    expect(inlineReason).not.toContain("<function>");
  });

  test("uses a shell function's echoed path in a command substitution", () => {
    const generated = script(
      "function-output.sh",
      'work="$(mktemp -d)"\nfixture() { local root="$work/$1"; mkdir -p "$root"; echo "$root"; }\nroot=$(fixture green)\nrm -rf "$root"\n'
    );
    expect(bash(`bash ${generated}`)).toBeUndefined();
  });

  test("uses a shell function's printf path in a command substitution", () => {
    const generated = script(
      "function-printf.sh",
      'work="$(mktemp -d)"\nheader() { local file="$work/header"; : > "$file"; printf \'%s\\n\' "$file"; }\npath=$(header)\nrm -f "$path"\n'
    );
    expect(bash(`bash ${generated}`)).toBeUndefined();
  });

  test("refuses every path a function can write to stdout", () => {
    const reason = bash('f() { echo "$HOME"; echo "$LEGION_WORKSPACE/x"; }; rm -rf $(f)');
    expect(reason).toBeDefined();
    expect(reason ?? "").toContain(home);
  });

  test("treats function output from nested scopes as unknown", () => {
    for (const command of [
      'f() { echo /tmp/x; if true; then echo "$HOME"; fi; }; rm -rf $(f)',
      'f() { echo /tmp/x; for d in "$HOME"; do echo "$d"; done; }; rm -rf $(f)',
      'f() { echo /tmp/x; ( echo "$HOME" ); }; rm -rf $(f)',
      'f() { echo /tmp/x; echo "$HOME" | cat; }; rm -rf $(f)',
    ]) {
      const reason = bash(command);
      expect(reason, command).toBeDefined();
      expect(reason ?? "", command).toContain("a command's output");
    }
  });

  test("invalidates a pid file after a later write from any source", () => {
    const pidFile = path.join(scratch, "mine", "replaced.pid");
    const reason = bash(
      `sleep 60 & echo "$!" > ${pidFile}; pgrep server > ${pidFile}; kill "$(<${pidFile})"`
    );
    expect(reason).toBeDefined();
    expect(reason ?? "").toContain("cannot resolve the pid");
  });

  test("invalidates pid files rewritten from a pipeline or subshell", () => {
    const pidFile = path.join(scratch, "mine", "nested-replaced.pid");
    for (const command of [
      `sleep 60 & echo "$!" > ${pidFile}; pgrep sleep | tee ${pidFile}; kill "$(<${pidFile})"`,
      `sleep 60 & echo "$!" > ${pidFile}; (cd ${path.dirname(pidFile)} && pgrep x > ${path.basename(pidFile)}); kill "$(<${pidFile})"`,
    ]) {
      const reason = bash(command);
      expect(reason, command).toBeDefined();
      expect(reason ?? "", command).toContain("cannot resolve the pid");
    }
  });

  test("tracks every array element's process provenance", () => {
    const reason = bash(`ids[0]=$(pgrep sleep); sleep 60 & ids[1]=$!; kill "\${ids[0]}"`);
    expect(reason).toBeDefined();
    expect(reason ?? "").toContain("cannot resolve the pid");
  });

  test("invalidates every array element on an untracked replacement", () => {
    for (const command of [
      `sleep 60 & ids[0]=$!; n=$(cat f); ids[$n]=$(pgrep sleep); kill "\${ids[0]}"`,
      `sleep 60 & ids[0]=$!; read -r -a ids < f; kill "\${ids[0]}"`,
      `sleep 60 & ids[0]=$!; mapfile -t ids < <(pgrep sleep); kill "\${ids[0]}"`,
      `sleep 60 & ids[0]=$!; declare -a ids=($(pgrep sleep)); kill "\${ids[0]}"`,
    ]) {
      const reason = bash(command);
      expect(reason, command).toBeDefined();
      expect(reason ?? "", command).toContain("cannot resolve the pid");
    }
  });

  test("preserves an indexed descendant pid through local declarations", () => {
    expect(
      bash(
        `ids=(); sleep 60 & ids[0]=$!; f() { local n="$1"; local pid="\${ids[$n]}"; kill "$pid"; }; f 0`
      )
    ).toBeUndefined();
  });

  test("checks every tracked shell script against the documented allow-list", () => {
    const listed = spawnSync("git", ["ls-files", "-z", "--", ":(glob)**/*.sh"], {
      cwd: repository,
      encoding: "utf8",
    });
    if (listed.status !== 0) throw new Error(listed.stderr);
    const refusals = Object.fromEntries(
      listed.stdout
        .split("\0")
        .filter((file) => file !== "")
        .flatMap((file) => {
          const reason = repositoryGuard.bash(`bash ${file}`, repository, repositoryEnv);
          return reason === undefined ? [] : [[file, reason]];
        })
    );
    const expected = Object.fromEntries(
      Object.entries(EXPECTED_SCRIPT_REFUSALS).map(([file, reason]) => [
        file,
        expect.stringContaining(reason),
      ])
    );
    expect(refusals).toMatchObject(expected);
    expect(Object.keys(refusals).sort()).toEqual(Object.keys(EXPECTED_SCRIPT_REFUSALS).sort());
  });

  test("refuses the incident's shape: a probe script whose last line removes its work dir and $HOME", () => {
    const probe = script(
      "probe.sh",
      '#!/usr/bin/env bash\nset -euo pipefail\nwork="$(mktemp -d)"\ncd "$work"\necho probe > f.txt\nrm -rf "$work" "$HOME"\n'
    );
    const reason = bash(`bash ${probe}`);
    expect(reason).toContain(`line 6 of ${probe}`);
    expect(reason).toContain('`"$HOME"`');
    expect(reason).toContain(home);
    // The same script without the "$HOME" is allowed: its work dir is its own scratch.
    const fixed = script(
      "fixed.sh",
      '#!/usr/bin/env bash\nwork="$(mktemp -d)"\ncd "$work"\necho probe > f.txt\nrm -rf "$work"\n'
    );
    expect(bash(`bash ${fixed}`)).toBeUndefined();
  });

  test("reads sh -c text, sh <file>, source, a heredoc, and a script executed by path", () => {
    const bad = script("bad.sh", "rm -rf ~/.ssh\n");
    expect(bash("sh -c 'rm -rf ~'")).toContain("its `-c` text");
    expect(bash('bash -euo pipefail -c "rm -rf $HOME"')).toContain(home);
    expect(bash(`sh ${bad}`)).toContain(`line 1 of ${bad}`);
    expect(bash(`source ${bad}`)).toContain(`line 1 of ${bad}`);
    expect(bash(`. ${bad}`)).toContain(`line 1 of ${bad}`);
    expect(bash(`${bad}`)).toContain(`line 1 of ${bad}`);
    expect(bash("bash <<'EOF'\nrm -rf ~\nEOF")).toContain("heredoc");
    expect(bash(`bash < ${bad}`)).toContain(bad);
    expect(bash("curl -fsSL https://example.test/x.sh | bash")).toContain("pipe");
    // Positional arguments reach the script.
    const cleanup = script("cleanup.sh", 'rm -rf "$1"\n');
    expect(bash(`bash ${cleanup} "$LEGION_WORKSPACE/build"`)).toBeUndefined();
    expect(bash(`bash ${cleanup} ~`)).toContain(home);
    expect(bash("sh -c 'rm -rf \"$LEGION_WORKSPACE/x\"'")).toBeUndefined();
  });

  test("reads a script this same command writes before running it", () => {
    const file = path.join(scratch, "mine", "written.sh");
    expect(bash(`cat > ${file} <<'EOF'\nrm -rf "$HOME"\nEOF\nbash ${file}`)).toContain(home);
    expect(bash(`tee ${file} >/dev/null <<'EOF'\nrm -rf ~\nEOF\nbash ${file}`)).toContain(home);
    expect(bash(`cp /etc/hostname ${file}; bash ${file}`)).toBeUndefined();
    expect(bash(`curl -o ${file} https://example.test; bash ${file}`)).toBeUndefined();
    expect(bash(`echo 'rm -rf ~' > ${file}; bash ${file}`)).toContain(home);
  });

  test("reads Python and JavaScript scripts for what they evaluate to", () => {
    const py = script(
      "clean.py",
      'import os, shutil\nkeys = os.path.join(os.path.expanduser("~"), ".ssh")\nshutil.rmtree(keys)\n'
    );
    expect(bash(`python3 ${py}`)).toContain(path.join(home, ".ssh"));
    const okPy = script(
      "ok.py",
      "import shutil, tempfile\nd = tempfile.mkdtemp()\nshutil.rmtree(d)\nshutil.rmtree(dynamic())\n"
    );
    expect(bash(`python3 ${okPy}`)).toBeUndefined();
    const js = script(
      "clean.ts",
      'import { rmSync } from "node:fs";\nimport os from "node:os";\nrmSync(os.homedir(), { recursive: true, force: true });\n'
    );
    expect(bash(`bun ${js}`)).toContain(home);
    expect(bash(`bun run ${js}`)).toContain(home);
    expect(bash(`node ${js}`)).toContain(home);
    expect(bash(`python3 -c 'import shutil; shutil.rmtree("${home}")'`)).toContain(home);
    expect(
      bash(
        "python3 -c 'import os, subprocess\nsuffix = unknown()\nsubprocess.run(f\"rm -rf {os.environ['\"'\"'HOME'\"'\"']}/{suffix}\", shell=True)'"
      )
    ).toContain(home);
    expect(
      bash(
        'node -e \'const suffix = unknown(); require("child_process").execSync("rm -rf " + process.env.HOME + "/" + suffix)\''
      )
    ).toContain(home);
    expect(bash("bun test && bun run build")).toBeUndefined();
    // `python3 -` reads its program from the heredoc.
    expect(bash(`python3 - <<'PY'\nimport shutil\nshutil.rmtree("${home}")\nPY`)).toContain(home);
    // A string's `.replace` is not a move; a path's is.
    expect(bash(`python3 -c 'print("a/b".replace("/", "${home}"))'`)).toBeUndefined();
    expect(
      bash(`python3 -c 'from pathlib import Path; Path("x").replace("${home}/.bashrc")'`)
    ).toContain(home);
  });

  test("builds the text a -c, eval, or interpreter runs from what it knows, and leaves the rest unknown", () => {
    // The unknown part is still unknown inside the text: a target made of it is refused.
    expect(bash('read d; bash -c "rm -rf $d"')).toContain("builds at run time");
    expect(bash("read d; bash -c \"rm -rf '$d'\"")).toContain("builds at run time");
    expect(bash('read d; eval "rm -rf $d"')).toContain("builds at run time");
    // A known part is judged as usual; an unknown part that is not a target is harmless.
    expect(bash('read d; bash -c "echo $d; rm -rf ~"')).toContain("`~`");
    expect(bash("f=$(ls); python3 -c \"print(open('$f').read())\"")).toBeUndefined();
    // Text that is only a run-time value runs a program the guard cannot name: the residual.
    expect(bash('eval "$(ssh-agent -s)"')).toBeUndefined();
  });

  test("runs a compiled program by path without reading it", () => {
    const program = path.join(scratch, "mine", "program");
    writeFileSync(program, `\u007fELF${"\u0000".repeat(2 * 1024 * 1024)}`);
    expect(bash(`${program} --flag`)).toBeUndefined();
  });
});

describe("the eval tool", () => {
  const code = (language: "py" | "js", source: string): string | undefined =>
    guard.code(language, source, workspace, env);

  test("refuses what is checkable, leaves the rest", () => {
    expect(code("py", 'import shutil, os\nshutil.rmtree(os.environ["HOME"])')).toContain(home);
    expect(code("py", "from pathlib import Path\n(Path.home() / '.ssh').unlink()")).toContain(
      path.join(home, ".ssh")
    );
    expect(
      code("py", 'import subprocess\nsubprocess.run(["rm", "-rf", os.path.expanduser("~")])')
    ).toContain(home);
    expect(code("py", 'import os\nos.system("rm -rf ~/.ssh")')).toContain(".ssh");
    expect(code("py", "!rm -rf ~")).toContain("`~`");
    expect(code("py", "%%bash\nrm -rf ~\n")).toContain("`~`");
    expect(code("py", 'open(os.path.expanduser("~/.bashrc"), "w").write("")')).toContain(".bashrc");
    expect(code("js", `const home = os.homedir();\nawait Bun.$\`rm -rf \${home}\`;`)).toContain(
      home
    );
    expect(
      code("js", 'fs.rmSync(path.join(process.env.HOME, ".ssh"), { recursive: true })')
    ).toContain(".ssh");
    expect(code("js", 'await tool.bash({ command: "rm -rf ~" })')).toContain("`~`");
    expect(code("js", 'execSync("pkill -x sleep")')).toContain("pkill");
    expect(
      code("py", `import os, sys\nopen(f"{os.environ['HOME']}/{sys.argv[1]}", "w").write("x")`)
    ).toContain(home);
    expect(code("js", 'Bun.write(process.env.HOME + "/" + process.argv[2], "x")')).toContain(home);

    // Allowed: inside the workspace, an unevaluable argument (the documented residual), reads.
    expect(
      code("py", 'shutil.rmtree(os.path.join(os.environ["LEGION_WORKSPACE"], "build"))')
    ).toBeUndefined();
    expect(code("py", "shutil.rmtree(compute_path())")).toBeUndefined();
    expect(code("js", `await Bun.$\`rm -rf \${dir}\``)).toBeUndefined();
    expect(code("py", 'print(read(".legion/plan.json"))')).toBeUndefined();
    expect(code("py", 'open(os.path.expanduser("~/.bashrc")).read()')).toBeUndefined();
  });
  test("recognizes CommonJS filesystem, process, and home-directory receivers", () => {
    for (const [source, target] of [
      ['require("fs").rmSync("/home/ubuntu/.ssh", { recursive: true })', "/home/ubuntu/.ssh"],
      ['require("node:fs").rmSync(process.env.HOME, { recursive: true })', home],
      ['require("fs/promises").rm(process.env.HOME, { recursive: true })', home],
      ['require("child_process").execSync("rm -rf ~/.ssh")', home],
      ['require("fs").rmSync(require("os").homedir(), { recursive: true })', home],
    ]) {
      expect(code("js", source), source).toContain(target);
    }
  });
});

describe("signals", () => {
  test("allows a descendant, refuses a non-descendant, pkill, killall, and unresolvable pids", async () => {
    const child = Bun.spawn(["sleep", "30"], { stdout: "ignore", stderr: "ignore" });
    try {
      // A child of this process, which stands in for the pane's Oh My Pi process.
      expect(bash(`kill ${child.pid}`)).toBeUndefined();
      expect(bash(`kill -TERM ${child.pid}`)).toBeUndefined();
      expect(bash(`kill 1`)).toContain("init");
      expect(bash(`kill -9 ${process.ppid}`)).toContain(
        `is not a descendant of this pane's Oh My Pi process (pid ${process.pid})`
      );
      expect(bash("kill -9 -1")).toContain("process group 1");
      expect(
        bash(`ids=(); sleep 60 & ids[0]=$!; for pid in "\${ids[@]}"; do kill "$pid"; done`)
      ).toBeUndefined();
      expect(
        bash(
          `declare -a ids=(); sleep 60 & ids[0]=$!; for pid in "\${ids[@]}"; do kill "$pid"; done`
        )
      ).toBeUndefined();
      expect(bash(`sleep 60 & ids[0]=$!; n=0; kill "\${ids[$n]}"`)).toBeUndefined();
      expect(bash("kill $$")).toContain("this pane's own Oh My Pi process");
      expect(bash("pkill -x sleep")).toContain("by name or pattern");
      expect(bash("killall sleep")).toContain("by name or pattern");
      expect(bash("sudo pkill -f 'node server'")).toContain("pkill");
      expect(bash('kill "$(cat server.pid)"')).toContain("cannot resolve the pid");
      expect(bash("tmux kill-server")).toContain("needs a socket");
      expect(bash("tmux -L scratch kill-server")).toContain("needs a socket");
      const foreignTmux = bash("tmux -L scratch new-session -d; tmux -L scratch kill-server");
      expect(foreignTmux).toBeDefined();
      expect(foreignTmux ?? "").toContain("needs a socket");
      for (const command of [
        "tmux kill-serv",
        "tmux killw",
        "tmux killp",
        "tmux respawn-pane -k",
      ]) {
        expect(bash(command), command).toContain("needs a socket");
      }
      expect(bash(`echo ${process.ppid} | xargs kill`)).toContain("standard input");
      const pidFile = path.join(scratch, "mine", "child.pid");
      expect(
        bash(`sleep 60 & echo "$!" > ${pidFile}; pid="$(<${pidFile})"; kill "$pid"`)
      ).toBeUndefined();
      expect(
        bash(
          `pid_file=${pidFile}; sleep 60 & echo "$!" > "$pid_file"; pid="$(<"$pid_file")"; kill "$pid"`
        )
      ).toBeUndefined();
      // Allowed without a pid lookup: a job, the shell's own last background job, a probe.
      expect(bash("sleep 60 & kill $!")).toBeUndefined();
      expect(bash('pid=""; kill "$pid"')).toBeUndefined();
      expect(
        bash(
          'sleep 60 & first=$!; sleep 60 & second=$!; for pid in "$first" "$second"; do kill "$pid"; done'
        )
      ).toBeUndefined();
      expect(
        bash('empty=""; sleep 60 & child=$!; for pid in "$empty" "$child"; do kill "$pid"; done')
      ).toBeUndefined();
      expect(bash("kill %1")).toBeUndefined();
      expect(bash("kill -0 1")).toBeUndefined();
      expect(bash("kill -l")).toBeUndefined();
      expect(guard.code("py", `import os\nos.kill(${process.ppid}, 9)`, workspace, env)).toContain(
        "is not a descendant"
      );
      expect(
        guard.code("py", `import os\nos.kill(${child.pid}, 9)`, workspace, env)
      ).toBeUndefined();
    } finally {
      child.kill();
      await child.exited;
    }
  });

  test("resolves the tmux socket before allowing a scratch path", () => {
    const attachedEnv = { ...env, TMUX: `${home}/.tmux/sockets/tmux-1000/legion-agentc,4242,0` };
    const foreignDefault = guard.bash(
      'TMUX_TMPDIR="$LEGION_WORKSPACE/t" tmux kill-server',
      workspace,
      attachedEnv
    );
    expect(foreignDefault).toBeDefined();
    expect(foreignDefault ?? "").toContain("needs a socket");
    const foreignSocket = guard.bash(
      `TMUX_TMPDIR="${scratch}/mine" tmux -S "${home}/.tmux/sockets/tmux-1000/legion-agentc" kill-server`,
      workspace,
      attachedEnv
    );
    expect(foreignSocket).toBeDefined();
    expect(foreignSocket ?? "").toContain("needs a socket");
    expect(
      guard.bash(
        `TMUX_TMPDIR="${scratch}/mine" tmux -S "${scratch}/mine/tmux" kill-server`,
        workspace,
        attachedEnv
      )
    ).toBeUndefined();
    expect(
      guard.bash(
        'env -u TMUX TMUX_TMPDIR="$LEGION_WORKSPACE/t" tmux kill-server',
        workspace,
        attachedEnv
      )
    ).toBeUndefined();
    expect(
      guard.bash(
        'TMUX_TMPDIR="$LEGION_WORKSPACE/t" tmux -L pane-private kill-server',
        workspace,
        attachedEnv
      )
    ).toBeUndefined();
  });

  test("follows a trap handler after `--`", () => {
    expect(bash("trap -- 'rm -rf ~' EXIT")).toContain(home);
  });

  test("walks /proc ancestry past intermediate processes", () => {
    const parents: Record<number, number> = { 400: 300, 300: 200, 200: 100, 500: 1 };
    const traced = createPaneGuard({
      workspace,
      ompPid: 100,
      scratch,
      parentOf: (pid) => parents[pid],
    });
    expect(traced.bash("kill 400", workspace, env)).toBeUndefined();
    expect(traced.bash("kill 500", workspace, env)).toContain("is not a descendant");
    // A pid that no longer exists receives nothing.
    expect(traced.bash("kill 999", workspace, env)).toBeUndefined();
  });
});

describe("hub process starts", () => {
  test("hold a program started without a shell to the same rules", () => {
    expect(guard.argv(["rm", "-rf", home], workspace, env)).toContain(home);
    expect(guard.argv(["bash", "-c", "rm -rf ~"], workspace, env)).toContain("`~`");
    expect(guard.argv(["pkill", "node"], workspace, env)).toContain("pkill");
    expect(guard.argv(["bun", "run", "dev"], workspace, env)).toBeUndefined();
  });
});
