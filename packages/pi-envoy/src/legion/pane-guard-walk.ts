/**
 * Walks every refusal the pane guard gives this repository's tracked shell scripts, one at a time.
 *
 * It reports; it never writes the expected set. A tool that rewrote `EXPECTED_SCRIPT_REFUSALS` in
 * `pane-guard.test.ts` would make "fix the set instead of the guard" one command, so this prints
 * each refusal with its line and reason and leaves the choice (fix the guard, fix the script, or
 * record the refusal in the set) to a person.
 *
 * The sweep in `pane-guard.test.ts` sees only a script's first refusal. The walk sees all of them:
 * it copies the checkout's tracked files into a scratch tree, asks the guard about
 * `bash <script>` the way the sweep does, replaces the one command the refusal names with `:` in
 * the copy, and asks again, until the guard allows the script or the refusal names no command it
 * can replace. Nothing is ever run. The last line counts the scripts refused and every refusal the
 * walk met, so a change's effect on the tracked scripts is one number to compare.
 *
 *   bun packages/pi-envoy/src/legion/pane-guard-walk.ts [script ...]
 */
import {
  cpSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import { sweepGuard, trackedFiles } from "./pane-guard-scripts";

interface Named {
  readonly file: string;
  readonly line: number;
  readonly command: string;
}

/** The innermost `line N of FILE, \`COMMAND\`` a refusal names, which is the command it refused.
 * A refusal inside an EXIT trap names the trap's command after `in the EXIT trap`. */
function refused(reason: string, root: string): Named | undefined {
  const located = /line (\d+) of (\S+?), `((?:[^`])*?)`: /g;
  let last: RegExpExecArray | null = null;
  for (let match = located.exec(reason); match !== null; match = located.exec(reason)) last = match;
  if (last === null) return undefined;
  const after = reason.slice(last.index + last[0].length);
  const trap = /^in the EXIT trap, `((?:[^`])*?)`: /.exec(after);
  const file = path.isAbsolute(last[2] as string)
    ? (last[2] as string)
    : path.join(root, last[2] as string);
  return { file, line: Number(last[1]), command: trap?.[1] ?? (last[3] as string) };
}

/** Replaces the refused command's text with `:`, keeping a here-document's opener so its body
 * stays a body. The command starts on the refusal's line; a trap's command sits in the function
 * the trap names, so it is found from the top of the file when that line does not hold it. A
 * refusal in a file the script writes at run time, outside the copy, cannot be neutralized. */
function neutralize(named: Named, copy: string): string | undefined {
  if (!named.file.startsWith(`${copy}/`) || !existsSync(named.file)) {
    return `the refusal is in ${named.file}, a file the script writes when it runs`;
  }
  const text = readFileSync(named.file, "utf8");
  const lines = text.split("\n");
  const lineStart = lines.slice(0, named.line - 1).join("\n").length + (named.line > 1 ? 1 : 0);
  const onLine = (lines[named.line - 1] ?? "").indexOf(named.command);
  const at = onLine === -1 ? text.indexOf(named.command) : lineStart + onLine;
  if (at === -1) return `\`${named.command.split("\n")[0]}\` is not in ${named.file}`;
  const heredoc = named.command.includes("\n") ? null : /<<-?\s*(['"]?)(\w+)\1/.exec(named.command);
  const standIn = heredoc === null ? ":" : `: ${heredoc[0]}`;
  writeFileSync(named.file, text.slice(0, at) + standIn + text.slice(at + named.command.length));
  return undefined;
}

const LIMIT = 40;

function main(): void {
  const checkout = path.resolve(import.meta.dir, "../../../..");
  const scripts = trackedFiles(checkout, "**/*.sh");
  const wanted = process.argv.slice(2);
  const copy = mkdtempSync(path.join(os.tmpdir(), "pane-guard-walk-"));
  // The guard reads the files a script sources or runs, so the copy holds every tracked file;
  // a walk changes only files it neutralizes a line in, and those are put back after each script.
  const all = trackedFiles(checkout, "**");
  for (const file of all) {
    mkdirSync(path.dirname(path.join(copy, file)), { recursive: true });
    cpSync(path.join(checkout, file), path.join(copy, file));
  }
  const ask = sweepGuard(copy);
  let refusedScripts = 0;
  let refusals = 0;
  try {
    for (const script of wanted.length > 0 ? wanted : scripts) {
      const touched = new Set<string>();
      const walked: string[] = [];
      let end = `the walk's limit of ${LIMIT}`;
      for (let step = 0; step < LIMIT; step += 1) {
        const reason = ask(script);
        if (reason === undefined) {
          end = "then allowed";
          break;
        }
        walked.push(reason.replaceAll(`${copy}/`, "").replace(/^refused `bash [^`]*`: /, ""));
        const named = refused(reason, copy);
        const why = named === undefined ? "the refusal names no command" : neutralize(named, copy);
        if (named !== undefined && why === undefined) touched.add(named.file);
        if (why !== undefined) {
          end = `stopped: ${why}`;
          break;
        }
      }
      for (const file of touched) cpSync(path.join(checkout, path.relative(copy, file)), file);
      if (walked.length === 0) continue;
      refusedScripts += 1;
      refusals += walked.length;
      console.log(`## ${script}: ${walked.length} refusal(s), ${end}`);
      // A multi-line command shows as its first line, so each refusal reads on one line.
      for (const reason of walked) console.log(`- ${reason.replace(/\n[^`]*`/g, " …`")}`);
    }
  } finally {
    rmSync(copy, { recursive: true, force: true });
  }
  console.log(
    `${refusedScripts} of ${wanted.length > 0 ? wanted.length : scripts.length} scripts refused, ${refusals} refusals in all`
  );
}

if (import.meta.main) main();
