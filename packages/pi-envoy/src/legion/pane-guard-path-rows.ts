import { spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import {
  chmodSync,
  lstatSync,
  mkdirSync,
  mkdtempSync,
  readdirSync,
  readFileSync,
  readlinkSync,
  rmSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import * as path from "node:path";
import { createPaneGuard, type PaneGuard } from "./pane-guard";

/** One command measured twice: what `guard.bash` returns, and what real bash does to a canary
 * HOME. The row carries no expected verdict — the expectation is what bash did. */
export interface PathRow {
  readonly name: string;
  /** The operation class a control vouches for; a control only speaks for its own family. */
  readonly family: string;
  readonly role: "probe" | "must-refuse" | "must-allow";
  readonly command: string;
  /** Whether the property under test is a `..` component; asserted to be literally present. */
  readonly dotdot: boolean;
}

/** The canary HOME's victims, and the fixture the guard and bash both see. `e` is the symlink
 * whose parent is the operator's home; a `..` after it is the whole subject. Every path below is
 * written as a literal string: `path.join` would normalise the `..` away and the row would
 * measure nothing. */
export interface PathFixture {
  readonly base: string;
  readonly workspace: string;
  readonly scratch: string;
  readonly home: string;
}

const PAYLOAD = 'rm -f "$HOME/.bashrc"\n';

export function buildPathFixture(base: string): PathFixture {
  const ws = path.join(base, "ws");
  const scratch = path.join(base, "tmp");
  const home = path.join(base, "home");
  for (const dir of [
    ws,
    path.join(ws, "d"),
    path.join(ws, "sub"),
    path.join(ws, "perm"),
    path.join(ws, "stage"),
    path.join(ws, "a", "b"),
    path.join(scratch, "s"),
    home,
    path.join(home, ".ssh"),
    path.join(home, "keep"),
  ]) {
    mkdirSync(dir, { recursive: true });
  }
  writeFileSync(path.join(ws, "inside"), "WS\n");
  writeFileSync(path.join(ws, "payload"), "PAYLOAD\n");
  writeFileSync(path.join(ws, "d", "in"), "WS\n");
  writeFileSync(path.join(ws, "sub", "f"), "WS\n");
  writeFileSync(path.join(ws, "run.sh"), "echo ok\n");
  writeFileSync(path.join(ws, "danger.sh"), "echo benign\n");
  // A script already on disk holding a destructive line, which the guard has never read: the
  // victim of every write-then-run row (#1551, #1553).
  writeFileSync(path.join(ws, "unread.sh"), PAYLOAD);
  writeFileSync(path.join(scratch, "s", "f"), "TMP\n");
  writeFileSync(path.join(home, ".bashrc"), "CANARY-ORIGINAL\n");
  writeFileSync(path.join(home, ".ssh", "id_rsa"), "CANARY-ORIGINAL\n");
  writeFileSync(path.join(home, "keep", "secret"), "CANARY-ORIGINAL\n");
  writeFileSync(path.join(home, "danger.sh"), PAYLOAD);
  writeFileSync(path.join(home, "other.sh"), PAYLOAD);
  writeFileSync(
    path.join(home, "danger.py"),
    "import os\nos.remove(os.environ['HOME'] + '/.bashrc')\n"
  );
  // `e` points at a directory inside the home, so `e/..` is the home itself.
  symlinkSync(path.join(home, ".ssh"), path.join(ws, "e"));
  symlinkSync(home, path.join(ws, "hs"));
  symlinkSync(path.join(ws, "sub"), path.join(ws, "in"));
  // `lnk` points at a nested subdirectory two levels below the workspace, so `lnk/..` is
  // `ws/a` — still inside the workspace, but a different directory than `lnk` sits under,
  // which a lexical reading of `lnk/../unread.sh` would miss.
  symlinkSync(path.join(ws, "a", "b"), path.join(ws, "lnk"));
  // A link in the operator's home pointing back into the workspace: `cd`'s logical and physical
  // modes land in different places through it, which is what `-L`/`-P` precedence decides.
  symlinkSync(path.join(ws, "sub"), path.join(home, "mine"));
  // A link already on disk whose target is `..`: the control for a command that makes one itself.
  symlinkSync("..", path.join(ws, "sub", "up"));
  // A relative link inside `sub` back up to an existing sibling directory: `readlink -f`/
  // `--canonicalize` on it must print `ws/d`, not treat the long-option spelling as unknown.
  symlinkSync(path.join("..", "d"), path.join(ws, "sub", "in2"));
  symlinkSync(path.join(home, ".gone"), path.join(ws, "dangling"));
  // A link into a directory this workspace does not have yet: an ordinary build layout, and the
  // cost of treating a non-final component the guard cannot follow as unknown.
  symlinkSync(path.join(ws, "build", "out"), path.join(ws, "latest"));
  symlinkSync(path.join(ws, "loop2"), path.join(ws, "loop"));
  symlinkSync(path.join(ws, "loop"), path.join(ws, "loop2"));
  symlinkSync(path.join(home, ".ssh"), path.join(ws, "perm", "inner"));
  writeFileSync(path.join(ws, "stage", ".bashrc"), "PWNED-BY-TAR\n");
  const tar = spawnSync("tar", ["cf", path.join(ws, "arc.tar"), "-C", path.join(ws, "stage"), "."]);
  if (tar.status !== 0) throw new Error(`fixture tar failed: ${tar.stderr?.toString()}`);
  const zip = spawnSync("zip", ["-q", path.join(ws, "arc.zip"), ".bashrc"], {
    cwd: path.join(ws, "stage"),
  });
  if (zip.status !== 0) throw new Error(`fixture zip failed: ${zip.stderr?.toString()}`);
  chmodSync(path.join(ws, "perm"), 0o000);
  return { base, workspace: ws, scratch, home };
}

/** Every name, mode and content under the canary home, so a removal, an overwrite, a mode change
 * and a new file all change the digest. A directory the row made unreadable is recorded as such,
 * which is itself a change rather than an error. */
export function canaryDigest(home: string): string {
  const hash = createHash("md5");
  let entries = 0;
  const walk = (dir: string, prefix: string): void => {
    let names: string[];
    try {
      names = readdirSync(dir).sort();
    } catch {
      hash.update(`${prefix}\u0000UNREADABLE\u0001`);
      return;
    }
    for (const name of names) {
      const full = path.join(dir, name);
      const rel = `${prefix}${name}`;
      entries += 1;
      try {
        const info = lstatSync(full);
        hash.update(
          `${rel}\u0000${(info.mode & 0o7777).toString(8)}\u0000${info.uid}:${info.gid}\u0000`
        );
        if (info.isSymbolicLink()) hash.update(`L${readlinkSync(full)}`);
        else if (info.isDirectory()) walk(full, `${rel}/`);
        else hash.update(createHash("md5").update(readFileSync(full)).digest("hex"));
      } catch {
        hash.update(`${rel}\u0000UNREADABLE`);
      }
      hash.update("\u0001");
    }
  };
  walk(home, "");
  // A digest over nothing would make every row look intact; the count is the positive control.
  return `${entries}:${hash.digest("hex")}`;
}

/** A `..` standing as a whole path component. A row whose property is the `..` must carry one
 * literally: `path.join` normalises a `..` away, so a fixture built with it would leave the row
 * passing while measuring nothing at all. The test asserts this over every such row. */
export const DOTDOT_COMPONENT = /(?:^|[\s"'(/])\.\.(?:[\s"')/]|$)/;

export const PATH_ROWS: readonly PathRow[] = [
  // --- deletion ---
  {
    name: "rm.dotdot.symlink",
    family: "delete",
    role: "probe",
    command: "rm -f e/../.bashrc",
    dotdot: true,
  },
  {
    name: "rm.rf.dotdot.symlink",
    family: "delete",
    role: "probe",
    command: "rm -rf e/../keep",
    dotdot: true,
  },
  {
    name: "unlink.dotdot.symlink",
    family: "delete",
    role: "probe",
    command: "unlink e/../.bashrc",
    dotdot: true,
  },
  {
    name: "shred.dotdot.symlink",
    family: "delete",
    role: "probe",
    command: "shred -u e/../.bashrc",
    dotdot: true,
  },
  {
    name: "rm.symlink.nodotdot",
    family: "delete",
    role: "must-refuse",
    command: "rm -f hs/.bashrc",
    dotdot: false,
  },
  {
    name: "rm.outside.literal",
    family: "delete",
    role: "must-refuse",
    command: 'rm -f "$HOME/.bashrc"',
    dotdot: false,
  },
  {
    name: "rm.dotdot.realdir",
    family: "delete",
    role: "must-allow",
    command: "rm -f d/../inside",
    dotdot: true,
  },
  {
    name: "rm.dotdot.symlink.inside",
    family: "delete",
    role: "must-allow",
    command: "rm -f in/../inside",
    dotdot: true,
  },
  {
    name: "rm.dotdot.twice.inside",
    family: "delete",
    role: "must-allow",
    command: "rm -f d/../d/../inside",
    dotdot: true,
  },
  {
    name: "rm.plain.inside",
    family: "delete",
    role: "must-allow",
    command: "rm -f inside",
    dotdot: false,
  },
  {
    name: "rm.symlink.inside.nodotdot",
    family: "delete",
    role: "must-allow",
    command: "rm -f in/f",
    dotdot: false,
  },
  {
    name: "rm.dotdot.scratch",
    family: "delete",
    role: "must-allow",
    command: 'rm -f "$TMPDIR"/s/../s/f',
    dotdot: true,
  },
  {
    name: "rm.glob.dotdot.symlink",
    family: "delete",
    role: "probe",
    // A glob's stem is resolved the same as any other target: `e` followed before the `..`.
    command: "rm -f e/../keep/*",
    dotdot: true,
  },
  {
    name: "rm.glob.stem.unresolved",
    family: "delete",
    role: "probe",
    // `late` does not exist until this same command creates it, so at check time the stem is
    // unresolvable — the unknown rule, not automatically harmless, has to refuse this too.
    command: 'ln -s "$HOME/.ssh" late && rm -f late/../.bash*',
    dotdot: true,
  },
  {
    name: "rm.glob.dotdot.realdir",
    family: "delete",
    role: "must-allow",
    command: "rm -f d/../sub/*",
    dotdot: true,
  },

  // --- move ---
  {
    name: "mv.source.dotdot.symlink",
    family: "move",
    role: "probe",
    command: "mv e/../.bashrc taken",
    dotdot: true,
  },
  {
    name: "mv.dest.dotdot.symlink",
    family: "move",
    role: "probe",
    command: "mv payload e/../.bashrc",
    dotdot: true,
  },
  {
    name: "mv.symlink.nodotdot",
    family: "move",
    role: "must-refuse",
    command: "mv payload hs/.bashrc",
    dotdot: false,
  },
  {
    name: "mv.dotdot.realdir",
    family: "move",
    role: "must-allow",
    command: "mv payload d/../moved",
    dotdot: true,
  },

  // --- truncation ---
  {
    name: "truncate.dotdot.symlink",
    family: "truncate",
    role: "probe",
    command: "truncate -s 0 e/../.bashrc",
    dotdot: true,
  },
  {
    name: "truncate.symlink.nodotdot",
    family: "truncate",
    role: "must-refuse",
    command: "truncate -s 0 hs/.bashrc",
    dotdot: false,
  },
  {
    name: "truncate.dotdot.realdir",
    family: "truncate",
    role: "must-allow",
    command: "truncate -s 0 d/../inside",
    dotdot: true,
  },

  // --- overwrite by redirection ---
  {
    name: "redirect.dotdot.symlink",
    family: "overwrite",
    role: "probe",
    command: "echo pwned > e/../.bashrc",
    dotdot: true,
  },
  {
    name: "redirect.empty.dotdot.symlink",
    family: "overwrite",
    role: "probe",
    command: ": > e/../.bashrc",
    dotdot: true,
  },
  {
    name: "tee.dotdot.symlink",
    family: "overwrite",
    role: "probe",
    command: "tee e/../.bashrc < payload",
    dotdot: true,
  },
  {
    name: "redirect.symlink.nodotdot",
    family: "overwrite",
    role: "must-refuse",
    command: "echo pwned > hs/.bashrc",
    dotdot: false,
  },
  {
    name: "redirect.dotdot.realdir",
    family: "overwrite",
    role: "must-allow",
    command: "echo ok > d/../inside",
    dotdot: true,
  },
  {
    name: "redirect.dotdot.newfile",
    family: "overwrite",
    role: "must-allow",
    command: "echo ok > d/../fresh",
    dotdot: true,
  },

  // --- copying programs: the create-the-target residual #1551 names, measured with its control ---
  {
    name: "cp.dotdot.symlink",
    family: "copy",
    role: "probe",
    command: "cp payload e/../.bashrc",
    dotdot: true,
  },
  {
    name: "dd.dotdot.symlink",
    family: "copy",
    role: "probe",
    command: "dd if=payload of=e/../.bashrc status=none",
    dotdot: true,
  },
  {
    name: "install.dotdot.symlink",
    family: "copy",
    role: "probe",
    command: "install -m 644 payload e/../.bashrc",
    dotdot: true,
  },
  {
    name: "ln.sf.dotdot.symlink",
    family: "copy",
    role: "probe",
    command: "ln -sf payload e/../.bashrc",
    dotdot: true,
  },
  {
    name: "cp.symlink.nodotdot",
    family: "copy",
    role: "must-refuse",
    command: "cp payload hs/.bashrc",
    dotdot: false,
  },
  {
    name: "cp.dotdot.realdir",
    family: "copy",
    role: "must-allow",
    command: "cp payload d/../copied",
    dotdot: true,
  },

  // --- recursive mode and owner ---
  {
    name: "chmod.R.dotdot.symlink",
    family: "recursive-mode",
    role: "probe",
    command: "chmod -R 000 e/../keep",
    dotdot: true,
  },
  {
    name: "chgrp.R.dotdot.symlink",
    family: "recursive-mode",
    role: "probe",
    command: "chgrp -R adm e/../keep",
    dotdot: true,
  },
  {
    name: "chown.R.dotdot.symlink",
    family: "recursive-mode",
    role: "probe",
    command: "chown -R root e/../keep",
    dotdot: true,
  },
  {
    name: "chmod.R.symlink.nodotdot",
    family: "recursive-mode",
    role: "must-refuse",
    command: "chmod -R 000 hs/keep",
    dotdot: false,
  },
  {
    name: "chmod.R.dotdot.realdir",
    family: "recursive-mode",
    role: "must-allow",
    command: "chmod -R 700 d/../sub",
    dotdot: true,
  },
  {
    name: "chmod.R.dotdot.workspace",
    family: "recursive-mode",
    role: "must-allow",
    command: "chmod -R 700 d/..",
    dotdot: true,
  },
  {
    name: "chmod.R.dotglob.parent",
    family: "recursive-mode",
    role: "probe",
    // With `globskipdots` off, a leading-dot glob matches `.` and `..` too, so its stem is the
    // PARENT of the resolved directory, not the directory itself. `000` would chmod the parent
    // unreadable before `chmod -R` can recurse into it, masking the damage; `700` does not.
    command: "shopt -u globskipdots; chmod -R 700 .*",
    dotdot: false,
  },

  // --- extraction and find ---
  {
    name: "tar.C.dotdot.symlink",
    family: "extract",
    role: "probe",
    command: "tar xf arc.tar -C e/..",
    dotdot: true,
  },
  {
    name: "tar.C.symlink.nodotdot",
    family: "extract",
    role: "must-refuse",
    command: "tar xf arc.tar -C hs",
    dotdot: false,
  },
  {
    name: "tar.C.dotdot.realdir",
    family: "extract",
    role: "must-allow",
    command: "tar xf arc.tar -C d/../sub",
    dotdot: true,
  },
  {
    name: "unzip.d.dotdot.symlink",
    family: "extract",
    role: "probe",
    command: "unzip -o -q arc.zip -d e/..",
    dotdot: true,
  },
  {
    name: "unzip.d.dotdot.realdir",
    family: "extract",
    role: "must-allow",
    command: "unzip -o -q arc.zip -d d/../sub",
    dotdot: true,
  },
  {
    name: "rsync.dotdot.symlink",
    family: "sync",
    role: "probe",
    command: "rsync payload e/../.bashrc",
    dotdot: true,
  },
  {
    name: "rsync.symlink.nodotdot",
    family: "sync",
    role: "must-refuse",
    command: "rsync payload hs/.bashrc",
    dotdot: false,
  },
  {
    name: "rsync.dotdot.realdir",
    family: "sync",
    role: "must-allow",
    command: "rsync payload d/../synced",
    dotdot: true,
  },
  {
    name: "find.delete.dotdot.symlink",
    family: "find",
    role: "probe",
    command: "find e/.. -name .bashrc -delete",
    dotdot: true,
  },
  {
    name: "find.delete.symlink.nodotdot",
    family: "find",
    role: "must-refuse",
    command: "find hs/ -name .bashrc -delete",
    dotdot: false,
  },
  {
    name: "find.delete.dotdot.realdir",
    family: "find",
    role: "must-allow",
    command: "find d/../sub -name f -delete",
    dotdot: true,
  },

  // --- running a script ---
  {
    name: "bash.dotdot.symlink.shadowed",
    family: "run",
    role: "probe",
    command: "bash e/../danger.sh",
    dotdot: true,
  },
  {
    name: "bash.dotdot.symlink.absent",
    family: "run",
    role: "probe",
    command: "bash e/../other.sh",
    dotdot: true,
  },
  {
    name: "bash.dotdot.symlink.unresolved",
    family: "run",
    role: "probe",
    // `late` does not exist until this same command creates it: `runFile` has its own unknown
    // check, unrelated to `judgePath`'s, and nothing else in the family exercises it.
    command: 'ln -s "$HOME/.ssh" late && bash late/../danger.sh',
    dotdot: true,
  },
  {
    name: "sh.dotdot.symlink",
    family: "run",
    role: "probe",
    command: "sh e/../other.sh",
    dotdot: true,
  },
  {
    name: "source.dotdot.symlink",
    family: "run",
    role: "probe",
    command: ". e/../other.sh",
    dotdot: true,
  },
  {
    name: "python.dotdot.symlink",
    family: "run",
    role: "probe",
    command: "python3 e/../danger.py",
    dotdot: true,
  },
  {
    name: "bash.symlink.nodotdot",
    family: "run",
    role: "must-refuse",
    command: "bash hs/danger.sh",
    dotdot: false,
  },
  {
    name: "bash.dotdot.realdir",
    family: "run",
    role: "must-allow",
    command: "bash sub/../run.sh",
    dotdot: true,
  },
  {
    name: "bash.dotdot.symlink.inside",
    family: "run",
    role: "must-allow",
    command: "bash in/../run.sh",
    dotdot: true,
  },

  // --- the working directory: bash keeps a logical PWD, open(2) uses the physical one ---
  {
    name: "cd.symlink.then.dotdot",
    family: "cwd",
    role: "probe",
    command: "cd e && rm -f ../.bashrc",
    dotdot: true,
  },
  {
    name: "cd.symlink.then.script",
    family: "cwd",
    role: "probe",
    command: "cd e && bash ../danger.sh",
    dotdot: true,
  },
  {
    name: "cd.P.dotdot",
    family: "cwd",
    role: "probe",
    command: "cd -P e/.. && rm -f .bashrc",
    dotdot: true,
  },
  {
    name: "env.chdir.dotdot",
    family: "cwd",
    role: "probe",
    command: "env -C e/.. rm -f .bashrc",
    dotdot: true,
  },

  // Bash takes the LAST of `-L` and `-P`, across words and within one, so a `P` anywhere in
  // the options is not physical mode. Reading it as physical sent the guard to the workspace
  // while bash went logically to the home directory.
  {
    name: "cd.PL.logical",
    family: "cwd",
    role: "probe",
    command: `cd -PL ../home/mine/.. && rm -f .bashrc`,
    dotdot: true,
  },
  {
    name: "cd.P.L.logical",
    family: "cwd",
    role: "probe",
    command: `cd -P -L ../home/mine/.. && rm -f .bashrc`,
    dotdot: true,
  },
  {
    name: "cd.LP.physical",
    family: "cwd",
    role: "must-allow",
    command: `cd -LP ../home/mine/.. && rm -f inside`,
    dotdot: true,
  },
  {
    name: "cd.L.P.physical",
    family: "cwd",
    role: "must-allow",
    command: `cd -L -P ../home/mine/.. && rm -f inside`,
    dotdot: true,
  },
  {
    name: "cd.symlink.nodotdot",
    family: "cwd",
    role: "must-refuse",
    command: "cd hs && rm -f .bashrc",
    dotdot: false,
  },
  {
    name: "cd.realdir.then.dotdot",
    family: "cwd",
    role: "must-allow",
    command: "cd d && rm -f ../inside",
    dotdot: true,
  },
  {
    name: "cd.symlink.back.inside",
    family: "cwd",
    role: "must-allow",
    command: "cd in && cd .. && rm -f inside",
    dotdot: true,
  },
  {
    name: "env.chdir.dotdot.realdir",
    family: "cwd",
    role: "must-allow",
    command: "env -C d/.. rm -f inside",
    dotdot: true,
  },
  {
    name: "env.chdir.eq.dotdot",
    family: "cwd",
    role: "probe",
    // The `--chdir=DIR` spelling chdirs the same as `-C DIR`, and must resolve the same way.
    command: "env --chdir=e/.. rm -f .bashrc",
    dotdot: true,
  },
  {
    name: "env.chdir.eq.dotdot.realdir",
    family: "cwd",
    role: "must-allow",
    command: "env --chdir=d/.. rm -f inside",
    dotdot: true,
  },

  // --- a path the guard computes from a command's output ---
  {
    name: "realpath.dotdot.symlink",
    family: "substitution",
    role: "probe",
    command: 'rm -f "$(realpath e/../.bashrc)"',
    dotdot: true,
  },
  {
    name: "readlink.f.dotdot.symlink",
    family: "substitution",
    role: "probe",
    command: 'rm -f "$(readlink -f e/../.bashrc)"',
    dotdot: true,
  },
  {
    name: "readlink.nof.letter.f",
    family: "substitution",
    role: "probe",
    // `fup` contains the letter `f`; a target-text search for it, instead of reading `-f` from
    // the options, wrongly treats a plain `readlink` as canonicalizing.
    command: 'ln -s .. sub/fup && rm -rf "$(readlink sub/fup)"/home/keep',
    dotdot: true,
  },
  {
    name: "dirname.dotdot.symlink",
    family: "substitution",
    role: "probe",
    command: 'chmod -R 000 "$(dirname e/../keep/secret)"',
    dotdot: true,
  },
  {
    name: "mktemp.template.dotdot",
    family: "substitution",
    role: "probe",
    command: 't=$(mktemp -u e/../tmpXXXXXX); chmod -R 000 "$(dirname "$t")"',
    dotdot: true,
  },
  {
    name: "realpath.symlink.nodotdot",
    family: "substitution",
    role: "must-refuse",
    command: 'rm -f "$(realpath hs/.bashrc)"',
    dotdot: false,
  },
  {
    name: "realpath.dotdot.realdir",
    family: "substitution",
    role: "must-allow",
    command: 'rm -f "$(realpath d/../inside)"',
    dotdot: true,
  },
  {
    name: "dirname.dotdot.realdir",
    family: "substitution",
    role: "must-allow",
    command: 'chmod -R 700 "$(dirname d/../sub/f)"',
    dotdot: true,
  },
  {
    name: "basename.dotdot.symlink",
    family: "substitution",
    role: "probe",
    command: 'chmod -R 000 "$LEGION_WORKSPACE/$(basename e/..)"',
    dotdot: true,
  },
  {
    name: "basename.dotdot.realdir",
    family: "substitution",
    role: "probe",
    command: 'chmod -R 000 "$LEGION_WORKSPACE/$(basename d/..)"',
    dotdot: true,
  },
  {
    name: "basename.plain",
    family: "substitution",
    role: "must-allow",
    command: 'rm -f "$LEGION_WORKSPACE/$(basename d/in)"',
    dotdot: false,
  },
  {
    name: "realpath.s.dotdot.symlink",
    family: "substitution",
    role: "probe",
    // `-s`/`--strip`/`--no-symlinks` expand no symlink at all: bash prints the lexical `..`.
    command: 'chmod -R 000 "$(realpath -s "$HOME/mine/../keep")"',
    dotdot: true,
  },
  {
    name: "realpath.strip.dotdot.symlink",
    family: "substitution",
    role: "probe",
    command: 'chmod -R 000 "$(realpath --strip "$HOME/mine/../keep")"',
    dotdot: true,
  },
  {
    name: "realpath.no-symlinks.dotdot.symlink",
    family: "substitution",
    role: "probe",
    command: 'chmod -R 000 "$(realpath --no-symlinks "$HOME/mine/../keep")"',
    dotdot: true,
  },
  {
    name: "realpath.L.dotdot.symlink",
    family: "substitution",
    role: "probe",
    // `-L`/`--logical` takes the text's own `..` before following the symlink in front of it.
    command: 'chmod -R 000 "$(realpath -L "$HOME/mine/../keep")"',
    dotdot: true,
  },
  {
    name: "realpath.logical.dotdot.symlink",
    family: "substitution",
    role: "probe",
    command: 'chmod -R 000 "$(realpath --logical "$HOME/mine/../keep")"',
    dotdot: true,
  },
  {
    name: "realpath.physical.dotdot.symlink",
    family: "substitution",
    role: "must-allow",
    // Plain `realpath` is physical, the guard's default: `mine` resolves to `sub`, so the `..`
    // lands back in the workspace, on a name that does not exist there.
    command: 'chmod -R 700 "$(realpath "$HOME/mine/../keep")"',
    dotdot: true,
  },
  {
    name: "realpath.nodotdot.outside",
    family: "substitution",
    role: "must-refuse",
    command: 'chmod -R 000 "$(realpath "$HOME/keep")"',
    dotdot: false,
  },
  {
    name: "realpath.s.logical.cwd",
    family: "substitution",
    role: "probe",
    // `e` is a symlink: bash's kernel cwd after `cd e` is physically the home, though the
    // logical `$PWD` text still reads `e`. A lexical mode must resolve the cwd itself first.
    command: 'cd e && chmod -R 000 "$(realpath -s ..)"',
    dotdot: true,
  },
  {
    name: "realpath.s.logical.cwd.inside",
    family: "substitution",
    role: "must-allow",
    command: 'cd in && chmod -R 700 "$(realpath -s ..)/sub"',
    dotdot: true,
  },
  {
    name: "realpath.L.logical.cwd",
    family: "substitution",
    role: "probe",
    // Same as `-s` above, for `-L`: after `cd e`, the physical cwd is the canary home, though
    // the logical `$PWD` text still reads `e`.
    command: 'cd e && chmod -R 000 "$(realpath -L ..)"',
    dotdot: true,
  },
  {
    name: "realpath.L.logical.cwd.inside",
    family: "substitution",
    role: "must-allow",
    command: 'cd in && chmod -R 700 "$(realpath -L ..)/sub"',
    dotdot: true,
  },
  {
    name: "realpath.P.L.lexical",
    family: "substitution",
    role: "probe",
    // Bash takes the LAST of `-L` and `-P`, so this is logical: a `P` anywhere is not physical.
    command: 'chmod -R 000 "$(realpath -P -L "$HOME/mine/../keep")"',
    dotdot: true,
  },
  {
    name: "realpath.L.P.physical",
    family: "substitution",
    role: "must-allow",
    command: 'chmod -R 700 "$(realpath -L -P "$HOME/mine/../keep")"',
    dotdot: true,
  },
  {
    name: "realpath.s.P.physical",
    family: "substitution",
    role: "probe",
    // Bash takes the LAST of `-s` and `-P`, so `-s` does not win merely by being present: this
    // is physical. `e` differs from its physical target, so lexical and physical disagree here.
    command: 'chmod -R 000 "$(realpath -s -P e/../keep)"',
    dotdot: true,
  },
  {
    name: "realpath.P.s.lexical",
    family: "substitution",
    role: "must-allow",
    command: 'chmod -R 700 "$(realpath -P -s e/../keep)"',
    dotdot: true,
  },
  {
    name: "realpath.strip.physical.long",
    family: "substitution",
    role: "probe",
    // `--physical` maps onto the same last-wins letter as `-P`: unmapped, it would count for
    // nothing and leave the earlier `--strip` (lexical) standing.
    command: 'chmod -R 000 "$(realpath --strip --physical e/../keep)"',
    dotdot: true,
  },
  {
    name: "realpath.s.L.final",
    family: "substitution",
    role: "probe",
    // No `..` at all: `-s` never follows any symlink, `-L` still follows the final component's
    // own. A `-s`-always-wins bug prints the link itself; the fix follows it to the canary home.
    command: 'rm -rf "$(realpath -s -L e)"',
    dotdot: false,
  },
  {
    name: "realpath.s.trailing.symlink",
    family: "substitution",
    role: "must-allow",
    // The mirror of the probe above: with only `-s` given, swapping the `-s`/`-L` branches
    // would follow `e`'s own symlink physically instead of printing it unresolved.
    command: 'rm -f "$(realpath -s e)"',
    dotdot: false,
  },
  {
    name: "readlink.e.suffix.appended",
    family: "substitution",
    role: "probe",
    // `-e`/`--canonicalize-existing` prints NOTHING and exits nonzero when any component,
    // including the last, does not exist. Modelling a path for it regardless lets an appended
    // suffix read as an in-workspace path while bash, having printed nothing, targets the
    // suffix alone.
    command: 'rm -rf "$(readlink -e nope)$HOME/keep"',
    dotdot: false,
  },
  {
    name: "readlink.f.e.last",
    family: "substitution",
    role: "probe",
    // The mirror of the must-allow below: with `-e` last the mode is `e`, which prints nothing
    // for a missing component, so bash's target is the suffix alone.
    command: 'rm -rf "$(readlink -f -e nope)$HOME/keep"',
    dotdot: false,
  },
  {
    name: "readlink.f.existing.long.last",
    family: "substitution",
    role: "probe",
    command: 'rm -rf "$(readlink -f --canonicalize-existing nope)$HOME/keep"',
    dotdot: false,
  },
  {
    name: "readlink.e.f.wins",
    family: "substitution",
    role: "must-allow",
    // Bash takes the LAST of `-f`/`-e`/`-m`: with `-f` last, the mode is `f`, which always
    // prints a path, so this is not the `-e` case above.
    command: 'rm -rf "$(readlink -e -f sub/gone)"',
    dotdot: false,
  },
  {
    name: "readlink.m.missing.component",
    family: "substitution",
    role: "must-allow",
    command: 'rm -rf "$(readlink -m sub/gone)"',
    dotdot: false,
  },
  {
    name: "readlink.canonicalize.long",
    family: "substitution",
    role: "must-allow",
    // Pins the long-option map, not the short `-f` flag: dropping `"--canonicalize": "f"` reads
    // this as mode `undefined` and refuses it, though bash prints `ws/d` and removes it safely.
    command: 'rm -rf "$(readlink --canonicalize sub/in2)"',
    dotdot: false,
  },
  {
    name: "readlink.canonicalize.missing.long",
    family: "substitution",
    role: "must-allow",
    // Pins the long-option map for `-m`, distinct from `readlink.m.missing.component`'s short
    // flag: dropping `"--canonicalize-missing": "m"` reads this as mode `undefined` too.
    command: 'rm -rf "$(readlink --canonicalize-missing sub/gone)"',
    dotdot: false,
  },
  {
    name: "realpath.physical.strip.control",
    family: "substitution",
    role: "must-allow",
    command: 'chmod -R 700 "$(realpath -P --strip e/../sub)"',
    dotdot: true,
  },

  // --- a resolution that fails: each must be unknown, never harmless ---
  {
    name: "dangling.dotdot",
    family: "unresolvable",
    role: "probe",
    command: "rm -f dangling/../.bashrc",
    dotdot: true,
  },
  {
    name: "dangling.dotdot.filled",
    family: "unresolvable",
    role: "probe",
    command: "mkdir -p e/../.gone && rm -f dangling/../.bashrc",
    dotdot: true,
  },
  {
    name: "absent.dotdot",
    family: "unresolvable",
    role: "probe",
    command: "rm -f nope/../.bashrc",
    dotdot: true,
  },
  {
    name: "absent.dotdot.created",
    family: "unresolvable",
    role: "probe",
    command: 'ln -s "$HOME/.ssh" late && rm -f late/../.bashrc',
    dotdot: true,
  },
  {
    name: "denied.dotdot",
    family: "unresolvable",
    role: "probe",
    command: "rm -f perm/inner/../.bashrc",
    dotdot: true,
  },
  {
    name: "denied.dotdot.opened",
    family: "unresolvable",
    role: "probe",
    command: "chmod 755 perm && rm -f perm/inner/../.bashrc",
    dotdot: true,
  },
  {
    name: "denied.nodotdot",
    family: "unresolvable",
    role: "probe",
    command: "rm -f perm/inner/id_rsa",
    dotdot: false,
  },
  {
    name: "denied.nodotdot.opened",
    family: "unresolvable",
    role: "probe",
    command: "chmod 755 perm && rm -f perm/inner/id_rsa",
    dotdot: false,
  },
  {
    name: "loop.dotdot",
    family: "unresolvable",
    role: "probe",
    command: "rm -f loop/../.bashrc",
    dotdot: true,
  },
  {
    name: "loop.dotdot.replaced",
    family: "unresolvable",
    role: "probe",
    command: 'rm -f loop && ln -s "$HOME/.ssh" loop && rm -f loop/../.bashrc',
    dotdot: true,
  },

  // A link into a directory the command creates: harmless in the ordinary build layout, and
  // the same shape is a live bypass when what it creates is a link out of the workspace.
  {
    name: "danglingmid.created",
    family: "unresolvable",
    role: "probe",
    command: `mkdir -p build/out && rm -rf latest/cache`,
    dotdot: false,
  },
  {
    name: "danglingmid.escapes",
    family: "unresolvable",
    role: "probe",
    command: `mkdir -p build && ln -s "$HOME" build/out && rm -rf latest/keep`,
    dotdot: false,
  },
  {
    name: "absent.nodotdot.inside",
    family: "unresolvable",
    role: "must-allow",
    command: "rm -f nope/gone",
    dotdot: false,
  },
  {
    name: "absent.deep.nodotdot.inside",
    family: "unresolvable",
    role: "must-allow",
    command: "rm -rf nope/deeper/gone",
    dotdot: false,
  },
  {
    name: "resolvable.outside",
    family: "unresolvable",
    role: "must-refuse",
    command: 'rm -rf "$HOME/.ssh/../keep"',
    dotdot: true,
  },

  // --- a pid file's model key, which a mismatch can only blur ---
  {
    name: "pidfile.dotdot.symlink",
    family: "pidfile",
    role: "probe",
    command: 'sleep 5 & echo $! > kept.pid; kill "$(< e/../kept.pid)"',
    dotdot: true,
  },
  {
    name: "pidfile.dotdot.realdir",
    family: "pidfile",
    role: "must-allow",
    command: 'sleep 5 & echo $! > kept.pid; kill "$(< d/../kept.pid)"',
    dotdot: true,
  },
  {
    name: "pidfile.foreign.key",
    family: "pidfile",
    role: "must-refuse",
    command: 'sleep 5 & echo $! > kept.pid; kill "$(< hs/kept.pid)"',
    dotdot: false,
  },

  // --- the command changing, during its own run, the namespace the guard resolved against:
  // retargeting a link the guard already followed, or creating the component that decides where
  // a path lands. Neither is the defect this batch closes — there the guard's reading was right
  // when it read it, or there was nothing on disk to read — and neither is closed here.
  {
    name: "retarget.symlink.dotdot",
    family: "retarget",
    role: "probe",
    command: 'ln -sfn "$HOME/.ssh" in && rm -f in/../.bashrc',
    dotdot: true,
  },
  {
    name: "retarget.symlink.nodotdot",
    family: "retarget",
    role: "probe",
    command: 'ln -sfn "$HOME" in && rm -f in/.bashrc',
    dotdot: false,
  },
  {
    name: "retarget.realdir.dotdot",
    family: "retarget",
    role: "probe",
    command: 'rm -rf d && ln -s "$HOME/.ssh" d && rm -f d/../.bashrc',
    dotdot: true,
  },
  {
    name: "retarget.script",
    family: "retarget",
    role: "probe",
    command: 'ln -sfn "$HOME/.ssh" in && bash in/../danger.sh',
    dotdot: true,
  },
  {
    name: "retarget.truncate",
    family: "retarget",
    role: "probe",
    command: 'ln -sfn "$HOME/.ssh" in && truncate -s 0 in/../.bashrc',
    dotdot: true,
  },
  {
    name: "madelink.append",
    family: "retarget",
    role: "probe",
    command: `echo 'echo hi' > unread.sh; ln -s .. sub/made; echo 'rm -f "$HOME/.bashrc"' >> sub/made/unread.sh; bash unread.sh`,
    dotdot: false,
  },
  {
    name: "madelink.group",
    family: "retarget",
    role: "probe",
    command: `echo 'echo hi' > unread.sh; ln -s .. sub/made; { echo 'rm -f "$HOME/.bashrc"'; } > sub/made/unread.sh; bash unread.sh`,
    dotdot: false,
  },
  {
    name: "madelink.tee",
    family: "retarget",
    role: "probe",
    command: `echo 'echo hi' > unread.sh; ln -s .. sub/made; tee sub/made/unread.sh <<EOF
rm -f "$HOME/.bashrc"
EOF
bash unread.sh`,
    dotdot: false,
  },
  {
    name: "prelink.append",
    family: "retarget",
    role: "probe",
    command: `echo 'echo hi' > unread.sh; echo 'rm -f "$HOME/.bashrc"' >> sub/up/unread.sh; bash unread.sh`,
    dotdot: false,
  },
  {
    name: "prelink.tee",
    family: "retarget",
    role: "probe",
    command: `echo 'echo hi' > unread.sh; tee sub/up/unread.sh <<EOF
rm -f "$HOME/.bashrc"
EOF
bash unread.sh`,
    dotdot: false,
  },
  {
    name: "collapsinglink.write",
    family: "unnameable",
    role: "probe",
    command: `mkdir -p a/b; ln -s a/b late; echo 'echo hi' > late/../unread.sh; bash unread.sh`,
    dotdot: true,
  },
  {
    name: "retarget.group.key",
    family: "retarget",
    role: "probe",
    // `lnk` is a pre-existing symlink into `a/b`, so `lnk/..` is `ws/a` — inside the workspace,
    // but not `ws` itself: the write really lands on `a/unread.sh`, a file nothing else reads.
    // A lexical key collapses `lnk/..` away and plants the benign content on the TOP-LEVEL
    // `unread.sh` instead — the file `bash unread.sh` actually runs, still holding its
    // original disk payload in reality.
    command: `{ echo 'echo hi'; } > lnk/../unread.sh; bash unread.sh`,
    dotdot: true,
  },
  {
    name: "collapsinglink.tee",
    family: "unnameable",
    role: "probe",
    command: `mkdir -p a/b; ln -s a/b late; tee late/../unread.sh <<EOF
echo hi
EOF
bash unread.sh`,
    dotdot: true,
  },
  {
    name: "collapsinglink.nolink",
    family: "unnameable",
    role: "must-allow",
    command: `echo 'echo hi' > made.sh; bash made.sh`,
    dotdot: false,
  },

  // An append whose target the guard cannot read AT ALL is the same hazard as one it can read
  // but cannot place: it may be any file, including the one about to run.
  {
    name: "unreadable.append.substitution",
    family: "unnameable",
    role: "probe",
    command: `echo 'echo hi' > t.sh; echo 'rm -f "$HOME/.bashrc"' >> "$(echo t.sh)"; bash t.sh`,
    dotdot: false,
  },
  {
    name: "unreadable.append.glob",
    family: "unnameable",
    role: "probe",
    command: `echo 'echo hi' > t.sh; echo 'rm -f "$HOME/.bashrc"' >> t.s?; bash t.sh`,
    dotdot: false,
  },
  {
    name: "unreadable.append.variable",
    family: "unnameable",
    role: "probe",
    command: `echo 'echo hi' > t.sh; v=$(echo t.sh); echo 'rm -f "$HOME/.bashrc"' >> "$v"; bash t.sh`,
    dotdot: false,
  },
  {
    name: "unreadable.teeappend",
    family: "unnameable",
    role: "probe",
    command: `echo 'echo hi' > t.sh; echo 'rm -f "$HOME/.bashrc"' | tee -a "$(echo t.sh)" >/dev/null; bash t.sh`,
    dotdot: false,
  },
  {
    name: "unreadable.write.lenient",
    family: "unnameable",
    role: "probe",
    command: `echo 'echo hi' > t.sh; echo 'rm -f "$HOME/.bashrc"' > "$(echo t.sh)"; bash t.sh`,
    dotdot: false,
  },
  {
    name: "retarget.outside.literal",
    family: "retarget",
    role: "must-refuse",
    command: 'ln -sfn "$HOME/.ssh" in && rm -f hs/.bashrc',
    dotdot: false,
  },
  {
    name: "retarget.inside",
    family: "retarget",
    role: "must-allow",
    command: "ln -sfn sub in && rm -f in/../inside",
    dotdot: true,
  },

  // --- a write whose file the guard cannot name, which is every modelled file at once: the
  // link it goes through is made by the same command, so the key is unknown while the write
  // still lands on a file the guard holds a model of, or on the script it is about to run.
  {
    name: "unnameable.append.modelled",
    family: "unnameable",
    role: "probe",
    command: `echo 'echo hi' > unread.sh; ln -s sub late; echo 'rm -f "$HOME/.bashrc"' >> late/../unread.sh; bash unread.sh`,
    dotdot: true,
  },
  {
    name: "unnameable.append.ondisk",
    family: "unnameable",
    role: "probe",
    command: `ln -s sub late; echo 'rm -f "$HOME/.bashrc"' >> late/../run.sh; bash run.sh`,
    dotdot: true,
  },
  {
    name: "unnameable.group.ondisk",
    family: "unnameable",
    role: "probe",
    command: `ln -s sub late; { echo 'rm -f "$HOME/.bashrc"'; } > late/../run.sh; bash run.sh`,
    dotdot: true,
  },
  {
    name: "unnameable.tee.ondisk",
    family: "unnameable",
    role: "probe",
    command: `ln -s sub late; tee late/../run.sh <<EOF
rm -f "$HOME/.bashrc"
EOF
bash run.sh`,
    dotdot: true,
  },
  {
    name: "unnameable.outside.literal",
    family: "unnameable",
    role: "must-refuse",
    command: `ln -s sub late; echo x >> late/../log.txt; bash hs/danger.sh`,
    dotdot: true,
  },
  {
    name: "unnameable.named.append",
    family: "unnameable",
    role: "must-allow",
    command: `echo hi >> log.txt; bash run.sh`,
    dotdot: false,
  },
];

/** One row's two observations. `refusal` is what `guard.bash` returned — undefined is ALLOW — and
 * `live` is whether real bash, run in the same fixture with `HOME` on the canary, changed it. */
export interface PathRowResult {
  readonly row: PathRow;
  readonly refusal: string | undefined;
  readonly live: boolean;
  readonly exit: number | null;
}

/** The pane a fixture stands for: the guard rooted on its workspace and scratch, and the
 * environment real bash runs the same command under, with `HOME` on the canary. */
export function fixtureGuard(fixture: PathFixture): { guard: PaneGuard; env: NodeJS.ProcessEnv } {
  return {
    guard: createPaneGuard({
      workspace: fixture.workspace,
      ompPid: process.pid,
      scratch: fixture.scratch,
    }),
    env: {
      HOME: fixture.home,
      LEGION_WORKSPACE: fixture.workspace,
      TMPDIR: fixture.scratch,
      PATH: process.env.PATH,
    },
  };
}

/** Builds the row its own fixture, asks the guard, then runs the command under real bash in that
 * same fixture, so both columns see one filesystem. */
export function measureRow(row: PathRow, root: string): PathRowResult {
  const base = mkdtempSync(path.join(root, "row-"));
  try {
    const fixture = buildPathFixture(base);
    const { guard, env } = fixtureGuard(fixture);
    const refusal = guard.bash(row.command, fixture.workspace, env);
    const before = canaryDigest(fixture.home);
    const run = spawnSync("bash", ["-c", row.command], {
      cwd: fixture.workspace,
      env,
      encoding: "utf8",
      timeout: 30_000,
    });
    return { row, refusal, live: before !== canaryDigest(fixture.home), exit: run.status };
  } finally {
    // A row that ran `chmod -R 000` leaves directories nothing can read or remove.
    spawnSync("chmod", ["-R", "u+rwX", base]);
    rmSync(base, { recursive: true, force: true });
  }
}

export function measureAllPathRows(root: string): PathRowResult[] {
  return PATH_ROWS.map((row) => measureRow(row, root));
}
