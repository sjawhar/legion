/** The shapes LEGION-354's rule is measured on. They live here rather than in the test file so
 * that `scripts/measure-pane-guard-model.ts` runs the same rows against any guard build, and the
 * numbers a pull request or a review states are derived from what the test actually runs. */

// A file a command writes is modelled in one map the whole walk shares, and `runFile` prefers
// that model to what is on disk. So a write the shell may never perform, or may not have
// finished, may not record what the file holds: the model would carry a branch's text into the
// branch the shell took, and the guard would read a script that is not there. Each row below
// writes benign text (`echo hi`) into `unread.sh`, whose text on disk deletes the canary HOME,
// and then runs it. Real bash decides the row: a row bash leaves destroyed is one the guard must
// refuse, and a row bash leaves intact is one it must still allow.

/** The modelled write, benign. */
const WRITE = "echo 'echo hi' > unread.sh";
/** The same write as one double-quoted operand, so nesting it in `trap` adds no quoting. */
const QUOTED_WRITE = "\"echo 'echo hi' > unread.sh\"";
/** Runs what the guard modelled. */
const RUN = "bash unread.sh";
/** False under real bash: `safe.sh` holds PRESENT and never holds zzzNOPE. */
const FALSE_COND = "grep -q zzzNOPE safe.sh";
/** True under real bash. */
const TRUE_COND = "grep -q PRESENT safe.sh";
export const DESTRUCTIVE = 'rm -rf "$HOME"\n';

/** What the row does when real bash runs it: `destroyed` and `intact` are checked, and `racy` is
 * a write bash performs beside the read, whose outcome one run cannot settle — those rows are
 * held to bash having performed the write. */
export type Live = "destroyed" | "intact" | "racy";

interface ModelRow {
  readonly name: string;
  readonly payload: string;
  readonly guard: "refused" | "allowed";
  readonly live: Live;
  /** Contents of `gen.sh` in the row's fixture, for a row whose handler a script sets. */
  readonly script?: string;
}

