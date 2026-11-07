// The driver of verification-results.sh: it drives a real envoy-dispatch over HTTP as one session
// (the run's agent token, each write naming the session as `actor`) and one person (the dev
// sign-in cookie), seeds the run's fake GitHub, and publishes each pull-request event as the
// normalized `pull_request` envelope on the run's own NATS, the one path Dispatch's delivery
// intake reads.
//
//   VERIFICATION_RESULTS_E2E_AGENT_TOKEN=<token> bun scripts/e2e/verification-results.ts \
//     <dispatch-url> <nats-url> <fake-github-url>
//
// Each case prints `== <case>`, what it read, then `ok <case>` or `FAIL <case>: <why>`; a summary
// follows, and the exit status is 1 when any case failed. The first case,
// `the-results-route-is-served`, is the run's first result write, against a plain issue whose
// spec marks nothing: when it fails, as it does against a binary built from the base commit,
// which serves no such route, the run stops there, since every later case records a result too.
// Each later case seeds the issue it drives, with the spec and the Verification document that
// case needs. A step outside the cases that fails (the sign-in, the projects, the fake GitHub's
// seed, the delivery settings, the probe issue) exits 2 naming it.
import type { CheckBlock, CheckResultRecorded } from "@legion/contracts";
import { connect, StringCodec } from "nats";

const [dispatchUrl, natsUrl, githubUrl] = process.argv.slice(2);
const agentToken = process.env.VERIFICATION_RESULTS_E2E_AGENT_TOKEN ?? "";
if (dispatchUrl === undefined || natsUrl === undefined || githubUrl === undefined || !agentToken) {
  process.stderr.write(
    "usage: VERIFICATION_RESULTS_E2E_AGENT_TOKEN=<token> bun verification-results.ts <dispatch-url> <nats-url> <fake-github-url>\n"
  );
  process.exit(2);
}
const origin = new URL(dispatchUrl).origin;

/** How long a currency a bus event decides may take to settle, and how often it is read again.
 *  The intake binds its durable once the settings cache holds a row, takes the envelope and
 *  writes the pull request's head row; nothing else is waited on, since every other answer is
 *  computed while the results are read. */
const currencyWaitMs = 30_000;
const pollMs = 250;

const person = "alice";
const session = "verification-results-e2e";
/** The repository the proof's issues resolve to, and the one the App is not installed on. */
const repo = "acme/verification-live";
const hiddenRepo = "acme/verification-hidden";

/** The commits the proof's repository holds, which is every commit a case records against it:
 *  the record route asks GitHub whether the repository the check's issue resolves to holds a
 *  result's commit and refuses one it does not, and the seed below gives that repository these
 *  and nothing else. A result's commit is 40 lowercase hex characters, which is all Dispatch
 *  reads of it. */
const heldCommits = {
  /** The result read back under its check. */
  a2: "1".repeat(40),
  /** The pull request's first head. */
  headOne: "2".repeat(40),
  /** The pull request's second head, which ages the result at the first. */
  headTwo: "3".repeat(40),
  /** The after-merge result the closed issue takes. */
  closed: "4".repeat(40),
  /** One of the two commits the default-branch check records at, the one the compare calls
   *  behind. The branch holds it as well, and it is named here so the fixture says what the case
   *  needs rather than leaning on the compare's seed. */
  behind: "5".repeat(40),
  /** The other of those two, the one the compare calls diverged. */
  diverged: "6".repeat(40),
  /** The head of the stranger's pull request. */
  fork: "9".repeat(40),
};

/** Every commit the cases name: the ones that repository holds, and three it does not.
 *  `mainHead` is nobody's result, only the end of the default branch, which the branch below
 *  holds; `unheld` is the commit the seed leaves out, so that GitHub's answer that the
 *  repository holds no such commit is what the refusal case drives; and `hidden` is recorded
 *  against the repository the App is not installed on, where the installation's 404 stops
 *  Dispatch before it can ask about a commit at all. */
const commits = {
  ...heldCommits,
  mainHead: "7".repeat(40),
  hidden: "8".repeat(40),
  unheld: "a".repeat(40),
};

type Json = Record<string, unknown>;

const codec = StringCodec();
const nats = await connect({ name: "verification-results", servers: natsUrl });

