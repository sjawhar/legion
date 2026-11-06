#!/usr/bin/env bun
// Stand-in for `gh` on a skill scenario's PATH. Each call is recorded to
// $SKILL_SCENARIO_RUN/calls.jsonl (argv, the contents of any file or stdin it hands GitHub a body
// through, its exit status, the agent that made the call when the run names one
// ($SKILL_SCENARIO_AGENT, a run that boots one agent per role), and the pull request body it
// kept, if any), then answered from the
// first route in $SKILL_SCENARIO_RUN/fixtures.json whose regex matches "gh <argv...>". A call no
// route matches answers `gh: Not Found (HTTP 404)` and exits 1. Nothing is sent anywhere.
// `--json a,b` picks fields and `--jq`/`-q` runs jq, as gh does. A pull request body edit (`gh pr
// edit --body`, `--body-file`, or a PATCH through `gh api`) is kept: every route answering with an
// object that has a `body` then answers with the new one, as GitHub's next read would, and the
// call's record carries it as `body`.
//   gh-standin.ts gh <args...>
import { appendFileSync, existsSync, readFileSync, writeFileSync } from "node:fs";
import { z } from "zod";

/** rig.sh's worker_fixture writes each route as {match, stdout?, stderr?, exit?}. */
const Fixtures = z.looseObject({
  routes: z.array(
    z.object({
      match: z.string(),
      stdout: z.unknown().optional(),
      stderr: z.string().optional(),
      exit: z.number().optional(),
    })
  ),
});

/** The part of rig.sh's <run>/world.json this stand-in reads: the issue branch of the run's own
 * bare remote, whose tip is the pull request's head, and the branch it is open against, which is
 * the other side of its diff. */
const World = z.looseObject({ branch: z.string(), base: z.string().default("main") });

const run = process.env.SKILL_SCENARIO_RUN;
if (!run) {
  console.error("gh-standin: SKILL_SCENARIO_RUN is unset");
  process.exit(2);
}
const [as = "gh", ...args] = Bun.argv.slice(2);
const at = new Date().toISOString();
const fixtures = Fixtures.parse(JSON.parse(readFileSync(`${run}/fixtures.json`, "utf8")));
const routes = fixtures.routes;

let stdin: string | undefined;
const readStdin = async () => {
  stdin ??= await new Response(Bun.stdin.stream()).text();
  return stdin;
};
const files: Record<string, string> = {};
const readSource = async (source: string) =>
  source === "-" ? readStdin() : readFileSync(source, "utf8");
// A pull request body edit: `gh pr edit` takes it from `-b/--body` or `-F/--body-file`, a
// `gh api -X PATCH repos/<o>/<r>/pulls/<n>` from a `body` field or an `--input` JSON object.
const prEdit = as === "gh" && args[0] === "pr" && args[1] === "edit";
const prPatch =
  as === "gh" &&
  args[0] === "api" &&
  /(^|\s)(-X|--method)\s?PATCH(\s|$)/.test(args.join(" ")) &&
  args.some((arg) => /^\/?repos\/[^/]+\/[^/]+\/pulls\/\d+$/.test(arg));
let body: string | undefined;
/** The body this call left the pull request with, once the routes serve it. */
let kept: string | undefined;
for (let index = 0; index < args.length; index += 1) {
  const arg = args[index] ?? "";
  const next = args[index + 1];
  if (!next) continue;
  if (prEdit && (arg === "-b" || arg === "--body")) body = next;
  else if (prEdit && (arg === "-F" || arg === "--body-file")) {
    files[next] = await readSource(next);
    body = files[next];
  } else if (arg === "--input" || arg === "--body-file") files[next] = await readSource(next);
  else if (arg === "-F" || arg === "--field" || arg === "-f" || arg === "--raw-field") {
    const [key = "", ...rest] = next.split("=");
    let value = rest.join("=");
    if ((arg === "-F" || arg === "--field") && value.startsWith("@")) {
      files[value.slice(1)] = await readSource(value.slice(1));
      value = files[value.slice(1)] ?? "";
    }
    if (prPatch && key === "body") body = value;
  }
}
const inputIndex = args.indexOf("--input");
const input = inputIndex >= 0 ? args[inputIndex + 1] : undefined;
if (prPatch && input !== undefined) {
  const parsed: unknown = JSON.parse(files[input] ?? "null");
  if (
    typeof parsed === "object" &&
    parsed !== null &&
    "body" in parsed &&
    typeof parsed.body === "string"
  ) {
    body = parsed.body;
  }
}
const joined = `${as} ${args.join(" ")}`;
const route = routes.find((candidate) => new RegExp(candidate.match, "s").test(joined));
/** Records the call with the status it exits with, then exits. `by` is the agent the run gave
 * this call's environment, so a run that boots one agent per role has each call say which role
 * made it; a run that names none records no `by`. */
