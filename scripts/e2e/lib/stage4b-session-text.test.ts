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

// session_text, session_copy and claim_session_text, taken from stage4b-sandbox-tree.sh by name:
// every pod of the run keeps its Oh My Pi session in the run's scratch Postgres, keyed by the path
// its file would have had, so the harness reads a session from the table through the database's own
// container (session_psql: `docker exec … psql -A -t`), never with a `cat` in the pod, where no file
// is. docker is stubbed here as psql answers in unaligned tuples-only mode: a held session is one
// row, its value exactly as stored followed by psql's one closing newline, and a session the table
// does not hold is no row and no output at all. Each test pins what the reader makes of that: the
// bytes whole, the closing newline taken off, a missing session refused by name, and a key, which
// an agent writes and so is not trusted, never taken as a path to write at.
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

  // [what the key is, the parts after the prefix in listing order, the refused one, the earlier one
  // whose name it would take]. Each list leads with a key that would copy, so a refusal is shown to
  // come before any write.
  const refusals: [string, string[], string, string?][] = [
    ["`..`, DIR's parent", ["ok.jsonl", ".."], ".."],
    ["`.`, DIR itself", ["ok.jsonl", "."], "."],
    ["the prefix alone, an empty name", ["ok.jsonl", ""], ""],
    [
      "`../x.jsonl`, which takes `.._x.jsonl`'s name",
      [".._x.jsonl", "../x.jsonl"],
      "../x.jsonl",
      ".._x.jsonl",
    ],
    [
      "absolute after the prefix, which takes `_abs.jsonl`'s name",
      ["_abs.jsonl", "/abs.jsonl"],
      "/abs.jsonl",
      "_abs.jsonl",
    ],
    [
      "a newline, which takes `a_b.jsonl`'s name",
      ["a_b.jsonl", "a\nb.jsonl"],
      "a\nb.jsonl",
      "a_b.jsonl",
    ],
  ];
  for (const [what, rels, refused, earlier] of refusals) {
    test(`a key of ${what} is refused by name, and nothing is written`, () => {
      const c = copyInto(`${stem}/`, () =>
        Object.fromEntries(rels.map((rel, i) => [`${stem}/${rel}`, `SESSION-BYTES-${i}\n`]))
      );
      untouched(c);
      expect(c.r.code).toBe(1);
      expect(c.r.stderr).toContain(`the session key ${stem}/${refused} `);
      if (earlier !== undefined) expect(c.r.stderr).toContain(`as the key ${stem}/${earlier} is`);
      expect(c.r.stderr).not.toContain("SESSION-BYTES");
      expect(readdirSync(c.dest)).toEqual([]);
    });
  }

  test("a name that resolves outside DIR, through a symlink already there, is refused by name", () => {
    const c = copyInto(
      `${stem}/`,
      () => ({ [`${stem}/evil.jsonl`]: "SESSION-BYTES\n" }),
      (dest, root) => symlinkSync(join(root, "l1", "escaped.jsonl"), join(dest, "evil.jsonl"))
    );
    untouched(c);
    expect(existsSync(join(c.root, "l1", "escaped.jsonl"))).toBe(false);
    expect(c.r.code).toBe(1);
    expect(c.r.stderr).toContain(
      `the session key ${stem}/evil.jsonl would be written at ${c.root}/l1/escaped.jsonl, outside`
    );
    expect(c.r.stderr).not.toContain("SESSION-BYTES");
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