async function call(
  method: string,
  path: string,
  headers: Record<string, string>,
  body?: Json
): Promise<unknown> {
  const response = await fetch(new URL(path, dispatchUrl), {
    body: body === undefined ? undefined : JSON.stringify(body),
    headers: body === undefined ? headers : { ...headers, "Content-Type": "application/json" },
    method,
  });
  const text = await response.text();
  if (!response.ok) throw new Error(`${method} ${path}: ${response.status} ${text}`);
  return text === "" ? undefined : JSON.parse(text);
}

/** The session's request: the shared agent token, and on a write the session as its `actor`. */
function asSession(method: string, path: string, body?: Json): Promise<unknown> {
  const headers = { Authorization: `Bearer ${agentToken}` };
  const payload =
    body === undefined ? undefined : { ...body, actor: { id: session, kind: "session" } };
  return call(method, path, headers, payload);
}

/** A write the server refuses, as the case that asserts the refusal reads it: `answer` is its
 *  status and its code, and `error` the words it carries, which a case may read for the commit
 *  and the repository they name. */
interface Refusal {
  readonly answer: string;
  readonly error: string;
}

async function refusal(method: string, path: string, body: Json): Promise<Refusal> {
  const response = await fetch(new URL(path, dispatchUrl), {
    body: JSON.stringify({ ...body, actor: { id: session, kind: "session" } }),
    headers: { Authorization: `Bearer ${agentToken}`, "Content-Type": "application/json" },
    method,
  });
  const text = await response.text();
  if (response.ok) return { answer: `${response.status} (the write was taken)`, error: text };
  let code = "";
  let error = text;
  try {
    const parsed = JSON.parse(text) as Json;
    code = String(parsed.code ?? "");
    error = String(parsed.error ?? text);
  } catch {
    code = text;
  }
  return { answer: `${response.status} ${code}`, error };
}

async function signIn(login: string): Promise<string> {
  const path = `/auth/_dev/signin?login=${encodeURIComponent(login)}`;
  const response = await fetch(new URL(path, dispatchUrl), { redirect: "manual" });
  const cookie = response.headers
    .getSetCookie()
    .find((header) => header.startsWith("dsession="))
    ?.split(";")[0];
  if (response.status !== 302 || cookie === undefined) {
    throw new Error(`GET ${path}: ${response.status} ${await response.text()}`);
  }
  return cookie;
}

function setupFailed(step: string, error: unknown): never {
  process.stderr.write(`setup ${step} failed: ${error instanceof Error ? error.message : error}\n`);
  process.exit(2);
}

const personCookie = await signIn(person).catch((error: unknown) => setupFailed("sign-in", error));

/** The person's request: the session cookie, and the dashboard origin every cookie write needs. */
function asPerson(method: string, path: string, body?: Json): Promise<unknown> {
  return call(method, path, { Cookie: personCookie, Origin: origin }, body);
}

function issuePath(key: string, rest = ""): string {
  return `/api/v1/issues/${encodeURIComponent(key)}${rest}`;
}

/** The checks of an issue's Verification document, each with the results recorded against it and
 *  each result's currency, as `GET /api/v1/issues/{key}/results` answers. */
async function checksOf(key: string): Promise<readonly CheckBlock[]> {
  const body = (await asSession("GET", issuePath(key, "/results"))) as { checks: CheckBlock[] };
  return body.checks;
}

/** One check of an issue, by its block id. */
async function checkOf(key: string, blockID: string): Promise<CheckBlock> {
  const found = (await checksOf(key)).find((check) => check.block_id === blockID);
  if (found === undefined) throw new Error(`${key} has no check ${blockID}`);
  return found;
}

/** The currency of the newest result recorded at `commit` against `blockID`, or "absent". */
async function currencyAt(key: string, blockID: string, commit: string): Promise<string> {
  const result = (await checkOf(key, blockID)).results.find((row) => row.commit_sha === commit);
  return result === undefined ? "absent" : (result.currency ?? "unset");
}

/** Waits, bounded, for the result at `commit` to read `want`, and answers what it last read. A
 *  currency is computed while the results are read, so this writes nothing and polls the read. */
async function awaitCurrency(
  key: string,
  blockID: string,
  commit: string,
  want: string
): Promise<string> {
  const deadline = Date.now() + currencyWaitMs;
  let seen = await currencyAt(key, blockID, commit);
  while (seen !== want && Date.now() < deadline) {
    await Bun.sleep(pollMs);
    seen = await currencyAt(key, blockID, commit);
  }
  return seen;
}