function exit(status: number): never {
  const by = process.env.SKILL_SCENARIO_AGENT;
  appendFileSync(
    `${run}/calls.jsonl`,
    `${JSON.stringify({ at, as, by, argv: args, files, stdin, route: route?.match ?? null, body: kept, exit: status })}\n`
  );
  process.exit(status);
}
if (!route) {
  console.error(as === "gh" ? "gh: Not Found (HTTP 404)" : `unknown command: ${args.join(" ")}`);
  exit(1);
}
if (body !== undefined) {
  const edited = routes.map((candidate) =>
    typeof candidate.stdout === "object" && candidate.stdout !== null && "body" in candidate.stdout
      ? { ...candidate, stdout: { ...candidate.stdout, body } }
      : candidate
  );
  writeFileSync(`${run}/fixtures.json`, JSON.stringify({ ...fixtures, routes: edited }));
  kept = body;
}
/** The run's world (rig.sh's worker_fixture), or undefined when the run has none: a scenario
 * that builds no world writes no world.json, and a call of its own reads the fixture's answer. */
function worldOf(): z.infer<typeof World> | undefined {
  if (!existsSync(`${run}/world.json`)) return undefined;
  const read = World.safeParse(JSON.parse(readFileSync(`${run}/world.json`, "utf8") || "{}"));
  return read.success ? read.data : undefined;
}
/** What `git -C <run>/remote.git <args...>` prints, or undefined when the command fails: the
 * run's own bare remote, read at the call. A run with no remote of its own fails every call. */
function fromRemote(...args: string[]): string | undefined {
  const read = Bun.spawnSync(["git", "-C", `${run}/remote.git`, ...args]);
  return read.exitCode === 0 ? read.stdout.toString().trim() : undefined;
}
// What GitHub answers for the pull request's head is the commit its branch points at, so a route
// whose object carries `headRefOid` answers the tip of the run's own remote rather than the
// fixture's first commit: a role that pushes and then reads the head, which the worker skill has
// every pushing role do, must see its own commit. The tip is read from the run's bare remote at
// the call, and a remote the run has not got leaves the fixture's value as it is.
function branchTip(): string | undefined {
  const world = worldOf();
  const tip =
    world === undefined ? undefined : fromRemote("rev-parse", `refs/heads/${world.branch}`);
  return tip === "" ? undefined : tip;
}
// What GitHub serves for `gh pr diff` is the pull request's diff as it stands, so the answer is
// the diff of the run's own remote at the call, the issue branch against the branch it is open
// against, rather than the diff the fixture froze. A role that reads the pull request after
// another role pushed to it — the reviewer, reading the fix the implementer pushed after an
// approval that left text threads open — must see what was pushed. `--name-only` answers the
// names alone, as `gh pr diff --name-only` does. A remote the run has not got leaves the
// fixture's answer as it is.
function branchDiff(nameOnly: boolean): string | undefined {
  const world = worldOf();
  if (world === undefined) return undefined;
  return fromRemote("diff", ...(nameOnly ? ["--name-only"] : []), world.base, world.branch);
}
let output: unknown = route.stdout ?? "";
if (typeof output === "object" && output !== null && "headRefOid" in output) {
  const tip = branchTip();
  if (tip !== undefined) {
    const nested =
      "head" in output && typeof output.head === "object" && output.head !== null
        ? { head: { ...output.head, sha: tip } }
        : {};
    output = { ...output, headRefOid: tip, ...nested };
  }
}
if (as === "gh" && args[0] === "pr" && args[1] === "diff") {
  // An empty diff is GitHub's own answer for a pull request whose branch carries no change, so
  // it stands as the answer rather than falling back to the route's.
  const diff = branchDiff(args.includes("--name-only"));
  if (diff !== undefined) output = diff === "" ? "" : `${diff}\n`;
}
const jsonIndex = args.indexOf("--json");
if (jsonIndex >= 0 && typeof output === "object" && output !== null && !Array.isArray(output)) {
  const record: Record<string, unknown> = { ...output };
  const fields = (args[jsonIndex + 1] ?? "").split(",");
  output = Object.fromEntries(
    fields.filter((field) => field in record).map((field) => [field, record[field]])
  );
}
let text = typeof output === "string" ? output : JSON.stringify(output);
const jqIndex = args.findIndex((arg) => arg === "--jq" || arg === "-q");
const filter = jqIndex >= 0 ? args[jqIndex + 1] : undefined;
if (filter !== undefined) {
  const jq = Bun.spawnSync(["jq", "-r", filter], { stdin: new TextEncoder().encode(text) });
  text = jq.stdout.toString();
  if (jq.exitCode !== 0) {
    process.stderr.write(jq.stderr.toString());
    exit(1);
  }
}
if (text !== "") process.stdout.write(text.endsWith("\n") ? text : `${text}\n`);
if (route.stderr) process.stderr.write(route.stderr);
exit(route.exit ?? 0);
