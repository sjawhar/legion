// The delivery pages' e2e fixture, written directly in Postgres: the timeline, the measures and
// the Pipeline page read only stored facts, so these are the rows intake and reconcile would have
// written, without a live GitHub App. One module so the timeline rows, the measures rows, the
// Pipeline rows and the docs screenshots read the same data. acme/widgets-shaped placeholders
// throughout (AGENTS.md: "This repository is public").
import { sql } from "./psql";

const SEED_ACTOR = `'{"kind":"system","id":"e2e-seed"}'`;

const SETTINGS = `INSERT INTO delivery_settings (
     singleton, deploy_repo, deploy_workflow_path, production_job_names,
     pr_checks_workflow_path, population_authors, excluded_repos, updated_by
   ) VALUES (
     true, 'acme/widgets', '.github/workflows/deploy.yml', ARRAY['widgets-release / widgets-release'],
     '.github/workflows/pr-checks.yml', ARRAY['octocat'], ARRAY[]::text[], ${SEED_ACTOR}
   )`;

/** The slice 1 timeline's fixture, in 2024-06-01: two population pull requests (#1 shipped by
 *  deploy run 500 and attributed to ACME-1, a P0 issue; #2 waiting, a rework, with no issue),
 *  the deploy run with its jobs, and a failed run (501). */
export async function seedDeliveryFixture(): Promise<void> {
  await sql(
    SETTINGS,
    `INSERT INTO projects (key, name) VALUES ('ACME', 'Acme') ON CONFLICT DO NOTHING`,
    `INSERT INTO issues (key, project_key, number, title, status, priority, created_by, rank)
     VALUES ('ACME-1', 'ACME', 1, 'Ship the widgets', 'todo', 0, ${SEED_ACTOR}, 'U')`,
    `INSERT INTO delivery_pull_requests (
       repo, number, title, url, author, created_at, merged_at, additions, deletions, rework, issue_key, sessions, partial
     ) VALUES
       ('acme/widgets', 1, 'feat: a shipped widget', 'https://github.com/acme/widgets/pull/1', 'octocat',
        '2024-06-01T00:00:00Z', '2024-06-01T01:00:00Z', 12, 3, false, 'ACME-1', ARRAY[]::text[], false),
       ('acme/widgets', 2, 'fix: a waiting widget', 'https://github.com/acme/widgets/pull/2', 'octocat',
        '2024-06-01T02:00:00Z', '2024-06-01T03:00:00Z', 4, 1, true, null, ARRAY[]::text[], false)`,
    `INSERT INTO delivery_runs (
       repo, run_id, kind, head_sha, head_commit_at, started_at, created_at, completed_at, conclusion, url, head_branch, event
     ) VALUES
       ('acme/widgets', 500, 'deploy', 'deadbeef', '2024-06-01T01:00:00Z',
        '2024-06-01T01:30:00Z', '2024-06-01T01:30:00Z', '2024-06-01T01:40:00Z', 'success', 'https://github.com/acme/widgets/actions/runs/500', 'main', 'push'),
       ('acme/widgets', 501, 'deploy', 'cafef00d', '2024-06-01T03:00:00Z',
        '2024-06-01T12:00:00Z', '2024-06-01T12:00:00Z', '2024-06-01T12:20:00Z', 'failure', 'https://github.com/acme/widgets/actions/runs/501', 'main', 'push')`,
    `INSERT INTO delivery_run_jobs (repo, run_id, name, started_at, completed_at, conclusion) VALUES
       ('acme/widgets', 500, 'build', '2024-06-01T01:30:00Z', '2024-06-01T01:32:00Z', 'success'),
       ('acme/widgets', 500, 'widgets-release / widgets-release', '2024-06-01T01:32:00Z', '2024-06-01T01:40:00Z', 'success'),
       ('acme/widgets', 501, 'build', '2024-06-01T12:00:00Z', '2024-06-01T12:05:00Z', 'success'),
       ('acme/widgets', 501, 'integration tests', '2024-06-01T12:05:00Z', '2024-06-01T12:20:00Z', 'failure'),
       ('acme/widgets', 501, 'widgets-release / widgets-release', null, null, 'skipped')`
  );
}