/** Records one result through `POST /api/v1/issues/{key}/results`, and answers what Dispatch
 *  says about it: the stored row and the line the pull-request body carries. */
async function record(key: string, input: Json): Promise<CheckResultRecorded> {
  return (await asSession("POST", issuePath(key, "/results"), {
    role: "tester",
    verdict: "pass",
    ...input,
  })) as CheckResultRecorded;
}

/** Publishes one normalized `pull_request` envelope, the shape Envoy relays and the shape
 *  Dispatch's intake reads: the envelope's `payload` is the flat string map, and its subject is
 *  `notifications.github.<owner>.<repo>.pr.<number>` with each segment's dots written `_`. */
async function publishPullRequest(payload: Record<string, string>): Promise<string> {
  const [owner, name] = repo.split("/");
  const subject = `notifications.github.${String(owner).replaceAll(".", "_")}.${String(name).replaceAll(".", "_")}.pr.${payload.number}`;
  const eventID = `verification-results-pr-${payload.number}-${payload.action}-${payload.head_sha.slice(0, 7)}`;
  nats.publish(
    subject,
    codec.encode(
      JSON.stringify({
        dedupe_key: eventID,
        event_id: eventID,
        issued_at: Date.now(),
        payload: JSON.stringify(payload),
        payload_summary: subject,
        source: "github",
        source_event_id: eventID,
        topic: subject,
        trace_id: eventID,
      })
    )
  );
  await nats.flush();
  return subject;
}

function note(line: string): void {
  console.log(`   ${line}`);
}

const results: { name: string; ok: boolean }[] = [];

async function runCase(name: string, body: () => Promise<string | undefined>): Promise<void> {
  console.log(`== ${name}`);
  let failure: string | undefined;
  try {
    failure = await body();
  } catch (error) {
    failure = error instanceof Error ? error.message : String(error);
  }
  results.push({ name, ok: failure === undefined });
  console.log(failure === undefined ? `ok ${name}` : `FAIL ${name}: ${failure}`);
}

function summarise(stopped?: string): never {
  const failed = results.filter((result) => !result.ok).map((result) => result.name);
  if (stopped !== undefined) console.log(`   ${stopped}`);
  console.log(
    `summary: ${results.length - failed.length} ok, ${failed.length} FAIL${failed.length === 0 ? "" : ` (${failed.join(", ")})`}`
  );
  void nats.close();
  process.exit(failed.length === 0 ? 0 : 1);
}

// --- setup ---------------------------------------------------------------------------------------

// The fake GitHub answers for two repositories: the one the proof's issues resolve to, whose
// default branch holds the `behind` commit and ends at `mainHead`, and one the App is not
// installed on, whose every read is GitHub's 404.
//
// The first is given `heldCommits` and no commit besides, so every commit a case records against
// it is held and the one the refusal case drives earns the 422 GitHub answers for a commit a
// repository does not hold. What each commit is for stands at its own declaration above, which
// is the one list of them.
//
// `commit_pulls` is empty on purpose: the record route's read-through asks GitHub which pull
// requests name a result's commit, and with no answer there the only thing that can teach
// Dispatch a head is the envelope the cases publish.
const seeded = await fetch(new URL("/__fixture/repos", githubUrl), {
  body: JSON.stringify({
    [repo]: {
      branch_commits: { main: [commits.behind, commits.mainHead] },
      commits: Object.values(heldCommits),
      contents: "read",
      default_branch: "main",
      installation_id: 256,
      pull_requests: "read",
    },
    [hiddenRepo]: { contents: "read", installation_id: 257, installation_missing: true },
  }),
  headers: { "Content-Type": "application/json" },
  method: "PUT",
}).catch((error: unknown) => setupFailed("fake GitHub seed", error));
if (!seeded.ok) setupFailed("fake GitHub seed", `${seeded.status} ${await seeded.text()}`);

for (const [key, name, mapped] of [
  ["VRF", "Verification results", repo],
  ["VRH", "Verification results, unreadable repository", hiddenRepo],
] as const) {
  await asPerson("POST", "/api/v1/projects", { key, name }).catch((error: unknown) =>
    setupFailed(`project ${key}`, error)
  );
  await asSession("PUT", `/api/v1/settings/repo-projects/${mapped}`, {
    default: true,
    project: key,
  }).catch((error: unknown) => setupFailed(`repository mapping ${mapped}`, error));
}