export const MODEL_ROWS: readonly ModelRow[] = [
  // Straight-line writes, both directions: the model is the only thing that can allow a row, and
  // the guard still reads what a command really writes.
  {
    name: "straight-line benign write",
    payload: `${WRITE}; ${RUN}`,
    guard: "allowed",
    live: "intact",
  },
  {
    name: "straight-line destructive write",
    payload: `echo 'rm -rf "$HOME"' > unread.sh; ${RUN}`,
    guard: "refused",
    live: "destroyed",
  },
  { name: "no modelled write at all", payload: RUN, guard: "refused", live: "destroyed" },

  // Conditional and loop bodies: the shell may never enter them.
  {
    name: "&& rhs",
    payload: `${FALSE_COND} && ${WRITE}; ${RUN}`,
    guard: "refused",
    live: "destroyed",
  },
  {
    name: "&& rhs after a test",
    payload: `[ -f nosuch.conf ] && ${WRITE}; ${RUN}`,
    guard: "refused",
    live: "destroyed",
  },
  {
    name: "&& rhs after a mkdir",
    payload: `mkdir deep 2>/dev/null && ${WRITE}; ${RUN}`,
    guard: "refused",
    live: "destroyed",
  },
  { name: "|| rhs", payload: `true || ${WRITE}; ${RUN}`, guard: "refused", live: "destroyed" },
  {
    name: "if then-body",
    payload: `if ${FALSE_COND}; then ${WRITE}; fi; ${RUN}`,
    guard: "refused",
    live: "destroyed",
  },
  {
    name: "if else-body",
    payload: `if ${TRUE_COND}; then :; else ${WRITE}; fi; ${RUN}`,
    guard: "refused",
    live: "destroyed",
  },
  {
    name: "elif body",
    payload: `if ${TRUE_COND}; then :; elif ${FALSE_COND}; then ${WRITE}; fi; ${RUN}`,
    guard: "refused",
    live: "destroyed",
  },
  {
    name: "case arm the guard cannot decide",
    payload: `case "$(uname)" in NOSUCHOS) ${WRITE} ;; esac; ${RUN}`,
    guard: "refused",
    live: "destroyed",
  },
  {
    name: "while body",
    payload: `while ${FALSE_COND}; do ${WRITE}; done; ${RUN}`,
    guard: "refused",
    live: "destroyed",
  },
  {
    name: "until body",
    payload: `until ${TRUE_COND}; do ${WRITE}; done; ${RUN}`,
    guard: "refused",
    live: "destroyed",
  },
  {
    name: "for body over an unknown wordlist",
    payload: `for n in $(grep -o zzzNOPE safe.sh); do ${WRITE}; done; ${RUN}`,
    guard: "refused",
    live: "destroyed",
  },
  {
    name: "arithmetic for body",
    payload: `for ((i=0;i<0;i++)); do ${WRITE}; done; ${RUN}`,
    guard: "refused",
    live: "destroyed",
  },
  {
    name: "select body",
    payload: `select n in a; do ${WRITE}; break; done < /dev/null; ${RUN}`,
    guard: "refused",
    live: "destroyed",
  },
  {
    name: "handler for a signal that never arrives",
    payload: `( trap ${QUOTED_WRITE} USR1; : ); ${RUN}`,
    guard: "refused",
    live: "destroyed",
  },
  {
    name: "definition only a branch that did not run leaves",
    payload: `if ${FALSE_COND}; then f() { ${WRITE}; }; fi; f 2>/dev/null; ${RUN}`,
    guard: "refused",
    live: "destroyed",
  },

  // Every site that records what a command writes, inside a body the shell may not enter.
  {
    name: "tee and a here-document in a branch",
    payload: `if ${FALSE_COND}; then tee unread.sh >/dev/null <<'EOF'\necho hi\nEOF\nfi; ${RUN}`,
    guard: "refused",
    live: "destroyed",
  },
  {
    name: "a rendered brace group in a branch",
    payload: `if ${FALSE_COND}; then { echo 'echo hi'; } > unread.sh; fi; ${RUN}`,
    guard: "refused",
    live: "destroyed",
  },
  {
    name: "cat and a here-document in a branch",
    payload: `${FALSE_COND} && cat > unread.sh <<'EOF'\necho hi\nEOF\n${RUN}`,
    guard: "refused",
    live: "destroyed",
  },
  {
    name: "printf in a branch",
    payload: `${FALSE_COND} && printf 'echo hi\\n' > unread.sh; ${RUN}`,
    guard: "refused",
    live: "destroyed",
  },
  {
    name: "an append in a branch",
    payload: `${FALSE_COND} && echo 'echo hi' >> unread.sh; ${RUN}`,
    guard: "refused",
    live: "destroyed",
  },

  // Commands this shell does not wait for: the write happens, but maybe not before the read.
  { name: "a background command", payload: `${WRITE} & ${RUN}`, guard: "refused", live: "racy" },
  {
    name: "an earlier part of the same pipeline",
    payload: `${WRITE} | ${RUN}`,
    guard: "refused",
    live: "racy",
  },
  {
    name: "a coprocess",
    payload: `coproc C { ${WRITE}; }; ${RUN}`,
    guard: "refused",
    live: "racy",
  },

  // The straight-line path keeps its model: these must not be refused.
  {
    name: "an if clause, which always runs",
    payload: `if ${WRITE}; then :; fi; ${RUN}`,
    guard: "allowed",
    live: "intact",
  },
  { name: "a subshell", payload: `( ${WRITE} ); ${RUN}`, guard: "allowed", live: "intact" },
  { name: "a brace group", payload: `{ ${WRITE}; }; ${RUN}`, guard: "allowed", live: "intact" },
  {
    name: "a command substitution",
    payload: `x=$( ${WRITE} ); ${RUN}`,
    guard: "allowed",
    live: "intact",
  },
  {
    name: "a here-document's command substitution",
    payload: `cat > /dev/null <<EOF\n$( ${WRITE} )\nEOF\n${RUN}`,
    guard: "allowed",
    live: "intact",
  },
  {
    name: "a function this shell called",
    payload: `f() { ${WRITE}; }; f; ${RUN}`,
    guard: "allowed",
    live: "intact",
  },
  {
    name: "an EXIT handler",
    payload: `( trap ${QUOTED_WRITE} EXIT; : ); ${RUN}`,
    guard: "allowed",
    live: "intact",
  },
  {
    name: "a straight-line write after a branch wrote the same file",
    payload: `if ${FALSE_COND}; then echo 'rm -rf "$HOME"' > unread.sh; fi; ${WRITE}; ${RUN}`,
    guard: "allowed",
    live: "intact",
  },

  // The cost: a conditional generate-then-run whose condition really holds is refused with the
  // rest, since nothing tells the two apart before the shell runs. Its refusal names the remedy.
  {
    name: "a legitimate conditional generate-then-run (&&)",
    payload: `${TRUE_COND} && ${WRITE}; ${RUN}`,
    guard: "refused",
    live: "intact",
  },
  {
    name: "a legitimate conditional generate-then-run (if)",
    payload: `if ${TRUE_COND}; then ${WRITE}; fi; ${RUN}`,
    guard: "refused",
    live: "intact",
  },
  {
    name: "a || rhs that does run",
    payload: `${FALSE_COND} || ${WRITE}; ${RUN}`,
    guard: "refused",
    live: "intact",
  },
  {
    name: "a for body over known words",
    payload: `for n in a b; do ${WRITE}; done; ${RUN}`,
    guard: "refused",
    live: "intact",
  },
  {
    name: "an earlier part of a pipeline the reader is not in",
    payload: `${WRITE} | cat; ${RUN}`,
    guard: "refused",
    live: "intact",
  },
  {
    name: "a definition the branch that ran left",
    payload: `if ${TRUE_COND}; then f() { ${WRITE}; }; fi; f 2>/dev/null; ${RUN}`,
    guard: "refused",
    live: "intact",
  },

  // A handler is set, not run, by the body it sits in. A branch the shell never enters sets no
  // handler at all, so even an `EXIT` one may never run — and a subshell's handlers and a
  // script's run mid-command, in front of a later read.
  {
    name: "an EXIT handler a branch that did not run set",
    payload: `( if ${FALSE_COND}; then trap ${QUOTED_WRITE} EXIT; fi; : ); ${RUN}`,
    guard: "refused",
    live: "destroyed",
  },
  {
    name: "an EXIT handler an && right-hand side that did not run set",
    payload: `( ${FALSE_COND} && trap ${QUOTED_WRITE} EXIT; : ); ${RUN}`,
    guard: "refused",
    live: "destroyed",
  },
  {
    name: "an EXIT handler a script set in a branch that did not run",
    payload: `bash gen.sh; ${RUN}`,
    script: `if ${FALSE_COND}; then trap ${QUOTED_WRITE} EXIT; fi\n`,
    guard: "refused",
    live: "destroyed",
  },
  {
    name: "an EXIT handler a script set on its straight line",
    payload: `bash gen.sh; ${RUN}`,
    script: `trap ${QUOTED_WRITE} EXIT\n`,
    guard: "allowed",
    live: "intact",
  },
  {
    name: "an EXIT handler a branch that DID run set",
    payload: `( if ${TRUE_COND}; then trap ${QUOTED_WRITE} EXIT; fi; : ); ${RUN}`,
    guard: "refused",
    live: "intact",
  },

  // Process substitution: bash waits for neither `>(…)` nor `<(…)`.
  {
    name: "an output process substitution",
    payload: `echo z > >( ${WRITE} ); ${RUN}`,
    guard: "refused",
    live: "racy",
  },
  {
    name: "an input process substitution",
    payload: `cat <( ${WRITE} ) >/dev/null; ${RUN}`,
    guard: "refused",
    live: "intact",
  },

  // An `&&` right-hand side the shell never runs leaves more behind than a file. The control for
  // each is the same command in an `if`, which the walk has always merged.
  {
    name: "a cd on an && right-hand side that did not run",
    payload: `cd "$HOME" && ${FALSE_COND} && cd "$LEGION_WORKSPACE"; rm -rf victim`,
    guard: "refused",
    live: "destroyed",
  },
  {
    name: "the same cd in an if",
    payload: `cd "$HOME"; if ${FALSE_COND}; then cd "$LEGION_WORKSPACE"; fi; rm -rf victim`,
    guard: "refused",
    live: "destroyed",
  },
  {
    name: "an assignment on an && right-hand side that did not run",
    payload: `d="$HOME"; ${FALSE_COND} && d="$LEGION_WORKSPACE"; rm -rf "$d/victim"`,
    guard: "refused",
    live: "destroyed",
  },
  {
    name: "the same assignment in an if",
    payload: `d="$HOME"; if ${FALSE_COND}; then d="$LEGION_WORKSPACE"; fi; rm -rf "$d/victim"`,
    guard: "refused",
    live: "destroyed",
  },
  {
    name: "a redefinition on an && right-hand side that did not run",
    payload: `f() { rm -rf "$HOME/victim"; }; ${FALSE_COND} && f() { :; }; f`,
    guard: "refused",
    live: "destroyed",
  },
  {
    name: "an && right-hand side whose value nothing reads",
    payload: `${FALSE_COND} && d="$LEGION_WORKSPACE"; rm -rf "$LEGION_WORKSPACE/x"`,
    guard: "allowed",
    live: "intact",
  },
  {
    name: "an && right-hand side that redirects a later rm",
    payload: `d="$HOME"; ${FALSE_COND} && d="$LEGION_WORKSPACE/junk"; rm -rf "$d"`,
    guard: "refused",
    live: "destroyed",
  },
  {
    name: "an && right-hand side that cds, and does run",
    payload: `[ -d deep ] && cd deep; rm -rf junk`,
    guard: "refused",
    live: "intact",
  },

  // The mirror of a handler a branch registered: one this shell certainly registered, whose
  // removal sits in a body. The removal really happening is what makes the modelled write not
  // happen, so these conditions hold rather than fail.
  {
    name: "a removal of an EXIT handler on the straight line",
    payload: `( trap ${QUOTED_WRITE} EXIT; trap - EXIT; : ); ${RUN}`,
    guard: "refused",
    live: "destroyed",
  },
  {
    name: "a removal of an EXIT handler in an if body that did run",
    payload: `( trap ${QUOTED_WRITE} EXIT; if ${TRUE_COND}; then trap - EXIT; fi; : ); ${RUN}`,
    guard: "refused",
    live: "destroyed",
  },
  {
    name: "a removal of an EXIT handler on an && right-hand side that did run",
    payload: `( trap ${QUOTED_WRITE} EXIT; ${TRUE_COND} && trap - EXIT; : ); ${RUN}`,
    guard: "refused",
    live: "destroyed",
  },
  {
    name: "a removal of an EXIT handler in a case arm",
    payload: `( trap ${QUOTED_WRITE} EXIT; case "$(uname)" in *) trap - EXIT ;; esac; : ); ${RUN}`,
    guard: "refused",
    live: "destroyed",
  },
  {
    name: "a removal of an EXIT handler in a called function",
    payload: `( trap ${QUOTED_WRITE} EXIT; f() { trap - EXIT; }; f; : ); ${RUN}`,
    guard: "refused",
    live: "destroyed",
  },
  {
    name: "a removal of an EXIT handler in an if body that did not run",
    payload: `( trap ${QUOTED_WRITE} EXIT; if ${FALSE_COND}; then trap - EXIT; fi; : ); ${RUN}`,
    guard: "refused",
    live: "intact",
  },

  // Leaving a body early. `break` and `continue` are closed, because the walk reaches their
  // bodies through a body wrap; the rest are the residual this rule does not reach, and are
  // pinned as they behave so that closing one is a visible change rather than a silent one.
  {
    name: "a break before the write",
    payload: `for n in a; do break; ${WRITE}; done; ${RUN}`,
    guard: "refused",
    live: "destroyed",
  },
  {
    name: "a break before the write in a while body",
    payload: `while true; do break; ${WRITE}; done; ${RUN}`,
    guard: "refused",
    live: "destroyed",
  },
  {
    name: "a continue before the write",
    payload: `for n in a b; do continue; ${WRITE}; done; ${RUN}`,
    guard: "refused",
    live: "destroyed",
  },
  {
    name: "RESIDUAL a return before the write",
    payload: `f() { return 0; ${WRITE}; }; f; ${RUN}`,
    guard: "allowed",
    live: "destroyed",
  },
  {
    name: "RESIDUAL a return in a branch before the write",
    payload: `f() { [ -f nosuch.conf ] || return 0; ${WRITE}; }; f; ${RUN}`,
    guard: "allowed",
    live: "destroyed",
  },
  {
    name: "RESIDUAL an exit before the write",
    payload: `( exit 0; ${WRITE} ); ${RUN}`,
    guard: "allowed",
    live: "destroyed",
  },
  {
    name: "RESIDUAL set -e and a command that fails before the write",
    payload: `( set -e; ${FALSE_COND}; ${WRITE} ); ${RUN}`,
    guard: "allowed",
    live: "destroyed",
  },
  {
    name: "RESIDUAL an exec before the write",
    payload: `( exec true; ${WRITE} ); ${RUN}`,
    guard: "allowed",
    live: "destroyed",
  },
  {
    name: "RESIDUAL an exit in a branch before the write",
    payload: `( [ -f nosuch.conf ] || exit 0; ${WRITE} ); ${RUN}`,
    guard: "allowed",
    live: "destroyed",
  },
  {
    name: "RESIDUAL set -o errexit and a command that fails before the write",
    payload: `( set -o errexit; ${FALSE_COND}; ${WRITE} ); ${RUN}`,
    guard: "allowed",
    live: "destroyed",
  },
  {
    name: "RESIDUAL set -u and an unset name before the write",
    payload: `( set -u; : "\${NOSUCHVAR}"; ${WRITE} ); ${RUN}`,
    guard: "allowed",
    live: "destroyed",
  },
  {
    name: "RESIDUAL set -e in a script whose caller ignores its status",
    payload: `bash gen.sh || true; ${RUN}`,
    script: `set -e\n${FALSE_COND}\n${WRITE}\n`,
    guard: "allowed",
    live: "destroyed",
  },

  // A handler carries the variables of the branch that set it, since the merge may already have
  // blurred what it reads. With a branch per `&&` prefix, two branches hold the same handler with
  // DIFFERENT captures, so the second is not a duplicate of the first: what it would delete is
  // named by a value a later prefix assigned.
  {
    name: "a later && prefix reassigns a name the handler reads",
    payload: `t="$LEGION_WORKSPACE/junk"; ( true && trap 'rm -rf "$t"' EXIT && t="$HOME"; : ); ${WRITE}; ${RUN}`,
    guard: "refused",
    live: "destroyed",
  },
  {
    name: "the same handler, reading nothing",
    payload: `( true && trap 'rm -rf "$LEGION_WORKSPACE/junk"' EXIT && t="$HOME"; : ); ${WRITE}; ${RUN}`,
    guard: "allowed",
    live: "intact",
  },
  {
    name: "the same handler, with nothing reassigned after it",
    payload: `t="$LEGION_WORKSPACE/junk"; ( true && trap 'rm -rf "$t"' EXIT; : ); ${WRITE}; ${RUN}`,
    guard: "allowed",
    live: "intact",
  },
];

export const START_PID = "sleep 5 & echo $! > pid";
export const KILL_PID = 'kill "$(<pid)"';
export const PID_ROWS = [
  {
    name: "a straight-line pid",
    payload: `${START_PID}; ${KILL_PID}`,
    guard: "allowed",
    ownChild: true,
  },
  {
    name: "a branch may rewrite it with another pid of this shell",
    payload: `${START_PID}; ${FALSE_COND} && echo $! > pid; ${KILL_PID}`,
    guard: "allowed",
    ownChild: true,
  },
  {
    name: "a branch may rewrite it with something that is not a pid",
    payload: `${START_PID}; ${FALSE_COND} && echo 1 > pid; ${KILL_PID}`,
    guard: "refused",
    ownChild: true,
  },
  {
    name: "only a branch ever wrote it",
    payload: `sleep 5 & ${FALSE_COND} && echo $! > pid; ${KILL_PID}`,
    guard: "refused",
    ownChild: false,
  },
];
