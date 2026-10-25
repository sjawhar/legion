import { afterAll, describe, expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { scriptFunctions } from "./script-functions";

// session_text, session_copy and claim_session_text, taken from stage4b-sandbox-tree.sh by name:
// every pod of the run keeps its Oh My Pi session in the run's scratch Postgres, keyed by the path
// its file would have had, so the harness reads a session from the table through the database's own
// container (session_psql: `docker exec … psql -A -t`), never with a `cat` in the pod, where no file
// is. docker is stubbed here as psql answers in unaligned tuples-only mode: a held session is one
// row, its value exactly as stored followed by psql's one closing newline, and a session the table
// does not hold is no row and no output at all. Each test pins what the reader makes of that: the
// bytes whole, the closing newline taken off, and a missing session refused by name.
const fn = scriptFunctions(join(import.meta.dir, "..", "stage4b-sandbox-tree.sh"));
const dir = mkdtempSync(join(tmpdir(), "stage4b-session-text-test."));
afterAll(() => rmSync(dir, { recursive: true, force: true }));

const sessions = "/home/legion/.omp/profiles/legion/agent/sessions";
const key = `${sessions}/-legion-workspaces-sjawhar-legion-smoke-legsmoke-1/2026-10-25_4242.jsonl`;
const container = "legion-e2e4b-db-4242";

// The stub answers psql's two statements by the SQL it is sent: the listing of the keys under a
// prefix (session_copy) one key a line, and a session's text from the row ROWS holds at the key
// (session_text). It records its arguments and the SQL, and fails with DOCKER_EXIT when that is set.
const dockerStub = `docker() {
  local sql value
  printf '%s\\n' "$@" >"$ARGS"
  sql=$(cat)
  printf '%s\\n' "$sql" >>"$SQL"
  [ "$DOCKER_EXIT" = 0 ] || return "$DOCKER_EXIT"
  for value; do :; done
  value=\${value#*=}
  case $sql in
    *"SELECT path FROM"*)
      (cd "$ROWS" && find . -type f | cut -c2- | sort) | while IFS= read -r p; do
        case $p in "$value"*) printf '%s\\n' "$p" ;; esac
      done ;;
    *) if [ -f "$ROWS$value" ]; then cat "$ROWS$value"; printf '\\n'; fi ;;
  esac
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
// run runs BODY after the reader's functions, over a table holding ROWS (key → content).
function run(body: string, rows: Record<string, string>, dockerExit = 0): Ran {
  const at = join(dir, `run-${++runs}`);
  const table = join(at, "rows");
  mkdirSync(table, { recursive: true });
  for (const [path, content] of Object.entries(rows)) {
    mkdirSync(dirname(join(table, path)), { recursive: true });
    writeFileSync(join(table, path), content);
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

describe("session_copy", () => {
  test("writes every session under the prefix at its key after the prefix, and none outside it", () => {
    const stem = key.replace(/\.jsonl$/, "");
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
    expect(readFileSync(join(r.out, "thermonuclear-deep-review-1.jsonl"), "utf8")).toBe(
      `${transcript}{"sub":1}\n`
    );
    expect(
      readFileSync(join(r.out, "thermonuclear-deep-review-1", "Explore-2.jsonl"), "utf8")
    ).toBe('{"nested":"\\"x\\""}\n');
    expect(() => readFileSync(join(r.out, "2026-10-25_4343.jsonl"))).toThrow();
  });

  test("no session under the prefix copies nothing and succeeds", () => {
    const r = run(`session_copy '${key.replace(/\.jsonl$/, "")}/' "$OUT"; echo copied`, {
      [key]: transcript,
    });
    expect(r.code).toBe(0);
    expect(r.stdout).toBe("copied\n");
  });

  test("a database that does not answer the listing fails", () => {
    const r = run(`session_copy '${sessions}/' "$OUT"`, { [key]: transcript }, 2);
    expect(r.code).toBe(1);
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