// The delivery intake handles an envelope only while the settings cache holds a row, so the proof
// writes one; the deploy repository is the proof's own, which the access check reads through the
// fake GitHub before the row is stored.
await asSession("PUT", "/api/v1/settings/delivery", {
  deploy_repo: repo,
  deploy_workflow_path: ".github/workflows/deploy.yml",
  excluded_repos: [],
  population_authors: ["octocat"],
  pr_checks_workflow_path: ".github/workflows/pr-checks.yml",
  production_job_names: ["verification-release / verification-release"],
}).catch((error: unknown) => setupFailed("delivery settings", error));

/** Seeds one issue with a spec that marks one requirement and a Verification document holding
 *  `checks`, whose `%KEY%` is the issue's own key — a `serves` is refused unless it names a block
 *  that exists, so the requirement is written with the issue, before any check points at it — and
 *  answers that key. */
async function seedIssue(project: string, title: string, checks: string): Promise<string> {
  const issue = (await asPerson("POST", "/api/v1/issues", {
    project,
    spec: `${title}.\n\n:::requirement{#r1}\nThe proof records a result against a check that serves this.\n:::\n`,
    title,
  })) as Json;
  const key = String(issue.key);
  await asSession("POST", issuePath(key, "/artifacts"), {
    content: `How ${key} is proved.\n\n${checks.replaceAll("%KEY%", key)}`,
    name: "verification.md",
  });
  return key;
}

// One issue with a plain spec, which every Dispatch serves: the route probe below writes its
// result against this one, so the probe reaches the record route rather than a typed block the
// server may not know.
const probeIssue = String(
  (
    (await asPerson("POST", "/api/v1/issues", {
      project: "VRF",
      spec: "The proof's first result write goes to this issue.\n",
      title: "The results route answers",
    }).catch((error: unknown) => setupFailed("the probe issue", error))) as Json
  ).key
);

console.log(
  `setup: person ${person}; session ${session}; probe issue ${probeIssue}; repositories ${repo} (default branch main) and ${hiddenRepo} (no installation). Each case seeds the issue it drives.`
);

// --- cases ---------------------------------------------------------------------------------------

// The run's first result write, and the one every later case depends on: the record route takes a
// result and judges the check it names. A binary that does not serve the route at all answers 404
// and the run stops here, which is what this proof's negative control reads.
await runCase("the-results-route-is-served", async () => {
  const { answer } = await refusal("POST", issuePath(probeIssue, "/results"), {
    check: "no-such-check",
    commit: commits.a2,
    role: "tester",
    verdict: "pass",
  });
  note(`a result naming a check ${probeIssue} does not have answers ${answer}`);
  return answer === "400 UNKNOWN_CHECK" ? undefined : `the record route answers ${answer}`;
});
if (!results[0]?.ok) {
  summarise("every later case records a result too, so the run stops at the first result write");
}

// A2. A recorded result is read back under its check, with the fields it was recorded with.
await runCase("a2-record-and-read", async () => {
  const a2Issue = await seedIssue(
    "VRF",
    "A result is recorded and read back",
    `:::check{#a2 serves="dispatch://%KEY%/spec#b-r1" surface="cli" run="bash scripts/e2e/verification-results.sh" negative_control="Record nothing and the check reads no result."}
The result read back under this check is the one that was recorded.
:::
`
  );
  note(`${a2Issue} holds the check a2`);
  const answered = await record(a2Issue, {
    check: "a2",
    commit: commits.a2,
    negative_control: "Recorded nothing and the check read no result.",
    observed: "the record route answered 201",
    run: "bash scripts/e2e/verification-results.sh",
    surface: "cli",
  });
  note(`the pull-request body line is ${JSON.stringify(answered.line)}`);
  note(`the record's answer says it was stored at ${answered.result.created_at}`);
  const check = await checkOf(a2Issue, "a2");
  note(
    `${a2Issue} check a2 (${check.when}) holds ${check.results.length} result(s): ${check.results
      .map(
        (row) =>
          `${row.verdict} at ${row.commit_sha.slice(0, 12)} by ${row.role} (${row.currency ?? "unset"})`
      )
      .join(", ")}`
  );
  if (check.results.length !== 1) return `the check holds ${check.results.length} results, want 1`;
  const [recorded] = check.results;
  if (recorded?.commit_sha !== commits.a2 || recorded.verdict !== "pass") {
    return `the result read back is ${JSON.stringify(recorded)}`;
  }
  if (recorded.observed !== "the record route answered 201") {
    return `the result's observed reads ${JSON.stringify(recorded.observed)}`;
  }
  // The record's own answer carries the moment the row was stored, not the zero time: a reader
  // of `dispatch result` takes it for when the run happened, and only the later read was ever
  // right about it.
  const stored = Date.parse(answered.result.created_at);
  if (Number.isNaN(stored) || stored <= 0) {
    return `the record's answer carries created_at ${JSON.stringify(answered.result.created_at)}`;
  }
  if (answered.result.created_at !== recorded.created_at) {
    return `the record answered created_at ${answered.result.created_at}, the read gives ${recorded.created_at}`;
  }
  return undefined;
});

