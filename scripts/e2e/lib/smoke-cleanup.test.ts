import { afterAll, describe, expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { scriptFunctions } from "./script-functions";

// clean_smoke_main's pull request and branch (lib/workflow.sh) through each stage's own teardown:
// Stage 4b's cleanup and remove_run_branches, and Stage 3's cleanup and the lib's
// close_unpassed_run_pull_requests. Each run takes the stage's functions from the script by name,
// sources lib/rig.sh and lib/workflow.sh as the script does, and runs under the script's own `set`
// line and top-level traps, read from the script. The teardown's cluster, NATS and process helpers
// are stubbed; gh is a fake holding the smoke repository's branches and pull requests, which each
// case seeds and reads back.
const e2e = join(import.meta.dir, "..");
const root = join(e2e, "..", "..");
const dir = mkdtempSync(join(tmpdir(), "smoke-cleanup-test."));
afterAll(() => rmSync(dir, { recursive: true, force: true }));
const bin = join(dir, "bin");
mkdirSync(bin);
for (const tool of ["docker", "tmux"]) {
  writeFileSync(join(bin, tool), "#!/bin/sh\nexit 0\n", { mode: 0o755 });
}
// The fake gh. FAKE_GH_MERGE_FAIL, FAKE_GH_CLOSE_FAIL and FAKE_GH_LIST_FAIL make that call fail with
// the variable's text on stderr; FAKE_GH_CREATE_LOST makes the branch's create fail after GitHub
// made the branch, as a create whose answer a 504 lost.
writeFileSync(
  join(bin, "gh"),
  `#!/usr/bin/env bash
set -euo pipefail
state=$FAKE_GH_DIR
printf '%s\\n' "$*" >>"$state/calls"
repo= method= jq= head= comment= delete=
fields=() pos=()
while [ $# -gt 0 ]; do
  case $1 in
    -R) repo=$2; shift 2 ;;
    -X) method=$2; shift 2 ;;
    --jq | -q) jq=$2; shift 2 ;;
    -f) fields+=("$2"); shift 2 ;;
    --head) head=$2; shift 2 ;;
    --comment) comment=$2; shift 2 ;;
    --delete-branch) delete=1; shift ;;
    --squash) shift ;;
    --base | --title | --body | --state | --limit | --json) shift 2 ;;
    *) pos+=("$1"); shift ;;
  esac
done
answer() { if [ -n "$jq" ]; then jq -r "$jq"; else cat; fi; }
prs() { jq "$@" "$state/prs.json" >"$state/prs.next"; mv "$state/prs.next" "$state/prs.json"; }
has_branch() { grep -qxF -- "$1" "$state/branches"; }
drop_branch() { grep -vxF -- "$1" "$state/branches" >"$state/branches.next" || true; mv "$state/branches.next" "$state/branches"; }
pr_field() { jq -r --argjson n "$1" ".[] | select(.number == \\$n) | .$2" "$state/prs.json"; }
refuse() { printf '%s\\n' "$1" >&2; exit 1; }
case "\${pos[0]}" in
  pr)
    n=\${pos[2]:-}
    case "\${pos[1]:-}" in
      list)
        [ -z "\${FAKE_GH_LIST_FAIL:-}" ] || refuse "$FAKE_GH_LIST_FAIL"
        jq --arg head "$head" '[.[] | select(.state == "OPEN" and ($head == "" or .headRefName == $head))]' "$state/prs.json" | answer ;;
      create)
        has_branch "$head" || refuse "pull request create failed: no branch $head"
        n=$(jq 'map(.number) | max + 1' "$state/prs.json")
        prs --argjson n "$n" --arg url "https://github.com/$repo/pull/$n" --arg head "$head" \\
          '. + [{number: $n, url: $url, headRefName: $head, state: "OPEN"}]'
        echo "https://github.com/$repo/pull/$n" ;;
      view) jq -n --arg state "$(pr_field "$n" state)" '{mergeStateStatus: "CLEAN", state: $state}' | answer ;;
      merge)
        [ -z "\${FAKE_GH_MERGE_FAIL:-}" ] || refuse "$FAKE_GH_MERGE_FAIL"
        prs --argjson n "$n" 'map(if .number == $n then .state = "MERGED" else . end)'
        [ -z "$delete" ] || drop_branch "$(pr_field "$n" headRefName)" ;;
      close)
        [ -z "\${FAKE_GH_CLOSE_FAIL:-}" ] || refuse "$FAKE_GH_CLOSE_FAIL"
        [ "$(pr_field "$n" state)" = OPEN ] || refuse "pull request $repo#$n is not open"
        prs --argjson n "$n" 'map(if .number == $n then .state = "CLOSED" else . end)'
        printf '#%s %s\\n' "$n" "$comment" >>"$state/comments"
        [ -z "$delete" ] || drop_branch "$(pr_field "$n" headRefName)" ;;
      *) refuse "fake gh: no answer for pr \${pos[1]:-}" ;;
    esac ;;
  api)
    path=\${pos[1]}
    if [ -z "$method" ]; then
      if [ \${#fields[@]} -eq 0 ]; then method=GET; else method=POST; fi
    fi
    case "$method $path" in
      "GET repos/"*"/git/trees/main?recursive=1")
        jq -n --arg paths "\${FAKE_GH_LEFTOVERS:-}" \\
          '{tree: ([$paths | split("\\n")[] | select(length > 0) | {type: "blob", path: .}] + [{type: "blob", path: "README.md"}])}' | answer ;;
      "GET repos/"*"/git/ref/heads/main") echo '{"object":{"sha":"0123abc"}}' | answer ;;
      "GET repos/"*"/git/matching-refs/heads/"*)
        jq -R -s --arg prefix "\${path#*/git/matching-refs/heads/}" \\
          '[split("\\n")[] | select(length > 0 and startswith($prefix)) | {ref: ("refs/heads/" + .)}]' "$state/branches" | answer ;;
      "POST repos/"*"/git/refs")
        ref=\${fields[0]#ref=refs/heads/}
        ! has_branch "$ref" || refuse "gh: Reference already exists (HTTP 422)"
        echo "$ref" >>"$state/branches"
        [ -z "\${FAKE_GH_CREATE_LOST:-}" ] || refuse "gh: HTTP 504: Gateway Timeout"
        echo '{}' ;;
      "DELETE repos/"*"/git/refs/heads/"*)
        ref=\${path#*/git/refs/heads/}
        has_branch "$ref" || refuse "gh: Reference does not exist (HTTP 422)"
        drop_branch "$ref" ;;
      "GET repos/"*"/contents/"*) echo '{"sha":"blob1"}' | answer ;;
      "DELETE repos/"*"/contents/"*) echo '{}' ;;
      *) refuse "fake gh: no answer for $method $path" ;;
    esac ;;
  *) refuse "fake gh: no answer for \${pos[*]}" ;;
esac
`,
  { mode: 0o755 }
);

// The script's own options and top-level traps, its EXIT trap among them.
function scriptShell(path: string) {
  const script = readFileSync(path, "utf8");
  const set = script.match(/^set -.*$/m);
  const traps = script.match(/^trap .*$/gm);
  if (set === null || traps === null || !traps.some((t) => t === "trap cleanup EXIT")) {
    throw new Error(`${path} has no top-level set line or no trap cleanup EXIT`);
  }
  return { set: set[0], traps: traps.join("\n") };
}
const stage4bScript = join(e2e, "stage4b-sandbox-tree.sh");
const stage3Script = join(e2e, "stage3-devbox-workflow.sh");
const fn4b = scriptFunctions(stage4bScript);
const fn3 = scriptFunctions(stage3Script);
const shell4b = scriptShell(stage4bScript);
const shell3 = scriptShell(stage3Script);
const libs = `. "$root/scripts/e2e/lib/rig.sh"
. "$root/scripts/e2e/lib/workflow.sh"
stop_tree() { :; }; stop_pid() { :; }; run_processes() { :; }
proof_human='sjawhar-agent[bot]'`;

// Stage 4b's driver from the start of `done`'s checkpoint: tree 1 merged, the proof human
// established and the lock held, so the teardown owns the smoke repository's run objects.
const stage4b = (tail: string) => `${shell4b.set}
root=${JSON.stringify(root)} work="$RUN/work" evidence="$RUN/evidence"
check=setup check_started=2026-10-08T00:00:00Z
ok= was_blocked= until=\${UNTIL:-} locked=1 compared= snapshotted= audited= prod_baseline=
tree1=LEGSMOKE-101 tree2=LEGSMOKE-102 tree3=LEGSMOKE-103 tree4=LEGSMOKE-104
pair_session= shape_pid= daemon_pid= watch_pid= events_pid= leaks_pid= sampler_pid= interests_pid= pg_container=none run_label=legsmoke smoke_main_cleaning=
project=LEGSMOKE repo=sjawhar/legion-smoke
mkdir -p "$work" "$evidence/model-gateway"
exec 7>&1
${fn4b("begin")}
${fn4b("note")}
${fn4b("pass")}
${fn4b("until_reached")}
${fn4b("fail")}
${fn4b("blocked")}
${libs}
collect_transcripts() { :; }; record_pair() { :; }; op() { :; }
teardown() { :; }; namespace_clean() { :; }; delete_consumers() { :; }
${fn4b("remove_run_branches")}
${fn4b("cleanup")}
${shell4b.traps}
${tail}
`;
// done's cleanup of the smoke main, as the driver runs it.
const done4b = `begin done
smoke_main_cleaning=1
clean_smoke_main
smoke_main_cleaning=
pass`;

// Stage 3's driver at its smoke-main-clean checkpoint, its first issue merged and signed off.
const stage3 = (tail: string) => `${shell3.set}
root=${JSON.stringify(root)} work="$RUN/work" evidence="$RUN/evidence" profile_agent="$RUN/no-profile"
check=setup check_started=2026-10-08T00:00:00Z
ok= audited= prod_baseline= watcher_pid= daemon_pid= dispatch_pid= listener_pid= bridge_pid= pg_container=none nats_container=none
project=S312345678 ptoken=s312345678 repo=sjawhar/legion-smoke
mkdir -p "$work" "$evidence/model-gateway" "$evidence/transcripts"
${fn3("begin")}
${fn3("note")}
${fn3("pass")}
${fn3("fail")}
${libs}
${fn3("collect_transcripts")}
${fn3("cleanup")}
${shell3.traps}
${tail}
`;
const clean3 = `begin smoke-main-clean
clean_smoke_main
pass`;

interface PullRequest {
  number: number;
  headRefName: string;
  state: "OPEN" | "CLOSED" | "MERGED";
}
interface Fake {
  mergeFails?: string;
  closeFails?: string;
  listFails?: string;
  createLost?: boolean;
  until?: string;
}
interface RunResult {
  code: number;
  stdout: string;
  stderr: string;
  calls: string[];
  comments: string[];
  branches: string[];
  pr: (n: number) => PullRequest["state"] | undefined;
}
const leftovers = [".legion/LEGION-1/plan.json", "docs/solutions/testing/smoke.md"];

let runs = 0;
function run(
  script: string,
  name: string,
  branches: string[],
  prs: PullRequest[],
  fake: Fake = {}
): RunResult {
  const runDir = join(dir, `run-${++runs}`);
  const state = join(runDir, "gh");
  mkdirSync(state, { recursive: true });
  writeFileSync(join(state, "branches"), ["main", ...branches].map((b) => `${b}\n`).join(""));
  const seeded = prs.map((p) => ({
    ...p,
    url: `https://github.com/sjawhar/legion-smoke/pull/${p.number}`,
  }));
  writeFileSync(join(state, "prs.json"), JSON.stringify(seeded));
  writeFileSync(join(state, "calls"), "");
  writeFileSync(join(state, "comments"), "");
  const result = Bun.spawnSync(["bash", "-c", script, name], {
    env: {
      PATH: `${bin}:${process.env.PATH}`,
      HOME: runDir,
      RUN: runDir,
      FAKE_GH_DIR: state,
      FAKE_GH_LEFTOVERS: leftovers.join("\n"),
      FAKE_GH_MERGE_FAIL: fake.mergeFails ?? "",
      FAKE_GH_CLOSE_FAIL: fake.closeFails ?? "",
      FAKE_GH_LIST_FAIL: fake.listFails ?? "",
      FAKE_GH_CREATE_LOST: fake.createLost ? "1" : "",
      UNTIL: fake.until ?? "",
    },
  });
  const read = (f: string) => readFileSync(join(state, f), "utf8");
  const after: PullRequest[] = JSON.parse(read("prs.json"));
  return {
    code: result.exitCode,
    stdout: result.stdout.toString(),
    stderr: result.stderr.toString(),
    calls: read("calls").split("\n").filter(Boolean),
    comments: read("comments").split("\n").filter(Boolean),
    branches: read("branches").split("\n").filter(Boolean),
    pr: (n: number) => after.find((p) => p.number === n)?.state,
  };
}

// Neither another run's cleanup branch nor one its prefix matches was closed, deleted or written;
// a read that names it (matching-refs) is no touch.
function untouched(r: RunResult, branches: string[], prs: number[]) {
  for (const b of branches) {
    expect(r.branches).toContain(b);
    const write = new RegExp(`git/refs/heads/${b}$|ref=refs/heads/${b} |branch=${b}$`);
    expect(r.calls.filter((c) => write.test(c))).toEqual([]);
  }
  for (const n of prs) {
    expect(r.pr(n)).toBe("OPEN");
    expect(r.calls.filter((c) => new RegExp(`\\bpr (close|merge) ${n}\\b`).test(c))).toEqual([]);
  }
}
const gateway504 = "HTTP 504: Gateway Timeout (https://api.github.com/graphql)";
const close502 = "HTTP 502: Bad Gateway (https://api.github.com/graphql)";
const url = (n: number) => `https://github.com/sjawhar/legion-smoke/pull/${n}`;

// Another lane's Stage 3 cleanup, and a branch the prefix proof/clean-main-legsmoke alone matches.
const others4b = {
  branches: ["proof/clean-main-s387654321", "proof/clean-main-legsmoke2"],
  prs: [
    { number: 1, headRefName: "legion/LEGSMOKE-101", state: "MERGED" },
    { number: 2, headRefName: "proof/clean-main-s387654321", state: "OPEN" },
    { number: 3, headRefName: "proof/clean-main-legsmoke2", state: "OPEN" },
  ] satisfies PullRequest[],
};

describe("Stage 4b's teardown of done's cleanup pull request", () => {
  test("a merge GitHub answers with a 504: the teardown closes the run's pull request and deletes its branch", () => {
    const r = run(stage4b(done4b), "stage4b-sandbox-tree.sh", others4b.branches, others4b.prs, {
      mergeFails: gateway504,
    });
    expect(r.code).toBe(1);
    expect(r.stdout).toContain(`   closed the run's cleanup pull request ${url(4)}\n`);
    expect(r.stdout).toContain(
      "   deleted the run's cleanup branch proof/clean-main-legsmoke from sjawhar/legion-smoke\n"
    );
    expect(r.stdout).toContain("stage 4b e2e: FAIL (fixture teardown, in check done)");
    expect(r.stdout).not.toContain("cleanup warning:");
    expect(r.pr(4)).toBe("CLOSED");
    expect(r.branches).not.toContain("proof/clean-main-legsmoke");
    expect(r.comments).toEqual([
      expect.stringMatching(
        /^#4 Closed by the run that opened it \(stage4b-sandbox-tree\.sh, project LEGSMOKE, pid \d+\), at its teardown\.$/
      ),
    ]);
    untouched(r, others4b.branches, [2, 3]);
  });

  test("a close GitHub refuses prints the pull request's URL, and the branch still goes", () => {
    const r = run(stage4b(done4b), "stage4b-sandbox-tree.sh", others4b.branches, others4b.prs, {
      mergeFails: gateway504,
      closeFails: close502,
    });
    expect(r.code).toBe(1);
    expect(r.stdout).toContain(
      `   could not close the run's cleanup pull request ${url(4)}: ${close502}\n`
    );
    expect(r.stdout).toContain(
      "   deleted the run's cleanup branch proof/clean-main-legsmoke from sjawhar/legion-smoke\n"
    );
    expect(r.stdout).not.toContain("cleanup warning:");
    untouched(r, others4b.branches, [2, 3]);
  });

  test("a listing GitHub refuses prints the recorded pull request's URL", () => {
    const r = run(stage4b(done4b), "stage4b-sandbox-tree.sh", [], [], {
      mergeFails: gateway504,
      listFails: close502,
    });
    expect(r.code).toBe(1);
    expect(r.stdout).toContain(
      `   could not list the open pull requests on sjawhar/legion-smoke, so the run's cleanup pull request ${url(1)} may still be open (gh's reason is on stderr)\n`
    );
    expect(r.pr(1)).toBe("OPEN");
  });

  test("a branch create whose answer was lost is still the run's to delete", () => {
    const r = run(stage4b(done4b), "stage4b-sandbox-tree.sh", others4b.branches, others4b.prs, {
      createLost: true,
    });
    expect(r.code).toBe(1);
    expect(r.stdout).toContain(
      "   deleted the run's cleanup branch proof/clean-main-legsmoke from sjawhar/legion-smoke\n"
    );
    expect(r.calls.filter((c) => c.includes(" pr create "))).toEqual([]);
    untouched(r, others4b.branches, [2, 3]);
  });

  // Every Stage 4b run's cleanup branch is proof/clean-main-legsmoke, so an earlier run's leftover
  // carries this run's name: the run that never reached done leaves it, and done refuses it.
  test("a run that never reached the cleanup touches no cleanup branch, an earlier run's included", () => {
    const r = run(
      stage4b('begin tree-moved\nfail "tree 2 did not move"'),
      "stage4b-sandbox-tree.sh",
      ["proof/clean-main-legsmoke", ...others4b.branches],
      [...others4b.prs, { number: 9, headRefName: "proof/clean-main-legsmoke", state: "OPEN" }]
    );
    expect(r.code).toBe(1);
    expect(r.stdout).toContain("stage 4b e2e: FAIL (check tree-moved)");
    expect(r.calls.filter((c) => c.includes("clean-main"))).toEqual([]);
    untouched(r, ["proof/clean-main-legsmoke", ...others4b.branches], [2, 3, 9]);
  });

  test("done refuses an earlier run's cleanup branch by name, and its teardown leaves it", () => {
    const r = run(
      stage4b(done4b),
      "stage4b-sandbox-tree.sh",
      ["proof/clean-main-legsmoke"],
      [{ number: 9, headRefName: "proof/clean-main-legsmoke", state: "OPEN" }]
    );
    expect(r.code).toBe(1);
    expect(r.stdout).toContain(
      "CHECK done: FAIL: sjawhar/legion-smoke already has the branch proof/clean-main-legsmoke, which this run did not make"
    );
    expect(r.calls.filter((c) => c.includes("matching-refs"))).toHaveLength(1);
    untouched(r, ["proof/clean-main-legsmoke"], [9]);
  });

  test("a run whose cleanup merged makes no teardown call for it", () => {
    const r = run(stage4b(done4b), "stage4b-sandbox-tree.sh", others4b.branches, others4b.prs, {
      until: "done",
    });
    expect(r.code).toBe(0);
    expect(r.stdout).toContain("stage 4b e2e: development run until done finished (not the proof)");
    expect(r.pr(4)).toBe("MERGED");
    expect(r.branches).not.toContain("proof/clean-main-legsmoke");
    const merged = r.calls.findIndex((c) => c.startsWith("-R sjawhar/legion-smoke pr merge 4 "));
    expect(merged).toBeGreaterThan(-1);
    expect(r.calls.slice(merged + 1).filter((c) => c.includes("clean-main"))).toEqual([]);
    untouched(r, others4b.branches, [2, 3]);
  });
});

// This run's held-worker pull request, another lane's issue pull request, and its cleanup.
const others3 = {
  branches: ["legion/S312345678-7", "legion/S387654321-5", "proof/clean-main-s387654321"],
  prs: [
    { number: 1, headRefName: "legion/S312345678-1", state: "MERGED" },
    { number: 2, headRefName: "legion/S312345678-7", state: "OPEN" },
    { number: 3, headRefName: "legion/S387654321-5", state: "OPEN" },
    { number: 4, headRefName: "proof/clean-main-s387654321", state: "OPEN" },
  ] satisfies PullRequest[],
};

describe("Stage 3's teardown of smoke-main-clean's cleanup pull request", () => {
  test("a merge GitHub answers with a 504: the EXIT trap closes the run's pull request and deletes its branch", () => {
    const r = run(stage3(clean3), "stage3-devbox-workflow.sh", others3.branches, others3.prs, {
      mergeFails: gateway504,
    });
    expect(r.code).toBe(1);
    expect(r.stderr).toContain("closed sjawhar/legion-smoke#2 and deleted its branch\n");
    expect(r.stderr).toContain(`closed the run's cleanup pull request ${url(5)}\n`);
    expect(r.stderr).toContain(
      "deleted the run's cleanup branch proof/clean-main-s312345678 from sjawhar/legion-smoke\n"
    );
    expect(r.stderr).toContain("stage 3 e2e: FAIL (fixture teardown, check smoke-main-clean)");
    expect(r.stderr).not.toContain("cleanup warning:");
    expect(r.pr(5)).toBe("CLOSED");
    expect(r.branches).not.toContain("proof/clean-main-s312345678");
    expect(r.comments).toContainEqual(
      expect.stringMatching(
        /^#5 Closed by the run that opened it \(stage3-devbox-workflow\.sh, project S312345678, pid \d+\), at its teardown\.$/
      )
    );
    untouched(r, ["legion/S387654321-5", "proof/clean-main-s387654321"], [3, 4]);
  });

  test("a close GitHub refuses prints the pull request's URL", () => {
    const r = run(stage3(clean3), "stage3-devbox-workflow.sh", others3.branches, others3.prs, {
      mergeFails: gateway504,
      closeFails: close502,
    });
    expect(r.code).toBe(1);
    expect(r.stderr).toContain(
      `could not close the run's cleanup pull request ${url(5)}: ${close502}\n`
    );
    expect(r.stderr).not.toContain("cleanup warning:");
    untouched(r, ["legion/S387654321-5", "proof/clean-main-s387654321"], [3, 4]);
  });

  test("a run that never reached the cleanup closes only its issue pull requests", () => {
    const r = run(
      stage3('begin rework\nfail "round 2 was not pushed"'),
      "stage3-devbox-workflow.sh",
      others3.branches,
      others3.prs
    );
    expect(r.code).toBe(1);
    expect(r.stderr).toContain("closed sjawhar/legion-smoke#2 and deleted its branch\n");
    expect(r.calls.filter((c) => c.includes("clean-main"))).toEqual([]);
    untouched(r, ["legion/S387654321-5", "proof/clean-main-s387654321"], [3, 4]);
  });
});
