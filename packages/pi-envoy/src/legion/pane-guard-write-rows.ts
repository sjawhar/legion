/** The shapes LEGION-357 is measured on: commands that write a path they name, where the path is
 * the operand each verb's own grammar makes the destination. They live here rather than in the
 * test file so that `scripts/measure-pane-guard-writes.ts` runs the same rows against any guard
 * build, and the numbers a pull request or a review states are derived from what the test runs.
 *
 * Every row is measured twice: what `guard.bash` RETURNS, and what real bash does to a canary
 * HOME the row's own fixture holds. No row carries an expected verdict for the dangerous
 * direction — the expectation is what bash did, so a row that stops destroying the canary stops
 * demanding a refusal rather than silently passing. `must-allow` rows carry the other direction,
 * which nothing derived from bash can supply: the command a pane is meant to be able to run.
 */
import { spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import {
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

export interface WriteRow {
  readonly name: string;
  /** The verb whose grammar the row is about; a control speaks only for its own family. */
  readonly family: string;
  /** `must-refuse` is asserted by what bash did, not by this field. `must-allow` is the cost
   * side: a command the pane keeps. `residual` is a shape the documented boundary leaves open,
   * listed by name in the test so closing one is a deliberate edit. */
  readonly role: "must-allow" | "residual" | "probe" | "unreadable";
  readonly command: string;
  /** A separate, earlier tool call, judged and run before the command under test. */
  readonly plant?: string;
  readonly unreadableWord?: string;
}

/** What the guard is given and what bash is given: one directory per row. `e` and `f` are links
 * inside the workspace that point out of it (at a directory and at a file); `home/link` is the
 * mirror, a link outside the roots pointing in, which a verb that replaces its destination
 * destroys. */
export interface WriteFixture {
  readonly base: string;
  readonly workspace: string;
  readonly scratch: string;
  readonly home: string;
}

const CANARY = "CANARY-ORIGINAL\n";

export function buildWriteFixture(base: string): WriteFixture {
  const ws = path.join(base, "ws");
  const scratch = path.join(base, "tmp");
  const home = path.join(base, "home");
  for (const dir of [
    ws,
    path.join(ws, "dir"),
    path.join(scratch, "mine"),
    home,
    path.join(home, ".ssh"),
    path.join(home, "bin"),
  ]) {
    mkdirSync(dir, { recursive: true });
  }
  writeFileSync(path.join(ws, "payload"), "PAYLOAD\n");
  writeFileSync(path.join(ws, "notes.txt"), "NOTES\n");
  writeFileSync(path.join(ws, "dir", "in"), "WS\n");
  writeFileSync(path.join(home, ".bashrc"), CANARY);
  writeFileSync(path.join(home, ".ssh", "id_rsa"), CANARY);
  writeFileSync(path.join(home, "bin", "tool"), CANARY);
  // Inside the workspace, pointing out of it: `e` at a directory, `f` at a file. A verb that
  // descends `e` writes under the home; one that writes through `f` rewrites the profile.
  symlinkSync(path.join(home, ".ssh"), path.join(ws, "e"));
  symlinkSync(path.join(home, ".bashrc"), path.join(ws, "f"));
  // Outside the roots, pointing in: destroying this link destroys a file outside the roots even
  // though what it points at is inside them.
  symlinkSync(path.join(ws, "notes.txt"), path.join(home, "link"));
  return { base, workspace: ws, scratch, home };
}

/** Every name, mode and content under the canary home, so a removal, an overwrite, a mode change,
 * a replaced symlink and a new file all change the digest. The entry count is the positive
 * control: a digest over nothing would make every row look intact. */
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
        hash.update(`${rel}\u0000${(info.mode & 0o7777).toString(8)}\u0000`);
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
  return `${entries}:${hash.digest("hex")}`;
}

/** A value the guard cannot read, so the word carrying it is one it cannot read whole. */
const HIDDEN = (text: string): string => `$(printf '%s' ${text})`;

export const WRITE_ROWS: readonly WriteRow[] = [
  // --- The controls the whole battery rests on --------------------------------------------
  // A verb the guard already judges, in both directions: if either of these two ever changes,
  // no other row in the file means what it says.
  {
    name: "control: rm of the profile",
    family: "control",
    role: "probe",
    command: 'rm -f "$HOME/.bashrc"',
  },
  {
    name: "control: a redirection over the profile",
    family: "control",
    role: "probe",
    command: 'echo pwned > "$HOME/.bashrc"',
  },
  {
    name: "control: a redirection inside the workspace",
    family: "control",
    role: "must-allow",
    command: "echo fine > notes.txt",
  },

  // --- cp ------------------------------------------------------------------------------------
  {
    name: "cp over the profile",
    family: "cp",
    role: "probe",
    command: 'cp payload "$HOME/.bashrc"',
  },
  {
    name: "cp into the home directory, which names the destination file itself",
    family: "cp",
    role: "probe",
    command: 'cp payload "$HOME"',
  },
  {
    name: "cp -t, where every operand is a source and the option carries the destination",
    family: "cp",
    role: "probe",
    command: 'cp -t "$HOME" payload',
  },
  {
    name: "cp --target-directory=, the same destination joined to its option",
    family: "cp",
    role: "probe",
    command: 'cp --target-directory="$HOME" payload',
  },
  {
    name: "cp -r of a directory into the home directory",
    family: "cp",
    role: "probe",
    command: 'cp -r dir "$HOME"',
  },
  {
    name: "cp through a workspace link that points at the profile",
    family: "cp",
    role: "probe",
    command: "cp payload f",
  },
  {
    name: "cp --remove-destination, which unlinks a destination outside the roots",
    family: "cp",
    role: "probe",
    command: 'cp --remove-destination payload "$HOME/link"',
  },
  {
    name: "cp -b, which renames a destination outside the roots to its backup",
    family: "cp",
    role: "probe",
    command: 'cp -b payload "$HOME/link"',
  },
  {
    name: "cp whose destination the guard cannot read",
    family: "cp",
    role: "probe",
    command: `cp payload "${HIDDEN('"$HOME/.bashrc"')}"`,
  },
  { name: "cp inside the workspace", family: "cp", role: "must-allow", command: "cp payload copy" },
  {
    name: "cp -r into the pane's own /tmp directory",
    family: "cp",
    role: "must-allow",
    command: 'cp -r dir "$TMPDIR/mine/"',
  },
  {
    name: "cp -t into the workspace",
    family: "cp",
    role: "must-allow",
    command: 'cp -t "$LEGION_WORKSPACE/dir" payload',
  },
  {
    name: "cp of a source outside the roots into the workspace, which writes nothing outside",
    family: "cp",
    role: "must-allow",
    command: 'cp "$HOME/.bashrc" theirs',
  },

  // --- dd --------------------------------------------------------------------------------------
  {
    name: "dd of= the profile",
    family: "dd",
    role: "probe",
    command: 'dd if=payload of="$HOME/.bashrc"',
  },
  {
    name: "dd of= before if=, since an operand of dd stands anywhere",
    family: "dd",
    role: "probe",
    command: 'dd of="$HOME/.bashrc" if=payload',
  },
  {
    name: "dd of= with conv=notrunc, which still writes the file",
    family: "dd",
    role: "probe",
    command: 'dd if=payload of="$HOME/.bashrc" conv=notrunc',
  },
  {
    name: "dd of= a workspace link that points at the profile",
    family: "dd",
    role: "probe",
    command: "dd if=payload of=f",
  },
  {
    name: "dd whose of= operand the guard cannot read",
    family: "dd",
    role: "probe",
    command: `dd if=payload "${HIDDEN('"of=$HOME/.bashrc"')}"`,
  },
  // bash expands a tilde after the `=` of an assignment-like prefix, so `of=~/…` is the home
  // directory while `--directory=~/…` and `-d~/…` are the literal text (measured; `tildeAt`).
  {
    name: "dd of=~ , where the tilde stands after an operand's own `=`",
    family: "dd",
    role: "probe",
    command: "dd if=payload of=~/.ssh/id_rsa",
  },
  {
    name: "cp to a tilde path, the ordinary position",
    family: "cp",
    role: "probe",
    command: "cp payload ~/.bashrc",
  },
  {
    name: "dd of= inside the workspace",
    family: "dd",
    role: "must-allow",
    command: "dd if=payload of=copy",
  },
  {
    name: "dd of=/dev/null, a device that overwrites nothing",
    family: "dd",
    role: "must-allow",
    command: "dd if=payload of=/dev/null",
  },
  {
    name: "dd reading a file outside the roots, which writes nothing",
    family: "dd",
    role: "must-allow",
    command: 'dd if="$HOME/.bashrc" of=theirs',
  },

  // --- install ---------------------------------------------------------------------------------
  {
    name: "install over the profile",
    family: "install",
    role: "probe",
    command: 'install -m 644 payload "$HOME/.bashrc"',
  },
  {
    name: "install into the home directory",
    family: "install",
    role: "probe",
    command: 'install payload "$HOME"',
  },
  {
    name: "install -t, whose option carries the destination",
    family: "install",
    role: "probe",
    command: 'install -t "$HOME" payload',
  },
  {
    name: "install -m 755 with the mode apart from its option",
    family: "install",
    role: "probe",
    command: 'install -m 755 payload "$HOME/bin/tool"',
  },
  {
    name: "install descending a workspace link that points at a directory outside the roots",
    family: "install",
    role: "probe",
    command: "install payload e",
  },
  {
    name: "install replacing a link outside the roots rather than writing through it",
    family: "install",
    role: "probe",
    command: 'install payload "$HOME/link"',
  },
  {
    name: "install -d over an existing directory outside the roots, which changes its mode",
    family: "install",
    role: "probe",
    command: 'install -d -m 700 "$HOME/.ssh"',
  },
  {
    name: "install inside the workspace",
    family: "install",
    role: "must-allow",
    command: "install -m 755 payload copy",
  },
  {
    name: "install -D inside the workspace, which creates the parents it needs",
    family: "install",
    role: "must-allow",
    command: "install -D payload deep/a/tool",
  },
  {
    name: "install --strip before -t, where the full name --strip wins over --strip-program",
    family: "install",
    role: "probe",
    command: 'cp payload .bashrc; install --strip -t "$HOME" .bashrc',
  },
  {
    name: "install --strip inside the workspace, a flag that takes no value",
    family: "install",
    role: "must-allow",
    command: "install --strip payload copy",
  },

  // --- ln ----------------------------------------------------------------------------------------
  {
    name: "ln -sf over the profile",
    family: "ln",
    role: "probe",
    command: 'ln -sf payload "$HOME/.bashrc"',
  },
  {
    name: "ln -f, a hard link over the profile",
    family: "ln",
    role: "probe",
    command: 'ln -f payload "$HOME/.bashrc"',
  },
  {
    name: "ln -sf descending a workspace link that points at a directory outside the roots",
    family: "ln",
    role: "probe",
    command: "ln -sf payload e",
  },
  {
    name: "ln -sfn replacing a link outside the roots",
    family: "ln",
    role: "probe",
    command: 'ln -sfn payload "$HOME/link"',
  },
  {
    name: "ln -s -t, whose option carries the destination directory",
    family: "ln",
    role: "probe",
    command: 'ln -s -t "$HOME" payload',
  },
  {
    name: "ln -s with one operand, which writes the working directory, not the operand",
    family: "ln",
    role: "probe",
    command: 'cd "$HOME" && ln -s "$LEGION_WORKSPACE/payload"',
  },
  {
    name: "ln of the profile into the workspace, then a write through the hard link",
    family: "ln",
    role: "probe",
    command: 'ln "$HOME/.bashrc" laundered && echo pwned > laundered',
  },
  // The guard reads the whole command before this link exists. The separate-call directory
  // copy probes below cover links that already exist; this row needs a same-command link model.
  {
    name: "RESIDUAL a link this command makes, written through by the same command",
    family: "residual",
    role: "residual",
    command: 'ln -s "$HOME/.bashrc" launder-s && echo pwned > launder-s',
  },
  {
    name: "control: a write through a link that already exists, which the guard resolves",
    family: "ln",
    role: "probe",
    command: "echo pwned > f",
  },
  {
    name: "ln -sf inside the workspace",
    family: "ln",
    role: "must-allow",
    command: "ln -sf payload link-here",
  },
  {
    name: "ln -s with one operand in the workspace, which writes the working directory",
    family: "ln",
    role: "must-allow",
    command: 'ln -s "$HOME/.bashrc" link-out',
  },

  // --- sed -i ------------------------------------------------------------------------------------
  {
    name: "sed -i over the profile",
    family: "sed",
    role: "probe",
    command: `sed -i 's/CANARY/PWNED/' "$HOME/.bashrc"`,
  },
  {
    name: "sed -i.bak, whose suffix joins its option",
    family: "sed",
    role: "probe",
    command: `sed -i.bak 's/CANARY/PWNED/' "$HOME/.bashrc"`,
  },
  {
    name: "sed -ni, in-place inside a cluster",
    family: "sed",
    role: "probe",
    command: `sed -ni 's/CANARY/PWNED/p' "$HOME/.bashrc"`,
  },
  {
    name: "sed -ie, where the letter after -i is its suffix and not another option",
    family: "sed",
    role: "probe",
    command: `sed -ie 's/CANARY/PWNED/' "$HOME/.bashrc"`,
  },
  {
    name: "sed -e ... -i, with the option after the script",
    family: "sed",
    role: "probe",
    command: `sed -e 's/CANARY/PWNED/' -i "$HOME/.bashrc"`,
  },
  {
    name: "sed --in-place, the long spelling",
    family: "sed",
    role: "probe",
    command: `sed --in-place 's/CANARY/PWNED/' "$HOME/.bashrc"`,
  },
  {
    name: "sed -i --follow-symlinks through a workspace link at the profile",
    family: "sed",
    role: "probe",
    command: `sed -i --follow-symlinks 's/CANARY/PWNED/' f`,
  },
  {
    name: "sed -i whose in-place option the guard cannot read",
    family: "sed",
    role: "probe",
    command: `sed "${HIDDEN("-i")}" 's/CANARY/PWNED/' "$HOME/.bashrc"`,
  },
  {
    name: "sed -i of a workspace link, which replaces the link and not the profile",
    family: "sed",
    role: "must-allow",
    command: `sed -i 's/CANARY/PWNED/' f`,
  },
  {
    name: "sed without -i, which writes nothing",
    family: "sed",
    role: "must-allow",
    command: `sed 's/CANARY/PWNED/' "$HOME/.bashrc"`,
  },
  {
    name: "sed -i inside the workspace",
    family: "sed",
    role: "must-allow",
    command: `sed -i 's/NOTES/notes/' notes.txt`,
  },
  {
    name: "sed -i over a file the script names, inside the workspace",
    family: "sed",
    role: "must-allow",
    command: `sed -i -e 's|/etc/passwd|x|' notes.txt`,
  },
  {
    name: "sed -n over two words the guard cannot read, which no reading of them makes write",
    family: "sed",
    role: "must-allow",
    command: `sed -n "${HIDDEN("1p")}" "${HIDDEN("notes.txt")}"`,
  },
  {
    name: "sed without -i whose script the guard reads only in part, over a workspace file and the profile",
    family: "sed",
    role: "must-allow",
    command: `sed "s/${HIDDEN("CANARY")}/x/" notes.txt "$HOME/.bashrc"`,
  },
  {
    name: "sed without -i whose joined --expression value the guard cannot read, over a file outside",
    family: "sed",
    role: "must-allow",
    command: `sed --expr"${HIDDEN("=p")}" "$HOME/.bashrc"`,
  },
  {
    name: "sed with one word bash splits into -i, the script and a file outside the roots",
    family: "sed",
    role: "probe",
    command: `sed ${HIDDEN('"-i s/CANARY/PWNED/ $HOME/.bashrc"')}`,
  },
  {
    name: "sed -i with a long option whose joined value the guard cannot read, before the file",
    family: "sed",
    role: "probe",
    command: `sed -i --exp"${HIDDEN("=s/CANARY/PWNED/")}" "$HOME/.bashrc"`,
  },
  {
    name: "sed -i with a word the guard cannot read that may be --follow-symlinks",
    family: "sed",
    role: "probe",
    command: `sed -i "${HIDDEN("--follow-symlinks")}" 's/CANARY/PWNED/' f`,
  },
  {
    name: "sed -f in a word the guard cannot read takes -- as its script file, and -i follows",
    family: "sed",
    role: "probe",
    plant: `echo 's/CANARY/PWNED/' > ./--`,
    command: `sed "${HIDDEN("-f")}" -- "$HOME/.bashrc" -i`,
  },
  {
    name: "sed with a long option word the guard reads only as --, which may be --in-place",
    family: "sed",
    role: "probe",
    command: `sed --"${HIDDEN("in-place")}" 's/CANARY/PWNED/' "$HOME/.bashrc"`,
  },
  {
    name: "sed with an option cluster the guard reads only as -, which may be -i",
    family: "sed",
    role: "probe",
    command: `sed -"${HIDDEN("i")}" 's/CANARY/PWNED/' "$HOME/.bashrc"`,
  },
  {
    name: "sed -i -e with its script joined in a word the guard cannot read, before the file",
    family: "sed",
    role: "probe",
    command: `sed -i -e"${HIDDEN("s/CANARY/PWNED/")}" "$HOME/.bashrc"`,
  },

  // --- shred -------------------------------------------------------------------------------------
  // Judged before LEGION-357, but on the link alone: it overwrites what a link points at.
  {
    name: "shred through a workspace link that points at the profile",
    family: "shred",
    role: "probe",
    command: "shred -n 1 f",
  },

  // Round-two review pairs. Each control/probe differs by one token; both must protect HOME.
  ...[
    'cp -S.mp payload "$HOME/.bashrc"',
    'cp -S.tmp payload "$HOME/.bashrc"',
    'install -m644 payload "$HOME/.bashrc"',
    'install -oroot payload "$HOME/.bashrc"',
    'install -mo+t payload "$HOME/.bashrc"',
    'ln -sf payload "$HOME/.bashrc"',
    'ln -sfS.tmp payload "$HOME/.bashrc"',
    'cp payload .bashrc; cp -S.x -t "$HOME" .bashrc',
    'cp payload .bashrc; cp -S.t -t "$HOME" .bashrc',
    'cp payload .bashrc; install -S.x -t "$HOME" .bashrc',
    'cp payload .bashrc; install -S.d -t "$HOME" .bashrc',
    'cp payload .bashrc; cp --target-directory="$HOME" .bashrc',
    'cp payload .bashrc; cp --target="$HOME" .bashrc',
    'cp payload .bashrc; install --target="$HOME" .bashrc',
    `sed --in-place 's/CANARY/PWNED/' "$HOME/.bashrc"`,
    `sed --in-pl 's/CANARY/PWNED/' "$HOME/.bashrc"`,
    `sed -i --expression='s/CANARY/PWNED/' "$HOME/.bashrc"`,
    `sed -i --exp='s/CANARY/PWNED/' "$HOME/.bashrc"`,
    'install -d -m 700 "$HOME/.ssh"',
    'install --dir -m 700 "$HOME/.ssh"',
    'cp --remove-destination payload "$HOME/link"',
    'cp --remove-dest payload "$HOME/link"',
    "cp payload f",
    'ln "$HOME/.bashrc" laundered; echo pwned > laundered',
    'ln -- "$HOME/.bashrc" -s; echo pwned > ./-s',
  ].map(
    (command): WriteRow => ({
      name: `review pair: ${command}`,
      family: "review",
      role: "probe",
      command,
    })
  ),
  ...["cp payload dir/", "cp payload dir", "cp -t dir payload"].map(
    (command): WriteRow => ({
      name: `earlier link: ${command}`,
      family: "cp",
      role: "probe",
      plant: 'ln -s "$HOME/.bashrc" dir/payload',
      command,
    })
  ),
  {
    name: "earlier nested link: cp -r sub dir/",
    family: "cp",
    role: "probe",
    plant: 'mkdir -p sub dir/sub && echo SUB > sub/in && ln -s "$HOME/.bashrc" dir/sub/in',
    command: "cp -r sub dir/",
  },
  ...[
    'cp "$HOME/.bashrc" theirs',
    'cp -t "$LEGION_WORKSPACE/dir" "$HOME/.bashrc"',
    "cp -S.tmp -b payload copy",
    "install -m 755 payload copy",
    "cp payload dir/",
    'cp -r dir "$TMPDIR/mine/"',
    'ln -s "$HOME/.bashrc" link-out',
    `sed -i --expression='s/NOTES/n/' notes.txt`,
    `sed -n 1p "$HOME/.bashrc"`,
  ].map(
    (command): WriteRow => ({
      name: `review keep: ${command}`,
      family: "review",
      role: "must-allow",
      command,
    })
  ),
  ...[
    'cp -S -tX payload "$HOME/.bashrc"',
    'install -b -S -t payload "$HOME/.bashrc"',
    'touch -- -tX; cp -- -tX "$HOME/.bashrc"',
    'cp payload .bashrc; cp --t="$HOME" .bashrc',
    'cp payload .bashrc; mv -ft "$HOME" .bashrc',
    'cp payload .bashrc; mv -t"$HOME" .bashrc',
    'cp payload .bashrc; mv --target-dir="$HOME" .bashrc',
    'ln -b -S -s "$HOME/.bashrc" hl; echo pwned > hl',
    'touch -- -s; mkdir -p d; ln -- -s "$HOME/.bashrc" d; echo pwned > d/.bashrc',
    `sed --in-pl=bak 's/CANARY/PWNED/' "$HOME/.bashrc"`,
    `sed -i --fol 's/CANARY/PWNED/' f`,
    `echo 's/CANARY/PWNED/' > script; sed -i --fi=script "$HOME/.bashrc"`,
    `sed -i --exp 's/CANARY/PWNED/' "$HOME/.bashrc"`,
    `cp payload .bashrc; cp "${HIDDEN('"--target-directory=$HOME"')}" .bashrc`,
    `cp payload .bashrc; install "${HIDDEN('"--target-directory=$HOME"')}" .bashrc`,
    `cp payload .bashrc; ln -sf "${HIDDEN('"--target-directory=$HOME"')}" .bashrc`,
  ].map(
    (command): WriteRow => ({
      name: `reader boundary: ${command}`,
      family: "reader",
      role: "probe",
      command,
    })
  ),
  ...[
    'cp --target dir "$HOME/.bashrc"',
    'cp --target=dir "$HOME/.bashrc"',
    'ln --sym "$HOME/.bashrc" link-out',
    `sed --in-pl --exp='s/NOTES/n/' notes.txt`,
    `sed -i --line-l 80 's/NOTES/n/' notes.txt`,
    `sed -i --line-l=80 's/NOTES/n/' notes.txt`,
    'cp -S -t"$HOME" payload notes.txt',
    'cp -T "$(printf %s payload)" copy',
    'ln -sfT "$(printf %s payload)" link-here',
  ].map(
    (command): WriteRow => ({
      name: `reader keep: ${command}`,
      family: "reader",
      role: "must-allow",
      command,
    })
  ),
  ...['cp --suff -tX payload "$HOME/.bashrc"', 'install -d -m700 "$HOME/.ssh" localdir'].map(
    (command): WriteRow => ({
      name: `value consumption: ${command}`,
      family: "reader",
      role: "probe",
      command,
    })
  ),
  ...[
    "install -d localdir",
    'ln -s "$HOME/.bashrc"',
    `sed -i --line-l 80 "$(printf %s s/NOTES/notes/)" notes.txt`,
  ].map(
    (command): WriteRow => ({
      name: `value keep: ${command}`,
      family: "reader",
      role: "must-allow",
      command,
    })
  ),
  {
    name: "copy does not treat its destination as another source",
    family: "cp",
    role: "must-allow",
    plant: 'ln -s "$HOME/.bashrc" dir/dir',
    command: "cp payload dir",
  },
  {
    name: "copy cannot resolve an unreadable source basename after --",
    family: "cp",
    role: "probe",
    plant: 'ln -s "$HOME/.bashrc" dir/payload',
    command: `cp -- "$(printf %s payload)" dir`,
  },
  {
    name: "copy resolves deep destination descendants",
    family: "cp",
    role: "probe",
    plant:
      'mkdir -p sub/deep dir/sub/deep; echo SUB > sub/deep/in; ln -s "$HOME/.bashrc" dir/sub/deep/in',
    command: "cp -r sub dir",
  },
  ...["cp payload", "install payload", "ln"].map(
    (command): WriteRow => ({
      name: `missing destination: ${command}`,
      family: "reader",
      role: "unreadable",
      command,
    })
  ),
  {
    name: "cp unknown replacement option with explicit last destination",
    family: "cp",
    role: "probe",
    command: `cp -T "$(printf %s --remove-destination)" payload "$HOME/link"`,
  },
  {
    name: "cp abbreviated backup replaces the destination link",
    family: "cp",
    role: "probe",
    command: 'cp --back payload "$HOME/link"',
  },
  ...["cp src/.bashrc trap/", "cp -r src/. trap/", "cp -rT src trap"].map(
    (command): WriteRow => ({
      name: `copy content root: ${command}`,
      family: "cp",
      role: "probe",
      plant: 'mkdir -p src trap; echo NEW > src/.bashrc; ln -s "$HOME/.bashrc" trap/.bashrc',
      command,
    })
  ),
  {
    name: "copy parents retains the written source path",
    family: "cp",
    role: "probe",
    plant: 'mkdir -p src trap/src; echo NEW > src/.bashrc; ln -s "$HOME/.bashrc" trap/src/.bashrc',
    command: "cp --parents src/.bashrc trap/",
  },
  ...['cp -T -r"$(printf %s)" src trap', 'cp -T "$(printf %s -r)" src trap'].map(
    (command): WriteRow => ({
      name: `a word the guard cannot read may itself make the copy recursive: ${command}`,
      family: "cp",
      role: "probe",
      plant:
        'mkdir -p src/sub trap/sub; echo NEW > src/sub/.bashrc; ln -s "$HOME/.bashrc" trap/sub/.bashrc',
      command,
    })
  ),
  {
    name: "unreadable basename cannot select an escaping destination entry",
    family: "cp",
    role: "probe",
    plant: 'ln -s "$HOME/.bashrc" dir/notes.txt',
    command: 'for f in *.txt; do cp -- "$f" dir/; done',
  },
  ...[
    'for f in *.txt; do cp -- "$f" dir/; done',
    'for f in ./*.txt; do cp "$f" copy-of-notes; done',
  ].map(
    (command): WriteRow => ({
      name: `unreadable basename keep: ${command}`,
      family: "cp",
      role: "must-allow",
      command,
    })
  ),
  ...["cp -r src/. fresh/", "cp --parents src/a fresh/"].map(
    (command): WriteRow => ({
      name: `fresh copy keep: ${command}`,
      family: "cp",
      role: "must-allow",
      plant: "mkdir -p src fresh; echo A > src/a",
      command,
    })
  ),
  {
    name: "wildcard-led loop can still supply a target-directory option",
    family: "cp",
    role: "probe",
    plant:
      'touch -- -tsub.txt; mkdir sub.txt; cp payload copy-of-notes; ln -s "$HOME/.bashrc" sub.txt/copy-of-notes',
    command: 'for f in *.txt; do cp "$f" copy-of-notes; done',
  },
  {
    name: "recursive copy does not overwrite an unrelated destination entry",
    family: "cp",
    role: "must-allow",
    plant: 'mkdir -p src trap/src; echo A > src/a; ln -s "$HOME/.bashrc" trap/src/unrelated',
    command: "cp -r src trap",
  },
  {
    name: "unreadable target option names its word and the operand remedies",
    family: "cp",
    role: "probe",
    plant: "cp payload .bashrc",
    command: `cp "${HIDDEN('"--target-directory=$HOME"')}" .bashrc`,
    unreadableWord: `"${HIDDEN('"--target-directory=$HOME"')}"`,
  },
  {
    name: "unreadable recursive basename tolerates a safe destination cycle",
    family: "cp",
    role: "must-allow",
    plant: "ln -s . dir/cycle",
    command: 'cp -r -- "$(printf %s notes.txt)" dir',
  },
  ...[
    'cp payload "$HOME/.bashrc" --suffix never',
    'cp payload "$HOME/.bashrc" --sparse never',
    'cp payload "$HOME/.bashrc" --sp never',
    'cp payload "$HOME/.bashrc" --no-preserve mode',
    'cp -S --help payload "$HOME/.bashrc"',
  ].map(
    (command): WriteRow => ({
      name: `trailing valued option: ${command}`,
      family: "cp",
      role: "probe",
      command,
    })
  ),
  ...[
    "cp --version",
    "mv --version",
    "install --help",
    "ln --help",
    'mv --version "$HOME/.bashrc" dir/',
    "cp -- *.txt dir/",
    "mv -- *.txt dir/",
    'cp -- "$(ls -t | head -1)" latest.txt',
  ].map(
    (command): WriteRow => ({
      name: `read-only or explicit operand keep: ${command}`,
      family: "reader",
      role: "must-allow",
      command,
    })
  ),
  {
    name: "fresh recursive copy does not traverse a source symlink cycle",
    family: "cp",
    role: "must-allow",
    plant: "mkdir src; ln -s . src/cycle",
    command: "cp -r src dir",
  },
  ...['dd of="$HOME/.bashrc" --version', 'sed -i --help "s/CANARY/PWNED/" "$HOME/.bashrc"'].map(
    (command): WriteRow => ({
      name: `GNU informational option keep: ${command}`,
      family: "reader",
      role: "must-allow",
      command,
    })
  ),
  {
    name: "sed script-file value is not an informational option",
    family: "sed",
    role: "probe",
    plant: `echo 's/CANARY/PWNED/' > ./--help`,
    command: 'sed -i -f --help "$HOME/.bashrc"',
  },
  // A quoted `@` expansion whose element count the guard does not know is not one argument: any
  // element may be the `-i` that makes sed write, or the file it writes. Every other verb already
  // refused such a word because its value is unknown; sed read it as a single part.
  {
    name: "sed -i over an array read from a file, quoted",
    family: "sed",
    role: "probe",
    plant: `printf '%s\\n' s/CANARY/PWNED/ "$HOME/.bashrc" > list`,
    command: `mapfile -t args < list; sed -i "\${args[@]}"`,
  },
  {
    name: "sed -i over an array read -a splits, quoted",
    family: "sed",
    role: "probe",
    plant: `printf '%s\\n' "s/CANARY/PWNED/ $HOME/.bashrc" > words`,
    command: `read -ra args < words; sed -i "\${args[@]}"`,
  },
  {
    name: "sed -i over an array built from a command's output, quoted",
    family: "sed",
    role: "probe",
    plant: `printf '%s\\n' s/CANARY/PWNED/ "$HOME/.bashrc" > list`,
    command: `args=($(cat list)); sed -i "\${args[@]}"`,
  },
  {
    name: "sed -i over a function's own arguments, quoted",
    family: "sed",
    role: "probe",
    plant: `printf '%s\\n' s/CANARY/PWNED/ "$HOME/.bashrc" > list`,
    command: `f() { sed -i "$@"; }; f $(cat list)`,
  },
  {
    name: "sed -n over an array that may carry -i, quoted",
    family: "sed",
    role: "probe",
    plant: `printf '%s\\n' -i s/CANARY/PWNED/ "$HOME/.bashrc" > list`,
    command: `mapfile -t args < list; sed -n "\${args[@]}"`,
  },
  {
    name: "sed over an array that may carry -i, before a workspace file",
    family: "sed",
    role: "probe",
    plant: `printf '%s\\n' -i s/CANARY/PWNED/ "$HOME/.bashrc" > list`,
    command: `mapfile -t args < list; sed "\${args[@]}" notes.txt`,
  },
  // An array or the positional parameters copied from an unknown-length list keep that unknown
  // length, so a copy is no way around the tag.
  {
    name: "sed -i over a copy of an unknown array",
    family: "sed",
    role: "probe",
    plant: `printf '%s\\n' s/CANARY/PWNED/ "$HOME/.bashrc" > list`,
    command: `mapfile -t a < list; b=("\${a[@]}"); sed -i "\${b[@]}"`,
  },
  {
    name: "sed -i over an unknown array appended to another",
    family: "sed",
    role: "probe",
    plant: `printf '%s\\n' s/CANARY/PWNED/ "$HOME/.bashrc" > list`,
    command: `mapfile -t a < list; b+=("\${a[@]}"); sed -i "\${b[@]}"`,
  },
  {
    name: "sed -i over positional parameters set from an unknown array",
    family: "sed",
    role: "probe",
    plant: `printf '%s\\n' s/CANARY/PWNED/ "$HOME/.bashrc" > list`,
    command: `mapfile -t a < list; set -- "\${a[@]}"; sed -i "$@"`,
  },
  {
    name: "sed -i over arguments passed through a second function",
    family: "sed",
    role: "probe",
    plant: `printf '%s\\n' s/CANARY/PWNED/ "$HOME/.bashrc" > list`,
    command: `f() { g "$@"; }; g() { sed -i "$@"; }; f $(cat list)`,
  },
  // A scalar is one argument whatever it was assigned from, and a quoted `*` joins every element
  // into one, so neither carries the tag. These were allowed before the tag existed and stay so.
  {
    name: "sed over a known array with no -i",
    family: "sed",
    role: "must-allow",
    command: `args=(-n 1p); sed "\${args[@]}" notes.txt`,
  },
  {
    name: "sed -i over a known array, inside the workspace",
    family: "sed",
    role: "must-allow",
    command: `args=(-i s/NOTES/n/); sed "\${args[@]}" notes.txt`,
  },
  {
    name: "sed -n with the unknown array after --, where it can only be files",
    family: "sed",
    role: "must-allow",
    plant: `printf '%s\\n' -i s/CANARY/PWNED/ "$HOME/.bashrc" > list`,
    command: `mapfile -t args < list; sed -n 1p -- "\${args[@]}"`,
  },
  {
    name: 'sed -n over a scalar assigned from "$*"',
    family: "sed",
    role: "must-allow",
    plant: `printf '%s\\n' 1p > list`,
    command: `f() { s="$*"; sed -n "$s" notes.txt; }; f $(cat list)`,
  },
  {
    name: 'sed -n over a scalar assigned from "$@"',
    family: "sed",
    role: "must-allow",
    plant: `printf '%s\\n' 1p > list`,
    command: `f() { s="$@"; sed -n "$s" notes.txt; }; f $(cat list)`,
  },
  {
    name: "sed -n over a scalar assigned from an unknown array joined",
    family: "sed",
    role: "must-allow",
    plant: `printf '%s\\n' 1p > list`,
    command: `mapfile -t a < list; s="\${a[*]}"; sed -n "$s" notes.txt`,
  },
  {
    name: "sed -n over a scalar assigned from an unknown array",
    family: "sed",
    role: "must-allow",
    plant: `printf '%s\\n' 1p > list`,
    command: `mapfile -t a < list; s="\${a[@]}"; sed -n "$s" notes.txt`,
  },
  {
    name: "sed -n over an unknown array joined into one argument",
    family: "sed",
    role: "must-allow",
    plant: `printf '%s\\n' 1p > list`,
    command: `mapfile -t args < list; sed -n "\${args[*]}" notes.txt`,
  },
  {
    name: "sed -n over unknown positional parameters joined into one argument",
    family: "sed",
    role: "must-allow",
    plant: `printf '%s\\n' 1p > list`,
    command: `f() { sed -n "$*" notes.txt; }; f $(cat list)`,
  },
  // A name the guard never saw assigned is reachable in one command: an `eval` of text it cannot
  // read leaves every name unknown and possibly unset, which is the same state a name an earlier
  // tool call assigned is in. `[@]` over it may be any number of arguments; `[*]` is one.
  {
    name: "sed -i over an array an unreadable eval may have assigned",
    family: "sed",
    role: "probe",
    plant: `printf '%s\\n' 'args=(s/CANARY/PWNED/ "$HOME/.bashrc")' > setter`,
    command: `eval "$(cat setter)"; sed -i "\${args[@]}"`,
  },
  {
    name: "sed -n over an array set nowhere, joined into one argument",
    family: "sed",
    role: "must-allow",
    command: `sed -n "\${args[*]}" notes.txt`,
  },
  // `ln` given one operand links into the working directory — but only when that word is
  // certainly ONE operand. A word bash may make several arguments of carries a link name too, and
  // `ln -sf` replaces an existing file there (the queue's oracle, #1565).
  {
    name: "ln -sf over an unknown-length array after --, which carries its own link name",
    family: "ln",
    role: "probe",
    plant: `printf '%s\\n' notes.txt "$HOME/.bashrc" > list`,
    command: `mapfile -t a < list; ln -sf -- "\${a[@]}"`,
  },
  {
    name: "ln -sf over an unquoted unknown scalar after --",
    family: "ln",
    role: "probe",
    plant: `printf '%s\\n' 'notes.txt '"$HOME/.bashrc" > list`,
    command: `s=$(cat list); ln -sf -- $s`,
  },
  // Bash removes these words before it chooses a command or positional argument. Each dangerous
  // row is paired with a harmless command that exercises the same position, and the final four
  // prove that quoted empty strings remain arguments rather than disappearing with no-field words.
  {
    name: "an unquoted empty scalar before rm passes no command argument",
    family: "no-argument word",
    role: "probe",
    command: `x=; $x rm -rf "$HOME/.ssh"`,
  },
  {
    name: "an unquoted empty scalar after sudo passes no wrapper argument",
    family: "no-argument word",
    role: "probe",
    command: `x=; sudo $x rm -rf "$HOME/.ssh"`,
  },
  {
    name: "an unquoted empty scalar after env passes no wrapper argument",
    family: "no-argument word",
    role: "probe",
    command: `x=; env $x rm -rf "$HOME/.ssh"`,
  },
  {
    name: "an unquoted empty scalar after nice passes no wrapper argument",
    family: "no-argument word",
    role: "probe",
    command: `x=; nice $x rm -rf "$HOME/.ssh"`,
  },
  {
    name: "an unquoted empty scalar after command passes no wrapper argument",
    family: "no-argument word",
    role: "probe",
    command: `x=; command $x rm -rf "$HOME/.ssh"`,
  },
  {
    name: "an unquoted empty scalar before an xargs program passes no argument",
    family: "no-argument word",
    role: "probe",
    command: `x=; printf '%s\\n' "$HOME/.ssh" | xargs $x rm -rf`,
  },
  {
    name: "an unquoted empty scalar before a find-exec program passes no argument",
    family: "no-argument word",
    role: "probe",
    command: `x=; find "$HOME" -maxdepth 1 -name .ssh -exec $x rm -rf {} +`,
  },
  {
    name: "an unquoted empty scalar leaves cd without an argument",
    family: "no-argument word",
    role: "probe",
    command: `x=; cd $x; rm -rf .ssh`,
  },
  {
    name: "an empty array leaves cd without an argument",
    family: "no-argument word",
    role: "probe",
    command: `e=(); cd "\${e[@]}"; rm -rf .ssh`,
  },
  {
    name: "known-empty positional parameters before rm pass no command argument",
    family: "no-argument word",
    role: "probe",
    command: `f() { "$@" rm -rf "$HOME/.ssh"; }; f`,
  },
  {
    name: "an unquoted empty scalar before a harmless command stays allowed",
    family: "no-argument word",
    role: "must-allow",
    command: `x=; $x true`,
  },
  {
    name: "an unquoted empty scalar after sudo before a harmless command stays allowed",
    family: "no-argument word",
    role: "must-allow",
    command: `x=; sudo $x true`,
  },
  {
    name: "an unquoted empty scalar after env before a harmless command stays allowed",
    family: "no-argument word",
    role: "must-allow",
    command: `x=; env $x true`,
  },
  {
    name: "an unquoted empty scalar after nice before a harmless command stays allowed",
    family: "no-argument word",
    role: "must-allow",
    command: `x=; nice $x true`,
  },
  {
    name: "an unquoted empty scalar after command before a harmless command stays allowed",
    family: "no-argument word",
    role: "must-allow",
    command: `x=; command $x true`,
  },
  {
    name: "an unquoted empty scalar before a harmless xargs program stays allowed",
    family: "no-argument word",
    role: "must-allow",
    command: `printf '%s\\n' harmless | xargs $x true`,
  },
  {
    name: "an unquoted empty scalar before a harmless find-exec program stays allowed",
    family: "no-argument word",
    role: "must-allow",
    command: `x=; find "$LEGION_WORKSPACE" -maxdepth 1 -name notes.txt -exec $x true {} +`,
  },
  {
    name: "an unquoted empty scalar leaves harmless cd without an argument",
    family: "no-argument word",
    role: "must-allow",
    command: `x=; cd $x; true`,
  },
  {
    name: "an empty array leaves harmless cd without an argument",
    family: "no-argument word",
    role: "must-allow",
    command: `e=(); cd "\${e[@]}"; true`,
  },
  {
    name: "known-empty positional parameters before a harmless command stay allowed",
    family: "no-argument word",
    role: "must-allow",
    command: `f() { "$@" true; }; f`,
  },
  {
    name: "a literal quoted empty string stays a command argument",
    family: "no-argument word",
    role: "must-allow",
    command: `"" rm -rf "$HOME/.ssh"`,
  },
  {
    name: "a quoted empty scalar stays a command argument",
    family: "no-argument word",
    role: "must-allow",
    command: `x=; "$x" rm -rf "$HOME/.ssh"`,
  },
  {
    name: "known-empty positional parameters joined by star stay a command argument",
    family: "no-argument word",
    role: "must-allow",
    command: `f() { "$*" rm -rf "$HOME/.ssh"; }; f`,
  },
  {
    name: "a quoted empty array joined by star stays a command argument",
    family: "no-argument word",
    role: "must-allow",
    command: `e=(); "\${e[*]}" rm -rf "$HOME/.ssh"`,
  },
  // A word of no fields produces NO argument, so it is not the last operand. Counting it made the
  // real destination a source, and the copy landed outside the roots (#1565, the deep lens).
  {
    name: "cp whose trailing word is an unquoted empty value, passing no argument",
    family: "cp",
    role: "probe",
    command: `x=; cp payload "$HOME/.bashrc" $x`,
  },
  {
    name: "cp whose trailing word is an empty array, passing no argument",
    family: "cp",
    role: "probe",
    command: `e=(); cp payload "$HOME/.bashrc" "\${e[@]}"`,
  },
  {
    name: "install whose trailing word passes no argument",
    family: "install",
    role: "probe",
    command: `x=; install payload "$HOME/.bashrc" $x`,
  },
  {
    name: "ln -sf whose trailing word passes no argument",
    family: "ln",
    role: "probe",
    command: `x=; ln -sf payload "$HOME/.bashrc" $x`,
  },
  {
    name: "cp after -- whose trailing word passes no argument",
    family: "cp",
    role: "probe",
    command: `x=; cp -- payload "$HOME/.bashrc" $x`,
  },
  {
    name: "rsync whose trailing word passes no argument",
    family: "rsync",
    role: "probe",
    command: `x=; rsync payload "$HOME/.bashrc" $x`,
  },
  {
    name: "rsync into a workspace destination with a trailing no-argument word",
    family: "rsync",
    role: "must-allow",
    plant: `mkdir -p from to && printf 'a\\n' > from/f.txt`,
    command: `x=; rsync -a from/ to/ $x`,
  },
  {
    name: "cp into a workspace destination with a trailing no-argument word",
    family: "cp",
    role: "must-allow",
    command: `x=; cp payload dest $x`,
  },
  {
    name: "ln -sf over a positional slice, which is one operand the guard cannot read",
    family: "ln",
    role: "probe",
    command: `f() { ln -sf -- "\${@:1}"; }; f notes.txt "$HOME/.bashrc"`,
  },
  {
    name: "ln -sf over one readable source still links into the working directory",
    family: "ln",
    role: "must-allow",
    plant: `mkdir -p sub && printf 'a\\n' > sub/file.txt`,
    command: `ln -sf -- sub/file.txt`,
  },
  ...[
    'for f in *.txt; do truncate -s 0 "$f"; done',
    'for f in *.txt; do : > "$f"; done',
    'for f in ""*.txt; do truncate -s 0 "$f"; done',
  ].map(
    (command): WriteRow => ({
      name: `non-copy wildcard loop: ${command}`,
      family: "loop",
      role: "probe",
      plant: 'ln -s "$HOME/.bashrc" victim.txt',
      command,
    })
  ),
  {
    name: "non-copy dash-led glob loop remains unreadable",
    family: "loop",
    role: "probe",
    plant: 'ln -s "$HOME/.bashrc" ./-victim.txt',
    command: 'for f in -*.txt; do truncate -s 0 -- "$f"; done',
  },
  {
    name: "non-copy literal-prefix loop keeps workspace writes",
    family: "loop",
    role: "must-allow",
    command: 'for f in ./*.txt; do truncate -s 0 "$f"; done',
  },

  // --- The documented boundary, which these verbs do not change -----------------------------
  // A path that does not exist yet overwrites nothing: the rule `>` and `tee` are judged by
  // (`judgePath`'s `overwrite`). These rows create a file outside the roots and are allowed, as
  // the redirection beside them is. Closing them is a change to the boundary in
  // `docs/deployment.md`, not to these verbs, so each is listed by name in the test.
  {
    name: "RESIDUAL a redirection creating a file outside the roots",
    family: "residual",
    role: "residual",
    command: 'echo planted > "$HOME/planted"',
  },
  {
    name: "RESIDUAL cp creating a file outside the roots",
    family: "residual",
    role: "residual",
    command: 'cp payload "$HOME/planted"',
  },
  {
    name: "RESIDUAL ln -sf creating a link outside the roots",
    family: "residual",
    role: "residual",
    command: 'ln -sf payload "$HOME/planted"',
  },
  {
    name: "RESIDUAL install creating a file outside the roots",
    family: "residual",
    role: "residual",
    command: 'install payload "$HOME/planted"',
  },
];

export interface WriteResult {
  readonly row: WriteRow;
  /** What `guard.bash` returned: the refusal text, or undefined for a command it allowed. */
  readonly refusal: string | undefined;
  /** A thrown guard error is neither ALLOW nor REFUSE, and is a cost on a must-allow row. */
  readonly error: string | undefined;
  /** Whether real bash changed anything under the canary home. */
  readonly live: boolean;
  readonly exit: number | null;
}

/** The unreadable-option refusal names the operand, not just the outer command snippet. */
export function writeRefusalMatches(result: WriteResult): boolean {
  const word = result.row.unreadableWord;
  return (
    word === undefined ||
    (result.refusal?.includes(`cannot read a write destination from \`${word}\``) &&
      !result.refusal.includes("outside the issue workspace") &&
      result.refusal.includes("`--`") &&
      result.refusal.includes("`-T`")) === true
  );
}

/** The one thing this battery needs of a guard build, so any revision's file satisfies it and
 * `scripts/measure-pane-guard-writes.ts` can measure a build that is not the one it imports. */
export type GuardFactory = (options: {
  workspace: string | undefined;
  ompPid: number;
  scratch?: string;
}) => { bash: (command: string, cwd: string, env: NodeJS.ProcessEnv) => string | undefined };

/** Measures one row twice, in a fixture of its own: what the guard returns for the command, and
 * whether real bash changed anything under the canary home while running it. */
export function measureWriteRow(
  base: string,
  row: WriteRow,
  createGuard: GuardFactory
): WriteResult {
  const fixture = buildWriteFixture(mkdtempSync(path.join(base, "row-")));
  const env: NodeJS.ProcessEnv = {
    HOME: fixture.home,
    LEGION_WORKSPACE: fixture.workspace,
    TMPDIR: fixture.scratch,
    PATH: process.env.PATH,
  };
  const guard = createGuard({
    workspace: fixture.workspace,
    ompPid: process.pid,
    scratch: fixture.scratch,
  });
  if (row.plant !== undefined) {
    const beforePlant = canaryDigest(fixture.home);
    const denied = guard.bash(row.plant, fixture.workspace, env);
    if (denied !== undefined) throw new Error(`fixture plant refused: ${denied}`);
    const planted = spawnSync("bash", ["-c", row.plant], {
      cwd: fixture.workspace,
      env,
      timeout: 20_000,
    });
    if (planted.status !== 0 || canaryDigest(fixture.home) !== beforePlant) {
      throw new Error(`fixture plant failed or changed HOME: ${row.plant}`);
    }
  }
  let refusal: string | undefined;
  let error: string | undefined;
  try {
    refusal = guard.bash(row.command, fixture.workspace, env);
  } catch (caught) {
    error = caught instanceof Error ? caught.message : String(caught);
  }
  const before = canaryDigest(fixture.home);
  const ran = spawnSync("bash", ["-c", row.command], {
    cwd: fixture.workspace,
    env,
    timeout: 20_000,
  });
  return { row, refusal, error, live: canaryDigest(fixture.home) !== before, exit: ran.status };
}

/** The fixture's own properties, as booleans rather than as prose: a row means nothing if the
 * links do not point where the file says they do. */
export function fixtureProperties(base: string): Record<string, boolean> {
  const fixture = buildWriteFixture(mkdtempSync(path.join(base, "props-")));
  const digest = canaryDigest(fixture.home);
  const entries = Number(digest.split(":")[0]);
  const properties = {
    e_is_a_link_at_a_directory_outside_the_roots:
      lstatSync(path.join(fixture.workspace, "e")).isSymbolicLink() &&
      readlinkSync(path.join(fixture.workspace, "e")) === path.join(fixture.home, ".ssh"),
    f_is_a_link_at_the_profile:
      lstatSync(path.join(fixture.workspace, "f")).isSymbolicLink() &&
      readlinkSync(path.join(fixture.workspace, "f")) === path.join(fixture.home, ".bashrc"),
    home_link_points_into_the_workspace:
      readlinkSync(path.join(fixture.home, "link")) === path.join(fixture.workspace, "notes.txt"),
    home_outside_workspace: !fixture.home.startsWith(`${fixture.workspace}/`),
    home_outside_scratch: !fixture.home.startsWith(`${fixture.scratch}/`),
    scratch_is_not_real_tmp: fixture.scratch !== "/tmp",
    digest_counts_entries: entries > 5,
  };
  rmSync(fixture.base, { recursive: true, force: true });
  return properties;
}