// A4. The head a `before_merge` result is measured against is the one the bus taught Dispatch: a
// push moves it with no write of anyone's to Dispatch, and the older result says so when it is
// next read.
await runCase("a4-a-push-moves-the-head", async () => {
  const a4Issue = await seedIssue(
    "VRF",
    "A push moves the head a result is measured against",
    `:::check{#a4 serves="dispatch://%KEY%/spec#b-r1" surface="cli" run="bash scripts/e2e/verification-results.sh"}
A result stands while its commit is the pull request's head.
:::
`
  );
  const opened = await publishPullRequest({
    action: "opened",
    base_ref: "main",
    body: `Proves ${a4Issue}.`,
    head_ref: `legion/${a4Issue}`,
    head_repo: repo,
    head_sha: commits.headOne,
    kind: "pr",
    merged: "false",
    number: "7",
    repo,
    title: `${a4Issue} the proof's pull request`,
    updated_at: new Date(Date.now() - 60_000).toISOString(),
    url: `https://github.com/${repo}/pull/7`,
  });
  note(`published the opened envelope on ${opened}`);
  await record(a4Issue, {
    check: "a4",
    commit: commits.headOne,
    observed: "recorded at the pull request's first head",
    run: "bash scripts/e2e/verification-results.sh",
    surface: "cli",
  });
  const atFirstHead = await awaitCurrency(a4Issue, "a4", commits.headOne, "current");
  note(`the result at ${commits.headOne.slice(0, 12)} reads ${atFirstHead}`);
  if (atFirstHead !== "current") return `the result at the first head reads ${atFirstHead}`;

  const pushed = await publishPullRequest({
    action: "synchronize",
    base_ref: "main",
    body: `Proves ${a4Issue}.`,
    head_ref: `legion/${a4Issue}`,
    head_repo: repo,
    head_sha: commits.headTwo,
    kind: "pr",
    merged: "false",
    number: "7",
    repo,
    title: `${a4Issue} the proof's pull request`,
    updated_at: new Date().toISOString(),
    url: `https://github.com/${repo}/pull/7`,
  });
  note(`published the synchronize envelope on ${pushed}`);
  const aged = await awaitCurrency(a4Issue, "a4", commits.headOne, "older");
  note(`after the push the same result reads ${aged}, with no write of Dispatch's`);
  if (aged !== "older") return `the result at the first head reads ${aged} after the push`;

  await record(a4Issue, {
    check: "a4",
    commit: commits.headTwo,
    observed: "recorded at the pull request's new head",
    run: "bash scripts/e2e/verification-results.sh",
    surface: "cli",
  });
  const atNewHead = await awaitCurrency(a4Issue, "a4", commits.headTwo, "current");
  note(`the result at the new head ${commits.headTwo.slice(0, 12)} reads ${atNewHead}`);
  return atNewHead === "current" ? undefined : `the result at the new head reads ${atNewHead}`;
});