/** The session-groups fixture, relative to now so the window control's presets see it: two
 *  population pull requests merged in the last seven days, first written by sessions
 *  `e2e-planner-1` and `e2e-planner-2`, and one merged 20 days ago, first written by
 *  `e2e-reviewer`. Each session carries the title Dispatch learns it by: an event the session
 *  wrote with its origin's `session_title` (delivery/agents.go's ResolveSessionTitles). */
export async function seedSessionGroupsFixture(): Promise<void> {
  await sql(
    SETTINGS,
    `INSERT INTO projects (key, name) VALUES ('ACME', 'Acme') ON CONFLICT DO NOTHING`,
    `INSERT INTO issues (key, project_key, number, title, status, priority, created_by, rank)
     VALUES ('ACME-7', 'ACME', 7, 'Group the planners', 'todo', 1, ${SEED_ACTOR}, 'U')`,
    `INSERT INTO delivery_pull_requests (
       repo, number, title, url, author, created_at, merged_at, additions, deletions, rework, issue_key, sessions, partial
     ) VALUES
       ('acme/widgets', 11, 'feat: plan the widgets', 'https://github.com/acme/widgets/pull/11', 'octocat',
        now() - interval '2 days 1 hour', now() - interval '2 days', 10, 2, false, null, ARRAY['e2e-planner-1'], false),
       ('acme/widgets', 12, 'feat: plan the gadgets', 'https://github.com/acme/widgets/pull/12', 'octocat',
        now() - interval '3 days 1 hour', now() - interval '3 days', 8, 1, false, null, ARRAY['e2e-planner-2'], false),
       ('acme/widgets', 13, 'feat: review the widgets', 'https://github.com/acme/widgets/pull/13', 'octocat',
        now() - interval '20 days 1 hour', now() - interval '20 days', 5, 5, false, null, ARRAY['e2e-reviewer'], false)`,
    `INSERT INTO events (issue_key, seq, type, actor, payload, notify, created_at) VALUES
       ('ACME-7', 1, 'message.created',
        '{"kind":"session","id":"e2e-planner-1","origin":{"session_title":"Planner, first run"}}', '{}', false, now() - interval '4 days'),
       ('ACME-7', 2, 'message.created',
        '{"kind":"session","id":"e2e-planner-2","origin":{"session_title":"Planner, second run"}}', '{}', false, now() - interval '4 days'),
       ('ACME-7', 3, 'message.created',
        '{"kind":"session","id":"e2e-reviewer","origin":{"session_title":"Reviewer"}}', '{}', false, now() - interval '21 days')`
  );
}

/** The measures fixture's window (packages/envoy/internal/dispatch/delivery/measures/testdata/
 *  dora-fixture.json, the prototype's own web/fixtures/dataset.json with its names replaced). */
export const FIXTURE_WINDOW = { from: "2026-08-30T00:00:00Z", to: "2026-09-27T20:00:00Z" } as const;

/** The measures fixture: the prototype fixture's 14 population pull requests and 8 deploy runs,
 *  each run's production job (`widgets-release / widgets-release`) and failed jobs, and project
 *  ACME's four issues, ACME-103 the one open P0 with neither a claim nor a route. Every run is a
 *  push on main. Its expected measures are the prototype's own computeDora with each deploy derived
 *  by its own containment (LEGION-567 slice 2 plan, "How the expected values are produced"). */
