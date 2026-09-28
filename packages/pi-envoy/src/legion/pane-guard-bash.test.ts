import { afterAll, beforeAll, expect, test } from "bun:test";
import { spawnSync } from "node:child_process";
import { mkdirSync, mkdtempSync, rmSync } from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import { createPaneGuard, type PaneGuard } from "./pane-guard";

// The guard evaluates `${v#…}`, `${v%…}` and `${v/…/…}` itself (pane-guard-bash.ts). Held to real
// bash here: every operand over every value, as a target the guard judges, is either unknown or the
// path bash computes, and a target bash puts outside the pane's roots is never allowed.

let base = "";
let workspace = "";
let scratch = "";
let guard: PaneGuard;
let env: NodeJS.ProcessEnv;

beforeAll(() => {
  base = mkdtempSync(path.join(os.tmpdir(), "legion-pane-guard-bash-"));
  workspace = path.join(base, "ws");
  scratch = path.join(base, "tmp");
  mkdirSync(workspace, { recursive: true });
  mkdirSync(scratch, { recursive: true });
  guard = createPaneGuard({ workspace, ompPid: process.pid, scratch });
  env = { HOME: path.join(base, "home"), LEGION_WORKSPACE: workspace, TMPDIR: scratch, E: "" };
});

afterAll(() => {
  rmSync(base, { recursive: true, force: true });
});

const OPERATORS = ["/", "#", "##", "%", "%%"];
// Written after the operator: `/`-leading operands (which the parser splits differently from
// bash), escaped and quoted slashes, empty and expanding-to-empty patterns, globs, anchors.
const OPERANDS = [
  "/",
  "//",
  "///",
  "////home/v",
  "//home/v",
  "/home/v",
  "\\//_",
  "\\//\\/home\\/u",
  "a/b",
  "*/x",
  "#/P",
  "#//P",
  "%/S",
  "%//S",
  "#a/X",
  "%c/X",
  "/$E/x",
  '"/"/x',
  "'/'/x",
  "*",
  "?/Q",
  "b*/Y",
  "/x/",
  "..//home",
  "*//",
  "*//etc/ssh",
  "a/&",
];

test("every pattern expansion is unknown or bash's own value, and never allows a target outside the roots", () => {
  const values = [
    `${workspace}/build`,
    `${workspace}/a/b`,
    "a/b/c.tar.gz",
    "aaa",
    "",
    "x:y:z",
    "/a//b/",
    "...",
    "a*b",
    `${scratch}/q/../w`,
  ];
  const cases = values.flatMap((value) =>
    OPERATORS.flatMap((operator) =>
      OPERANDS.map((operand) => ({ value, expression: `\${v${operator}${operand}}` }))
    )
  );
  // One bash computes every case; a case bash rejects prints a marker instead.
  const script = cases
    .map(
      ({ value, expression }) =>
        `(v='${value}'; printf '%s\\0' "${expression}") 2>/dev/null || printf '\\1\\0'`
    )
    .join("\n");
  const run = spawnSync("bash", ["-c", `E=; ${script}`], { encoding: "utf8" });
  expect(run.status).toBe(0);
  const results = run.stdout.split("\0").slice(0, cases.length);
  expect(results).toHaveLength(cases.length);

  const underRefused: string[] = [];
  const wrong: string[] = [];
  let resolved = 0;
  cases.forEach(({ value, expression }, index) => {
    const printed = results[index] as string;
    if (printed === "\u0001") return;
    const target = path.resolve(workspace, printed);
    const inside =
      target === workspace ||
      target.startsWith(`${workspace}/`) ||
      target.startsWith(`${scratch}/`);
    const reason = guard.bash(`v='${value}'; rm -rf "${expression}"`, workspace, env);
    if (reason === undefined) {
      if (!inside) underRefused.push(`${value} ${expression} -> ${target}`);
      else resolved += 1;
      return;
    }
    const named = /rm would delete `[^`]*` \((\/[^)`]*)\)/.exec(reason)?.[1];
    if (named === undefined) return;
    resolved += 1;
    if (named !== target) wrong.push(`${value} ${expression}: bash ${target}, guard ${named}`);
  });
  expect(underRefused).toEqual([]);
  expect(wrong).toEqual([]);
  // The harness itself: most cases resolve, so an empty comparison is not a pass by default.
  expect(resolved).toBeGreaterThan(cases.length / 2);
});