// A fork's pull request makes no result current. Anyone may open one against a public
// repository, at any commit and with any title, so the head a result is measured against is only
// a pull request whose head branch lives in this repository. The same pull request reported with
// its head here, which is the only difference between the two envelopes, does make it current.
await runCase("a-fork-pull-request-makes-no-result-current", async () => {
  const forkIssue = await seedIssue(
    "VRF",
    "A fork's pull request makes no result current",
    `:::check{#fork serves="dispatch://%KEY%/spec#b-r1" surface="cli" run="bash scripts/e2e/verification-results.sh"}
A result stands while its commit is the head of a pull request of this repository.
:::
`
  );
  const forked = await publishPullRequest({
    action: "opened",
    base_ref: "main",
    body: `Proves ${forkIssue}.`,
    head_ref: `legion/${forkIssue}`,
    head_repo: `stranger/${String(repo.split("/")[1])}`,
    head_sha: commits.fork,
    kind: "pr",
    merged: "false",
    number: "9",
    repo,
    title: `${forkIssue} the stranger's pull request`,
    updated_at: new Date(Date.now() - 60_000).toISOString(),
    url: `https://github.com/${repo}/pull/9`,
  });
  note(`published the fork's envelope on ${forked}`);
  await record(forkIssue, {
    check: "fork",
    commit: commits.fork,
    observed: "recorded at the commit the fork's pull request heads",
    run: "bash scripts/e2e/verification-results.sh",
    surface: "cli",
  });
  const atFork = await awaitCurrency(forkIssue, "fork", commits.fork, "current");
  note(`the result at the fork's head ${commits.fork.slice(0, 12)} reads ${atFork}`);
  if (atFork === "current") return "a fork's pull request made the result current";

  // The same pull request, now reported with its head in this repository, does make it current:
  // the refusal above is the head's repository and nothing else.
  const owned = await publishPullRequest({
    action: "synchronize",
    base_ref: "main",
    body: `Proves ${forkIssue}.`,
    head_ref: `legion/${forkIssue}`,
    head_repo: repo,
    head_sha: commits.fork,
    kind: "pr",
    merged: "false",
    number: "9",
    repo,
    title: `${forkIssue} the stranger's pull request`,
    updated_at: new Date().toISOString(),
    url: `https://github.com/${repo}/pull/9`,
  });
  note(`published the same pull request with its head in ${repo} on ${owned}`);
  const atOwnHead = await awaitCurrency(forkIssue, "fork", commits.fork, "current");
  note(`the same result now reads ${atOwnHead}`);
  return atOwnHead === "current"
    ? undefined
    : `the result at this repository's head reads ${atOwnHead}`;
});

// A5. A closed issue takes an after-merge result, and still refuses a document write.
await runCase("a5-a-closed-issue-takes-the-after-merge-result", async () => {
  const a5Issue = await seedIssue(
    "VRF",
    "A closed issue takes an after-merge result",
    `:::check{#after when="after_merge" surface="api" run="curl the production route"}
The production check runs after the merge.
:::

:::check{#before surface="cli" run="bun test"}
The pre-merge check runs before it.
:::
`
  );
  await asSession("PATCH", issuePath(a5Issue), { status: "done" });
  const closed = (await asSession("GET", issuePath(a5Issue))) as Json;
  note(`${a5Issue} is ${String(closed.status)}`);
  const afterMerge = await record(a5Issue, {
    check: "after",
    commit: commits.closed,
    observed: "the production route answered",
    run: "curl the production route",
    surface: "api",
  });
  note(`the after-merge result recorded: ${JSON.stringify(afterMerge.line)}`);
  const check = await checkOf(a5Issue, "after");
  if (check.results.length !== 1)
    return `the after-merge check holds ${check.results.length} results, want 1`;

  const { answer: beforeMerge } = await refusal("POST", issuePath(a5Issue, "/results"), {
    check: "before",
    commit: commits.closed,
    role: "tester",
    verdict: "pass",
  });
  note(`a result for the before-merge check there answers ${beforeMerge}`);
  if (beforeMerge !== "409 ISSUE_CLOSED") return `the before-merge result answers ${beforeMerge}`;

  const { answer: document } = await refusal("POST", issuePath(a5Issue, "/artifacts"), {
    content: "A note the closed issue must refuse.\n",
    name: "note.md",
  });
  note(`a document write there answers ${document}`);
  return document === "409 ISSUE_CLOSED" ? undefined : `the document write answers ${document}`;
});

// A10. A `default_branch` result stands while GitHub says the branch holds its commit.
await runCase("a10-a-default-branch-result-follows-the-branch", async () => {
  const a10Issue = await seedIssue(
    "VRF",
    "A default-branch result stands while the branch holds its commit",
    `:::check{#branch when="default_branch" surface="api" run="curl the production route"}
The default branch holds the commit this check ran at.
:::
`
  );
  await record(a10Issue, {
    check: "branch",
    commit: commits.behind,
    observed: "the production route answered",
    run: "curl the production route",
    surface: "api",
  });
  const held = await currencyAt(a10Issue, "branch", commits.behind);
  note(
    `the result at ${commits.behind.slice(0, 12)}, which the compare calls behind, reads ${held}`
  );
  if (held !== "current") return `the result at the commit the branch holds reads ${held}`;

  await record(a10Issue, {
    check: "branch",
    commit: commits.diverged,
    observed: "the production route answered",
    run: "curl the production route",
    surface: "api",
  });
  const apart = await currencyAt(a10Issue, "branch", commits.diverged);
  note(
    `the result at ${commits.diverged.slice(0, 12)}, which the compare calls diverged, reads ${apart}`
  );
  return apart === "current" ? `the result at the diverged commit reads ${apart}` : undefined;
});