export async function seedMeasuresFixture(): Promise<void> {
  await sql(
    SETTINGS,
    `INSERT INTO projects (key, name) VALUES ('ACME', 'Acme') ON CONFLICT DO NOTHING`,
    `INSERT INTO issues (key, project_key, number, title, status, priority, route, claimed_by, claimed_at, created_by, rank) VALUES
       ('ACME-100', 'ACME', 100, 'Build timeline UI', 'in_progress', 1, null,
        '{"kind":"session","id":"01a1-e2e-session"}', now(), ${SEED_ACTOR}, 'U100'),
       ('ACME-101', 'ACME', 101, 'DORA metrics module', 'in_progress', 2, 'role:e2e', null, null, ${SEED_ACTOR}, 'U101'),
       ('ACME-102', 'ACME', 102, 'Collector schema', 'done', 1, null, null, null, ${SEED_ACTOR}, 'U102'),
       ('ACME-103', 'ACME', 103, 'Production deploy gate flakes', 'triage', 0, null, null, null, ${SEED_ACTOR}, 'U103')`,
    `INSERT INTO delivery_pull_requests (
       repo, number, title, url, author, created_at, merged_at, first_commit_at,
       additions, deletions, rework, issue_key, sessions, partial
     ) VALUES
       ('acme/widgets', 101, 'Add timeline zoom brush', 'https://github.com/acme/widgets/pull/101', 'octocat',
        '2026-09-04T18:00:00Z', '2026-09-05T12:00:00Z', '2026-09-04T15:00:00Z', 210, 12, false, 'ACME-100', ARRAY[]::text[], false),
       ('acme/widgets', 102, 'fix: timeline brush jumps on resize', 'https://github.com/acme/widgets/pull/102', 'octocat',
        '2026-09-06T09:00:00Z', '2026-09-06T13:00:00Z', '2026-09-06T08:30:00Z', 18, 4, true, 'ACME-100', ARRAY[]::text[], false),
       ('acme/widgets', 103, 'Add DORA lead-time calc', 'https://github.com/acme/widgets/pull/103', 'octocat',
        '2026-09-07T10:00:00Z', '2026-09-07T16:00:00Z', '2026-09-07T08:00:00Z', 140, 6, false, 'ACME-101', ARRAY[]::text[], false),
       ('acme/widgets', 104, 'Add facet filtering module', 'https://github.com/acme/widgets/pull/104', 'octocat',
        '2026-09-08T09:00:00Z', '2026-09-08T15:00:00Z', '2026-09-08T07:00:00Z', 95, 3, false, 'ACME-100', ARRAY[]::text[], false),
       ('acme/widgets', 105, 'revert: Add facet filtering module', 'https://github.com/acme/widgets/pull/105', 'octocat',
        '2026-09-09T08:00:00Z', '2026-09-09T09:00:00Z', '2026-09-09T07:45:00Z', 3, 95, true, 'ACME-100', ARRAY[]::text[], false),
       ('acme/widgets', 106, 'Add drill-down panel', 'https://github.com/acme/widgets/pull/106', 'octocat',
        '2026-09-10T09:00:00Z', '2026-09-10T17:00:00Z', '2026-09-10T08:00:00Z', 180, 20, false, 'ACME-100', ARRAY[]::text[], false),
       ('acme/widgets', 107, 'hotfix: drill-down panel crash on null issue', 'https://github.com/acme/widgets/pull/107', 'octocat',
        '2026-09-11T10:00:00Z', '2026-09-11T11:00:00Z', '2026-09-11T09:45:00Z', 9, 2, true, 'ACME-100', ARRAY[]::text[], false),
       ('acme/gadgets', 20, 'Add dispatch API client', 'https://github.com/acme/gadgets/pull/20', 'octocat',
        '2026-09-12T09:00:00Z', '2026-09-12T14:00:00Z', '2026-09-12T08:00:00Z', 60, 0, false, 'ACME-102', ARRAY[]::text[], false),
       ('acme/widgets', 108, 'Wire SSE events endpoint', 'https://github.com/acme/widgets/pull/108', 'octocat',
        '2026-09-13T07:00:00Z', '2026-09-13T08:00:00Z', '2026-09-13T06:00:00Z', 75, 5, false, 'ACME-100', ARRAY[]::text[], false),
       ('acme/widgets', 109, 'Add priority facet counts', 'https://github.com/acme/widgets/pull/109', 'octocat',
        '2026-09-13T08:00:00Z', '2026-09-13T08:30:00Z', '2026-09-13T07:00:00Z', 40, 2, false, 'ACME-101', ARRAY[]::text[], false),
       ('acme/widgets', 110, 'Improve waiting-to-deploy step line', 'https://github.com/acme/widgets/pull/110', 'octocat',
        '2026-09-15T09:00:00Z', '2026-09-15T15:00:00Z', '2026-09-15T08:00:00Z', 30, 8, false, 'ACME-100', ARRAY[]::text[], false),
       ('acme/widgets', 111, 'Add component hierarchy filter', 'https://github.com/acme/widgets/pull/111', 'octocat',
        '2026-09-16T09:00:00Z', '2026-09-16T16:00:00Z', '2026-09-16T08:00:00Z', 120, 10, false, null, ARRAY[]::text[], false),
       ('acme/widgets', 112, 'fix: component hierarchy includes grandchildren', 'https://github.com/acme/widgets/pull/112', 'octocat',
        '2026-09-17T07:00:00Z', '2026-09-17T07:30:00Z', '2026-09-17T06:45:00Z', 14, 3, true, null, ARRAY[]::text[], false),
       ('acme/widgets', 113, 'Add rework share metric', 'https://github.com/acme/widgets/pull/113', 'octocat',
        '2026-09-18T09:00:00Z', '2026-09-18T15:00:00Z', '2026-09-18T08:00:00Z', 50, 4, false, 'ACME-101', ARRAY[]::text[], false)`,
    `INSERT INTO delivery_runs (
       repo, run_id, kind, head_sha, head_commit_at, started_at, created_at, completed_at, conclusion, url, head_branch, event
     ) VALUES
      ('acme/widgets', 501, 'deploy', 'aaa101', '2026-09-05T12:30:00Z', '2026-09-05T12:01:00Z', '2026-09-05T12:01:00Z',
        '2026-09-05T13:05:00Z', 'success', 'https://github.com/acme/widgets/actions/runs/501', 'main', 'push'),
       ('acme/widgets', 502, 'deploy', 'aaa102', '2026-09-06T13:30:00Z', '2026-09-06T13:35:00Z', '2026-09-06T13:35:00Z',
        '2026-09-06T14:05:00Z', 'success', 'https://github.com/acme/widgets/actions/runs/502', 'main', 'push'),
       ('acme/widgets', 503, 'deploy', 'aaa103', '2026-09-09T09:30:00Z', '2026-09-09T09:35:00Z', '2026-09-09T09:35:00Z',
        '2026-09-09T10:05:00Z', 'success', 'https://github.com/acme/widgets/actions/runs/503', 'main', 'push'),
       ('acme/widgets', 504, 'deploy', 'aaa104', '2026-09-13T08:45:00Z', '2026-09-13T08:50:00Z', '2026-09-13T08:50:00Z',
        '2026-09-13T09:05:00Z', 'success', 'https://github.com/acme/widgets/actions/runs/504', 'main', 'push'),
       ('acme/widgets', 505, 'deploy', 'aaa105', '2026-09-17T07:45:00Z', '2026-09-17T07:50:00Z', '2026-09-17T07:50:00Z',
        '2026-09-17T08:05:00Z', 'success', 'https://github.com/acme/widgets/actions/runs/505', 'main', 'push'),
       ('acme/widgets', 506, 'deploy', 'aaa106', '2026-09-11T08:00:00Z', '2026-09-11T08:30:00Z', '2026-09-11T08:30:00Z',
        '2026-09-11T09:05:00Z', 'failure', 'https://github.com/acme/widgets/actions/runs/506', 'main', 'push'),
       ('acme/widgets', 507, 'deploy', 'aaa107', '2026-09-14T08:30:00Z', '2026-09-14T08:35:00Z', '2026-09-14T08:35:00Z',
        '2026-09-14T09:05:00Z', 'failure', 'https://github.com/acme/widgets/actions/runs/507', 'main', 'push'),
       ('acme/widgets', 508, 'deploy', 'aaa108', '2026-09-19T09:50:00Z', '2026-09-19T10:00:00Z', '2026-09-19T10:00:00Z',
        null, 'cancelled', 'https://github.com/acme/widgets/actions/runs/508', 'main', 'push')`,
    `INSERT INTO delivery_run_jobs (repo, run_id, name, completed_at, conclusion) VALUES
       ('acme/widgets', 501, 'widgets-release / widgets-release', '2026-09-05T13:00:00Z', 'success'),
       ('acme/widgets', 502, 'widgets-release / widgets-release', '2026-09-06T14:00:00Z', 'success'),
       ('acme/widgets', 503, 'widgets-release / widgets-release', '2026-09-09T10:00:00Z', 'success'),
       ('acme/widgets', 504, 'widgets-release / widgets-release', '2026-09-13T09:00:00Z', 'success'),
       ('acme/widgets', 505, 'widgets-release / widgets-release', '2026-09-17T08:00:00Z', 'success'),
       ('acme/widgets', 506, 'integration-test', '2026-09-11T09:00:00Z', 'failure'),
       ('acme/widgets', 507, 'widgets-release / widgets-release', '2026-09-14T09:00:00Z', 'failure')`
  );
}

