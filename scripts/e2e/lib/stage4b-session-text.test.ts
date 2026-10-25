import { afterAll, describe, expect, test } from "bun:test";
import {
  existsSync,
  mkdirSync,
  mkdtempSync,
  readdirSync,
  readFileSync,
  rmSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { scriptFunctions } from "./script-functions";

// session_text, session_copy, claim_session_text and collect_transcripts, taken from
// stage4b-sandbox-tree.sh by name: every pod of the run keeps its Oh My Pi session in the run's
// scratch Postgres, keyed by the path its file would have had, so the harness reads a session from
// the table through the database's own container (session_psql: `docker exec … psql -A -t`), never
// with a `cat` in the pod, where no file is. docker is stubbed here as psql answers in unaligned
// tuples-only mode: a held session is one row, its value exactly as stored followed by psql's one
// closing newline, and a session the table does not hold is no row and no output at all. Each test
// pins what the reader makes of that: the bytes whole, the closing newline taken off, a missing
// session refused by name, a key, which an agent writes and so is not trusted, never taken as a
// path to write at, and a key refused costing no other session yet failing the run's teardown.
const fn = scriptFunctions(join(import.meta.dir, "..", "stage4b-sandbox-tree.sh"));
const dir = mkdtempSync(join(tmpdir(), "stage4b-session-text-test."));
afterAll(() => rmSync(dir, { recursive: true, force: true }));

const sessions = "/home/legion/.omp/profiles/legion/agent/sessions";
const key = `${sessions}/-legion-workspaces-sjawhar-legion-smoke-legsmoke-1/2026-10-25_4242.jsonl`;
const container = "legion-e2e4b-db-4242";

// The stub answers psql's statements by the SQL it is sent, over ROWS, one directory a row holding
// its key, the key's hex and its content, so a key can hold `..`, `//` or a newline that no file
// name could: session_copy's listing of the keys under a prefix as hex, one key a line, as
// encode(convert_to(path, 'UTF8'), 'hex') gives it; the plain listing (SELECT path) as psql gives
// it, each key raw, so a key holding a newline spans two lines; and a session's text from the row at
// the key (session_text). It records its arguments and the SQL, and fails with DOCKER_EXIT when that
// is set.
const dockerStub = `docker() {
  local sql value r k
  printf '%s\\n' "$@" >"$ARGS"
  sql=$(cat)
  printf '%s\\n' "$sql" >>"$SQL"
  [ "$DOCKER_EXIT" = 0 ] || return "$DOCKER_EXIT"
  for value; do :; done
  value=\${value#*=}
  for r in "$ROWS"/*/; do
    [ -f "$r/key" ] || continue
    k=$(cat "$r/key"; printf x)
    k=\${k%x}
    case $sql in
      *"SELECT encode(convert_to(path, 'UTF8'), 'hex') FROM"*) case $k in "$value"*) cat "$r/hex"; printf '\\n' ;; esac ;;
      *"SELECT path FROM"*) case $k in "$value"*) printf '%s\\n' "$k" ;; esac ;;
      *) if [ "$k" = "$value" ]; then cat "$r/content"; printf '\\n'; fi ;;
    esac
  done
}`;

let runs = 0;
interface Ran {
  code: number;
  stdout: string;
  stderr: string;
  args: string[];
  sql: string;
  out: string;
}
// run runs BODY after the reader's functions, over a table holding ROWS (key → content), listed in
// the order ROWS gives them.
function run(body: string, rows: Record<string, string>, dockerExit = 0): Ran {
  const at = join(dir, `run-${++runs}`);
  const table = join(at, "rows");
  mkdirSync(table, { recursive: true });
  for (const [i, [path, content]] of Object.entries(rows).entries()) {
    const row = join(table, String(i).padStart(4, "0"));
    mkdirSync(row);
    writeFileSync(join(row, "key"), path);
    writeFileSync(join(row, "hex"), Buffer.from(path).toString("hex"));
    writeFileSync(join(row, "content"), content);
  }
  const args = join(at, "args");
  const sql = join(at, "sql");
  writeFileSync(args, "");
  writeFileSync(sql, "");
  const ran = Bun.spawnSync(
    [
      "bash",
      "-c",
      `set -Eeuo pipefail
session_db_container=${container}
${dockerStub}
${fn("session_psql")}
${fn("session_text")}
${fn("session_copy")}
${fn("collect_transcripts")}
${body}`,
    ],
    {
      env: {
        PATH: process.env.PATH ?? "",
        ARGS: args,
        SQL: sql,
        ROWS: table,
        OUT: join(at, "out"),
        DOCKER_EXIT: String(dockerExit),
      },
    }
  );
  return {
    code: ran.exitCode ?? -1,
    stdout: ran.stdout.toString(),
    stderr: ran.stderr.toString(),
    args: readFileSync(args, "utf8").split("\n").slice(0, -1),
    sql: readFileSync(sql, "utf8"),
    out: join(at, "out"),
  };
}

// A transcript as Oh My Pi writes one: JSONL whose values carry quotes, backslashes and escaped
// newlines, ending in its own newline.
const transcript = `${JSON.stringify({ type: "message", message: { role: "user", content: 'say "hi"\\n\tC:\\dir' } })}\n${JSON.stringify({ type: "message", message: { role: "assistant", content: "line one\nline two" } })}\n`;

describe("session_text", () => {
  test("prints the session's bytes whole, its own trailing newlines kept and psql's taken off", () => {
    const r = run(`session_text '${key}' >"$OUT"`, { [key]: `${transcript}\n` });
    expect(r.code).toBe(0);
    expect(readFileSync(r.out, "utf8")).toBe(`${transcript}\n`);
  });

  test("a session the table holds with no content yet is held, and prints nothing", () => {
    const r = run(`session_text '${key}' >"$OUT"; echo held`, { [key]: "" });
    expect(r.code).toBe(0);
    expect(r.stdout).toBe("held\n");
    expect(readFileSync(r.out, "utf8")).toBe("");
  });

  test("a session the table does not hold fails, naming its key", () => {
    const r = run(`session_text '${key}'`, {});
    expect(r.code).toBe(1);
    expect(r.stdout).toBe("");
    expect(r.stderr).toContain(`the session database ${container} holds no session ${key}`);
  });

  test("a database that does not answer fails, naming the key", () => {
    const r = run(`session_text '${key}'`, { [key]: transcript }, 2);
    expect(r.code).toBe(1);
    expect(r.stderr).toContain(`could not be read for the session ${key}`);
  });

  test("psql runs in the run's database container, unaligned and tuples-only, with the key a variable", () => {
    const r = run(`session_text '${key}' >/dev/null`, { [key]: transcript });
    expect(r.code).toBe(0);
    expect(r.args.slice(0, 5)).toEqual(["exec", "-i", "-e", "PGCLIENTENCODING=UTF8", container]);
    expect(r.args).toContain("-A");
    expect(r.args).toContain("-t");
    expect(r.args).toContain("ON_ERROR_STOP=1");
    expect(r.args.join(" ")).toContain("-U legion -d sessions");
    expect(r.args.at(-1)).toBe(`path=${key}`);
    // The key travels as the variable psql quotes, never spliced into the statement; the parts
    // follow the row in offset order, as ompsessions.Read reads them.
    expect(r.sql).not.toContain(key);
    expect(r.sql).toContain(":'path'");
    expect(r.sql).toContain("ORDER BY p.start_offset");
  });
});

// derived is the name session_copy gives a key whose part after the prefix is REL: each byte
// outside [A-Za-z0-9._-] made `_`.
function derived(rel: string): string {
  return Array.from(Buffer.from(rel), (b) => {
    const c = String.fromCharCode(b);
    return /[A-Za-z0-9._-]/.test(c) ? c : "_";
  }).join("");
}

// copyInto runs session_copy PREFIX into DEST, a directory three levels down a fresh temporary
// directory ROOT, with a sentinel file beside it, so a key that climbs out of DEST, or a file written
// through a symlink in it, lands where outside sees it. ROWS is given ROOT, so a key can name it;
// BEFORE prepares DEST.
interface Copied {
  r: Ran;
  root: string;
  base: string;
  dest: string;
}
const sentinel = "the sentinel beside the destination\n";
function copyInto(
  prefix: string,
  rows: (root: string) => Record<string, string>,
  before?: (dest: string, root: string) => void
): Copied {
  const root = mkdtempSync(join(dir, "copy."));
  const base = join(root, "l1", "l2", "base");
  const dest = join(base, "dest");
  mkdirSync(dest, { recursive: true });
  writeFileSync(join(base, "sentinel"), sentinel);
  before?.(dest, root);
  return { r: run(`session_copy '${prefix}' '${dest}'`, rows(root)), root, base, dest };
}
// outside lists every entry under ROOT that is neither DEST, nor in it, nor one copyInto made.
function outside({ root, dest }: Copied): string[] {
  const made: Record<string, true> = {
    l1: true,
    "l1/l2": true,
    "l1/l2/base": true,
    "l1/l2/base/sentinel": true,
  };
  const found: string[] = [];
  const walk = (at: string, rel: string) => {
    for (const e of readdirSync(at, { withFileTypes: true })) {
      const path = join(at, e.name);
      if (path === dest) continue;
      const shown = rel === "" ? e.name : `${rel}/${e.name}`;
      if (!made[shown]) found.push(shown);
      if (e.isDirectory()) walk(path, shown);
    }
  };
  walk(root, "");
  return found;
}
// untouched: nothing appeared outside DEST, and the sentinel beside it is as it was, alone with it.
function untouched(c: Copied) {
  expect(outside(c)).toEqual([]);
  expect(readFileSync(join(c.base, "sentinel"), "utf8")).toBe(sentinel);
  expect(readdirSync(c.base).sort()).toEqual(["dest", "sentinel"]);
}

describe("session_copy", () => {
  const stem = key.replace(/\.jsonl$/, "");

  test("writes every session under the prefix at the name its key after the prefix derives, and none outside it", () => {
    const sub = `${stem}/thermonuclear-deep-review-1.jsonl`;
    const nested = `${stem}/thermonuclear-deep-review-1/Explore-2.jsonl`;
    const other = `${sessions}/-legion-workspaces-sjawhar-legion-smoke-legsmoke-2/2026-10-25_4343.jsonl`;
    const r = run(`session_copy '${stem}/' "$OUT"`, {
      [key]: transcript,
      [sub]: `${transcript}{"sub":1}\n`,
      [nested]: '{"nested":"\\"x\\""}\n',
      [other]: transcript,
    });
    expect(r.code).toBe(0);
    // A subagent's session keeps its name; a nested one's `/` becomes `_`, so DIR holds files alone.
    expect(readdirSync(r.out).sort()).toEqual([
      "thermonuclear-deep-review-1.jsonl",
      "thermonuclear-deep-review-1_Explore-2.jsonl",
    ]);
    expect(readFileSync(join(r.out, "thermonuclear-deep-review-1.jsonl"), "utf8")).toBe(
      `${transcript}{"sub":1}\n`
    );
    expect(readFileSync(join(r.out, "thermonuclear-deep-review-1_Explore-2.jsonl"), "utf8")).toBe(
      '{"nested":"\\"x\\""}\n'
    );
  });

  test("no session under the prefix copies nothing, makes no directory, and succeeds", () => {
    const r = run(`session_copy '${stem}/' "$OUT"; echo copied`, { [key]: transcript });
    expect(r.code).toBe(0);
    expect(r.stdout).toBe("copied\n");
    expect(existsSync(r.out)).toBe(false);
  });

  test("a database that does not answer the listing fails", () => {
    const r = run(`session_copy '${sessions}/' "$OUT"`, { [key]: transcript }, 2);
    expect(r.code).toBe(1);
  });

  // An agent writes its own session rows, so these keys are an attacker's, each a way out of DIR were
  // its part after the prefix taken as a path. Each row is [what the key holds, its parts after the
  // prefix → content in listing order, the names DIR must hold after], given ROOT, the temporary
  // directory DIR is three levels down.
  const crafted: [string, (root: string) => Record<string, string>, (root: string) => string[]][] =
    [
      ["`../`", () => ({ "../../climbed.jsonl": "climbed\n" }), () => [".._.._climbed.jsonl"]],
      [
        "an absolute path after the prefix",
        (root) => ({ [`${root}/l1/absolute.jsonl`]: "absolute\n" }),
        (root) => [derived(`${root}/l1/absolute.jsonl`)],
      ],
      // psql lists keys one a line, so the key's first line, a key of its own here, and its second,
      // `../../newline.jsonl`, read as two keys to a reader that does not take the listing as hex.
      [
        "a newline",
        () => ({ nl: "first\n", "nl\n../../newline.jsonl": "newline\n" }),
        () => ["nl", "nl_.._.._newline.jsonl"],
      ],
      // é is two bytes, each made `_`, then the `/`.
      [
        "bytes past ASCII",
        () => ({ "café/Explore 2.jsonl": "bytes\n" }),
        () => ["caf___Explore_2.jsonl"],
      ],
    ];
  for (const [what, rows, names] of crafted) {
    test(`a key holding ${what} is written inside DIR, at a name of [A-Za-z0-9._-]`, () => {
      let rels: Record<string, string> = {};
      const c = copyInto(`${stem}/`, (root) => {
        rels = rows(root);
        return Object.fromEntries(
          Object.entries(rels).map(([rel, text]) => [`${stem}/${rel}`, text])
        );
      });
      untouched(c);
      expect(c.r.code).toBe(0);
      expect(c.r.stderr).toBe("");
      expect(readdirSync(c.dest).sort()).toEqual(names(c.root).sort());
      for (const [rel, text] of Object.entries(rels)) {
        expect(readFileSync(join(c.dest, derived(rel)), "utf8")).toBe(text);
      }
    });
  }

  // [what the key is, the parts after the prefix in listing order, the refused ones, each with the
  // other key it names (for a shared name), and the names DIR holds after]. Each list leads with a
  // key the refusal must not cost: a refusal skips its own key, never the rest of the call.
  const refusals: [string, string[], [string, string?][], string[]][] = [
    ["`..`, DIR's parent", ["ok.jsonl", ".."], [[".."]], ["ok.jsonl"]],
    ["`.`, DIR itself", ["ok.jsonl", "."], [["."]], ["ok.jsonl"]],
    ["the prefix alone, an empty name", ["ok.jsonl", ""], [[""]], ["ok.jsonl"]],
    // Of two keys of one name neither is written: first-come would let an agent's row that sorts
    // first put its bytes at the name of the session it collides with.
    [
      "`../x.jsonl`, which shares `.._x.jsonl`'s name",
      ["ok.jsonl", ".._x.jsonl", "../x.jsonl"],
      [
        ["../x.jsonl", ".._x.jsonl"],
        [".._x.jsonl", "../x.jsonl"],
      ],
      ["ok.jsonl"],
    ],
    [
      "absolute after the prefix, which shares `_abs.jsonl`'s name",
      ["ok.jsonl", "_abs.jsonl", "/abs.jsonl"],
      [
        ["/abs.jsonl", "_abs.jsonl"],
        ["_abs.jsonl", "/abs.jsonl"],
      ],
      ["ok.jsonl"],
    ],
    [
      "a newline, which shares `a_b.jsonl`'s name",
      ["ok.jsonl", "a_b.jsonl", "a\nb.jsonl"],
      [
        ["a\nb.jsonl", "a_b.jsonl"],
        ["a_b.jsonl", "a\nb.jsonl"],
      ],
      ["ok.jsonl"],
    ],
  ];
  for (const [what, rels, refused, written] of refusals) {
    test(`a key of ${what} is refused by name and skipped, and every other key is still written`, () => {
      const c = copyInto(`${stem}/`, () =>
        Object.fromEntries(rels.map((rel, i) => [`${stem}/${rel}`, `SESSION-BYTES-${i}\n`]))
      );
      untouched(c);
      expect(c.r.code).toBe(1);
      for (const [rel, other] of refused) {
        expect(c.r.stderr).toContain(`the session key ${stem}/${rel} `);
        if (other !== undefined) {
          expect(c.r.stderr).toContain(
            `the session key ${stem}/${rel} would be written as ${derived(rel)} in ${c.dest}, as would the key ${stem}/${other}; neither is copied`
          );
        }
      }
      expect(c.r.stderr).not.toContain("SESSION-BYTES");
      expect(readdirSync(c.dest).sort()).toEqual(written);
      for (const name of written) {
        expect(readFileSync(join(c.dest, name), "utf8")).toBe(
          `SESSION-BYTES-${rels.indexOf(name)}\n`
        );
      }
    });
  }

  test("a prefix holding two keys of one name writes every other key, names both, and fails", () => {
    // The colliding pair comes first and apart in the listing, so a refusal that stopped the call,
    // or one judged only once both were read, would cost the keys after it.
    const rels = ["sub/x.jsonl", "a.jsonl", "sub_x.jsonl", "z/Explore-1.jsonl"];
    const c = copyInto(`${stem}/`, () =>
      Object.fromEntries(rels.map((rel, i) => [`${stem}/${rel}`, `SESSION-BYTES-${i}\n`]))
    );
    untouched(c);
    expect(c.r.code).toBe(1);
    expect(readdirSync(c.dest).sort()).toEqual(["a.jsonl", "z_Explore-1.jsonl"]);
    expect(readFileSync(join(c.dest, "a.jsonl"), "utf8")).toBe("SESSION-BYTES-1\n");
    expect(readFileSync(join(c.dest, "z_Explore-1.jsonl"), "utf8")).toBe("SESSION-BYTES-3\n");
    expect(c.r.stderr).toBe(
      `the session key ${stem}/sub/x.jsonl would be written as sub_x.jsonl in ${c.dest}, as would the key ${stem}/sub_x.jsonl; neither is copied\n` +
        `the session key ${stem}/sub_x.jsonl would be written as sub_x.jsonl in ${c.dest}, as would the key ${stem}/sub/x.jsonl; neither is copied\n`
    );
  });

  test("three keys of one name are each refused by name, naming another of them", () => {
    const rels = ["p q.jsonl", "p/q.jsonl", "ok.jsonl", "p_q.jsonl"];
    const c = copyInto(`${stem}/`, () =>
      Object.fromEntries(rels.map((rel, i) => [`${stem}/${rel}`, `SESSION-BYTES-${i}\n`]))
    );
    untouched(c);
    expect(c.r.code).toBe(1);
    expect(readdirSync(c.dest)).toEqual(["ok.jsonl"]);
    for (const rel of ["p q.jsonl", "p/q.jsonl", "p_q.jsonl"]) {
      expect(c.r.stderr).toContain(
        `the session key ${stem}/${rel} would be written as p_q.jsonl in ${c.dest}, as would the key ${stem}/`
      );
    }
    expect(c.r.stderr.split("\n").filter((l) => l !== "")).toHaveLength(3);
    expect(c.r.stderr).not.toContain("SESSION-BYTES");
  });

  test("a name that resolves outside DIR, through a symlink already there, is refused by name, and the rest written", () => {
    const c = copyInto(
      `${stem}/`,
      () => ({
        [`${stem}/evil.jsonl`]: "SESSION-BYTES\n",
        [`${stem}/ok.jsonl`]: transcript,
      }),
      (dest, root) => symlinkSync(join(root, "l1", "escaped.jsonl"), join(dest, "evil.jsonl"))
    );
    untouched(c);
    expect(existsSync(join(c.root, "l1", "escaped.jsonl"))).toBe(false);
    expect(c.r.code).toBe(1);
    expect(c.r.stderr).toContain(
      `the session key ${stem}/evil.jsonl would be written at ${c.root}/l1/escaped.jsonl, outside`
    );
    expect(c.r.stderr).not.toContain("SESSION-BYTES");
    expect(readFileSync(join(c.dest, "ok.jsonl"), "utf8")).toBe(transcript);
  });
});

// collect_transcripts, the teardown's copy of every pod's session out of the session database
// before the teardown removes it: a session it leaves out is gone for good, so it must fail, and
// cleanup must carry that failure to the run's verdict as the teardown check `transcripts`.
describe("collect_transcripts", () => {
  test("over a table with a colliding pair, copies every other session and the controller's, and fails", () => {
    const pod = `${sessions}/-legion-workspaces-sjawhar-legion-smoke-legsmoke-1`;
    const r = run(
      `pod_sessions_dir=${sessions} evidence="$OUT" profile_agent="$OUT.profile"
mkdir -p "$profile_agent/sessions/-home"
printf 'controller\\n' >"$profile_agent/sessions/-home/c.jsonl"
collect_transcripts || echo "collect_transcripts failed $?"`,
      {
        [`${pod}/a.jsonl`]: "SESSION-BYTES-a\n",
        [`${pod}/x/y.jsonl`]: "SESSION-BYTES-xy\n",
        [`${pod}_x/y.jsonl`]: "SESSION-BYTES-x_y\n",
        [`${pod}/b.jsonl`]: "SESSION-BYTES-b\n",
      }
    );
    expect(r.code).toBe(0);
    expect(r.stdout).toBe("collect_transcripts failed 1\n");
    const out = join(r.out, "transcripts");
    expect(readdirSync(out).sort()).toEqual([
      "-legion-workspaces-sjawhar-legion-smoke-legsmoke-1_a.jsonl",
      "-legion-workspaces-sjawhar-legion-smoke-legsmoke-1_b.jsonl",
      "controller",
    ]);
    expect(
      readFileSync(join(out, "-legion-workspaces-sjawhar-legion-smoke-legsmoke-1_b.jsonl"), "utf8")
    ).toBe("SESSION-BYTES-b\n");
    expect(readFileSync(join(out, "controller", "-home", "c.jsonl"), "utf8")).toBe("controller\n");
    expect(r.stderr).toContain(`the session key ${pod}/x/y.jsonl would be written as `);
    expect(r.stderr).toContain(`the session key ${pod}_x/y.jsonl would be written as `);
    expect(r.stderr).not.toContain("SESSION-BYTES");
  });

  test("over a table it copies whole, succeeds", () => {
    const pod = `${sessions}/-legion-workspaces-sjawhar-legion-smoke-legsmoke-1`;
    const r = run(
      `pod_sessions_dir=${sessions} evidence="$OUT" profile_agent="$OUT.profile"
collect_transcripts && echo copied`,
      { [`${pod}/a.jsonl`]: transcript }
    );
    expect(r.code).toBe(0);
    expect(r.stdout).toBe("copied\n");
    expect(r.stderr).toBe("");
  });

  // teardownRun runs the script's own cleanup, as the EXIT trap of a run whose checkpoints all
  // passed (ok set), with the real collect_transcripts over SESSION_COPY, the stand-in for the
  // database copy, and the rest of the teardown stubbed; the run took no lock, so the cluster's
  // teardown is not reached.
  function teardownRun(sessionCopy: string) {
    const at = mkdtempSync(join(dir, "teardown."));
    const evidence = join(at, "evidence");
    const profile = join(at, "profile");
    mkdirSync(join(profile, "sessions"), { recursive: true });
    writeFileSync(join(profile, "sessions", "c.jsonl"), "controller\n");
    const ran = Bun.spawnSync(
      [
        "bash",
        "-c",
        `set -Eeuo pipefail
work=${JSON.stringify(join(at, "work"))} evidence=${JSON.stringify(evidence)} profile_agent=${JSON.stringify(profile)}
pod_sessions_dir=${sessions} session_db_container=${container}
check=controller ok=1 was_blocked= locked= smoke_main_cleaning=
pair_session= negative_config_pod= block_tree= pg_container=none
shape_pid= daemon_pid= watch_pid= events_pid= leaks_pid= sampler_pid= interests_pid=
mkdir -p "$work" "$evidence"
exec 7>&1
stop_tree() { :; }; stop_pid() { :; }; record_pair() { :; }; run_processes() { :; }; docker() { :; }
note() { echo "   $*"; }
${sessionCopy}
${fn("collect_transcripts")}
${fn("cleanup")}
trap cleanup EXIT
exit 0`,
      ],
      { env: { PATH: process.env.PATH ?? "" } }
    );
    const stdout = ran.stdout.toString();
    // cleanup's ERR trap reports a teardown command that fails, a helper left unstubbed included.
    expect(stdout).not.toContain("cleanup warning:");
    return { code: ran.exitCode, stdout, evidence };
  }

  test("a copy that leaves a session out fails the run's teardown check transcripts", () => {
    const r = teardownRun(`session_copy() {
  [ "$1 $2" = "${sessions}/ $evidence/transcripts" ] || return 9
  echo "the session key ${sessions}/x/y.jsonl would be written as x_y.jsonl in $2, as would the key ${sessions}/x_y.jsonl; neither is copied" >&2
  return 1
}`);
    expect(r.code).toBe(1);
    expect(r.stdout).toContain(
      `CHECK transcripts: FAIL: the pods' sessions could not all be copied from the session database ${container} into ${r.evidence}/transcripts`
    );
    expect(r.stdout).toContain(
      "stage 4b e2e: FAIL (check transcripts, in the teardown after check controller)"
    );
    // The refusal is in the transcript above the verdict, and the controller's sessions still went.
    expect(r.stdout.indexOf(`the session key ${sessions}/x/y.jsonl`)).toBeLessThan(
      r.stdout.indexOf("CHECK transcripts: FAIL")
    );
    expect(readFileSync(join(r.evidence, "transcripts", "controller", "c.jsonl"), "utf8")).toBe(
      "controller\n"
    );
  });

  test("a copy that leaves nothing out leaves a passed run's teardown clean", () => {
    const r = teardownRun(`session_copy() { [ "$1 $2" = "${sessions}/ $evidence/transcripts" ]; }`);
    expect(r.code).toBe(0);
    expect(r.stdout).not.toContain("FAIL");
  });
});

describe("claim_session_text", () => {
  const claimed = `claim_session_file() { [ "$1 $2" = "LEGSMOKE-1 planner" ] || return 1; printf '%s\\n' '${key}'; }
${fn("claim_session_text")}`;

  test("reads the session the claim records from the table", () => {
    const r = run(`${claimed}\nclaim_session_text LEGSMOKE-1 planner >"$OUT"`, {
      [key]: transcript,
    });
    expect(r.code).toBe(0);
    expect(readFileSync(r.out, "utf8")).toBe(transcript);
    expect(r.args.at(-1)).toBe(`path=${key}`);
  });

  test("a claim that records no session file reads nothing", () => {
    const r = run(`${claimed}\nclaim_session_text LEGSMOKE-1 reviewer`, { [key]: transcript });
    expect(r.code).toBe(1);
    expect(r.args).toEqual([]);
  });

  test("a claim whose session the table does not hold fails, naming the key", () => {
    const r = run(`${claimed}\nclaim_session_text LEGSMOKE-1 planner`, {});
    expect(r.code).toBe(1);
    expect(r.stderr).toContain(`holds no session ${key}`);
  });
});