// A commit the repository does not hold: the record route asks GitHub about every result's
// commit before it writes anything, and the fake answers the 422 GitHub answers for a 40-hex
// commit it was not seeded with. The refusal names the commit whole and the `owner/repo` the
// check's issue resolves to, and the check keeps no row, since nothing is written before the
// read.
await runCase("a-result-at-a-commit-the-repository-does-not-hold-is-refused", async () => {
  const unheldIssue = await seedIssue(
    "VRF",
    "An unpushed commit earns a refusal",
    `:::check{#unheld serves="dispatch://%KEY%/spec#b-r1" surface="cli" run="bash scripts/e2e/verification-results.sh"}
A result is recorded only at a commit the repository holds.
:::
`
  );
  const refused = await refusal("POST", issuePath(unheldIssue, "/results"), {
    check: "unheld",
    commit: commits.unheld,
    observed: "the run passed at a commit nobody pushed",
    role: "tester",
    run: "bash scripts/e2e/verification-results.sh",
    surface: "cli",
    verdict: "pass",
  });
  note(`a result at ${commits.unheld.slice(0, 12)} answers ${refused.answer}: ${refused.error}`);
  if (refused.answer !== "400 RESULT_COMMIT_UNKNOWN") {
    return `the record route answers ${refused.answer}`;
  }
  if (!refused.error.includes(commits.unheld)) {
    return `the refusal does not name the commit: ${refused.error}`;
  }
  if (!refused.error.includes(repo)) {
    return `the refusal does not name ${repo}: ${refused.error}`;
  }
  const check = await checkOf(unheldIssue, "unheld");
  note(`${unheldIssue} check unheld holds ${check.results.length} result(s)`);
  return check.results.length === 0
    ? undefined
    : `the refused result left ${check.results.length} row(s) under the check`;
});

// The unreadable repository: Dispatch cannot say, so it marks the result neither way — for a
// `before_merge` check as well as a `default_branch` one. Nothing can teach Dispatch the pull
// request heads of a repository it cannot read, so "this commit heads none I know" is no
// evidence that the work has moved on, and an unmarked pass is what the reviewer reads.
await runCase("an-unreadable-repository-leaves-the-result-unmarked", async () => {
  const hiddenIssue = await seedIssue(
    "VRH",
    "A result on a repository Dispatch cannot read",
    `:::check{#hidden when="default_branch" surface="api" run="curl the production route"}
Dispatch cannot see the repository this check's issue resolves to.
:::

:::check{#hidden-pre surface="cli" run="bash scripts/e2e/verification-results.sh"}
The pre-merge check of the same unreadable repository.
:::
`
  );
  await record(hiddenIssue, {
    check: "hidden",
    commit: commits.hidden,
    observed: "the production route answered",
    run: "curl the production route",
    surface: "api",
  });
  const unknown = await currencyAt(hiddenIssue, "hidden", commits.hidden);
  note(`${hiddenIssue} resolves to ${hiddenRepo}, which the installation lookup does not find`);
  note(`the default_branch result at ${commits.hidden.slice(0, 12)} reads ${unknown}`);
  if (unknown !== "unknown") return `the default_branch result reads ${unknown}, want unknown`;

  await record(hiddenIssue, {
    check: "hidden-pre",
    commit: commits.hidden,
    observed: "the pre-merge run passed",
    run: "bash scripts/e2e/verification-results.sh",
    surface: "cli",
  });
  const beforeMerge = await currencyAt(hiddenIssue, "hidden-pre", commits.hidden);
  note(`the before_merge result at ${commits.hidden.slice(0, 12)} reads ${beforeMerge}`);
  return beforeMerge === "unknown"
    ? undefined
    : `the before_merge result reads ${beforeMerge}, want unknown`;
});

summarise();