/** The Pipeline page's pinned "today" (`?at=`): the e2e rows and the docs screenshot show the
 *  seven days ending on its day, 2026-09-26 to 2026-10-02. */
export const PIPELINE_AT = "2026-10-02T12:00:00Z";

/** A time on 2026-09-<day> (a day past 30 rolls into October), as the prototype's
 *  collector/tests/test_pipeline.py `_ts` writes it. */
function fixtureTime(day: number, hour: number, minute = 0): string {
  return new Date(Date.UTC(2026, 8, day, hour, 0, 0) + minute * 60_000).toISOString();
}

interface PipelineJob {
  name: string;
  minutes: number;
  conclusion: string | null;
}

/** One stored run and its jobs as VALUES rows: each job starts at `jobStart` and lasts its
 *  minutes; a null conclusion is a job still running. */
function pipelineRun(run: {
  id: number;
  kind: "pr_checks" | "deploy";
  started: string;
  completed: string;
  conclusion: string;
  branch: string | null;
  event: string;
  jobStart: string;
  jobs: PipelineJob[];
}): { run: string; jobs: string[] } {
  const quote = (value: string | null) => (value === null ? "null" : `'${value}'`);
  const prNumber = run.kind === "pr_checks" ? "7" : "null";
  const started = Date.parse(run.jobStart);
  return {
    run: `('acme/widgets', ${run.id}, '${run.kind}', ${prNumber}, 's${run.id}', '${run.started}', '${run.started}', '${run.started}', '${run.completed}', '${run.conclusion}', 'https://github.com/acme/widgets/actions/runs/${run.id}', ${quote(run.branch)}, '${run.event}')`,
    jobs: run.jobs.map(
      (job) =>
        `('acme/widgets', ${run.id}, '${job.name}', '${run.jobStart}', '${new Date(started + job.minutes * 60_000).toISOString()}', ${quote(job.conclusion)})`
    ),
  };
}

/** The Pipeline page's rows: the ones the prototype's collector/tests/test_pipeline.py builds in
 *  code, with placeholder job names (the API test `api/delivery_pipeline_test.go` seeds the same
 *  rows), dated so `?at=PIPELINE_AT` shows them, and the three job names set on the stored
 *  settings row. PR checks: the failing-jobs runs (today, two failures and a cancelled run;
 *  yesterday, a success and a failure before the 24 hours) and 50 build / unit results to rank
 *  among the slowest jobs, every one a `pull_request` run, plus a `merge_group` run no card
 *  counts. Deploys (run ids 601-606, clear of the measures fixture's 501-508, so the docs harness
 *  seeds both): four push runs on main naming the three settings' jobs, plus a workflow_dispatch
 *  run and a push on feature/x no card counts. The settings name `staging-e2e / run`,
 *  `staging-release / apply` and the roll-up `checks-complete`. Two pull requests merged today:
 *  one shipped by run 601 (created 08:00, after its 07:10 merge), one still waiting. */
export async function seedPipelineRows(): Promise<void> {
  const job = (name: string, minutes: number, conclusion: string | null = "success") => ({
    conclusion,
    minutes,
    name,
  });
  const check = (
    id: number,
    day: number,
    hour: number,
    minutes: number,
    conclusion: string,
    jobs: PipelineJob[],
    event = "pull_request"
  ) =>
    pipelineRun({
      branch: null,
      completed: fixtureTime(day, hour, minutes),
      conclusion,
      event,
      id,
      jobStart: fixtureTime(day, hour),
      jobs,
      kind: "pr_checks",
      started: fixtureTime(day, hour),
    });
  const deploy = (id: number, branch: string, event: string, jobs: PipelineJob[]) =>
    pipelineRun({
      branch,
      completed: fixtureTime(32, 12),
      conclusion: "success",
      event,
      id,
      jobStart: fixtureTime(32, 9),
      jobs: [job("staging-e2e / inventory", 0, "skipped"), ...jobs],
      kind: "deploy",
      started: fixtureTime(32, 8),
    });
  const runs = [
    check(1, 32, 8, 30, "failure", [
      job("build / unit", 29, "failure"),
      job("checks-complete", 0.5, "failure"),
      job("lint", 5),
    ]),
    check(2, 32, 10, 30, "failure", [
      job("build / unit", 29, "failure"),
      job("lint", 5, "failure"),
    ]),
    check(3, 31, 13, 30, "success", [
      job("build / unit", 29),
      job("lint", 5),
      job("docs", 0, "skipped"),
    ]),
    check(4, 31, 6, 30, "failure", [job("typecheck", 4, "failure")]),
    check(5, 32, 11, 10, "cancelled", [job("build / unit", 9, "failure")]),
    ...Array.from({ length: 50 }, (_, i) =>
      check(100 + i, 27 + (i % 5), 8, 95, "success", [job("build / unit", 90 + i)])
    ),
    check(900, 32, 9, 20, "success", [job("build / unit", 19)], "merge_group"),
    deploy(601, "main", "push", [
      job("staging-release / apply", 7),
      job("staging-e2e / run", 40),
      job("widgets-release / widgets-release", 9),
    ]),
    deploy(602, "main", "push", [
      job("staging-release / apply", 8),
      job("staging-e2e / run", 70, "failure"),
      job("widgets-release / widgets-release", 0, "skipped"),
    ]),
    deploy(603, "main", "push", [
      job("staging-release / apply", 9),
      job("staging-e2e / run", 3, "cancelled"),
    ]),
    deploy(604, "main", "push", [job("staging-release / apply", 0, null)]),
    deploy(605, "main", "workflow_dispatch", [
      job("staging-release / apply", 99, "failure"),
      job("staging-e2e / run", 99, "failure"),
    ]),
    deploy(606, "feature/x", "push", [
      job("staging-e2e / run", 30),
      job("widgets-release / widgets-release", 9),
    ]),
  ];
  await sql(
    `UPDATE delivery_settings SET staging_e2e_job_name = 'staging-e2e / run',
       staging_apply_job_name = 'staging-release / apply', pr_checks_rollup_job_name = 'checks-complete'
     WHERE singleton`,
    `INSERT INTO delivery_pull_requests (
       repo, number, title, url, author, created_at, merged_at, additions, deletions, rework, issue_key, sessions, partial
     ) VALUES
       ('acme/widgets', 11, 'feat: a piped widget', 'https://github.com/acme/widgets/pull/11', 'octocat',
        '${fixtureTime(32, 7)}', '${fixtureTime(32, 7, 10)}', 3, 1, false, null, ARRAY[]::text[], false),
       ('acme/widgets', 12, 'feat: a waiting widget', 'https://github.com/acme/widgets/pull/12', 'octocat',
        '${fixtureTime(32, 9)}', '${fixtureTime(32, 10)}', 2, 0, false, null, ARRAY[]::text[], false)`,
    `INSERT INTO delivery_runs (
       repo, run_id, kind, pr_number, head_sha, head_commit_at, started_at, created_at, completed_at, conclusion, url, head_branch, event
     ) VALUES ${runs.map((r) => r.run).join(",\n")}`,
    `INSERT INTO delivery_run_jobs (repo, run_id, name, started_at, completed_at, conclusion) VALUES
       ${runs.flatMap((r) => r.jobs).join(",\n")}`
  );
}

/** The Pipeline page's fixture on a reset database: the delivery settings, then its rows. */
export async function seedPipelineFixture(): Promise<void> {
  await sql(SETTINGS);
  await seedPipelineRows();
}
